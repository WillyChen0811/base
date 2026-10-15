package projsync

import (
	"reflect"
	"strings"
	"testing"
)

func one(t *testing.T, repo RepoFacts, b BranchFacts) BranchAction {
	t.Helper()
	r := DecideBranches(repo, []BranchFacts{b})
	if len(r) != 1 {
		t.Fatalf("want 1 action, got %d", len(r))
	}
	return r[0]
}

func wantKind(t *testing.T, a BranchAction, k ActionKind) {
	t.Helper()
	if a.Kind != k {
		t.Fatalf("kind=%s want %s (reason=%s)", a.Kind, k, a.Reason)
	}
}

func TestDecideSame(t *testing.T) {
	a := one(t, RepoFacts{}, BranchFacts{Name: "main", Mac: "aaa", Ubuntu: "aaa"})
	wantKind(t, a, KindNothing)
	if len(a.Sides) != 0 || a.Target != "" {
		t.Fatalf("unexpected %+v", a)
	}
}

func TestDecideOnlyMac(t *testing.T) {
	a := one(t, RepoFacts{}, BranchFacts{Name: "x", Mac: "aaa", UbuntuCheckedOut: true, UbuntuDirty: true})
	wantKind(t, a, KindCreateOnOther)
	if a.Target != "aaa" || !reflect.DeepEqual(a.Sides, []Side{SideUbuntu}) {
		t.Fatalf("%+v", a)
	}
}

func TestDecideOnlyUbuntu(t *testing.T) {
	a := one(t, RepoFacts{}, BranchFacts{Name: "x", Ubuntu: "bbb"})
	wantKind(t, a, KindCreateOnOther)
	if a.Target != "bbb" || !reflect.DeepEqual(a.Sides, []Side{SideMac}) {
		t.Fatalf("%+v", a)
	}
}

func macAhead() BranchFacts {
	return BranchFacts{Name: "main", Mac: "m2", Ubuntu: "u1", UbuntuIsAncestorOfMac: true}
}

func TestDecideMacAheadUbuntuClean(t *testing.T) {
	a := one(t, RepoFacts{}, macAhead())
	wantKind(t, a, KindFastForward)
	if a.Target != "m2" || !reflect.DeepEqual(a.Sides, []Side{SideUbuntu}) {
		t.Fatalf("%+v", a)
	}
	// 被检出但干净也可以快进
	b := macAhead()
	b.UbuntuCheckedOut = true
	wantKind(t, one(t, RepoFacts{}, b), KindFastForward)
}

func TestDecideUbuntuAheadMacBehind(t *testing.T) {
	a := one(t, RepoFacts{}, BranchFacts{Name: "main", Mac: "m1", Ubuntu: "u2", MacIsAncestorOfUbuntu: true})
	wantKind(t, a, KindFastForward)
	if a.Target != "u2" || !reflect.DeepEqual(a.Sides, []Side{SideMac}) {
		t.Fatalf("%+v", a)
	}
}

func TestDecideWaitDirty(t *testing.T) {
	b := macAhead()
	b.UbuntuCheckedOut, b.UbuntuDirty, b.UbuntuQuietMinutes = true, true, 5
	a := one(t, RepoFacts{}, b)
	wantKind(t, a, KindWaitDirty)
	if !strings.Contains(a.Reason, "15 分钟") {
		t.Fatalf("reason=%s", a.Reason)
	}
	if !reflect.DeepEqual(a.Sides, []Side{SideUbuntu}) {
		t.Fatalf("%+v", a)
	}
}

func TestDecideNeedCommit(t *testing.T) {
	b := macAhead()
	b.UbuntuCheckedOut, b.UbuntuDirty, b.UbuntuQuietMinutes = true, true, 25
	a := one(t, RepoFacts{}, b)
	wantKind(t, a, KindNeedCommit)
	if !strings.Contains(a.Reason, "Ubuntu") {
		t.Fatalf("reason=%s", a.Reason)
	}
}

func TestDecideQuietBoundary(t *testing.T) {
	b := macAhead()
	b.UbuntuCheckedOut, b.UbuntuDirty = true, true
	b.UbuntuQuietMinutes = QuietMinutesForAutoCommit
	wantKind(t, one(t, RepoFacts{}, b), KindNeedCommit)
	b.UbuntuQuietMinutes = QuietMinutesForAutoCommit - 1
	a := one(t, RepoFacts{}, b)
	wantKind(t, a, KindWaitDirty)
	if !strings.Contains(a.Reason, "1 分钟") {
		t.Fatalf("reason=%s", a.Reason)
	}
}

func TestDecideLeadingSideDirtyIgnored(t *testing.T) {
	b := macAhead()
	b.MacCheckedOut, b.MacDirty, b.MacQuietMinutes = true, true, 1
	a := one(t, RepoFacts{}, b)
	wantKind(t, a, KindFastForward)
	if !reflect.DeepEqual(a.Sides, []Side{SideUbuntu}) {
		t.Fatalf("%+v", a)
	}
}

func TestDecideDirtyButNotCheckedOutIgnored(t *testing.T) {
	b := macAhead()
	b.UbuntuDirty, b.UbuntuQuietMinutes = true, 0 // 未检出，dirty 无意义
	wantKind(t, one(t, RepoFacts{}, b), KindFastForward)
}

func diverged() BranchFacts {
	return BranchFacts{Name: "dev", Mac: "m", Ubuntu: "u", MergeClean: true}
}

func TestDecideAutoMergeClean(t *testing.T) {
	a := one(t, RepoFacts{}, diverged())
	wantKind(t, a, KindAutoMerge)
	if a.Target != "" || !reflect.DeepEqual(a.Sides, []Side{SideMac, SideUbuntu}) {
		t.Fatalf("%+v", a)
	}
}

func TestDecideAutoMergeMacDirty(t *testing.T) {
	b := diverged()
	b.MacCheckedOut, b.MacDirty, b.MacQuietMinutes = true, true, 3
	a := one(t, RepoFacts{}, b)
	wantKind(t, a, KindWaitDirty)
	if a.Target != "" || !reflect.DeepEqual(a.Sides, []Side{SideMac}) {
		t.Fatalf("%+v", a)
	}
	if !strings.Contains(a.Reason, "17 分钟") || !strings.Contains(a.Reason, "两边都不动") {
		t.Fatalf("reason=%s", a.Reason)
	}
	b.MacQuietMinutes = 30
	wantKind(t, one(t, RepoFacts{}, b), KindNeedCommit)
}

func TestDecideAutoMergeBothDirtyMixed(t *testing.T) {
	b := diverged()
	b.MacCheckedOut, b.MacDirty, b.MacQuietMinutes = true, true, 30
	b.UbuntuCheckedOut, b.UbuntuDirty, b.UbuntuQuietMinutes = true, true, 10
	a := one(t, RepoFacts{}, b)
	wantKind(t, a, KindWaitDirty) // 任一边未静止够就先等
	if !strings.Contains(a.Reason, "10 分钟") {
		t.Fatalf("reason=%s", a.Reason)
	}
}

func TestDecideConflict(t *testing.T) {
	b := BranchFacts{Name: "dev", Mac: "m", Ubuntu: "u", ConflictFiles: []string{"a.go", "b/c.txt"},
		MacCheckedOut: true, MacDirty: true, MacQuietMinutes: 99}
	a := one(t, RepoFacts{}, b)
	wantKind(t, a, KindConflict)
	if len(a.Sides) != 0 || a.Target != "" {
		t.Fatalf("%+v", a)
	}
	if !strings.Contains(a.Reason, "a.go") || !strings.Contains(a.Reason, "b/c.txt") {
		t.Fatalf("reason=%s", a.Reason)
	}
}

func TestDecideRepoBusy(t *testing.T) {
	repo := RepoFacts{UbuntuBusy: true, UbuntuBusyReason: "rebase 进行中"}
	r := DecideBranches(repo, []BranchFacts{macAhead(), {Name: "a", Mac: "1", Ubuntu: "1"}})
	if len(r) != 2 {
		t.Fatal(len(r))
	}
	for _, a := range r {
		wantKind(t, a, KindSkipRepoBusy)
		if !strings.Contains(a.Reason, "rebase 进行中") {
			t.Fatalf("reason=%s", a.Reason)
		}
	}
}

func TestDecideSortedDeterministic(t *testing.T) {
	in := []BranchFacts{
		{Name: "zeta", Mac: "1", Ubuntu: "1"},
		macAhead(), // main
		{Name: "alpha", Mac: "x"},
		diverged(), // dev
	}
	orig := append([]BranchFacts(nil), in...)
	r := DecideBranches(RepoFacts{}, in)
	var names []string
	for _, a := range r {
		names = append(names, a.Name)
	}
	if !reflect.DeepEqual(names, []string{"alpha", "dev", "main", "zeta"}) {
		t.Fatalf("names=%v", names)
	}
	if !reflect.DeepEqual(in, orig) {
		t.Fatal("input was mutated")
	}
	if !reflect.DeepEqual(r, DecideBranches(RepoFacts{}, in)) {
		t.Fatal("non-deterministic")
	}
}

func oneWT(t *testing.T, repo RepoFacts, w WorktreeFacts) WorktreeAction {
	t.Helper()
	r := DecideWorktrees(repo, []WorktreeFacts{w})
	if len(r) != 1 {
		t.Fatalf("want 1, got %d", len(r))
	}
	return r[0]
}

func TestWorktreeDirtyWhileBranchesSame(t *testing.T) {
	// 分支两边相同（DecideBranches 返回 Nothing），但 Mac 工作区 dirty 静止 30 分钟
	wantKind(t, one(t, RepoFacts{}, BranchFacts{Name: "main", Mac: "a", Ubuntu: "a", MacCheckedOut: true, MacDirty: true, MacQuietMinutes: 30}), KindNothing)
	a := oneWT(t, RepoFacts{}, WorktreeFacts{Side: SideMac, Path: "/w/x", Branch: "main", Dirty: true, QuietMinutes: 30})
	if a.Kind != KindNeedCommit {
		t.Fatalf("%+v", a)
	}
	for _, s := range []string{"Mac", "/w/x", "main"} {
		if !strings.Contains(a.Reason, s) {
			t.Fatalf("reason missing %q: %s", s, a.Reason)
		}
	}
}

func TestWorktreeBoundary(t *testing.T) {
	w := WorktreeFacts{Side: SideUbuntu, Path: "/p", Branch: "b", Dirty: true, QuietMinutes: 19}
	a := oneWT(t, RepoFacts{}, w)
	if a.Kind != KindWaitDirty || !strings.Contains(a.Reason, "1 分钟") {
		t.Fatalf("%+v", a)
	}
	w.QuietMinutes = 20
	if a := oneWT(t, RepoFacts{}, w); a.Kind != KindNeedCommit {
		t.Fatalf("%+v", a)
	}
}

func TestWorktreeCleanAndBusy(t *testing.T) {
	if a := oneWT(t, RepoFacts{}, WorktreeFacts{Side: SideMac, Path: "/p", QuietMinutes: 99}); a.Kind != KindNothing {
		t.Fatalf("%+v", a)
	}
	a := oneWT(t, RepoFacts{MacBusy: true, MacBusyReason: "merge 中"}, WorktreeFacts{Side: SideMac, Path: "/p", Dirty: true, QuietMinutes: 99})
	if a.Kind != KindSkipRepoBusy || !strings.Contains(a.Reason, "merge 中") {
		t.Fatalf("%+v", a)
	}
}

func TestWorktreeSorted(t *testing.T) {
	in := []WorktreeFacts{
		{Side: SideUbuntu, Path: "/a"},
		{Side: SideMac, Path: "/z"},
		{Side: SideMac, Path: "/b"},
	}
	r := DecideWorktrees(RepoFacts{}, in)
	got := []string{string(r[0].Side) + r[0].Path, string(r[1].Side) + r[1].Path, string(r[2].Side) + r[2].Path}
	if !reflect.DeepEqual(got, []string{"mac/b", "mac/z", "ubuntu/a"}) {
		t.Fatalf("%v", got)
	}
	if in[0].Path != "/a" {
		t.Fatal("input mutated")
	}
}

func TestGateDirtyReasonByKind(t *testing.T) {
	b := macAhead()
	b.UbuntuCheckedOut, b.UbuntuDirty, b.UbuntuQuietMinutes = true, true, 5
	if a := one(t, RepoFacts{}, b); strings.Contains(a.Reason, "两边都不动") {
		t.Fatalf("FastForward reason wrong: %s", a.Reason)
	}
}
