package projsync

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// 所有测试只在 t.TempDir() 里新建临时仓库。

func ag(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s 失败: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func newRepo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ag(t, dir, "init", "-b", "main")
	ag(t, dir, "config", "user.name", "test")
	ag(t, dir, "config", "user.email", "t@t")
	ag(t, dir, "config", "commit.gpgsign", "false")
	commitFile(t, dir, "README.md", "base\n")
	return dir
}

// commitFile 写文件并提交，返回新 HEAD。
func commitFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	ag(t, dir, "add", name)
	ag(t, dir, "commit", "-m", "c "+name)
	return ag(t, dir, "rev-parse", "HEAD")
}

// snapshot 记录：全部引用、索引、状态、工作区文件内容（不含 .git）。
func snapshot(t *testing.T, dir string) string {
	t.Helper()
	var sb strings.Builder
	sb.WriteString(ag(t, dir, "for-each-ref", "--format=%(refname) %(objectname)"))
	symOut, _ := exec.Command("git", "-C", dir, "symbolic-ref", "-q", "HEAD").Output()
	sb.WriteString("\nHEAD:" + string(symOut))
	headOut, _ := exec.Command("git", "-C", dir, "rev-parse", "-q", "--verify", "HEAD").Output()
	sb.WriteString(string(headOut))
	sb.WriteString("\nINDEX:" + ag(t, dir, "ls-files", "-s"))
	sb.WriteString("\nSTATUS:" + ag(t, dir, "status", "--porcelain", "-uall", "--ignored"))
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if !d.IsDir() {
			b, _ := os.ReadFile(p)
			sb.WriteString("\nF:" + p + "=" + string(b))
		}
		return nil
	})
	return sb.String()
}

func rev(t *testing.T, dir, ref string) string { return ag(t, dir, "rev-parse", ref) }

func ffPlan(dir, branch, old, new string) ApplyPlan {
	return ApplyPlan{RepoDir: dir, Branch: branch, OldOID: old, NewOID: new, Kind: KindFastForward, FactsVersion: 7}
}

// ffRepo: main 在 c1，feat 领先一个提交 c2；返回 dir, c1, c2。
func ffRepo(t *testing.T) (string, string, string) {
	dir := newRepo(t)
	c1 := rev(t, dir, "HEAD")
	ag(t, dir, "branch", "feat")
	ag(t, dir, "checkout", "-q", "feat")
	c2 := commitFile(t, dir, "new.txt", "new\n")
	ag(t, dir, "checkout", "-q", "main")
	return dir, c1, c2
}

func wantRefuse(t *testing.T, err error, dirs ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("期望拒绝，却成功了")
	}
	if !strings.Contains(err.Error(), "：") {
		t.Fatalf("错误缺少中文原因: %v", err)
	}
}

func TestCreateBranch(t *testing.T) {
	ctx := context.Background()
	dir, c1, c2 := ffRepo(t)
	p := ApplyPlan{RepoDir: dir, Branch: "newb", NewOID: c2, Kind: KindCreateOnOther}
	if err := CreateBranch(ctx, p); err != nil {
		t.Fatal(err)
	}
	if rev(t, dir, "refs/heads/newb") != c2 {
		t.Fatal("分支值不对")
	}
	if ag(t, dir, "symbolic-ref", "HEAD") != "refs/heads/main" || rev(t, dir, "HEAD") != c1 {
		t.Fatal("HEAD 被改动")
	}

	// 目标已存在 → 拒绝，原分支不变
	before := snapshot(t, dir)
	p.NewOID = c1
	wantRefuse(t, CreateBranch(ctx, p))
	if snapshot(t, dir) != before || rev(t, dir, "refs/heads/newb") != c2 {
		t.Fatal("拒绝后现场变了")
	}
}

func TestCreateBranchRejectedByWorktreeHead(t *testing.T) {
	ctx := context.Background()
	// unborn：orphan 检出一个尚无提交的分支名
	dir, _, c2 := ffRepo(t)
	ag(t, dir, "checkout", "-q", "--orphan", "target")
	ag(t, dir, "reset", "-q") // 仅测试仓库内清索引使其干净
	before := snapshot(t, dir)
	wantRefuse(t, CreateBranch(ctx, ApplyPlan{RepoDir: dir, Branch: "target", NewOID: c2, Kind: KindCreateOnOther}))
	if snapshot(t, dir) != before {
		t.Fatal("拒绝后现场变了")
	}

	// linked worktree 的 HEAD 指向该名，但引用被删（引用缺失仍被 symbolic HEAD 指向）
	dir2, _, d2 := ffRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	ag(t, dir2, "worktree", "add", "-q", "-b", "gone", wt)
	ag(t, dir2, "update-ref", "-d", "refs/heads/gone")
	before = snapshot(t, dir2)
	wantRefuse(t, CreateBranch(ctx, ApplyPlan{RepoDir: dir2, Branch: "gone", NewOID: d2, Kind: KindCreateOnOther}))
	if snapshot(t, dir2) != before {
		t.Fatal("拒绝后现场变了")
	}
}

func TestFastForwardUnchecked(t *testing.T) {
	ctx := context.Background()
	dir, c1, c2 := ffRepo(t)
	ag(t, dir, "checkout", "-q", "--detach")
	headBefore := rev(t, dir, "HEAD")
	if err := FastForwardBranch(ctx, ffPlan(dir, "main", c1, c2)); err != nil {
		t.Fatal(err)
	}
	if rev(t, dir, "refs/heads/main") != c2 || rev(t, dir, "HEAD") != headBefore {
		t.Fatal("未检出分支快进结果不对")
	}
}

func TestFastForwardRefusals(t *testing.T) {
	ctx := context.Background()

	t.Run("OldOID不符", func(t *testing.T) {
		dir, c1, c2 := ffRepo(t)
		ag(t, dir, "checkout", "-q", "--detach")
		other := commitFile(t, dir, "x.txt", "x\n")
		ag(t, dir, "update-ref", "refs/heads/main", other)
		before := snapshot(t, dir)
		err := FastForwardBranch(ctx, ffPlan(dir, "main", c1, c2))
		wantRefuse(t, err)
		if !strings.Contains(err.Error(), "引用已变化") || snapshot(t, dir) != before {
			t.Fatalf("现场变了或原因不对: %v", err)
		}
	})
	t.Run("非后代", func(t *testing.T) {
		dir, c1, c2 := ffRepo(t)
		ag(t, dir, "checkout", "-q", "-b", "div")
		d := commitFile(t, dir, "d.txt", "d\n")
		ag(t, dir, "checkout", "-q", "main")
		before := snapshot(t, dir)
		// feat(c2) 与 div(d) 分叉：d 不是 c2 的后代
		wantRefuse(t, FastForwardBranch(ctx, ffPlan(dir, "feat", c2, d)))
		// 方向反了：c1 是 d 的祖先，不是后代
		wantRefuse(t, FastForwardBranch(ctx, ffPlan(dir, "div", d, c1)))
		if snapshot(t, dir) != before {
			t.Fatal("现场变了")
		}
		_ = c1
	})
	t.Run("被检出有未提交改动", func(t *testing.T) {
		dir, c1, c2 := ffRepo(t)
		os.WriteFile(filepath.Join(dir, "README.md"), []byte("dirty\n"), 0o644)
		before := snapshot(t, dir)
		wantRefuse(t, FastForwardBranch(ctx, ffPlan(dir, "main", c1, c2)))
		if snapshot(t, dir) != before {
			t.Fatal("改动没原样保留")
		}
	})
	t.Run("已暂存的改动", func(t *testing.T) {
		dir, c1, c2 := ffRepo(t)
		os.WriteFile(filepath.Join(dir, "README.md"), []byte("staged\n"), 0o644)
		ag(t, dir, "add", "README.md")
		before := snapshot(t, dir)
		wantRefuse(t, FastForwardBranch(ctx, ffPlan(dir, "main", c1, c2)))
		if snapshot(t, dir) != before {
			t.Fatal("索引/文件变了")
		}
	})
	t.Run("被检出有未跟踪文件", func(t *testing.T) {
		dir, c1, c2 := ffRepo(t)
		os.WriteFile(filepath.Join(dir, "new.txt"), []byte("mine\n"), 0o644) // 与新提交同名
		before := snapshot(t, dir)
		wantRefuse(t, FastForwardBranch(ctx, ffPlan(dir, "main", c1, c2)))
		if snapshot(t, dir) != before {
			t.Fatal("未跟踪文件没原样保留")
		}
	})
	t.Run("被忽略的同名文件", func(t *testing.T) {
		dir := newRepo(t)
		os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("*.log\n"), 0o644)
		ag(t, dir, "add", ".gitignore")
		ag(t, dir, "commit", "-q", "-m", "ignore")
		c1 := rev(t, dir, "HEAD")
		ag(t, dir, "checkout", "-q", "-b", "feat")
		os.WriteFile(filepath.Join(dir, "x.log"), []byte("theirs\n"), 0o644)
		ag(t, dir, "add", "-f", "x.log")
		ag(t, dir, "commit", "-q", "-m", "log")
		c2 := rev(t, dir, "HEAD")
		ag(t, dir, "checkout", "-q", "main")
		os.WriteFile(filepath.Join(dir, "x.log"), []byte("mine\n"), 0o644)
		before := snapshot(t, dir)
		wantRefuse(t, FastForwardBranch(ctx, ffPlan(dir, "main", c1, c2)))
		if snapshot(t, dir) != before {
			t.Fatal("被忽略文件没原样保留")
		}
	})
	t.Run("merge进行中", func(t *testing.T) {
		dir, c1, c2 := ffRepo(t)
		gd := ag(t, dir, "rev-parse", "--absolute-git-dir")
		os.WriteFile(filepath.Join(gd, "MERGE_HEAD"), []byte(c2+"\n"), 0o644)
		before := snapshot(t, dir)
		wantRefuse(t, FastForwardBranch(ctx, ffPlan(dir, "main", c1, c2)))
		if snapshot(t, dir) != before {
			t.Fatal("现场变了")
		}
	})
	t.Run("rebase进行中", func(t *testing.T) {
		dir, c1, c2 := ffRepo(t)
		gd := ag(t, dir, "rev-parse", "--absolute-git-dir")
		os.MkdirAll(filepath.Join(gd, "rebase-merge"), 0o755)
		before := snapshot(t, dir)
		wantRefuse(t, FastForwardBranch(ctx, ffPlan(dir, "main", c1, c2)))
		if snapshot(t, dir) != before {
			t.Fatal("现场变了")
		}
	})
	t.Run("两个工作区检出同一分支", func(t *testing.T) {
		dir, c1, c2 := ffRepo(t)
		wt := filepath.Join(t.TempDir(), "wt2")
		ag(t, dir, "worktree", "add", "-q", "--force", wt, "main")
		before, beforeWt := snapshot(t, dir), snapshot(t, wt)
		wantRefuse(t, FastForwardBranch(ctx, ffPlan(dir, "main", c1, c2)))
		if snapshot(t, dir) != before || snapshot(t, wt) != beforeWt {
			t.Fatal("现场变了")
		}
	})
	t.Run("校验后写入前切了分支", func(t *testing.T) {
		dir, c1, c2 := ffRepo(t)
		ag(t, dir, "branch", "other", c1)
		applyTestHook = func(stage string) {
			if stage == "beforeMerge" {
				ag(t, dir, "checkout", "-q", "other")
			}
		}
		defer func() { applyTestHook = nil }()
		err := FastForwardBranch(ctx, ffPlan(dir, "main", c1, c2))
		wantRefuse(t, err)
		if rev(t, dir, "refs/heads/main") != c1 || rev(t, dir, "refs/heads/other") != c1 || rev(t, dir, "HEAD") != c1 {
			t.Fatal("引用被动了")
		}
		if _, e := os.Stat(filepath.Join(dir, "new.txt")); e == nil {
			t.Fatal("文件被写入")
		}
	})
	t.Run("校验后写入前HEAD前进", func(t *testing.T) {
		dir, c1, c2 := ffRepo(t)
		applyTestHook = func(string) { commitFile(t, dir, "late.txt", "late\n") }
		defer func() { applyTestHook = nil }()
		wantRefuse(t, FastForwardBranch(ctx, ffPlan(dir, "main", c1, c2)))
		if _, e := os.Stat(filepath.Join(dir, "new.txt")); e == nil {
			t.Fatal("文件被写入")
		}
	})
}

func TestFastForwardCheckedOut(t *testing.T) {
	ctx := context.Background()
	dir, c1, c2 := ffRepo(t)
	if err := FastForwardBranch(ctx, ffPlan(dir, "main", c1, c2)); err != nil {
		t.Fatal(err)
	}
	if rev(t, dir, "HEAD") != c2 || rev(t, dir, "refs/heads/main") != c2 {
		t.Fatal("没快进")
	}
	if b, err := os.ReadFile(filepath.Join(dir, "new.txt")); err != nil || string(b) != "new\n" {
		t.Fatal("文件没更新")
	}
}

func TestFastForwardLinkedWorktree(t *testing.T) {
	ctx := context.Background()
	dir, c1, c2 := ffRepo(t)
	ag(t, dir, "branch", "lw", c1)
	wt := filepath.Join(t.TempDir(), "wt")
	ag(t, dir, "worktree", "add", "-q", wt, "lw")

	wts, err := listWorktrees(ctx, dir, nil)
	if err != nil || len(wts) != 2 {
		t.Fatalf("工作区列表不对: %v %+v", err, wts)
	}
	h := checkedOutBy(wts, "lw")
	if len(h) != 1 || h[0].Unborn || h[0].Detached {
		t.Fatalf("识别 lw 检出失败: %+v", h)
	}

	// 从主工作区路径调用，linked 里 merge；先验证脏文件会拒绝
	os.WriteFile(filepath.Join(wt, "junk.txt"), []byte("j"), 0o644)
	wantRefuse(t, FastForwardBranch(ctx, ffPlan(dir, "lw", c1, c2)))
	os.Remove(filepath.Join(wt, "junk.txt"))

	// linked worktree 里 merge 中也要识别（git-dir 在 .git/worktrees/ 下）
	gd := ag(t, wt, "rev-parse", "--absolute-git-dir")
	os.WriteFile(filepath.Join(gd, "MERGE_HEAD"), []byte(c2+"\n"), 0o644)
	wantRefuse(t, FastForwardBranch(ctx, ffPlan(dir, "lw", c1, c2)))
	os.Remove(filepath.Join(gd, "MERGE_HEAD"))

	if err := FastForwardBranch(ctx, ffPlan(dir, "lw", c1, c2)); err != nil {
		t.Fatal(err)
	}
	if rev(t, wt, "HEAD") != c2 || rev(t, dir, "refs/heads/lw") != c2 {
		t.Fatal("linked 快进失败")
	}
	if _, err := os.Stat(filepath.Join(wt, "new.txt")); err != nil {
		t.Fatal("linked 文件没更新")
	}
	// 主工作区未受影响
	if rev(t, dir, "HEAD") != c1 {
		t.Fatal("主工作区被改")
	}
}

func TestAutoMergeCommit(t *testing.T) {
	ctx := context.Background()
	dir := newRepo(t)
	base := rev(t, dir, "HEAD")
	ag(t, dir, "checkout", "-q", "-b", "a")
	a := commitFile(t, dir, "a.txt", "a\n")
	ag(t, dir, "checkout", "-q", "-b", "b", base)
	b := commitFile(t, dir, "b.txt", "b\n")
	ag(t, dir, "checkout", "-q", "main")
	before := snapshot(t, dir)

	p := ApplyPlan{RepoDir: dir, Branch: "main", Kind: KindAutoMerge, Parents: [2]string{a, b}}
	m, err := AutoMergeCommit(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot(t, dir) != before {
		t.Fatal("工作区/引用/索引被改动")
	}
	parents := strings.Fields(ag(t, dir, "rev-list", "--parents", "-n1", m))
	if len(parents) != 3 || parents[1] != a || parents[2] != b {
		t.Fatalf("父提交不对: %v", parents)
	}
	for _, x := range []string{a, b} {
		if ok, err := isAncestor(ctx, dir, nil, x, m); err != nil || !ok {
			t.Fatal("父不是祖先")
		}
	}
	if !strings.Contains(ag(t, dir, "log", "-1", "--format=%s", m), "XXYI 自动合并") {
		t.Fatal("提交信息不对")
	}
	// 两边分别快进到合并提交
	if err := FastForwardBranch(ctx, ApplyPlan{RepoDir: dir, Branch: "a", OldOID: a, NewOID: m, Kind: KindFastForward}); err != nil {
		t.Fatal(err)
	}
	if err := FastForwardBranch(ctx, ApplyPlan{RepoDir: dir, Branch: "b", OldOID: b, NewOID: m, Kind: KindFastForward}); err != nil {
		t.Fatal(err)
	}
}

func TestAutoMergeConflict(t *testing.T) {
	ctx := context.Background()
	dir := newRepo(t)
	base := rev(t, dir, "HEAD")
	ag(t, dir, "checkout", "-q", "-b", "a")
	a := commitFile(t, dir, "README.md", "a side\n")
	ag(t, dir, "checkout", "-q", "-b", "b", base)
	b := commitFile(t, dir, "README.md", "b side\n")
	ag(t, dir, "checkout", "-q", "main")
	before := snapshot(t, dir)
	allBefore := ag(t, dir, "rev-list", "--all", "--count")
	_, err := AutoMergeCommit(ctx, ApplyPlan{RepoDir: dir, Branch: "main", Kind: KindAutoMerge, Parents: [2]string{a, b}})
	wantRefuse(t, err)
	if !strings.Contains(err.Error(), "冲突") {
		t.Fatalf("原因应提冲突: %v", err)
	}
	if snapshot(t, dir) != before || ag(t, dir, "rev-list", "--all", "--count") != allBefore {
		t.Fatal("现场变了")
	}
}

func TestReadFailures(t *testing.T) {
	ctx := context.Background()
	missing := filepath.Join(t.TempDir(), "nope")
	if _, _, err := readRef(ctx, missing, nil, "refs/heads/main"); err == nil {
		t.Fatal("不存在的目录应报错")
	}
	notRepo := t.TempDir()
	if _, _, err := readRef(ctx, notRepo, nil, "refs/heads/main"); err == nil {
		t.Fatal("非仓库应报错")
	}
	dir, c1, _ := ffRepo(t)
	// 引用不存在：不是错误，且前缀不能误匹配
	if _, ex, err := readRef(ctx, dir, nil, "refs/heads/fe"); err != nil || ex {
		t.Fatalf("不存在应 (false,nil): %v %v", ex, err)
	}
	if o, ex, err := readRef(ctx, dir, nil, "refs/heads/main"); err != nil || !ex || o != c1 {
		t.Fatal("读取已有引用失败")
	}
	if _, err := isAncestor(ctx, missing, nil, c1, c1); err == nil {
		t.Fatal("不存在目录的祖先检测应报错")
	}
	ghost := strings.Repeat("a", 40)
	if _, err := isAncestor(ctx, dir, nil, ghost, c1); err == nil {
		t.Fatal("对象缺失应报错而不是 false")
	}
	if _, err := isAncestor(ctx, dir, nil, "garbage", c1); err == nil {
		t.Fatal("损坏 OID 应报错")
	}

	// 写函数遇读取失败：不写
	before := snapshot(t, dir)
	wantRefuse(t, FastForwardBranch(ctx, ffPlan(dir, "main", c1, ghost)))
	wantRefuse(t, FastForwardBranch(ctx, ffPlan(dir, "main", c1, "garbage")))
	wantRefuse(t, FastForwardBranch(ctx, ffPlan(missing, "main", c1, c1)))
	wantRefuse(t, CreateBranch(ctx, ApplyPlan{RepoDir: dir, Branch: "x", NewOID: ghost, Kind: KindCreateOnOther}))
	wantRefuse(t, CreateBranch(ctx, ApplyPlan{RepoDir: missing, Branch: "x", NewOID: c1, Kind: KindCreateOnOther}))
	_, err := AutoMergeCommit(ctx, ApplyPlan{RepoDir: dir, Branch: "main", Kind: KindAutoMerge, Parents: [2]string{c1, ghost}})
	wantRefuse(t, err)
	if snapshot(t, dir) != before {
		t.Fatal("读取失败后现场变了")
	}
	if _, ex, _ := readRef(ctx, dir, nil, "refs/heads/x"); ex {
		t.Fatal("读取失败路径创建了分支")
	}
}

func TestListWorktreesDetachedAndMismatchedKind(t *testing.T) {
	ctx := context.Background()
	dir, c1, c2 := ffRepo(t)
	ag(t, dir, "checkout", "-q", "--detach")
	wts, err := listWorktrees(ctx, dir, nil)
	if err != nil || len(wts) != 1 || !wts[0].Detached || wts[0].Branch != "" {
		t.Fatalf("detached 识别失败: %v %+v", err, wts)
	}
	// Kind 不符拒绝
	p := ffPlan(dir, "main", c1, c2)
	p.Kind = KindCreateOnOther
	wantRefuse(t, FastForwardBranch(ctx, p))
}
