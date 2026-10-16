package macexec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newRepo 在临时目录里搭一个 <home>/xxyi-work/xxyi 的假仓库（含 mac/ 子目录和假 .git），
// 返回 home 与仓库根。
func newRepo(t *testing.T) (home, root string) {
	t.Helper()
	home = t.TempDir()
	root = filepath.Join(home, "xxyi-work", "xxyi")
	if err := os.MkdirAll(filepath.Join(root, "mac"), 0o755); err != nil {
		t.Fatalf("建仓库目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("ok"), 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	// 假 .git：不用真是 git 仓库，EvalSymlinks 只按路径段解析。
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("建 .git 目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "config"), []byte("[core]\n"), 0o644); err != nil {
		t.Fatalf("写 .git/config 失败: %v", err)
	}
	return home, root
}

func TestRepoRootPathValidation(t *testing.T) {
	home, root := newRepo(t)

	got, err := RepoRoot(home, "xxyi", DefaultRepos)
	if err != nil {
		t.Fatalf("合法仓库名应通过，实际报错: %v", err)
	}
	if got != root {
		t.Errorf("RepoRoot = %q，期望 %q", got, root)
	}

	// 名字不合法或不在名单里。
	badNames := []struct {
		name    string
		allowed []string
		why     string
	}{
		{"", DefaultRepos, "空名"},
		{"..", DefaultRepos, ".. 当仓库名"},
		{".", DefaultRepos, ". 当仓库名"},
		{"a/b", DefaultRepos, "名字带斜杠"},
		{"a\\b", DefaultRepos, "名字带反斜杠"},
		{"evil", DefaultRepos, "不在默认名单"},
		{"xxyi", []string{"base"}, "不在自定义名单"},
	}
	for _, c := range badNames {
		if _, err := RepoRoot(home, c.name, c.allowed); err == nil {
			t.Errorf("RepoRoot(%q) 应因 %s 被拒，实际通过", c.name, c.why)
		}
	}

	// 名字在名单里但目录不存在。
	if _, err := RepoRoot(home, "base", DefaultRepos); err == nil {
		t.Error("仓库目录不存在时应报错，实际通过")
	}

	// 路径存在但不是目录。
	fileName := "websubmit"
	if err := os.WriteFile(filepath.Join(home, "xxyi-work", fileName), []byte("x"), 0o644); err != nil {
		t.Fatalf("建普通文件失败: %v", err)
	}
	if _, err := RepoRoot(home, fileName, DefaultRepos); err == nil {
		t.Error("仓库路径是普通文件时应报错，实际通过")
	}
}

func TestResolveInRepoPathValid(t *testing.T) {
	_, root := newRepo(t)

	cases := []string{
		"README.md",
		"mac/main.swift",
		"docs/a/b/c.txt",
	}
	for _, rel := range cases {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("建目录失败: %v", err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatalf("写文件失败: %v", err)
		}
		want, err := filepath.EvalSymlinks(full)
		if err != nil {
			t.Fatalf("构造期望值失败: %v", err)
		}
		got, err := ResolveInRepo(root, rel)
		if err != nil {
			t.Errorf("ResolveInRepo(%q) 合法路径应通过，实际报错: %v", rel, err)
			continue
		}
		if !filepath.IsAbs(got) {
			t.Errorf("ResolveInRepo(%q) = %q，应返回绝对路径", rel, got)
		}
		if got != want {
			t.Errorf("ResolveInRepo(%q) = %q，期望 %q", rel, got, want)
		}
	}
}

func TestResolveInRepoPathRejects(t *testing.T) {
	home, root := newRepo(t)

	// 仓库外放一个敏感文件，并造一个仓库外目标目录。
	outside := filepath.Join(home, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("建仓库外目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("s"), 0o644); err != nil {
		t.Fatalf("写仓库外文件失败: %v", err)
	}

	// 指向仓库外的符号链接（相对、绝对两种）。
	if err := os.Symlink("../../outside", filepath.Join(root, "escape_rel")); err != nil {
		t.Fatalf("建逃逸符号链接失败: %v", err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "escape_abs")); err != nil {
		t.Fatalf("建绝对逃逸符号链接失败: %v", err)
	}
	// 指向 .git 的符号链接。
	if err := os.Symlink(".git", filepath.Join(root, "alias")); err != nil {
		t.Fatalf("建 alias -> .git 符号链接失败: %v", err)
	}

	bad := []struct {
		rel string
		why string
	}{
		{"", "空路径"},
		{"/etc/passwd", "绝对路径"},
		{"../outside/secret.txt", "首段 .."},
		{"mac/../../outside/secret.txt", "中段 .."},
		{"mac/../README.md", "段内 .. 回退（即使结果在仓库内）"},
		{".git/config", ".git 段"},
		{".GIT/config", "大小写变体 .GIT"},
		{".Git/config", "大小写变体 .Git"},
		{"mac/.git/config", "子目录下的 .git"},
		{"alias/config", "符号链接 alias -> .git 后落到 .git"},
		{"alias", "符号链接直接指向 .git"},
		{"ALIAS/config", "指向 .git 的符号链接名大写（解析后仍含 .git）"},
		{"escape_rel/secret.txt", "相对符号链接逃逸到仓库外"},
		{"escape_rel", "相对符号链接本身（指向仓库外）"},
		{"escape_abs", "绝对符号链接逃逸到仓库外"},
		{"nope/missing.txt", "路径不存在"},
	}
	for _, c := range bad {
		got, err := ResolveInRepo(root, c.rel)
		if err == nil {
			t.Errorf("ResolveInRepo(%q) 应因 %s 被拒，实际返回 %q", c.rel, c.why, got)
			continue
		}
		if !strings.Contains(err.Error(), c.rel) && c.rel != "" {
			// 错误信息里应带上收到的路径，方便排查。
			t.Logf("ResolveInRepo(%q) 报错信息未包含原路径: %v", c.rel, err)
		}
	}
}
