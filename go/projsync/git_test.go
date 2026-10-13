package projsync

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// initRepo 初始化一个 git 仓库（bare 或非裸均可），写入初始提交。
func initRepo(dir string, t *testing.T) {
	t.Helper()
	exec.Command("git", "-C", dir, "init", "-b", "main").Run()
	exec.Command("git", "-C", dir, "config", "user.name", "test").Run()
	exec.Command("git", "-C", dir, "config", "user.email", "test@test").Run()

	os.WriteFile(filepath.Join(dir, "README.md"), []byte("# test"), 0o644)
	git(dir, "add", ".")
	git(dir, "commit", "-m", "initial")
}

// git 快捷执行，收集 stderr 到 t.Fatal。
func git(dir string, args ...string) string {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		panic(fmt.Sprintf("git %s 失败: %v\n%s", strings.Join(args, " "), err, out))
	}
	return string(out)
}

// setupBareMain 创建一个 bare 仓库并推送一个初始提交到 main，返回 bare 仓库路径。
func setupBareMain(baseDir string, t *testing.T) string {
	t.Helper()
	bareDir := filepath.Join(baseDir, "bare.git")
	git(baseDir, "init", "--bare", "-b", "main", bareDir)

	tmpDir := filepath.Join(baseDir, "tmpinit")
	exec.Command("git", "init", "-b", "main", tmpDir).Run()
	exec.Command("git", "-C", tmpDir, "config", "user.name", "test").Run()
	exec.Command("git", "-C", tmpDir, "config", "user.email", "test@test").Run()
	os.WriteFile(filepath.Join(tmpDir, "README.md"), []byte("# test"), 0o644)
	git(tmpDir, "add", ".")
	git(tmpDir, "commit", "-m", "initial")
	git(tmpDir, "remote", "add", "origin", bareDir)
	git(tmpDir, "push", "-u", "origin", "main")

	return bareDir
}

// testSyncGit 覆盖本任务要求的所有行为。
func testSyncGit(t *testing.T, mergeStrat string) {
	_ = mergeStrat // unused
	ctx := context.Background()

	// ========== 准备仓库 ==========
	baseDir := t.TempDir()
	dirA := filepath.Join(baseDir, "A")
	dirB := filepath.Join(baseDir, "B")
	bareDir := setupBareMain(baseDir, t)

	// 克隆出 A 和 B
	git(baseDir, "clone", bareDir, dirA)
	git(dirA, "config", "user.name", "test")
	git(dirA, "config", "user.email", "test@test")
	git(baseDir, "clone", bareDir, dirB)
	git(dirB, "config", "user.name", "test")
	git(dirB, "config", "user.email", "test@test")

	// A 在 main 上多 2 个提交
	for i := 1; i <= 2; i++ {
		os.WriteFile(filepath.Join(dirA, fmt.Sprintf("a%d.txt", i)), []byte(fmt.Sprintf("a%d", i)), 0o644)
		git(dirA, "add", ".")
		git(dirA, "commit", "-m", fmt.Sprintf("a commit %d", i))
	}

	// B 在 main 上多 1 个提交
	os.WriteFile(filepath.Join(dirB, "b1.txt"), []byte("b1"), 0o644)
	git(dirB, "add", ".")
	git(dirB, "commit", "-m", "b commit 1")

	// B 创建 feat 分支
	git(dirB, "checkout", "-b", "feat")
	os.WriteFile(filepath.Join(dirB, "feat.txt"), []byte("feat"), 0o644)
	git(dirB, "add", ".")
	git(dirB, "commit", "-m", "feat work")
	git(dirB, "checkout", "main") // 切回 main

	// A 创建 only-a 分支
	git(dirA, "checkout", "-b", "only-a")
	os.WriteFile(filepath.Join(dirA, "only-a.txt"), []byte("only-a"), 0o644)
	git(dirA, "add", ".")
	git(dirA, "commit", "-m", "only-a work")
	git(dirA, "checkout", "main") // 切回 main

	// A 留一个未提交改动文件，验证工作区不受影响
	os.WriteFile(filepath.Join(dirA, "dirty.txt"), []byte("dirty"), 0o644)

	// ========== 取初始状态 ==========
	mainCommitA := strings.TrimSpace(git(dirA, "rev-parse", "refs/heads/main"))
	onlyACommit := strings.TrimSpace(git(dirA, "rev-parse", "refs/heads/only-a"))
	mainCommitB := strings.TrimSpace(git(dirB, "rev-parse", "refs/heads/main"))
	featCommit := strings.TrimSpace(git(dirB, "rev-parse", "refs/heads/feat"))

	aStatusBefore := strings.TrimSpace(git(dirA, "status", "--porcelain"))
	aHEADBefore := strings.TrimSpace(git(dirA, "rev-parse", "HEAD"))
	aRefsBefore := strings.TrimSpace(git(dirA, "for-each-ref", "refs/heads"))

	// ========== 执行 SyncGit(A→B) ==========
	remoteURL := dirB // 从 A 的角度，远端是 B
	res, err := SyncGit(ctx, dirA, remoteURL, []string{}, "mac", "ubuntu")
	if err != nil {
		t.Fatalf("SyncGit 失败: %v", err)
	}

	// ========== 验证 ==========

	// 1. B 有 refs/remotes/mac/main 和 mac/only-a，指向 A 的提交
	t.Run("remote_refs_created", func(t *testing.T) {
		for _, ref := range []string{"refs/remotes/mac/main", "refs/remotes/mac/only-a"} {
			out, err := exec.Command("git", "-C", dirB, "rev-parse", "-q", ref).CombinedOutput()
			if err != nil {
				t.Fatalf("git rev-parse %s: %v\n%s", ref, err, out)
			}
			commit := strings.TrimSpace(string(out))
			if ref == "refs/remotes/mac/main" && commit != mainCommitA {
				t.Errorf("mac/main 指向 %s，期望 %s", commit, mainCommitA)
			}
			if ref == "refs/remotes/mac/only-a" && commit != onlyACommit {
				t.Errorf("mac/only-a 指向 %s，期望 %s", commit, onlyACommit)
			}
		}
	})

	// 2. A 有 refs/remotes/ubuntu/main 和 ubuntu/feat
	t.Run("fetch_refs_created", func(t *testing.T) {
		for ref, want := range map[string]string{
			"refs/remotes/ubuntu/main": mainCommitB,
			"refs/remotes/ubuntu/feat": featCommit,
		} {
			out, err := exec.Command("git", "-C", dirA, "rev-parse", "-q", ref).CombinedOutput()
			if err != nil {
				t.Fatalf("git rev-parse %s: %v\n%s", ref, err, out)
			}
			commit := strings.TrimSpace(string(out))
			if commit != want {
				t.Errorf("%s 指向 %s，期望 %s", ref, commit, want)
			}
		}
	})

	// 3. Branches 结果验证
	t.Run("branch_states", func(t *testing.T) {
		var mainState, onlyAState *BranchState
		for i := range res.Branches {
			if res.Branches[i].Branch == "main" {
				mainState = &res.Branches[i]
			}
			if res.Branches[i].Branch == "only-a" {
				onlyAState = &res.Branches[i]
			}
		}
		if mainState == nil {
			t.Fatal("没有 main 分支")
		}
		if mainState.Ahead != 2 {
			t.Errorf("main.Ahead=%d，期望 2", mainState.Ahead)
		}
		if mainState.Behind != 1 {
			t.Errorf("main.Behind=%d，期望 1", mainState.Behind)
		}
		if !mainState.Remote {
			t.Error("main.Remote=false，期望 true")
		}
		if onlyAState == nil {
			t.Fatal("没有 only-a 分支")
		}
		if onlyAState.Remote {
			t.Error("only-a.Remote=true，期望 false")
		}
	})

	// 4. RemoteOnly
	t.Run("remote_only", func(t *testing.T) {
		if len(res.RemoteOnly) != 1 || res.RemoteOnly[0] != "feat" {
			t.Errorf("RemoteOnly=%v，期望 [feat]", res.RemoteOnly)
		}
	})

	// 5. 工作区、HEAD、refs/heads 不变
	t.Run("workspace_untouched", func(t *testing.T) {
		aStatusAfter := strings.TrimSpace(git(dirA, "status", "--porcelain"))
		aHEADAfter := strings.TrimSpace(git(dirA, "rev-parse", "HEAD"))
		aRefsAfter := strings.TrimSpace(git(dirA, "for-each-ref", "refs/heads"))

		if aStatusBefore != aStatusAfter {
			t.Errorf("工作区改动：\n前: %s\n后: %s", aStatusBefore, aStatusAfter)
		}
		if aHEADBefore != aHEADAfter {
			t.Errorf("HEAD 被修改: %s → %s", aHEADBefore, aHEADAfter)
		}
		if aRefsBefore != aRefsAfter {
			t.Errorf("refs/heads 被修改:\n前: %s\n后: %s", aRefsBefore, aRefsAfter)
		}
	})
}

// TestSyncGit 完整集成测试。
func TestSyncGit(t *testing.T) {
	testSyncGit(t, "fast-forward")
}

// TestSyncGitInvalidParam 命名空间名非法 / remoteURL 以 - 开头 → 报错。
func TestSyncGitInvalidParam(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	exec.Command("git", "init", "-b", "main", dir).Run()
	exec.Command("git", "-C", dir, "config", "user.name", "test").Run()
	exec.Command("git", "-C", dir, "config", "user.email", "test@test").Run()
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("# test"), 0o644)
	git(dir, "add", ".")
	git(dir, "commit", "-m", "initial")

	// 测试 1：命名空间名含非法字符
	_, err := SyncGit(ctx, dir, dir, []string{}, "Mac", "ubuntu")
	if err == nil {
		t.Fatal("期望报错，实际未报错（Mac 含大写字母）")
	}
	if !strings.Contains(err.Error(), "非法字符") {
		t.Errorf("错误信息应包含'非法字符'，实际: %v", err)
	}

	// 测试 2：命名空间名超长
	_, err = SyncGit(ctx, dir, dir, []string{}, "a", strings.Repeat("a", 33))
	if err == nil {
		t.Fatal("期望报错，实际未报错（长度 33）")
	}

	// 测试 3：remoteURL 以 - 开头
	_, err = SyncGit(ctx, dir, "-foo", []string{}, "mac", "ubuntu")
	if err == nil {
		t.Fatal("期望报错，实际未报错（remoteURL 以 - 开头）")
	}

	// 验证报错时没有执行 git 命令（refs/remotes 下不应有内容）
	for _, ns := range []string{"mac", "ubuntu"} {
		out, _ := exec.Command("git", "-C", dir, "for-each-ref",
			fmt.Sprintf("refs/remotes/%s", ns)).CombinedOutput()
		if len(strings.TrimSpace(string(out))) > 0 {
			t.Errorf("报错后仍创建了 refs/remotes/%s 下的引用", ns)
		}
	}
}

// TestSyncGitNonExistentRemote 对方地址不存在 → 返回错误，错误信息带 git stderr。
func TestSyncGitNonExistentRemote(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	initRepo(dir, t)

	_, err := SyncGit(ctx, dir, "/nonexistent/nowhere", []string{}, "mac", "ubuntu")
	if err == nil {
		t.Fatal("期望报错，实际未报错")
	}
	// 错误信息应包含 git 的 stderr（如 "repository does not exist" 之类）
	if !strings.Contains(err.Error(), "stderr") {
		t.Errorf("错误信息应包含 stderr，实际: %v", err)
	}
	if !strings.Contains(err.Error(), "失败") {
		t.Errorf("错误信息应包含'失败'，实际: %v", err)
	}
}

// TestSyncGitPrune 删掉 only-a 再同步 → B 的 mac/only-a 被 prune。
func TestSyncGitPrune(t *testing.T) {
	ctx := context.Background()

	baseDir := t.TempDir()
	dirA := filepath.Join(baseDir, "A")
	dirB := filepath.Join(baseDir, "B")
	bareDir := setupBareMain(baseDir, t)

	git(baseDir, "clone", bareDir, dirA)
	git(dirA, "config", "user.name", "test")
	git(dirA, "config", "user.email", "test@test")
	git(baseDir, "clone", bareDir, dirB)
	git(dirB, "config", "user.name", "test")
	git(dirB, "config", "user.email", "test@test")

	// A 和 B 各在 main 上多一个提交
	os.WriteFile(filepath.Join(dirA, "a.txt"), []byte("a"), 0o644)
	git(dirA, "add", ".")
	git(dirA, "commit", "-m", "a commit")

	os.WriteFile(filepath.Join(dirB, "b.txt"), []byte("b"), 0o644)
	git(dirB, "add", ".")
	git(dirB, "commit", "-m", "b commit")

	// A 创建 only-a 分支
	git(dirA, "checkout", "-b", "only-a")
	os.WriteFile(filepath.Join(dirA, "only-a.txt"), []byte("only-a"), 0o644)
	git(dirA, "add", ".")
	git(dirA, "commit", "-m", "only-a")
	git(dirA, "checkout", "main")

	// B 创建 feat 分支
	git(dirB, "checkout", "-b", "feat")
	os.WriteFile(filepath.Join(dirB, "feat.txt"), []byte("feat"), 0o644)
	git(dirB, "add", ".")
	git(dirB, "commit", "-m", "feat")
	git(dirB, "checkout", "main")

	// 第一次同步
	_, err := SyncGit(ctx, dirA, dirB, []string{}, "mac", "ubuntu")
	if err != nil {
		t.Fatalf("第一次同步失败: %v", err)
	}

	// 确认 B 有 refs/remotes/mac/only-a
	if !refExists(ctx, dirB, []string{}, "refs/remotes/mac/only-a") {
		t.Fatal("第一次同步后 B 没有 mac/only-a")
	}

	// A 删除 only-a 分支
	git(dirA, "branch", "-D", "only-a")

	// 第二次同步（从 A 推送到 B，此时 only-a 没了）
	_, err = SyncGit(ctx, dirA, dirB, []string{}, "mac", "ubuntu")
	if err != nil {
		t.Fatalf("第二次同步失败: %v", err)
	}

	// B 的 refs/remotes/mac/only-a 应被 prune
	if refExists(ctx, dirB, []string{}, "refs/remotes/mac/only-a") {
		t.Error("删掉 only-a 再同步后，B 仍有 refs/remotes/mac/only-a，预期被 prune")
	}
}
