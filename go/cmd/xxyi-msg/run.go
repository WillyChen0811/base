package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// 超时与上限，测试里可调小。
var (
	stateDeadline = 2 * time.Second        // state 的总耗时上限：钩子在等，超了直接放弃
	tmuxTimeout   = 2 * time.Second        // 取 tmux 会话名的超时
	stopHookWait  = 800 * time.Millisecond // state --start 等标准输入里钩子 JSON 的上限
)

// 退出码。
const (
	exitOK  = 0
	exitErr = 2
)

// tmuxSession 取当前 tmux 会话名；测试里替换，绝不真执行 tmux。
// 在 tmux 里时指定 $TMUX_PANE：同一服务器上有别的客户端/会话也不取错。
var tmuxSession = func() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), tmuxTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tmux", tmuxSessionArgs(os.Getenv("TMUX_PANE"))...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// tmuxSessionArgs 是取会话名的 tmux 参数。-t 必须在格式串之前：tmux 把格式串之后的参数都当多余参数报错。
func tmuxSessionArgs(pane string) []string {
	if pane == "" {
		return []string{"display-message", "-p", "#S"}
	}
	return []string{"display-message", "-p", "-t", pane, "#S"}
}

const usage = "用法：xxyi-msg state busy|idle|gone [--check|--start]（钩子用，只写本机 ~/.xxyi-state）" +
	" | xxyi-msg projsync-facts|projsync-apply（stdin 读 JSON 请求，项目同步内部用）" +
	" | xxyi-msg projfiles-poll|projfiles-facts|projfiles-apply（stdin 读 JSON 请求，项目文件同步内部用）"

// errNoSession 是找不到自己所在 tmux 会话时的统一说法（state 据此什么都不写）。
var errNoSession = errors.New("找不到自己所在的 tmux 会话")

// addrNameIllegal 匹配 tmux 会话名里不支持的字符：状态文件按会话名命名，只许 ASCII 字母数字和 . _ -。
var addrNameIllegal = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// tmuxEnvSession 在 TMUX 环境变量存在时，用 tmux 取当前会话名并校验。
func tmuxEnvSession() (string, error) {
	name, err := tmuxSession()
	if err != nil {
		return "", fmt.Errorf("取 tmux 会话名失败：%w", err)
	}
	if err := checkSessionName(name); err != nil {
		return "", err
	}
	return name, nil
}

// checkSessionName 校验 tmux 会话名能不能当状态文件名用。
func checkSessionName(name string) error {
	if name == "" {
		return errors.New("tmux 会话名为空")
	}
	if len(name) > 64 || addrNameIllegal.FindString(name) != "" {
		return fmt.Errorf("tmux 会话名 %q 含不支持的字符，只允许字母数字 . _ -，请改名（tmux rename-session）", name)
	}
	return nil
}

// run 是程序主体，返回退出码。
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return exitErr
	}
	if stdin == nil {
		stdin = bytes.NewReader(nil)
	}
	var err error
	switch args[0] {
	case "state":
		return cmdState(args[1:], stdin, stdout)
	case "projsync-facts", "projsync-apply":
		// 项目同步的远程协议：只读 stdin、只写 stdout，不读配置、不联网；见 projsync/remote.go。
		err = cmdProjsync(args[0], stdin, stdout)
	case "projfiles-poll", "projfiles-facts", "projfiles-apply":
		// 项目文件同步的远程协议：同上，只读 stdin、只写 stdout；见 projsync/pfremote.go。
		err = cmdProjfiles(args[0], stdin, stdout)
	default:
		fmt.Fprintf(stderr, "未知命令 %q\n%s\n", args[0], usage)
		return exitErr
	}
	if err != nil {
		fmt.Fprintln(stderr, "错误："+err.Error())
		return exitErr
	}
	return exitOK
}
