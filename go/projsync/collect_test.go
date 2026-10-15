package projsync

import (
	"context"
	"errors"
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

// 带子模块的仓库：即使配了 diff.ignoreSubmodules=all / submodule.<name>.ignore=all，
// 子模块里的改动也必须算 dirty；CheckScope 判超出，执行器不写。
func TestSubmoduleDirtyAndOutOfScope(t *testing.T) {
	sub := newRepo(t)
	dir := newRepo(t)
	ag(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", sub, "sm")
	ag(t, dir, "commit", "-m", "add sm")
	ag(t, dir, "config", "diff.ignoreSubmodules", "all")
	ag(t, dir, "config", "submodule.sm.ignore", "all")
	ctx := context.Background()

	if d, err := worktreeDirty(ctx, dir, nil); err != nil || d {
		t.Fatalf("干净子模块不应 dirty: %v %v", d, err)
	}
	os.WriteFile(filepath.Join(dir, "sm", "README.md"), []byte("changed\n"), 0o644)
	if out := ag(t, dir, "status", "--porcelain", "-uall"); strings.Contains(out, "sm") {
		t.Skip("此 git 版本下 ignore 配置未隐藏子模块改动，无法复现")
	}
	if d, err := worktreeDirty(ctx, dir, nil); err != nil || !d {
		t.Fatalf("子模块内改动应判 dirty: %v %v", d, err)
	}
	wf, err := CollectLocalWorktrees(ctx, dir, nil, SideMac, nil)
	if err != nil || len(wf) != 1 || !wf[0].Dirty {
		t.Fatalf("CollectLocalWorktrees: %+v %v", wf, err)
	}
	// 执行器写前的 checkClean 同样看得见
	if err := (ApplyPlan{RepoDir: dir}).checkClean(ctx, dir); err == nil {
		t.Fatal("checkClean 应拒绝子模块内有改动的工作区")
	}

	// CheckScope 判超出
	err = CheckScope(ctx, dir, nil, []string{"main"})
	var oos *ErrOutOfScope
	if !errors.As(err, &oos) || !strings.Contains(err.Error(), "仓库包含子模块，超出自动同步范围") {
		t.Fatalf("应判超出范围: %v", err)
	}
	// 执行器不写（开启范围检查）：创建分支被拒，引用不变
	refs := ag(t, dir, "for-each-ref")
	head := rev(t, dir, "HEAD")
	cerr := CreateBranch(ctx, ApplyPlan{RepoDir: dir, Branch: "newb", NewOID: head, Kind: KindCreateOnOther})
	if cerr == nil || ag(t, dir, "for-each-ref") != refs {
		t.Fatalf("执行器应拒绝且不写: %v", cerr)
	}

	// 仅有 gitlink、没有 .gitmodules 也判超出
	os.Remove(filepath.Join(dir, ".gitmodules"))
	if err := CheckScope(ctx, dir, nil, []string{"main"}); err == nil || !strings.Contains(err.Error(), "子模块") {
		t.Fatalf("gitlink 应判超出: %v", err)
	}

	// 普通仓库不受影响
	plain := newRepo(t)
	if err := CheckScope(ctx, plain, nil, []string{"main"}); err != nil {
		t.Fatalf("普通仓库不应超出: %v", err)
	}
}

// 目标提交树含子模块（工作区都不含）：写前提交树检查判超出，引用/对象不变。
func TestCommitTreeSubmoduleOutOfScope(t *testing.T) {
	ctx := context.Background()
	dir := newRepo(t)
	a := rev(t, dir, "HEAD")
	ag(t, dir, "branch", "other", a) // 未检出分支
	// B：A 的后代，新增 gitlink（不必真有子模块）
	ag(t, dir, "checkout", "-q", "-b", "withsub", a)
	ag(t, dir, "update-index", "--add", "--cacheinfo", "160000,"+a+",sub")
	ag(t, dir, "commit", "-m", "add gitlink")
	b := rev(t, dir, "HEAD")
	// C：A 的后代，只新增 .gitmodules
	ag(t, dir, "checkout", "-q", "-b", "withmods", a)
	c := commitFile(t, dir, ".gitmodules", "[submodule \"x\"]\n")
	// D：A 的后代，普通提交
	ag(t, dir, "checkout", "-q", "-b", "plain", a)
	d := commitFile(t, dir, "p.txt", "p\n")
	ag(t, dir, "checkout", "-q", "main")
	ag(t, dir, "branch", "-D", "withsub", "withmods", "plain")

	expect := func(err error, what string) {
		t.Helper()
		var oos *ErrOutOfScope
		if !errors.As(err, &oos) || !strings.Contains(err.Error(), "包含子模块，超出自动同步范围") {
			t.Fatalf("%s 应判超出范围: %v", what, err)
		}
	}
	refs := ag(t, dir, "for-each-ref")
	objs := ag(t, dir, "count-objects", "-v")
	same := func(what string) {
		t.Helper()
		if ag(t, dir, "for-each-ref") != refs || ag(t, dir, "count-objects", "-v") != objs {
			t.Fatalf("%s 后引用或对象库变了", what)
		}
	}
	expect(FastForwardBranch(ctx, ffPlan(dir, "other", a, b)), "快进到含 gitlink 的提交")
	expect(FastForwardBranch(ctx, ffPlan(dir, "other", a, c)), "快进到含 .gitmodules 的提交")
	same("快进")
	expect(CreateBranch(ctx, ApplyPlan{RepoDir: dir, Branch: "nb", NewOID: b, Kind: KindCreateOnOther}), "创建")
	same("创建")
	_, err := AutoMergeCommit(ctx, ApplyPlan{RepoDir: dir, Branch: "other", Kind: KindAutoMerge, Parents: [2]string{b, d}})
	expect(err, "自动合并")
	same("自动合并")
	// 旧值含子模块也拒绝
	expect(FastForwardBranch(ctx, ffPlan(dir, "other", b, d)), "旧值含子模块")
	// 普通提交不受影响
	if err := FastForwardBranch(ctx, ffPlan(dir, "other", a, d)); err != nil || rev(t, dir, "other") != d {
		t.Fatalf("普通快进应成功: %v", err)
	}
}

// RepoDir 必须是工作区根；ls-tree 用 --full-tree，子目录调用不会只查子树。
func TestRepoDirMustBeWorktreeRoot(t *testing.T) {
	ctx := context.Background()
	dir, c1, c2 := ffRepo(t)
	sub := filepath.Join(dir, "d")
	os.MkdirAll(sub, 0o755)
	refs := ag(t, dir, "for-each-ref")

	err := CheckScope(ctx, sub, nil, []string{"main"})
	if err == nil || !strings.Contains(err.Error(), "仓库路径不是工作区根目录") {
		t.Fatalf("子目录应判超出: %v", err)
	}
	if err := FastForwardBranch(ctx, ffPlan(sub, "feat", c1, c2)); err == nil || ag(t, dir, "for-each-ref") != refs {
		t.Fatalf("子目录调用执行器应拒绝且不写: %v", err)
	}

	// 符号链接指向仓库根：解析后相等，视为根，正常通过
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if err := CheckScope(ctx, link, nil, []string{"main"}); err != nil {
		t.Fatalf("指向仓库根的符号链接应通过: %v", err)
	}

	// gitlink 在子目录之外，从仓库内子目录调用 CheckCommitsScope 也要查整棵树（--full-tree）
	ag(t, dir, "checkout", "-q", "-b", "g", c1)
	ag(t, dir, "update-index", "--add", "--cacheinfo", "160000,"+c1+",outside")
	ag(t, dir, "commit", "-m", "gitlink")
	g := rev(t, dir, "HEAD")
	ag(t, dir, "checkout", "-q", "main")
	for _, d := range []string{dir, sub} {
		if err := CheckCommitsScope(ctx, d, nil, g); err == nil || !strings.Contains(err.Error(), "包含子模块") {
			t.Fatalf("%s 调用应判超出: %v", d, err)
		}
	}
}

// refs/replace、grafts、浅克隆、部分克隆：CheckScope 判超出，执行器不写。
func TestHistoryAlteringOutOfScope(t *testing.T) {
	ctx := context.Background()
	wantOOS := func(dir, key string) {
		t.Helper()
		err := CheckScope(ctx, dir, nil, []string{"main"})
		var oos *ErrOutOfScope
		if !errors.As(err, &oos) || !strings.Contains(err.Error(), key) {
			t.Fatalf("应判超出（%s）: %v", key, err)
		}
	}
	refuses := func(dir string) {
		t.Helper()
		before := ag(t, dir, "for-each-ref")
		head := rev(t, dir, "HEAD")
		err := CreateBranch(ctx, ApplyPlan{RepoDir: dir, Branch: "nb", NewOID: head, Kind: KindCreateOnOther})
		if err == nil || ag(t, dir, "for-each-ref") != before {
			t.Fatalf("执行器应拒绝且不写: %v", err)
		}
	}

	// refs/replace：B（与 A 无关）被替换成以 A 为父
	dir := newRepo(t)
	a := rev(t, dir, "HEAD")
	ag(t, dir, "checkout", "-q", "--orphan", "bb")
	ag(t, dir, "rm", "-rfq", ".")
	b := commitFile(t, dir, "b.txt", "b\n")
	ag(t, dir, "checkout", "-q", "main")
	ag(t, dir, "branch", "-D", "bb")
	if ok, err := isAncestor(ctx, dir, nil, a, b); err != nil || ok {
		t.Fatalf("无替换时 a 不是 b 的祖先: %v %v", ok, err)
	}
	// 用替换对象让 b 看起来以 a 为父
	fake := ag(t, dir, "commit-tree", ag(t, dir, "rev-parse", b+"^{tree}"), "-p", a, "-m", "fake")
	ag(t, dir, "replace", b, fake)
	if ok, err := isAncestor(ctx, dir, nil, a, b); err != nil || ok {
		t.Fatalf("有 refs/replace 时 isAncestor 应返回真实结果 false: %v %v", ok, err)
	}
	// 调用方 env 也不能覆盖
	if ok, err := isAncestor(ctx, dir, []string{"GIT_NO_REPLACE_OBJECTS=0"}, a, b); err != nil || ok {
		t.Fatalf("调用方不能覆盖 GIT_NO_REPLACE_OBJECTS: %v %v", ok, err)
	}
	wantOOS(dir, "refs/replace")
	refuses(dir)

	// grafts
	g := newRepo(t)
	os.WriteFile(filepath.Join(g, ".git", "info", "grafts"), nil, 0o644)
	wantOOS(g, "grafts")
	refuses(g)

	// 浅克隆 / 部分克隆
	src := newRepo(t)
	commitFile(t, src, "s2.txt", "2\n")
	shallow := filepath.Join(t.TempDir(), "shallow")
	ag(t, src, "clone", "-q", "--depth", "1", "file://"+src, shallow)
	wantOOS(shallow, "浅克隆")
	refuses(shallow)

	partial := filepath.Join(t.TempDir(), "partial")
	ag(t, src, "config", "uploadpack.allowFilter", "true")
	ag(t, src, "config", "uploadpack.allowAnySHA1InWant", "true")
	ag(t, src, "clone", "-q", "--filter=blob:none", "file://"+src, partial)
	wantOOS(partial, "部分克隆")
	refuses(partial)

	// 普通仓库不受影响
	if err := CheckScope(ctx, newRepo(t), nil, []string{"main"}); err != nil {
		t.Fatalf("普通仓库不应超出: %v", err)
	}
}

// 布尔配置由 Git 规范化：yes/1/TRUE/on 都算真；无法解析判超出；假值不因此超出。
func TestBoolConfigNormalized(t *testing.T) {
	ctx := context.Background()
	check := func(key, val string) error {
		dir := newRepo(t)
		ag(t, dir, "config", key, val)
		return CheckScope(ctx, dir, nil, []string{"main"})
	}
	for _, v := range []string{"yes", "1", "TRUE", "on"} {
		if err := check("remote.origin.promisor", v); err == nil || !strings.Contains(err.Error(), "部分克隆") {
			t.Fatalf("promisor=%s 应判超出: %v", v, err)
		}
	}
	if err := check("core.sparseCheckout", "yes"); err == nil || !strings.Contains(err.Error(), "sparse-checkout") {
		t.Fatalf("sparseCheckout=yes 应判超出: %v", err)
	}
	for _, v := range []string{"false", "0", "no"} {
		if err := check("core.sparseCheckout", v); err != nil {
			t.Fatalf("sparseCheckout=%s 不应超出: %v", v, err)
		}
		if err := check("remote.origin.promisor", v); err != nil {
			t.Fatalf("promisor=%s 不应超出: %v", v, err)
		}
	}
	for _, k := range []string{"core.sparseCheckout", "remote.origin.promisor"} {
		if err := check(k, "maybe"); err == nil {
			t.Fatalf("%s=maybe 无法解析应判超出", k)
		}
	}
}
