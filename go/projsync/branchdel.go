package projsync

// DeleteBranch：分支删除的写原语（用户 11/04 定「分支删除也同步」，PLAN-sync-v2.md 第十九节）。
// 与 apply.go 的其它原语同一套规矩：先重新读现场再写，任何读取失败、前提不符都返回带中文原因的 error 且不写。
//
//   - 只删引用：`update-ref --no-deref -d refs/heads/<分支> <旧OID>`（带旧值的比较交换），不碰文件/索引/HEAD；
//   - main、master 拒绝（判定层已不会产生，这里再挡一次）；
//   - 现场引用不存在或不等于旧值 → ErrRefsChanged（被并发改过，需重新判定），不写；
//   - 任何工作区（含 linked、unborn）HEAD 指向该分支 → 拒绝（ErrRefsChanged：判定时没检出、现在检出了）；
//   - 写后复读：引用必须已不存在；再复查一遍没有工作区检出它。

import (
	"context"
	"fmt"
)

// DeleteBranch 删除 refs/heads/<Branch>，前提是它当前恰好等于 OldOID。
func DeleteBranch(ctx context.Context, p ApplyPlan) error {
	if err := p.validateCommon(ctx, KindDeleteOnOther); err != nil {
		return err
	}
	if isProtectedBranch(p.Branch) {
		return fmt.Errorf("%s：受保护分支，永远不自动删除", p.tag())
	}
	if !oidRe.MatchString(p.OldOID) || p.NewOID != "" {
		return fmt.Errorf("%s：删除需要合法的旧值、且不能带目标值，停止", p.tag())
	}
	if err := p.checkScope(ctx); err != nil {
		return err
	}
	ref := "refs/heads/" + p.Branch
	if err := p.requireDirectRef(ctx, ref); err != nil {
		return err
	}
	cur, ex, err := readRef(ctx, p.RepoDir, p.Env, ref)
	if err != nil {
		return fmt.Errorf("%s：读取失败，停止: %w", p.tag(), err)
	}
	if !ex || cur != p.OldOID {
		return fmt.Errorf("%s：%w（当前 %q，预期 %s），不删除，需重新判定", p.tag(), ErrRefsChanged, cur, p.OldOID)
	}
	wts, err := listWorktrees(ctx, p.RepoDir, p.Env)
	if err != nil {
		return fmt.Errorf("%s：%w，停止", p.tag(), err)
	}
	if hs := checkedOutBy(wts, p.Branch); len(hs) > 0 {
		return fmt.Errorf("%s：%w：工作区 %s 正检出该分支，不删除，需重新判定", p.tag(), ErrRefsChanged, hs[0].Path)
	}
	if err := p.requireAllIdle(ctx, wts); err != nil {
		return err
	}
	if applyTestHook != nil {
		applyTestHook("beforeUpdateRef")
	}
	// 检查与写入之间不是原子的：旧值由 update-ref 自己比较交换；检出变化只能事后复查发现（调度层对同一仓库串行）。
	if _, err := p.writeGit(ctx, p.RepoDir, false, nil, "update-ref", "--no-deref", "-d", ref, p.OldOID); err != nil {
		return fmt.Errorf("%s：删除引用失败（引用可能已变化），需重新判定: %w", p.tag(), err)
	}
	if got, ex, err := readRef(ctx, p.RepoDir, p.Env, ref); err != nil || ex {
		return fmt.Errorf("%s：删除后复读引用不符（%q，%v），需人工确认", p.tag(), got, err)
	}
	return p.postCheckNotCheckedOut(ctx)
}
