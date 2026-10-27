package projsync

// 本文件管项目文件同步的传输队列和调度：提交任务、优先级与抢占、并发上限、限速、预计时间。
// 怎么搬一个文件（暂存区 part、续传、核验）在 pftransfer_copy.go，文件系统抽象在 pftransfer_fs.go，
// 暂存区文件格式与清理在 pftransfer_stage.go，测速在 pftransfer_speed.go。
//
// 两个方向：
//   上传 PFUpload：本机源文件 →（SFTP 写）→ 远端暂存区 <RemoteStaging>/<sha256>.part
//   下载 PFDownload：远端源文件 →（SFTP 按偏移读）→ 本机暂存区 <LocalStaging>/<sha256>.part
// 输出是「暂存区里核验通过的 part 路径」（PFTransferResult.PartPath，是目标机上的路径）；
// 提交到正位（原子交换）不在这里。
//
// 调度规矩（技术设计 §5.5）：
//   - 优先级：用户指令 100 > Ubuntu 请求 80 > 普通 0；同优先级按预计时间从小到大，再按路径。
//   - 同一方向上，只要有更高优先级的任务在排队或在传，低优先级的就不启动；
//     已在传的每传完一块检查一次，有更高优先级的就让出（part 保留，之后从断点续传）。
//     方向之间互不影响（两个方向各有各的带宽和限速）。
//   - 并发：小文件（< SmallBytes，缺省 1MB）最多 MaxSmall（缺省 8）个同时传（两个方向合计）；
//     大文件每个方向同时只传 MaxLargePerDir（缺省 1）个。
//   - 同一份内容（同方向同 sha256）同一时刻只有一个任务在传，后到的排队，等前面的传完直接复用。
// 通信错误不盲目重试：任务以 *PFTransferError 返回，由上层决定（技术设计 §5.6）。

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
)

// 优先级（与 pybase 约定对齐，数字越大越先）。
const (
	PFPrioUser   = 100 // 用户指令
	PFPrioUbuntu = 80  // Ubuntu 请求
	PFPrioNormal = 0   // 普通同步
)

const (
	PFTransferDefaultBlock    = 8 << 20 // 块大小：续传、核对、抢占的单位
	pfTrDefaultIOChunk        = 1 << 20 // 一次读写的大小，也是限速的基本粒度
	pfTrDefaultSmallBytes     = 1 << 20
	pfTrDefaultMaxSmall       = 8
	pfTrDefaultMaxLargePerDir = 1
	pfTrDefaultMinSampleBytes = 1 << 20
	pfTrLimiterBurst          = 64 << 10 // 限速令牌桶容量；一次最多等这么多
)

// PFTransferConfig 是传输层的配置。
type PFTransferConfig struct {
	Local  PFTransferFS // 本机文件系统；nil 用 PFLocalFS
	Remote PFTransferFS // 远端文件系统（生产用 PFSFTP(client)）；运行中可用 SetRemote 换

	LocalStaging  string // 本机暂存区目录（下载方向的 part 放这里）
	RemoteStaging string // 远端暂存区目录（上传方向的 part 放这里）

	// 目标端核验。VerifyUp 上传方向必填（目标是 Ubuntu，由调用方注入，约定见 PFTargetVerify）；
	// VerifyDown 下载方向目标是本机，nil 用 PFHashLocalFile。
	VerifyUp   PFTargetVerify
	VerifyDown PFTargetVerify

	BlockSize      int64 // 块大小，缺省 8MB（测试可调小）
	IOChunk        int   // 一次读写大小，缺省 1MB
	SmallBytes     int64 // 小于它算小文件，缺省 1MB
	MaxSmall       int   // 小文件并发上限，缺省 8
	MaxLargePerDir int   // 每个方向大文件并发上限，缺省 1
	MinSampleBytes int64 // 块不小于它才算测速样本，缺省 1MB

	DefaultBps float64 // 没有测量值时的缺省速度（字节/秒），缺省 5MB/s；网络状态变化用 ResetSpeed 换

	// Progress 每确认一块后同步调用一次（在传输的 goroutine 里，不要长时间阻塞）。
	Progress func(PFTransferProgress)
}

// PFTransferJob 是一个传输任务。
type PFTransferJob struct {
	Dir      PFDirection
	Src      string // 上传：本机源文件完整路径；下载：远端源文件完整路径
	Size     int64  // 期望大小
	SHA256   string // 期望的 sha256（小写十六进制），也是暂存区里的文件名
	Priority int    // PFPrioUser / PFPrioUbuntu / PFPrioNormal
}

func (j PFTransferJob) key() string { return fmt.Sprintf("%d:%s", j.Dir, j.SHA256) }

// PFTransferProgress 是每确认一块后给 Progress 的信息。
type PFTransferProgress struct {
	Job     PFTransferJob
	Attempt int   // 第几轮搬运（核验不符重来会加一）
	Block   int   // 刚确认的块序号（从 0 数）
	Done    int64 // part 里已确认的字节数
	Total   int64
}

// PFTransferResult 是任务的结果；出错时 PartPath 为空，其余字段仍记录已发生的事。
type PFTransferResult struct {
	Job         PFTransferJob
	PartPath    string // 暂存区里核验通过的 part 路径（目标机上的路径）
	Reused      bool   // 暂存区里已有核验过的同内容 part，没传
	SentBytes   int64  // 本任务实际搬过去的字节数（含重传、含核验不符重来的）
	ResumedFrom int64  // 第一次开工时从哪个偏移续传（0 = 从头；复用时等于 Size）
	Attempts    int    // 搬运轮数；核验不符重来会大于 1
	Yields      int    // 被更高优先级任务抢占、让出的次数
	Duration    time.Duration
}

// PFErrKind 是传输错误的类别。
type PFErrKind int

const (
	PFErrComm     PFErrKind = iota + 1 // 远端通信或读写失败：结果不确定，part 保留，由上层决定怎么办
	PFErrLocal                         // 本机文件读写失败（磁盘满、权限等）
	PFErrSource                        // 源文件不存在或不是文件
	PFErrVerify                        // 连续两次整体核验不符
	PFErrCanceled                      // 被取消
	PFErrConfig                        // 任务或配置不合法
)

// PFTransferError 是传输层返回的错误，Msg 是中文原因。
type PFTransferError struct {
	Kind PFErrKind
	Dir  PFDirection
	Path string
	Msg  string
	Err  error
}

func (e *PFTransferError) Error() string {
	s := fmt.Sprintf("%s %s：%s", e.Dir, e.Path, e.Msg)
	if e.Err != nil {
		s += "：" + e.Err.Error()
	}
	return s
}

func (e *PFTransferError) Unwrap() error { return e.Err }

// Uncertain 为 true 表示通信类错误：不知道对面发生了什么，调用方应停止这个单元后续写入、下一轮重新采集。
func (e *PFTransferError) Uncertain() bool { return e.Kind == PFErrComm }

// PFTransferStats 是队列快照，给状态显示和测试用。
type PFTransferStats struct {
	Pending      int
	Running      int
	RunningSmall int
	RunningLarge [2]int // 按方向
}

// PFTransfer 是传输队列：提交任务、调度、限速、测速。并发安全。
type PFTransfer struct {
	cfg     PFTransferConfig
	speed   *PFSpeed
	limiter [2]*rate.Limiter
	limit   [2]atomic.Int64 // 每个方向的限速（字节/秒），0 = 不限

	mu      sync.Mutex
	remote  PFTransferFS
	closed  bool
	seq     uint64
	pending []*pfTrJob
	running map[*pfTrJob]struct{}
	small   int    // 正在传的小文件数
	large   [2]int // 正在传的大文件数（按方向）
	wg      sync.WaitGroup
}

// NewPFTransfer 建传输队列。
func NewPFTransfer(cfg PFTransferConfig) (*PFTransfer, error) {
	if cfg.Local == nil {
		cfg.Local = PFLocalFS{}
	}
	if cfg.BlockSize <= 0 {
		cfg.BlockSize = PFTransferDefaultBlock
	}
	if cfg.IOChunk <= 0 {
		cfg.IOChunk = pfTrDefaultIOChunk
	}
	if cfg.SmallBytes <= 0 {
		cfg.SmallBytes = pfTrDefaultSmallBytes
	}
	if cfg.MaxSmall <= 0 {
		cfg.MaxSmall = pfTrDefaultMaxSmall
	}
	if cfg.MaxLargePerDir <= 0 {
		cfg.MaxLargePerDir = pfTrDefaultMaxLargePerDir
	}
	if cfg.MinSampleBytes <= 0 {
		cfg.MinSampleBytes = pfTrDefaultMinSampleBytes
	}
	if cfg.VerifyDown == nil {
		cfg.VerifyDown = PFHashLocalFile
	}
	m := &PFTransfer{
		cfg:     cfg,
		speed:   NewPFSpeed(cfg.DefaultBps),
		remote:  cfg.Remote,
		running: map[*pfTrJob]struct{}{},
	}
	for d := range m.limiter {
		m.limiter[d] = rate.NewLimiter(rate.Inf, pfTrLimiterBurst)
	}
	return m, nil
}

// SetRemote 换远端文件系统（重连之后用）。已在传的任务下一次读写起用新的会不安全，
// 所以只对之后开始的传输生效；通信错误后调用方应等任务返回再换。
func (m *PFTransfer) SetRemote(fs PFTransferFS) {
	m.mu.Lock()
	m.remote = fs
	m.mu.Unlock()
}

func (m *PFTransfer) getRemote() PFTransferFS {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.remote
}

// SetLimit 设置某方向的限速（字节/秒），0 = 不限；运行中随时可改，下一次读写起生效。
func (m *PFTransfer) SetLimit(dir PFDirection, bytesPerSec int64) {
	if !dir.valid() {
		return
	}
	if bytesPerSec <= 0 {
		bytesPerSec = 0
		m.limiter[dir].SetLimit(rate.Inf)
	} else {
		m.limiter[dir].SetLimit(rate.Limit(bytesPerSec))
	}
	m.limiter[dir].SetBurst(pfTrLimiterBurst)
	m.limit[dir].Store(bytesPerSec)
}

// Limit 返回某方向当前的限速（0 = 不限）。
func (m *PFTransfer) Limit(dir PFDirection) int64 {
	if !dir.valid() {
		return 0
	}
	return m.limit[dir].Load()
}

// Speed 返回测速器（可 Observe/Rate/Reset）。
func (m *PFTransfer) Speed() *PFSpeed { return m.speed }

// ResetSpeed 是「网络状态变了」的接口：丢掉两个方向的测量值，缺省换成 defaultBps（<=0 沿用原缺省）。
func (m *PFTransfer) ResetSpeed(defaultBps float64) { m.speed.Reset(defaultBps) }

// EstimateSeconds 预计传完 remaining 字节还要多少秒。
func (m *PFTransfer) EstimateSeconds(remaining int64, dir PFDirection) float64 {
	return m.speed.EstimateSeconds(remaining, dir)
}

// ConfirmedBytes 返回这个任务在暂存区里已经确认的字节数（只读 meta，不读回内容，不改任何东西）：
// 没有 = 0；核验过的完整 part = Size。用来估算「传了一半的按剩余算」。
func (m *PFTransfer) ConfirmedBytes(ctx context.Context, job PFTransferJob) (int64, error) {
	if err := m.checkJob(job); err != nil {
		return 0, err
	}
	r := m.newRun(&pfTrJob{spec: job, ctx: ctx})
	meta, err := r.loadMeta()
	if err != nil || meta == nil || !r.metaMatches(meta) {
		return 0, err
	}
	if meta.hasVer {
		return job.Size, nil
	}
	return min(int64(len(meta.blocks))*m.cfg.BlockSize, job.Size), nil
}

// EstimateJob 预计这个任务还要多少秒（已确认的部分不算）。
func (m *PFTransfer) EstimateJob(ctx context.Context, job PFTransferJob) (float64, error) {
	done, err := m.ConfirmedBytes(ctx, job)
	if err != nil {
		return 0, err
	}
	return m.EstimateSeconds(job.Size-done, job.Dir), nil
}

// PartPath 返回这个内容在暂存区里的 part 路径（目标机上的路径）。
func (m *PFTransfer) PartPath(dir PFDirection, sha256 string) string {
	r := m.newRun(&pfTrJob{spec: PFTransferJob{Dir: dir, SHA256: sha256}})
	return r.part
}

// Stats 返回队列快照。
func (m *PFTransfer) Stats() PFTransferStats {
	m.mu.Lock()
	defer m.mu.Unlock()
	return PFTransferStats{Pending: len(m.pending), Running: len(m.running), RunningSmall: m.small, RunningLarge: m.large}
}

// CleanStaging 删掉某方向暂存区里 maxAge 没被引用的 part（上传清远端、下载清本机）；
// 正在传的内容不动。调用方每天调一次。
func (m *PFTransfer) CleanStaging(ctx context.Context, dir PFDirection, maxAge time.Duration) (PFCleanResult, error) {
	r := m.newRun(&pfTrJob{spec: PFTransferJob{Dir: dir}, ctx: ctx})
	if r.stage == nil {
		return PFCleanResult{}, &PFTransferError{Kind: PFErrConfig, Dir: dir, Path: r.stageDir, Msg: "没有可用的远端连接，无法清理暂存区"}
	}
	return PFCleanStaging(ctx, r.stage, r.stageDir, maxAge, time.Now(), func(sha string) bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		for j := range m.running {
			if j.spec.Dir == dir && j.spec.SHA256 == sha {
				return true
			}
		}
		for _, j := range m.pending {
			if j.spec.Dir == dir && j.spec.SHA256 == sha {
				return true
			}
		}
		return false
	})
}

// Close 取消所有任务并等它们结束；part 都保留。之后 Submit 会报错。
func (m *PFTransfer) Close() {
	m.mu.Lock()
	m.closed = true
	pend := m.pending
	m.pending = nil
	var run []*pfTrJob
	for j := range m.running {
		run = append(run, j)
	}
	m.mu.Unlock()
	for _, j := range pend {
		j.cancel()
		m.finish(j, PFTransferResult{Job: j.spec}, m.canceledErr(j))
	}
	for _, j := range run {
		j.cancel()
	}
	m.wg.Wait()
}

// ── 任务 ────────────────────────────────────────────────

type pfTrJob struct {
	spec   PFTransferJob
	ctx    context.Context
	cancel context.CancelFunc
	seq    uint64
	done   chan struct{}
	stop   func() bool // 取消 context.AfterFunc

	// 以下在持 m.mu 时读写，或只由正在跑这个任务的 goroutine 读写（任务同一时刻只有一个 goroutine 在跑）
	confirmed   atomic.Int64 // 已确认字节，估预计时间用
	verifyFails int
	attempts    int  // 搬运轮数（核验不符重来才加一，让出后续传不加）
	attemptOpen bool // 当前这一轮已经开始、还没以核验不符结束
	yields      int
	sent        int64
	firstOffset int64
	firstSet    bool
	started     time.Time

	res PFTransferResult
	err error
}

// PFTransferHandle 是 Submit 返回的句柄。
type PFTransferHandle struct {
	m *PFTransfer
	j *pfTrJob
}

// Done 在任务结束（成功、失败、取消）后关闭。
func (h *PFTransferHandle) Done() <-chan struct{} { return h.j.done }

// Wait 等任务结束，返回结果。
func (h *PFTransferHandle) Wait() (PFTransferResult, error) {
	<-h.j.done
	return h.j.res, h.j.err
}

// Cancel 取消任务；part 保留，之后可以续传。
func (h *PFTransferHandle) Cancel() { h.m.cancelJob(h.j) }

func (m *PFTransfer) checkJob(job PFTransferJob) error {
	bad := func(msg string) error {
		return &PFTransferError{Kind: PFErrConfig, Dir: job.Dir, Path: job.Src, Msg: msg}
	}
	switch {
	case !job.Dir.valid():
		return bad("方向不对")
	case job.Src == "":
		return bad("源路径为空")
	case job.Size < 0:
		return bad("大小不能为负")
	case !pfTrHex64.MatchString(job.SHA256):
		return bad("期望的 sha256 必须是 64 位小写十六进制")
	}
	if job.Dir == PFUpload {
		if m.cfg.RemoteStaging == "" {
			return bad("没有配置远端暂存区目录")
		}
		if m.cfg.VerifyUp == nil {
			return bad("没有配置上传方向的目标端核验（需要在 Ubuntu 本机算整体哈希）")
		}
	} else if m.cfg.LocalStaging == "" {
		return bad("没有配置本机暂存区目录")
	}
	if m.getRemote() == nil {
		return bad("没有远端连接")
	}
	return nil
}

// Submit 提交任务，立即返回；ctx 取消等同于 Cancel。
func (m *PFTransfer) Submit(ctx context.Context, job PFTransferJob) (*PFTransferHandle, error) {
	if err := m.checkJob(job); err != nil {
		return nil, err
	}
	jctx, cancel := context.WithCancel(ctx)
	j := &pfTrJob{spec: job, ctx: jctx, cancel: cancel, done: make(chan struct{})}
	j.stop = context.AfterFunc(ctx, func() { m.cancelJob(j) })
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		cancel()
		j.stop()
		return nil, &PFTransferError{Kind: PFErrCanceled, Dir: job.Dir, Path: job.Src, Msg: "传输队列已关闭"}
	}
	m.seq++
	j.seq = m.seq
	m.pending = append(m.pending, j)
	m.schedule()
	m.mu.Unlock()
	return &PFTransferHandle{m: m, j: j}, nil
}

// Transfer 提交并等结果。
func (m *PFTransfer) Transfer(ctx context.Context, job PFTransferJob) (PFTransferResult, error) {
	h, err := m.Submit(ctx, job)
	if err != nil {
		return PFTransferResult{Job: job}, err
	}
	return h.Wait()
}

func (m *PFTransfer) canceledErr(j *pfTrJob) error {
	return &PFTransferError{Kind: PFErrCanceled, Dir: j.spec.Dir, Path: j.spec.Src, Msg: "传输已取消（暂存区文件保留，可续传）", Err: context.Canceled}
}

func (m *PFTransfer) cancelJob(j *pfTrJob) {
	j.cancel()
	m.mu.Lock()
	for i, p := range m.pending {
		if p == j {
			m.pending = append(m.pending[:i], m.pending[i+1:]...)
			m.mu.Unlock()
			m.finish(j, PFTransferResult{Job: j.spec, ResumedFrom: j.firstOffset, SentBytes: j.sent, Attempts: j.attempts, Yields: j.yields}, m.canceledErr(j))
			return
		}
	}
	m.mu.Unlock() // 在跑的由它自己的 goroutine 发现 ctx 取消后收尾
}

// finish 收尾：记结果、通知等待者。任务不能同时在 pending/running。
func (m *PFTransfer) finish(j *pfTrJob, res PFTransferResult, err error) {
	if !j.started.IsZero() {
		res.Duration = time.Since(j.started)
	}
	j.res, j.err = res, err
	j.cancel()
	if j.stop != nil {
		j.stop()
	}
	close(j.done)
	m.mu.Lock()
	m.schedule()
	m.mu.Unlock()
}

// ── 调度 ────────────────────────────────────────────────

func (m *PFTransfer) isSmall(j *pfTrJob) bool { return j.spec.Size < m.cfg.SmallBytes }

// schedule 把能启动的排队任务启动起来；调用时必须持 m.mu。
func (m *PFTransfer) schedule() {
	if m.closed || len(m.pending) == 0 {
		return
	}
	type ranked struct {
		j   *pfTrJob
		eta float64
	}
	rs := make([]ranked, len(m.pending))
	for i, j := range m.pending {
		rs[i] = ranked{j, m.speed.EstimateSeconds(j.spec.Size-j.confirmed.Load(), j.spec.Dir)}
	}
	sort.SliceStable(rs, func(a, b int) bool {
		x, y := rs[a], rs[b]
		if x.j.spec.Priority != y.j.spec.Priority {
			return x.j.spec.Priority > y.j.spec.Priority
		}
		if x.eta != y.eta {
			return x.eta < y.eta
		}
		if x.j.spec.Src != y.j.spec.Src {
			return x.j.spec.Src < y.j.spec.Src
		}
		return x.j.seq < y.j.seq
	})
	// 每个方向上最高的优先级（排队的和在传的一起算）
	const none = -1 << 31
	top := [2]int{none, none}
	for _, j := range m.pending {
		top[j.spec.Dir] = max(top[j.spec.Dir], j.spec.Priority)
	}
	for j := range m.running {
		top[j.spec.Dir] = max(top[j.spec.Dir], j.spec.Priority)
	}
	started := map[*pfTrJob]bool{}
	for _, r := range rs {
		j := r.j
		if j.spec.Priority < top[j.spec.Dir] || !m.hasRoom(j) || m.sameContentRunning(j) {
			continue
		}
		started[j] = true
		m.start(j)
	}
	if len(started) > 0 {
		rest := m.pending[:0]
		for _, j := range m.pending {
			if !started[j] {
				rest = append(rest, j)
			}
		}
		m.pending = rest
	}
}

func (m *PFTransfer) hasRoom(j *pfTrJob) bool {
	if m.isSmall(j) {
		return m.small < m.cfg.MaxSmall
	}
	return m.large[j.spec.Dir] < m.cfg.MaxLargePerDir
}

func (m *PFTransfer) sameContentRunning(j *pfTrJob) bool {
	for k := range m.running {
		if k.spec.key() == j.spec.key() {
			return true
		}
	}
	return false
}

func (m *PFTransfer) start(j *pfTrJob) {
	m.running[j] = struct{}{}
	if m.isSmall(j) {
		m.small++
	} else {
		m.large[j.spec.Dir]++
	}
	m.wg.Add(1)
	go m.runPass(j)
}

// higherWaiting 返回同方向有没有优先级更高的任务在排队或在传（传输每传完一块问一次）。
func (m *PFTransfer) higherWaiting(j *pfTrJob) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, k := range m.pending {
		if k.spec.Dir == j.spec.Dir && k.spec.Priority > j.spec.Priority {
			return true
		}
	}
	for k := range m.running {
		if k != j && k.spec.Dir == j.spec.Dir && k.spec.Priority > j.spec.Priority {
			return true
		}
	}
	return false
}

// runPass 跑任务的一轮：传完、出错，或让出（回到排队，part 保留）。
func (m *PFTransfer) runPass(j *pfTrJob) {
	defer m.wg.Done()
	if j.started.IsZero() {
		j.started = time.Now()
	}
	res, yield, err := m.transferPass(j)

	m.mu.Lock()
	delete(m.running, j)
	if m.isSmall(j) {
		m.small--
	} else {
		m.large[j.spec.Dir]--
	}
	if yield && err == nil && !m.closed && j.ctx.Err() == nil {
		j.yields++
		m.pending = append(m.pending, j)
		m.schedule()
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()
	if yield && err == nil {
		err = m.canceledErr(j)
	}
	if err != nil {
		res.PartPath = ""
	}
	m.finish(j, res, err)
}

// waitLimit 按该方向的限速等 n 字节的额度；ctx 取消立即返回错误。
func (m *PFTransfer) waitLimit(ctx context.Context, dir PFDirection, n int) error {
	lim := m.limit[dir].Load()
	if lim <= 0 {
		return ctx.Err()
	}
	l := m.limiter[dir]
	for n > 0 {
		k := min(n, pfTrLimiterBurst)
		// 每次只等约 100ms 的量，限速在运行中改掉时不会白等太久
		k = min(k, max(int(lim/10), 256))
		if err := l.WaitN(ctx, k); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		n -= k
	}
	return nil
}

// chunkFor 是该方向这一次读写的大小：限速很低时缩小，免得一口气写出去等太久才能响应取消。
func (m *PFTransfer) chunkFor(dir PFDirection) int {
	c := m.cfg.IOChunk
	if lim := m.limit[dir].Load(); lim > 0 && int64(c) > lim/2 {
		c = int(max(lim/2, 4096))
	}
	return c
}
