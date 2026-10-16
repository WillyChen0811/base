package macexec

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// 固定的动作名单（HTTP /exec/start 的 action、MCP mac_* 工具一一对应）。
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

// Params 是动作的参数：Repo 仓库名（仅 git_status 有意义），
// Name 是 tests/js 下的 node 测试文件名（不含 .test.js 后缀，仅 node_test 用），
// Lines 是尾部行数（取日志/输出用的选项，BuildSpec 不消费）。
type Params struct {
	Repo  string `json:"repo"`
	Name  string `json:"name"`
	Lines int    `json:"lines"`
}

// BuildSpec 把动作 + 参数拼成可直接交给 Runner.Start 的 Spec；
// 未知动作、仓库不合法、node 测试名不合法或文件不存在都返回错误，不拼半截命令。
func BuildSpec(action string, p Params, home string) (Spec, error) {
	switch action {
	case ActionGitStatus:
		root, err := RepoRoot(home, p.Repo, DefaultRepos)
		if err != nil {
			return Spec{}, err
		}
		return Spec{
			Action:  action,
			Dir:     root,
			Argv:    []string{"git", "status", "--porcelain=v1", "-b"},
			Timeout: gitStatusTimeout,
		}, nil
	case ActionSwiftBuild:
		dir, err := xxyiMacDir(home, p.Repo)
		if err != nil {
			return Spec{}, err
		}
		return Spec{Action: action, Dir: dir, Argv: []string{"swift", "build"}, Timeout: swiftTimeout}, nil
	case ActionSelftest:
		dir, err := xxyiMacDir(home, p.Repo)
		if err != nil {
			return Spec{}, err
		}
		return Spec{Action: action, Dir: dir, Argv: []string{"swift", "run", "xxyi-selftest"}, Timeout: swiftTimeout}, nil
	case ActionBuildApp:
		dir, err := xxyiMacDir(home, p.Repo)
		if err != nil {
			return Spec{}, err
		}
		return Spec{Action: action, Dir: dir, Argv: []string{"./build.sh"}, Timeout: buildAppTimeout}, nil
	case ActionNodeTest:
		root, err := xxyiRepoRoot(home, p.Repo)
		if err != nil {
			return Spec{}, err
		}
		if !nodeTestNamePattern.MatchString(p.Name) {
			return Spec{}, fmt.Errorf("node 测试文件名不合法 %q（只许小写字母/数字/下划线）", p.Name)
		}
		rel := filepath.Join("tests", "js", p.Name+".test.js")
		// ResolveInRepo 顺带完成"文件存在"校验，并挡住 tests/js 被符号链接挪出仓库的情况。
		if _, err := ResolveInRepo(root, rel); err != nil {
			return Spec{}, fmt.Errorf("node 测试文件不可用: %w", err)
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
// 调用方传了别的仓库名直接报错（不能悄悄忽略调用方的选择）。
func xxyiRepoRoot(home, repo string) (string, error) {
	if repo != "" && repo != xxyiRepo {
		return "", fmt.Errorf("该动作固定作用于 %q 仓库，收到 %q", xxyiRepo, repo)
	}
	return RepoRoot(home, xxyiRepo, DefaultRepos)
}

// xxyiMacDir 取编译类动作的工作目录 <xxyi 仓库根>/mac（须存在且是目录）。
func xxyiMacDir(home, repo string) (string, error) {
	root, err := xxyiRepoRoot(home, repo)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, "mac")
	st, err := os.Stat(dir)
	if err != nil {
		return "", fmt.Errorf("mac 目录不可用 %q: %w", dir, err)
	}
	if !st.IsDir() {
		return "", fmt.Errorf("mac 路径不是目录: %q", dir)
	}
	return dir, nil
}
