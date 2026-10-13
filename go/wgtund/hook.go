package wgtund

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/WillyChen0811/base/go/support"
)

// hookTimeout 略短于钩子配置的 600 秒，让拒绝由我们给出而不是被 Claude Code 掐断。
const hookTimeout = 595 * time.Second

// RunHook 实现 `wgtund support-hook [地址]`：把标准输入的钩子 JSON 转给本机协助服务，
// 原样输出它的回答。任何失败都输出「拒绝」，退出码为 0（让 Claude Code 按拒绝处理）；只有标准输出写失败才返回 1。
func RunHook(args []string, stdin io.Reader, stdout io.Writer, env func(string) string) int {
	out, err := hookCall(args, stdin, env)
	if err != nil {
		out = support.HookOutput(support.Decision{Allow: false, Message: "协助服务不可用：" + err.Error()})
	}
	if _, err := stdout.Write(out); err != nil {
		return 1 // 输出写不出去：以非零退出，靠钩子的 onFailure=block 兜底拒绝
	}
	return 0
}

func hookCall(args []string, stdin io.Reader, env func(string) string) ([]byte, error) {
	addr := env("XXYI_HOOK_URL")
	if len(args) > 0 {
		addr = args[0]
	}
	if addr == "" {
		return nil, errors.New("没有钩子地址")
	}
	u, err := url.Parse(addr)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Port() == "" ||
		(u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") {
		return nil, errors.New("钩子地址不是本机")
	}
	body, err := io.ReadAll(io.LimitReader(stdin, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("读钩子输入失败: %w", err)
	}
	resp, err := (&http.Client{Timeout: hookTimeout}).Post(addr, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("连不上本机服务")
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("读回答失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("服务返回 %d", resp.StatusCode)
	}
	if !json.Valid(b) {
		return nil, errors.New("回答不是合法 JSON")
	}
	return b, nil
}
