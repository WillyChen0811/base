package macexec

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// newFakeHome 搭一个最小仓库：<home>/xxyi-work/xxyi（含 mac/build.sh、tests/js/unit.test.js），
// 外加 <home>/xxyi-work/base（git_status 换仓库用）。返回 home。
func newFakeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	xxyi := filepath.Join(home, "xxyi-work", "xxyi")
	for _, d := range []string{
		filepath.Join(xxyi, "mac"),
		filepath.Join(xxyi, "tests", "js"),
		filepath.Join(home, "xxyi-work", "base"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(xxyi, "mac", "build.sh"), []byte("#!/bin/sh\necho build\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(xxyi, "tests", "js", "unit.test.js"), []byte("// test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return home
}

// TestBuildSpecActions：五个动作各自的 Argv/Dir/Timeout 都按约定生成；
// 编译类动作的 Dir 是 ResolveInRepo 解析后的 <xxyi>/mac；build_app 用解析后的绝对路径；
// git_status 的仓库由调用方选。
func TestBuildSpecActions(t *testing.T) {
	home := newFakeHome(t)
	xxyi, err := RepoRoot(home, "xxyi", DefaultRepos)
	if err != nil {
		t.Fatalf("RepoRoot(xxyi): %v", err)
	}
	base, err := RepoRoot(home, "base", DefaultRepos)
	if err != nil {
		t.Fatalf("RepoRoot(base): %v", err)
	}
	macDir, err := filepath.EvalSymlinks(filepath.Join(xxyi, "mac"))
	if err != nil {
		t.Fatal(err)
	}
	buildSh, err := filepath.EvalSymlinks(filepath.Join(xxyi, "mac", "build.sh"))
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name     string
		action   string
		p        Params
		wantDir  string
		wantArgv []string
		wantTO   time.Duration
	}{
		{"git_status_xxyi", ActionGitStatus, Params{Repo: "xxyi"}, xxyi,
			[]string{"git", "status", "-b", "--porcelain=v1"}, 30 * time.Second},
		{"git_status_base", ActionGitStatus, Params{Repo: "base"}, base,
			[]string{"git", "status", "-b", "--porcelain=v1"}, 30 * time.Second},
		{"swift_build", ActionSwiftBuild, Params{}, macDir,
			[]string{"swift", "build"}, 15 * time.Minute},
		{"selftest", ActionSelftest, Params{}, macDir,
			[]string{"swift", "run", "xxyi-selftest"}, 15 * time.Minute},
		{"node_test", ActionNodeTest, Params{Name: "unit"}, xxyi,
			[]string{"node", filepath.Join("tests", "js", "unit.test.js")}, 5 * time.Minute},
		{"build_app", ActionBuildApp, Params{}, macDir,
			[]string{buildSh}, 20 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := BuildSpec(tc.action, tc.p, home)
			if err != nil {
				t.Fatalf("BuildSpec(%q): %v", tc.action, err)
			}
			if spec.Action != tc.action {
				t.Errorf("Action=%q，期望 %q", spec.Action, tc.action)
			}
			if spec.Dir != tc.wantDir {
				t.Errorf("Dir=%q，期望 %q", spec.Dir, tc.wantDir)
			}
			if !reflect.DeepEqual(spec.Argv, tc.wantArgv) {
				t.Errorf("Argv=%q，期望 %q", spec.Argv, tc.wantArgv)
			}
			if spec.Timeout != tc.wantTO {
				t.Errorf("Timeout=%v，期望 %v", spec.Timeout, tc.wantTO)
			}
		})
	}

	// 编译类动作固定作用于 xxyi：调用方传别的仓库必须报错，不能悄悄忽略。
	if _, err := BuildSpec(ActionSwiftBuild, Params{Repo: "base"}, home); err == nil ||
		!strings.Contains(err.Error(), "固定作用于") {
		t.Errorf("swift_build 传 repo=base 应报固定仓库错，实际 err=%v", err)
	}
}

// TestBuildSpecNodeTestBadNames：测试名只许 [a-z0-9_]+，其余（大写、连字符、点、斜杠、
// 空、上跳）全部报错且不拼半截命令。
func TestBuildSpecNodeTestBadNames(t *testing.T) {
	home := newFakeHome(t)
	for _, name := range []string{"", "Bad", "a-b", "a.b", "a b", "../evil", "a/b", ".hidden", "unit-test"} {
		spec, err := BuildSpec(ActionNodeTest, Params{Name: name}, home)
		if err == nil {
			t.Errorf("name=%q 应报错，实际 spec=%+v", name, spec)
			continue
		}
		if !strings.Contains(err.Error(), "不合法") {
			t.Errorf("name=%q 应报名字不合法错，实际 err=%v", name, err)
		}
	}
}

// TestBuildSpecNodeTestNotRegularOrMissing：tests/js 下的目标不存在、或是目录
// （名字校验过但类型不对），必须报错。
func TestBuildSpecNodeTestNotRegularOrMissing(t *testing.T) {
	home := newFakeHome(t)
	xxyi := filepath.Join(home, "xxyi-work", "xxyi")

	// 名字合法但文件不存在。
	if _, err := BuildSpec(ActionNodeTest, Params{Name: "ghost"}, home); err == nil ||
		!strings.Contains(err.Error(), "不可用") {
		t.Errorf("缺失文件应报不可用，实际 err=%v", err)
	}

	// tests/js/<name>.test.js 是目录 → 不是普通文件。
	dirTarget := filepath.Join(xxyi, "tests", "js", "weird.test.js")
	if err := os.Mkdir(dirTarget, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildSpec(ActionNodeTest, Params{Name: "weird"}, home); err == nil ||
		!strings.Contains(err.Error(), "不是普通文件") {
		t.Errorf("目录目标应报不是普通文件，实际 err=%v", err)
	}

	// tests/js 被符号链接挪出仓库 → ResolveInRepo 挡住。
	outside := filepath.Join(home, "outside-js")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "host.test.js"), []byte("//\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(xxyi, "tests", "js")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(xxyi, "tests", "js")); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildSpec(ActionNodeTest, Params{Name: "host"}, home); err == nil ||
		!strings.Contains(err.Error(), "越出仓库") {
		t.Errorf("tests/js 挪出仓库应报越界，实际 err=%v", err)
	}
}

// TestBuildSpecMacSymlinkEscape：mac 是指向仓库外的符号链接时，
// 三个以 mac 为工作目录的动作与 build_app 全部报错（ResolveInRepo 挡住）。
func TestBuildSpecMacSymlinkEscape(t *testing.T) {
	home := newFakeHome(t)
	xxyi := filepath.Join(home, "xxyi-work", "xxyi")
	outside := filepath.Join(home, "outside-mac")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "build.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// mac 里有 build.sh，整个移走再换成指向仓库外的符号链接。
	if err := os.RemoveAll(filepath.Join(xxyi, "mac")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(xxyi, "mac")); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{ActionSwiftBuild, ActionSelftest, ActionBuildApp} {
		spec, err := BuildSpec(action, Params{}, home)
		if err == nil {
			t.Errorf("%s：mac 指向仓库外仍返回了 spec=%+v", action, spec)
			continue
		}
		if !strings.Contains(err.Error(), "越出仓库") {
			t.Errorf("%s：应报越出仓库，实际 err=%v", action, err)
		}
	}
}

// TestBuildSpecBuildAppNotRegular：build.sh 不存在或不是普通文件（这里是目录）都报错。
func TestBuildSpecBuildAppNotRegular(t *testing.T) {
	home := newFakeHome(t)
	xxyi := filepath.Join(home, "xxyi-work", "xxyi")

	// build.sh 换成目录。
	sh := filepath.Join(xxyi, "mac", "build.sh")
	if err := os.Remove(sh); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(sh, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildSpec(ActionBuildApp, Params{}, home); err == nil ||
		!strings.Contains(err.Error(), "不是普通文件") {
		t.Errorf("build.sh 是目录应报不是普通文件，实际 err=%v", err)
	}

	// build.sh 是指向仓库外的符号链接 → ResolveInRepo 先挡住。
	if err := os.Remove(sh); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(home, "outside.sh")
	if err := os.WriteFile(outside, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, sh); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildSpec(ActionBuildApp, Params{}, home); err == nil ||
		!strings.Contains(err.Error(), "build.sh 不可用") {
		t.Errorf("build.sh 挪出仓库应报不可用，实际 err=%v", err)
	}
}

// TestBuildSpecUnknownAction：约定外的动作（含空串）报错，并列出可用名单。
func TestBuildSpecUnknownAction(t *testing.T) {
	home := newFakeHome(t)
	for _, action := range []string{"", "shutdown", "run_command", "git_status "} {
		spec, err := BuildSpec(action, Params{}, home)
		if err == nil {
			t.Errorf("action=%q 应报错，实际 spec=%+v", action, spec)
			continue
		}
		if !strings.Contains(err.Error(), "未知动作") {
			t.Errorf("action=%q 应报未知动作，实际 err=%v", action, err)
		}
		if !strings.Contains(err.Error(), ActionGitStatus) || !strings.Contains(err.Error(), ActionBuildApp) {
			t.Errorf("未知动作错误应列出可用名单，实际 err=%v", err)
		}
	}
}
