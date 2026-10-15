package projsync

import (
	"fmt"
	"sort"
	"strings"
)

// QuietMinutesForAutoCommit 是允许交给 agent 自动提交未提交改动前，
// 工作区改动必须连续不变的分钟数。来源：用户 10/15 在
// PLAN-sync-v2.md 第六节第 3 条的决定（至少 20 分钟没有变动）。
const QuietMinutesForAutoCommit = 20

// Side 表示两台设备之一。
type Side string

const (
	SideMac    Side = "mac"
	SideUbuntu Side = "ubuntu"
)

// ActionKind 是对一个分支要做的动作类型。
type ActionKind string

const (
	KindNothing       ActionKind = "nothing"         // 两边相同
	KindCreateOnOther ActionKind = "create_on_other" // 只有一边有，在另一边建同名分支
	KindFastForward   ActionKind = "fast_forward"    // 落后的一边快进到前面的提交
	KindAutoMerge     ActionKind = "auto_merge"      // 分叉且可干净合并，生成合并提交后两边都更新
	KindConflict      ActionKind = "conflict"        // 分叉且有冲突，交用户手动处理，不更新任何一边
	KindWaitDirty     ActionKind = "wait_dirty"      // 需更新的一边工作区 dirty 且静止不足，本轮不更新
	KindNeedCommit    ActionKind = "need_commit"     // 同上但已静止足够，交该设备的 agent 提交
	KindSkipRepoBusy  ActionKind = "skip_repo_busy"  // 仓库正在 rebase/merge 等，整个项目本轮跳过
)

// BranchFacts 是某个分支已收集好的事实（本包纯函数不收集事实）。
type BranchFacts struct {
	Name string
	// 两边该分支的提交哈希；空串表示这一边没有该分支。
	Mac, Ubuntu string
	// 两边都存在且不同时才有意义。
	MacIsAncestorOfUbuntu bool
	UbuntuIsAncestorOfMac bool
	// 分叉时 git merge-tree --write-tree 试合并是否无冲突；ConflictFiles 为冲突文件。
	MergeClean    bool
	ConflictFiles []string
	// 该分支是否在那一边被某个工作区检出。
	MacCheckedOut, UbuntuCheckedOut bool
	// 若被检出，该工作区是否有未提交改动（已跟踪改动或按 .gitignore 过滤后的未跟踪文件）。
	MacDirty, UbuntuDirty bool
	// 未提交改动已连续多少分钟没有变化（不 dirty 时忽略）。
	MacQuietMinutes, UbuntuQuietMinutes int
}

// RepoFacts 是仓库级事实。Busy 表示该设备上仓库正在 rebase/merge/cherry-pick 等。
type RepoFacts struct {
	MacBusy, UbuntuBusy             bool
	MacBusyReason, UbuntuBusyReason string
}

// BranchAction 是对一个分支的判定结果。
type BranchAction struct {
	Name string
	Kind ActionKind
	// Target 是要更新到的提交哈希；AutoMerge 时为空，由执行器生成合并提交后填。
	Target string
	// Sides 是要更新的一边（AutoMerge 为两边）；WaitDirty/NeedCommit 时是
	// 「被 dirty 阻塞的那些边」（NeedCommit 时即需要 agent 提交的设备）。
	Sides  []Side
	Reason string
}

// DecideBranches 是纯函数：不调用 git、不读文件、不联网，只依据已收集的事实
// 给出每个分支的动作，按分支名排序，结果确定。
//
// 安全约束：本函数从不产生强制更新；FastForward 和 AutoMerge 的语义保证任何
// 更新都只到现有提交的后代。但执行器不得盲信本函数的输入，执行前必须再次校验
// （例如 git update-ref 带旧值、merge --ff-only、确认目标是当前值的后代），
// 校验失败则拒绝更新并记日志。
func DecideBranches(repo RepoFacts, branches []BranchFacts) []BranchAction {
	sorted := make([]BranchFacts, len(branches))
	copy(sorted, branches)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	out := make([]BranchAction, 0, len(sorted))
	for _, b := range sorted {
		out = append(out, decideOne(repo, b))
	}
	return out
}

func busyReason(repo RepoFacts) string {
	var parts []string
	if repo.MacBusy {
		parts = append(parts, "Mac："+orUnknown(repo.MacBusyReason))
	}
	if repo.UbuntuBusy {
		parts = append(parts, "Ubuntu："+orUnknown(repo.UbuntuBusyReason))
	}
	return strings.Join(parts, "；")
}

func orUnknown(s string) string {
	if s == "" {
		return "进行中（原因未知）"
	}
	return s
}

func decideOne(repo RepoFacts, b BranchFacts) BranchAction {
	act := BranchAction{Name: b.Name}

	if repo.MacBusy || repo.UbuntuBusy {
		act.Kind = KindSkipRepoBusy
		act.Reason = "仓库正在 rebase/merge/cherry-pick 中，本轮跳过：" + busyReason(repo)
		return act
	}

	switch {
	case b.Mac == "" && b.Ubuntu == "":
		act.Kind = KindNothing
		act.Reason = "两边都没有该分支"
	case b.Mac == b.Ubuntu:
		act.Kind = KindNothing
		act.Reason = "两边相同"
	case b.Ubuntu == "":
		act.Kind, act.Target, act.Sides = KindCreateOnOther, b.Mac, []Side{SideUbuntu}
		act.Reason = "只有 Mac 有该分支，在 Ubuntu 建同名分支"
	case b.Mac == "":
		act.Kind, act.Target, act.Sides = KindCreateOnOther, b.Ubuntu, []Side{SideMac}
		act.Reason = "只有 Ubuntu 有该分支，在 Mac 建同名分支"
	case b.MacIsAncestorOfUbuntu:
		// Mac 落后，更新 Mac。
		return gateDirty(act, KindFastForward, b.Ubuntu, []Side{SideMac}, b,
			"Ubuntu 领先，Mac 快进到 Ubuntu 的提交")
	case b.UbuntuIsAncestorOfMac:
		return gateDirty(act, KindFastForward, b.Mac, []Side{SideUbuntu}, b,
			"Mac 领先，Ubuntu 快进到 Mac 的提交")
	case b.MergeClean:
		return gateDirty(act, KindAutoMerge, "", []Side{SideMac, SideUbuntu}, b,
			"两边分叉但可干净合并，生成合并提交后两边都更新")
	default:
		act.Kind = KindConflict
		if len(b.ConflictFiles) > 0 {
			act.Reason = "两边分叉且合并有冲突，不更新任何一边，请在控制中心手动处理；冲突文件：" +
				strings.Join(b.ConflictFiles, "、")
		} else {
			act.Reason = "两边分叉且合并有冲突（未取得冲突文件列表），不更新任何一边，请在控制中心手动处理"
		}
	}
	return act
}

// gateDirty 检查要更新的各边是否因被检出且 dirty 而不能更新。
func gateDirty(act BranchAction, kind ActionKind, target string, update []Side, b BranchFacts, okReason string) BranchAction {
	var blocked []Side
	maxWait := 0
	allQuiet := true
	for _, s := range update {
		dirty, quiet, co := b.MacDirty, b.MacQuietMinutes, b.MacCheckedOut
		if s == SideUbuntu {
			dirty, quiet, co = b.UbuntuDirty, b.UbuntuQuietMinutes, b.UbuntuCheckedOut
		}
		if !co || !dirty {
			continue
		}
		blocked = append(blocked, s)
		if quiet < QuietMinutesForAutoCommit {
			allQuiet = false
			if w := QuietMinutesForAutoCommit - quiet; w > maxWait {
				maxWait = w
			}
		}
	}
	if len(blocked) == 0 {
		act.Kind, act.Target, act.Sides, act.Reason = kind, target, update, okReason
		return act
	}
	act.Sides = blocked
	if !allQuiet {
		act.Kind = KindWaitDirty
		act.Reason = fmt.Sprintf("%s工作区有未提交改动且静止不足 %d 分钟，本轮不更新（两边都不动），还需等待约 %d 分钟",
			sideNames(blocked), QuietMinutesForAutoCommit, maxWait)
		return act
	}
	act.Kind = KindNeedCommit
	act.Reason = fmt.Sprintf("%s工作区有未提交改动且已静止 ≥%d 分钟，本轮不更新（两边都不动），请该设备上指定的 agent 在仓库中提交",
		sideNames(blocked), QuietMinutesForAutoCommit)
	return act
}

func sideNames(ss []Side) string {
	names := make([]string, len(ss))
	for i, s := range ss {
		if s == SideMac {
			names[i] = "Mac"
		} else {
			names[i] = "Ubuntu"
		}
	}
	return strings.Join(names, "、")
}
