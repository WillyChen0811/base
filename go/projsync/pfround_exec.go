package projsync

// 项目文件同步 T9a：一轮里「按计划顺序执行动作」的部分（技术设计 §2.3、§3.3、§3.4 执行、§5.4 暂缓、§5.5、§5.6）。
//
// 总原则：
//   - 动作按 PFPlan.Actions 的顺序一个个执行（Phase 0 建目录 → 1 文件 → 2 删空目录 → 3 台账）。
//   - Mac 侧动作用本机 PFApplier 立刻执行；Ubuntu 侧动作攒成 PFRemoteStep 批量发 Apply（遇到阶段交界、需要先后依赖的多步动作、
//     同一个上传 part 要再装一次时发出去）。每个动作成功后才把它的 Base 变更一次 Apply 写进 base；Fill* 为真的那一边装好后
//     重新 stat（Mac 自己 stat，Ubuntu 取 apply 结果里的 Installed），把真实 mtime/ino 填进去。
//   - 要传字节的动作（copy_new / copy_over / 冲突）先按预计时间判暂缓（pfRoundAdmit）；放行的先把传输任务全部提交给传输器
//     （小文件并发，大文件按方向串行），执行到那个动作时才等它传完、拿到核验过的 part 再安装。
//   - 停止规则见 pfround.go 文件头；停止后没执行的动作记成「没做」，Ubuntu 批里没发出去的步骤丢掉。

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// pfrdMaxBatchSteps：一批 apply 最多放这么多步（协议上限 500，留余量）。
const pfrdMaxBatchSteps = 400

// ── 暂缓（§5.4）────────────────────────────────────────

// pfRoundAdmit 判断一次传输放不放行。est 是预计还要传多少秒。
//
// 细则：断网全暂缓；网好缺省全放行（MaxGood=0），设了就按它；热点（expensive）或未知（unknown，含空串）预计 ≤ MaxExpensive 放行，
// 用户指令或 Ubuntu 请求要的文件（wanted）放宽到 ≤ MaxUser。返回的 limit 是适用的上限（秒，0 表示没有）。
func pfRoundAdmit(net string, lim PFRoundLimits, wanted bool, est float64) (ok bool, why string, limit float64) {
	switch net {
	case PFRemoteNetOffline:
		return false, "断网，传输暂缓", 0
	case PFRemoteNetGood:
		if lim.MaxGood <= 0 || est <= lim.MaxGood.Seconds() {
			return true, "", lim.MaxGood.Seconds()
		}
		return false, fmt.Sprintf("预计要 %.0f 秒，超过网好时的上限 %.0f 秒，暂缓", est, lim.MaxGood.Seconds()), lim.MaxGood.Seconds()
	}
	netName := "未知网络（按热点对待）"
	if net == PFRemoteNetExpensive {
		netName = "热点"
	}
	limit = lim.MaxExpensive.Seconds()
	if wanted {
		limit = lim.MaxUser.Seconds()
	}
	if est <= limit {
		return true, "", limit
	}
	return false, fmt.Sprintf("%s下预计要 %.0f 秒，超过 %.0f 秒的上限，暂缓，网好后自动补传", netName, est, limit), limit
}

type pfrdAdm struct {
	jobs     []PFTransferJob
	deferred bool
	est      float64
	limit    float64
	wanted   bool
	why      string
	skipCode string // 非空：准备阶段就出错了，整个动作跳过
	skipWhy  string
}

type pfrdXfer struct {
	h    *PFTransferHandle
	used bool
}

func (r *pfrdRun) wanted(rel string) bool {
	if r.in.Priority <= PFRoundPrioNormal {
		return false
	}
	if len(r.in.PriorityPaths) == 0 {
		return true
	}
	for _, p := range r.in.PriorityPaths {
		p = strings.Trim(p, "/")
		if p == "" || rel == p || strings.HasPrefix(rel, p+"/") {
			return true
		}
	}
	return false
}

func (r *pfrdRun) prio(rel string) int {
	if r.wanted(rel) {
		return int(r.in.Priority)
	}
	return PFPrioNormal
}

// jobFor 造一个传输任务：src 边 rel 这份（内容为 exp）搬到另一边的暂存区。
func (r *pfrdRun) jobFor(src PFSide, rel string, exp *PFExpect) PFTransferJob {
	j := PFTransferJob{Size: exp.Size, SHA256: exp.SHA256, Priority: r.prio(rel)}
	if src == PFSideMac {
		j.Dir, j.Src = PFUpload, filepath.Join(r.macRoot, filepath.FromSlash(rel))
	} else {
		j.Dir, j.Src = PFDownload, path.Join(r.ubPath, rel)
	}
	return j
}

// admitAll 在执行之前把所有要传字节的动作过一遍：判暂缓，放行的提交给传输器。
func (r *pfrdRun) admitAll() {
	unconditional := r.in.Net == PFRemoteNetGood && r.in.Limits.MaxGood <= 0
	for i, a := range r.plan.Actions {
		if r.stopped {
			return
		}
		var jobs []PFTransferJob
		switch a.Kind {
		case PFActCopyNew, PFActCopyOver:
			jobs = []PFTransferJob{r.jobFor(a.Src, a.Path, a.SrcExpect)}
		case PFActConflict:
			// 胜出版本搬到落败边，落败版本搬到胜出边：两个都放行才做
			jobs = []PFTransferJob{r.jobFor(a.Src, a.Path, a.SrcExpect), r.jobFor(a.Dst, a.Path, a.DstExpect)}
		default:
			continue
		}
		adm := &r.adm[i]
		adm.jobs, adm.wanted = jobs, r.wanted(a.Path)
		if !unconditional {
			for _, j := range jobs {
				est, err := r.in.Transfer.EstimateJob(r.ctx, j)
				if err != nil {
					var te *PFTransferError
					if errors.As(err, &te) && te.Uncertain() {
						r.markUncertain(true, "估算传输时间时 Ubuntu 通信失败：%v", err)
						return
					}
					adm.skipCode, adm.skipWhy = "estimate", fmt.Sprintf("估算传输时间失败：%v", err)
					break
				}
				adm.est += est
				if ok, why, lim := pfRoundAdmit(r.in.Net, r.in.Limits, adm.wanted, est); !ok {
					adm.deferred, adm.why, adm.limit = true, why, lim
					break
				}
			}
		}
		if adm.deferred || adm.skipCode != "" {
			continue
		}
		for _, j := range jobs {
			r.submit(j)
		}
	}
}

func (r *pfrdRun) submit(j PFTransferJob) {
	key := j.key()
	if _, ok := r.xfers[key]; ok {
		return
	}
	h, err := r.in.Transfer.Submit(r.ctx, j)
	if err != nil {
		return // 到执行时再试一次，错误在那时记
	}
	r.xfers[key] = &pfrdXfer{h: h}
}

// cancelXfers 取消没用上的传输任务（part 保留，下一轮续传）并等它们结束。
func (r *pfrdRun) cancelXfers() {
	for _, x := range r.xfers {
		if !x.used {
			x.h.Cancel()
			x.h.Wait()
		}
	}
}

func (r *pfrdRun) addBytes(dir PFDirection, n int64) {
	if dir == PFUpload {
		r.res.BytesUp += n
	} else {
		r.res.BytesDown += n
	}
}

// fetch 拿到一个传输任务的结果（已核验的 part 在目标机暂存区）。第一次用预先提交的任务，
// 同一内容再要一份就重新传（传输层会复用暂存区里还在的）；上传的 part 若还被挂着的批里的 install 用着，先把批发出去。
func (r *pfrdRun) fetch(i int, a PFAction, job PFTransferJob) (PFTransferResult, bool) {
	if job.Dir == PFUpload && r.batchSHA[job.SHA256] {
		r.flush()
	}
	if r.stopped {
		r.markNotRun(i, a)
		return PFTransferResult{}, false
	}
	var res PFTransferResult
	var err error
	if x := r.xfers[job.key()]; x != nil && !x.used {
		x.used = true
		res, err = x.h.Wait()
	} else {
		res, err = r.in.Transfer.Transfer(r.ctx, job)
	}
	r.addBytes(job.Dir, res.SentBytes)
	if err != nil {
		r.transferFailed(i, a, job, err)
		return res, false
	}
	return res, true
}

func (r *pfrdRun) transferFailed(i int, a PFAction, job PFTransferJob, err error) {
	var te *PFTransferError
	if !errors.As(err, &te) {
		r.addError(a.Path, "transfer", false, "传输 %s 失败：%v", a.Path, err)
		r.setSkip(i, a, "", "transfer", fmt.Sprintf("传输失败：%v", err))
		return
	}
	switch te.Kind {
	case PFErrComm:
		r.markUncertain(true, "传输 %s 时 Ubuntu 通信失败（%s）：%v", a.Path, job.Dir, err)
		r.setSkip(i, a, "", "transfer_comm", fmt.Sprintf("传输时通信失败：%s", te.Msg))
	case PFErrCanceled:
		if r.ctx.Err() != nil {
			r.stopHard("canceled", "已取消")
		}
		r.setSkip(i, a, "", "canceled", "传输被取消（暂存文件保留，下一轮续传）")
	case PFErrSource:
		r.addError(a.Path, "source_missing", false, "传输 %s 失败：%s", a.Path, te.Msg)
		r.setSkip(i, a, "", "source_missing", "源文件不见了或不是文件，本轮不传，下一轮重新判定")
	case PFErrVerify:
		r.addError(a.Path, "verify_failed", false, "传输 %s 失败：%s", a.Path, te.Msg)
		r.setSkip(i, a, "", "verify_failed", "传完后哈希对不上（源文件在传输中被改了？），丢弃这次传输，下一轮重新判定")
	default:
		r.addError(a.Path, "transfer", false, "传输 %s 失败：%v", a.Path, err)
		r.setSkip(i, a, "", "transfer", fmt.Sprintf("传输失败：%s", te.Msg))
	}
}

// ── 记录 ────────────────────────────────────────────────

func (r *pfrdRun) setSkip(i int, a PFAction, side PFSide, code, reason string) {
	r.state[i] = pfrdSkipped
	r.res.Skipped = append(r.res.Skipped, PFRoundSkip{Path: a.Path, Kind: a.Kind, Side: side, Code: code, Reason: reason, Source: PFRoundSrcApply})
}

func (r *pfrdRun) markNotRun(i int, a PFAction) {
	if r.state[i] != pfrdNotRun {
		return
	}
	r.res.Skipped = append(r.res.Skipped, PFRoundSkip{Path: a.Path, Kind: a.Kind, Code: "stopped",
		Reason: "这个单元本轮已停止后续写入，这一步没有执行，下一轮重新采集判定", Source: PFRoundSrcStopped})
}

func (r *pfrdRun) recordDone(i int, a PFAction, bytes int64) {
	r.state[i] = pfrdDone
	item := PFRoundDoneItem{Kind: a.Kind, Path: a.Path, Side: a.Dst, From: a.From, Bytes: bytes}
	if a.Kind == PFActBaseSet || a.Kind == PFActBaseDel {
		item.Side = ""
	}
	r.res.Done = append(r.res.Done, item)
	if a.Notice != "" {
		r.notice("%s", a.Notice)
	}
}

func (r *pfrdRun) recordDeferred(i int, a PFAction) {
	adm := r.adm[i]
	r.state[i] = pfrdDeferred
	dir := PFRoundToMac
	if a.Src == PFSideMac {
		dir = PFRoundToUbuntu
	}
	r.res.Deferred = append(r.res.Deferred, PFRoundDeferred{Path: a.Path, Size: a.SrcExpect.Size, Dir: dir,
		EstSeconds: adm.est, Limit: adm.limit, Wanted: adm.wanted, Reason: adm.why, Conflict: a.Kind == PFActConflict})
}

// ── base ────────────────────────────────────────────────

// touchesMac：这个动作会让 Mac 上出现（或换成）一个内容已知的文件。
func touchesMac(a PFAction) bool {
	return a.Dst == PFSideMac || a.Kind == PFActConflict
}

func (r *pfrdRun) macStatOf(rel string) (PFStat, bool) {
	st, err := r.statf(filepath.Join(r.macRoot, filepath.FromSlash(rel)))
	return st, err == nil
}

// baseOps 把动作的 Base 变更变成 base 的操作：Fill* 为真的那一边用真实 stat 替换预估值。
func (r *pfrdRun) baseOps(a PFAction, ub map[string]PFFileInfo) []PFBaseOp {
	ops := make([]PFBaseOp, 0, len(a.Base))
	for _, ch := range a.Base {
		if ch.Del {
			ops = append(ops, PFBaseOp{Path: ch.Path, Del: true})
			continue
		}
		e := ch.Entry
		if ch.FillMac || (touchesMac(a) && e.T == PFBaseTypeFile) {
			if st, ok := r.macStatOf(ch.Path); ok && ((e.T == PFBaseTypeFile && st.Kind == PFKindFile && st.Size == e.Size) || (e.T == PFBaseTypeDir && st.Kind == PFKindDir)) {
				if ch.FillMac {
					e.Mac = PFSideStat{MtimeNs: st.MtimeNs, Ino: st.Ino}
				}
				if e.T == PFBaseTypeFile {
					// 装好的文件内容已核验过，哈希进缓存，下一轮不用重读大文件
					r.cache.Put(ch.Path, st, e.SHA256, r.now(), r.in.QuietSeconds)
					r.installedMac[ch.Path] = true
				}
			}
		}
		if ch.FillUb {
			if fi, ok := ub[ch.Path]; ok {
				e.Ub = PFSideStat{MtimeNs: fi.MtimeNs, Ino: fi.Ino}
			}
		}
		ops = append(ops, PFBaseOp{Path: ch.Path, Entry: e})
	}
	return ops
}

// commit 写 base（同一个动作的多条变更一次 Apply）并记下做成了。
func (r *pfrdRun) commit(i int, a PFAction, ub map[string]PFFileInfo, bytes int64) bool {
	if err := r.base.Apply(r.baseOps(a, ub)); err != nil {
		r.stopHard("base_write", "写 base 失败（%s）：%v", a.Path, err)
		r.setSkip(i, a, a.Dst, "base_write", fmt.Sprintf("动作已做，但写 base 失败：%v", err))
		return false
	}
	if a.Kind == PFActDelete {
		if a.Dst == PFSideMac {
			r.res.Deletes.Mac++
		} else {
			r.res.Deletes.Ubuntu++
		}
	}
	r.recordDone(i, a, bytes)
	return true
}

// ── 主循环 ──────────────────────────────────────────────

func (r *pfrdRun) execute() {
	acts := r.plan.Actions
	r.state = make([]pfrdState, len(acts))
	r.adm = make([]pfrdAdm, len(acts))
	r.admitAll()
	defer r.cancelXfers()
	phase := -1
	for i := range acts {
		a := acts[i]
		if a.Phase != phase {
			r.flush()
			phase = a.Phase
		}
		if r.stopped {
			r.markNotRun(i, a)
			continue
		}
		r.exec(i, a)
	}
	r.flush()
}

func (r *pfrdRun) exec(i int, a PFAction) {
	switch a.Kind {
	case PFActMkdir:
		r.doMkdir(i, a)
	case PFActRmdir:
		r.doRmdir(i, a)
	case PFActCopyNew, PFActCopyOver:
		r.doCopy(i, a)
	case PFActDelete:
		r.doDelete(i, a)
	case PFActRename:
		r.doRename(i, a)
	case PFActClone:
		r.doClone(i, a)
	case PFActConflict:
		r.doConflict(i, a)
	case PFActBaseSet, PFActBaseDel:
		r.commit(i, a, nil, 0)
	case PFActLedgerCopy, PFActBaseLedger:
		r.state[i] = pfrdSkipped
		r.res.Skipped = append(r.res.Skipped, PFRoundSkip{Path: a.Path, Kind: a.Kind, Code: "ledger_todo",
			Reason: "台账同步待做（T10）", Source: PFRoundSrcTodo})
	default:
		r.setSkip(i, a, "", "unknown_action", fmt.Sprintf("不认识的动作种类 %q", a.Kind))
	}
}

// applier 建（或取已建的）Mac 侧写原语执行器；配置错误整个停下。
func (r *pfrdRun) applier() *PFApplier {
	if r.app != nil {
		return r.app
	}
	if r.rec == nil {
		rec, err := NewPFRecycle(r.in.RecycleDir, PFRecycleOptions{Stat: r.in.Stat, Now: r.in.Now})
		if err != nil {
			r.stopHard("config", "Mac 侧同步回收区不可用：%v", err)
			return nil
		}
		r.rec = rec
	}
	app, err := NewPFApplier(PFApplyConfig{Root: r.macRoot, UnitRel: r.in.Unit.Rel, Staging: r.in.Staging, Recycle: r.rec,
		Round: r.in.Round, Now: r.in.Now, Stat: r.in.Stat, LockStale: r.in.LockStale, Hook: r.in.ApplyHook})
	if err != nil {
		r.stopHard("config", "Mac 侧写原语不可用：%v", err)
		return nil
	}
	r.app = app
	return app
}

// macResult 处理一步 Mac 写原语的返回；做成了返回 true。
func (r *pfrdRun) macResult(i int, a PFAction, res PFApplyResult, err error) bool {
	if err != nil {
		r.stopHard("apply_config", "Mac 写原语配置错误（%s）：%v", a.Path, err)
		r.setSkip(i, a, PFSideMac, "config", err.Error())
		return false
	}
	return r.applyResult(i, a, PFSideMac, res)
}

func (r *pfrdRun) applyResult(i int, a PFAction, side PFSide, res PFApplyResult) bool {
	for _, n := range res.Notes {
		r.notice("%s %s：%s", side.Machine(), a.Path, n)
	}
	if res.Status == PFApplyDone {
		return true
	}
	if res.Status == PFApplyAbandoned && res.Uncertain {
		r.markUncertain(side == PFSideUbuntu, "%s 上 %s（%s）：%s", side.Machine(), a.Path, a.Kind, res.Reason)
		r.setSkip(i, a, side, res.Code, fmt.Sprintf("%s，结果不确定：%s", res.Status, res.Reason))
		return false
	}
	r.setSkip(i, a, side, res.Code, fmt.Sprintf("%s：%s", res.Status, res.Reason))
	return false
}

// ── Ubuntu 批 ───────────────────────────────────────────

type pfrdUbItem struct {
	steps []PFRemoteStep
	// done 拿到这一项自己那几步的结果；结果可能比步骤少甚至为空（批中途停了、没发出去），要自己处理。
	done func(rs []PFRemoteStepResult)
}

func (r *pfrdRun) ubQueue(it *pfrdUbItem) {
	if r.stopped {
		it.done(nil)
		return
	}
	r.batch = append(r.batch, it)
	for _, st := range it.steps {
		if st.Op == PFRemoteOpInstallNew || st.Op == PFRemoteOpInstallOver {
			r.batchSHA[st.SHA256] = true
		}
	}
	if len(r.batch) >= pfrdMaxBatchSteps {
		r.flush()
	}
}

// ubOne 排一个只有一步的 Ubuntu 动作；做成了调用 onDone。
func (r *pfrdRun) ubOne(i int, a PFAction, step PFRemoteStep, onDone func(res PFApplyResult)) {
	r.ubQueue(&pfrdUbItem{steps: []PFRemoteStep{step}, done: func(rs []PFRemoteStepResult) {
		if len(rs) == 0 {
			r.markNotRun(i, a)
			return
		}
		if res, ok := r.stepApply(i, a, rs[0]); ok {
			onDone(res)
		}
	}})
}

// stepApply 归类一步 Ubuntu 步骤的结果：做成了返回 (res, true)。
func (r *pfrdRun) stepApply(i int, a PFAction, sr PFRemoteStepResult) (PFApplyResult, bool) {
	if sr.Error != "" {
		r.addError(a.Path, "ubuntu_step", false, "Ubuntu 上这一步没能执行（%s）：%s", a.Path, sr.Error)
		r.setSkip(i, a, PFSideUbuntu, "step_error", sr.Error)
		return PFApplyResult{}, false
	}
	if sr.Apply == nil {
		r.addError(a.Path, "ubuntu_step", true, "Ubuntu 的结果里缺少 %s 的执行结果", a.Path)
		r.setSkip(i, a, PFSideUbuntu, "step_error", "结果里缺少执行结果")
		return PFApplyResult{}, false
	}
	return *sr.Apply, r.applyResult(i, a, PFSideUbuntu, *sr.Apply)
}

// flush 把挂着的 Ubuntu 批发出去并分发结果。
func (r *pfrdRun) flush() {
	if len(r.batch) == 0 {
		return
	}
	items := r.batch
	r.batch, r.batchSHA = nil, map[string]bool{}
	for len(items) > 0 {
		if r.stopped {
			for _, it := range items {
				it.done(nil)
			}
			return
		}
		n, total := 0, 0
		for n < len(items) && (n == 0 || total+len(items[n].steps) <= pfrdMaxBatchSteps) {
			total += len(items[n].steps)
			n++
		}
		chunk := items[:n]
		items = items[n:]
		steps := make([]PFRemoteStep, 0, total)
		for _, it := range chunk {
			steps = append(steps, it.steps...)
		}
		rs, _ := r.sendNow(steps, false)
		cursor := 0
		for _, it := range chunk {
			end := min(cursor+len(it.steps), len(rs))
			var part []PFRemoteStepResult
			if cursor < end {
				part = rs[cursor:end]
			}
			cursor += len(it.steps)
			it.done(part)
		}
	}
}

// sendNow 立刻发一批 Ubuntu 步骤，处理通信层的各种失败；返回已执行的那些步骤的结果。
func (r *pfrdRun) sendNow(steps []PFRemoteStep, stopOnFail bool) ([]PFRemoteStepResult, bool) {
	resp, err := r.in.Remote.Apply(r.ctx, PFRemoteApplyOptions{Unit: r.ubUnit, Round: r.in.Round, StopOnFail: stopOnFail}, steps)
	if err != nil {
		switch {
		case PFRemoteIsUncertain(err):
			r.markUncertain(true, "Ubuntu 通信不确定（这一批可能已经执行了一部分）：%v", err)
		case errors.Is(err, ErrPFRemoteLeaseHeld):
			r.stopHard("lease_held", "Ubuntu 的协调者租约在别的 Mac 手里，停止写入：%v", err)
		default:
			r.stopHard("ubuntu_refused", "Ubuntu 拒绝了这批写入（一步都没执行）：%v", err)
		}
		return nil, false
	}
	if resp.Stopped && resp.StopAt >= 0 && resp.StopAt < len(resp.Results) {
		last := resp.Results[resp.StopAt]
		switch {
		case last.Config:
			r.stopHard("ubuntu_config", "Ubuntu 上出现配置错误，停止写入：%s", last.Error)
		case last.Apply != nil && last.Apply.Status == PFApplyAbandoned && last.Apply.Uncertain:
			r.markUncertain(true, "Ubuntu 上 %s 放弃且结果不确定，停止写入：%s", last.Apply.Rel, last.Apply.Reason)
		}
	}
	return resp.Results, true
}

// ── 各种动作 ────────────────────────────────────────────

func (r *pfrdRun) doMkdir(i int, a PFAction) {
	if a.Path == "" {
		r.doUnitMkdir(i, a)
		return
	}
	if a.Dst == PFSideMac {
		app := r.applier()
		if app == nil {
			r.markNotRun(i, a)
			return
		}
		res, err := app.Mkdir(r.ctx, a.Path)
		if r.macResult(i, a, res, err) {
			r.commit(i, a, nil, 0)
		}
		return
	}
	r.ubOne(i, a, PFRemoteStep{Op: PFRemoteOpMkdir, Path: a.Path}, func(PFApplyResult) { r.commit(i, a, nil, 0) })
}

// doUnitMkdir：建单元根目录本身（首次，一边还没有这个单元）。
func (r *pfrdRun) doUnitMkdir(i int, a PFAction) {
	if a.Dst == PFSideMac {
		if err := os.MkdirAll(r.macRoot, 0o755); err != nil {
			r.stopHard("mac_root", "建 Mac 侧单元目录 %s 失败：%v", r.macRoot, err)
			r.setSkip(i, a, PFSideMac, "mkdir_root", err.Error())
			return
		}
		r.macMissing = false
		r.state[i] = pfrdDone
		r.res.Done = append(r.res.Done, PFRoundDoneItem{Kind: PFActMkdir, Path: "", Side: PFSideMac, Note: "新建单元目录"})
		return
	}
	r.flush()
	rs, ok := r.sendNow([]PFRemoteStep{{Op: PFRemoteOpUnitMkdir}}, true)
	if !ok || len(rs) == 0 {
		r.markNotRun(i, a)
		return
	}
	res, done := r.stepApply(i, a, rs[0])
	if !done {
		r.stopHard("unit_mkdir", "在 Ubuntu 上建单元目录失败")
		return
	}
	r.ubCreated = !res.Existed
	r.ubMissing = false
	r.state[i] = pfrdDone
	r.res.Done = append(r.res.Done, PFRoundDoneItem{Kind: PFActMkdir, Path: "", Side: PFSideUbuntu, Note: "新建单元目录"})
}

func (r *pfrdRun) doDelete(i int, a PFAction) {
	if a.Dst == PFSideMac {
		app := r.applier()
		if app == nil {
			r.markNotRun(i, a)
			return
		}
		res, err := app.Delete(r.ctx, a.Path, *a.DstExpect, PFReasonDelete)
		if r.macResult(i, a, res, err) {
			r.commit(i, a, nil, 0)
		}
		return
	}
	r.ubOne(i, a, PFRemoteStep{Op: PFRemoteOpDelete, Path: a.Path, Expect: a.DstExpect, Reason: string(PFReasonDelete)},
		func(PFApplyResult) { r.commit(i, a, nil, 0) })
}

func (r *pfrdRun) doRename(i int, a PFAction) {
	if a.Dst == PFSideMac {
		app := r.applier()
		if app == nil {
			r.markNotRun(i, a)
			return
		}
		res, err := app.Rename(r.ctx, a.From, a.Path, *a.FromExpect)
		if r.macResult(i, a, res, err) {
			r.commit(i, a, nil, 0)
		}
		return
	}
	r.ubOne(i, a, PFRemoteStep{Op: PFRemoteOpRename, Path: a.From, To: a.Path, Expect: a.FromExpect},
		func(PFApplyResult) { r.commit(i, a, nil, 0) })
}

func (r *pfrdRun) doClone(i int, a PFAction) {
	mtime := max(a.SrcExpect.MtimeNs, 1)
	if a.Dst == PFSideMac {
		app := r.applier()
		if app == nil {
			r.markNotRun(i, a)
			return
		}
		src := PFCopySrc{Path: filepath.Join(r.macRoot, filepath.FromSlash(a.From)), SHA256: a.FromExpect.SHA256, Size: a.FromExpect.Size}
		res, err := app.CopyInstallNew(r.ctx, src, a.Path, mtime)
		if r.macResult(i, a, res, err) {
			r.commit(i, a, nil, 0)
		}
		return
	}
	step := PFRemoteStep{Op: PFRemoteOpCopyNew, Path: a.Path, MtimeNs: mtime,
		Src: &PFRemoteCopySrc{Where: PFRemoteSrcUnit, Path: a.From, SHA256: a.FromExpect.SHA256, Size: a.FromExpect.Size}}
	r.ubOne(i, a, step, func(res PFApplyResult) { r.commit(i, a, pfrdInstalled(a.Path, res), 0) })
}

func pfrdInstalled(rel string, res PFApplyResult) map[string]PFFileInfo {
	if res.Installed == nil {
		return nil
	}
	return map[string]PFFileInfo{rel: *res.Installed}
}

// macSourceChanged：上传前核对 Mac 上的源文件还是判定时看到的那份（大小、mtime、ino）；不是返回原因。
func (r *pfrdRun) macSourceChanged(rel string, exp *PFExpect) string {
	st, ok := r.macStatOf(rel)
	switch {
	case !ok:
		return "Mac 上的源文件不见了或读不了"
	case st.Kind != PFKindFile:
		return "Mac 上的源路径不是普通文件了"
	case st.Size != exp.Size || st.MtimeNs != exp.MtimeNs || (exp.Ino != 0 && st.Ino != exp.Ino):
		return "Mac 上的源文件在判定之后又被改了"
	}
	return ""
}

func (r *pfrdRun) doCopy(i int, a PFAction) {
	adm := &r.adm[i]
	switch {
	case adm.skipCode != "":
		r.setSkip(i, a, "", adm.skipCode, adm.skipWhy)
		return
	case adm.deferred:
		r.recordDeferred(i, a)
		return
	}
	if a.Src == PFSideMac {
		if why := r.macSourceChanged(a.Path, a.SrcExpect); why != "" {
			r.setSkip(i, a, PFSideMac, "source_changed", why+"，本轮不传，下一轮重新判定")
			return
		}
	}
	tr, ok := r.fetch(i, a, adm.jobs[0])
	if !ok {
		return
	}
	mtime := max(a.SrcExpect.MtimeNs, 1)
	sent := tr.SentBytes
	if a.Dst == PFSideMac {
		app := r.applier()
		if app == nil {
			r.markNotRun(i, a)
			return
		}
		part := PFPart{Path: tr.PartPath, SHA256: a.SrcExpect.SHA256, Size: a.SrcExpect.Size}
		var res PFApplyResult
		var err error
		if a.Kind == PFActCopyNew {
			res, err = app.InstallNew(r.ctx, a.Path, part, mtime)
		} else {
			res, err = app.InstallOver(r.ctx, a.Path, part, mtime, *a.DstExpect)
		}
		if r.macResult(i, a, res, err) {
			r.commit(i, a, nil, sent)
		}
		return
	}
	step := PFRemoteStep{Op: PFRemoteOpInstallNew, Path: a.Path, SHA256: a.SrcExpect.SHA256, Size: a.SrcExpect.Size, MtimeNs: mtime}
	if a.Kind == PFActCopyOver {
		step.Op, step.Expect = PFRemoteOpInstallOver, a.DstExpect
	}
	r.ubOne(i, a, step, func(res PFApplyResult) { r.commit(i, a, pfrdInstalled(a.Path, res), sent) })
}

// doConflict：冲突（§3.4）。先把两份内容都搬到对方的暂存区（不动任何文件），都到了再动手：
//
//	落败边：原名文件改名为冲突名 → 在原名处装胜出版本；胜出边：在冲突名处装落败版本。
//
// 中途失败每一步都不丢数据，自己会在下一轮收尾（落败边只剩冲突名文件 → 下一轮当「一边改一边删」改的赢；
// 胜出边还没有冲突副本 → 下一轮当落败边的新文件传回来）；base 只在三步都成功后才写。
func (r *pfrdRun) doConflict(i int, a PFAction) {
	adm := &r.adm[i]
	switch {
	case adm.skipCode != "":
		r.setSkip(i, a, "", adm.skipCode, adm.skipWhy)
		return
	case adm.deferred:
		r.recordDeferred(i, a)
		return
	}
	cs := a.Conflict
	cname := path.Base(cs.ConflictPath)
	for _, s := range []struct {
		side PFSide
		exp  *PFExpect
	}{{a.Src, a.SrcExpect}, {a.Dst, a.DstExpect}} {
		if s.side == PFSideMac {
			if why := r.macSourceChanged(a.Path, s.exp); why != "" {
				r.setSkip(i, a, PFSideMac, "source_changed", why+"，本轮不处理这个冲突，下一轮重新判定")
				return
			}
		}
	}
	trW, ok := r.fetch(i, a, adm.jobs[0]) // 胜出版本 → 落败边
	if !ok {
		return
	}
	trL, ok := r.fetch(i, a, adm.jobs[1]) // 落败版本 → 胜出边
	if !ok {
		return
	}
	sent := trW.SentBytes + trL.SentBytes
	mtimeW, mtimeL := max(a.SrcExpect.MtimeNs, 1), max(a.DstExpect.MtimeNs, 1)
	ub := map[string]PFFileInfo{}
	r.flush()
	if r.stopped {
		r.markNotRun(i, a)
		return
	}

	if cs.Loser == PFSideMac {
		app := r.applier()
		if app == nil {
			r.markNotRun(i, a)
			return
		}
		res, err := app.ConflictRename(r.ctx, a.Path, cname, *a.DstExpect)
		if !r.macResult(i, a, res, err) {
			return
		}
		res, err = app.InstallNew(r.ctx, a.Path, PFPart{Path: trW.PartPath, SHA256: a.SrcExpect.SHA256, Size: a.SrcExpect.Size}, mtimeW)
		if !r.macResult(i, a, res, err) {
			r.addError(a.Path, "conflict_partial", false, "冲突 %s：原名文件已改名为冲突副本，但装胜出版本失败，下一轮会补上", a.Path)
			return
		}
		step := PFRemoteStep{Op: PFRemoteOpInstallNew, Path: cs.ConflictPath, SHA256: a.DstExpect.SHA256, Size: a.DstExpect.Size, MtimeNs: mtimeL}
		rs, sok := r.sendNow([]PFRemoteStep{step}, true)
		if !sok || len(rs) == 0 {
			r.addError(a.Path, "conflict_partial", false, "冲突 %s：Mac 侧已处理，Ubuntu 侧装冲突副本没做成，下一轮会补上", a.Path)
			r.markNotRun(i, a)
			return
		}
		ures, uok := r.stepApply(i, a, rs[0])
		if !uok {
			r.addError(a.Path, "conflict_partial", false, "冲突 %s：Mac 侧已处理，Ubuntu 侧装冲突副本没做成，下一轮会补上", a.Path)
			return
		}
		if ures.Installed != nil {
			ub[cs.ConflictPath] = *ures.Installed
		}
	} else {
		steps := []PFRemoteStep{
			{Op: PFRemoteOpConflictName, Path: a.Path, To: cname, Expect: a.DstExpect},
			{Op: PFRemoteOpInstallNew, Path: a.Path, SHA256: a.SrcExpect.SHA256, Size: a.SrcExpect.Size, MtimeNs: mtimeW},
		}
		rs, sok := r.sendNow(steps, true)
		if !sok || len(rs) == 0 {
			r.markNotRun(i, a)
			return
		}
		for k := range rs {
			ures, uok := r.stepApply(i, a, rs[k])
			if !uok {
				if k == 1 {
					r.addError(a.Path, "conflict_partial", false, "冲突 %s：Ubuntu 上原名文件已改名为冲突副本，但装胜出版本失败，下一轮会补上", a.Path)
				}
				return
			}
			if k == 1 && ures.Installed != nil {
				ub[a.Path] = *ures.Installed
			}
		}
		if len(rs) < 2 {
			r.setSkip(i, a, PFSideUbuntu, "conflict_partial", "Ubuntu 上只做了一半，下一轮会补上")
			return
		}
		app := r.applier()
		if app == nil {
			r.markNotRun(i, a)
			return
		}
		res, err := app.InstallNew(r.ctx, cs.ConflictPath, PFPart{Path: trL.PartPath, SHA256: a.DstExpect.SHA256, Size: a.DstExpect.Size}, mtimeL)
		if !r.macResult(i, a, res, err) {
			r.addError(a.Path, "conflict_partial", false, "冲突 %s：Ubuntu 侧已处理，Mac 侧装冲突副本没做成，下一轮会补上", a.Path)
			return
		}
	}
	if r.commit(i, a, ub, sent) {
		r.res.NewConflicts = append(r.res.NewConflicts, a.Path)
		r.res.Conflicts = append(r.res.Conflicts, PFRemoteConflict{Path: a.Path, ConflictPath: cs.ConflictPath,
			Winner: string(cs.Winner), At: r.now().Format("2006-01-02T15:04:05Z07:00")})
	}
}

// doRmdir：删空目录。目录里只剩系统垃圾文件（.DS_Store 等）时先把它们移进同步回收再删；还有别的就保留并报告。
func (r *pfrdRun) doRmdir(i int, a PFAction) {
	if a.Dst == PFSideMac {
		app := r.applier()
		if app == nil {
			r.markNotRun(i, a)
			return
		}
		res, err := app.Rmdir(r.ctx, a.Path)
		if err == nil && res.Status == PFApplyAbandoned && res.Code == PFApplyCodeNotEmpty {
			jr, moved, _ := pfrdJunkClearDir(r.ctx, app, a.Path)
			switch {
			case jr.Status == PFApplyDone:
				if len(moved) > 0 {
					r.notice("Mac 上目录 %s 里只剩系统垃圾文件（%s），已移进同步回收", a.Path, strings.Join(moved, "、"))
				}
				res, err = app.Rmdir(r.ctx, a.Path)
			case jr.Code == PFApplyCodeNotEmpty:
				r.keepDir(i, a, PFSideMac, jr.Reason)
				return
			default:
				res = jr
			}
		}
		if r.macResult(i, a, res, err) {
			r.commit(i, a, nil, 0)
		}
		return
	}
	r.ubQueue(&pfrdUbItem{steps: []PFRemoteStep{{Op: PFRemoteOpRmdir, Path: a.Path}}, done: func(rs []PFRemoteStepResult) {
		if len(rs) == 0 {
			r.markNotRun(i, a)
			return
		}
		sr := rs[0]
		if sr.Error == "" && sr.Apply != nil && sr.Apply.Status == PFApplyAbandoned && sr.Apply.Code == PFApplyCodeNotEmpty && !r.stopped {
			r.ubJunkThenRmdir(i, a)
			return
		}
		if _, ok := r.stepApply(i, a, sr); ok {
			r.commit(i, a, nil, 0)
		}
	}})
}

func (r *pfrdRun) ubJunkThenRmdir(i int, a PFAction) {
	rs, ok := r.sendNow([]PFRemoteStep{{Op: pfrdOpJunkClear, Path: a.Path}}, true)
	if !ok || len(rs) == 0 {
		r.markNotRun(i, a)
		return
	}
	sr := rs[0]
	if sr.Error == "" && sr.Apply != nil && sr.Apply.Status == PFApplyAbandoned && sr.Apply.Code == PFApplyCodeNotEmpty {
		r.keepDir(i, a, PFSideUbuntu, sr.Apply.Reason)
		return
	}
	jr, jok := r.stepApply(i, a, sr)
	if !jok {
		return
	}
	if moved := pfrdJunkMoved(sr.Ext); len(moved) > 0 {
		r.notice("Ubuntu 上目录 %s 里只剩系统垃圾文件（%s），已移进同步回收", a.Path, strings.Join(moved, "、"))
	}
	_ = jr
	rs, ok = r.sendNow([]PFRemoteStep{{Op: PFRemoteOpRmdir, Path: a.Path}}, true)
	if !ok || len(rs) == 0 {
		r.markNotRun(i, a)
		return
	}
	if _, ok := r.stepApply(i, a, rs[0]); ok {
		r.commit(i, a, nil, 0)
	}
}

// keepDir：目录里还有别的东西，保留（不是错误），base 不动。
func (r *pfrdRun) keepDir(i int, a PFAction, side PFSide, why string) {
	r.state[i] = pfrdSkipped
	r.res.Skipped = append(r.res.Skipped, PFRoundSkip{Path: a.Path, Kind: a.Kind, Side: side, Code: PFSkipDirKept,
		Reason: "另一边删了这个目录，但这边的目录里还有没同步的东西，保留：" + why, Source: PFRoundSrcApply})
}
