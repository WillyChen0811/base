package agentstate

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/WillyChen0811/base/go/projsync"
)

const (
	claudeEnd  = `{"type":"user","message":{"content":"hi"}}` + "\n" + `{"type":"assistant","message":{"stop_reason":"end_turn"}}` + "\n"
	claudeTool = `{"type":"user","message":{"content":"hi"}}` + "\n" + `{"type":"assistant","message":{"stop_reason":"tool_use"}}` + "\n"
	claudeUser = claudeEnd + `{"type":"user","message":{"content":[{"type":"tool_result"}]}}` + "\n"
)

func claudeIn(screen, log string) PaneInput {
	return PaneInput{Command: "claude", Screen: screen, Log: LogInput{Found: true, Content: []byte(log)}}
}

func TestJudgePaneClaude(t *testing.T) {
	cases := []struct {
		name string
		in   PaneInput
		want PaneState
	}{
		{"有 esc", claudeIn("working... esc to interrupt", claudeEnd), Busy}, // 冲突以 Busy 为准
		{"有 esc 无记录", PaneInput{Command: "claude", Screen: "esc to interrupt"}, Busy},
		{"end_turn", claudeIn("> ", claudeEnd), Done},
		{"最后 tool_use", claudeIn("> ", claudeTool), Unknown},
		{"end_turn 后又有 user", claudeIn("> ", claudeUser), Unknown},
		{"无记录", PaneInput{Command: "claude", Screen: "> "}, Unknown},
		{"记录读不了", PaneInput{Command: "claude", Screen: "> ", Log: LogInput{Found: true, ReadErr: true}}, Unknown},
		{"屏幕读不了", PaneInput{Command: "claude", ScreenErr: true, Log: LogInput{Found: true, Content: []byte(claudeEnd)}}, Unknown},
		{"空记录", claudeIn("> ", ""), Unknown},
		{"shell 无子进程", PaneInput{Command: "bash", ChildrenKnown: true}, Idle},
		{"shell 有子进程", PaneInput{Command: "zsh", ChildrenKnown: true, HasChildren: true}, Unknown},
		{"shell 进程树不可读", PaneInput{Command: "sh"}, Unknown},
		{"窗格已死", PaneInput{Command: "vim", Dead: true}, Idle},
		{"未知程序", PaneInput{Command: "vim"}, Unknown},
	}
	for _, c := range cases {
		got, reason := JudgePane(c.in)
		if got != c.want || reason == "" {
			t.Errorf("%s: got %s (%s), want %s", c.name, got, reason, c.want)
		}
	}
}

func TestJudgePaneCodex(t *testing.T) {
	started := `{"type":"event_msg","payload":{"type":"task_started"}}` + "\n"
	complete := `{"type":"event_msg","payload":{"type":"task_complete"}}` + "\n"
	in := func(screen, log string) PaneInput {
		return PaneInput{Command: "codex", Screen: screen, Log: LogInput{Found: true, Content: []byte(log)}}
	}
	cases := []struct {
		name string
		in   PaneInput
		want PaneState
	}{
		{"started+complete", in("> ", started+complete), Done},
		{"complete 后又 started", in("> ", started+complete+started), Unknown},
		{"只有 started", in("> ", started), Unknown},
		{"有 esc 冲突", in("esc to interrupt", started+complete), Busy},
		{"无记录", PaneInput{Command: "codex", Screen: "> "}, Unknown},
	}
	for _, c := range cases {
		if got, _ := JudgePane(c.in); got != c.want {
			t.Errorf("%s: got %s want %s", c.name, got, c.want)
		}
	}
}

func TestSummarize(t *testing.T) {
	p := func(s ...PaneState) []Pane {
		var r []Pane
		for _, x := range s {
			r = append(r, Pane{State: x})
		}
		return r
	}
	cases := []struct {
		in   []Pane
		want Verdict
	}{
		{nil, VerdictNoAgent},
		{p(Idle, Done), VerdictFinished},
		{p(Idle, Unknown), VerdictUnknown},
		{p(Unknown, Busy, Done), VerdictBusy},
	}
	for i, c := range cases {
		if got := Summarize(c.in); got != c.want {
			t.Errorf("case %d: got %s want %s", i, got, c.want)
		}
	}
}

func TestInWorktree(t *testing.T) {
	base := t.TempDir()
	a := filepath.Join(base, "a")
	for _, d := range []string{"xxyi/sub", "xxyi2"} {
		if err := os.MkdirAll(filepath.Join(a, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	root := filepath.Join(a, "xxyi")
	if !InWorktree(root, root) || !InWorktree(root, filepath.Join(root, "sub")) {
		t.Error("根与子目录应匹配")
	}
	if InWorktree(root, filepath.Join(a, "xxyi2")) {
		t.Error("xxyi2 不应匹配 xxyi")
	}
	if InWorktree(root, a) {
		t.Error("上级目录不应匹配")
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if !InWorktree(root, filepath.Join(link, "sub")) || !InWorktree(link, filepath.Join(root, "sub")) {
		t.Error("符号链接解析后应匹配")
	}
}

func TestFindLogs(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(t.TempDir(), "proj")
	_ = os.MkdirAll(dir, 0o755)

	if l := findClaudeLog(home, dir); l.Found {
		t.Error("没有记录目录应为 Found=false")
	}
	cd := filepath.Join(home, ".claude", "projects", projsync.ClaudeDirName(dir))
	_ = os.MkdirAll(cd, 0o755)
	old := filepath.Join(cd, "old.jsonl")
	_ = os.WriteFile(old, []byte(claudeTool), 0o644)
	_ = os.Chtimes(old, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour))
	_ = os.WriteFile(filepath.Join(cd, "new.jsonl"), []byte(claudeEnd), 0o644)
	l := findClaudeLog(home, dir)
	if !l.Found || !claudeEndTurn(l.Content) {
		t.Error("应取最近修改的记录")
	}
	// 记录目录不可读 → ReadErr
	if os.Geteuid() != 0 {
		_ = os.Chmod(cd, 0)
		l = findClaudeLog(home, dir)
		_ = os.Chmod(cd, 0o755)
		if !l.Found || !l.ReadErr {
			t.Error("读不了应标 ReadErr")
		}
	}

	sd := filepath.Join(home, ".codex", "sessions", "2026", "10", "16")
	_ = os.MkdirAll(sd, 0o755)
	meta := func(cwd string) string {
		return fmt.Sprintf(`{"type":"session_meta","payload":{"cwd":%q}}`+"\n", cwd)
	}
	tc := `{"type":"event_msg","payload":{"type":"task_started"}}` + "\n" + `{"type":"event_msg","payload":{"type":"task_complete"}}` + "\n"
	_ = os.WriteFile(filepath.Join(sd, "rollout-other.jsonl"), []byte(meta("/elsewhere")), 0o644)
	_ = os.WriteFile(filepath.Join(sd, "rollout-mine.jsonl"), []byte(meta(dir)+tc), 0o644)
	cl := findCodexLog(home, dir)
	if !cl.Found || !codexTaskComplete(cl.Content) {
		t.Error("应找到 cwd 匹配的 codex 记录")
	}
	if findCodexLog(home, "/nowhere").Found {
		t.Error("cwd 不匹配不应找到")
	}
}

// ── 集成：隔离 tmux 服务器 ──

func isolated(t *testing.T) (string, func(args ...string) error) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("没有 tmux")
	}
	name := fmt.Sprintf("xxyi-agentstate-test-%d", os.Getpid())
	run := func(args ...string) error {
		cmd := exec.Command("tmux", append([]string{"-L", name}, args...)...)
		var env []string
		for _, e := range os.Environ() {
			if !strings.HasPrefix(e, "TMUX=") && !strings.HasPrefix(e, "TMUX_PANE=") {
				env = append(env, e)
			}
		}
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%v: %s", err, out)
		}
		return nil
	}
	t.Cleanup(func() { _ = run("kill-server") }) // 只杀隔离服务器
	return "tmux -L " + name, run
}

func waitCmd(t *testing.T, opt Options, dir, cmd string) (Verdict, []Pane) {
	t.Helper()
	var v Verdict
	var ps []Pane
	for i := 0; i < 50; i++ {
		var err error
		v, ps, err = Judge(context.Background(), dir, opt)
		if err != nil {
			t.Fatal(err)
		}
		if len(ps) == 1 && ps[0].Command == cmd {
			return v, ps
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("窗格前台没变成 %s: %+v", cmd, ps)
	return "", nil
}

func TestJudgeIntegration(t *testing.T) {
	tm, run := isolated(t)
	ws := t.TempDir()
	home := t.TempDir()
	opt := Options{Tmux: tm, Home: home}

	// 服务器还没起：没有窗格 → NoAgent
	if v, ps, err := Judge(context.Background(), ws, opt); err != nil || v != VerdictNoAgent || len(ps) != 0 {
		t.Fatalf("无服务器: %s %v %v", v, ps, err)
	}

	// 其它目录里的窗格不算
	other := t.TempDir()
	if err := run("new-session", "-d", "-s", "o", "-c", other, "sleep 300"); err != nil {
		t.Fatal(err)
	}
	if v, ps, _ := Judge(context.Background(), ws, opt); v != VerdictNoAgent || len(ps) != 0 {
		t.Fatalf("别处窗格: %s %v", v, ps)
	}

	// 其它程序 → Unknown
	if err := run("new-session", "-d", "-s", "w1", "-c", ws, "sleep 300"); err != nil {
		t.Fatal(err)
	}
	v, ps := waitCmd(t, opt, ws, "sleep")
	if v != VerdictUnknown || ps[0].State != Unknown {
		t.Errorf("sleep: %s %+v", v, ps)
	}
	if err := run("kill-session", "-t", "w1"); err != nil {
		t.Fatal(err)
	}

	// 空闲 sh → Idle → Finished
	if err := run("new-session", "-d", "-s", "w2", "-c", ws, "sh"); err != nil {
		t.Fatal(err)
	}
	v, ps = waitCmd(t, opt, ws, "sh")
	if v != VerdictFinished || ps[0].State != Idle {
		t.Errorf("sh: %s %+v", v, ps)
	}

	// 进程树读不出来 → Unknown，不是 Idle
	opt2 := opt
	opt2.ProcTree = func() (*ProcTable, error) { return nil, fmt.Errorf("boom") }
	if v, ps, _ := Judge(context.Background(), ws, opt2); v != VerdictUnknown || ps[0].State != Unknown {
		t.Errorf("进程树失败: %s %+v", v, ps)
	}
}

func TestJudgeTmuxUnavailable(t *testing.T) {
	v, _, err := Judge(context.Background(), t.TempDir(), Options{Tmux: "/nonexistent/tmux-xyz"})
	if err == nil || v != VerdictUnknown {
		t.Errorf("tmux 不可用应为 Unknown+错误: %s %v", v, err)
	}
}

func TestIdentifyByCmdline(t *testing.T) {
	cases := []struct {
		name  string
		cmd   string
		procs []string
		known bool
		want  PaneState
		why   string
	}{
		{"node+codex", "node", []string{"node /usr/lib/node_modules/@openai/codex/bin/codex.js --yolo"}, true, Done, "命令行含 codex"},
		{"node+codex 子进程", "node", []string{"node /x/y.js", "/opt/bin/codex --foo"}, true, Done, "命令行含 codex"},
		{"node+claude-code", "node", []string{"node /usr/lib/node_modules/@anthropic-ai/claude-code/cli.js"}, true, Done, "命令行含 claude"},
		{"版本号+子进程 claude", "2.1.300", []string{"-bash", "/home/u/.local/bin/claude --resume"}, true, Done, "命令行含 claude"},
		{"node 两者都无", "node", []string{"node server.js", "vim claude-notes.txt"}, true, Unknown, ""},
		{"两者都出现", "node", []string{"node @openai/codex/bin/codex.js", "claude"}, true, Unknown, "同时出现"},
		{"读不到命令行", "node", nil, false, Unknown, "读不到"},
	}
	for _, c := range cases {
		in := PaneInput{Command: c.cmd, Procs: c.procs, ProcsKnown: c.known, Screen: "> "}
		if strings.Contains(c.name, "codex") {
			in.Log = LogInput{Found: true, Content: []byte(realCodexShape)}
		} else {
			in.Log = LogInput{Found: true, Content: []byte(claudeEnd)}
		}
		got, reason := JudgePane(in)
		if got != c.want || !strings.Contains(reason, c.why) {
			t.Errorf("%s: got %s (%s)", c.name, got, reason)
		}
	}
	got, reason := JudgePane(PaneInput{Command: "node", Procs: []string{"node @openai/codex/bin/codex.js"}, ProcsKnown: true, Screen: "esc to interrupt"})
	if got != Busy || !strings.Contains(reason, "命令行含 codex") {
		t.Errorf("node+codex busy: %s %s", got, reason)
	}
}

// 真实 codex rollout 的结构（脱敏，只留 type / payload.type / cwd；首行 session_meta，
// task_complete 之后还会有 thread_settings_applied 等事件）。
const realCodexShape = `{"type":"session_meta","payload":{"cwd":"/work/ws","originator":"codex-tui"}}
{"type":"event_msg","payload":{"type":"task_started"}}
{"type":"response_item","payload":{"type":"message"}}
{"type":"event_msg","payload":{"type":"token_count"}}
{"type":"event_msg","payload":{"type":"task_complete"}}
{"type":"event_msg","payload":{"type":"thread_settings_applied"}}
`

func TestCodexRealShape(t *testing.T) {
	if !codexTaskComplete([]byte(realCodexShape)) {
		t.Error("真实结构应判 task_complete")
	}
	home := t.TempDir()
	dir := t.TempDir()
	sd := filepath.Join(home, ".codex", "sessions", "2026", "10", "15")
	_ = os.MkdirAll(sd, 0o755)
	first := fmt.Sprintf(`{"type":"session_meta","payload":{"cwd":%q,"base_instructions":{"text":%q}}}`+"\n", dir, strings.Repeat("x", 200<<10))
	_ = os.WriteFile(filepath.Join(sd, "rollout-a.jsonl"), []byte(first+realCodexShape[strings.Index(realCodexShape, "\n")+1:]), 0o644)
	l := findCodexLog(home, dir)
	if !l.Found || !codexTaskComplete(l.Content) {
		t.Error("长首行的 rollout 应能按 cwd 找到并判 Done")
	}
}
