// 分工：DecideWorktrees 按工作区判定「要不要派 agent 提交未提交改动」，与分支要不要更新无关；
// DecideBranches 判定每个分支的更新动作，但不负责派提交：被 dirty 挡住时只返回
// KindWaitDirty（静止不足）或 KindBlockedDirty（已静止够，等工作区判定派活）；
// KindNeedCommit 只由 DecideWorktrees 产生。
//
// 提交派活统一以 WorktreeAction 为入口（执行器按 设备+仓库+规范化路径 去重，并跟踪在途任务）。
// 派活后相关事实全部失效，执行器必须重新采集后再执行分支动作。
// BranchAction 不是完整执行凭据：执行上下文要另外带两边旧 OID、目标 OID、工作区映射、事实版本。
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
	KindBlockedDirty  ActionKind = "blocked_dirty"   // 同上但已静止足够：等工作区判定派提交，本轮分支不更新
	KindNeedCommit    ActionKind = "need_commit"     // 仅由 DecideWorktrees 产生：交该设备的 agent 提交
	KindInvalidFacts  ActionKind = "invalid_facts"   // 事实矛盾或不安全，不更新任何一边，需重新采集
	KindSkipRepoBusy  ActionKind = "skip_repo_busy"  // 仓库正在 rebase/merge 等，整个项目本轮跳过
)

// BranchFacts 是某个分支已收集好的事实（本包纯函数不收集事实）。
type BranchFacts struct {
	Name string
	// 两边该分支的提交哈希；空串表示这一边没有该分支。
	Mac, Ubuntu string
	// 仅当两边都存在且哈希不同时才有意义；两边相同或有一边为空时被忽略。
	// 两个标志同时为 true 且哈希不同是矛盾输入，判 KindInvalidFacts。
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
	// Sides 是要更新的一边（AutoMerge 为两边）；WaitDirty/BlockedDirty 时表示
	// 被未提交改动挡住的设备。分支侧不负责派提交（由 DecideWorktrees 负责）。
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
		return createOnOther(act, b.Mac, SideUbuntu, b.UbuntuCheckedOut)
	case b.Mac == "":
		return createOnOther(act, b.Ubuntu, SideMac, b.MacCheckedOut)
	case b.MacIsAncestorOfUbuntu && b.UbuntuIsAncestorOfMac:
		act.Kind = KindInvalidFacts
		act.Reason = "事实矛盾（两边提交不同却互为祖先），需重新采集，不更新任何一边"
	case b.MacIsAncestorOfUbuntu:
		// Mac 落后，更新 Mac。
		return gateDirty(act, KindFastForward, b.Ubuntu, []Side{SideMac}, b,
			"Ubuntu 领先，Mac 快进到 Ubuntu 的提交")
	case b.UbuntuIsAncestorOfMac:
		return gateDirty(act, KindFastForward, b.Mac, []Side{SideUbuntu}, b,
			"Mac 领先，Ubuntu 快进到 Mac 的提交")
	case b.MergeClean && len(b.ConflictFiles) > 0:
		act.Kind = KindInvalidFacts
		act.Reason = "事实矛盾（试合并标为干净却列出冲突文件），需重新采集，不更新任何一边"
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

// createOnOther 在 target 侧建同名分支。只允许创建「未被任何工作区 HEAD 指向」的引用：
// 目标侧若有工作区检出该名字（HEAD 指向不存在的分支，如 unborn 或引用被外部删除），
// 创建引用会悄悄改变那个工作区的状态，因此拒绝。执行器只建引用，不碰文件/索引/HEAD。
func createOnOther(act BranchAction, commit string, target Side, targetCheckedOut bool) BranchAction {
	if targetCheckedOut {
		act.Kind = KindInvalidFacts
		act.Reason = fmt.Sprintf("%s侧有工作区指向不存在的分支，需重新采集并人工确认，不创建", sideNames([]Side{target}))
		return act
	}
	act.Kind, act.Target, act.Sides = KindCreateOnOther, commit, []Side{target}
	act.Reason = fmt.Sprintf("只有%s有该分支，在%s建同名分支", sideNames([]Side{otherSide(target)}), sideNames([]Side{target}))
	return act
}

func otherSide(s Side) Side {
	if s == SideMac {
		return SideUbuntu
	}
	return SideMac
}

// gateDirty 检查要更新的各边是否因被检出且 dirty 而不能更新。
func gateDirty(act BranchAction, kind ActionKind, target string, update []Side, b BranchFacts, okReason string) BranchAction {
	var blocked []Side
	var waits []string
	for _, side := range update {
		dirty, quiet, co := b.MacDirty, b.MacQuietMinutes, b.MacCheckedOut
		if side == SideUbuntu {
			dirty, quiet, co = b.UbuntuDirty, b.UbuntuQuietMinutes, b.UbuntuCheckedOut
		}
		if !co || !dirty {
			continue
		}
		blocked = append(blocked, side)
		if quiet < QuietMinutesForAutoCommit {
			waits = append(waits, fmt.Sprintf("%s尚需等待约 %d 分钟", sideNames([]Side{side}), QuietMinutesForAutoCommit-quiet))
		}
	}
	if len(blocked) == 0 {
		act.Kind, act.Target, act.Sides, act.Reason = kind, target, update, okReason
		return act
	}
	act.Sides = blocked
	if len(waits) > 0 {
		act.Kind = KindWaitDirty
		act.Reason = fmt.Sprintf("%s有未提交改动；%s，%s", sideNames(blocked), strings.Join(waits, "；"), notUpdatedText(kind))
		return act
	}
	act.Kind = KindBlockedDirty
	act.Reason = fmt.Sprintf("%s有未提交改动且已静止 ≥%d 分钟；等待该设备提交未提交改动（由工作区判定派活），%s",
		sideNames(blocked), QuietMinutesForAutoCommit, notUpdatedText(kind))
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

func notUpdatedText(kind ActionKind) string {
	if kind == KindAutoMerge {
		return "本分支两边暂不更新"
	}
	return "本分支暂不更新"
}

// WorktreeFacts 是某台设备上一个工作区的事实（一个仓库在一台设备上可有多个工作区）。
type WorktreeFacts struct {
	Side         Side
	Path         string
	Branch       string
	Dirty        bool
	QuietMinutes int // 未提交改动连续多少分钟没变化；不 dirty 时忽略
}

// WorktreeAction 是对一个工作区的判定。Kind 只会是
// KindNothing / KindWaitDirty / KindNeedCommit / KindSkipRepoBusy。
type WorktreeAction struct {
	Side   Side
	Path   string
	Branch string
	Kind   ActionKind
	Reason string
}

// DecideWorktrees 是纯函数：busy 则全部跳过；不 dirty 无事；dirty 且静止不足
// QuietMinutesForAutoCommit 则等待；否则交该设备的 agent 提交。输出按 Side、Path 排序。
func DecideWorktrees(repo RepoFacts, wts []WorktreeFacts) []WorktreeAction {
	sorted := make([]WorktreeFacts, len(wts))
	copy(sorted, wts)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Side != sorted[j].Side {
			return sorted[i].Side < sorted[j].Side
		}
		return sorted[i].Path < sorted[j].Path
	})
	out := make([]WorktreeAction, 0, len(sorted))
	for _, w := range sorted {
		a := WorktreeAction{Side: w.Side, Path: w.Path, Branch: w.Branch}
		switch {
		case repo.MacBusy || repo.UbuntuBusy:
			a.Kind = KindSkipRepoBusy
			a.Reason = "仓库正在 rebase/merge/cherry-pick 中，本轮跳过：" + busyReason(repo)
		case !w.Dirty:
			a.Kind = KindNothing
			a.Reason = "工作区没有未提交改动"
		case w.QuietMinutes < QuietMinutesForAutoCommit:
			a.Kind = KindWaitDirty
			a.Reason = fmt.Sprintf("%s工作区 %s（分支 %s）有未提交改动，静止不足 %d 分钟，还需等待约 %d 分钟",
				sideNames([]Side{w.Side}), w.Path, w.Branch, QuietMinutesForAutoCommit, QuietMinutesForAutoCommit-w.QuietMinutes)
		default:
			a.Kind = KindNeedCommit
			a.Reason = fmt.Sprintf("%s工作区 %s（分支 %s）有未提交改动且已静止 ≥%d 分钟，请该设备上指定的 agent 提交",
				sideNames([]Side{w.Side}), w.Path, w.Branch, QuietMinutesForAutoCommit)
		}
		out = append(out, a)
	}
	return out
}
