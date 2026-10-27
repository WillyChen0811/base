package projsync

// 项目文件同步 T9b：全部项目的一轮（技术设计 §5.2 第 1–3 步、§5.3、§8.3、§12.2、§12.3 第 5 条）。
//
// 入口 PFSched.RunRound：
//
//	Poll（续租；被别人持有 → 本轮什么都不写，只报告）→ 写 Ubuntu 的 status/_mac.json → 轮前 CheckLinks →
//	读 Mac 的 units.json → PFClassifyUnits 分类 → 挑单元、排序 → 逐个（串行）加单元锁跑 PFRoundRunUnit →
//	取走已处理的 Ubuntu 请求 → 汇总成 PFSchedReport。
//
// 分类后的处理：
//   - syncing（Ubuntu 上找到）：参加一轮；Missing（登记 syncing、Ubuntu 没发现）也交给 PFRoundRunUnit（它按 G1/首次规则处理）。
//     Poll 发现不完整（Incomplete）时本文件不据「没发现」做任何推断，只是照常把单元交给 PFRoundRunUnit（它自己再核对）。
//   - Ubuntu 新项目：PFCheckWorkDirForUnit 看桌面——空位 → CreateSynced 在 Mac 建好，登记后同一轮就跑；
//     已有同编号真实目录 → 只报告「请先对 Mac 上这个项目点开始同步」；位置被占（或读不了、无法确认）→ 只报告。
//   - stopped：不处理，只在 Ubuntu 状态文件标「已停止同步」（每个进程只写一次）；starting/stopping：不处理；同编号重复（G4）：只报告。
//
// 单元顺序：有请求的（Ubuntu 请求、取最新后的 touched）> 指纹变了的（Ubuntu poll 的指纹或 Mac 本机轻量扫描的指纹）>
// 上一轮还有暂缓/待传/冲突/不明/出错的 > 其他（距上次全量核对超过间隔，或 Full 选项）。
// 指纹没变、上一轮干净、两边都没有不明条目、没到全量核对间隔的单元跳过本轮（T2 提醒：有不明条目时不能只靠指纹跳过）。
// 指纹只记在内存里：进程重启后第一轮所有单元都会跑。
//
// 并发：每个单元一把进程内的锁（取最新与定时轮次共用），同一单元不并发；一轮之内单元串行；同一时间只允许一轮 RunRound。
//
// Ubuntu 请求在这一轮处理完之后才用 take_requests 取走（先取走再处理，中途崩溃会丢请求；重复处理是幂等的）；
// 没处理成（通信失败、不确定、租约、过渡状态）的请求留着下一轮。
//
// 不管：定时循环、网络变化触发、HTTP（归 T11）；台账（T10）；RecoverMoves 只在服务启动时由 PFSchedStartup 调一次，不在每轮调。

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// PFSchedSettings 是调度的可调参数（以后由设置文件和 Swift 上报接入），零值取缺省。
type PFSchedSettings struct {
	// Net 是网络状态（PFRemoteNetGood / Expensive / Unknown / Offline）；NetAt 是上报时间，零值 = 不检查是否过期。
	// 超过 NetStaleAfter（缺省 3 分钟）没更新的按 Unknown（热点）对待（§5.3）。
	Net             string
	NetAt           time.Time
	NetStaleAfter   time.Duration
	Limits          PFRoundLimits
	Host            string        // 写进 status/_mac.json 的主机名
	LeaseSeconds    int           // 缺省 600
	QuietSeconds    int           // 缺省 10
	FullInterval    time.Duration // 定期全量核对间隔，缺省 6 小时
	MacHashBudget   time.Duration // 0 = 不限
	UbuntuBudget    time.Duration // Ubuntu 哈希预算，缺省 60 秒
	TouchedDebounce time.Duration // touched 去抖，缺省 10 秒
	StaleAfter      time.Duration // 取最新时 Ubuntu 不在线，上次对齐超过它就附注，缺省 1 小时
}

const (
	pfscDefaultNetStale = 3 * time.Minute
	pfscDefaultFull     = 6 * time.Hour
	pfscDefaultDebounce = 10 * time.Second
	pfscDefaultStale    = time.Hour
)

func (s PFSchedSettings) filled() PFSchedSettings {
	if s.NetStaleAfter <= 0 {
		s.NetStaleAfter = pfscDefaultNetStale
	}
	if s.FullInterval <= 0 {
		s.FullInterval = pfscDefaultFull
	}
	if s.TouchedDebounce <= 0 {
		s.TouchedDebounce = pfscDefaultDebounce
	}
	if s.StaleAfter <= 0 {
		s.StaleAfter = pfscDefaultStale
	}
	return s
}

// PFSchedConfig 是调度器的固定配置。
type PFSchedConfig struct {
	MetaDir  string // Mac 的 meta 目录（units.json、base、status 都在里面）
	WorkDir  string // local.work_dir（桌面 Projects），Ubuntu 新项目查位置用
	Remote   *PFRemoteClient
	Transfer *PFTransfer
	Mover    *PFUnitMover // 开始/取消同步、建新项目、CheckLinks、RecoverMoves；可以为 nil（则不建新项目、不检查链接）
	Settings PFSchedSettings

	// OnTouchedDue：touched 去抖到期时调用（在独立 goroutine 里），T11 在这里触发 RunRound。可为 nil，
	// 那就由 T11 自己看 PendingTouched。
	OnTouchedDue func(codes []string)

	// 以下只给测试注入。
	Now       func() time.Time
	Loc       *time.Location
	DirOps    PFDirOps
	Stat      PFStatFunc
	ApplyHook PFApplyHook
}

// PFSched 是调度器。创建后可并发使用。
type PFSched struct {
	cfg PFSchedConfig

	mu       sync.Mutex
	settings PFSchedSettings
	units    map[string]*pfscUnit
	locks    map[string]chan struct{}
	touched  map[string]time.Time // 编号 → 最近一次 touched 的时间
	timer    *time.Timer
	marked   map[string]bool // 已在 Ubuntu 标过「已停止同步」的编号
	leftover []PFRemoteLeftover

	roundMu sync.Mutex
	baseCtx context.Context
	cancel  context.CancelFunc
}

// pfscUnit 是一个单元在内存里的上一轮印象。
type pfscUnit struct {
	ubFP, macFP string
	lastFull    time.Time // 上次跑完且没有遗留事项的时间
	lastDone    time.Time // 上次一轮 Done（两边比较过）的时间
	ran         bool
	open        bool // 上一轮还有没做完的事
	lastAt      time.Time
}

// NewPFSched 创建调度器。
func NewPFSched(cfg PFSchedConfig) (*PFSched, error) {
	if cfg.MetaDir == "" || cfg.Remote == nil || cfg.Transfer == nil {
		return nil, errors.New("projsync: 调度器缺少 meta 目录、Ubuntu 客户端或传输器")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &PFSched{cfg: cfg, settings: cfg.Settings.filled(), units: map[string]*pfscUnit{}, locks: map[string]chan struct{}{},
		touched: map[string]time.Time{}, marked: map[string]bool{}, baseCtx: ctx, cancel: cancel}, nil
}

// Close 取消后台还在跑的取最新轮次、停掉去抖计时器。
func (s *PFSched) Close() {
	s.cancel()
	s.mu.Lock()
	if s.timer != nil {
		s.timer.Stop()
	}
	s.mu.Unlock()
}

// SetSettings 整体更新参数；SetNet 只更新网络状态。
func (s *PFSched) SetSettings(st PFSchedSettings) {
	s.mu.Lock()
	s.settings = st.filled()
	s.mu.Unlock()
}

func (s *PFSched) SetNet(net string, at time.Time) {
	s.mu.Lock()
	s.settings.Net, s.settings.NetAt = net, at
	s.mu.Unlock()
}

func (s *PFSched) getSettings() PFSchedSettings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.settings
}

// netNow 返回现在应使用的网络状态（空或过期按 Unknown）。
func (s *PFSched) netNow() string {
	st := s.getSettings()
	if st.Net == "" {
		return PFRemoteNetUnknown
	}
	if !st.NetAt.IsZero() && s.cfg.Now().Sub(st.NetAt) > st.NetStaleAfter {
		return PFRemoteNetUnknown
	}
	return st.Net
}

// PFSchedStartup 在服务启动时调一次：恢复没做完的移动（崩溃恢复，§12.3 第 5 条）。结果原样返回。
func (s *PFSched) PFSchedStartup(ctx context.Context) ([]PFUnitMoveResult, error) {
	if s.cfg.Mover == nil {
		return nil, nil
	}
	return s.cfg.Mover.RecoverMoves(ctx)
}

// ── 单元锁 ──────────────────────────────────────────────

func (s *PFSched) lockCh(code string) chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.locks[code]
	if c == nil {
		c = make(chan struct{}, 1)
		s.locks[code] = c
	}
	return c
}

// lockUnit 取单元锁；ctx 取消（或 wait>0 且超时）返回 false。
func (s *PFSched) lockUnit(ctx context.Context, code string, wait time.Duration) (unlock func(), ok bool) {
	c := s.lockCh(code)
	var tc <-chan time.Time
	if wait > 0 {
		t := time.NewTimer(wait)
		defer t.Stop()
		tc = t.C
	}
	select {
	case c <- struct{}{}:
		return func() { <-c }, true
	case <-ctx.Done():
		return nil, false
	case <-tc:
		return nil, false
	}
}

// ── touched ─────────────────────────────────────────────

// MarkTouched 记下这些单元刚被写过，去抖（缺省 10 秒）后让调度跑它们；每次调用重新计时。
func (s *PFSched) MarkTouched(codes ...string) {
	if len(codes) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.cfg.Now()
	for _, c := range codes {
		s.touched[c] = now
	}
	if s.cfg.OnTouchedDue == nil {
		return
	}
	if s.timer != nil {
		s.timer.Stop()
	}
	s.timer = time.AfterFunc(s.settings.TouchedDebounce, func() {
		if due := s.PendingTouched(); len(due) > 0 {
			s.cfg.OnTouchedDue(due)
		}
	})
}

// PendingTouched 返回去抖已到期、还没被一轮处理掉的 touched 单元（按编号排序）。
func (s *PFSched) PendingTouched() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dueTouchedLocked(s.cfg.Now())
}

func (s *PFSched) dueTouchedLocked(now time.Time) []string {
	var out []string
	for c, at := range s.touched {
		if now.Sub(at) >= s.settings.TouchedDebounce {
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out
}

func (s *PFSched) clearTouched(codes []string) {
	s.mu.Lock()
	for _, c := range codes {
		delete(s.touched, c)
	}
	s.mu.Unlock()
}

// ── 报告 ────────────────────────────────────────────────

// PFSchedOutcome 是一整轮的总体结果。
type PFSchedOutcome string

const (
	PFSchedDone        PFSchedOutcome = "done"        // 跑完了（各单元结果见 Units）
	PFSchedOffline     PFSchedOutcome = "offline"     // 断网，什么都没做
	PFSchedLeaseHeld   PFSchedOutcome = "lease_held"  // Ubuntu 租约在别的 Mac 手里，什么都没写
	PFSchedUnreachable PFSchedOutcome = "unreachable" // Poll 通信失败或被拒，什么都没做
	PFSchedBusy        PFSchedOutcome = "busy"        // 已有一轮在跑
	PFSchedFailed      PFSchedOutcome = "failed"      // 登记表读不了等，没有跑单元
	PFSchedCanceled    PFSchedOutcome = "canceled"
)

// 单元在本轮的处理方式（PFSchedUnitReport.Action）。
const (
	PFSchedActRan     = "ran"       // 跑了 PFRoundRunUnit，结果在 Result
	PFSchedActSkipped = "skipped"   // 指纹没变、上轮干净，本轮跳过
	PFSchedActCreated = "created"   // Ubuntu 新项目：在 Mac 建好并跑了一轮（Result 有）
	PFSchedActNotDone = "not_done"  // 没处理，原因在 Why（桌面已有同编号目录、位置被占、过渡状态、重复、已停止……）
	PFSchedActLocked  = "lock_wait" // 没拿到单元锁（本轮被取消）
)

// 单元被选中的理由（PFSchedUnitReport.Why 的取值之一，跑了才有）。
const (
	PFSchedWhyRequest     = "request"     // Ubuntu 请求
	PFSchedWhyTouched     = "touched"     // 取最新/入件之后的 touched
	PFSchedWhyFingerprint = "fingerprint" // 指纹变了（含进程刚启动）
	PFSchedWhyPending     = "pending"     // 上一轮还有没做完的
	PFSchedWhyFull        = "full"        // 定期全量核对或要求全量
)

// PFSchedUnitReport 是一个单元在这一轮的情况。
type PFSchedUnitReport struct {
	Code   string `json:"code"`
	Rel    string `json:"rel"`
	Action string `json:"action"`
	Why    string `json:"why,omitempty"`  // 选中的理由，或 not_done 时的原因
	Note   string `json:"note,omitempty"` // 给人看的一句话（如「请先对 Mac 上这个项目点开始同步」）
	// 新项目才有：桌面上的情况。
	Slot   PFWorkSlot        `json:"slot,omitempty"`
	Create *PFUnitMoveResult `json:"create,omitempty"`
	Result *PFRoundResult    `json:"result,omitempty"`
}

// PFSchedTotals 是各单元结果的总数（只数跑了的）。
type PFSchedTotals struct {
	Ran       int   `json:"ran"`
	Skipped   int   `json:"skipped"`
	NotDone   int   `json:"not_done"`
	Done      int   `json:"done"`      // 做成的动作总数
	Deferred  int   `json:"deferred"`  // 暂缓总数
	Conflicts int   `json:"conflicts"` // 没解决的冲突
	Errors    int   `json:"errors"`
	Uncertain int   `json:"uncertain"` // 结果不确定的单元数
	BytesUp   int64 `json:"bytes_up"`
	BytesDown int64 `json:"bytes_down"`
}

// PFSchedReport 是 RunRound 的返回值，永远非 nil。
type PFSchedReport struct {
	StartedAt time.Time      `json:"started_at"`
	EndedAt   time.Time      `json:"ended_at"`
	Net       string         `json:"net"`
	Outcome   PFSchedOutcome `json:"outcome"`
	Reason    string         `json:"reason,omitempty"`
	// LeaseHolder 在租约被占时给出现任持有者。
	LeaseHolder *PFRemoteLease `json:"lease_holder,omitempty"`

	Units  []PFSchedUnitReport `json:"units,omitempty"`
	Totals PFSchedTotals       `json:"totals"`
	// Links 是轮前链接检查的结果（只列有问题或重建了的；全部正常时为空）；LinksChecked 是检查了几个。
	Links        []PFUnitMoveLinkCheck `json:"links,omitempty"`
	LinksChecked int                   `json:"links_checked"`
	LinkError    string                `json:"link_error,omitempty"`
	// Ubuntu 残留（暂存区里崩溃留下的私有文件，只报告，绝不自动删）。
	Leftovers  []PFRemoteLeftover `json:"leftovers,omitempty"`
	Incomplete bool               `json:"incomplete,omitempty"` // Poll 发现不完整
	FreeBytes  int64              `json:"free_bytes"`
	// 请求：本轮从 poll 收到几个、取走几个、读不懂几个。
	RequestsSeen  int      `json:"requests_seen"`
	RequestsTaken int      `json:"requests_taken"`
	BadRequests   []string `json:"bad_requests,omitempty"`
	Notices       []string `json:"notices,omitempty"`
}

func (r *PFSchedReport) notice(f string, a ...any) {
	r.Notices = append(r.Notices, fmt.Sprintf(f, a...))
}

// ── 一整轮 ──────────────────────────────────────────────

// PFSchedRoundOptions 是一轮的选项。
type PFSchedRoundOptions struct {
	// Full：不看指纹，所有 syncing 单元都跑（界面「立即同步」、每 6 小时全量核对）。
	Full bool
	// Only 非空：只处理这些编号的单元（touched 触发时用）；Ubuntu 新项目的检查仍然照做。
	Only []string
}

// pfscCand 是一个要处理的 syncing 单元。
type pfscCand struct {
	rec    PFUnitRecord
	ubRel  string
	ubFP   string
	ubUnk  int
	macFP  string
	macUnk int
	why    string
	rank   int
	reqs   []PFRemoteRequest
}

// RunRound 跑全部项目的一轮。
func (s *PFSched) RunRound(ctx context.Context, opt PFSchedRoundOptions) *PFSchedReport {
	rep := &PFSchedReport{StartedAt: s.cfg.Now(), Net: s.netNow(), FreeBytes: -1}
	defer func() { rep.EndedAt = s.cfg.Now() }()
	if !s.roundMu.TryLock() {
		rep.Outcome, rep.Reason = PFSchedBusy, "上一轮还没跑完"
		return rep
	}
	defer s.roundMu.Unlock()
	st := s.getSettings()
	if rep.Net == PFRemoteNetOffline {
		rep.Outcome, rep.Reason = PFSchedOffline, "断网，本轮不同步"
		return rep
	}

	// 1. Poll：续租；被别人持有就什么都不写
	poll, err := s.cfg.Remote.Poll(ctx, PFRemotePollOptions{LeaseSeconds: st.LeaseSeconds, QuietSeconds: st.QuietSeconds})
	if err != nil {
		switch {
		case errors.Is(err, ErrPFRemoteLeaseHeld):
			rep.Outcome, rep.Reason = PFSchedLeaseHeld, "Ubuntu 的协调者租约在别的 Mac 手里，这台不动 Ubuntu，也不写任何东西"
			if poll.Lease.Holder.ID != "" {
				h := poll.Lease.Holder
				rep.LeaseHolder = &h
			}
			var re *PFRemoteError
			if errors.As(err, &re) && re.Holder != nil && rep.LeaseHolder == nil {
				h := *re.Holder
				rep.LeaseHolder = &h
			}
		case ctx.Err() != nil:
			rep.Outcome, rep.Reason = PFSchedCanceled, "已取消"
		default:
			rep.Outcome, rep.Reason = PFSchedUnreachable, fmt.Sprintf("连不上 Ubuntu 或它没有应答：%v", err)
		}
		return rep
	}
	rep.FreeBytes, rep.Incomplete = poll.FreeBytes, poll.Incomplete
	rep.Leftovers = poll.StagingLeftovers
	s.mu.Lock()
	s.leftover = poll.StagingLeftovers
	s.mu.Unlock()
	for _, b := range poll.BadRequests {
		rep.BadRequests = append(rep.BadRequests, b.Name+"："+b.Reason)
	}
	rep.RequestsSeen = len(poll.Requests)
	s.writeMacStatus(ctx, rep, st, rep.StartedAt)

	// 2. 轮前链接检查
	if s.cfg.Mover != nil {
		checks, err := s.cfg.Mover.CheckLinks(ctx)
		if err != nil {
			rep.LinkError = fmt.Sprintf("检查桌面链接失败：%v", err)
		}
		rep.LinksChecked = len(checks)
		for _, c := range checks {
			if c.Status != PFUnitMoveLinkOK {
				rep.Links = append(rep.Links, c)
			}
		}
	}

	// 3. 登记表与分类
	reg, err := PFUnitMoveLoadUnits(s.cfg.MetaDir)
	if err != nil {
		rep.Outcome, rep.Reason = PFSchedFailed, fmt.Sprintf("读登记表失败，本轮不跑任何单元：%v", err)
		return rep
	}
	cls, err := PFClassifyUnits(pfscDiscover(poll), reg)
	if err != nil {
		rep.Outcome, rep.Reason = PFSchedFailed, fmt.Sprintf("登记表不合格，本轮不跑任何单元：%v", err)
		return rep
	}

	// 4. 请求按编号归类
	reqsBy := map[string][]PFRemoteRequest{}
	reqNames := map[string][]string{}
	for _, rf := range poll.Requests {
		reqsBy[rf.Request.Unit] = append(reqsBy[rf.Request.Unit], rf.Request)
		reqNames[rf.Request.Unit] = append(reqNames[rf.Request.Unit], rf.Name)
	}
	now := s.cfg.Now()
	s.mu.Lock()
	touchedDue := map[string]bool{}
	for _, c := range s.dueTouchedLocked(now) {
		touchedDue[c] = true
	}
	s.mu.Unlock()
	only := map[string]bool{}
	for _, c := range opt.Only {
		only[c] = true
	}

	// 5. 非 syncing 的：只报告
	handled := map[string]bool{} // 请求可以取走的编号
	for _, m := range cls.Stopped {
		rep.Units = append(rep.Units, PFSchedUnitReport{Code: m.Reg.Code, Rel: m.Found.Rel, Action: PFSchedActNotDone, Why: "stopped", Note: PFNoteStopped})
		s.markStopped(ctx, rep, m)
		handled[m.Reg.Code] = true
	}
	for _, m := range cls.Transitional {
		rep.Units = append(rep.Units, PFSchedUnitReport{Code: m.Reg.Code, Rel: m.Found.Rel, Action: PFSchedActNotDone, Why: "transitional",
			Note: fmt.Sprintf("正在%s同步，本轮不处理", map[PFUnitState]string{PFUnitStarting: "开始", PFUnitStopping: "取消"}[m.Reg.State])})
	}
	for _, d := range cls.Duplicates {
		rep.Units = append(rep.Units, PFSchedUnitReport{Code: d.Code, Rel: d.Rel, Action: PFSchedActNotDone, Why: "duplicate",
			Note: PFNoteDuplicate + "：同编号还在 " + strings.Join(d.DupRels, "、")})
	}

	// 6. 挑 syncing 单元（含 Missing）
	var cands []pfscCand
	pollBy := map[string]PFRemotePollUnit{}
	for _, u := range poll.Units {
		pollBy[u.Code] = u
	}
	addCand := func(rec PFUnitRecord, ubRel string) {
		if len(only) > 0 && !only[rec.Code] {
			return
		}
		c := pfscCand{rec: rec, ubRel: ubRel, reqs: reqsBy[rec.Code]}
		if pu, ok := pollBy[rec.Code]; ok {
			c.ubFP, c.ubUnk = pu.Fingerprint, pu.Summary.Unknown
			if pu.Error != "" {
				c.ubUnk++ // 扫描失败：当作有不明，不跳过
			}
		}
		c.macFP, c.macUnk = s.macFingerprint(ctx, rec)
		c.why, c.rank = s.decideWhy(c, opt.Full, touchedDue[rec.Code], now, st)
		cands = append(cands, c)
	}
	for _, m := range cls.Syncing {
		addCand(m.Reg, m.Found.Rel)
	}
	for _, r := range cls.Missing {
		addCand(r, r.Rel)
	}

	// 7. Ubuntu 新项目
	newRecs := s.handleNew(ctx, rep, cls.New, only)
	for _, rec := range newRecs {
		c := pfscCand{rec: rec.rec, ubRel: rec.rec.Rel, reqs: reqsBy[rec.rec.Code], why: PFSchedWhyFingerprint, rank: 1}
		cands = append(cands, c)
		rep.Units = append(rep.Units, PFSchedUnitReport{Code: rec.rec.Code, Rel: rec.rec.Rel, Slot: PFSlotFree, Create: rec.create, Action: PFSchedActCreated, Why: PFSchedWhyFingerprint, Note: PFNoteNewOnUbuntu + "，已在 Mac 建好"})
	}
	newIdx := map[string]int{}
	for i := range rep.Units {
		if rep.Units[i].Action == PFSchedActCreated {
			newIdx[rep.Units[i].Code] = i
		}
	}

	// 8. 排序、逐个跑
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].rank != cands[j].rank {
			return cands[i].rank < cands[j].rank
		}
		return cands[i].rec.Code < cands[j].rec.Code
	})
	var ranTouched []string
	for _, c := range cands {
		if ctx.Err() != nil {
			rep.notice("本轮被取消，后面的单元没有处理")
			break
		}
		if c.why == "" { // 跳过
			rep.Units = append(rep.Units, PFSchedUnitReport{Code: c.rec.Code, Rel: c.rec.Rel, Action: PFSchedActSkipped, Note: "指纹没变，上一轮已对齐"})
			rep.Totals.Skipped++
			handled[c.rec.Code] = true
			continue
		}
		res, ok := s.runOne(ctx, c, st, poll.StagingLeftovers)
		ur := PFSchedUnitReport{Code: c.rec.Code, Rel: c.rec.Rel, Action: PFSchedActRan, Why: c.why, Result: res}
		if i, isNew := newIdx[c.rec.Code]; isNew {
			rep.Units[i].Result = res
			if !ok {
				rep.Units[i].Action, rep.Units[i].Note = PFSchedActLocked, "没拿到单元锁"
			}
		} else if !ok {
			ur.Action, ur.Note = PFSchedActLocked, "没拿到单元锁"
			rep.Units = append(rep.Units, ur)
		} else {
			rep.Units = append(rep.Units, ur)
		}
		if !ok {
			continue
		}
		s.account(rep, res)
		s.remember(c, res)
		if touchedDue[c.rec.Code] {
			ranTouched = append(ranTouched, c.rec.Code)
		}
		if pfscRequestsHandled(res) {
			handled[c.rec.Code] = true
		}
	}
	s.clearTouched(ranTouched)

	// 9. 取走已处理的请求；没有对应 syncing/stopped 单元的请求也取走（再留着没有意义），已报告
	if ctx.Err() == nil {
		for code := range reqsBy {
			inReg := false
			for _, r := range reg {
				if r.Code == code {
					inReg = true
				}
			}
			if !inReg && !pfscIsNew(cls, code) {
				handled[code] = true
				rep.notice("Ubuntu 请求的单元 %s 不在登记表里也不在 Ubuntu 上，已丢弃这些请求", code)
			}
		}
		var names []string
		for code := range handled {
			names = append(names, reqNames[code]...)
		}
		sort.Strings(names)
		s.takeRequests(ctx, rep, names)
	}

	s.writeMacStatus(ctx, rep, st, s.cfg.Now())
	rep.Outcome = PFSchedDone
	if ctx.Err() != nil {
		rep.Outcome, rep.Reason = PFSchedCanceled, "已取消"
	}
	return rep
}

func pfscIsNew(cls PFUnitClassification, code string) bool {
	for _, f := range cls.New {
		if f.Code == code {
			return true
		}
	}
	for _, f := range cls.Duplicates {
		if f.Code == code {
			return true
		}
	}
	return false
}

// pfscDiscover 把 poll 的单元清单还原成发现结果，交给 PFClassifyUnits。
func pfscDiscover(p PFRemotePollResponse) PFDiscoverResult {
	d := PFDiscoverResult{Incomplete: p.Incomplete, Excluded: p.DiscoverExcluded}
	for _, u := range p.Units {
		d.Units = append(d.Units, PFFoundUnit{Code: u.Code, Rel: u.Rel, Dup: u.Dup, DupRels: u.DupRels, Others: u.Others})
	}
	for _, k := range p.Skipped {
		d.Skipped = append(d.Skipped, PFDiscoverSkip{Rel: k.Rel, Reason: k.Reason, Unreadable: k.Unreadable})
	}
	sort.Slice(d.Units, func(i, j int) bool { return d.Units[i].Rel < d.Units[j].Rel })
	return d
}

// decideWhy 返回选中的理由和排序等级；理由为空 = 本轮跳过。
func (s *PFSched) decideWhy(c pfscCand, full, touched bool, now time.Time, st PFSchedSettings) (string, int) {
	s.mu.Lock()
	u := s.units[c.rec.Code]
	s.mu.Unlock()
	switch {
	case len(c.reqs) > 0:
		return PFSchedWhyRequest, 0
	case touched:
		return PFSchedWhyTouched, 0
	case u == nil || !u.ran || c.ubFP == "" || c.macFP == "" || c.ubFP != u.ubFP || c.macFP != u.macFP:
		return PFSchedWhyFingerprint, 1
	case u.open || c.ubUnk > 0 || c.macUnk > 0:
		return PFSchedWhyPending, 2
	case full || now.Sub(u.lastFull) >= st.FullInterval:
		return PFSchedWhyFull, 3
	}
	return "", 4
}

// macFingerprint 对 Mac 侧单元目录做一次不哈希的轻量扫描，返回指纹和不明条目数；扫不了返回空指纹（当作变了）。
func (s *PFSched) macFingerprint(ctx context.Context, rec PFUnitRecord) (string, int) {
	res, err := PFScan(ctx, rec.SyncPath, PFScanOptions{Stat: s.cfg.Stat, Filter: PFRemoteScanFilter(), Now: s.cfg.Now, QuietSeconds: s.getSettings().QuietSeconds})
	if err != nil || res == nil {
		return "", 1
	}
	return res.Fingerprint(), res.Summary().Unknown
}

// remember 记下这一轮的结果，下一轮用来决定要不要跳过。
func (s *PFSched) remember(c pfscCand, res *PFRoundResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.units[c.rec.Code]
	if u == nil {
		u = &pfscUnit{}
		s.units[c.rec.Code] = u
	}
	u.ran, u.ubFP, u.macFP, u.lastAt = true, c.ubFP, c.macFP, res.EndedAt
	u.open = pfscOpen(res)
	if res.Outcome == PFRoundDone {
		u.lastDone = res.EndedAt
	}
	if res.Outcome == PFRoundDone && !u.open {
		u.lastFull = res.EndedAt
	}
}

// pfscOpen：这一轮之后是否还有没做完的事（下一轮不能只靠指纹跳过）。
func pfscOpen(r *PFRoundResult) bool {
	return r.Outcome != PFRoundDone || r.Uncertain || len(r.Deferred) > 0 || len(r.Conflicts) > 0 || len(r.Errors) > 0 ||
		len(r.Done) > 0 || r.Deletes.Mac+r.Deletes.Ubuntu > 0 || r.hasOpenSkips() ||
		len(r.Status.PendingToMac)+len(r.Status.PendingToUbuntu) > 0
}

// pfscRequestsHandled：这一轮是否真的处理了请求（采集不到、不确定、租约、停下的都留给下一轮）。
func pfscRequestsHandled(r *PFRoundResult) bool {
	return r != nil && r.Outcome == PFRoundDone && !r.Uncertain
}

func (s *PFSched) account(rep *PFSchedReport, r *PFRoundResult) {
	t := &rep.Totals
	t.Ran++
	t.Done += len(r.Done)
	t.Deferred += len(r.Deferred)
	t.Conflicts += len(r.Conflicts)
	t.Errors += len(r.Errors)
	if r.Uncertain {
		t.Uncertain++
	}
	t.BytesUp += r.BytesUp
	t.BytesDown += r.BytesDown
}

// runOne 拿单元锁，跑一个单元的一轮。第二个返回值为假表示没拿到锁（被取消）。
func (s *PFSched) runOne(ctx context.Context, c pfscCand, st PFSchedSettings, leftovers []PFRemoteLeftover) (*PFRoundResult, bool) {
	unlock, ok := s.lockUnit(ctx, c.rec.Code, 0)
	if !ok {
		return nil, false
	}
	defer unlock()
	in := s.roundInput(c.rec, c.ubRel, st)
	in.UbuntuLeftovers = leftovers
	var fetch []string
	for _, r := range c.reqs {
		if r.Kind == PFRemoteReqTouched {
			continue
		}
		fetch = append(fetch, r.Paths...)
	}
	if len(fetch) > 0 {
		in.Priority, in.PriorityPaths = PFRoundPrioUbuntu, fetch
	}
	res, err := PFRoundRunUnit(ctx, in)
	if err != nil { // 输入不合格：什么都没做
		res = &PFRoundResult{Unit: c.rec.Code, Outcome: PFRoundFailed, Reason: err.Error(), StartedAt: s.cfg.Now(), EndedAt: s.cfg.Now()}
		res.Errors = append(res.Errors, PFRoundError{Code: "bad_input", Message: err.Error()})
	}
	return res, true
}

func (s *PFSched) roundInput(rec PFUnitRecord, ubRel string, st PFSchedSettings) PFRoundInput {
	return PFRoundInput{
		Unit: rec, UbuntuRel: ubRel, MetaDir: s.cfg.MetaDir, Remote: s.cfg.Remote, Transfer: s.cfg.Transfer,
		Net: s.netNow(), Limits: st.Limits, Priority: PFRoundPrioNormal,
		Now: s.cfg.Now, Loc: s.cfg.Loc, QuietSeconds: st.QuietSeconds,
		MacHashBudget: st.MacHashBudget, UbuntuHashBudget: st.UbuntuBudget,
		Stat: s.cfg.Stat, ApplyHook: s.cfg.ApplyHook, UbuntuLeftovers: nil,
	}
}

// ── 状态文件、请求 ──────────────────────────────────────

func (s *PFSched) writeMacStatus(ctx context.Context, rep *PFSchedReport, st PFSchedSettings, at time.Time) {
	ms := PFRemoteMacStatus{Version: PFRemoteStatusVersion, MacSeenAt: at.UTC().Format(time.RFC3339), Network: rep.Net,
		Host: st.Host, RoundAt: at.UTC().Format(time.RFC3339)}
	_, err := s.cfg.Remote.Apply(ctx, PFRemoteApplyOptions{Round: "sched"}, []PFRemoteStep{{Op: PFRemoteOpWriteStatus, MacStatus: &ms}})
	if err != nil {
		rep.notice("写 Ubuntu 的 status/_mac.json 失败：%v", err)
	}
}

func (s *PFSched) takeRequests(ctx context.Context, rep *PFSchedReport, names []string) {
	for len(names) > 0 {
		n := len(names)
		if n > 200 {
			n = 200
		}
		res, err := s.cfg.Remote.Apply(ctx, PFRemoteApplyOptions{Round: "sched"}, []PFRemoteStep{{Op: PFRemoteOpTakeRequests, Names: names[:n]}})
		if err != nil {
			rep.notice("取走 Ubuntu 请求失败（下一轮会重复处理，不会出错）：%v", err)
			return
		}
		if len(res.Results) == 1 && res.Results[0].Error == "" {
			rep.RequestsTaken += len(res.Results[0].Taken)
		} else {
			rep.notice("取走 Ubuntu 请求没有成功（下一轮会重复处理，不会出错）")
		}
		names = names[n:]
	}
}

// markStopped 在 Ubuntu 的状态文件里标「已停止同步」（每个进程每个单元写一次）。
func (s *PFSched) markStopped(ctx context.Context, rep *PFSchedReport, m PFUnitMatch) {
	s.mu.Lock()
	done := s.marked[m.Reg.Code]
	s.mu.Unlock()
	if done {
		return
	}
	stt := PFRemoteStatus{Version: PFRemoteStatusVersion, Unit: m.Reg.Code, UbuntuRel: m.Found.Rel, Stopped: true, Note: PFNoteStopped,
		RoundAt: s.cfg.Now().UTC().Format(time.RFC3339)}
	res, err := s.cfg.Remote.Apply(ctx, PFRemoteApplyOptions{Unit: PFRemoteUnit{Code: m.Reg.Code, Rel: m.Found.Rel}, Round: "sched"},
		[]PFRemoteStep{{Op: PFRemoteOpWriteStatus, Status: &stt}})
	if err != nil || len(res.Results) != 1 || res.Results[0].Error != "" {
		rep.notice("给已停止的单元 %s 写 Ubuntu 状态失败", m.Reg.Code)
		return
	}
	s.mu.Lock()
	s.marked[m.Reg.Code] = true
	s.mu.Unlock()
}

// ── Ubuntu 新项目 ───────────────────────────────────────

type pfscNew struct {
	rec    PFUnitRecord
	create *PFUnitMoveResult
}

// handleNew 处理 Ubuntu 新项目：空位 → 在 Mac 建好（返回新登记）；其余只报告。
func (s *PFSched) handleNew(ctx context.Context, rep *PFSchedReport, news []PFFoundUnit, only map[string]bool) []pfscNew {
	var out []pfscNew
	for _, f := range news {
		ur := PFSchedUnitReport{Code: f.Code, Rel: f.Rel, Action: PFSchedActNotDone, Why: "new_on_ubuntu"}
		if s.cfg.Mover == nil || s.cfg.WorkDir == "" {
			ur.Note = PFNoteNewOnUbuntu + "，没有配置自动建项目，只报告"
			rep.Units = append(rep.Units, ur)
			continue
		}
		fnd, err := PFCheckWorkDirForUnit(s.cfg.WorkDir, f, s.cfg.DirOps)
		if err != nil {
			ur.Note = fmt.Sprintf("%s：无法确认 Mac 工作目录里的情况，不自动建（%v）", PFNoteNewOnUbuntu, err)
			rep.Units = append(rep.Units, ur)
			continue
		}
		ur.Slot = fnd.Slot
		switch fnd.Slot {
		case PFSlotRealDir:
			ur.Note = fmt.Sprintf("%s：%s（Mac 上 %s 已有同编号的项目）", PFNoteNewOnUbuntu, PFNoteStartOnMacFirst, fnd.Rel)
			rep.Units = append(rep.Units, ur)
		case PFSlotOccupied:
			ur.Note = fmt.Sprintf("%s：%s（%s）", PFNoteNewOnUbuntu, PFNoteSlotOccupied, fnd.Rel)
			rep.Units = append(rep.Units, ur)
		default:
			cr, err := s.cfg.Mover.CreateSynced(ctx, f.Rel)
			if err != nil || cr.Status != PFUnitMoveDone {
				ur.Create = &cr
				ur.Note = fmt.Sprintf("%s：在 Mac 建项目没成功（%s %s）", PFNoteNewOnUbuntu, cr.Status, cr.Reason)
				if err != nil {
					ur.Note += "：" + err.Error()
				}
				rep.Units = append(rep.Units, ur)
				continue
			}
			us, err := PFUnitMoveLoadUnits(s.cfg.MetaDir)
			rec, ok := PFFindUnit(us, f.Code)
			if err != nil || !ok || rec.State != PFUnitSyncing {
				ur.Create = &cr
				ur.Note = PFNoteNewOnUbuntu + "：Mac 上建好了但登记表里没读到，本轮不跑，下一轮再来"
				rep.Units = append(rep.Units, ur)
				continue
			}
			if len(only) > 0 && !only[f.Code] {
				ur.Create, ur.Action, ur.Note = &cr, PFSchedActCreated, PFNoteNewOnUbuntu+"，已在 Mac 建好，这一轮没点到它，下一轮同步"
				rep.Units = append(rep.Units, ur)
				continue
			}
			out = append(out, pfscNew{rec: rec, create: &cr})
		}
	}
	return out
}
