package projsync

// 项目文件同步 T9a：单个项目单元的一轮同步编排（技术设计 §0、§2.3、§3.4 执行、§5.2 第 4–5 步、§5.4、§5.5、§5.6、§6.2、§12.0/12.1）。
//
// 入口 PFRoundRunUnit：把已经做好的各块串起来——
//
//	加载 base → Mac 扫描+哈希（哈希缓存读入、结束原子写回）→ Ubuntu facts → PFDecide 出计划 →
//	按计划顺序执行（Phase 0 建目录 → 1 文件 → 2 删空目录 → 3 台账）→ 每个动作成功就写 base →
//	结束：压实 base、写状态文件到 Ubuntu、返回结果。
//
// 文件分工：本文件放入口、输入输出结构、采集与判定、收尾；动作执行（Mac 本机原语、Ubuntu 批、传输、暂缓）在 pfround_exec.go；
// 删空目录前清系统垃圾文件（Ubuntu 侧是一个扩展步骤）在 pfround_junk.go；结果与状态文件格式在 pfstatus.go。
//
// 【Mac 侧单元根】= 单元登记里的 SyncPath（同步目录里的真实路径，不经桌面链接，§12.0/12.1）。
// 【Ubuntu 侧单元】= Remote.Root 下的 UbuntuRel（缺省等于登记里的 Rel）。
//
// 【停止规则】（硬标准：不写错一边、不覆盖未同步的改动、不丢数据）
//   - 某一步结果是「放弃且 Uncertain」、Ubuntu 通信不确定（PFRemoteIsUncertain）、传输层通信失败、配置错误、base 写入失败
//     → 立刻停止这个单元本轮的后续写入（没发出去的 Ubuntu 步骤不再发）；已做完的照样写进了 base；下一轮重新采集。
//   - 「放弃」（非不确定）、「推迟」、「超出范围」只跳过那一步，记进结果，其余照做。
//   - 判定出的单元级关卡（G1、G4）：整个单元不执行，不写 base，只写状态文件。
//
// 【暂缓】（§5.4）只针对 copy_new / copy_over / 冲突里要传字节的部分，见 pfRoundAdmit。删除、本机改名/复制、建目录不受影响。
// 【同一 sha256 的 part 安装串行】（Codex 评审 6-3）：一个 part 同一时刻只交给一个安装动作；同一内容要装第二处时，
// 先把挂着的 Ubuntu 批发出去（让前一处装完），再重新要一份 part（传输层会复用已有的，没有就重传），不并发消费。
//
// 【不做】台账（Phase 3）本任务只记成跳过「台账同步待做（T10）」；不跑调度（pfsched，T9b）、不管多单元并发和单元锁（T9b）；
// Ubuntu 侧 staging_leftovers 来自 poll，本函数只接收调用方传入的并原样报告；.unlink-* 等 moves/ 下的东西不碰。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// PFRoundPriority 是本轮的优先级（§5.5）。
type PFRoundPriority int

const (
	PFRoundPrioNormal PFRoundPriority = PFPrioNormal // 普通同步
	PFRoundPrioUbuntu PFRoundPriority = PFPrioUbuntu // 有 Ubuntu 请求
	PFRoundPrioUser   PFRoundPriority = PFPrioUser   // 用户指令
)

// 暂缓阈值的缺省值（设置项 projfiles.max_seconds_expensive / max_seconds_user，§10）。
const (
	PFRoundDefaultMaxExpensive = 20 * time.Second
	PFRoundDefaultMaxUser      = 60 * time.Second
)

// PFRoundLimits 是暂缓阈值（§5.4），由调用方从设置里读好传进来。
type PFRoundLimits struct {
	// MaxGood：网好时预计超过它才暂缓；0 = 网好全放行（缺省）。
	MaxGood time.Duration
	// MaxExpensive：热点或未知时，预计超过它就暂缓；<=0 取缺省 20 秒。
	MaxExpensive time.Duration
	// MaxUser：用户指令（或 Ubuntu 请求）要的文件，热点或未知时的上限；<=0 取缺省 60 秒。断网一律暂缓。
	MaxUser time.Duration
}

// PFRoundInput 是一个单元一轮的全部输入。
type PFRoundInput struct {
	// Unit 是单元登记记录：Code、Rel、SyncPath（Mac 侧单元根）必填，State 必须是 syncing。
	Unit PFUnitRecord
	// UbuntuRel 是单元在 Ubuntu 根下的相对路径；空 = 同 Unit.Rel。
	UbuntuRel string
	// MetaDir 是 Mac 的 meta 目录：base、hashcache、staging、回收、status 都在它下面。
	MetaDir string

	// Remote 是 Ubuntu 的客户端（Root、Meta 在里面）。
	Remote *PFRemoteClient
	// Transfer 是传输器：本机暂存区必须就是 Mac 的暂存区，远端暂存区必须就是 PFRemoteStagingDir(Remote.Meta)，
	// 上传方向要配好 VerifyUp（Remote.TargetVerify()）。
	Transfer *PFTransfer

	// Staging、RecycleDir 缺省 <MetaDir>/staging、<MetaDir>/回收；与 Mac 单元根必须同一个文件系统。
	Staging    string
	RecycleDir string

	// Net 是网络状态：PFRemoteNetGood / Expensive / Unknown / Offline。空串按 Unknown（热点）处理。
	Net    string
	Limits PFRoundLimits

	// Priority 是本轮优先级。PriorityPaths 非空时，只有这些路径（或这些目录之下）按 Priority 传、按 MaxUser 判暂缓，
	// 其余仍按普通；为空 = 整轮都按 Priority。
	Priority      PFRoundPriority
	PriorityPaths []string

	// Round 是本轮编号（写进回收记录）；空则用开始时间生成。
	Round string
	// Now 是时钟，缺省 time.Now。Loc 是冲突副本名里时间的时区，缺省 time.Local。
	Now func() time.Time
	Loc *time.Location

	// QuietSeconds 是「静止」秒数，缺省 10；两边扫描共用。
	QuietSeconds int
	// MacHashBudget：Mac 侧哈希时间预算，0 = 不限；UbuntuHashBudget 缺省 60 秒。
	MacHashBudget    time.Duration
	UbuntuHashBudget time.Duration

	// 以下只给测试注入。
	Stat      PFStatFunc
	Open      func(path string) (io.ReadCloser, error)
	ApplyHook PFApplyHook
	LockStale time.Duration

	// UbuntuLeftovers 是 poll 带回的 Ubuntu 暂存区残留（可选），原样并进结果。
	UbuntuLeftovers []PFRemoteLeftover
}

// PFRoundMacStagingDir / PFRoundMacRecycleDir 是 meta 下缺省的暂存区和同步回收区（传输器、调用方建配置时用）。
func PFRoundMacStagingDir(metaDir string) string { return filepath.Join(metaDir, "staging") }
func PFRoundMacRecycleDir(metaDir string) string { return filepath.Join(metaDir, PFRemoteRecycleName) }

func (in *PFRoundInput) fill() {
	if in.UbuntuRel == "" {
		in.UbuntuRel = in.Unit.Rel
	}
	if in.Staging == "" {
		in.Staging = PFRoundMacStagingDir(in.MetaDir)
	}
	if in.RecycleDir == "" {
		in.RecycleDir = PFRoundMacRecycleDir(in.MetaDir)
	}
	if in.Now == nil {
		in.Now = time.Now
	}
	if in.Loc == nil {
		in.Loc = time.Local
	}
	if in.QuietSeconds <= 0 {
		in.QuietSeconds = PFDefaultQuietSeconds
	}
	if in.UbuntuHashBudget <= 0 {
		in.UbuntuHashBudget = PFRemoteDefaultHashBudget
	}
	if in.Limits.MaxExpensive <= 0 {
		in.Limits.MaxExpensive = PFRoundDefaultMaxExpensive
	}
	if in.Limits.MaxUser <= 0 {
		in.Limits.MaxUser = PFRoundDefaultMaxUser
	}
	if in.Net == "" {
		in.Net = PFRemoteNetUnknown
	}
	if in.Round == "" {
		in.Round = "r" + in.Now().UTC().Format("20060102T150405Z")
	}
}

func (in *PFRoundInput) validate() error {
	bad := func(f string, a ...any) error {
		return fmt.Errorf("projsync: 一轮同步的输入不合格：%s", fmt.Sprintf(f, a...))
	}
	if err := PFValidateUnit(in.Unit); err != nil {
		return bad("单元登记不合格：%v", err)
	}
	if in.Unit.State != PFUnitSyncing {
		return bad("单元 %s 的状态是 %q，不是同步中", in.Unit.Code, in.Unit.State)
	}
	if !filepath.IsAbs(in.Unit.SyncPath) {
		return bad("单元 %s 没有 Mac 侧的同步路径", in.Unit.Code)
	}
	if err := validRel(in.UbuntuRel); err != nil {
		return bad("Ubuntu 侧相对路径不合法：%v", err)
	}
	if !filepath.IsAbs(in.MetaDir) {
		return bad("meta 目录 %q 不是绝对路径", in.MetaDir)
	}
	if in.Remote == nil || in.Transfer == nil {
		return bad("缺少 Ubuntu 客户端或传输器")
	}
	if !filepath.IsAbs(in.Staging) || !filepath.IsAbs(in.RecycleDir) {
		return bad("暂存区、同步回收区必须是绝对路径")
	}
	sha := strings.Repeat("0", 64)
	if got := filepath.Dir(in.Transfer.PartPath(PFDownload, sha)); filepath.Clean(got) != filepath.Clean(in.Staging) {
		return bad("传输器的本机暂存区是 %s，不是 Mac 暂存区 %s", got, in.Staging)
	}
	if got, want := path.Dir(in.Transfer.PartPath(PFUpload, sha)), PFRemoteStagingDir(in.Remote.Meta); path.Clean(got) != path.Clean(want) {
		return bad("传输器的远端暂存区是 %s，不是 Ubuntu 暂存区 %s", got, want)
	}
	return nil
}

// PFRoundRunUnit 跑一个单元的一轮同步。
//
// 返回的 error 只表示输入不合格（什么都没做）；其余一切——采集失败、关卡停下、中途不确定、出错——都在返回的结果里
// （结果永远非 nil，Outcome 说明总体情况）。调用方负责同一单元同一时间只跑一轮（单元锁归 T9b）。
func PFRoundRunUnit(ctx context.Context, in PFRoundInput) (*PFRoundResult, error) {
	in.fill()
	if err := in.validate(); err != nil {
		return nil, err
	}
	r := newPFRDRun(ctx, &in)
	r.run()
	return r.res, nil
}

// ── 一轮的现场 ──────────────────────────────────────────

type pfrdState int

const (
	pfrdNotRun   pfrdState = iota // 没执行（停止了，或前面的步骤挡住了）
	pfrdDone                      // 做成了
	pfrdDeferred                  // 暂缓
	pfrdSkipped                   // 跳过（前提不符、放弃、推迟、超出范围、出错）
)

type pfrdRun struct {
	ctx  context.Context
	in   *PFRoundInput
	res  *PFRoundResult
	plan *PFPlan

	macRoot string
	ubUnit  PFRemoteUnit
	ubPath  string // Ubuntu 上单元目录的绝对路径（下载的源路径用）
	statf   PFStatFunc

	macScan    *PFScanResult
	macNames   []PFRawName
	ubScan     *PFScanResult
	cache      *PFHashCache
	cachePath  string
	macMissing bool // 开始时 Mac 单元目录不存在
	ubMissing  bool // 开始时 Ubuntu 单元目录不存在

	base *PFBase
	app  *PFApplier
	rec  *PFRecycle

	executed  bool // 已经开始动 base（关卡停下、采集失败的轮次不碰 base，包括它的元数据）
	stopped   bool // 已停止后续写入
	hardStop  bool // 停止不是因为「不确定」而是配置错误、base 写失败、Ubuntu 整批拒绝等
	ubBroken  bool // Ubuntu 通信不确定（连状态文件也不再写）
	ubCreated bool // 本轮由我们建了 Ubuntu 的单元目录

	state []pfrdState
	adm   []pfrdAdm
	xfers map[string]*pfrdXfer
	batch []*pfrdUbItem
	// batchSHA：挂着的批里已有 install 步骤用到的上传 part（同一 sha256 的安装串行）。
	batchSHA map[string]bool
	// installedMac：本轮在 Mac 上装好、哈希已知的路径（并进哈希缓存，免得下一轮重算大文件）。
	installedMac map[string]bool
	// carried：上几轮留下的冲突；prevStatus：上一轮写下的状态（采集失败那类没有判定的轮次，pending 沿用它）。
	carried    []PFRemoteConflict
	prevStatus *PFRemoteStatus
}

func newPFRDRun(ctx context.Context, in *PFRoundInput) *pfrdRun {
	r := &pfrdRun{
		ctx: ctx, in: in, macRoot: filepath.Clean(in.Unit.SyncPath),
		ubUnit: PFRemoteUnit{Code: in.Unit.Code, Rel: in.UbuntuRel},
		statf:  in.Stat, xfers: map[string]*pfrdXfer{}, batchSHA: map[string]bool{}, installedMac: map[string]bool{},
		res: &PFRoundResult{Unit: in.Unit.Code, Round: in.Round},
	}
	if r.statf == nil {
		r.statf = PFLstat
	}
	return r
}

func (r *pfrdRun) now() time.Time { return r.in.Now() }

func (r *pfrdRun) notice(f string, a ...any) {
	r.res.Notices = append(r.res.Notices, fmt.Sprintf(f, a...))
}

func (r *pfrdRun) addError(path, code string, uncertain bool, f string, a ...any) {
	r.res.Errors = append(r.res.Errors, PFRoundError{Path: path, Code: code, Message: fmt.Sprintf(f, a...), Uncertain: uncertain})
}

// markUncertain：停止这个单元本轮的后续写入，记下原因（只记第一次）。ubuntu 为真表示是 Ubuntu 通信不确定。
func (r *pfrdRun) markUncertain(ubuntu bool, f string, a ...any) {
	msg := fmt.Sprintf(f, a...)
	if !r.res.Uncertain {
		r.res.Uncertain, r.res.UncertainReason = true, msg
	}
	r.stopped = true
	if ubuntu {
		r.ubBroken = true
	}
	r.addError("", "uncertain", true, "%s", msg)
}

// stopHard：非「不确定」的停止（配置错误、base 写失败、被取消）。
func (r *pfrdRun) stopHard(code, f string, a ...any) {
	r.stopped, r.hardStop = true, true
	r.addError("", code, false, f, a...)
	if r.res.Reason == "" {
		r.res.Reason = fmt.Sprintf(f, a...)
	}
}

// finishEarly 在没有任何写入的情况下结束（采集失败、关卡停下等），并按需写状态文件。
func (r *pfrdRun) finishEarly(out PFRoundOutcome, reason string, writeStatus bool) {
	r.res.Outcome, r.res.Reason = out, reason
	r.finalizeResult(writeStatus)
}

// ── 主流程 ──────────────────────────────────────────────

func (r *pfrdRun) run() {
	in := r.in
	r.res.StartedAt = r.now()
	r.loadCarried()
	if in.Net == PFRemoteNetOffline {
		r.finishEarly(PFRoundSkipped, "断网，本轮不同步", false)
		return
	}

	// 1. base
	lr, err := PFBaseLoad(in.MetaDir, in.Unit.Code)
	if err != nil {
		r.addError("", "base_load", false, "读取 base 失败：%v", err)
		r.finishEarly(PFRoundFailed, fmt.Sprintf("读取 base 失败：%v", err), false)
		return
	}
	for _, n := range lr.Notes {
		r.notice("base：%s", n)
	}
	baseProblem := ""
	var baseView PFBaseView // 注意：nil 接口，不能装一个 nil 的 *PFBase
	switch lr.Status {
	case PFBaseOK:
		r.base = lr.Base
		baseView = lr.Base
	case PFBaseCorrupt:
		baseProblem = lr.Reason
		r.notice("base 损坏（%s），原文件已移进 坏/，本轮按首次规则", lr.Reason)
	}

	// 2. Mac 扫描 + 哈希
	if !r.scanMac() {
		return
	}
	// 3. Ubuntu facts
	facts, ok := r.collectUbuntu()
	if !ok {
		return
	}

	// 4. 判定
	macDir, _ := r.statf(r.macRoot)
	skew := r.now().Sub(time.Unix(0, facts.NowNs))
	din := PFDecideInput{
		Unit: in.Unit.Code, Base: baseView, BaseProblem: baseProblem,
		Mac: r.macScan, Ubuntu: r.ubScan,
		MacDirID: PFDirID{Dev: macDir.Dev, Ino: macDir.Ino}, UbuntuDirID: facts.DirID,
		MacRel: in.Unit.Rel, UbuntuRel: in.UbuntuRel,
		ClockSkew: skew, ClockUnknown: facts.NowNs == 0,
		MacNames: r.macNames, UbuntuNames: facts.Names,
		Now: r.now(), Loc: in.Loc,
	}
	if facts.Dup {
		din.UbuntuDup = facts.DupRels
	} else if !facts.Exists && len(facts.Found) > 0 {
		din.UbuntuDup = facts.Found
	}
	r.plan = PFDecide(din)
	pl := r.plan
	r.res.Gates = pl.Verdict.Gates
	r.res.FirstTime, r.res.ResetBase = pl.Verdict.FirstTime, pl.Verdict.ResetBase
	r.res.PlannedActions = len(pl.Actions)
	for _, n := range pl.Notes {
		r.notice("%s", n.Text)
	}
	r.recordDecideSkips()

	// 5. 关卡停下：不执行，不动 base
	if pl.Verdict.Stopped {
		var why []string
		for _, g := range pl.Verdict.Gates {
			if g.Effect == PFGateStop {
				why = append(why, g.Code+"："+g.Reason)
			}
		}
		r.finishEarly(PFRoundStopped, strings.Join(why, "；"), true)
		return
	}

	// 6. 准备 base（首次规则：先 Reset 或新建）
	if !r.prepareBase() {
		return
	}

	r.executed = true

	// 7. 暂存区残留（只报告）
	r.scanLeftovers()

	// 8. 执行
	r.execute()

	// 9. 收尾
	switch {
	case r.res.Uncertain:
		r.res.Outcome = PFRoundUncertain
		r.res.Reason = r.res.UncertainReason
	case r.hardStop:
		r.res.Outcome = PFRoundFailed
	case r.res.Outcome == "":
		r.res.Outcome = PFRoundDone
	}
	r.finalizeResult(true)
}

// scanMac 扫描并哈希 Mac 侧单元目录；返回 false 表示本轮到此为止（已收尾）。
func (r *pfrdRun) scanMac() bool {
	in := r.in
	st, err := r.statf(r.macRoot)
	switch {
	case err == nil && st.Kind == PFKindDir:
	case err == nil:
		r.addError("", "mac_root", false, "Mac 侧单元目录 %s 不是真实目录（是%s）", r.macRoot, pfKindName(st.Kind))
		r.finishEarly(PFRoundFailed, "Mac 侧单元目录不是真实目录", false)
		return false
	case errors.Is(err, fs.ErrNotExist):
		r.macMissing = true
	default:
		r.addError("", "mac_root", false, "读取 Mac 侧单元目录 %s 失败：%v", r.macRoot, err)
		r.finishEarly(PFRoundFailed, fmt.Sprintf("读取 Mac 侧单元目录失败：%v", err), false)
		return false
	}

	// 哈希缓存：读不了或损坏就重算，不影响同步
	r.cachePath = filepath.Join(in.MetaDir, PFRemoteCacheName, in.Unit.Code+".json")
	r.cache = NewPFHashCache()
	if b, err := os.ReadFile(r.cachePath); err == nil {
		if c, perr := PFParseHashCache(b); perr == nil {
			r.cache = c
		} else {
			r.notice("Mac 侧哈希缓存损坏（%v），已重算", perr)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		r.notice("Mac 侧哈希缓存读不了（%v），已重算", err)
	}
	if r.macMissing {
		return true
	}
	opt := PFScanOptions{
		Stat: in.Stat, Filter: PFRemoteScanFilter(), Now: in.Now, QuietSeconds: in.QuietSeconds, Open: in.Open,
		HashBudget: in.MacHashBudget,
	}
	opt.OnExcluded = func(rel string, isDir bool) {
		n := PFRawName{Path: rel, IsDir: isDir}
		if !isDir {
			if s, e := r.statf(filepath.Join(r.macRoot, filepath.FromSlash(rel))); e == nil {
				n.MtimeNs = s.MtimeNs
			}
		}
		r.macNames = append(r.macNames, n)
	}
	res, err := PFScanAndHash(r.ctx, r.macRoot, r.cache, opt)
	if res == nil {
		r.addError("", "mac_scan", false, "扫描 Mac 侧单元目录失败：%v", err)
		r.finishEarly(PFRoundFailed, fmt.Sprintf("扫描 Mac 侧单元目录失败：%v", err), false)
		return false
	}
	if err != nil { // 只会是被取消
		r.finishEarly(PFRoundSkipped, "已取消", false)
		return false
	}
	sort.Slice(r.macNames, func(i, j int) bool { return r.macNames[i].Path < r.macNames[j].Path })
	r.macScan = res
	return true
}

// collectUbuntu 取 Ubuntu 侧事实；返回 false 表示本轮到此为止（已收尾）。
func (r *pfrdRun) collectUbuntu() (*PFRemoteFacts, bool) {
	in := r.in
	facts, err := in.Remote.Facts(r.ctx, r.ubUnit, PFRemoteFactsOptions{QuietSeconds: in.QuietSeconds, HashBudget: in.UbuntuHashBudget})
	if err != nil {
		switch {
		case errors.Is(err, ErrPFRemoteLeaseHeld):
			r.addError("", "lease_held", false, "%v", err)
			r.finishEarly(PFRoundLeaseHeld, "Ubuntu 的协调者租约在别的 Mac 手里，这台不动 Ubuntu", false)
		case PFRemoteIsUncertain(err):
			r.addError("", "ubuntu_comm", true, "采集 Ubuntu 事实时通信失败：%v", err)
			r.res.Uncertain, r.res.UncertainReason = true, fmt.Sprintf("采集 Ubuntu 事实时通信失败：%v", err)
			r.finishEarly(PFRoundSkipped, "Ubuntu 通信失败，本轮没有采集到事实（没有写过任何东西），下一轮重试", false)
		case errors.Is(err, ErrPFRemoteUnstable):
			r.addError("", "ubuntu_unstable", false, "%v", err)
			r.finishEarly(PFRoundSkipped, "Ubuntu 上这个单元采集时在变化，本轮跳过，下一轮再采", false)
		default:
			r.addError("", "ubuntu_refused", false, "%v", err)
			r.finishEarly(PFRoundFailed, fmt.Sprintf("Ubuntu 没有给出事实：%v", err), false)
		}
		return nil, false
	}
	r.res.Notices = append(r.res.Notices, pfrdPrefixed("Ubuntu：", facts.Notes)...)
	if !facts.Exists && facts.Incomplete {
		r.addError("", "ubuntu_incomplete", false, "Ubuntu 上没找到这个单元，但单元发现不完整，「没有」不可信")
		r.finishEarly(PFRoundSkipped, "Ubuntu 上没找到这个单元，但发现不完整（有读不了的目录），不能当成「不存在」，本轮跳过", false)
		return nil, false
	}
	if facts.TooLarge {
		r.addError("", "too_large", false, "Ubuntu 上单元有 %d 个条目，超过上限", facts.TotalItems)
		r.finishEarly(PFRoundSkipped, fmt.Sprintf("Ubuntu 上这个单元有 %d 个条目，超过 %d，整个单元超出范围", facts.TotalItems, PFRemoteDefaultMaxItems), false)
		return nil, false
	}
	if facts.Exists {
		r.ubScan = facts.Scan
		r.ubPath = facts.UnitPath
	} else {
		r.ubMissing = true
	}
	return facts, true
}

func pfrdPrefixed(prefix string, ss []string) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		out = append(out, prefix+s)
	}
	return out
}

// recordDecideSkips 把判定的跳过清单并进结果；原因代码尽量用扫描条目的具体代码（如 not_quiet）。
func (r *pfrdRun) recordDecideSkips() {
	for _, s := range r.plan.Skips {
		code := s.Code
		if code == PFSkipUnknown {
			for _, scan := range []*PFScanResult{r.macScan, r.ubScan} {
				if scan == nil {
					continue
				}
				if it, ok := scan.Find(s.Path); ok && it.Code != "" {
					code = it.Code
					break
				}
			}
		}
		r.res.Skipped = append(r.res.Skipped, PFRoundSkip{Path: s.Path, Side: s.Side, Code: code, Reason: s.Reason, Source: PFRoundSrcDecide})
	}
}

// loadCarried 读上几轮留下的冲突（Mac 本机状态文件）；读不懂就当没有。
func (r *pfrdRun) loadCarried() {
	s, err := PFRoundLoadState(r.in.MetaDir, r.in.Unit.Code)
	if err != nil {
		r.notice("Mac 本机状态文件读不了（%v），上几轮的冲突记录没带过来", err)
		return
	}
	if s != nil {
		r.carried = s.Status.Conflicts
		st := s.Status
		r.prevStatus = &st
	}
}

// prepareBase：首次规则下清空旧 base（G2）或新建（没有/损坏），其余沿用已加载的。
func (r *pfrdRun) prepareBase() bool {
	meta := r.plan.Meta
	var err error
	switch {
	case r.base == nil:
		r.base, err = PFBaseCreate(r.in.MetaDir, r.in.Unit.Code, meta)
	case r.plan.Verdict.ResetBase:
		err = r.base.Reset(meta)
	}
	if err != nil {
		r.base = nil
		r.addError("", "base_prepare", false, "准备 base 失败：%v", err)
		r.finishEarly(PFRoundFailed, fmt.Sprintf("准备 base 失败：%v", err), true)
		return false
	}
	return true
}

// scanLeftovers 扫 Mac 暂存区里崩溃留下的私有文件，只报告（swap 类可能是用户内容，绝不删）。
func (r *pfrdRun) scanLeftovers() {
	lo, err := PFStagingLeftovers(r.in.Staging)
	if err != nil {
		r.notice("读 Mac 暂存区残留失败：%v", err)
	}
	for _, l := range lo {
		r.res.Leftovers = append(r.res.Leftovers, PFRemoteLeftover{Path: l.Path, Kind: l.Kind, Size: l.Size, MtimeNs: l.MtimeNs})
	}
	r.res.Leftovers = append(r.res.Leftovers, r.in.UbuntuLeftovers...)
	for _, l := range r.res.Leftovers {
		if l.Kind == "swap" {
			r.notice("暂存区里有覆盖时留下的交换文件 %s（%d 字节），可能是你的文件内容，没有动它，请人工看一眼", l.Path, l.Size)
		}
	}
}

// ── 收尾 ────────────────────────────────────────────────

// finalizeResult：压实 base、写哈希缓存、拼并写状态文件、记耗时。所有退出路径都经过这里。
func (r *pfrdRun) finalizeResult(writeStatus bool) {
	res := r.res
	// base：写最终的单元级元数据再压实
	if r.executed && r.base != nil && r.base.Err() == nil {
		meta := r.plan.Meta
		if st, err := r.statf(r.macRoot); err == nil && st.Kind == PFKindDir {
			meta.MacDirID = PFDirID{Dev: st.Dev, Ino: st.Ino}
		}
		if r.ubCreated && !r.ubBroken {
			if id, ok := r.ubuntuDirID(); ok {
				meta.UbuntuDirID = id
			}
		}
		if err := r.base.SetMeta(meta); err == nil {
			if err := r.base.Compact(); err != nil {
				r.addError("", "base_compact", false, "压实 base 失败：%v（已写进日志的条目不会丢）", err)
			}
		}
	}
	r.saveCache()
	r.collectConflicts()

	// 状态
	toUb, toMac := r.pendingLists()
	res.EndedAt = r.now()
	res.Duration = res.EndedAt.Sub(res.StartedAt)
	if res.Outcome == "" {
		res.Outcome = PFRoundDone
	}
	res.Status = pfRoundBuildStatus(res, r.in.UbuntuRel, res.EndedAt, toUb, toMac)
	if writeStatus && !r.ubBroken {
		r.writeUbuntuStatus()
	}
	if err := pfRoundSaveState(r.in.MetaDir, res); err != nil {
		r.notice("写 Mac 本机状态文件失败：%v", err)
	}
	res.EndedAt = r.now()
	res.Duration = res.EndedAt.Sub(res.StartedAt)
}

// ubuntuDirID：本轮新建了 Ubuntu 单元目录，取它的 dev+ino 记进 base（否则下一轮 G2 会把 base 当成不可信）。
func (r *pfrdRun) ubuntuDirID() (PFDirID, bool) {
	f, err := r.in.Remote.Facts(r.ctx, r.ubUnit, PFRemoteFactsOptions{QuietSeconds: r.in.QuietSeconds, MaxItems: 1})
	if err != nil || f == nil || !f.Exists {
		if err != nil && PFRemoteIsUncertain(err) {
			r.ubBroken = true
		}
		return PFDirID{}, false
	}
	return f.DirID, true
}

func (r *pfrdRun) writeUbuntuStatus() {
	st := r.res.Status
	steps := []PFRemoteStep{{Op: PFRemoteOpWriteStatus, Status: &st}}
	resp, err := r.in.Remote.Apply(r.ctx, PFRemoteApplyOptions{Unit: r.ubUnit, Round: r.in.Round}, steps)
	switch {
	case err != nil:
		r.addError("", "status_write", PFRemoteIsUncertain(err), "写 Ubuntu 状态文件失败：%v", err)
	case len(resp.Results) != 1 || resp.Results[0].Error != "":
		msg := "没有结果"
		if len(resp.Results) == 1 {
			msg = resp.Results[0].Error
		}
		r.addError("", "status_write", false, "写 Ubuntu 状态文件失败：%s", msg)
	default:
		r.res.StatusWritten = true
	}
}

func (r *pfrdRun) saveCache() {
	if r.cache == nil {
		return
	}
	// 清掉确认已不在的文件的缓存；不明、占位、没看的子树下的一律保留
	if r.macScan != nil {
		r.cache.Prune(func(p string) bool {
			if r.installedMac[p] {
				return true
			}
			if _, ok := r.macScan.Find(p); ok {
				return true
			}
			_, skipped := r.macScan.SubtreeSkipped(p)
			if skipped {
				return true
			}
			// 扫描之后本轮才出现在 Mac 上的（改名、冲突改名）按 installedMac 保留；其余已不在
			return false
		})
	}
	b, err := r.cache.Marshal()
	if err == nil {
		err = writeFileAtomic(r.cachePath, b, 0o600)
	}
	if err != nil {
		r.notice("写 Mac 侧哈希缓存失败：%v（下一轮会重算）", err)
	}
}

// collectConflicts：上几轮留下的冲突，只要原名和冲突副本在 base 里都还在，就算没解决，带下去。
func (r *pfrdRun) collectConflicts() {
	seen := map[string]bool{}
	for _, c := range r.res.Conflicts {
		seen[c.Path+"\x00"+c.ConflictPath] = true
	}
	if r.base == nil || r.base.Err() != nil {
		// 没有 base 可核对：照搬上几轮的，宁可多报
		for _, c := range r.carried {
			if k := c.Path + "\x00" + c.ConflictPath; !seen[k] {
				r.res.Conflicts = append(r.res.Conflicts, c)
				seen[k] = true
			}
		}
		return
	}
	for _, c := range r.carried {
		k := c.Path + "\x00" + c.ConflictPath
		if seen[k] {
			continue
		}
		_, a := r.base.Get(c.Path)
		_, b := r.base.Get(c.ConflictPath)
		if a && b {
			r.res.Conflicts = append(r.res.Conflicts, c)
			seen[k] = true
		}
	}
	sort.SliceStable(r.res.Conflicts, func(i, j int) bool { return r.res.Conflicts[i].Path < r.res.Conflicts[j].Path })
}

// pendingLists 把没做完的拷贝（暂缓的、跳过的、没执行的）拼成状态文件里的 pending。
func (r *pfrdRun) pendingLists() (toUbuntu, toMac []PFRemotePending) {
	if r.plan == nil {
		if r.prevStatus != nil { // 这一轮没判定：待传清单沿用上一轮的
			return r.prevStatus.PendingToUbuntu, r.prevStatus.PendingToMac
		}
		return nil, nil
	}
	for i, a := range r.plan.Actions {
		if a.Kind != PFActCopyNew && a.Kind != PFActCopyOver && a.Kind != PFActConflict {
			continue
		}
		if i < len(r.state) && r.state[i] == pfrdDone {
			continue
		}
		reason := PFRemoteReasonQueued
		if i < len(r.state) && r.state[i] == pfrdDeferred {
			reason = PFRemoteReasonDeferred
		}
		if a.Kind == PFActConflict {
			reason = PFRemoteReasonConflict
		}
		p := PFRemotePending{Path: a.Path, Size: a.SrcExpect.Size, Sha: a.SrcExpect.SHA256, Reason: reason}
		if a.Src == PFSideMac {
			p.MacMtimeNs = a.SrcExpect.MtimeNs
			if a.DstExpect != nil && !a.DstExpect.Absent {
				sz := a.DstExpect.Size
				p.UbSize, p.UbMtimeNs = &sz, a.DstExpect.MtimeNs
			}
			toUbuntu = append(toUbuntu, p)
		} else {
			p.UbMtimeNs = a.SrcExpect.MtimeNs
			toMac = append(toMac, p)
		}
	}
	return toUbuntu, toMac
}
