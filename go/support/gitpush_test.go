package support

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitOK 在 dir 里跑 git，失败即 t.Fatal，返回 TrimSpace 后的合并输出。
func gitOK(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git -C %s %v: %v\n%s", dir, args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// setupMacUbuntu 建 mac / ubuntu 两个普通仓库（ubuntu 非裸、检出 master 且有初始提交）。
func setupMacUbuntu(t *testing.T) (mac, ubuntu string) {
	t.Helper()
	mac, ubuntu = t.TempDir(), t.TempDir()
	for _, d := range []string{mac, ubuntu} {
		if out, err := exec.Command("git", "-c", "init.defaultBranch=master", "init", d).CombinedOutput(); err != nil {
			t.Fatalf("git init %s: %v\n%s", d, err, out)
		}
		gitOK(t, d, "config", "user.name", "Test User")
		gitOK(t, d, "config", "user.email", "test@example.com")
	}
	// ubuntu 的 master 上要有初始提交，并检出在 master
	makeCommit(t, ubuntu, "ubuntu-base")
	return mac, ubuntu
}

// runSh 用 sh -c 执行脚本，返回输出和退出码。
func runSh(t *testing.T, script string) ([]byte, int) {
	t.Helper()
	out, err := exec.Command("sh", "-c", script).CombinedOutput()
	if err == nil {
		return out, 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return out, ee.ExitCode()
	}
	t.Fatalf("sh 执行失败: %v\n%s", err, out)
	return nil, -1
}

// stageAndWant 把 mac 的 branch 推过去，返回提交号。
func stageAndWant(t *testing.T, mac, ubuntu, branch string) string {
	t.Helper()
	commit, err := PushRef(context.Background(), Repo{Dir: mac, Remote: ubuntu}, branch, nil)
	if err != nil {
		t.Fatalf("PushRef: %v", err)
	}
	return commit
}

func TestPushRef_stagingRef(t *testing.T) {
	mac, ubuntu := setupMacUbuntu(t)
	gitOK(t, mac, "checkout", "-b", "feat/x")
	makeCommit(t, mac, "x1")

	c1 := stageAndWant(t, mac, ubuntu, "feat/x")
	if got := gitOK(t, ubuntu, "rev-parse", "refs/xxyi/mac/feat/x"); got != c1 {
		t.Fatalf("暂存引用 = %s，期望 %s", got, c1)
	}
	if len(c1) != 40 {
		t.Fatalf("返回提交号不是 40 位: %q", c1)
	}

	// 再推一次（mac 多一个提交），暂存引用被覆盖
	makeCommit(t, mac, "x2")
	c2 := stageAndWant(t, mac, ubuntu, "feat/x")
	if c2 == c1 {
		t.Fatal("第二次推的提交号没变")
	}
	if got := gitOK(t, ubuntu, "rev-parse", "refs/xxyi/mac/feat/x"); got != c2 {
		t.Fatalf("覆盖后暂存引用 = %s，期望 %s", got, c2)
	}
	// ubuntu 的 refs/heads/feat/x 不受 push 影响（本来就不该动）
	if _, _, err := runGitCode(context.Background(), Repo{Dir: ubuntu}, nil,
		"rev-parse", "--verify", "-q", "refs/heads/feat/x"); err == nil {
		t.Fatal("push 不应创建 refs/heads/feat/x")
	}
}

func TestPushRef_badInputs(t *testing.T) {
	mac, ubuntu := setupMacUbuntu(t)
	ctx := context.Background()
	if _, err := PushRef(ctx, Repo{Dir: mac, Remote: ubuntu}, "bad branch", nil); err == nil {
		t.Error("非法分支名应报错")
	}
	if _, err := PushRef(ctx, Repo{Dir: mac, Remote: ""}, "master", nil); err == nil {
		t.Error("空 Remote 应报错")
	}
	if _, err := PushRef(ctx, Repo{Dir: mac, Remote: "-x"}, "master", nil); err == nil {
		t.Error("Remote 以 - 开头应报错")
	}
	// mac 上没有这个分支
	if _, err := PushRef(ctx, Repo{Dir: mac, Remote: ubuntu}, "no-such-branch", nil); err == nil {
		t.Error("分支不存在应报错")
	}
}

func TestFFScript_createBranch(t *testing.T) {
	mac, ubuntu := setupMacUbuntu(t)
	gitOK(t, mac, "checkout", "-b", "feat/x")
	makeCommit(t, mac, "x1")
	want := stageAndWant(t, mac, ubuntu, "feat/x")

	script, err := FFScript(ubuntu, "feat/x", want)
	if err != nil {
		t.Fatalf("FFScript: %v", err)
	}
	out, code := runSh(t, script)
	got, err := ParseFFResult(out, code)
	if err != nil {
		t.Fatalf("ParseFFResult: %v (out=%q code=%d)", err, out, code)
	}
	if got != want {
		t.Fatalf("OK 提交号 = %s，期望 %s", got, want)
	}
	if h := gitOK(t, ubuntu, "rev-parse", "refs/heads/feat/x"); h != want {
		t.Fatalf("refs/heads/feat/x = %s，期望 %s", h, want)
	}
	// 不该把 HEAD 切过去
	if b := currentBranch(t, ubuntu); b != "master" {
		t.Fatalf("HEAD 被切到了 %s", b)
	}
}

func TestFFScript_otherBranchFF(t *testing.T) {
	mac, ubuntu := setupMacUbuntu(t)
	gitOK(t, mac, "checkout", "-b", "feat/x")
	makeCommit(t, mac, "x1")
	c1 := stageAndWant(t, mac, ubuntu, "feat/x")
	// 先建出分支
	script, _ := FFScript(ubuntu, "feat/x", c1)
	out, code := runSh(t, script)
	if _, err := ParseFFResult(out, code); err != nil {
		t.Fatalf("首次 FF: %v (out=%q code=%d)", err, out, code)
	}

	// ubuntu 仍检出 master，feat/x 能快进
	makeCommit(t, mac, "x2")
	c2 := stageAndWant(t, mac, ubuntu, "feat/x")
	script, _ = FFScript(ubuntu, "feat/x", c2)
	out, code = runSh(t, script)
	got, err := ParseFFResult(out, code)
	if err != nil {
		t.Fatalf("ParseFFResult: %v (out=%q code=%d)", err, out, code)
	}
	if got != c2 {
		t.Fatalf("OK 提交号 = %s，期望 %s", got, c2)
	}
	if h := gitOK(t, ubuntu, "rev-parse", "refs/heads/feat/x"); h != c2 {
		t.Fatalf("refs/heads/feat/x = %s，期望 %s", h, c2)
	}
	if b := currentBranch(t, ubuntu); b != "master" {
		t.Fatalf("HEAD 被切到了 %s", b)
	}
}

func TestFFScript_currentBranchClean(t *testing.T) {
	mac, ubuntu := setupMacUbuntu(t)
	gitOK(t, mac, "checkout", "-b", "feat/x")
	makeCommit(t, mac, "x1")
	c1 := stageAndWant(t, mac, ubuntu, "feat/x")
	script, _ := FFScript(ubuntu, "feat/x", c1)
	runSh(t, script) // 建出分支
	gitOK(t, ubuntu, "checkout", "feat/x")

	// 当前检出 feat/x 且干净：merge 快进，工作区文件更新
	makeCommit(t, mac, "x2")
	c2 := stageAndWant(t, mac, ubuntu, "feat/x")
	script, _ = FFScript(ubuntu, "feat/x", c2)
	out, code := runSh(t, script)
	got, err := ParseFFResult(out, code)
	if err != nil {
		t.Fatalf("ParseFFResult: %v (out=%q code=%d)", err, out, code)
	}
	if got != c2 {
		t.Fatalf("OK 提交号 = %s，期望 %s", got, c2)
	}
	body, err := os.ReadFile(filepath.Join(ubuntu, "x2"))
	if err != nil || string(body) != "x2" {
		t.Fatalf("工作区文件没更新: %q %v", body, err)
	}
}

func TestFFScript_currentBranchDirty(t *testing.T) {
	mac, ubuntu := setupMacUbuntu(t)
	gitOK(t, mac, "checkout", "-b", "feat/x")
	makeCommit(t, mac, "x1")
	c1 := stageAndWant(t, mac, ubuntu, "feat/x")
	script, _ := FFScript(ubuntu, "feat/x", c1)
	runSh(t, script)
	gitOK(t, ubuntu, "checkout", "feat/x")

	// 工作区弄脏
	if err := os.WriteFile(filepath.Join(ubuntu, "x1"), []byte("dirty"), 0644); err != nil {
		t.Fatal(err)
	}
	makeCommit(t, mac, "x2")
	c2 := stageAndWant(t, mac, ubuntu, "feat/x")
	script, _ = FFScript(ubuntu, "feat/x", c2)
	out, code := runSh(t, script)
	if _, err := ParseFFResult(out, code); !errors.Is(err, ErrDirty) {
		t.Fatalf("期望 ErrDirty，得到 %v (out=%q code=%d)", err, out, code)
	}
	if code != 5 {
		t.Fatalf("退出码 = %d，期望 5", code)
	}
	if h := gitOK(t, ubuntu, "rev-parse", "refs/heads/feat/x"); h != c1 {
		t.Fatalf("分支被移动了: %s → %s", c1, h)
	}
}

func TestFFScript_untrackedDoesNotBlock(t *testing.T) {
	mac, ubuntu := setupMacUbuntu(t)
	gitOK(t, mac, "checkout", "-b", "feat/x")
	makeCommit(t, mac, "x1")
	c1 := stageAndWant(t, mac, ubuntu, "feat/x")
	script, _ := FFScript(ubuntu, "feat/x", c1)
	runSh(t, script)
	gitOK(t, ubuntu, "checkout", "feat/x")

	// 当前检出该分支，只有未跟踪文件：不挡快进
	if err := os.WriteFile(filepath.Join(ubuntu, "scratch.txt"), []byte("scratch"), 0644); err != nil {
		t.Fatal(err)
	}
	makeCommit(t, mac, "x2")
	c2 := stageAndWant(t, mac, ubuntu, "feat/x")
	script, _ = FFScript(ubuntu, "feat/x", c2)
	out, code := runSh(t, script)
	got, err := ParseFFResult(out, code)
	if err != nil {
		t.Fatalf("只有未跟踪文件应能快进: %v (out=%q code=%d)", err, out, code)
	}
	if got != c2 {
		t.Fatalf("OK 提交号 = %s，期望 %s", got, c2)
	}
}

func TestFFScript_alreadyConsistent(t *testing.T) {
	mac, ubuntu := setupMacUbuntu(t)
	gitOK(t, mac, "checkout", "-b", "feat/x")
	makeCommit(t, mac, "x1")
	c1 := stageAndWant(t, mac, ubuntu, "feat/x")
	script, _ := FFScript(ubuntu, "feat/x", c1)
	out, code := runSh(t, script)
	if _, err := ParseFFResult(out, code); err != nil {
		t.Fatalf("首次 FF: %v (out=%q code=%d)", err, out, code)
	}

	// 工作区把已跟踪文件弄脏，再跑一次同样的 want：已一致 → 直接 OK，分支不动
	if err := os.WriteFile(filepath.Join(ubuntu, "x1"), []byte("local"), 0644); err != nil {
		t.Fatal(err)
	}
	c1again := stageAndWant(t, mac, ubuntu, "feat/x") // mac 没动，重新摆好暂存引用
	if c1again != c1 {
		t.Fatalf("重推提交号变了: %s → %s", c1, c1again)
	}
	script, _ = FFScript(ubuntu, "feat/x", c1)
	out, code = runSh(t, script)
	got, err := ParseFFResult(out, code)
	if err != nil {
		t.Fatalf("已一致应报 OK: %v (out=%q code=%d)", err, out, code)
	}
	if got != c1 {
		t.Fatalf("OK 提交号 = %s，期望 %s", got, c1)
	}
	if h := gitOK(t, ubuntu, "rev-parse", "refs/heads/feat/x"); h != c1 {
		t.Fatalf("分支被动了: %s → %s", c1, h)
	}
	// 本地改动必须原样保留（没被 merge/reset 抹掉）
	body, err := os.ReadFile(filepath.Join(ubuntu, "x1"))
	if err != nil || string(body) != "local" {
		t.Fatalf("本地改动被覆盖: %q %v", body, err)
	}
}

func TestFFScript_noRepoDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "not-a-repo")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	script, err := FFScript(dir, "feat/x", strings.Repeat("a", 40))
	if err != nil {
		t.Fatalf("FFScript: %v", err)
	}
	out, code := runSh(t, script)
	if _, err := ParseFFResult(out, code); !errors.Is(err, ErrNoRepoDir) {
		t.Fatalf("期望 ErrNoRepoDir，得到 %v (out=%q code=%d)", err, out, code)
	}
	if code != 6 {
		t.Fatalf("退出码 = %d，期望 6", code)
	}
}

func TestFFScript_busyWorktree(t *testing.T) {
	mac, ubuntu := setupMacUbuntu(t)
	gitOK(t, mac, "checkout", "-b", "feat/x")
	makeCommit(t, mac, "x1")
	c1 := stageAndWant(t, mac, ubuntu, "feat/x")
	script, _ := FFScript(ubuntu, "feat/x", c1)
	out, code := runSh(t, script)
	if _, err := ParseFFResult(out, code); err != nil {
		t.Fatalf("首次 FF: %v (out=%q code=%d)", err, out, code)
	}

	// 另一个工作区检出 feat/x：ubuntu 主工作区仍在 master，但分支被别人占了
	wt := filepath.Join(t.TempDir(), "wt")
	gitOK(t, ubuntu, "worktree", "add", wt, "feat/x")

	makeCommit(t, mac, "x2")
	c2 := stageAndWant(t, mac, ubuntu, "feat/x")
	script, _ = FFScript(ubuntu, "feat/x", c2)
	out, code = runSh(t, script)
	if _, err := ParseFFResult(out, code); !errors.Is(err, ErrRepoBusy) {
		t.Fatalf("期望 ErrRepoBusy，得到 %v (out=%q code=%d)", err, out, code)
	}
	if code != 7 {
		t.Fatalf("退出码 = %d，期望 7", code)
	}
	if h := gitOK(t, ubuntu, "rev-parse", "refs/heads/feat/x"); h != c1 {
		t.Fatalf("忙时分支被移动了: %s → %s", c1, h)
	}
}

func TestFFScript_busyRebase(t *testing.T) {
	mac, ubuntu := setupMacUbuntu(t)
	// mac 的 feat/x 上有一个碰 conf 文件的提交
	gitOK(t, mac, "checkout", "-b", "feat/x")
	if err := os.WriteFile(filepath.Join(mac, "conf"), []byte("mac"), 0644); err != nil {
		t.Fatal(err)
	}
	gitOK(t, mac, "add", "conf")
	gitOK(t, mac, "commit", "-m", "mac-conf")
	c1 := stageAndWant(t, mac, ubuntu, "feat/x")
	script, _ := FFScript(ubuntu, "feat/x", c1)
	out, code := runSh(t, script)
	if _, err := ParseFFResult(out, code); err != nil {
		t.Fatalf("首次 FF: %v (out=%q code=%d)", err, out, code)
	}
	// ubuntu 的 master 上同名文件不同内容：rebase feat/x 必冲突，rebase 停在半途
	if err := os.WriteFile(filepath.Join(ubuntu, "conf"), []byte("ubuntu"), 0644); err != nil {
		t.Fatal(err)
	}
	gitOK(t, ubuntu, "add", "conf")
	gitOK(t, ubuntu, "commit", "-m", "ubuntu-conf")
	gitOK(t, ubuntu, "checkout", "feat/x")
	if out, err := exec.Command("git", "-C", ubuntu, "rebase", "master").CombinedOutput(); err == nil {
		t.Fatalf("rebase 应冲突停下: %s", out)
	}

	makeCommit(t, mac, "x2")
	c2 := stageAndWant(t, mac, ubuntu, "feat/x")
	script, _ = FFScript(ubuntu, "feat/x", c2)
	out, code = runSh(t, script)
	if _, err := ParseFFResult(out, code); !errors.Is(err, ErrRepoBusy) {
		t.Fatalf("期望 ErrRepoBusy，得到 %v (out=%q code=%d)", err, out, code)
	}
	if code != 7 {
		t.Fatalf("退出码 = %d，期望 7", code)
	}
}

func TestFFScript_stagingRefRemoved(t *testing.T) {
	mac, ubuntu := setupMacUbuntu(t)
	gitOK(t, mac, "checkout", "-b", "feat/x")
	makeCommit(t, mac, "x1")
	c1 := stageAndWant(t, mac, ubuntu, "feat/x")
	script, _ := FFScript(ubuntu, "feat/x", c1)
	out, code := runSh(t, script)
	if _, err := ParseFFResult(out, code); err != nil {
		t.Fatalf("首次 FF: %v (out=%q code=%d)", err, out, code)
	}
	// 成功快进后暂存引用应被删掉
	if out, err := exec.Command("git", "-C", ubuntu, "rev-parse", "--verify", "-q", "refs/xxyi/mac/feat/x").CombinedOutput(); err == nil {
		t.Fatalf("成功后暂存引用还在: %s", out)
	}
}

func TestFFScript_notFastForward(t *testing.T) {
	mac, ubuntu := setupMacUbuntu(t)
	gitOK(t, mac, "checkout", "-b", "feat/x")
	makeCommit(t, mac, "x1")
	c1 := stageAndWant(t, mac, ubuntu, "feat/x")
	script, _ := FFScript(ubuntu, "feat/x", c1)
	runSh(t, script)

	// ubuntu 在 feat/x 上有自己的独有提交
	gitOK(t, ubuntu, "checkout", "feat/x")
	makeCommit(t, ubuntu, "ubuntu-only")

	// mac 前进一个提交（与 ubuntu 独有提交分叉）
	makeCommit(t, mac, "x2")
	c2 := stageAndWant(t, mac, ubuntu, "feat/x")
	script, _ = FFScript(ubuntu, "feat/x", c2)
	out, code := runSh(t, script)
	if _, err := ParseFFResult(out, code); !errors.Is(err, ErrNotFastForward) {
		t.Fatalf("期望 ErrNotFastForward，得到 %v (out=%q code=%d)", err, out, code)
	}
	if code != 4 {
		t.Fatalf("退出码 = %d，期望 4", code)
	}
}

func TestFFScript_mismatch(t *testing.T) {
	mac, ubuntu := setupMacUbuntu(t)
	gitOK(t, mac, "checkout", "-b", "feat/x")
	makeCommit(t, mac, "x1")
	stageAndWant(t, mac, ubuntu, "feat/x")

	// want 与暂存引用不符
	script, err := FFScript(ubuntu, "feat/x", strings.Repeat("0", 40))
	if err != nil {
		t.Fatalf("FFScript: %v", err)
	}
	out, code := runSh(t, script)
	if _, err := ParseFFResult(out, code); !errors.Is(err, ErrMismatch) {
		t.Fatalf("期望 ErrMismatch，得到 %v (out=%q code=%d)", err, out, code)
	}
	if code != 3 {
		t.Fatalf("退出码 = %d，期望 3", code)
	}
}

func TestFFScript_badInputs(t *testing.T) {
	if _, err := FFScript("/tmp/repo", "bad branch", strings.Repeat("a", 40)); err == nil {
		t.Error("非法分支名应报错")
	}
	for _, want := range []string{"", "abc", strings.Repeat("g", 40), strings.Repeat("a", 39), strings.Repeat("a", 41)} {
		if _, err := FFScript("/tmp/repo", "main", want); err == nil {
			t.Errorf("want=%q 应报错", want)
		}
	}
	// 合法 want 不该报错
	if _, err := FFScript("/tmp/re po", "feat/x", strings.Repeat("a", 40)); err != nil {
		t.Errorf("合法输入报错: %v", err)
	}
}

func TestParseFFResult_genericError(t *testing.T) {
	// 没有 OK/ERR 标记 → 带输出前 500 字节的错误
	long := strings.Repeat("x", 600)
	out := []byte(long + "\nsome git output\n")
	_, err := ParseFFResult(out, 128)
	if err == nil {
		t.Fatal("期望报错")
	}
	if !strings.Contains(err.Error(), "128") {
		t.Errorf("错误应带退出码: %v", err)
	}
	if strings.Contains(err.Error(), string(out[500:])) {
		t.Error("错误应只带输出前 500 字节")
	}
	// OK 但提交号不对 → 报错
	if _, err := ParseFFResult([]byte("OK notacommit"), 0); err == nil {
		t.Error("OK 后不是提交号应报错")
	}
	// 退出码非 0 但输出是 OK → 不能当成功
	if _, err := ParseFFResult([]byte("OK "+strings.Repeat("a", 40)), 1); err == nil {
		t.Error("退出码非 0 不能当成功")
	}
	// ERR nodir / ERR busy → 对应哨兵错误
	if _, err := ParseFFResult([]byte("ERR nodir"), 6); !errors.Is(err, ErrNoRepoDir) {
		t.Errorf("ERR nodir 应得 ErrNoRepoDir，得到 %v", err)
	}
	if _, err := ParseFFResult([]byte("ERR busy"), 7); !errors.Is(err, ErrRepoBusy) {
		t.Errorf("ERR busy 应得 ErrRepoBusy，得到 %v", err)
	}
	// OK 前混着 merge 的提示输出 → 取 OK 行的提交号
	noisy := []byte("Updating eafc78b..5fbeb6e\nFast-forward\n x2 | 1 +\nOK " + strings.Repeat("b", 40) + "\n")
	got, err := ParseFFResult(noisy, 0)
	if err != nil {
		t.Errorf("带噪音输出应解析出提交号: %v", err)
	}
	if got != strings.Repeat("b", 40) {
		t.Errorf("提交号 = %q", got)
	}
}
