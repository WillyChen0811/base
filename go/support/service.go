package support

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Remote 由调用方（wgtund）实现：在远端主机上开监听、发布/撤销入口信息。
type Remote interface {
	Open(ctx context.Context) (net.Listener, error)                     // 在远端开监听；Accept 到的连接由本服务处理
	Publish(ctx context.Context, addr, token string, until int64) error // 把入口地址、远端令牌和截止时间（Unix 秒）写到远端
	Unpublish(ctx context.Context) error                                // 删掉远端的入口信息
}

// Config 是协助服务的配置；零值不自带任何项目默认值。
type Config struct {
	Runner        RunnerConfig
	Repos         map[string]Repo                                     // 允许同步的仓库
	GitEnv        []string                                            // 同步时追加给 git 的环境变量
	LogPath       string                                              // 审计日志（jsonl，追加写，文件权限 0600）；空则不写文件
	HookWait      time.Duration                                       // 钩子最多等用户多久，<=0 用 9 分钟
	ProjSyncNames []string                                            // 可同步的项目名；空则不提供项目同步
	ProjSync      func(ctx context.Context, name string) (any, error) // 执行一次同步，返回可 JSON 编码的结果
	// Prepare 在每次从未开启到开启时、开远端入口之前调用（可为 nil），例如重新读 Claude 设置；
	// 返回错误则不开启。
	Prepare func() error
}

// Status 是协助服务的状态快照。
type Status struct {
	Active     bool        `json:"active"`
	Until      int64       `json:"until"`       // Unix 秒，未开为 0
	RemoteAddr string      `json:"remote_addr"` // 远端监听地址，未开或暂时断开为空
	Claude     RunnerState `json:"claude"`
	Pending    []Request   `json:"pending"`  // 永远不是 null
	Repos      []string    `json:"repos"`    // 排序后的名字，永远不是 null
	Projects   []string    `json:"projects"` // 可同步项目名列表，排序后永远不是 null
}

// LogEntry 是一条审计日志。
type LogEntry struct {
	Time   time.Time         `json:"time"`
	Kind   string            `json:"kind"` // start / stop / task / ask / ask_rejected / decide / timeout / cancelled / sync / remote
	Detail map[string]string `json:"detail"`
}

var (
	// reconnectDelay 是远端断线后重试 Open+Publish 的间隔；测试可调小。
	reconnectDelay = 5 * time.Second
	// minuteUnit 是 Start 里一分钟的实际长度；测试可调小。
	minuteUnit = time.Minute
	// maxPending 是待批请求的上限，超出的钩子请求直接拒绝；测试可调小。
	maxPending = 50
)

const (
	defaultHookWait  = 9 * time.Minute // HookWait<=0 时的上限
	maxTaskBytes     = 64 << 10        // /task 正文上限
	maxHookBytes     = 1 << 20         // /support/hook 正文上限
	maxLogText       = 2000            // 任务文字进日志的截断长度（按 rune）
	maxDecideMessage = 500             // 本机批准/拒绝附言的上限（按 rune）
	maxLogEntries    = 200             // 内存里保留的日志条数
	syncTimeout      = 3 * time.Minute // 单次同步上限
	remoteOpWait     = 10 * time.Second
	stopDrainWait    = 10 * time.Second // Stop 等在途 handler 退出多久后改为先停 Claude
	unpublishWait    = 5 * time.Second
)

// session 是一次协助会话：Start（非延长）创建，Stop 取消。
// 所有挂在这次会话上的 goroutine / 远端 handler / 钩子都登记在 wg 里，Stop 会等它们退出。
type session struct {
	ctx    context.Context
	cancel context.CancelFunc
	token  string // 远端令牌
	wg     sync.WaitGroup
}

// Service 把待批队列、钩子、Claude 进程和代码同步组装成协助服务。并发安全。
type Service struct {
	cfg       Config
	remote    Remote
	queue     *Queue
	runner    *Runner
	hookToken string // NewService 时随机生成，不变
	repos     []string
	syncLocks map[string]*sync.Mutex // 每个仓库一把，同仓库的同步串行

	opMu sync.Mutex // 串行化 Start/Stop；Stop 持有它等 wg，所以 wg 里的任何 goroutine 都不能拿它

	mu         sync.Mutex // 保护以下状态字段（只做短临界区，绝不在持有时等 wg / 做网络调用）
	active     bool
	until      time.Time
	sess       *session // 当前会话；未开为 nil
	remoteAddr string
	srv        *http.Server
	ln         net.Listener
	timer      *time.Timer // 截止时间计时器

	logMu   sync.Mutex
	logRecs []LogEntry
	logFile *os.File // 延迟打开；写失败即关闭丢弃，不影响功能
	closed  bool     // Close 之后不再打开日志文件
}

// NewService 建一个未开启的协助服务。
func NewService(cfg Config, remote Remote) *Service {
	names := make([]string, 0, len(cfg.Repos))
	locks := make(map[string]*sync.Mutex, len(cfg.Repos))
	for name := range cfg.Repos {
		names = append(names, name)
		locks[name] = &sync.Mutex{}
	}
	sort.Strings(names)
	return &Service{
		cfg:       cfg,
		remote:    remote,
		queue:     NewQueue(),
		runner:    NewRunner(cfg.Runner),
		hookToken: randHex32(),
		repos:     names,
		syncLocks: locks,
	}
}

// randHex32 生成 32 字节随机数的十六进制串（64 位）。读不到随机数就 panic（fail closed）。
func randHex32() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("support: 读系统随机数失败: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// tokenMatch 恒定时间比较两个令牌（先各自哈希，避免泄露长度）。
func tokenMatch(want, got string) bool {
	if want == "" {
		return false
	}
	hw := sha256.Sum256([]byte(want))
	hg := sha256.Sum256([]byte(got))
	return subtle.ConstantTimeCompare(hw[:], hg[:]) == 1
}

// newRemoteServer 建远端 http.Server。不设 WriteTimeout：/events 长轮询要等 60 秒。
func newRemoteServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
}

// opCtx 给一次远端操作配上会话 ctx + 超时。
func (sess *session) opCtx(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(sess.ctx, d)
}

// bindSession 返回一个同时跟随 parent 和会话 ctx 的 ctx。
func bindSession(parent context.Context, sess *session) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(sess.ctx, cancel)
	return ctx, func() { stop(); cancel() }
}

// MaxMinutes 是一次协助的最长时长（分钟）。
const MaxMinutes = 600

// Start 开启协助。minutes 限 1..MaxMinutes；已在协助中则把截止时间改成「现在+minutes」，
// 保持原远端令牌。Open 或 Publish 失败时关掉已开的东西并保持未开启。
func (s *Service) Start(minutes int) error {
	if minutes < 1 || minutes > MaxMinutes {
		return fmt.Errorf("support: 时长必须 1..%d 分钟，实际 %d", MaxMinutes, minutes)
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()
	dur := time.Duration(minutes) * minuteUnit

	s.mu.Lock()
	if s.active {
		s.until = time.Now().Add(dur)
		if s.timer != nil {
			s.timer.Reset(time.Until(s.until))
		}
		addr, sess, until := s.remoteAddr, s.sess, s.until
		s.mu.Unlock()
		s.logEvent("start", map[string]string{"minutes": strconv.Itoa(minutes), "reason": "延长时长", "addr": addr})
		// 把新的截止时间写到远端；失败只记日志（远端文件里的旧时间只会让 macsup 更早提示过期）
		if addr != "" && sess != nil {
			ctx, cancel := sess.opCtx(remoteOpWait)
			err := s.remote.Publish(ctx, addr, sess.token, until.Unix())
			cancel()
			if err != nil {
				s.logEvent("remote", map[string]string{"event": "republish_failed", "error": err.Error()})
			}
		}
		return nil
	}
	s.mu.Unlock()

	if s.cfg.Prepare != nil {
		if err := s.cfg.Prepare(); err != nil {
			return fmt.Errorf("support: 开启前准备失败: %w", err)
		}
	}

	sctx, scancel := context.WithCancel(context.Background())
	sess := &session{ctx: sctx, cancel: scancel, token: randHex32()}

	ctx, cancel := sess.opCtx(remoteOpWait)
	ln, err := s.remote.Open(ctx)
	cancel()
	if err != nil {
		scancel()
		return fmt.Errorf("support: 开远端监听失败: %w", err)
	}
	addr := ln.Addr().String()
	until := time.Now().Add(dur)
	ctx2, cancel2 := sess.opCtx(remoteOpWait)
	err = s.remote.Publish(ctx2, addr, sess.token, until.Unix())
	cancel2()
	if err != nil {
		_ = ln.Close()
		scancel()
		// Publish 可能已部分写入：尽力撤销一次（忽略结果）
		uctx, ucancel := context.WithTimeout(context.Background(), unpublishWait)
		_ = s.remote.Unpublish(uctx)
		ucancel()
		return fmt.Errorf("support: 发布远端入口失败: %w", err)
	}

	srv := newRemoteServer(s.remoteMux(sess))
	s.mu.Lock()
	s.active = true
	s.until = until
	s.sess = sess
	s.remoteAddr = addr
	s.srv = srv
	s.ln = ln
	s.timer = time.AfterFunc(time.Until(s.until), func() { s.stop("到时自动结束", sess) })
	sess.wg.Add(1) // 在 opMu 内、Stop 不可能同时进行，Add 安全
	s.mu.Unlock()

	go s.serveLoop(sess, ln, srv)
	s.logEvent("start", map[string]string{"minutes": strconv.Itoa(minutes), "addr": addr})
	return nil
}

// Stop 结束协助：关远端入口、停 Claude、拒绝所有待批。幂等。
func (s *Service) Stop(reason string) { s.stop(reason, nil) }

// stop 是 Stop 的实现。expired 非 nil 表示由该会话的到点计时器触发：
// 只有该会话仍是当前会话且确已到点才停（Start 延长后直接返回）。
//
// 顺序：标记结束并 cancel 会话 ctx → 拒绝所有待批 → 关远端 srv/ln →
// 等会话内所有 goroutine/handler 退出（此时不持有 mu；opMu 只被 Start/Stop 持有，
// 会话内的代码都不拿它）→ Unpublish → 停 Claude。
func (s *Service) stop(reason string, expired *session) {
	s.opMu.Lock()
	defer s.opMu.Unlock()

	s.mu.Lock()
	if !s.active || s.sess == nil {
		s.mu.Unlock()
		return
	}
	if expired != nil && (s.sess != expired || time.Now().Before(s.until)) {
		s.mu.Unlock()
		return
	}
	sess, srv, ln, timer := s.sess, s.srv, s.ln, s.timer
	s.active = false
	s.remoteAddr = ""
	s.sess, s.srv, s.ln, s.timer = nil, nil, nil, nil
	sess.cancel()
	s.mu.Unlock()

	go s.runner.Stop() // 让 Claude 尽快停；下面还有同步的 Stop 兜底（Runner.Stop 可并发、幂等）
	if timer != nil {
		timer.Stop()
	}
	// CancelAll 让 Ask 正常返回拒绝（不带错误），钩子那边不会记日志，所以在这里逐条记
	for _, p := range s.queue.Pending() {
		s.logEvent("cancelled", map[string]string{"id": p.ID, "tool": p.Tool, "reason": "协助已结束"})
	}
	s.queue.CancelAll("协助已结束")
	if srv != nil {
		_ = srv.Close() // 顺带关掉 listener 上的活动连接
	}
	if ln != nil {
		_ = ln.Close()
	}

	// 等在途的远端 handler、钩子、serveLoop/重连 goroutine 全部退出。
	// 万一有 handler 卡在向 Claude 写标准输入，先停 Claude 把它放出来。
	done := make(chan struct{})
	go func() { sess.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(stopDrainWait):
		_ = s.runner.Stop()
		<-done
	}

	ctx, cancel := context.WithTimeout(context.Background(), unpublishWait)
	err := s.remote.Unpublish(ctx)
	cancel()
	if err != nil {
		s.logEvent("remote", map[string]string{"event": "unpublish_failed", "error": err.Error()})
	}
	_ = s.runner.Stop()
	s.queue.CancelAll("协助已结束")
	s.logEvent("stop", map[string]string{"reason": reason})
}

// Close 等同 Stop 并释放资源（日志文件句柄）。
func (s *Service) Close() {
	s.Stop("正在关闭")
	s.logMu.Lock()
	defer s.logMu.Unlock()
	s.closed = true
	if s.logFile != nil {
		_ = s.logFile.Close()
		s.logFile = nil
	}
}

// Status 返回状态快照。
func (s *Service) Status() Status {
	claude := s.runner.State()
	s.mu.Lock()
	active, addr, until := s.active, s.remoteAddr, s.until
	s.mu.Unlock()
	st := Status{
		Active:     active,
		RemoteAddr: addr,
		Claude:     claude,
		Pending:    s.queue.Pending(),
		Repos:      append([]string{}, s.repos...),
		Projects:   sortedStrings(s.cfg.ProjSyncNames),
	}
	if active {
		st.Until = until.Unix()
	}
	return st
}

// Log 返回内存里最近的日志（最多 200 条），从旧到新。
func (s *Service) Log() []LogEntry {
	s.logMu.Lock()
	defer s.logMu.Unlock()
	return append([]LogEntry{}, s.logRecs...)
}

// HookToken 返回钩子令牌（建服务时生成，不再变化）。
func (s *Service) HookToken() string { return s.hookToken }

// serveLoop 在 listener 上服务远端接口；Accept 出错（隧道断了）且会话没结束时
// 进入重连，成功后换新 listener 继续，直到 Stop。它占会话 wg 的一个名额。
func (s *Service) serveLoop(sess *session, ln net.Listener, srv *http.Server) {
	defer sess.wg.Done()
	for {
		err := srv.Serve(ln)
		_ = srv.Close()
		_ = ln.Close()

		s.mu.Lock()
		cur := s.active && s.sess == sess
		if cur && s.ln == ln {
			s.remoteAddr = ""
		}
		s.mu.Unlock()
		if !cur {
			return // 主动 Stop 关的，不重连
		}
		errText := "listener 正常结束"
		if err != nil {
			errText = err.Error()
		}
		s.logEvent("remote", map[string]string{"event": "disconnected", "error": errText})
		ln, srv = s.reconnectLoop(sess)
		if ln == nil {
			return
		}
	}
}

// reconnectLoop 每 reconnectDelay 重试 Open+Publish，成功后换入新 listener 并返回；
// 会话结束时返回 nil。Open/Publish 用会话 ctx；期间被 Stop 时只关自己的 srv/ln，
// 撤销入口由 Stop 在等完所有 goroutine 之后统一做。
func (s *Service) reconnectLoop(sess *session) (net.Listener, *http.Server) {
	for {
		select {
		case <-sess.ctx.Done():
			return nil, nil
		case <-time.After(reconnectDelay):
		}
		ctx, cancel := sess.opCtx(remoteOpWait)
		ln, err := s.remote.Open(ctx)
		cancel()
		if err != nil {
			s.logEvent("remote", map[string]string{"event": "reconnect_failed", "error": err.Error()})
			continue
		}
		addr := ln.Addr().String()
		s.mu.Lock()
		until := s.until
		s.mu.Unlock()
		ctx2, cancel2 := sess.opCtx(remoteOpWait)
		err = s.remote.Publish(ctx2, addr, sess.token, until.Unix())
		cancel2()
		if err != nil {
			_ = ln.Close()
			s.logEvent("remote", map[string]string{"event": "republish_failed", "error": err.Error()})
			continue
		}
		srv := newRemoteServer(s.remoteMux(sess))

		s.mu.Lock()
		swapped := s.active && s.sess == sess
		if swapped {
			s.ln, s.srv, s.remoteAddr = ln, srv, addr
		}
		s.mu.Unlock()
		if !swapped {
			_ = srv.Close()
			_ = ln.Close()
			return nil, nil
		}
		s.logEvent("remote", map[string]string{"event": "reconnected", "addr": addr})
		return ln, srv
	}
}

// ---------- 远端接口 ----------

// remoteMux 是某个会话的远端接口；只有 GET /status、POST /task、GET /events、POST /sync、
// POST /stop-claude、POST /projsync，绝不含开始/停止协助或批准请求。
func (s *Service) remoteMux(sess *session) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/status", only(http.MethodGet, s.remoteStatus))
	mux.HandleFunc("/task", only(http.MethodPost, s.remoteTask(sess)))
	mux.HandleFunc("/events", only(http.MethodGet, s.remoteEvents))
	mux.HandleFunc("/sync", only(http.MethodPost, s.remoteSync))
	mux.HandleFunc("/stop-claude", only(http.MethodPost, s.remoteStopClaude))
	mux.HandleFunc("/projsync", only(http.MethodPost, s.remoteProjSync))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		errJSON(w, http.StatusNotFound, "没有这个路径")
	})
	return s.guard(sess, mux)
}

// guard 校验远端令牌，并把请求登记进会话 wg：会话已不是当前会话（Stop 过 / 被新会话取代）
// 就拒绝；通过后的 handler 收到的 ctx 同时跟随连接和会话，Stop 会等它退出。
func (s *Service) guard(sess *session, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		auth := r.Header.Get("Authorization")
		got := ""
		if len(auth) > len(prefix) && strings.EqualFold(auth[:len(prefix)], prefix) {
			got = strings.TrimSpace(auth[len(prefix):])
		}
		if !tokenMatch(sess.token, got) {
			errJSON(w, http.StatusUnauthorized, "令牌不对")
			return
		}
		s.mu.Lock()
		ok := s.active && s.sess == sess
		if ok {
			sess.wg.Add(1) // 与 Stop 的 wg.Wait 互斥：Stop 先在 mu 内清掉 s.sess，之后这里必不通过
		}
		s.mu.Unlock()
		if !ok {
			errJSON(w, http.StatusServiceUnavailable, "协助已结束")
			return
		}
		defer sess.wg.Done()
		ctx, cancel := bindSession(r.Context(), sess)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Service) remoteStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Status())
}

func (s *Service) remoteTask(sess *session) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxTaskBytes+1))
		if err != nil {
			errJSON(w, http.StatusBadRequest, "读请求体失败")
			return
		}
		if len(body) > maxTaskBytes {
			errJSON(w, http.StatusRequestEntityTooLarge, "任务文字太长，最多 64KB")
			return
		}
		text := strings.TrimSpace(string(body))
		detail := map[string]string{"text": truncRunes(text, maxLogText)}
		if text == "" {
			detail["result"] = "空任务"
			s.logEvent("task", detail)
			errJSON(w, http.StatusBadRequest, "任务文字为空")
			return
		}
		// Send 在进程没跑时会重新拉起 Claude，所以发之前必须确认会话还在。
		// 之后若 Stop 抢进来，它会等本 handler 退出再 runner.Stop，不会残留进程。
		if sess.ctx.Err() != nil {
			detail["result"] = "协助已结束"
			s.logEvent("task", detail)
			errJSON(w, http.StatusServiceUnavailable, "协助已结束")
			return
		}
		if err := s.runner.Send(text); err != nil {
			detail["result"] = "发送失败"
			s.logEvent("task", detail)
			errJSON(w, http.StatusBadGateway, "发送任务失败")
			return
		}
		detail["result"] = "已发送"
		s.logEvent("task", detail)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

func (s *Service) remoteEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	after, err := strconv.ParseInt(q.Get("after"), 10, 64)
	if err != nil && q.Get("after") != "" {
		errJSON(w, http.StatusBadRequest, "after 必须是整数")
		return
	}
	limit, err := strconv.Atoi(q.Get("limit"))
	if err != nil && q.Get("limit") != "" {
		errJSON(w, http.StatusBadRequest, "limit 必须是整数")
		return
	}
	waitStr := q.Get("wait")
	secs := 0.0
	if waitStr != "" {
		secs, err = strconv.ParseFloat(waitStr, 64)
		if err != nil {
			errJSON(w, http.StatusBadRequest, "wait 必须是秒数")
			return
		}
		if !(secs >= 0 && secs <= 60) { // NaN 也拒绝
			errJSON(w, http.StatusBadRequest, "wait 必须 0..60 秒")
			return
		}
	}
	ev, last := s.runner.Events(r.Context(), after, limit, time.Duration(secs*float64(time.Second)))
	if ev == nil {
		ev = []Event{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": ev, "last": last})
}

func (s *Service) remoteSync(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	name := q.Get("repo")
	repo, ok := s.cfg.Repos[name]
	if !ok {
		errJSON(w, http.StatusNotFound, "未知仓库")
		return
	}
	branch := q.Get("branch")
	if err := ValidBranch(branch); err != nil {
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	detail := map[string]string{"repo": name, "branch": branch}
	if s.runner.State().Busy {
		detail["result"] = "Claude 正在工作"
		s.logEvent("sync", detail)
		errJSON(w, http.StatusConflict, "Mac 上的 Claude 正在工作，稍后再同步")
		return
	}
	lock := s.syncLocks[name]
	lock.Lock() // 同一仓库的同步串行
	defer lock.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), syncTimeout) // r.Context 已跟随会话
	defer cancel()
	if ctx.Err() != nil {
		detail["result"] = "已取消"
		s.logEvent("sync", detail)
		errJSON(w, http.StatusServiceUnavailable, "协助已结束")
		return
	}
	head, err := Sync(ctx, repo, branch, s.cfg.GitEnv)
	switch {
	case err == nil:
		detail["result"] = "ok"
		detail["head"] = head
		s.logEvent("sync", detail)
		writeJSON(w, http.StatusOK, map[string]string{"head": head})
	case errors.Is(err, ErrDirty):
		detail["result"] = "工作区不干净"
		s.logEvent("sync", detail)
		errJSON(w, http.StatusConflict, "工作区有未提交的改动")
	case errors.Is(err, ErrNotFastForward):
		detail["result"] = "不能快进"
		s.logEvent("sync", detail)
		errJSON(w, http.StatusConflict, "本地分支不能快进到远端")
	default:
		detail["result"] = "同步失败"
		s.logEvent("sync", detail)
		errJSON(w, http.StatusBadGateway, "同步失败")
	}
}

func (s *Service) remoteStopClaude(w http.ResponseWriter, r *http.Request) {
	if err := s.runner.Stop(); err != nil {
		errJSON(w, http.StatusBadGateway, "停止 Claude 失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Service) remoteProjSync(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	detail := map[string]string{"project": truncRunes(name, 100)}

	// ProjSync 未配置或名字不在白名单 → 404
	if s.cfg.ProjSync == nil {
		detail["result"] = "未配置"
		s.logEvent("projsync", detail)
		errJSON(w, http.StatusNotFound, "未知项目")
		return
	}
	ok := false
	for _, n := range s.cfg.ProjSyncNames {
		if n == name {
			ok = true
			break
		}
	}
	if !ok {
		detail["result"] = "未知项目"
		s.logEvent("projsync", detail)
		errJSON(w, http.StatusNotFound, "未知项目")
		return
	}

	body, err := json.Marshal(map[string]string{"project": name})
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "内部错误")
		return
	}

	s.logEvent("projsync", map[string]string{"project": name, "result": "requested"})

	// 待批数已达上限 → 直接拒绝
	if len(s.queue.Pending()) >= maxPending {
		detail["result"] = "待批太多"
		s.logEvent("projsync", detail)
		errJSON(w, http.StatusServiceUnavailable, "待批请求太多")
		return
	}

	// 等用户决定：用 HookWait 的上限（<=0 用默认值）。r.Context() 已由 guard 绑定会话，Stop 会取消它
	wait := s.cfg.HookWait
	if wait <= 0 {
		wait = defaultHookWait
	}
	ctx, cancel := context.WithTimeout(r.Context(), wait)
	defer cancel()

	decision, err := s.queue.AskWithNotify(ctx, "ProjectSync", json.RawMessage(body), func(req Request) {
		s.logEvent("ask", map[string]string{
			"id":      req.ID,
			"tool":    "ProjectSync",
			"summary": req.Summary,
		})
	})
	if err != nil {
		// Ask 返回错误 = ctx 先到期（超时或取消），decision 是零值 Allow=false
		switch {
		case r.Context().Err() != nil:
			detail["result"] = "cancelled"
			s.logEvent("projsync", detail)
			errJSON(w, http.StatusServiceUnavailable, "请求已取消")
		default:
			// ctx.Err() != nil 但 r.Context() 没断：超时（HookWait）
			detail["result"] = "timeout"
			s.logEvent("projsync", detail)
			errJSON(w, http.StatusGatewayTimeout, "等待授权超时")
		}
		return
	}

	// Ask 返回 nil err：用户明确决定
	if !decision.Allow {
		detail["result"] = "denied"
		if decision.Message != "" {
			detail["message"] = decision.Message
		}
		s.logEvent("projsync", detail)
		msg := "用户拒绝了同步"
		if decision.Message != "" {
			msg += "：" + decision.Message
		}
		errJSON(w, http.StatusForbidden, msg)
		return
	}

	// 用户允许：调 ProjSync（30 分钟上限）
	ctx2, cancel2 := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel2()
	result, err := s.cfg.ProjSync(ctx2, name)
	detail = map[string]string{"project": name}
	if err != nil {
		detail["result"] = "failed"
		detail["error"] = truncRunes(err.Error(), 300)
		s.logEvent("projsync", detail)
		errJSON(w, http.StatusBadGateway, "同步失败："+err.Error())
		return
	}
	detail["result"] = "ok"
	s.logEvent("projsync", detail)
	writeJSON(w, http.StatusOK, map[string]any{"result": result})
}

// ---------- 本机接口 ----------

// localStatus 是本机 /support 的正文：Status 外加最近 50 条日志。
type localStatus struct {
	Status
	Log []LogEntry `json:"log"`
}

// LocalHandler 是本机接口（由 wgtund 挂在它的令牌校验之后，这里不再校验令牌）。
func (s *Service) LocalHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/support", only(http.MethodGet, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, localStatus{Status: s.Status(), Log: s.logSnapshot(50)})
	}))
	mux.HandleFunc("/support/start", only(http.MethodPost, func(w http.ResponseWriter, r *http.Request) {
		minutes, err := strconv.Atoi(r.URL.Query().Get("minutes"))
		if err != nil || minutes < 1 || minutes > MaxMinutes {
			errJSON(w, http.StatusBadRequest, fmt.Sprintf("minutes 必须是 1..%d 的整数", MaxMinutes))
			return
		}
		if err := s.Start(minutes); err != nil {
			errJSON(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, s.Status())
	}))
	mux.HandleFunc("/support/stop", only(http.MethodPost, func(w http.ResponseWriter, r *http.Request) {
		s.Stop("用户停止")
		writeJSON(w, http.StatusOK, s.Status())
	}))
	mux.HandleFunc("/support/decide", only(http.MethodPost, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		id := q.Get("id")
		allow := q.Get("allow")
		if allow != "1" && allow != "0" {
			errJSON(w, http.StatusBadRequest, "allow 必须是 1 或 0")
			return
		}
		message := truncRunes(q.Get("message"), maxDecideMessage)
		detail := map[string]string{"id": id, "allow": allow, "message": message}
		for _, p := range s.queue.Pending() {
			if p.ID == id {
				detail["tool"], detail["summary"] = p.Tool, p.Summary
				break
			}
		}
		err := s.queue.Decide(id, Decision{Allow: allow == "1", Message: message})
		switch {
		case err == nil:
			detail["result"] = "ok"
		case errors.Is(err, ErrNotFound):
			detail["result"] = "not_found"
		default:
			detail["result"] = "error"
		}
		s.logEvent("decide", detail)
		if errors.Is(err, ErrNotFound) {
			errJSON(w, http.StatusNotFound, "没有这条待批请求")
			return
		}
		if err != nil {
			errJSON(w, http.StatusInternalServerError, "决定失败")
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}))
	return mux
}

// ---------- 钩子接口 ----------

// HookHandler 是 Claude Code 钩子的入口：POST /support/hook?token=<HookToken>。
// 阻塞到用户决定或超时；拿不到决定一律拒绝。
func (s *Service) HookHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			errJSON(w, http.StatusMethodNotAllowed, "方法不对")
			return
		}
		if r.URL.Path != "/support/hook" {
			errJSON(w, http.StatusNotFound, "没有这个路径")
			return
		}
		if !tokenMatch(s.hookToken, r.URL.Query().Get("token")) {
			errJSON(w, http.StatusUnauthorized, "令牌不对")
			return
		}

		deny := func(code int, msg string) {
			writeJSON(w, code, json.RawMessage(HookOutput(Decision{Allow: false, Message: msg})))
		}
		s.mu.Lock()
		active := s.active
		s.mu.Unlock()
		if !active {
			deny(http.StatusOK, "协助未开启")
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, maxHookBytes+1))
		if err != nil || len(body) > maxHookBytes {
			deny(http.StatusBadRequest, "钩子输入无法解析")
			return
		}
		h, err := ParseHookInput(body)
		if err != nil {
			deny(http.StatusBadRequest, "钩子输入无法解析")
			return
		}

		// 读完正文后再登记会话（读正文可能很慢，不能拖住 Stop）。
		// 同一把 mu 里取 active 和当前会话：Stop 之后到达的钩子在这里立即被拒绝。
		s.mu.Lock()
		sess := s.sess
		ok := s.active && sess != nil
		if ok {
			sess.wg.Add(1)
		}
		s.mu.Unlock()
		if !ok {
			deny(http.StatusOK, "协助已结束")
			return
		}
		defer sess.wg.Done()
		if sess.ctx.Err() != nil {
			deny(http.StatusOK, "协助已结束")
			return
		}

		if len(s.queue.Pending()) >= maxPending {
			s.logEvent("ask_rejected", map[string]string{"tool": h.Tool, "summary": Summarize(h.Tool, h.Input), "reason": "待批请求太多"})
			deny(http.StatusOK, "待批请求太多")
			return
		}

		wait := s.cfg.HookWait
		if wait <= 0 {
			wait = defaultHookWait
		}
		// ctx = 请求 ctx + 会话 ctx + HookWait
		ctx, cancel := bindSession(r.Context(), sess)
		defer cancel()
		ctx, cancel2 := context.WithTimeout(ctx, wait)
		defer cancel2()

		id := ""
		d, err := s.queue.AskWithNotify(ctx, h.Tool, h.Input, func(req Request) {
			id = req.ID
			s.logEvent("ask", map[string]string{
				"id":         req.ID,
				"tool":       h.Tool,
				"summary":    req.Summary,
				"session_id": h.SessionID,
			})
		})
		if err != nil {
			switch {
			case sess.ctx.Err() != nil:
				s.logEvent("cancelled", map[string]string{"id": id, "tool": h.Tool, "reason": "协助已结束"})
				d = Decision{Allow: false, Message: "协助已结束"}
			case r.Context().Err() != nil:
				s.logEvent("cancelled", map[string]string{"id": id, "tool": h.Tool, "reason": "客户端断开"})
				d = Decision{Allow: false, Message: "请求已取消"}
			default:
				s.logEvent("timeout", map[string]string{"id": id, "tool": h.Tool})
				d = Decision{Allow: false, Message: "等待授权超时"}
			}
		}
		writeJSON(w, http.StatusOK, json.RawMessage(HookOutput(d)))
	})
}

// ---------- 日志 ----------

// logEvent 追加一条日志：内存（保留最近 200 条）+ LogPath（写失败不影响功能）。
func (s *Service) logEvent(kind string, detail map[string]string) {
	if detail == nil {
		detail = map[string]string{}
	}
	e := LogEntry{Time: time.Now(), Kind: kind, Detail: detail}
	s.logMu.Lock()
	defer s.logMu.Unlock()
	s.logRecs = append(s.logRecs, e)
	if over := len(s.logRecs) - maxLogEntries; over > 0 {
		s.logRecs = append(s.logRecs[:0], s.logRecs[over:]...)
	}
	if s.cfg.LogPath == "" {
		return
	}
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	b = append(b, '\n')
	if s.closed {
		return
	}
	if s.logFile == nil {
		f, err := os.OpenFile(s.cfg.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return
		}
		if err := f.Chmod(0o600); err != nil { // 已存在的宽权限文件也收紧；收不紧就不写
			_ = f.Close()
			return
		}
		s.logFile = f
	}
	if _, err := s.logFile.Write(b); err != nil {
		_ = s.logFile.Close()
		s.logFile = nil
	}
}

// logSnapshot 返回最近 n 条日志的拷贝，从旧到新。
func (s *Service) logSnapshot(n int) []LogEntry {
	s.logMu.Lock()
	defer s.logMu.Unlock()
	if len(s.logRecs) > n {
		return append([]LogEntry{}, s.logRecs[len(s.logRecs)-n:]...)
	}
	return append([]LogEntry{}, s.logRecs...)
}

// ---------- HTTP 小工具 ----------

// sortedStrings 返回排序后的字符串切片副本。
func sortedStrings(s []string) []string {
	out := append([]string{}, s...)
	sort.Strings(out)
	return out
}

// only 校验请求方法，不对就 405。
func only(method string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			errJSON(w, http.StatusMethodNotAllowed, "方法不对")
			return
		}
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func errJSON(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// truncRunes 按 rune 截断到 n 个字符。
func truncRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
