package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestTmuxSessionArgsOrder(t *testing.T) {
	if got := strings.Join(tmuxSessionArgs("%3"), " "); got != "display-message -p -t %3 #S" {
		t.Errorf("参数 %q：-t 要在格式串前面", got)
	}
	if got := strings.Join(tmuxSessionArgs(""), " "); got != "display-message -p #S" {
		t.Errorf("参数 %q", got)
	}
}

func TestCheckSessionName(t *testing.T) {
	for _, ok := range []string{"main", "codex1", "a.b_c-d", strings.Repeat("x", 64)} {
		if err := checkSessionName(ok); err != nil {
			t.Errorf("%q 应合法: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "中文", "a b", "a/b", strings.Repeat("x", 65)} {
		if err := checkSessionName(bad); err == nil {
			t.Errorf("%q 应不合法", bad)
		}
	}
}

func TestTmuxEnvSession(t *testing.T) {
	old := tmuxSession
	t.Cleanup(func() { tmuxSession = old })
	tmuxSession = func() (string, error) { return "sess1", nil }
	if name, err := tmuxEnvSession(); err != nil || name != "sess1" {
		t.Errorf("得到 %q,%v", name, err)
	}
	tmuxSession = func() (string, error) { return "", errors.New("tmux 不答") }
	if _, err := tmuxEnvSession(); err == nil || !strings.Contains(err.Error(), "tmux 不答") {
		t.Errorf("得到 %v", err)
	}
}

// 没有参数、未知命令、已拆除的老子命令：都打用法、退出码 2，且不碰标准输入。
func TestRunUsageAndRemovedCommands(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"nope"},
		{"send", "mac:x", "hi"}, {"reply", "1"}, {"recall", "1"}, {"inbox"}, {"verify", "1"}, {"who"},
		{"mcp"}, {"approve-hook"}, {"macexec"},
	} {
		var out, errOut bytes.Buffer
		if code := run(args, nil, &out, &errOut); code != exitErr {
			t.Errorf("%v：退出码 %d，want %d", args, code, exitErr)
		}
		if out.Len() != 0 || !strings.Contains(errOut.String(), "用法：peerctl state") {
			t.Errorf("%v：stdout %q stderr %q", args, out.String(), errOut.String())
		}
	}
}
