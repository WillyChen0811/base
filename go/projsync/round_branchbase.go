package projsync

// 一轮里与分支对账基线、分支删除有关的编排（用户 11/04 定，PLAN-sync-v2.md 第十九节）。
//
// 基线什么时候变：
//   - 两边相同（KindNothing）→ 记成该提交（首次运行没有基线文件时，这一步就是初始化：只有一边有的分支照规则 1 新建）；
//   - 快进、自动合并（两边都已更新）、新建成功 → 记成新的一致值；
//   - 删除成功 → 清掉；两边都没有该分支（不在本轮采集的分支名并集里）→ 清掉（规则 6）；
//   - 冲突、跳过、失败、结果不确定 → 不动（下一轮重新判定）。
// 本轮所有变化在判定执行结束时一次写盘。中途崩溃没写盘时，基线停在「更早的一致值」：
// 只会让下一轮把某些分支当成「在基线之后有新提交」而新建，不会因此多删（删除要求仍存在的一边恰好等于基线）。

import (
	"context"
	"fmt"
	"time"
)

// loadBase 加载对账基线。读不了（权限/IO）则本轮结束、不写任何分支。
func (r *roundRun) loadBase() bool {
	b, notes, err := loadBranchBase(r.env.BaseDir, r.env.Project, r.repoKey, r.env.RemoteRepo, r.now())
	if err != nil {
		r.res.Error = "读取分支对账基线失败，本轮不更新任何分支: " + err.Error()
		r.att(AttentionItem{Kind: AttApplyFailed, Message: "分支对账基线读不了，本轮没有更新任何分支，请检查", Detail: err.Error()})
		return false
	}
	r.base = b
	r.res.Notes = append(r.res.Notes, notes...)
	return true
}

func (r *roundRun) now() time.Time {
	if r.env.Now != nil {
		return r.env.Now()
	}
	return time.Now()
}

// baseAgree 记下两边相同的分支。
func (r *roundRun) baseAgree(bf BranchFacts) {
	if bf.Mac != "" && bf.Mac == bf.Ubuntu {
		r.base.set(bf.Name, bf.Mac)
	}
}

// finishBase 清掉两边都没有的分支的基线，并写盘。写不下只报告（下一轮仍用旧基线，见文件头）。
func (r *roundRun) finishBase(facts map[string]BranchFacts) {
	if r.base == nil {
		return
	}
	for _, n := range r.base.names() {
		if _, ok := facts[n]; !ok {
			r.base.del(n)
		}
	}
	if err := r.base.save(r.now()); err != nil {
		r.att(AttentionItem{Kind: AttApplyFailed, Message: "分支对账基线保存失败，本轮的基线变化没有记下", Detail: err.Error()})
	}
}

// deleteBlocked：一边已删，另一边受保护或正被检出，不删，交用户处理。
func (r *roundRun) deleteBlocked(act BranchAction) {
	r.addBranch(act, ResultSkipped, act.Reason)
	side := act.Sides[0]
	r.att(AttentionItem{Kind: AttDeleteBlocked, Side: side, Branch: act.Name,
		Message: fmt.Sprintf("分支 %s：%s", act.Name, act.Reason)})
}

// execDelete 在仍存在的一边带旧值删除分支。删除前后各往删除记录追加一行（原提交号，方便找回）；
// 删除前那一行写不下就不删。
func (r *roundRun) execDelete(ctx context.Context, act BranchAction, bf BranchFacts) {
	if r.skipStopped(act) {
		return
	}
	side := act.Sides[0]
	old := bf.Mac
	if side == SideUbuntu {
		old = bf.Ubuntu
	}
	who := sideNames([]Side{side})
	howTo := fmt.Sprintf("找回：在%s执行 git branch %s %s", who, act.Name, old)
	rec := branchDeleteRecord{At: r.now(), Branch: act.Name, Side: side, OID: old, State: "deleting"}
	if err := r.base.logDelete(rec); err != nil {
		r.res.Branches = append(r.res.Branches, BranchOutcome{Name: act.Name, Action: act.Kind, Result: ResultFailed,
			Sides: act.Sides, Deleted: old, Detail: "删除记录写不下，未删除: " + err.Error()})
		r.att(AttentionItem{Kind: AttApplyFailed, Side: side, Branch: act.Name,
			Message: fmt.Sprintf("分支 %s 应在%s删除，但删除记录写不下，未删除", act.Name, who), Detail: err.Error()})
		return
	}
	o := r.applyOn(ctx, side, ApplyPlan{Branch: act.Name, OldOID: old, Kind: KindDeleteOnOther})
	rec.At = r.now()
	switch o.state {
	case applyOK:
		rec.State = "deleted"
	case applyUncertain:
		rec.State, rec.Detail = "uncertain", o.detail
		o.detail = fmt.Sprintf("原提交 %s（%s）；%s", old, howTo, o.detail)
	default:
		rec.State, rec.Detail = "failed", o.detail
	}
	logErr := r.base.logDelete(rec)
	n := len(r.res.Branches)
	if r.finishApply(act, side, o) {
		r.wrote(fmt.Sprintf("分支 %s 在%s已删除（原提交 %s）", act.Name, who, short(old)))
		r.res.Branches = append(r.res.Branches, BranchOutcome{Name: act.Name, Action: act.Kind, Result: ResultDone,
			Sides: act.Sides, Deleted: old, Detail: act.Reason + "；" + howTo})
		r.res.Notes = append(r.res.Notes, fmt.Sprintf("已在%s删除分支 %s，原提交 %s（%s）", who, act.Name, old, howTo))
		r.base.del(act.Name)
	}
	for i := n; i < len(r.res.Branches); i++ {
		r.res.Branches[i].Deleted = old
	}
	if logErr != nil {
		r.res.Notes = append(r.res.Notes, fmt.Sprintf("分支 %s 删除结果没能写进删除记录（%v）；原提交 %s", act.Name, logErr, old))
	}
}
