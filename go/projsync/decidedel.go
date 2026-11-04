package projsync

// 只有一边有的分支怎么判（用户 11/04 定，PLAN-sync-v2.md 第十九节）。纯函数，基线由调用方填进 BranchFacts.Base。
//
//  1. 没有基线（从来没同步过）→ 照旧，在另一边新建。
//  2. 有基线，且仍存在的那边等于基线 → 另一边删了，这边也删（不管有没有合进 main）。
//  3. 有基线，但仍存在的那边在基线之后有新提交（不等于基线）→ 旧的删了、新的照样建：当新分支，在删掉的一边新建。
//  4. 要删的那边该分支正被某个工作区检出 → 不删、不切分支，交用户处理（KindDeleteBlocked）。
//  5. main、master 永远不自动删除：遇到情况 2 也只交用户处理（KindDeleteBlocked）。
//  6. 两边都没有 → 清掉基线（调度层做，见 round_branchbase.go）。
//
// 另：删掉的那一边若有工作区 HEAD 仍指向这个（已不存在的）分支名，和建分支时一样判 KindInvalidFacts，不写。

import "fmt"

// protectedBranches 永远不自动删除。
var protectedBranches = map[string]bool{"main": true, "master": true}

func isProtectedBranch(name string) bool { return protectedBranches[name] }

// decideOneSided 处理只有 has 一边有该分支的情况。
func decideOneSided(act BranchAction, b BranchFacts, has Side) BranchAction {
	gone := otherSide(has)
	cur, hasCO, goneCO := b.Mac, b.MacCheckedOut, b.UbuntuCheckedOut
	if has == SideUbuntu {
		cur, hasCO, goneCO = b.Ubuntu, b.UbuntuCheckedOut, b.MacCheckedOut
	}
	hasName, goneName := sideNames([]Side{has}), sideNames([]Side{gone})
	if b.Base == "" { // 规则 1
		return createOnOther(act, cur, gone, goneCO)
	}
	if cur != b.Base { // 规则 3
		a := createOnOther(act, cur, gone, goneCO)
		if a.Kind == KindCreateOnOther {
			a.Reason = fmt.Sprintf("%s已删除该分支，但%s上的分支在上次同步（基线 %s）之后有新提交：旧的删了，新的照样建，在%s新建，指向 %s",
				goneName, hasName, short(b.Base), goneName, cur)
		}
		return a
	}
	// 规则 2：仍存在的一边就停在基线。
	if goneCO {
		act.Kind = KindInvalidFacts
		act.Reason = fmt.Sprintf("%s侧有工作区指向不存在的分支，需重新采集并人工确认，不删除", goneName)
		return act
	}
	act.Sides = []Side{has}
	if isProtectedBranch(b.Name) { // 规则 5
		act.Kind = KindDeleteBlocked
		act.Reason = fmt.Sprintf("%s已删除受保护分支 %s，%s上仍是上次同步时的提交 %s；main/master 永远不自动删除，请手动处理（在%s也删掉，或在%s重建）",
			goneName, b.Name, hasName, cur, hasName, goneName)
		return act
	}
	if hasCO { // 规则 4
		act.Kind = KindDeleteBlocked
		act.Reason = fmt.Sprintf("%s已删除该分支，但%s上该分支正被工作区检出，不删除、不切分支；请手动处理（切走后下一轮会自动删除，或在%s重建）。当前提交 %s",
			goneName, hasName, goneName, cur)
		return act
	}
	act.Kind = KindDeleteOnOther
	act.Reason = fmt.Sprintf("%s已删除该分支，%s上仍是上次同步时的提交，在%s也删除（原提交 %s）", goneName, hasName, hasName, cur)
	return act
}
