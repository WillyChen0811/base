package projsync

import (
	"strings"
	"testing"
)

// 规则 1–5 的纯判定（两个方向都测）。
func TestDecideOneSidedRules(t *testing.T) {
	const x, y = "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222"
	cases := []struct {
		name string
		bf   BranchFacts
		kind ActionKind
		side Side
		want string // Reason 里应含的字样
	}{
		{"规则1 没有基线 Mac 独有→Ubuntu 建", BranchFacts{Name: "f", Mac: x}, KindCreateOnOther, SideUbuntu, "只有Mac有"},
		{"规则1 没有基线 Ubuntu 独有→Mac 建", BranchFacts{Name: "f", Ubuntu: x}, KindCreateOnOther, SideMac, "只有Ubuntu有"},
		{"规则2 Ubuntu 删了、Mac 停在基线→Mac 删", BranchFacts{Name: "f", Mac: x, Base: x}, KindDeleteOnOther, SideMac, "Ubuntu已删除"},
		{"规则2 Mac 删了、Ubuntu 停在基线→Ubuntu 删", BranchFacts{Name: "f", Ubuntu: x, Base: x}, KindDeleteOnOther, SideUbuntu, "Mac已删除"},
		{"规则3 Ubuntu 删了、Mac 有新提交→Ubuntu 建", BranchFacts{Name: "f", Mac: y, Base: x}, KindCreateOnOther, SideUbuntu, "旧的删了，新的照样建"},
		{"规则3 Mac 删了、Ubuntu 有新提交→Mac 建", BranchFacts{Name: "f", Ubuntu: y, Base: x}, KindCreateOnOther, SideMac, "旧的删了，新的照样建"},
		{"规则4 要删的 Mac 正被检出", BranchFacts{Name: "f", Mac: x, Base: x, MacCheckedOut: true, MacDirty: true}, KindDeleteBlocked, SideMac, "正被工作区检出"},
		{"规则4 要删的 Ubuntu 正被检出", BranchFacts{Name: "f", Ubuntu: x, Base: x, UbuntuCheckedOut: true}, KindDeleteBlocked, SideUbuntu, "正被工作区检出"},
		{"规则5 main 受保护", BranchFacts{Name: "main", Mac: x, Base: x}, KindDeleteBlocked, SideMac, "永远不自动删除"},
		{"规则5 master 受保护（即使被检出也按保护说）", BranchFacts{Name: "master", Ubuntu: x, Base: x, UbuntuCheckedOut: true}, KindDeleteBlocked, SideUbuntu, "永远不自动删除"},
		{"规则3 main 有新提交照样建", BranchFacts{Name: "main", Mac: y, Base: x}, KindCreateOnOther, SideUbuntu, "旧的删了"},
		{"删掉的一边有工作区指向不存在的分支→事实矛盾不删", BranchFacts{Name: "f", Mac: x, Base: x, UbuntuCheckedOut: true}, KindInvalidFacts, "", "不删除"},
		{"规则3 删掉的一边有工作区指向不存在的分支→不建", BranchFacts{Name: "f", Mac: y, Base: x, UbuntuCheckedOut: true}, KindInvalidFacts, "", "不创建"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			acts := DecideBranches(RepoFacts{}, []BranchFacts{c.bf})
			a := acts[0]
			if a.Kind != c.kind {
				t.Fatalf("Kind=%s，期望 %s（%s）", a.Kind, c.kind, a.Reason)
			}
			if c.side != "" && (len(a.Sides) != 1 || a.Sides[0] != c.side) {
				t.Fatalf("Sides=%v，期望 %s", a.Sides, c.side)
			}
			if !strings.Contains(a.Reason, c.want) {
				t.Fatalf("Reason=%q 应含 %q", a.Reason, c.want)
			}
			if c.kind == KindCreateOnOther && a.Target != c.bf.Mac+c.bf.Ubuntu {
				t.Fatalf("新建目标 %s 应是仍存在一边的提交", a.Target)
			}
			if c.kind == KindDeleteOnOther && a.Target != "" {
				t.Fatalf("删除不应带目标 %q", a.Target)
			}
		})
	}
}

// 两边都有时基线不参与判定；仓库忙时一律跳过（不删）。
func TestDecideBaseIgnoredWhenBothPresentOrBusy(t *testing.T) {
	const x, y = "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222"
	if a := DecideBranches(RepoFacts{}, []BranchFacts{{Name: "f", Mac: x, Ubuntu: x, Base: y}})[0]; a.Kind != KindNothing {
		t.Fatalf("%+v", a)
	}
	if a := DecideBranches(RepoFacts{UbuntuBusy: true}, []BranchFacts{{Name: "f", Mac: x, Base: x}})[0]; a.Kind != KindSkipRepoBusy {
		t.Fatalf("%+v", a)
	}
}
