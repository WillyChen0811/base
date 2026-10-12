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

// helper: 创建一个裸仓（模拟远端）和一个工作仓库（模拟本地）。
func setupTestRepos(t *testing.T) (local, remote string) {
	t.Helper()
	local = t.TempDir()
	remote = t.TempDir()

	// 初始化裸仓
	cmd := exec.Command("git", "-c", "init.defaultBranch=master", "init", "--bare", remote)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init --bare %s: %v\n%s", remote, err, out)
	}

	// 初始化本地仓
	cmd = exec.Command("git", "-c", "init.defaultBranch=master", "init", local)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init %s: %v\n%s", local, err, out)
	}

	// 设置用户信息（仓库级，不影响全局）
	for _, dir := range []string{local, remote} {
		cmd = exec.Command("git", "-C", dir, "config", "user.name", "Test User")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git config user.name in %s: %v\n%s", dir, err, out)
		}
		cmd = exec.Command("git", "-C", dir, "config", "user.email", "test@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git config user.email in %s: %v\n%s", dir, err, out)
		}
	}

	return local, remote
}

// helper: 在本地仓创建一个提交。
func makeCommit(t *testing.T, dir string, content string) {
	t.Helper()
	p := filepath.Join(dir, content)
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "-C", dir, "add", p)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	cmd = exec.Command("git", "-C", dir, "commit", "-m", content)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
}

// helper: 把本地当前分支推送到远端同名引用。
func pushDefault(t *testing.T, local, remote string) {
	t.Helper()
	cmd := exec.Command("git", "-C", local, "push", remote, "HEAD")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git push: %v\n%s", err, out)
	}
}

// helper: 获取某仓某个引用的 40 位提交号。
func headCommit(t *testing.T, dir, ref string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "rev-parse", ref)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse %s: %v\n%s", ref, err, out)
	}
	h := strings.TrimSpace(string(out))
	if len(h) < 40 {
		t.Fatalf("got short hash %q", h)
	}
	return h[:40]
}

// helper: 获取当前分支名。
func currentBranch(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git branch: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestSync_newBranch(t *testing.T) {
	local, remote := setupTestRepos(t)
	// 在本地创建初始提交，推送到远端
	makeCommit(t, local, "file1")
	pushDefault(t, local, remote)

	r := Repo{Dir: local, Remote: remote}
	ctx := context.Background()

	// 本地没有 feat/x，远端也没有 → 应该报错
	_, err := Sync(ctx, r, "feat/x", nil)
	if err == nil {
		t.Fatal("expected error for non-existent remote branch")
	}
	// 获取错误信息中包含 git stderr
	errStr := err.Error()
	if !strings.Contains(errStr, "stderr") || !strings.Contains(errStr, "couldn't find remote ref") {
		t.Fatalf("error should carry git stderr, got: %q", errStr)
	}

	// 创建远端分支 feat/x
	cmd := exec.Command("git", "-C", local, "checkout", "-b", "feat/x")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git checkout -b: %v\n%s", err, out)
	}
	cmd = exec.Command("git", "-C", local, "push", remote, "HEAD")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git push feat/x: %v\n%s", err, out)
	}
	// 切回 master
	cmd = exec.Command("git", "-C", local, "checkout", "master")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git checkout master: %v\n%s", err, out)
	}

	// 现在远端有 feat/x，本地没有 → Sync 应该创建并切过去
	remoteHead := headCommit(t, remote, "feat/x")
	commit, err := Sync(ctx, r, "feat/x", nil)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if commit != remoteHead {
		t.Fatalf("expected HEAD %s, got %s", remoteHead, commit)
	}
	// 确认当前在 feat/x 分支
	br := currentBranch(t, local)
	if br != "feat/x" {
		t.Fatalf("expected branch feat/x, got %s", br)
	}
}

func TestSync_fastForward_currentBranch(t *testing.T) {
	local, remote := setupTestRepos(t)
	// 创建初始提交
	makeCommit(t, local, "file1")
	// 推送到远端的 feat/x
	cmd := exec.Command("git", "-C", local, "checkout", "-b", "feat/x")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git checkout -b: %v\n%s", err, out)
	}
	pushDefault(t, local, remote)

	r := Repo{Dir: local, Remote: remote}
	ctx := context.Background()

	// 首次 Sync：应该在 feat/x 上
	_, err := Sync(ctx, r, "feat/x", nil)
	if err != nil {
		t.Fatalf("Sync #1: %v", err)
	}
	if currentBranch(t, local) != "feat/x" {
		t.Fatalf("expected on feat/x, got %s", currentBranch(t, local))
	}

	// 远端推进一个提交
	makeCommit(t, local, "file2")
	remoteHead := headCommit(t, local, "HEAD")
	cmd = exec.Command("git", "-C", local, "push", remote, "HEAD:feat/x")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git push: %v\n%s", err, out)
	}

	// 当前就在 feat/x 上，再 Sync 应该快进
	commit, err := Sync(ctx, r, "feat/x", nil)
	if err != nil {
		t.Fatalf("Sync #2: %v", err)
	}
	if commit != remoteHead {
		t.Fatalf("expected HEAD %s, got %s", remoteHead, commit)
	}
}

func TestSync_fastForward_otherBranch(t *testing.T) {
	local, remote := setupTestRepos(t)
	// 创建 master 和 feat/x
	makeCommit(t, local, "file1")
	cmd := exec.Command("git", "-C", local, "checkout", "-b", "feat/x")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git checkout -b: %v\n%s", err, out)
	}
	// 推送 feat/x 到远端
	cmd = exec.Command("git", "-C", local, "push", remote, "HEAD:feat/x")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git push feat/x: %v\n%s", err, out)
	}
	// 切回 master
	cmd = exec.Command("git", "-C", local, "checkout", "master")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git checkout master: %v\n%s", err, out)
	}

	r := Repo{Dir: local, Remote: remote}
	ctx := context.Background()

	// 首次 Sync：本地没有 feat/x，远端有 → 创建
	_, err := Sync(ctx, r, "feat/x", nil)
	if err != nil {
		t.Fatalf("Sync #1: %v", err)
	}
	// 应该在 feat/x 上
	if currentBranch(t, local) != "feat/x" {
		t.Fatalf("expected on feat/x, got %s", currentBranch(t, local))
	}

	// 再切回 master，模拟用户从别的分支操作
	cmd = exec.Command("git", "-C", local, "checkout", "master")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git checkout master: %v\n%s", err, out)
	}

	// 远端推进一个提交
	makeCommit(t, local, "file2")
	remoteHead := headCommit(t, local, "HEAD")
	cmd = exec.Command("git", "-C", local, "push", remote, "HEAD:feat/x")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git push: %v\n%s", err, out)
	}

	// 当前在 master，Sync feat/x 应该更新本地 feat/x 并切过去
	commit, err := Sync(ctx, r, "feat/x", nil)
	if err != nil {
		t.Fatalf("Sync #2: %v", err)
	}
	if commit != remoteHead {
		t.Fatalf("expected HEAD %s, got %s", remoteHead, commit)
	}
	// 确认切到了 feat/x
	if currentBranch(t, local) != "feat/x" {
		t.Fatalf("expected on feat/x after Sync, got %s", currentBranch(t, local))
	}
	// 确认本地 feat/x 的 HEAD 等于远端
	localHead := headCommit(t, local, "refs/heads/feat/x")
	if localHead != remoteHead {
		t.Fatalf("local feat/x %s != remote %s", localHead, remoteHead)
	}
}

func TestSync_notFastForward(t *testing.T) {
	local, remote := setupTestRepos(t)
	// 创建共同初始提交
	makeCommit(t, local, "file1")
	cmd := exec.Command("git", "-C", local, "checkout", "-b", "feat/x")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git checkout -b: %v\n%s", err, out)
	}
	pushDefault(t, local, remote)
	// 切回 master
	cmd = exec.Command("git", "-C", local, "checkout", "master")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git checkout master: %v\n%s", err, out)
	}

	r := Repo{Dir: local, Remote: remote}
	ctx := context.Background()

	// 首次 Sync：创建 feat/x
	_, err := Sync(ctx, r, "feat/x", nil)
	if err != nil {
		t.Fatalf("Sync #1: %v", err)
	}

	// 本地 feat/x 上有独有提交（在 local 上额外 commit，不推送到远端）
	// 先切到 feat/x
	cmd = exec.Command("git", "-C", local, "checkout", "feat/x")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git checkout feat/x: %v\n%s", err, out)
	}
	// 在本地 feat/x 上创建一个远端没有的提交
	// 再创建一个新提交
	p := filepath.Join(local, "local-only.txt")
	if err := os.WriteFile(p, []byte("local only"), 0644); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("git", "-C", local, "add", p)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	cmd = exec.Command("git", "-C", local, "commit", "-m", "local-only")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
	localHead := headCommit(t, local, "HEAD")

	// 远端没有这个提交 → Sync 应该返回 ErrNotFastForward
	_, err = Sync(ctx, r, "feat/x", nil)
	if !errors.Is(err, ErrNotFastForward) {
		t.Fatalf("expected ErrNotFastForward, got: %v", err)
	}

	// 确认本地分支和当前分支都没变
	if currentBranch(t, local) != "feat/x" {
		t.Fatalf("current branch changed: was feat/x, got %s", currentBranch(t, local))
	}
	if headCommit(t, local, "feat/x") != localHead {
		t.Fatalf("feat/x moved after ErrNotFastForward: was %s, got %s", localHead, headCommit(t, local, "feat/x"))
	}
	// 确认本地 HEAD == feat/x（本来就在 feat/x 上）
	if headCommit(t, local, "HEAD") != localHead {
		t.Fatalf("HEAD moved after ErrNotFastForward: was %s, got %s", localHead, headCommit(t, local, "HEAD"))
	}
}

// 与分支同名的旧标签不能遮蔽分支：本地有独有提交时必须拒绝，且分支不变。
func TestSync_tagShadowsBranch(t *testing.T) {
	local, remote := setupTestRepos(t)
	makeCommit(t, local, "file1")
	git := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", local}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("checkout", "-b", "feat/x")
	git("push", remote, "HEAD:feat/x")
	git("tag", "feat/x") // 旧提交上的同名标签
	// 远端前进一个提交
	makeCommit(t, local, "file2")
	git("push", remote, "HEAD:feat/x")
	// 本地分支回到旧提交后做一个独有提交
	git("reset", "--hard", "HEAD~1")
	makeCommit(t, local, "local-only")
	before := headCommit(t, local, "refs/heads/feat/x")

	_, err := Sync(context.Background(), Repo{Dir: local, Remote: remote}, "feat/x", nil)
	if !errors.Is(err, ErrNotFastForward) {
		t.Fatalf("expected ErrNotFastForward, got: %v", err)
	}
	if after := headCommit(t, local, "refs/heads/feat/x"); after != before {
		t.Fatalf("feat/x moved: %s -> %s", before, after)
	}
}

func TestSync_badRemote(t *testing.T) {
	local, _ := setupTestRepos(t)
	makeCommit(t, local, "file1")
	for _, rm := range []string{"", "-x", "--upload-pack=evil"} {
		_, err := Sync(context.Background(), Repo{Dir: local, Remote: rm}, "master", nil)
		if err == nil || !strings.Contains(err.Error(), "Remote 非法") {
			t.Fatalf("Remote %q: expected 非法 error, got: %v", rm, err)
		}
	}
}

func TestSync_dirtyWorkingDir(t *testing.T) {
	local, remote := setupTestRepos(t)
	// 创建并推送初始提交
	makeCommit(t, local, "file1")
	cmd := exec.Command("git", "-C", local, "push", remote, "HEAD:master")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git push: %v\n%s", err, out)
	}
	// 创建 untracked 文件
	p := filepath.Join(local, "untracked.txt")
	if err := os.WriteFile(p, []byte("untracked"), 0644); err != nil {
		t.Fatal(err)
	}

	r := Repo{Dir: local, Remote: remote}
	ctx := context.Background()

	_, err := Sync(ctx, r, "master", nil)
	if !errors.Is(err, ErrDirty) {
		t.Fatalf("expected ErrDirty, got: %v", err)
	}
}

func TestSync_modifiedFile(t *testing.T) {
	local, remote := setupTestRepos(t)
	// 创建并提交一个文件
	p := filepath.Join(local, "file1.txt")
	if err := os.WriteFile(p, []byte("content"), 0644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "-C", local, "add", p)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	cmd = exec.Command("git", "-C", local, "commit", "-m", "file1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
	cmd = exec.Command("git", "-C", local, "push", remote, "HEAD:master")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git push: %v\n%s", err, out)
	}

	// 修改已跟踪文件（不提交）
	if err := os.WriteFile(p, []byte("modified"), 0644); err != nil {
		t.Fatal(err)
	}

	r := Repo{Dir: local, Remote: remote}
	ctx := context.Background()

	_, err := Sync(ctx, r, "master", nil)
	if !errors.Is(err, ErrDirty) {
		t.Fatalf("expected ErrDirty, got: %v", err)
	}
}

func TestSync_remoteBranchNotFound(t *testing.T) {
	local, remote := setupTestRepos(t)
	// 创建初始提交，不推送任何 feat/nonexist
	makeCommit(t, local, "file1")
	cmd := exec.Command("git", "-C", local, "push", remote, "HEAD:master")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git push: %v\n%s", err, out)
	}

	r := Repo{Dir: local, Remote: remote}
	ctx := context.Background()

	_, err := Sync(ctx, r, "feat/nonexist", nil)
	if err == nil {
		t.Fatal("expected error for non-existent remote branch")
	}
	errStr := err.Error()
	// 错误信息中应包含 git 的 stderr
	if !strings.Contains(errStr, "stderr") || !strings.Contains(errStr, "couldn't find remote ref") {
		t.Fatalf("error should carry git stderr, got: %q", errStr)
	}
}

func TestSync_contextCanceled(t *testing.T) {
	local, remote := setupTestRepos(t)
	makeCommit(t, local, "file1")
	cmd := exec.Command("git", "-C", local, "push", remote, "HEAD:master")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git push: %v\n%s", err, out)
	}

	r := Repo{Dir: local, Remote: remote}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立即取消

	_, err := Sync(ctx, r, "master", nil)
	if err == nil {
		t.Fatal("expected error for canceled context")
	}
}

func TestValidBranch(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"合法 feat/x", "feat/x", false},
		{"合法 v1.2_a", "v1.2_a", false},
		{"合法 main", "main", false},
		{"合法 a-b", "a-b", false},
		{"合法 a/b/c", "a/b/c", false},
		{"空字符串", "", true},
		{"以 - 开头", "-x", true},
		{"以 / 开头", "/x", true},
		{"以 / 结尾", "x/", true},
		{"含 ..", "a..b", true},
		{"含 //", "a//b", true},
		{"含 @{", "a@{1", true},
		{"以 .lock 结尾", "x.lock", true},
		{"含空格", "a b", true},
		{"含中文", "中文", true},
		{"201 字节", string(makeBytes(201)), true},
		{"200 字节（边界）", string(makeBytes(200)), false},
		{"1 字节", "a", false},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			err := ValidBranch(tt.input)
			if tt.wantErr && err == nil {
				t.Errorf("ValidBranch(%q) expected error, got nil", tt.input)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("ValidBranch(%q) unexpected error: %v", tt.input, err)
			}
		})
	}
}

func makeBytes(n int) string {
	b := make([]byte, n)
	for i := 0; i < n; i++ {
		b[i] = 'a' + byte(i%26)
	}
	return string(b)
}
