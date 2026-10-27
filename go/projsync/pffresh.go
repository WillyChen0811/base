package projsync

// 项目文件同步 T9b：指令服务「取最新」的核心逻辑（技术设计 §6.1、§12.6 末尾；HTTP 接口以后由 T11 接）。
//
// PFFresh.Paths（取最新）：
//   - 输入是【工作目录（桌面）里的绝对路径】，经过桌面链接。先 EvalSymlinks（文件已不在时解析到最近的存在的上级再接上剩下的名字，
//     因为 Ubuntu 上可能还有而 Mac 上没有的文件正是要取的），结果落在同步目录内、且属于一个登记为 syncing 的单元才认，
//     映射成「单元 + 单元内路径」；否则答「不在同步范围」。
//   - 按单元：Ubuntu 在线（Poll 成功、没断网）→ 以用户优先级（100）对这些路径跑一次 PFRoundRunUnit（PriorityPaths 设为这些路径），
//     在预算内等它完成；预计传不完的（超过预算）由那一轮的暂缓机制挡下，答「对方较新未取」并附一句话。
//     等到预算用完而那一轮还没跑完 → 答「未能确认」（那一轮在后台接着跑，不会半途取消）。
//   - Ubuntu 不在线（断网、连不上、租约在别的 Mac 手里）→ 读上一轮的本机状态：已知对方较新 → 「对方较新未取」+ 说明；
//     上次对齐超过 1 小时（参数）→ note「上次与 Ubuntu 对齐是 X，之后的改动未知」；否则不附 note。
//
// 结果里的 note 是程序给的事实，让模型照实转述。
//
// PFFresh.Touched（入件放入、指令写完文件后调）：把这些路径所在的单元记下，10 秒去抖后让调度跑这个单元（见 PFSched.MarkTouched）。
//
// 不管：HTTP、cmdproto 钩子、定时循环（T11/T12）。

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// 取最新的结果状态（PFFreshItem.State）。
const (
	PFFreshStateLatest   = "最新"
	PFFreshStateFetched  = "已取最新"
	PFFreshStateNewer    = "对方较新未取"
	PFFreshStateOffline  = "对方不在线"
	PFFreshStateOutScope = "不在同步范围"
	PFFreshStateConflict = "冲突"
	// PFFreshStateUnknown 是设计表之外加的一种：这一轮没能确认这个路径（文件还在被写、等不到单元锁、预算内没跑完、出错、结果不确定……），
	// 原因在 note。不能把「不知道」说成「最新」。
	PFFreshStateUnknown = "未能确认"
)

// PFFreshDefaultBudget 是缺省的等待预算（指令最多等 60 秒）。
const PFFreshDefaultBudget = 60 * time.Second

// PFFreshItem 是一个输入路径的结果。目录路径按其下所有内容合并成一个状态（冲突 > 对方较新未取 > 未能确认 > 已取最新 > 最新）。
type PFFreshItem struct {
	Path       string `json:"path"`                  // 原样返回输入的路径
	State      string `json:"state"`                 //
	OtherMtime string `json:"other_mtime,omitempty"` // 对方（Ubuntu）那份的修改时间，RFC3339；已知时才有
	Size       int64  `json:"size,omitempty"`
	Unit       string `json:"unit,omitempty"` // 单元编号
	Rel        string `json:"rel,omitempty"`  // 单元内相对路径（/ 分隔）；整个单元为空
}

// PFFreshResult 是取最新的结果。Items 与输入的 paths 一一对应、同序。
type PFFreshResult struct {
	Items []PFFreshItem `json:"items"`
	// Note 是一句（或几句，用「；」连接）给模型照实转述的事实；没有要说的为空。
	Note string `json:"note,omitempty"`
}

// PFFreshConfig 是 PFFresh 的配置。
type PFFreshConfig struct {
	SyncDir string // projfiles.sync_dir，绝对路径
}

// PFFresh 把调度器包成「取最新」。
type PFFresh struct {
	s   *PFSched
	cfg PFFreshConfig
}

// NewPFFresh 创建。
func NewPFFresh(s *PFSched, cfg PFFreshConfig) (*PFFresh, error) {
	if s == nil || !filepath.IsAbs(cfg.SyncDir) {
		return nil, errors.New("projsync: 取最新需要调度器和绝对路径的同步目录")
	}
	return &PFFresh{s: s, cfg: cfg}, nil
}

// pfffTarget 是一个输入路径映射的结果。
type pfffTarget struct {
	idx   int
	rec   PFUnitRecord
	inner string // 单元内路径，"" = 整个单元
}

// resolve 解析成真实路径；不存在的尾部名字原样接在最近的存在的上级之后。
func pfffResolve(p string) (string, bool) {
	if !filepath.IsAbs(p) {
		return "", false
	}
	cur := filepath.Clean(p)
	var tail []string
	for {
		r, err := filepath.EvalSymlinks(cur)
		if err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				r = filepath.Join(r, tail[i])
			}
			return r, true
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", false
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", false
		}
		tail = append(tail, filepath.Base(cur))
		cur = parent
	}
}

// mapPaths 把输入路径映射到单元。第二个返回值是每个路径的映射（nil = 不在同步范围）。
func (f *PFFresh) mapPaths(paths []string) ([]*pfffTarget, error) {
	out := make([]*pfffTarget, len(paths))
	syncReal, err := filepath.EvalSymlinks(f.cfg.SyncDir)
	if err != nil {
		return out, nil // 同步目录都不在：没有任何路径在范围内
	}
	reg, err := PFUnitMoveLoadUnits(f.s.cfg.MetaDir)
	if err != nil {
		return nil, err
	}
	for i, p := range paths {
		real, ok := pfffResolve(p)
		if !ok {
			continue
		}
		rel, err := filepath.Rel(syncReal, real)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		rel = filepath.ToSlash(rel)
		for _, u := range reg {
			if u.State != PFUnitSyncing {
				continue
			}
			if rel == u.Rel || strings.HasPrefix(rel, u.Rel+"/") {
				inner := strings.TrimPrefix(strings.TrimPrefix(rel, u.Rel), "/")
				if inner != "" && validRel(inner) != nil {
					break
				}
				out[i] = &pfffTarget{idx: i, rec: u, inner: inner}
				break
			}
		}
	}
	return out, nil
}

// Touched 记下这些路径所在的单元刚被写过（只认同步范围内的），返回记下的单元编号。
func (f *PFFresh) Touched(paths []string) []string {
	ts, err := f.mapPaths(paths)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var codes []string
	for _, t := range ts {
		if t != nil && !seen[t.rec.Code] {
			seen[t.rec.Code] = true
			codes = append(codes, t.rec.Code)
		}
	}
	sort.Strings(codes)
	f.s.MarkTouched(codes...)
	return codes
}

// Paths 取最新。budget <= 0 取缺省 60 秒。
func (f *PFFresh) Paths(ctx context.Context, paths []string, budget time.Duration) PFFreshResult {
	if budget <= 0 {
		budget = PFFreshDefaultBudget
	}
	deadline := time.Now().Add(budget)
	res := PFFreshResult{Items: make([]PFFreshItem, len(paths))}
	var notes []string
	for i, p := range paths {
		res.Items[i] = PFFreshItem{Path: p, State: PFFreshStateOutScope}
	}
	ts, err := f.mapPaths(paths)
	if err != nil {
		for i := range res.Items {
			res.Items[i].State = PFFreshStateUnknown
		}
		res.Note = fmt.Sprintf("读不了同步登记表，没法判断：%v", err)
		return res
	}
	groups := map[string][]*pfffTarget{}
	var order []string
	for _, t := range ts {
		if t == nil {
			continue
		}
		if _, ok := groups[t.rec.Code]; !ok {
			order = append(order, t.rec.Code)
		}
		groups[t.rec.Code] = append(groups[t.rec.Code], t)
		res.Items[t.idx].Unit, res.Items[t.idx].Rel = t.rec.Code, t.inner
	}
	if len(order) == 0 {
		return res
	}

	// 网络与 Poll：不在线就读本机状态
	st := f.s.getSettings()
	net := f.s.netNow()
	var poll PFRemotePollResponse
	offlineWhy := ""
	switch {
	case net == PFRemoteNetOffline:
		offlineWhy = "offline"
	default:
		pctx, cancel := context.WithDeadline(ctx, deadline)
		poll, err = f.s.cfg.Remote.Poll(pctx, PFRemotePollOptions{LeaseSeconds: st.LeaseSeconds, QuietSeconds: st.QuietSeconds})
		cancel()
		if err != nil {
			offlineWhy = "unreachable"
			if errors.Is(err, ErrPFRemoteLeaseHeld) {
				offlineWhy = "lease"
			}
		}
	}
	pollBy := map[string]PFRemotePollUnit{}
	for _, u := range poll.Units {
		pollBy[u.Code] = u
	}

	for _, code := range order {
		g := groups[code]
		var n string
		if offlineWhy != "" {
			n = f.offline(&res, g, offlineWhy, st)
		} else {
			n = f.online(ctx, &res, g, pollBy, net, st, deadline)
		}
		if n != "" {
			notes = append(notes, n)
		}
	}
	res.Note = strings.Join(notes, "；")
	return res
}

// ── Ubuntu 不在线 ───────────────────────────────────────

func (f *PFFresh) offline(res *PFFreshResult, g []*pfffTarget, why string, st PFSchedSettings) string {
	rec := g[0].rec
	saved, _ := PFRoundLoadState(f.s.cfg.MetaDir, rec.Code) // 读不了当没有
	var pend []PFRemotePending
	if saved != nil {
		pend = saved.Status.PendingToMac
	}
	var notes []string
	for _, t := range g {
		it := &res.Items[t.idx]
		it.State = PFFreshStateOffline
		var hits []PFRemotePending
		for _, p := range pend {
			if pfffUnder(p.Path, t.inner) {
				hits = append(hits, p)
			}
		}
		if len(hits) > 0 {
			it.State = PFFreshStateNewer
			if len(hits) == 1 {
				it.OtherMtime, it.Size = pfffNsRFC(hits[0].UbMtimeNs), hits[0].Size
			}
			notes = append(notes, pfffNewerNote(hits, "上一轮就已知")+"，现在连不上 Ubuntu，没法取")
		}
	}
	if why == "lease" {
		notes = append(notes, "Ubuntu 现在由另一台电脑在同步")
	}
	last, ok := f.s.lastAligned(rec.Code, saved)
	switch {
	case !ok:
		notes = append(notes, "还没有与 Ubuntu 对齐过的记录，Ubuntu 上的改动未知")
	case f.s.cfg.Now().Sub(last) > st.StaleAfter:
		notes = append(notes, fmt.Sprintf("上次与 Ubuntu 对齐是 %s，之后的改动未知", pfffClock(last)))
	}
	return strings.Join(uniqStrings(notes), "；")
}

// lastAligned：这个单元最近一次完整比较过两边（一轮 Done）的时间。
func (s *PFSched) lastAligned(code string, saved *PFRoundSaved) (time.Time, bool) {
	s.mu.Lock()
	u := s.units[code]
	var t time.Time
	if u != nil {
		t = u.lastDone
	}
	s.mu.Unlock()
	if saved != nil && saved.Outcome == PFRoundDone {
		if st, err := time.Parse(time.RFC3339, saved.At); err == nil && st.After(t) {
			t = st
		}
	}
	return t, !t.IsZero()
}

// ── Ubuntu 在线 ─────────────────────────────────────────

// pfffRun 是 online 那一轮 goroutine 的返回：跑出来的结果、拿没拿到锁、是否跳过（登记变了或超时被取消）、
// 以及给人看的一句话原因。
type pfffRun struct {
	res     *PFRoundResult
	locked  bool
	skipped bool   // 拿到锁后重读登记表发现项目刚开始/取消同步；或预算用完那一轮被取消
	reason  string // 给人看的一句话（skipped 时用）
}

// pfffCancelStopGrace 是取消后等那一轮真正停下的兜底时间。假传输/假 Runner 通常秒级回，真传输被 cancel 之后
// 也是每个 block 检查一次；给到 30 秒是防止某个环节拖太久让 HTTP 应答挂死。
const pfffCancelStopGrace = 30 * time.Second

func (f *PFFresh) online(ctx context.Context, out *PFFreshResult, g []*pfffTarget, pollBy map[string]PFRemotePollUnit, net string, st PFSchedSettings, deadline time.Time) string {
	s := f.s
	rec := g[0].rec
	ubRel := rec.Rel
	pu, havePoll := pollBy[rec.Code]
	if havePoll {
		ubRel = pu.Rel
	}
	remain := time.Until(deadline)
	if remain < time.Second {
		remain = time.Second
	}
	in := s.roundInput(rec, ubRel, st)
	in.Priority = PFRoundPrioUser
	for _, t := range g {
		in.PriorityPaths = append(in.PriorityPaths, t.inner)
	}
	in.Limits.MaxUser = remain
	if in.Limits.MaxGood <= 0 || in.Limits.MaxGood > remain {
		in.Limits.MaxGood = remain
	}
	in.UbuntuLeftovers = s.currentLeftovers()

	done := make(chan pfffRun, 1)
	// 拿锁 + 那一轮跑都用同一个可取消的 runCtx（从 baseCtx 派生）：
	// 预算用完或调用方断开都 cancelRun()，让正在等锁或正在传的 block 都停下，
	// 免得调用方（解析层指令）已按旧文件继续写回、后台却还在把新版装进来把新版盖掉。
	runCtx, cancelRun := context.WithCancel(s.baseCtx)
	defer cancelRun()
	go func() {
		unlock, ok := s.lockUnit(runCtx, rec.Code, 0)
		if !ok {
			done <- pfffRun{}
			return
		}
		defer unlock()
		// 拿到锁之后重读登记表：拿锁前若刚点了 start/stop、改了 Rel/SyncPath，这一轮不跑。
		if stillOK, why, err := s.pfscUnitSyncingOnDisk(rec.Code, rec.Rel, rec.SyncPath); !stillOK || err != nil {
			if err != nil {
				why = "重读登记表失败，这一轮跳过：" + err.Error()
			}
			r := &PFRoundResult{Unit: rec.Code, Outcome: PFRoundSkipped, Reason: why,
				StartedAt: s.cfg.Now(), EndedAt: s.cfg.Now()}
			done <- pfffRun{res: r, skipped: true, reason: why}
			return
		}
		r, err := PFRoundRunUnit(runCtx, in)
		if runCtx.Err() != nil {
			// 那一轮被取消：无论 PFRoundRunUnit 是回了结果还是抛了错，都不能记进内存印象
			// （半截的 macFP 不能代表本机），也不能让调用方以为已经拿到最新。
			done <- pfffRun{res: r, locked: true, skipped: true, reason: "预算用完或调用方断开，这一轮已取消"}
			return
		}
		if err != nil {
			r = &PFRoundResult{Unit: rec.Code, Outcome: PFRoundFailed, Reason: err.Error(), StartedAt: s.cfg.Now(), EndedAt: s.cfg.Now()}
			r.Errors = append(r.Errors, PFRoundError{Code: "bad_input", Message: err.Error()})
		}
		// macFP 留空：下一轮定时同步一定会再看一遍这个单元
		s.remember(pfscCand{rec: rec, ubFP: pu.Fingerprint}, r)
		done <- pfffRun{res: r, locked: true}
	}()
	timer := time.NewTimer(remain)
	defer timer.Stop()
	var run pfffRun
	select {
	case run = <-done:
	case <-timer.C:
		// 预算用完：取消那一轮，**等它真正停下**（正在装的那个文件装完或放弃），
		// 再答「未能确认」——不能让后台继续把新版装进 Mac、而调用方（解析层指令）已按旧文件继续写回。
		cancelRun()
		gctx, gcancel := context.WithTimeout(context.Background(), pfffCancelStopGrace)
		defer gcancel()
		select {
		case run = <-done:
		case <-gctx.Done():
			// 兜底：真传输出问题没能停下来时不能永远挂着，等满宽限期后照样答未确认。
			return f.noAnswer(out, g, "没取完，等它停下也没等到，用的是本机现有的文件")
		}
		return f.noAnswer(out, g, "没取完，已停下，用的是本机现有的文件")
	case <-ctx.Done():
		cancelRun()
		// 调用方（HTTP 请求）已断开，等它停下同样上限。
		gctx, gcancel := context.WithTimeout(context.Background(), pfffCancelStopGrace)
		defer gcancel()
		select {
		case <-done:
		case <-gctx.Done():
		}
		return f.noAnswer(out, g, "已取消")
	}
	if run.skipped {
		// 拿到锁之后重读登记表：项目刚开始/取消同步/Rel 或 SyncPath 变了；或预算用完那一轮被取消。
		// 都不写盘、也不把这一轮记进内存印象——反正没跑成或已停下。
		why := run.reason
		if why == "" && run.res != nil {
			why = run.res.Reason
		}
		if why == "" {
			why = "这一轮没跑成，已停下"
		}
		return f.noAnswer(out, g, why)
	}
	if !run.locked {
		return f.noAnswer(out, g, "这个项目正在同步")
	}
	return f.fromResult(out, g, run.res)
}

func (f *PFFresh) noAnswer(out *PFFreshResult, g []*pfffTarget, why string) string {
	for _, t := range g {
		out.Items[t.idx].State = PFFreshStateUnknown
	}
	return fmt.Sprintf("项目 %s：没能确认是不是最新（%s）", g[0].rec.Code, why)
}

func (s *PFSched) currentLeftovers() []PFRemoteLeftover {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]PFRemoteLeftover(nil), s.leftover...)
}

// fromResult 把一轮的结果翻成各路径的状态和一句话。
func (f *PFFresh) fromResult(out *PFFreshResult, g []*pfffTarget, r *PFRoundResult) string {
	code := g[0].rec.Code
	// 整轮没做成
	switch r.Outcome {
	case PFRoundLeaseHeld:
		for _, t := range g {
			out.Items[t.idx].State = PFFreshStateOffline
		}
		return "Ubuntu 现在由另一台电脑在同步"
	case PFRoundSkipped, PFRoundFailed, PFRoundStopped:
		st := PFFreshStateUnknown
		for _, e := range r.Errors {
			if e.Code == "ubuntu_comm" || e.Code == "lease_held" {
				st = PFFreshStateOffline
			}
		}
		for _, t := range g {
			out.Items[t.idx].State = st
		}
		if st == PFFreshStateOffline {
			return ""
		}
		return fmt.Sprintf("项目 %s：没能确认是不是最新（%s）", code, r.Reason)
	}
	var notes []string
	pend := map[string]PFRemotePending{}
	for _, p := range r.Status.PendingToMac {
		pend[p.Path] = p
	}
	for _, t := range g {
		it := &out.Items[t.idx]
		state, why := pfffStateOf(r, t.inner)
		it.State = state
		var hits []PFRemotePending
		for _, d := range r.Deferred {
			if d.Dir == PFRoundToMac && pfffUnder(d.Path, t.inner) {
				p := pend[d.Path]
				if p.Path == "" {
					p = PFRemotePending{Path: d.Path, Size: d.Size}
				}
				hits = append(hits, p)
			}
		}
		switch state {
		case PFFreshStateNewer:
			if len(hits) == 1 {
				it.OtherMtime, it.Size = pfffNsRFC(hits[0].UbMtimeNs), hits[0].Size
			}
			reason := ""
			for _, d := range r.Deferred {
				if d.Dir == PFRoundToMac && pfffUnder(d.Path, t.inner) {
					reason = d.Reason
					break
				}
			}
			n := pfffNewerNote(hits, "Ubuntu 上有较新的")
			if reason != "" {
				n += "：" + reason
			}
			notes = append(notes, n)
		case PFFreshStateConflict:
			for _, c := range r.Conflicts {
				if pfffUnder(c.Path, t.inner) || pfffUnder(c.ConflictPath, t.inner) {
					notes = append(notes, fmt.Sprintf("%s 两边都改过，没有覆盖，另存了冲突副本 %s", c.Path, c.ConflictPath))
				}
			}
		case PFFreshStateUnknown, PFFreshStateOutScope:
			if why != "" {
				notes = append(notes, why)
			}
		case PFFreshStateLatest, PFFreshStateFetched:
			f.fillFromDisk(it, g[0].rec, t.inner)
		}
	}
	if r.Uncertain {
		notes = append(notes, "这一轮中途出了不确定的情况，已停止后续写入："+r.UncertainReason)
	}
	return strings.Join(uniqStrings(notes), "；")
}

// fillFromDisk：路径是文件时，补上本机现在的大小和修改时间（两边一致，等于对方的）。
func (f *PFFresh) fillFromDisk(it *PFFreshItem, rec PFUnitRecord, inner string) {
	if inner == "" {
		return
	}
	fi, err := os.Lstat(filepath.Join(rec.SyncPath, filepath.FromSlash(inner)))
	if err != nil || !fi.Mode().IsRegular() {
		return
	}
	it.Size, it.OtherMtime = fi.Size(), fi.ModTime().UTC().Format(time.RFC3339)
}

// pfffStateOf 判一个单元内路径（目录取子树）在这一轮之后的状态。
func pfffStateOf(r *PFRoundResult, inner string) (state, why string) {
	for _, c := range r.Conflicts {
		if pfffUnder(c.Path, inner) || pfffUnder(c.ConflictPath, inner) {
			return PFFreshStateConflict, ""
		}
	}
	for _, d := range r.Deferred {
		if d.Dir == PFRoundToMac && pfffUnder(d.Path, inner) {
			return PFFreshStateNewer, ""
		}
	}
	for _, s := range r.Skipped {
		if s.Source == PFRoundSrcTodo || !pfffUnder(s.Path, inner) {
			continue
		}
		switch s.Code {
		case PFSkipOutOfScope, PFCodeSymlink, PFCodeSpecialFile, PFCodeFilter, PFCodeTooDeep:
			state, why = PFFreshStateOutScope, fmt.Sprintf("%s 超出同步范围（%s）", s.Path, s.Reason)
			continue
		}
		return PFFreshStateUnknown, fmt.Sprintf("%s 这一轮没能比较（%s）", s.Path, s.Reason)
	}
	if state != "" {
		return state, why
	}
	for _, e := range r.Errors {
		if e.Path != "" && pfffUnder(e.Path, inner) {
			return PFFreshStateUnknown, fmt.Sprintf("%s 出错：%s", e.Path, e.Message)
		}
	}
	if r.Uncertain {
		return PFFreshStateUnknown, ""
	}
	for _, d := range r.Done {
		if d.Side == PFSideMac && pfffUnder(d.Path, inner) {
			return PFFreshStateFetched, ""
		}
	}
	return PFFreshStateLatest, ""
}

// ── 小工具 ──────────────────────────────────────────────

// pfffUnder：path 等于 prefix 或在 prefix 之下；prefix 为空 = 整个单元。
func pfffUnder(path, prefix string) bool {
	return prefix == "" || path == prefix || strings.HasPrefix(path, prefix+"/")
}

func pfffNsRFC(ns int64) string {
	if ns == 0 {
		return ""
	}
	return time.Unix(0, ns).UTC().Format(time.RFC3339)
}

func pfffClock(t time.Time) string { return t.Local().Format("1月2日 15:04") }

func pfffSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fGB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0fKB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d字节", n)
}

func pfffNewerNote(ps []PFRemotePending, lead string) string {
	if len(ps) == 0 {
		return lead + "改动"
	}
	var parts []string
	for i, p := range ps {
		if i >= 3 {
			parts = append(parts, fmt.Sprintf("等共 %d 个", len(ps)))
			break
		}
		s := p.Path + "（" + pfffSize(p.Size)
		if p.UbMtimeNs != 0 {
			s += "，" + pfffClock(time.Unix(0, p.UbMtimeNs))
		}
		parts = append(parts, s+"）")
	}
	return lead + "版本 " + strings.Join(parts, "、") + " 还没取过来"
}

func uniqStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
