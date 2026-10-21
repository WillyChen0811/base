// Package macexec 提供 Mac 执行端的纯逻辑：仓库路径校验、命令任务执行与动作映射，
// 不依赖 HTTP，也不依赖 support 包。
package macexec

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// 固定的动作名单（HTTP /exec/run 的 action、MCP mac_* 工具一一对应，见 tasks-macexec/CONTRACT.md）。
// 不提供任意命令执行：命令、工作目录、超时都在这里写死，参数只是选项。
const (
	ActionGitStatus  = "git_status"
	ActionSwiftBuild = "swift_build"
	ActionSelftest   = "selftest"
	ActionNodeTest   = "node_test"
	ActionBuildApp   = "build_app"
)

// 各动作的超时（见 tasks-macexec/CONTRACT.md）。
const (
	gitStatusTimeout = 30 * time.Second
	swiftTimeout     = 15 * time.Minute // swift_build / selftest
	nodeTestTimeout  = 5 * time.Minute
	buildAppTimeout  = 20 * time.Minute
)

// xxyiRepo 是编译类动作（swift_build/selftest/node_test/build_app）固定作用的仓库；
// 只有 git_status 由调用方选仓库。
const xxyiRepo = "xxyi"

// nodeTestNamePattern 限定 node 测试文件名：小写字母/数字/下划线，不含路径分隔符与点。
var nodeTestNamePattern = regexp.MustCompile(`^[a-z0-9_]+$`)

// Params 是 BuildSpec 的输入，对应 HTTP /exec/run 的 {action, repo, name}：
// Repo 仓库名（仅 git_status 有意义），
// Name 是 tests/js 下的 node 测试文件名（不含 .test.js 后缀，仅 node_test 用），
// RootOverride 非空时编译类动作改以它为 xxyi 仓库根（专用 worktree，由调用方准备好；
// 仍经 ResolveInRepo 校验），git_status 不接受。
type Params struct {
	Repo         string `json:"repo"`
	Name         string `json:"name"`
	RootOverride string `json:"root_override"`
}

// BuildSpec 把动作 + 参数拼成可直接交给 Run 的 Spec；
// 未知动作、仓库不合法、工作目录/脚本不可用都返回错误，不拼半截命令。
// 编译类动作的工作目录一律经 ResolveInRepo 解析（挡住 mac 是指向仓库外的符号链接）；
// build_app 用解析后的 build.sh 绝对路径，不依赖 ./。
func BuildSpec(action string, p Params, home string) (Spec, error) {
	switch action {
	case ActionGitStatus:
		if p.RootOverride != "" {
			return Spec{}, fmt.Errorf("git_status 不支持仓库根覆盖")
		}
		root, err := RepoRoot(home, p.Repo, DefaultRepos)
		if err != nil {
			return Spec{}, err
		}
		return Spec{
			Action:  action,
			Dir:     root,
			Argv:    []string{"git", "status", "-b", "--porcelain=v1"},
			Timeout: gitStatusTimeout,
		}, nil
	case ActionSwiftBuild:
		dir, err := xxyiMacDir(home, p)
		if err != nil {
			return Spec{}, err
		}
		return Spec{Action: action, Dir: dir, Argv: []string{"swift", "build"}, Timeout: swiftTimeout}, nil
	case ActionSelftest:
		dir, err := xxyiMacDir(home, p)
		if err != nil {
			return Spec{}, err
		}
		return Spec{Action: action, Dir: dir, Argv: []string{"swift", "run", "xxyi-selftest"}, Timeout: swiftTimeout}, nil
	case ActionBuildApp:
		dir, err := xxyiMacDir(home, p)
		if err != nil {
			return Spec{}, err
		}
		script, err := ResolveInRepo(dir, "build.sh")
		if err != nil {
			return Spec{}, fmt.Errorf("build.sh 不可用: %w", err)
		}
		st, err := os.Stat(script)
		if err != nil {
			return Spec{}, fmt.Errorf("build.sh 不可用 %q: %w", script, err)
		}
		if !st.Mode().IsRegular() {
			return Spec{}, fmt.Errorf("build.sh 不是普通文件: %q", script)
		}
		return Spec{Action: action, Dir: dir, Argv: []string{script}, Timeout: buildAppTimeout}, nil
	case ActionNodeTest:
		root, err := xxyiRepoRoot(home, p)
		if err != nil {
			return Spec{}, err
		}
		if !nodeTestNamePattern.MatchString(p.Name) {
			return Spec{}, fmt.Errorf("node 测试文件名不合法 %q（只许小写字母/数字/下划线）", p.Name)
		}
		rel := filepath.Join("tests", "js", p.Name+".test.js")
		// ResolveInRepo 顺带挡住 tests/js 或文件本身被符号链接挪出仓库；解析后还须是普通文件。
		target, err := ResolveInRepo(root, rel)
		if err != nil {
			return Spec{}, fmt.Errorf("node 测试文件不可用: %w", err)
		}
		st, err := os.Stat(target)
		if err != nil {
			return Spec{}, fmt.Errorf("node 测试文件不可用 %q: %w", target, err)
		}
		if !st.Mode().IsRegular() {
			return Spec{}, fmt.Errorf("node 测试文件不是普通文件: %q", target)
		}
		return Spec{
			Action:  action,
			Dir:     root,
			Argv:    []string{"node", rel},
			Timeout: nodeTestTimeout,
		}, nil
	default:
		return Spec{}, fmt.Errorf("未知动作 %q（可用: %s）", action,
			strings.Join([]string{ActionGitStatus, ActionSwiftBuild, ActionSelftest, ActionNodeTest, ActionBuildApp}, ", "))
	}
}

// xxyiRepoRoot 取编译类动作的仓库根：动作固定作用于 xxyi，
// 调用方传了别的仓库名直接报错（不能悄悄忽略调用方的选择）；
// RootOverride 非空就用它（须是已存在的目录），否则取 <home>/xxyi-work/xxyi。
func xxyiRepoRoot(home string, p Params) (string, error) {
	if p.Repo != "" && p.Repo != xxyiRepo {
		return "", fmt.Errorf("该动作固定作用于 %q 仓库，收到 %q", xxyiRepo, p.Repo)
	}
	if p.RootOverride == "" {
		return RepoRoot(home, xxyiRepo, DefaultRepos)
	}
	st, err := os.Stat(p.RootOverride)
	if err != nil {
		return "", fmt.Errorf("仓库根覆盖目录不可用 %q: %w", p.RootOverride, err)
	}
	if !st.IsDir() {
		return "", fmt.Errorf("仓库根覆盖路径不是目录: %q", p.RootOverride)
	}
	return p.RootOverride, nil
}

// xxyiMacDir 取编译类动作的工作目录：ResolveInRepo 解析 <xxyi 仓库根>/mac，
// 挡住 mac 本身是指向仓库外的符号链接；解析结果须存在且是目录。
func xxyiMacDir(home string, p Params) (string, error) {
	root, err := xxyiRepoRoot(home, p)
	if err != nil {
		return "", err
	}
	dir, err := ResolveInRepo(root, "mac")
	if err != nil {
		return "", fmt.Errorf("mac 目录不可用: %w", err)
	}
	st, err := os.Stat(dir)
	if err != nil {
		return "", fmt.Errorf("mac 目录不可用 %q: %w", dir, err)
	}
	if !st.IsDir() {
		return "", fmt.Errorf("mac 路径不是目录: %q", dir)
	}
	return dir, nil
}
