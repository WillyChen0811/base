package main

// approve-hook：给 Claude Code / Codex 的 PermissionRequest 钩子用，走 XXYI 通用审批通道。
// 从标准输入读钩子 JSON，连同自己的地址/agent/cwd 提交 POST /approval/ask（要证书签名），
// 阻塞等用户在 XXYI 里决定（服务端最多 10 分钟，这里 HTTP 超时留 11 分钟）。
// 拿不到决定——没有地址、没有证书、连不上、断开、非 200、超时——一律什么都不输出、退出 0：
// 让 agent 退回它自己终端里的确认（用户已同意这条规则）。原因记进 msg-hook.log 便于排查。
// 拿到了决定才向标准输出打印钩子决定 JSON（格式即 support.HookOutput，两边同源）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/WillyChen0811/base/go/support"
)

// 钩子输入读取与 HTTP 等待的上限；测试里可调小。
var (
	approveStdinTimeout = 5 * time.Second  // 读完标准输入钩子 JSON 的上限（goroutine 里读，不拖死钩子）
	approveHTTPTimeout  = 11 * time.Minute // 比服务端 approvalWait（10 分钟）长一点，让服务端先回 decided=false
)

// defaultApproveDeny 是用户在 XXYI 里点了拒绝但没写原因时的兜底文案。
const defaultApproveDeny = "用户在 XXYI 里拒绝"

// approveHookInput 是从钩子标准输入里读到的原始 JSON 与关心的两个字段。
type approveHookInput struct {
	raw   json.RawMessage
	cwd   string
	agent string
}

// readApproveHook 限时读完标准输入里的钩子 JSON：goroutine 里 io.ReadAll，
// 超过 approveStdinTimeout 没读完（agent 不关 stdin）就返回错误，绝不傻等。
func readApproveHook(ctx context.Context, stdin io.Reader) (approveHookInput, error) {
	type got struct {
		b   []byte
		err error
	}
	readCtx, cancel := context.WithTimeout(ctx, approveStdinTimeout)
	defer cancel()
	ch := make(chan got, 1)
	go func() {
		b, err := io.ReadAll(stdin)
		ch <- got{b, err}
	}()
	select {
	case g := <-ch:
		if g.err != nil {
			return approveHookInput{}, g.err
		}
		if len(strings.TrimSpace(string(g.b))) == 0 {
			return approveHookInput{}, errors.New("标准输入没有钩子输入")
		}
		if !json.Valid(g.b) {
			return approveHookInput{}, errors.New("标准输入不是合法 JSON")
		}
		var meta struct {
			Cwd   string `json:"cwd"`
			Agent string `json:"agent"`
		}
		if err := json.Unmarshal(g.b, &meta); err != nil {
			return approveHookInput{}, err
		}
		return approveHookInput{raw: json.RawMessage(g.b), cwd: meta.Cwd, agent: meta.Agent}, nil
	case <-readCtx.Done():
		return approveHookInput{}, readCtx.Err()
	}
}

// parseApproveArgs 解析 approve-hook 的参数：只认 --agent，其余（含多余位置参数）由调用方当不合法处理。
func parseApproveArgs(args []string) (agent string, err error) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--agent":
			if i+1 >= len(args) {
				return "", errors.New("--agent 后面要跟值")
			}
			agent = args[i+1]
			i++
		case strings.HasPrefix(a, "--"):
			return "", fmt.Errorf("未知选项 %s", a)
		default:
			return "", fmt.Errorf("多余参数 %s", a)
		}
	}
	return agent, nil
}

// cmdApproveHook 是 approve-hook 子命令主体：任何失败都不输出、退出 0（退回终端确认），
// 只在拿到明确决定时打印钩子决定 JSON。原因一律记 msg-hook.log；日志失败忽略。
func cmdApproveHook(args []string, stdin io.Reader, stdout io.Writer) int {
	fail := func(reason string) int {
		hookLogCmd("approve-hook", args, reason)
		return exitOK
	}

	agent, err := parseApproveArgs(args)
	if err != nil {
		return fail("参数不合法：" + err.Error())
	}
	in, err := readApproveHook(context.Background(), stdin)
	if err != nil {
		return fail("读钩子输入失败：" + err.Error())
	}
	from, err := selfAddr()
	if err != nil {
		return fail("取地址失败：" + err.Error())
	}
	cwd := in.cwd
	if cwd == "" {
		if wd, err := os.Getwd(); err == nil {
			cwd = wd
		}
	}
	if agent == "" {
		agent = in.agent
	}
	if agent == "" {
		agent = "claude"
	}

	ctx, cancel := context.WithTimeout(context.Background(), approveHTTPTimeout)
	defer cancel()
	c, err := connect(ctx, approveHTTPTimeout)
	if err != nil {
		return fail("addr=" + from + " 连不上：" + err.Error())
	}
	body, err := json.Marshal(map[string]any{"addr": from, "agent": agent, "cwd": cwd, "hook": in.raw})
	if err != nil {
		return fail("拼请求正文失败：" + err.Error())
	}
	b, err := c.do(ctx, http.MethodPost, c.path("/approval/ask", nil), body)
	if err != nil {
		return fail("addr=" + from + " 审批请求失败：" + err.Error())
	}
	var resp struct {
		Decided bool   `json:"decided"`
		Allow   bool   `json:"allow"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(b, &resp); err != nil {
		return fail("服务端响应看不懂：" + err.Error())
	}
	if !resp.Decided {
		return fail("addr=" + from + " 没拿到决定（超时或已取消），退回终端确认")
	}
	d := support.Decision{Allow: resp.Allow, Message: resp.Message}
	if !d.Allow && d.Message == "" {
		d.Message = defaultApproveDeny
	}
	out := support.HookOutput(d)
	if _, err := stdout.Write(append(out, '\n')); err != nil {
		return fail("写标准输出失败：" + err.Error())
	}
	hookLogCmd("approve-hook", args, "addr="+from+" 已决定 allow="+boolText(resp.Allow))
	return exitOK
}

func boolText(v bool) string {
	if v {
		return "true"
	}
	return "false"
}
