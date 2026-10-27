package wgtund

// projfiles_http.go：项目文件同步（projfiles）的本机 HTTP 接口与每日回收清理（T11b）。
//
// 全部挂在 wgtund 现有的本机 HTTP 上（主令牌在 wgtund.go 的 guarded 里统一校验，缺/错一律 401）：
//
//	GET  /projfiles/status                  状态（与 ~/.xxyi/projfiles/status/_round.json 同一份，外加每日回收清理结果）
//	POST /projfiles/net    {net, at}       Swift 上报网络状态（net：good/expensive/unknown/offline）
//	POST /projfiles/sync                    立即同步：后台跑一轮；已在跑回 busy（409）
//	POST /projfiles/fresh  {paths, budget_s}
//	                                        取最新（budget_s 缺省 60、最大 120）；返回 PFFreshResult 原样
//	POST /projfiles/touched {paths}        记下这些路径所在的单元刚被写过
//	POST /projfiles/unit/start {rel}       开始同步一个项目（PFUnitMoveResult 原样）
//	POST /projfiles/unit/stop  {code}      取消同步
//	POST /projfiles/unit/create {rel}      新建一个已同步的项目
//	GET  /projfiles/unit?code=             这个单元最近一轮的结果与 Aligned（给以后的归档判断用）
//	POST /projfiles/unit/release|restore   以后做（501）
//	POST /projfiles/conflict/resolve       界面定了再做（501）
//
// 鉴权只认令牌、不查 Origin：这些调用来自 cmdproto 起的子进程和本机命令行，
// 浏览器页面那套（要白名单 Origin）是 projsync v2 的接口，两者不要混。
// 单元读写都先 acquire() 占住轮次（占不到回 busy，让调用方稍后再试），做完释放，
// 免得和定时的那一轮同时动同一个项目。
//
// 每日回收清理（T11a 没做的）：包在 runRound 外面——每天第一轮结束后清 Mac 本机回收区
// （recycle_days 少于 7 按 7），并给 Ubuntu 发一次 apply 步骤 recycle_clean（days 同上）；
// 结果记进状态（recycle）。成败都算「今天清过」，失败等明天第一轮再试。

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

	"github.com/WillyChen0811/base/go/projsync"
)

const (
	projfilesAPIFileName = "projfiles-api.json" // ~/.xxyi/projfiles-api.json：本机接口地址与令牌

	projfilesFreshDefaultBudgetS = 60  // fresh 的缺省预算（秒）
	projfilesFreshMaxBudgetS     = 120 // fresh 的预算上限（秒）
	projfilesRecycleMinDays      = projsync.PFRecycleMinDays
	projfilesMoveTimeout         = 5 * time.Minute // 单元搬运（扫描/挪目录）的上限
	projfilesMaxBodyBytes        = 1 << 20         // 请求体上限 1MiB
)

// projfilesCodeRe 与 projsync 的单元键一致：大写字母和数字，1–64 个。
var projfilesCodeRe = regexp.MustCompile(`^[A-Z0-9]{1,64}$`)

// projfilesFresh 是接口用到的 PFFresh 的那一小块面（测试换成假的）。
type projfilesFresh interface {
	Paths(ctx context.Context, paths []string, budget time.Duration) projsync.PFFreshResult
	Touched(paths []string) []string
}

// projfilesRecycleResult 是一次每日回收清理的结果，记进状态给人看。
type projfilesRecycleResult struct {
	At         time.Time `json:"at"`
	Days       int       `json:"days"` // 实际用的保留天数（不少于 7）
	LocalRemoved []string `json:"local_removed,omitempty"`
	LocalFiles   int     `json:"local_files,omitempty"`
	LocalBytes   int64   `json:"local_bytes,omitempty"`
	LocalError   string  `json:"local_error,omitempty"`
	// Remote 是 Ubuntu apply 的原始结果；连不上/被拒只记 RemoteError，不影响本机清理。
	Remote      *projsync.PFRemoteApplyResult `json:"remote,omitempty"`
	RemoteError string                        `json:"remote_error,omitempty"`
}

// projfilesRecycler 管「每天第一轮结束后清一次回收区」。
type projfilesRecycler struct {
	svc *projfilesSvc
	now func() time.Time

	// 注入点（测试换掉）；生产用 projsync 真实现。
	cleanLocal  func(dir string, days int) (projsync.PFRecycleCleanResult, error)
	cleanRemote func(ctx context.Context, days int) (*projsync.PFRemoteApplyResult, error)

	mu      sync.Mutex
	lastDay string // 今天是否已经清过（YYYY-MM-DD）；失败的也记，等明天再试
	last    *projfilesRecycleResult
}

func newProjfilesRecycler(s *projfilesSvc) *projfilesRecycler {
	r := &projfilesRecycler{svc: s, now: s.now}
	r.cleanLocal = func(dir string, days int) (projsync.PFRecycleCleanResult, error) {
		rc, err := projsync.NewPFRecycle(dir, projsync.PFRecycleOptions{Now: r.now})
		if err != nil {
			return projsync.PFRecycleCleanResult{}, err
		}
		return rc.Clean(days)
	}
	r.cleanRemote = func(ctx context.Context, days int) (*projsync.PFRemoteApplyResult, error) {
		remote := s.remoteNow()
		if remote == nil {
			return nil, errors.New("还没有 Ubuntu 客户端")
		}
		return remote.Apply(ctx, projsync.PFRemoteApplyOptions{Round: "recycle-" + r.now().Format("20060102-150405")},
			[]projsync.PFRemoteStep{{Op: projsync.PFRemoteOpRecycleClean, Days: days}})
	}
	return r
}

// wrapRound 把回收清理包在轮次执行外面：orig 跑完（无论成败）再清，一轮结束才算「今天的第一轮」。
func (r *projfilesRecycler) wrapRound(orig func(ctx context.Context, opt projsync.PFSchedRoundOptions) *projsync.PFSchedReport) func(ctx context.Context, opt projsync.PFSchedRoundOptions) *projsync.PFSchedReport {
	return func(ctx context.Context, opt projsync.PFSchedRoundOptions) *projsync.PFSchedReport {
		rep := orig(ctx, opt)
		r.afterRound(ctx)
		return rep
	}
}

// afterRound 每天只做一次：占住「今天」再清，避免同一天反复扫回收区。
func (r *projfilesRecycler) afterRound(ctx context.Context) {
	if ctx.Err() != nil {
		return // 正在退出，清理留给明天第一轮
	}
	now := r.now()
	day := now.Format("2006-01-02")
	r.mu.Lock()
	if r.lastDay == day {
		r.mu.Unlock()
		return
	}
	r.lastDay = day
	r.mu.Unlock()

	days := r.svc.settingsNow().RecycleDays
	if days < projfilesRecycleMinDays {
		days = projfilesRecycleMinDays
	}
	res := &projfilesRecycleResult{At: now, Days: days}
	lr, err := r.cleanLocal(projsync.PFRoundMacRecycleDir(r.svc.prep.metaDir), days)
	if err != nil {
		res.LocalError = err.Error()
	} else {
		res.LocalRemoved, res.LocalFiles, res.LocalBytes = lr.Removed, lr.Files, lr.Bytes
	}
	rr, err := r.cleanRemote(ctx, days)
	if err != nil {
		res.RemoteError = err.Error()
	} else {
		res.Remote = rr
	}
	r.mu.Lock()
	r.last = res
	r.mu.Unlock()
}

func (r *projfilesRecycler) lastResult() *projfilesRecycleResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}

// projfilesStatusView 是 GET /projfiles/status 的内容：状态本体 + 每日回收清理结果。
type projfilesStatusView struct {
	projfilesStatus
	Recycle *projfilesRecycleResult `json:"recycle,omitempty"`
}

// projfilesAPI 是本机接口：只引用服务，不碰它的定时与状态（那些在 projfiles.go）。
type projfilesAPI struct {
	svc *projfilesSvc
	rec *projfilesRecycler

	// newFresh 建「取最新」；测试换成假的（假 PFSched 不必真连机器）。
	newFresh func(s *projsync.PFSched, syncDir string) (projfilesFresh, error)

	mu    sync.Mutex
	fresh projfilesFresh // PFFresh 复用；整套重建（builtGen 变了）后作废重建
	freshGen uint64       // h.fresh 建出来时对应的 svc.builtGen
}

func newProjfilesAPI(s *projfilesSvc, rec *projfilesRecycler) *projfilesAPI {
	return &projfilesAPI{svc: s, rec: rec,
		newFresh: func(sc *projsync.PFSched, syncDir string) (projfilesFresh, error) {
			return projsync.NewPFFresh(sc, projsync.PFFreshConfig{SyncDir: syncDir})
		}}
}

// ── 小工具 ──────────────────────────────────────────────

// needMethod 统一方法校验。
func needMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method != method {
		errJSON(w, http.StatusMethodNotAllowed, "请用 "+method)
		return false
	}
	return true
}

// busyJSON 回「正忙」（409）：调用方稍后再试。
func busyJSON(w http.ResponseWriter, why string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	json.NewEncoder(w).Encode(map[string]any{"busy": true, "reason": why})
}

// decodeBody 解 JSON 请求体（限 1MiB，允许多余键：Swift 那边字段会变）。
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	defer r.Body.Close()
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, projfilesMaxBodyBytes)).Decode(v); err != nil {
		errJSON(w, http.StatusBadRequest, "请求体不是合法的 JSON: "+err.Error())
		return false
	}
	return true
}

// pathsArg 校验路径列表：非空、每条都是合法绝对路径。
func pathsArg(v []string) ([]string, error) {
	if len(v) == 0 {
		return nil, errors.New("paths 不能为空")
	}
	for _, p := range v {
		if p == "" || !filepath.IsAbs(p) || strings.ContainsRune(p, 0) || !utf8.ValidString(p) {
			return nil, fmt.Errorf("paths 里的 %q 不是合法的绝对路径", p)
		}
	}
	return v, nil
}

// relArg 校验单元相对路径：非空、不许是绝对路径、不能含 .. 段。
func relArg(s string) error {
	if s == "" || strings.ContainsRune(s, 0) || !utf8.ValidString(s) {
		return errors.New("rel 不能为空且不能含控制字符")
	}
	if filepath.IsAbs(s) || strings.HasPrefix(s, "/") {
		return errors.New("rel 必须是同步根下面的相对路径")
	}
	for _, seg := range strings.Split(filepath.ToSlash(s), "/") {
		if seg == ".." {
			return errors.New("rel 不能含 ..")
		}
	}
	return nil
}

// ensureBuilt 保证零件已建：没启用定时循环（或还没到第一轮）时，手动接口也要能用。
func (h *projfilesAPI) ensureBuilt() error {
	st, _ := loadProjfilesSettings(h.svc.prep.settingsPath)
	if err := h.svc.build(st); err != nil {
		return fmt.Errorf("零件还没建成：%w", err)
	}
	return nil
}

// getFresh 拿到复用的 PFFresh（第一次用时建，之后复用同一套）。跟 build 共用 buildMu，
// 保证「检查 built → 建 → 发布」串行；切换期（目录改了还没重建完）一律拒绝，避免拿旧 sched 建新 fresh。
// builtGen 变了说明整套已重建（旧 sched 已被关掉），缓存的 fresh 必须作废重建，不能继续绑旧的。
func (h *projfilesAPI) getFresh() (projfilesFresh, error) {
	// 切换期先拒（含「改了设置但 tick 还没跑到」的窗口）——已缓存的 fresh 绑的是旧目录那一套，
	// 这时命中缓存也不能给用。
	if why := h.dirSwitchWhy(); why != "" {
		return nil, errors.New(why)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.fresh != nil && h.freshGen == h.svc.builtGen.Load() {
		return h.fresh, nil
	}
	h.fresh = nil
	if err := h.ensureBuilt(); err != nil {
		return nil, err
	}
	// sched 与代次同刻取：ensureBuilt 之后若又并发重建了一次，代次对不上就重来，
	// 不会把旧 sched 绑进缓存。
	sched, gen := h.svc.schedGen()
	if sched == nil {
		return nil, errors.New("调度器还没建起来")
	}
	// 用「实际在用」的 sync_dir（build 已经记下来的），不拿设置文件的旧值——
	// tick 刚改过设置但还没重建时，设置里的 sync_dir 已经不是当前 sched 用的那个。
	_, syncDir := h.svc.activeDirs()
	if syncDir == "" {
		return nil, errors.New("设置里没有 sync_dir，取最新用不了")
	}
	f, err := h.newFresh(sched, syncDir)
	if err != nil {
		return nil, err
	}
	h.fresh, h.freshGen = f, gen
	return f, nil
}

func (h *projfilesAPI) statusView() projfilesStatusView {
	v := projfilesStatusView{projfilesStatus: h.svc.status()}
	if h.rec != nil {
		v.Recycle = h.rec.lastResult()
	}
	return v
}

// ── 接口 ────────────────────────────────────────────────

func (h *projfilesAPI) localHandler(ctx context.Context) http.Handler {
	mux := http.NewServeMux()
	notYet := func(why string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !needMethod(w, r, http.MethodPost) {
				return
			}
			errJSON(w, http.StatusNotImplemented, why)
		}
	}

	mux.HandleFunc("/projfiles/status", func(w http.ResponseWriter, r *http.Request) {
		if !needMethod(w, r, http.MethodGet) {
			return
		}
		writeJSON(w, h.statusView())
	})

	mux.HandleFunc("/projfiles/net", func(w http.ResponseWriter, r *http.Request) {
		if !needMethod(w, r, http.MethodPost) {
			return
		}
		var body struct {
			Net string `json:"net"`
			At  string `json:"at"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		if !projfilesNetValid(body.Net) {
			errJSON(w, http.StatusBadRequest, "net 只能是 good/expensive/unknown/offline")
			return
		}
		at := h.svc.now()
		if body.At != "" {
			t, err := time.Parse(time.RFC3339, body.At)
			if err != nil {
				errJSON(w, http.StatusBadRequest, "at 要用 RFC3339: "+err.Error())
				return
			}
			at = t
		}
		h.svc.setNet(body.Net, at)
		writeJSON(w, map[string]any{"net": body.Net, "at": at.Format(time.RFC3339)})
	})

	mux.HandleFunc("/projfiles/sync", func(w http.ResponseWriter, r *http.Request) {
		if !needMethod(w, r, http.MethodPost) {
			return
		}
		if h.svc.isRunning() {
			busyJSON(w, "上一轮还在进行")
			return
		}
		go h.svc.tick(ctx, "立即同步")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]any{"started": true})
	})

	mux.HandleFunc("/projfiles/fresh", func(w http.ResponseWriter, r *http.Request) {
		if !needMethod(w, r, http.MethodPost) {
			return
		}
		var body struct {
			Paths   []string `json:"paths"`
			BudgetS *int     `json:"budget_s"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		paths, err := pathsArg(body.Paths)
		if err != nil {
			errJSON(w, http.StatusBadRequest, err.Error())
			return
		}
		budget := projfilesFreshDefaultBudgetS
		if body.BudgetS != nil {
			if *body.BudgetS < 1 || *body.BudgetS > projfilesFreshMaxBudgetS {
				errJSON(w, http.StatusBadRequest, fmt.Sprintf("budget_s 要在 1..%d 之间", projfilesFreshMaxBudgetS))
				return
			}
			budget = *body.BudgetS
		}
		f, err := h.getFresh()
		if err != nil {
			errJSON(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		writeJSON(w, f.Paths(r.Context(), paths, time.Duration(budget)*time.Second))
	})

	mux.HandleFunc("/projfiles/touched", func(w http.ResponseWriter, r *http.Request) {
		if !needMethod(w, r, http.MethodPost) {
			return
		}
		var body struct {
			Paths []string `json:"paths"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		paths, err := pathsArg(body.Paths)
		if err != nil {
			errJSON(w, http.StatusBadRequest, err.Error())
			return
		}
		f, err := h.getFresh()
		if err != nil {
			errJSON(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		writeJSON(w, map[string]any{"codes": f.Touched(paths)})
	})

	mux.HandleFunc("/projfiles/unit", func(w http.ResponseWriter, r *http.Request) {
		if !needMethod(w, r, http.MethodGet) {
			return
		}
		code := r.URL.Query().Get("code")
		if !projfilesCodeRe.MatchString(code) {
			errJSON(w, http.StatusBadRequest, "code 要是 1..64 位大写字母或数字")
			return
		}
		saved, err := projsync.PFRoundLoadState(h.svc.prep.metaDir, code)
		if err != nil {
			errJSON(w, http.StatusInternalServerError, "读本机单元状态失败: "+err.Error())
			return
		}
		if saved == nil {
			errJSON(w, http.StatusNotFound, "没有这个单元的同步记录")
			return
		}
		writeJSON(w, map[string]any{"code": code, "aligned": saved.Aligned, "last": saved})
	})

	mux.HandleFunc("/projfiles/unit/start", func(w http.ResponseWriter, r *http.Request) {
		if !needMethod(w, r, http.MethodPost) {
			return
		}
		var body struct {
			Rel string `json:"rel"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		if err := relArg(body.Rel); err != nil {
			errJSON(w, http.StatusBadRequest, err.Error())
			return
		}
		h.unitMove(ctx, w, "start", body.Rel)
	})

	mux.HandleFunc("/projfiles/unit/create", func(w http.ResponseWriter, r *http.Request) {
		if !needMethod(w, r, http.MethodPost) {
			return
		}
		var body struct {
			Rel string `json:"rel"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		if err := relArg(body.Rel); err != nil {
			errJSON(w, http.StatusBadRequest, err.Error())
			return
		}
		h.unitMove(ctx, w, "create", body.Rel)
	})

	mux.HandleFunc("/projfiles/unit/stop", func(w http.ResponseWriter, r *http.Request) {
		if !needMethod(w, r, http.MethodPost) {
			return
		}
		var body struct {
			Code string `json:"code"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		if !projfilesCodeRe.MatchString(body.Code) {
			errJSON(w, http.StatusBadRequest, "code 要是 1..64 位大写字母或数字")
			return
		}
		h.unitMove(ctx, w, "stop", body.Code)
	})

	mux.HandleFunc("/projfiles/unit/release", notYet("以后做"))
	mux.HandleFunc("/projfiles/unit/restore", notYet("以后做"))
	mux.HandleFunc("/projfiles/conflict/resolve", notYet("界面定了再做"))
	return mux
}

// unitMove 跑一个单元搬运操作：
//  1. 切换期（目录改了还没重建完）一律 busy——不只看 svc.switchPending 的记号，
//     也主动对比一次磁盘上的设置与已建成的目录，让「改了设置文件但 tick 还没跑到」这段窗口也拒绝；
//  2. 再占住轮次（占不到回 busy，避免与定时的一轮同时动）；
//  3. 若认得出单元编号，再拿住**调度器里的同一把单元锁**——它跟 runOne / PFFresh.online 是同一把，
//     能让「开始/取消同步、新建项目」跟「本轮跑 / 取最新」互斥；
//  4. 调搬运器，结果原样返回。
func (h *projfilesAPI) unitMove(ctx context.Context, w http.ResponseWriter, op, arg string) {
	if why := h.dirSwitchWhy(); why != "" {
		busyJSON(w, why)
		return
	}
	if !h.svc.acquire() {
		busyJSON(w, "有一轮同步或别的单元操作在进行，稍后再试")
		return
	}
	defer h.svc.release()
	if err := h.ensureBuilt(); err != nil {
		errJSON(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	m := h.svc.moverNow()
	if m == nil {
		errJSON(w, http.StatusServiceUnavailable, "单元搬运器没建起来（设置里要有 work_dir 和 sync_dir）")
		return
	}
	mctx, cancel := context.WithTimeout(ctx, projfilesMoveTimeout)
	defer cancel()
	// 拿住这个单元在调度器里的同一把锁：认不出编号就不加锁（此时 StartSync 自己也进不去），
	// 拿不到锁（ctx 取消）就走超时/取消。
	if code, ok := pfUnitCodeOf(op, arg); ok {
		unlock, ok2 := h.svc.unitLock(mctx, code)
		if !ok2 {
			errJSON(w, http.StatusServiceUnavailable, "拿单元锁失败或请求已取消")
			return
		}
		defer unlock()
	}
	var res projsync.PFUnitMoveResult
	var err error
	switch op {
	case "start":
		res, err = m.StartSync(mctx, arg)
	case "stop":
		res, err = m.StopSync(mctx, arg)
	case "create":
		res, err = m.CreateSynced(mctx, arg)
	}
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "单元操作失败: "+err.Error())
		return
	}
	writeJSON(w, res)
}

// dirSwitchWhy 返回「目录刚改过、正在切换」的一句话；不需要切换时空串。
// 同时看 svc 已标记的切换期，和磁盘上设置与 activeDirs 的差异（覆盖「改了设置但 tick 还没跑到」）。
func (h *projfilesAPI) dirSwitchWhy() string {
	if h.svc.switchPending() {
		return h.svc.switchReason()
	}
	st, _ := loadProjfilesSettings(h.svc.prep.settingsPath)
	if h.svc.needRebuild(st) {
		return "同步目录或工作目录刚改过，正在切换，稍后再试"
	}
	return ""
}

// pfUnitCodeOf 从 op 的参数里认出单元编号：unit/stop 的 arg 就是 code；unit/start / unit/create 的 arg 是 rel，
// 认 rel 的最后一段（跟 projsync.PFUnitMover.prepare 的编号来源一致）。认不出返回 ok=false。
func pfUnitCodeOf(op, arg string) (string, bool) {
	if op == "stop" {
		code := projsync.PFNormCode(arg)
		return code, code != ""
	}
	last := filepath.Base(filepath.ToSlash(arg))
	return projsync.PFCodeFromName(last)
}

// ── 服务的只读/释放入口（不动 projfiles.go，方法定义在这里）──

// isRunning 看当前有没有一轮（或单元操作）在进行。
func (s *projfilesSvc) isRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// release 放开轮次占位（与 acquire 成对；单元操作做完调）。
func (s *projfilesSvc) release() {
	s.mu.Lock()
	s.running = false
	s.mu.Unlock()
}

// moverNow 取当前的单元搬运器（没建成是 nil）。
func (s *projfilesSvc) moverNow() *projsync.PFUnitMover {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mover
}

// remoteNow 取当前的 Ubuntu 客户端（没建成是 nil）。
func (s *projfilesSvc) remoteNow() *projsync.PFRemoteClient {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.remote
}

// settingsNow 取当前生效的设置。
func (s *projfilesSvc) settingsNow() projfilesSettings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.settings
}

// unitLock 拿到当前调度器里那个单元的锁（跟 runOne / PFFresh.online 是同一把）。
// 调度器还没建起来、或 ctx 取消 → ok=false；调用方必须在 unlock 之后再返回。
func (s *projfilesSvc) unitLock(ctx context.Context, code string) (func(), bool) {
	sched := s.getSched()
	if sched == nil {
		return nil, false
	}
	return sched.LockUnit(ctx, code, 0)
}

// ── 启动信息文件 ────────────────────────────────────────

// projfilesAPIPath 是本机接口地址文件的默认路径 ~/.xxyi/projfiles-api.json。
func projfilesAPIPath() (string, error) {
	dir, err := xxyiDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, projfilesAPIFileName), nil
}

// writeProjfilesAPI 把本机接口地址与令牌原子写到 path（0600）：本机的界面/命令行从这里取令牌。
func writeProjfilesAPI(path, url, token string) error {
	b, err := json.Marshal(map[string]string{"url": url, "token": token})
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".projfiles-api-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(append(b, '\n'))
	cerr := f.Chmod(0o600)
	clerr := f.Close()
	if werr != nil || cerr != nil || clerr != nil {
		os.Remove(tmp)
		return werr
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
