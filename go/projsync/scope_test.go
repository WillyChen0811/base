package projsync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// 所有用例只在 t.TempDir() 的临时仓库里进行。

const (
	nfdName = "café" // NFD
	nfcName = "café"  // NFC（Latin：本检查的保守做法下同样判超出）
)

// wantOOS 断言三个写原语之一被范围检查拦下：返回 ErrOutOfScope、Reasons 含 wants 的每个子串、且 dir 的快照完全不变。
func wantOOS(t *testing.T, dir string, p ApplyPlan, wants ...string) *ErrOutOfScope {
	t.Helper()
	before := snapshot(t, dir)
	err := FastForwardBranch(context.Background(), p)
	var oos *ErrOutOfScope
	if !errors.As(err, &oos) {
		t.Fatalf("期望 ErrOutOfScope，实际: %v", err)
	}
	if !strings.Contains(oos.Error(), "超出自动同步范围：") {
		t.Fatalf("错误文案不对: %v", oos)
	}
	all := strings.Join(oos.Reasons, "\n")
	for _, w := range wants {
		if !strings.Contains(all, w) {
			t.Fatalf("原因缺少 %q，实际:\n%s", w, all)
		}
	}
	if after := snapshot(t, dir); after != before {
		t.Fatalf("超出范围却发生了写入:\n前:%s\n后:%s", before, after)
	}
	return oos
}

// 在 ffRepo 上建分支 name 指向 c1，返回 FF 计划（c1->c2，分支未检出）。
func branchPlan(t *testing.T, name string) (string, ApplyPlan) {
	dir, c1, c2 := ffRepo(t)
	ag(t, dir, "branch", name, c1)
	return dir, ffPlan(dir, name, c1, c2)
}

func TestScopeInRange(t *testing.T) {
	ctx := context.Background()
	dir, c1, c2 := ffRepo(t)
	if err := CheckScope(ctx, dir, nil, []string{"main", "feat"}); err != nil {
		t.Fatal(err)
	}
	if err := FastForwardBranch(ctx, ffPlan(dir, "main", c1, c2)); err != nil || rev(t, dir, "HEAD") != c2 {
		t.Fatalf("普通分支快进应成功: %v", err)
	}
	// 中文分支名：创建 + 快进
	cn := "功能/测试"
	cp := ApplyPlan{RepoDir: dir, Branch: cn, NewOID: c1, Kind: KindCreateOnOther}
	if err := CreateBranch(ctx, cp); err != nil || rev(t, dir, "refs/heads/"+cn) != c1 {
		t.Fatalf("中文分支创建应成功: %v", err)
	}
	if err := FastForwardBranch(ctx, ffPlan(dir, cn, c1, c2)); err != nil || rev(t, dir, "refs/heads/"+cn) != c2 {
		t.Fatalf("中文分支快进应成功: %v", err)
	}
	// 合并提交原语也在范围内可用
	mp := ApplyPlan{RepoDir: dir, Branch: "main", Kind: KindAutoMerge, Parents: [2]string{c1, c2}}
	if _, err := AutoMergeCommit(ctx, mp); err != nil {
		t.Fatal(err)
	}
}

func TestScopeLinkedWorktreeAllowed(t *testing.T) {
	dir, _, _ := ffRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	ag(t, dir, "worktree", "add", "-b", "wtb", wt)
	if err := CheckScope(context.Background(), wt, nil, []string{"feat"}); err != nil {
		t.Fatal(err)
	}
}

func TestScopeNonNFC(t *testing.T) {
	dir, p := branchPlan(t, nfdName)
	wantOOS(t, dir, p, "NFC")
	dir, p = branchPlan(t, nfcName) // 保守做法：Latin 带音标一律超出
	wantOOS(t, dir, p, "NFC")
}

func TestScopeEquals(t *testing.T) {
	dir, p := branchPlan(t, "a=b")
	wantOOS(t, dir, p, "含 =")
}

func TestScopeNormalizationClash(t *testing.T) {
	dir, c1, c2 := ffRepo(t)
	ag(t, dir, "branch", nfcName, c1)
	ag(t, dir, "branch", nfdName, c1)
	// 写的是普通分支 feat，也因仓库内存在冲突对而停
	o := wantOOS(t, dir, ffPlan(dir, "main", c1, c2), "规范化后相同但字节不同", nfcName, nfdName)
	_ = o
}

func TestScopeSymbolicRef(t *testing.T) {
	dir, c1, c2 := ffRepo(t)
	ag(t, dir, "symbolic-ref", "refs/heads/alias", "refs/heads/feat")
	wantOOS(t, dir, ffPlan(dir, "alias", c1, c2), "符号引用")
	// 悬空符号引用，创建路径同样停
	ag(t, dir, "symbolic-ref", "refs/heads/dangling", "refs/heads/nowhere")
	before := snapshot(t, dir)
	err := CreateBranch(context.Background(), ApplyPlan{RepoDir: dir, Branch: "dangling", NewOID: c1, Kind: KindCreateOnOther})
	var oos *ErrOutOfScope
	if !errors.As(err, &oos) || snapshot(t, dir) != before {
		t.Fatalf("悬空符号引用应超出范围且不写: %v", err)
	}
}

func TestScopeBranchConfig(t *testing.T) {
	for _, key := range []string{"mergeOptions", "rebase"} {
		dir, p := branchPlan(t, "feat2")
		val := "--squash"
		if key == "rebase" {
			val = "true"
		}
		ag(t, dir, "config", "branch.feat2."+key, val)
		wantOOS(t, dir, p, "branch.feat2."+strings.ToLower(key))
	}
	// 别的分支的配置不影响本分支
	dir, c1, c2 := ffRepo(t)
	ag(t, dir, "config", "branch.feat.mergeOptions", "--squash")
	if err := CheckScope(context.Background(), dir, nil, []string{"main"}); err != nil {
		t.Fatal(err)
	}
	_ = c1
	_ = c2
}

func TestScopeSparseCheckout(t *testing.T) {
	dir, c1, c2 := ffRepo(t)
	ag(t, dir, "config", "core.sparseCheckout", "true")
	wantOOS(t, dir, ffPlan(dir, "main", c1, c2), "sparse-checkout")

	dir, c1, c2 = ffRepo(t)
	if err := os.WriteFile(filepath.Join(dir, ".git", "info", "sparse-checkout"), []byte("/*\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wantOOS(t, dir, ffPlan(dir, "main", c1, c2), "sparse-checkout 文件")
}

func TestScopeSkipWorktreeAndAssumeUnchanged(t *testing.T) {
	dir, c1, c2 := ffRepo(t)
	ag(t, dir, "update-index", "--skip-worktree", "README.md")
	wantOOS(t, dir, ffPlan(dir, "main", c1, c2), "skip-worktree")

	dir, c1, c2 = ffRepo(t)
	ag(t, dir, "update-index", "--assume-unchanged", "README.md")
	wantOOS(t, dir, ffPlan(dir, "main", c1, c2), "assume-unchanged")
}

func TestScopeSeparateGitDir(t *testing.T) {
	base := t.TempDir()
	wd, gd := filepath.Join(base, "work"), filepath.Join(base, "gitdir")
	if err := os.MkdirAll(wd, 0o755); err != nil {
		t.Fatal(err)
	}
	ag(t, wd, "init", "-b", "main", "--separate-git-dir", gd)
	ag(t, wd, "config", "user.name", "t")
	ag(t, wd, "config", "user.email", "t@t")
	ag(t, wd, "config", "commit.gpgsign", "false")
	c1 := commitFile(t, wd, "a.txt", "a\n")
	ag(t, wd, "branch", "feat")
	c2 := commitFile(t, wd, "b.txt", "b\n")
	wantOOS(t, wd, ffPlan(wd, "feat", c1, c2), "特殊布局")
}

func TestScopeInheritedGitDir(t *testing.T) {
	other, _, _ := ffRepo(t)
	otherSnap := snapshot(t, other)
	dir, c1, c2 := ffRepo(t)
	// 选择：继承的 GIT_* 被 cleanEnv 清掉，执行器作用于 repoDir 指向的正确仓库（不判超出）。
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)
	err := FastForwardBranch(context.Background(), ffPlan(dir, "main", c1, c2))
	os.Unsetenv("GIT_DIR") // 之后的断言用的 git 辅助命令不能再受它影响（t.Setenv 的清理会恢复原值）
	os.Unsetenv("GIT_WORK_TREE")
	if err != nil {
		t.Fatal(err)
	}
	if rev(t, dir, "refs/heads/main") != c2 {
		t.Fatal("正确仓库应被更新")
	}
	if snapshot(t, other) != otherSnap {
		t.Fatal("GIT_DIR 指向的另一个仓库被改动")
	}
	// 但调用方显式经 Env 传入的布局类变量：判超出，不写
	dir2, d1, d2 := ffRepo(t)
	p := ffPlan(dir2, "main", d1, d2)
	p.Env = []string{"GIT_DIR=" + filepath.Join(other, ".git")}
	wantOOS(t, dir2, p, "GIT_DIR")
}

func TestScopeTooManyWorktrees(t *testing.T) {
	dir, c1, c2 := ffRepo(t)
	for i := 1; i <= maxWorktrees-1; i++ { // 主 + 7 = 8，边界内
		ag(t, dir, "worktree", "add", "-b", fmt.Sprintf("w%d", i), filepath.Join(t.TempDir(), fmt.Sprintf("w%d", i)))
	}
	if err := CheckScope(context.Background(), dir, nil, []string{"feat"}); err != nil {
		t.Fatalf("8 个工作区应在范围内: %v", err)
	}
	ag(t, dir, "worktree", "add", "-b", "w9", filepath.Join(t.TempDir(), "w9"))
	wantOOS(t, dir, ffPlan(dir, "feat", c1, c2), "超过上限")
}

func TestScopeMultipleReasonsAtOnce(t *testing.T) {
	dir, c1, c2 := ffRepo(t)
	name := nfdName + "=x"
	ag(t, dir, "branch", name, c1)
	ag(t, dir, "config", "core.sparseCheckout", "true")
	ag(t, dir, "update-index", "--assume-unchanged", "README.md") // 开着 sparse 时 git 会自动清掉 skip-worktree
	ag(t, dir, "config", "--add", "branch."+name+".mergeOptions", "--squash")
	wantOOS(t, dir, ffPlan(dir, name, c1, c2), "NFC", "含 =", "mergeoptions", "sparse-checkout", "assume-unchanged")
}

func TestScopeReadFailureIsOutOfScope(t *testing.T) {
	err := CheckScope(context.Background(), filepath.Join(t.TempDir(), "nonexistent"), nil, []string{"main"})
	var oos *ErrOutOfScope
	if !errors.As(err, &oos) || len(oos.Reasons) == 0 {
		t.Fatalf("读取失败应判超出: %v", err)
	}
	// 非仓库目录
	err = CheckScope(context.Background(), t.TempDir(), nil, []string{"main"})
	if !errors.As(err, &oos) {
		t.Fatalf("非仓库应判超出: %v", err)
	}
}

func TestCreateBranchOutOfScopeNoWrite(t *testing.T) {
	dir, c1, _ := ffRepo(t)
	before := snapshot(t, dir)
	err := CreateBranch(context.Background(), ApplyPlan{RepoDir: dir, Branch: "x=y", NewOID: c1, Kind: KindCreateOnOther})
	var oos *ErrOutOfScope
	if !errors.As(err, &oos) || snapshot(t, dir) != before {
		t.Fatalf("创建应被拦: %v", err)
	}
	_, err = AutoMergeCommit(context.Background(), ApplyPlan{RepoDir: dir, Branch: "x=y", Kind: KindAutoMerge, Parents: [2]string{c1, rev(t, dir, "feat")}})
	if !errors.As(err, &oos) {
		t.Fatalf("合并应被拦: %v", err)
	}
}

func TestParseGitVersion(t *testing.T) {
	cases := []struct {
		in       string
		ma, mi   int
		wantFail bool
	}{
		{"git version 2.43.0\n", 2, 43, false},
		{"git version 2.37.1", 2, 37, false},
		{"git version 2.39.3 (Apple Git-146)", 2, 39, false},
		{"git version 2.40", 2, 40, false},
		{"git version 2.44.0.windows.1", 2, 44, false},
		{"垃圾输出", 0, 0, true},
		{"git version abc", 0, 0, true},
		{"", 0, 0, true},
	}
	for _, c := range cases {
		ma, mi, err := parseGitVersion(c.in)
		if c.wantFail != (err != nil) || (!c.wantFail && (ma != c.ma || mi != c.mi)) {
			t.Errorf("%q => %d.%d %v", c.in, ma, mi, err)
		}
	}
}

func TestCleanEnv(t *testing.T) {
	got := cleanEnv([]string{"PATH=/bin", "GIT_DIR=/x", "HOME=/h", "GIT_CONFIG_COUNT=1", "GITHUB_TOKEN=a", "GIT_SSH_COMMAND=ssh"})
	want := []string{"PATH=/bin", "HOME=/h", "GITHUB_TOKEN=a"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%v", got)
	}
}

func TestNormalizationClashesAndMerge(t *testing.T) {
	merged := MergeBranchNames([]string{"main", nfcName, "功能"}, []string{"main", nfdName})
	if len(merged) != 4 {
		t.Fatalf("合并去重不对: %q", merged)
	}
	cl := NormalizationClashes(merged)
	if len(cl) != 1 || !((cl[0][0] == nfcName && cl[0][1] == nfdName) || (cl[0][0] == nfdName && cl[0][1] == nfcName)) {
		t.Fatalf("冲突对不对: %q", cl)
	}
	if len(NormalizationClashes([]string{"main", "功能/测试", "feat", "功能/测试"})) != 0 {
		t.Fatal("相同字节不算冲突")
	}
}

func TestScopeAnyUnsafeLocalBranchStopsRepo(t *testing.T) {
	dir, c1, c2 := ffRepo(t)
	ag(t, dir, "branch", "\u0100", c1)
	ag(t, dir, "branch", "A\u0304", c1)
	wantOOS(t, dir, ffPlan(dir, "main", c1, c2), "仓库里有无法确认规范化的分支名")

	dir, c1, c2 = ffRepo(t)
	ag(t, dir, "branch", "功能/测试", c1)
	if err := FastForwardBranch(context.Background(), ffPlan(dir, "main", c1, c2)); err != nil {
		t.Fatal(err)
	}
}

func TestAutoMergeBlockedByMergeInProgress(t *testing.T) {
	dir, c1, c2 := ffRepo(t)
	if err := os.WriteFile(filepath.Join(dir, ".git", "MERGE_HEAD"), []byte(c2+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := ag(t, dir, "count-objects", "-v")
	_, err := AutoMergeCommit(context.Background(), ApplyPlan{RepoDir: dir, Branch: "main", Kind: KindAutoMerge, Parents: [2]string{c1, c2}})
	if err == nil {
		t.Fatal("合并进行中应拒绝")
	}
	if after := ag(t, dir, "count-objects", "-v"); after != before {
		t.Fatalf("创建了新对象:\n%s\n%s", before, after)
	}
}
