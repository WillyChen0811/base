package wgtund

// projsync_v2.go：项目同步 v2 的调度与本机状态接口（PLAN-sync-v2.md 第十五节第③项）。
//
// 每一轮同步的编排在 projsync.RunRound（纯编排，不知道 SSH）；这里只负责：
//   - hosts.json projsync.v2 配置（缺省关闭）；
//   - enabled=true 时每 interval 分钟对每个项目依次跑一轮（不并发）；
//   - 本机接口 GET /projsync/v2、POST /projsync/v2/run、POST /projsync/v2/commit（鉴权与 Origin 校验同 v1 /projsync）；
//   - 结果写 ~/.xxyi/projsync-v2-status.json（原子写、0600）便于诊断。
// enabled=false 时不自动跑，但手动接口照常可用。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/WillyChen0811/base/go/agentstate"
	"github.com/WillyChen0811/base/go/projsync"
)

// projsyncV2MinMinutes 是自动同步间隔的下限（分钟）：小于它（含未设置、负数）一律按它算。
// 来源：PLAN-sync-v2.md 第六节第 4 条，检测间隔至少 5 分钟。
const projsyncV2MinMinutes = 5

const (
	defaultProjsyncV2Msg = "~/.local/bin/xxyi-msg" // 对方 xxyi-msg 的缺省路径
	projsyncV2StatusFile = "projsync-v2-status.json"
	// 启动后第一轮自动同步的等待时间（不超过一个间隔），避免刚启动就抢网络。
	projsyncV2InitialDelay = 30 * time.Second
)

// 与 projsync.remoteCommand 同一个字符集；这里提前在启动时挡住，不等到第一轮才报错。
var projsyncV2MsgRe = regexp.MustCompile(`^[A-Za-z0-9._/~-]+$`)

// projsyncV2Conf 是 hosts.json projsync.v2 段。
type projsyncV2Conf struct {
	Enabled         bool   `json:"enabled"`          // 缺省 false：不自动跑
	IntervalMinutes int    `json:"interval_minutes"` // 小于 5 按 5
	XxyiMsg         string `json:"xxyi_msg"`         // 对方 xxyi-msg 路径，只允许 [A-Za-z0-9._/~-]
}

type projsyncV2Prep struct {
	enabled  bool
	interval time.Duration
	xxyiMsg  string
}

// prepareProjsyncV2 校验并补缺省；c 为 nil 返回关闭的缺省值。错误文本接在「的 v2 」之后。
func prepareProjsyncV2(c *projsyncV2Conf) (projsyncV2Prep, error) {
	p := projsyncV2Prep{interval: projsyncV2MinMinutes * time.Minute, xxyiMsg: defaultProjsyncV2Msg}
	if c == nil {
		return p, nil
	}
	p.enabled = c.Enabled
	if c.IntervalMinutes > projsyncV2MinMinutes {
		p.interval = time.Duration(c.IntervalMinutes) * time.Minute
	}
	if c.XxyiMsg != "" {
		if !projsyncV2MsgRe.MatchString(c.XxyiMsg) || strings.HasPrefix(c.XxyiMsg, "-") {
			return p, fmt.Errorf("的 xxyi_msg %q 不合规：只允许 [A-Za-z0-9._/~-]，且不能以 - 开头", c.XxyiMsg)
		}
		p.xxyiMsg = c.XxyiMsg
	}
	return p, nil
}

// projsyncV2Item 是一个项目在状态接口里的内容。
type projsyncV2Item struct {
	Name    string `json:"name"`
	Running bool   `json:"running"`
	// Last：最近一次真正跑完的一轮。LastSkip：最近一次因上一轮还在进行而跳过的记录。
	Last     *projsync.RoundResult `json:"last"`
	LastSkip *projsync.RoundResult `json:"last_skip"`
}

// projsyncV2Status 是 GET /projsync/v2 与状态文件的内容。
type projsyncV2Status struct {
	Enabled         bool             `json:"enabled"`
	IntervalMinutes int              `json:"interval_minutes"`
	NextRun         *time.Time       `json:"next_run"` // 未启用自动同步时为 null
	Updated         time.Time        `json:"updated"`
	Projects        []projsyncV2Item `json:"projects"`
}

type projsyncV2Svc struct {
	v1     *projsyncSvc // 复用其配置、git 目标与线路转发口
	runner projsync.RemoteRunner
	// localJudge 判定本机工作区里 agent 的状态；测试可换。
	localJudge projsync.AgentJudge
	// round 是一轮的执行；默认 projsync.RunRound，测试可换成假的。
	round func(ctx context.Context, env projsync.RoundEnv) projsync.RoundResult
	// statusPath 状态文件；空则不写。
	statusPath   string
	interval     time.Duration
	initialDelay time.Duration
	now          func() time.Time

	mu       sync.Mutex
	running  map[string]bool
	last     map[string]*projsync.RoundResult
	lastSkip map[string]*projsync.RoundResult
	next     time.Time
}

func newProjsyncV2Svc(v1 *projsyncSvc, pool *sshPool) *projsyncV2Svc {
	s := &projsyncV2Svc{v1: v1, localJudge: judgeLocalAgent, round: projsync.RunRound,
		interval: v1.prep.v2.interval, initialDelay: projsyncV2InitialDelay, now: time.Now,
		running: map[string]bool{}, last: map[string]*projsync.RoundResult{}, lastSkip: map[string]*projsync.RoundResult{}}
	if pool != nil {
		s.runner = newSSHRemoteRunner(pool, v1.prep.host)
	}
	if dir, err := xxyiDir(); err == nil {
		s.statusPath = filepath.Join(dir, projsyncV2StatusFile)
	}
	return s
}

// judgeLocalAgent 用 agentstate 判定本机工作区里 agent 的状态；失败按 unknown。
func judgeLocalAgent(ctx context.Context, worktree string) projsync.AgentVerdict {
	r, err := agentstate.JudgeDetail(ctx, worktree, agentstate.Options{})
	av := projsync.AgentVerdict{Verdict: string(r.Verdict), Notes: r.Notes}
	if err != nil {
		av.Verdict = string(agentstate.VerdictUnknown)
		av.Notes = append(av.Notes, "判定失败: "+err.Error())
	}
	return av
}

// env 组装一轮需要的外部依赖。git 交换沿用 v1 的 gitTarget（当前线路 + 本机转发口）。
func (s *projsyncV2Svc) env(t projsyncTarget, force []projsync.ForceCommit) projsync.RoundEnv {
	gt := s.v1.gitTarget
	if s.v1.gitOverride != nil {
		gt = s.v1.gitOverride
	}
	prep := s.v1.prep
	return projsync.RoundEnv{
		Project: t.name, Repo: t.local, RemoteRepo: t.remote, RemoteNS: prep.remoteName,
		Exchange: func(ctx context.Context) error {
			url, env, err := gt(t)
			if err != nil {
				return err
			}
			_, err = projsync.SyncGit(ctx, t.local, url, env, prep.localName, prep.remoteName)
			return err
		},
		Remote: s.runner, XxyiMsg: prep.v2.xxyiMsg, LocalJudge: s.localJudge, Now: s.now, Force: force,
	}
}

// acquire 占位：同一项目已在跑时返回 errProjsyncBusy。
func (s *projsyncV2Svc) acquire(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running[name] {
		return errProjsyncBusy
	}
	s.running[name] = true
	return nil
}

// execute 跑一轮并记下结果、写状态文件；调用方必须已占位。
func (s *projsyncV2Svc) execute(ctx context.Context, t projsyncTarget, force []projsync.ForceCommit) projsync.RoundResult {
	ctx, cancel := context.WithTimeout(ctx, projsyncRunTimeout)
	defer cancel()
	res := s.round(ctx, s.env(t, force))
	s.mu.Lock()
	delete(s.running, t.name)
	r := res
	if res.Skipped {
		s.lastSkip[t.name] = &r
	} else {
		s.last[t.name] = &r
	}
	s.mu.Unlock()
	s.persist()
	return res
}

// Start 本机接口用：先占位再起后台协程，立即返回。
func (s *projsyncV2Svc) Start(ctx context.Context, name string, force []projsync.ForceCommit) error {
	t, ok := s.v1.target(name)
	if !ok {
		return errProjsyncUnknown
	}
	if err := s.acquire(name); err != nil {
		return err
	}
	go s.execute(ctx, t, force)
	return nil
}

// runAll 依次（不并发）对每个项目跑一轮。某项目已在跑（手动触发中）则记一条跳过。
func (s *projsyncV2Svc) runAll(ctx context.Context) {
	for _, t := range s.v1.prep.targets {
		if ctx.Err() != nil {
			return
		}
		if err := s.acquire(t.name); err != nil {
			now := s.now()
			sk := &projsync.RoundResult{Project: t.name, Started: now, Finished: now, Skipped: true,
				SkipReason: "上一轮还在进行", Summary: "上一轮还在进行"}
			s.mu.Lock()
			s.lastSkip[t.name] = sk
			s.mu.Unlock()
			continue
		}
		s.execute(ctx, t, nil)
	}
}

// loop 是定时调度：未启用自动同步时立即返回；否则直到 ctx 结束。
func (s *projsyncV2Svc) loop(ctx context.Context) {
	if !s.v1.prep.v2.enabled {
		return
	}
	delay := s.initialDelay
	if delay > s.interval {
		delay = s.interval
	}
	for {
		s.mu.Lock()
		s.next = s.now().Add(delay)
		s.mu.Unlock()
		s.persist()
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		s.runAll(ctx)
		delay = s.interval
	}
}

func (s *projsyncV2Svc) status() projsyncV2Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := projsyncV2Status{Enabled: s.v1.prep.v2.enabled, IntervalMinutes: int(s.interval / time.Minute),
		Updated: s.now(), Projects: make([]projsyncV2Item, 0, len(s.v1.prep.targets))}
	if st.Enabled && !s.next.IsZero() {
		n := s.next
		st.NextRun = &n
	}
	for _, t := range s.v1.prep.targets {
		st.Projects = append(st.Projects, projsyncV2Item{Name: t.name, Running: s.running[t.name],
			Last: s.last[t.name], LastSkip: s.lastSkip[t.name]})
	}
	return st
}

// persist 把当前状态原子写到状态文件（临时文件 + 改名，0600）；失败只忽略（仅用于诊断）。
func (s *projsyncV2Svc) persist() {
	if s.statusPath == "" {
		return
	}
	b, err := json.MarshalIndent(s.status(), "", "  ")
	if err != nil {
		return
	}
	dir := filepath.Dir(s.statusPath)
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	f, err := os.CreateTemp(dir, ".projsync-v2-status-*.tmp")
	if err != nil {
		return
	}
	tmp := f.Name()
	_, werr := f.Write(append(b, '\n'))
	cerr := f.Chmod(0o600)
	clerr := f.Close()
	if werr != nil || cerr != nil || clerr != nil || os.Rename(tmp, s.statusPath) != nil {
		os.Remove(tmp)
	}
}

// localHandler 是本机接口（挂在 wgtund 主令牌校验之后）：
//
//	GET  /projsync/v2                         每个项目最近一轮结果与下次计划时间
//	POST /projsync/v2/run?name=               立即后台跑一轮
//	POST /projsync/v2/commit?name=&side=mac|ubuntu&worktree=<路径>
//	                                          用户点「提交并同步」：对该工作区强制提交，然后跑一轮
//
// POST 与 /hosts/route、/projsync/run 一样只许从 xxyi 页面发起（要求白名单 Origin）。
func (s *projsyncV2Svc) localHandler(ctx context.Context, allowedOrigins []string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/projsync/v2", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			errJSON(w, http.StatusMethodNotAllowed, "只支持 GET")
			return
		}
		writeJSON(w, s.status())
	})
	post := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPost {
			errJSON(w, http.StatusMethodNotAllowed, "请用 POST")
			return false
		}
		if o := r.Header.Get("Origin"); o == "" || !originAllowed(o, allowedOrigins) {
			errJSON(w, http.StatusForbidden, "需要从 xxyi 页面操作")
			return false
		}
		return true
	}
	start := func(w http.ResponseWriter, name string, force []projsync.ForceCommit) {
		err := s.Start(ctx, name, force)
		switch {
		case errors.Is(err, errProjsyncUnknown):
			errJSON(w, http.StatusNotFound, "未知项目")
		case errors.Is(err, errProjsyncBusy):
			errJSON(w, http.StatusConflict, "正在同步")
		case err != nil:
			errJSON(w, http.StatusInternalServerError, err.Error())
		default:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(map[string]bool{"started": true})
		}
	}
	mux.HandleFunc("/projsync/v2/run", func(w http.ResponseWriter, r *http.Request) {
		if post(w, r) {
			start(w, r.URL.Query().Get("name"), nil)
		}
	})
	mux.HandleFunc("/projsync/v2/commit", func(w http.ResponseWriter, r *http.Request) {
		if !post(w, r) {
			return
		}
		q := r.URL.Query()
		var side projsync.Side
		switch q.Get("side") {
		case "mac":
			side = projsync.SideMac
		case "ubuntu":
			side = projsync.SideUbuntu
		default:
			errJSON(w, http.StatusBadRequest, "side 只能是 mac 或 ubuntu")
			return
		}
		wt := q.Get("worktree")
		if wt == "" || !filepath.IsAbs(wt) || strings.ContainsRune(wt, 0) || !utf8.ValidString(wt) {
			errJSON(w, http.StatusBadRequest, "worktree 必须是合法的绝对路径")
			return
		}
		start(w, q.Get("name"), []projsync.ForceCommit{{Side: side, Worktree: wt}})
	})
	return mux
}
