package main

// mac_* 工具：从这边经协助远端（wgtund 的 remoteMux）操作 Mac 上的执行接口 /exec/*。
// 地址与令牌走现有 connect()（每次现读 endpoint.json）、发一次请求就返回：不轮询、不重试；
// 唯一例外是 401（wgtund 重启后新令牌就在 endpoint.json 里，重读入口再试一次是免费的，
// 而对跑着的命令不能重试——服务端可能已经起了动作）。
// 动作跑完（含非零退出与超时被杀）都是数据：文字返回退出码/用时/输出尾部；
// 报错只发生在没跑成：忙（409）、参数/路径错（400）、令牌不对（401）、连不上（connError）。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/WillyChen0811/base/go/macexec"
)

var (
	// macRunTimeout：动作超时 + 一分钟收尾（超时秒数以 go/macexec 为准，取够用的最大一档 build_app 20 分钟）。
	macRunTimeout = 21 * time.Minute
	// macQuickTimeout 是截图/日志/装 app 起手的 HTTP 超时（约定：60s）。
	macQuickTimeout = 60 * time.Second
	// screenshotMax 是截图响应读取上限（服务端已限宽 1600，防异常响应撑爆内存）。
	screenshotMax int64 = 8 << 20
)

var pngSignature = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'} // PNG 完整 8 字节签名（RFC 2083），只比前 4 字节太松

// macNodeTestName 与服务端 actions 同一套规则：测试名只许小写字母数字下划线。
var macNodeTestName = regexp.MustCompile(`^[a-z0-9_]+$`)

// macBranchName 是 branch 允许的字符集（与服务端 ValidBranch 同一套；@{ 因 { 不在集合内已被挡）。
var macBranchName = regexp.MustCompile(`^[A-Za-z0-9._/\-]+$`)

// macNoArgs 是无参数工具的公共 inputSchema。
var macNoArgs = map[string]any{"type": "object", "properties": map[string]any{}, "required": []string{}}

// macBranchProp 是编译类工具共用的可选 branch 属性。
var macBranchProp = map[string]any{"type": "string", "description": "可选：要编译的分支。给了就在专用目录 ~/xxyi-work/.xxyi-branch-build 里 fetch 并检出该分支再跑，不切换 Mac 上 xxyi 工作区的分支；不给就编当前检出的分支"}

// macBranchOnly 是只有可选 branch 参数的 inputSchema（mac_build/mac_selftest/mac_build_app）。
var macBranchOnly = map[string]any{"type": "object", "properties": map[string]any{"branch": macBranchProp}, "required": []string{}}

// mcpMacTools 是加进 tools/list 的八个 mac_* 工具。
var mcpMacTools = []map[string]any{
	{"name": "mac_git_status", "description": "看 Mac 上某个仓库的 git status（porcelain 格式，含分支行）。",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"repo": map[string]any{"type": "string", "enum": macexec.DefaultRepos, "description": "仓库名"}}, "required": []string{"repo"}}},
	{"name": "mac_build", "description": "在 Mac 上跑 swift build（xxyi/mac），同步等到跑完（上限 15 分钟）；返回退出码、用时与输出末尾，构建失败也是文字（含错误尾部）。支持 branch。",
		"inputSchema": macBranchOnly},
	{"name": "mac_selftest", "description": "在 Mac 上跑 swift run xxyi-selftest（xxyi/mac），同步等到跑完（上限 15 分钟）；返回退出码、用时与输出末尾。支持 branch。",
		"inputSchema": macBranchOnly},
	{"name": "mac_node_test", "description": "在 Mac 上跑 node tests/js/<name>.test.js（xxyi 仓库），同步等到跑完（上限 5 分钟）；name 只许小写字母数字下划线。返回退出码、用时与输出末尾。支持 branch。",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string", "description": "测试文件名（不带 .test.js 后缀）"}, "branch": macBranchProp}, "required": []string{"name"}}},
	{"name": "mac_build_app", "description": "在 Mac 上跑 mac/build.sh 打包 app（xxyi 仓库），同步等到跑完（上限 20 分钟）；返回退出码、用时与输出末尾；用 branch 时产物在 <worktree>/mac/build/XXYI.app（输出第一行会写明目录）。",
		"inputSchema": macBranchOnly},
	{"name": "mac_install_app", "description": "让 Mac 退出正在运行的 XXYI，改为打开打包目录里刚打好的新版（不覆盖桌面上的 App）：起脚本后立即返回，不等装完（脚本会重启 XXYI 与 wgtund，协助随之断开重开）。看结果请用 mac_screenshot 与 mac_logs。",
		"inputSchema": macNoArgs},
	{"name": "mac_screenshot", "description": "截 Mac 的整屏（PNG 图片）。", "inputSchema": macNoArgs},
	{"name": "mac_logs", "description": "看 Mac 上日志的末尾若干行：name 取 service（协助服务）/ wgtund（隧道守护）。",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
			"name":  map[string]any{"type": "string", "enum": []string{"service", "wgtund"}, "description": "日志名"},
			"lines": map[string]any{"type": "integer", "description": "末尾行数，1..500，默认 200"},
		}, "required": []string{"name"}}},
}

// allMcpTools 是 tools/list 的完整名单：五个消息工具 + 八个 mac 工具。
func allMcpTools() []map[string]any {
	return append(append([]map[string]any{}, mcpTools...), mcpMacTools...)
}

// macRunActions 是"经 /exec/run 同步跑动作"的工具名 → 动作名映射；
// 其余两个 mac 工具（install/screenshot、logs）各有专门的接口，不在这张表里。
var macRunActions = map[string]string{
	"mac_git_status": "git_status",
	"mac_build":      "swift_build",
	"mac_selftest":   "selftest",
	"mac_build_app":  "build_app",
	"mac_node_test":  "node_test",
}

// runMacTool 是 mac_* 文字类工具的实现（mac_screenshot 在 callTool 里走图片通道，不经过这里）；
// HTTP 截止在 macCall 里按 path 定，这里只管参数校验；名单与服务端 actions 同一套。
func runMacTool(ctx context.Context, name string, args map[string]any) (string, error) {
	var repo, testName, branch string
	var err error
	if action, ok := macRunActions[name]; ok {
		if name != "mac_git_status" {
			if branch, err = macBranchArg(args); err != nil {
				return "", err
			}
		}
		switch name {
		case "mac_git_status":
			if repo, err = macRepoArg(args); err != nil {
				return "", err
			}
		case "mac_node_test":
			if testName, err = argString(args, "name", true); err != nil {
				return "", err
			}
			if !macNodeTestName.MatchString(testName) {
				return "", fmt.Errorf("测试名 %q 不合法：只许小写字母、数字、下划线", testName)
			}
		}
		return macExecRun(ctx, action, repo, testName, branch)
	}
	switch name {
	case "mac_install_app":
		return macInstall(ctx)
	case "mac_logs":
		return macLogs(ctx, args)
	default:
		return "", fmt.Errorf("没有这个工具 %q", name)
	}
}

// macRepoArg 取必填的 repo 并按服务端同一套名单校验。
func macRepoArg(args map[string]any) (string, error) {
	repo, err := argString(args, "repo", true)
	if err != nil {
		return "", err
	}
	if !slices.Contains(macexec.DefaultRepos, repo) {
		return "", fmt.Errorf("仓库 %q 不在允许名单里（可用：%s）", repo, strings.Join(macexec.DefaultRepos, "/"))
	}
	return repo, nil
}

// macBranchArg 取可选的 branch 并按服务端同一套规则做轻量校验（以服务端为准）：
// 1..200 字节，只许 ASCII 字母数字和 . _ / -，不以 - 或 / 开头，不以 / 或 .lock 结尾，不含 ".."、"//"、"@{"。
func macBranchArg(args map[string]any) (string, error) {
	branch, err := argString(args, "branch", false)
	if err != nil || branch == "" {
		return "", err
	}
	if len(branch) > 200 || !macBranchName.MatchString(branch) ||
		strings.HasPrefix(branch, "-") || strings.HasPrefix(branch, "/") ||
		strings.HasSuffix(branch, "/") || strings.HasSuffix(branch, ".lock") ||
		strings.Contains(branch, "..") || strings.Contains(branch, "//") {
		return "", fmt.Errorf("分支名 %q 不合法：只许 ASCII 字母数字和 . _ / -，不以 - 或 / 开头，不含 .. 和 //", branch)
	}
	return branch, nil
}

// macCall 是所有 mac 请求的统一入口：按 path 定 HTTP 截止（/exec/run 长、其余 60s），
// 现读入口发一次请求；只有令牌不对（401）才重读入口再试一次（见文件头说明），其余都不重试。
func macCall(ctx context.Context, path string, fn func(c *client) ([]byte, error)) ([]byte, error) {
	timeout := macQuickTimeout
	if path == "/exec/run" {
		timeout = macRunTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	b, err := macOnce(ctx, timeout, fn)
	var se *statusError
	if err != nil && errors.As(err, &se) && se.status == http.StatusUnauthorized {
		return macOnce(ctx, timeout, fn)
	}
	return b, err
}

// macOnce 用现读的入口发一次请求；客户端超时与 ctx 截止一致（长动作不能被短超时掐掉）。
func macOnce(ctx context.Context, timeout time.Duration, fn func(c *client) ([]byte, error)) ([]byte, error) {
	c, err := connect(ctx, timeout)
	if err != nil {
		return nil, err
	}
	return fn(c)
}

// macExecRun 跑一次动作并渲染结果：非零退出码与超时都是数据（文字），
// 忙/参数错/连不上这些"没跑成"才走错误通道（Error() 已带服务端原因与状态码）。
func macExecRun(ctx context.Context, action, repo, name, branch string) (string, error) {
	req := map[string]any{"action": action, "repo": repo, "name": name}
	if branch != "" {
		req["branch"] = branch
	}
	body, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	b, err := macCall(ctx, "/exec/run", func(c *client) ([]byte, error) {
		return c.do(ctx, http.MethodPost, c.path("/exec/run", nil), body)
	})
	if err != nil {
		return "", err
	}
	var r struct {
		ExitCode int     `json:"exit_code"`
		TimedOut bool    `json:"timed_out"`
		Seconds  float64 `json:"seconds"`
		Tail     string  `json:"tail"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return "", fmt.Errorf("Mac 端 run 响应格式不对：%w", err)
	}
	head := fmt.Sprintf("退出码 %d，用时 %.1fs", r.ExitCode, r.Seconds)
	if r.TimedOut {
		head += "（超时被终止）"
	}
	if r.Tail == "" {
		return head, nil
	}
	return head + "\n输出（末尾）：\n" + r.Tail, nil
}

// macInstall 起装 app 脚本：服务端脱开本进程跑脚本并立即返回，这里只确认已起手。
func macInstall(ctx context.Context) (string, error) {
	b, err := macCall(ctx, "/exec/install", func(c *client) ([]byte, error) {
		return c.do(ctx, http.MethodPost, c.path("/exec/install", nil), nil)
	})
	if err != nil {
		return "", err
	}
	var r struct {
		Started bool `json:"started"`
	}
	if err := json.Unmarshal(b, &r); err != nil || !r.Started {
		return "", errors.New("Mac 端 install 响应格式不对")
	}
	return "已让 Mac 起安装脚本（会重启 XXYI 与 wgtund）；用 mac_screenshot 和 mac_logs 看结果", nil
}

// macLogs 取远端日志末尾（名单与行数先在本地拒，合法才发 GET）。
func macLogs(ctx context.Context, args map[string]any) (string, error) {
	name, err := argString(args, "name", true)
	if err != nil {
		return "", err
	}
	if name != "service" && name != "wgtund" {
		return "", fmt.Errorf("name 只能是 service/wgtund，实际是 %q", name)
	}
	lines, err := argInt(args, "lines", 200)
	if err != nil {
		return "", err
	}
	if lines < 1 || lines > 500 {
		return "", fmt.Errorf("lines 必须 1..500，实际 %d", lines)
	}
	q := url.Values{"name": {name}, "lines": {fmt.Sprint(lines)}}
	b, err := macCall(ctx, "/exec/logs", func(c *client) ([]byte, error) {
		return c.doLimited(ctx, http.MethodGet, c.path("/exec/logs", q), nil, 0)
	})
	if err != nil {
		return "", err
	}
	var r struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return "", fmt.Errorf("Mac 端 logs 响应格式不对：%w", err)
	}
	if strings.TrimSpace(r.Text) == "" {
		return fmt.Sprintf("%s 日志最近 %d 行是空的", name, lines), nil
	}
	return r.Text, nil
}

// macScreenshot 取整屏 PNG 原始字节：查完整 8 字节签名，再整图解码一次验完整性
// （截断/坏块在解码时就会报，只查前缀会把中断当成功交出去）。
func macScreenshot(ctx context.Context, args map[string]any) ([]byte, error) {
	b, err := macCall(ctx, "/exec/screenshot", func(c *client) ([]byte, error) {
		return c.doLimited(ctx, http.MethodGet, c.path("/exec/screenshot", nil), nil, screenshotMax)
	})
	if err != nil {
		return nil, err
	}
	if !bytes.HasPrefix(b, pngSignature) {
		return nil, fmt.Errorf("Mac 端截图不可用：响应不是 PNG（%d 字节）", len(b))
	}
	if _, err := png.Decode(bytes.NewReader(b)); err != nil {
		return nil, fmt.Errorf("Mac 端截图不可用：PNG 不完整（%d 字节）：%w", len(b), err)
	}
	return b, nil
}
