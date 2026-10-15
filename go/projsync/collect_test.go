package projsync

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// 所有测试只在 t.TempDir() 里新建临时仓库，不 push。

func facts(t *testing.T, bs []BranchFacts, name string) BranchFacts {
	t.Helper()
	for _, b := range bs {
		if b.Name == name {
			return b
		}
	}
	t.Fatalf("没有分支 %s: %+v", name, bs)
	return BranchFacts{}
}

func collect(t *testing.T, dir string, rw []WorktreeFacts, rr RepoFacts, q func(string) (int, bool)) ([]BranchFacts, RepoFacts, error) {
	return CollectBranchFacts(context.Background(), dir, nil, "ubuntu", rw, rr, q)
}

// branchAt 在 base 之上新建提交并放到分支 name，返回 oid；结束时回到 main。
func branchAt(t *testing.T, dir, name, base, file, content string) string {
	t.Helper()
	ag(t, dir, "checkout", "-q", "-b", "tmp-"+name, base)
	oid := commitFile(t, dir, file, content)
	ag(t, dir, "checkout", "-q", "main")
	ag(t, dir, "branch", "-f", name, oid)
	ag(t, dir, "branch", "-D", "tmp-"+name)
	return oid
}

func setRemote(t *testing.T, dir, name, oid string) {
	t.Helper()
	ag(t, dir, "update-ref", "refs/remotes/ubuntu/"+name, oid)
}

// 构造覆盖各类关系的仓库；main 两边相同，位于 base。
func relationRepo(t *testing.T) (dir string, base string) {
	dir = newRepo(t)
	base = rev(t, dir, "HEAD")
	setRemote(t, dir, "main", base)
	// 只 Mac 有
	ag(t, dir, "branch", "onlymac", base)
	// 只 Ubuntu 有
	setRemote(t, dir, "onlyub", base)
	// Mac 领先
	m := branchAt(t, dir, "macahead", base, "m.txt", "m\n")
	_ = m
	setRemote(t, dir, "macahead", base)
	// Ubuntu 领先
	ag(t, dir, "branch", "ubahead", base)
	setRemote(t, dir, "ubahead", branchAt(t, dir, "tmpu", base, "u.txt", "u\n"))
	ag(t, dir, "branch", "-D", "tmpu")
	// 分叉可干净合并
	branchAt(t, dir, "clean", base, "a.txt", "a\n")
	setRemote(t, dir, "clean", branchAt(t, dir, "tmpc", base, "b.txt", "b\n"))
	ag(t, dir, "branch", "-D", "tmpc")
	// 分叉有冲突，含中文文件名（两侧都改 README.md 和 中文 文件）
	cb := commitFileOn(t, dir, "base-c", base, "中 文.txt", "base\n")
	branchAt(t, dir, "conf", cb, "中 文.txt", "mac\n")
	setRemote(t, dir, "conf", branchAt(t, dir, "tmpf", cb, "中 文.txt", "ubuntu\n"))
	ag(t, dir, "branch", "-D", "tmpf")
	ag(t, dir, "branch", "-D", "base-c")
	return dir, base
}

func commitFileOn(t *testing.T, dir, branch, base, file, content string) string {
	t.Helper()
	ag(t, dir, "checkout", "-q", "-b", branch, base)
	oid := commitFile(t, dir, file, content)
	ag(t, dir, "checkout", "-q", "main")
	return oid
}

func TestCollectRelations(t *testing.T) {
	dir, base := relationRepo(t)
	bs, repo, err := collect(t, dir, nil, RepoFacts{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if repo.MacBusy || repo.UbuntuBusy {
		t.Fatalf("不该 busy: %+v", repo)
	}
	var names []string
	for _, b := range bs {
		names = append(names, b.Name)
	}
	want := []string{"clean", "conf", "macahead", "main", "onlymac", "onlyub", "ubahead"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("分支并集 %v != %v", names, want)
	}
	if b := facts(t, bs, "onlymac"); b.Mac != base || b.Ubuntu != "" {
		t.Fatalf("onlymac: %+v", b)
	}
	if b := facts(t, bs, "onlyub"); b.Mac != "" || b.Ubuntu != base {
		t.Fatalf("onlyub: %+v", b)
	}
	if b := facts(t, bs, "main"); b.Mac != b.Ubuntu || b.MacIsAncestorOfUbuntu || b.UbuntuIsAncestorOfMac {
		t.Fatalf("main: %+v", b)
	}
	if b := facts(t, bs, "macahead"); !b.UbuntuIsAncestorOfMac || b.MacIsAncestorOfUbuntu {
		t.Fatalf("macahead: %+v", b)
	}
	if b := facts(t, bs, "ubahead"); !b.MacIsAncestorOfUbuntu || b.UbuntuIsAncestorOfMac {
		t.Fatalf("ubahead: %+v", b)
	}
	if b := facts(t, bs, "clean"); !b.MergeClean || len(b.ConflictFiles) != 0 || b.MacIsAncestorOfUbuntu || b.UbuntuIsAncestorOfMac {
		t.Fatalf("clean: %+v", b)
	}
	if b := facts(t, bs, "conf"); b.MergeClean || !reflect.DeepEqual(b.ConflictFiles, []string{"中 文.txt"}) {
		t.Fatalf("conf: %+v", b)
	}
}

func TestCollectMacWorktrees(t *testing.T) {
	dir, _ := relationRepo(t)
	// 主工作区在 main：已跟踪文件改动 -> dirty
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// linked worktree 检出 onlymac，干净；另一个检出 clean，含未跟踪文件
	wt1 := filepath.Join(t.TempDir(), "wt1")
	wt2 := filepath.Join(t.TempDir(), "wt2")
	ag(t, dir, "worktree", "add", wt1, "onlymac")
	ag(t, dir, "worktree", "add", wt2, "clean")
	os.WriteFile(filepath.Join(wt2, "untracked.txt"), []byte("u"), 0o644)
	q := func(p string) (int, bool) {
		if p == dir {
			return 7, true
		}
		return 0, false
	}
	bs, repo, err := collect(t, dir, nil, RepoFacts{}, q)
	if err != nil {
		t.Fatal(err)
	}
	if repo.MacBusy {
		t.Fatal("不该 busy")
	}
	if b := facts(t, bs, "main"); !b.MacCheckedOut || !b.MacDirty || b.MacQuietMinutes != 7 {
		t.Fatalf("main: %+v", b)
	}
	if b := facts(t, bs, "onlymac"); !b.MacCheckedOut || b.MacDirty {
		t.Fatalf("onlymac: %+v", b)
	}
	if b := facts(t, bs, "clean"); !b.MacCheckedOut || !b.MacDirty || b.MacQuietMinutes != 0 {
		t.Fatalf("clean（未跟踪文件算 dirty，ok=false 视为 0）: %+v", b)
	}
	if b := facts(t, bs, "conf"); b.MacCheckedOut || b.MacDirty {
		t.Fatalf("conf: %+v", b)
	}
}

func TestCollectBusyMergeHead(t *testing.T) {
	dir, _ := relationRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	ag(t, dir, "worktree", "add", wt, "onlymac")
	gd := ag(t, wt, "rev-parse", "--absolute-git-dir")
	if err := os.WriteFile(filepath.Join(gd, "MERGE_HEAD"), []byte(rev(t, dir, "HEAD")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, repo, err := collect(t, dir, nil, RepoFacts{UbuntuBusy: true, UbuntuBusyReason: "rebase-merge"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !repo.MacBusy || repo.MacBusyReason != "MERGE_HEAD" || !repo.UbuntuBusy || repo.UbuntuBusyReason != "rebase-merge" {
		t.Fatalf("repo: %+v", repo)
	}
}

func TestCollectUbuntuWorktrees(t *testing.T) {
	dir, _ := relationRepo(t)
	rw := []WorktreeFacts{
		{Side: SideUbuntu, Path: "/u/proj", Branch: "ubahead", Dirty: true, QuietMinutes: 25},
		{Side: SideUbuntu, Path: "/u/det", Branch: ""},
	}
	bs, _, err := collect(t, dir, rw, RepoFacts{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	b := facts(t, bs, "ubahead")
	if !b.UbuntuCheckedOut || !b.UbuntuDirty || b.UbuntuQuietMinutes != 25 || b.MacCheckedOut {
		t.Fatalf("ubahead: %+v", b)
	}
	if o := facts(t, bs, "main"); o.UbuntuCheckedOut || o.UbuntuDirty {
		t.Fatalf("main: %+v", o)
	}
}

func TestCollectReadFailures(t *testing.T) {
	if _, _, err := collect(t, filepath.Join(t.TempDir(), "nope"), nil, RepoFacts{}, nil); err == nil {
		t.Fatal("仓库不存在应返回 error")
	}
	if _, _, err := CollectBranchFacts(context.Background(), t.TempDir(), nil, "ubuntu", nil, RepoFacts{}, nil); err == nil {
		t.Fatal("非仓库应返回 error")
	}
	// 对象损坏：删掉分叉分支其中一个提交的对象文件 -> 祖先检测失败 -> error，而不是当成「不是祖先」
	dir, _ := relationRepo(t)
	oid := rev(t, dir, "clean")
	if err := os.Remove(filepath.Join(dir, ".git", "objects", oid[:2], oid[2:])); err != nil {
		t.Fatal(err)
	}
	if _, _, err := collect(t, dir, nil, RepoFacts{}, nil); err == nil {
		t.Fatal("对象损坏应返回 error")
	}
	// 非法命名空间
	if _, _, err := CollectBranchFacts(context.Background(), dir, nil, "../x", nil, RepoFacts{}, nil); err == nil {
		t.Fatal("非法 remoteNS 应返回 error")
	}
}

func TestCollectFeedsDecide(t *testing.T) {
	dir, base := relationRepo(t)
	// ubahead 在 Mac 未检出但 Mac 侧 macahead 被主工作区检出且 dirty（静止 5 分钟）；
	ag(t, dir, "checkout", "-q", "ubahead")
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("dirty\n"), 0o644)
	q := func(string) (int, bool) { return 5, true }
	bs, repo, err := collect(t, dir, nil, RepoFacts{}, q)
	if err != nil {
		t.Fatal(err)
	}
	acts := map[string]BranchAction{}
	for _, a := range DecideBranches(repo, bs) {
		acts[a.Name] = a
	}
	if a := acts["main"]; a.Kind != KindNothing {
		t.Fatalf("main: %+v", a)
	}
	if a := acts["onlymac"]; a.Kind != KindCreateOnOther || a.Target != base {
		t.Fatalf("onlymac: %+v", a)
	}
	if a := acts["onlyub"]; a.Kind != KindCreateOnOther {
		t.Fatalf("onlyub: %+v", a)
	}
	if a := acts["macahead"]; a.Kind != KindFastForward || !reflect.DeepEqual(a.Sides, []Side{SideUbuntu}) {
		t.Fatalf("macahead: %+v", a)
	}
	if a := acts["ubahead"]; a.Kind != KindWaitDirty || !reflect.DeepEqual(a.Sides, []Side{SideMac}) {
		t.Fatalf("ubahead 被 dirty 且静止 5 分钟挡住应 WaitDirty: %+v", a)
	}
	if a := acts["clean"]; a.Kind != KindAutoMerge {
		t.Fatalf("clean: %+v", a)
	}
	if a := acts["conf"]; a.Kind != KindConflict {
		t.Fatalf("conf: %+v", a)
	}
	// 静止够久 -> BlockedDirty
	bs, repo, err = collect(t, dir, nil, RepoFacts{}, func(string) (int, bool) { return 30, true })
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range DecideBranches(repo, bs) {
		if a.Name == "ubahead" && a.Kind != KindBlockedDirty {
			t.Fatalf("ubahead 静止 30 分钟应 BlockedDirty: %+v", a)
		}
	}
}

func TestCollectLocalWorktrees(t *testing.T) {
	dir, _ := relationRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	ag(t, dir, "worktree", "add", "--detach", wt)
	os.WriteFile(filepath.Join(wt, "x"), []byte("x"), 0o644)
	got, err := CollectLocalWorktrees(context.Background(), dir, nil, SideUbuntu, func(string) (int, bool) { return 3, true })
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	for _, w := range got {
		switch w.Path {
		case dir:
			if w.Side != SideUbuntu || w.Branch != "main" || w.Dirty || w.QuietMinutes != 0 {
				t.Fatalf("主工作区: %+v", w)
			}
		default:
			if w.Branch != "" || !w.Dirty || w.QuietMinutes != 3 {
				t.Fatalf("detached: %+v", w)
			}
		}
	}
	if _, err := CollectLocalWorktrees(context.Background(), filepath.Join(t.TempDir(), "nope"), nil, SideMac, nil); err == nil {
		t.Fatal("仓库不存在应返回 error")
	}
}

// 采集前后：引用、索引、工作区、stash 全部不变；对象库一个对象都没多（merge-tree 的对象写到临时目录）。
func TestCollectIsReadOnly(t *testing.T) {
	dir, _ := relationRepo(t)
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("dirty\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("u"), 0o644)
	wt := filepath.Join(t.TempDir(), "wt")
	ag(t, dir, "worktree", "add", wt, "clean")
	before := snapshot(t, dir)
	beforeWt := snapshot(t, wt)
	objs := ag(t, dir, "count-objects", "-v")
	allRefs := ag(t, dir, "for-each-ref")
	if _, _, err := collect(t, dir, nil, RepoFacts{}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := CollectLocalWorktrees(context.Background(), dir, nil, SideMac, nil); err != nil {
		t.Fatal(err)
	}
	if snapshot(t, dir) != before || snapshot(t, wt) != beforeWt {
		t.Fatal("采集改变了现场")
	}
	if ag(t, dir, "for-each-ref") != allRefs {
		t.Fatal("引用变了")
	}
	if got := ag(t, dir, "count-objects", "-v"); got != objs {
		t.Fatalf("对象库变了（merge-tree 不应写入真实对象库）:\n%s\n---\n%s", objs, got)
	}
	if !strings.Contains(ag(t, dir, "status", "--porcelain"), "untracked.txt") {
		t.Fatal("未跟踪文件应仍在")
	}
}
