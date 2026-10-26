package main

// 忙闲灯（state 子命令）的测试：只写本机文件、不联网。
// tmuxSession/parentPID/listPanes/procCmdline 一律注入，绝不真执行 tmux、不读真进程；HOME 指到临时目录。

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// busyEnv 是一次忙闲测试的环境：临时家目录、一个会记请求的假服务（用来证明 state 不联网）。
type busyEnv struct {
	home string
	cap  *captured
}

// newBusyEnv 在 tmux 里（TMUX 已设、会话名为 sess）的环境。
func newBusyEnv(t *testing.T, sess string) *busyEnv {
	t.Helper()
	e := &busyEnv{home: t.TempDir(), cap: &captured{}}
	srv := httptest.NewServer(e.cap.handler())
	t.Cleanup(srv.Close)
	useEndpoint(t, srv.URL)
	t.Setenv("HOME", e.home)
	t.Setenv("XXYI_ADDR", "ubuntu:不该被用到")
	t.Setenv("TMUX", "/tmp/tmux-1000/default,1,0")
	t.Setenv("CLAUDECODE", "")
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	oldTmux, oldPP, oldCL := tmuxSession, parentPID, procCmdline
	t.Cleanup(func() { tmuxSession, parentPID, procCmdline = oldTmux, oldPP, oldCL })
	tmuxSession = func() (string, error) { return sess, nil }
	parentPID = func(int) (int, error) { return 1, nil }
	procCmdline = func(int) (string, error) { return "", errors.New("没有这个进程") }
	return e
}

func (e *busyEnv) dir() string { return filepath.Join(e.home, ".xxyi-state") }

func (e *busyEnv) file(sess string) string { return filepath.Join(e.dir(), sess+".json") }

// state 跑一次 state 子命令，返回退出码与 stdout。
func (e *busyEnv) state(t *testing.T, stdin io.Reader, args ...string) (int, string) {
	t.Helper()
	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	code := run(append([]string{"state"}, args...), stdin, out, errOut)
	if errOut.Len() != 0 {
		t.Errorf("state %v 不该有 stderr：%q", args, errOut.String())
	}
	return code, out.String()
}

func (e *busyEnv) read(t *testing.T, sess string) busyRecord {
	t.Helper()
	raw, err := os.ReadFile(e.file(sess))
	if err != nil {
		t.Fatalf("状态文件读不了：%v", err)
	}
	var rec busyRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("状态文件不是合法 JSON %q：%v", raw, err)
	}
	return rec
}

func (e *busyEnv) noFile(t *testing.T, sess string) {
	t.Helper()
	if _, err := os.Stat(e.file(sess)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("不该有状态文件 %s：%v", e.file(sess), err)
	}
}

func (e *busyEnv) noNetwork(t *testing.T) {
	t.Helper()
	if n := e.cap.count(); n != 0 {
		t.Errorf("state 不许联网，假服务收到 %d 个请求", n)
	}
}

func TestBusyStateBusyIdleWriteFile(t *testing.T) {
	e := newBusyEnv(t, "sess1")
	// 进程链 自己→90→80(claude)：认出 agent
	me := os.Getpid()
	parentPID = func(pid int) (int, error) {
		return map[int]int{me: 90, 90: 80, 80: 1}[pid], nil
	}
	procCmdline = func(pid int) (string, error) {
		switch pid {
		case me:
			return "xxyi-msg state busy", nil
		case 90:
			return "sh -c /home/u/.local/bin/xxyi-msg state busy", nil
		case 80:
			return "claude --resume", nil
		}
		return "", errors.New("没有这个进程")
	}

	before := time.Now().Unix()
	code, out := e.state(t, nil, "busy")
	if code != exitOK || out != "" {
		t.Fatalf("退出码 %d，stdout %q", code, out)
	}
	rec := e.read(t, "sess1")
	if rec.State != "busy" || rec.Agent != "claude" || rec.Time < before || rec.Time > time.Now().Unix() {
		t.Errorf("busy 记录 %+v", rec)
	}
	// 权限与残留
	if st, err := os.Stat(e.dir()); err != nil || st.Mode().Perm() != 0o700 {
		t.Errorf("目录权限 %v,%v，want 0700", st, err)
	}
	if st, err := os.Stat(e.file("sess1")); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("文件权限 %v,%v，want 0600", st, err)
	}
	if ents, _ := os.ReadDir(e.dir()); len(ents) != 1 {
		t.Errorf("目录里该只有一个文件（不留临时文件），实际 %v", ents)
	}

	// idle 覆盖 busy
	if code, out := e.state(t, nil, "idle"); code != exitOK || out != "" {
		t.Fatalf("idle 退出码 %d，stdout %q", code, out)
	}
	if rec := e.read(t, "sess1"); rec.State != "idle" || rec.Agent != "claude" {
		t.Errorf("idle 记录 %+v", rec)
	}
	if ents, _ := os.ReadDir(e.dir()); len(ents) != 1 {
		t.Errorf("覆盖后目录里该只有一个文件，实际 %v", ents)
	}
	e.noNetwork(t)
}

// 认不出 agent：JSON 里不带 agent 字段。
func TestBusyStateUnknownAgentOmitted(t *testing.T) {
	e := newBusyEnv(t, "sess1")
	if code, _ := e.state(t, nil, "busy"); code != exitOK {
		t.Fatalf("退出码 %d", code)
	}
	raw, err := os.ReadFile(e.file("sess1"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "agent") {
		t.Errorf("认不出 agent 时不该写这个字段：%s", raw)
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil || m["state"] != "busy" || m["time"] == nil {
		t.Errorf("内容 %s", raw)
	}
}

// 进程链上没有 claude/codex，但钩子环境里有 Claude Code 的变量：认作 claude。
func TestBusyStateAgentFromEnv(t *testing.T) {
	e := newBusyEnv(t, "sess1")
	t.Setenv("CLAUDE_PROJECT_DIR", "/work/proj")
	e.state(t, nil, "busy")
	if rec := e.read(t, "sess1"); rec.Agent != "claude" {
		t.Errorf("记录 %+v", rec)
	}
}

// gone 删文件；文件或目录本来就不在也不算错、不凭空建目录。
func TestBusyStateGone(t *testing.T) {
	e := newBusyEnv(t, "sess1")
	if code, out := e.state(t, nil, "gone"); code != exitOK || out != "" {
		t.Fatalf("没文件时 gone 退出码 %d，stdout %q", code, out)
	}
	if _, err := os.Stat(e.dir()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("gone 不该建目录：%v", err)
	}

	e.state(t, nil, "busy")
	// 别的会话的文件不能动
	if err := os.WriteFile(e.file("other"), []byte(`{"state":"busy","time":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _ := e.state(t, nil, "gone"); code != exitOK {
		t.Fatalf("退出码 %d", code)
	}
	e.noFile(t, "sess1")
	if _, err := os.Stat(e.file("other")); err != nil {
		t.Errorf("别的会话的文件被动了：%v", err)
	}
	// 再来一次：已经没了，仍然 0
	if code, _ := e.state(t, nil, "gone"); code != exitOK {
		t.Fatalf("重复 gone 退出码 %d", code)
	}
	e.noNetwork(t)
}

// 不在 tmux 里（认不出会话名）：什么都不写，退出 0。
func TestBusyStateNotInTmux(t *testing.T) {
	e := newBusyEnv(t, "x")
	t.Setenv("TMUX", "")
	me := os.Getpid()
	useFakeTree(t, map[int]int{me: 90, 90: 1}, "55 other\n") // 链上没有 pane
	for _, st := range []string{"busy", "idle", "gone"} {
		if code, out := e.state(t, nil, st); code != exitOK || out != "" {
			t.Errorf("%s 退出码 %d，stdout %q", st, code, out)
		}
	}
	if _, err := os.Stat(e.dir()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("不在 tmux 里不该建状态目录：%v", err)
	}
	// 列 pane 失败（没装 tmux）也一样
	listPanes = func() (string, error) { return "", errors.New("找不到 tmux") }
	if code, _ := e.state(t, nil, "busy"); code != exitOK {
		t.Errorf("列 pane 失败退出码 %d", code)
	}
	if _, err := os.Stat(e.dir()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("列 pane 失败不该建状态目录：%v", err)
	}
	e.noNetwork(t)
}

// 没有 TMUX 变量（Codex 跑钩子）：沿进程树找到 pane，按会话名写；认出 codex。
func TestBusyStateProcTreeCodex(t *testing.T) {
	e := newBusyEnv(t, "x")
	t.Setenv("TMUX", "")
	me := os.Getpid()
	useFakeTree(t, map[int]int{me: 90, 90: 80, 80: 70, 70: 1}, "70 codex1\n55 other\n")
	procCmdline = func(pid int) (string, error) {
		switch pid {
		case 90:
			return "sh -c xxyi-msg state busy", nil
		case 80:
			return "node /usr/lib/node_modules/@openai/codex/bin/codex.js", nil
		}
		return "", errors.New("没有这个进程")
	}
	if code, out := e.state(t, nil, "busy"); code != exitOK || out != "" {
		t.Fatalf("退出码 %d，stdout %q", code, out)
	}
	if rec := e.read(t, "codex1"); rec.State != "busy" || rec.Agent != "codex" {
		t.Errorf("记录 %+v", rec)
	}
	e.noNetwork(t)
}

// 会话名不合法（带空格/中文）：不写。
func TestBusyStateIllegalSessionName(t *testing.T) {
	e := newBusyEnv(t, "中文 会话")
	if code, _ := e.state(t, nil, "busy"); code != exitOK {
		t.Fatalf("退出码 %d", code)
	}
	if _, err := os.Stat(e.dir()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("会话名不合法不该写：%v", err)
	}
}

// 取会话名失败（TMUX 设了但 tmux 不答）：不写，退出 0。
func TestBusyStateTmuxFails(t *testing.T) {
	e := newBusyEnv(t, "x")
	tmuxSession = func() (string, error) { return "", errors.New("tmux 不答") }
	if code, out := e.state(t, nil, "busy"); code != exitOK || out != "" {
		t.Fatalf("退出码 %d，stdout %q", code, out)
	}
	if _, err := os.Stat(e.dir()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("不该写：%v", err)
	}
}

// idle --check：参数照收，不检查、不拦（标准输出为空），照常写 idle；不读标准输入。
func TestBusyStateCheckDoesNotBlock(t *testing.T) {
	e := newBusyEnv(t, "sess1")
	e.state(t, nil, "busy")

	var read atomic.Bool
	stdin := readFunc(func(p []byte) (int, error) { read.Store(true); return 0, io.EOF })
	code, out := e.state(t, stdin, "idle", "--check")
	if code != exitOK || out != "" {
		t.Fatalf("退出码 %d，stdout %q（--check 不拦、不输出）", code, out)
	}
	if read.Load() {
		t.Error("--check 不该读标准输入")
	}
	if rec := e.read(t, "sess1"); rec.State != "idle" {
		t.Errorf("记录 %+v", rec)
	}
	// 钩子塞了 stop_hook_active 的 JSON 也一样
	code, out = e.state(t, strings.NewReader(`{"stop_hook_active":false}`), "idle", "--check")
	if code != exitOK || out != "" {
		t.Fatalf("带 JSON：退出码 %d，stdout %q", code, out)
	}
	e.noNetwork(t)
}

// readFunc 把函数当 io.Reader 用。
type readFunc func(p []byte) (int, error)

func (f readFunc) Read(p []byte) (int, error) { return f(p) }

// --start：source 是 compact 时什么都不改；startup/resume/clear/读不到都写 idle。
func TestBusyStateStart(t *testing.T) {
	e := newBusyEnv(t, "sess1")
	e.state(t, nil, "busy")
	if code, out := e.state(t, strings.NewReader(`{"source":"compact"}`), "idle", "--start"); code != exitOK || out != "" {
		t.Fatalf("compact 退出码 %d，stdout %q", code, out)
	}
	if rec := e.read(t, "sess1"); rec.State != "busy" {
		t.Errorf("compact 时状态不该变：%+v", rec)
	}
	for _, src := range []string{"startup", "resume", "clear"} {
		e.state(t, nil, "busy")
		if code, out := e.state(t, strings.NewReader(`{"source":"`+src+`"}`), "idle", "--start"); code != exitOK || out != "" {
			t.Fatalf("%s 退出码 %d，stdout %q", src, code, out)
		}
		if rec := e.read(t, "sess1"); rec.State != "idle" {
			t.Errorf("%s 该写 idle：%+v", src, rec)
		}
	}
	// 标准输入一直不关：等满限时后照常写
	oldWait := stopHookWait
	t.Cleanup(func() { stopHookWait = oldWait })
	stopHookWait = 200 * time.Millisecond
	e.state(t, nil, "busy")
	pr, pw := io.Pipe()
	t.Cleanup(func() { pw.Close() })
	start := time.Now()
	if code, _ := e.state(t, pr, "idle", "--start"); code != exitOK {
		t.Fatalf("读不到 JSON 退出码 %d", code)
	}
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Errorf("耗时 %v，标准输入不关闭时没及时返回", d)
	}
	if rec := e.read(t, "sess1"); rec.State != "idle" {
		t.Errorf("读不到 JSON 时该照常写 idle：%+v", rec)
	}
	e.noNetwork(t)
}

// 参数不合法：静默退出 0，什么都不写。
func TestBusyStateBadArgs(t *testing.T) {
	e := newBusyEnv(t, "sess1")
	for _, args := range [][]string{{}, {"asleep"}, {"busy", "idle"}, {"--check"}, {"--start"}, {"busy", "--nope"}} {
		if code, out := e.state(t, strings.NewReader(`{"stop_hook_active":false}`), args...); code != exitOK || out != "" {
			t.Errorf("%v 退出码 %d，stdout %q", args, code, out)
		}
	}
	if _, err := os.Stat(e.dir()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("参数不合法不该写：%v", err)
	}
}

// 写不进去（~/.xxyi-state 是个普通文件）：照样静默退出 0。
func TestBusyStateWriteFailureIsSilent(t *testing.T) {
	e := newBusyEnv(t, "sess1")
	if err := os.WriteFile(e.dir(), []byte("挡路的文件"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, st := range []string{"busy", "idle", "gone"} {
		if code, out := e.state(t, nil, st); code != exitOK || out != "" {
			t.Errorf("%s 退出码 %d，stdout %q", st, code, out)
		}
	}
}

// 不再写 msg-hook.log（联网失败行没了，也不记别的）。
func TestBusyStateNoHookLog(t *testing.T) {
	e := newBusyEnv(t, "sess1")
	for _, d := range []string{".xxyi", ".xxyi-support"} {
		if err := os.MkdirAll(filepath.Join(e.home, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	e.state(t, nil, "busy")
	e.state(t, nil, "idle", "--check")
	e.state(t, nil, "gone")
	for _, d := range []string{".xxyi", ".xxyi-support"} {
		if _, err := os.Stat(filepath.Join(e.home, d, "msg-hook.log")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s 里不该有 msg-hook.log：%v", d, err)
		}
	}
}

func TestAgentOfCmdline(t *testing.T) {
	cases := []struct {
		cmdline, want string
	}{
		{"claude", "claude"},
		{"/home/u/.local/bin/claude --resume abc", "claude"},
		{"node /usr/lib/node_modules/@anthropic-ai/claude-code/cli.js", "claude"},
		{"codex", "codex"},
		{"/usr/local/bin/codex exec", "codex"},
		{"node /usr/lib/node_modules/@openai/codex/bin/codex.js --yolo", "codex"},
		{"vim claude", ""},
		{"bash -lc claude", ""},
		{"sh -c /home/u/.local/bin/xxyi-msg state busy", ""},
		{"xxyi-msg state busy", ""},
		{"tmux new-session", ""},
		{"", ""},
		{"node /x/@anthropic-ai/claude-code/cli.js /y/@openai/codex/bin/codex.js", ""}, // 两种都出现：不认
	}
	for _, c := range cases {
		if got := agentOfCmdline(c.cmdline); got != c.want {
			t.Errorf("agentOfCmdline(%q) = %q，want %q", c.cmdline, got, c.want)
		}
	}
}
