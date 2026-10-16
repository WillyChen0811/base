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
	claudeEnd  = `{"type":"user","message":{"content":"hi"}}` + "\n" + `{"type":"assistant","timestamp":"2026-10-16T12:00:10Z","message":{"stop_reason":"end_turn"}}` + "\n"
	claudeTool = `{"type":"user","message":{"content":"hi"}}` + "\n" + `{"type":"assistant","message":{"stop_reason":"tool_use"}}` + "\n"
	claudeUser = claudeEnd + `{"type":"user","message":{"content":[{"type":"tool_result"}]}}` + "\n"
)

// procs 把命令行列表做成 Proc（第一个是 Root，启动时间已知）。
func procs(args ...string) []Proc {
	var r []Proc
	for i, a := range args {
		r = append(r, Proc{PID: 100 + i, Args: a, Start: time.Unix(1000, 0), StartKnown: true, Root: i == 0})
	}
	return r
}

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
		{"最后一行不完整", claudeIn("> ", claudeEnd+`{"type":"assistant","mess`), Unknown},
		{"不以换行结尾", claudeIn("> ", strings.TrimSuffix(claudeEnd, "\n")), Unknown},
		{"记录绑定有问题", PaneInput{Command: "claude", Screen: "> ", Log: LogInput{Found: true, Problem: "有 2 个会话记录晚于 agent 启动", Content: []byte(claudeEnd)}}, Unknown},
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
	started := `{"timestamp":"2026-10-16T12:00:00Z","type":"event_msg","payload":{"type":"task_started"}}` + "\n"
	complete := `{"timestamp":"2026-10-16T12:00:10Z","type":"event_msg","payload":{"type":"task_complete"}}` + "\n"
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
		{p(Idle), VerdictNoAgent},
		{p(Idle, Idle), VerdictNoAgent},
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

func ts(d time.Duration) string { return time.Now().Add(d).UTC().Format(time.RFC3339Nano) }

func claudeFile(home, dir, name string, first time.Time, cwd string) string {
	cd := filepath.Join(home, ".claude", "projects", projsync.ClaudeDirName(dir))
	_ = os.MkdirAll(cd, 0o755)
	p := filepath.Join(cd, name)
	body := fmt.Sprintf(`{"type":"mode"}`+"\n"+`{"type":"user","timestamp":%q,"cwd":%q,"message":{}}`+"\n"+
		`{"type":"assistant","timestamp":%q,"cwd":%q,"message":{"stop_reason":"end_turn"}}`+"\n",
		first.UTC().Format(time.RFC3339Nano), cwd, first.Add(time.Second).UTC().Format(time.RFC3339Nano), cwd)
	_ = os.WriteFile(p, []byte(body), 0o644)
	return p
}

func TestFindClaudeLogBinding(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(t.TempDir(), "proj")
	_ = os.MkdirAll(dir, 0o755)
	start := time.Now().Add(-time.Minute)

	if l := findClaudeLog(home, dir, start); l.Found {
		t.Error("没有记录目录应为 Found=false")
	}
	// 启动前的旧会话不算；启动后只有一个 → 用它
	claudeFile(home, dir, "old.jsonl", start.Add(-time.Hour), dir)
	_ = os.Chtimes(filepath.Join(home, ".claude", "projects", projsync.ClaudeDirName(dir), "old.jsonl"), start.Add(-time.Hour), start.Add(-time.Hour))
	mine := claudeFile(home, dir, "mine.jsonl", start.Add(2*time.Second), dir)
	l := findClaudeLog(home, dir, start)
	if l.Problem != "" || l.Path != mine {
		t.Errorf("应只绑定 mine: %+v", l)
	}
	if ok, _ := claudeEndTurn(l.Content); !ok {
		t.Error("内容应是 end_turn")
	}
	// 同目录第二个在启动后也有写入 → Unknown
	claudeFile(home, dir, "other.jsonl", start.Add(10*time.Second), dir)
	l = findClaudeLog(home, dir, start)
	if l.Problem == "" || !strings.Contains(l.Problem, "2 个") {
		t.Errorf("两个候选应有 Problem: %+v", l)
	}
	if st, _ := JudgePane(PaneInput{Command: "claude", Screen: "> ", Log: l}); st != Unknown {
		t.Error("多候选应 Unknown")
	}
}

// resume 的会话：首条记录早于进程启动，但启动后仍有写入 → 能绑定。
func TestFindClaudeLogResume(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(t.TempDir(), "proj")
	_ = os.MkdirAll(dir, 0o755)
	start := time.Now().Add(-time.Minute)
	p := claudeFile(home, dir, "resumed.jsonl", start.Add(-48*time.Hour), dir) // 首条记录两天前，文件刚写过
	l := findClaudeLog(home, dir, start)
	if l.Problem != "" || l.Path != p {
		t.Fatalf("resume 的会话应能绑定: %+v", l)
	}
	// 启动后一直没写入的旧会话不算
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(p, old, old)
	if findClaudeLog(home, dir, start).Problem == "" {
		t.Error("启动后没有写入的记录不应绑定")
	}
}

func TestFindClaudeLogCwdMismatch(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(t.TempDir(), "proj")
	_ = os.MkdirAll(dir, 0o755)
	start := time.Now().Add(-time.Minute)
	claudeFile(home, dir, "a.jsonl", start.Add(time.Second), "/somewhere/else")
	l := findClaudeLog(home, dir, start)
	if !strings.Contains(l.Problem, "cwd") {
		t.Errorf("cwd 不符应有 Problem: %+v", l)
	}
}

func TestFindCodexLogBinding(t *testing.T) {
	home := t.TempDir()
	dir := t.TempDir()
	start := time.Now().Add(-time.Minute)
	sd := filepath.Join(home, ".codex", "sessions", "2026", "10", "15")
	_ = os.MkdirAll(sd, 0o755)
	meta := func(cwd string, at time.Time) string {
		return fmt.Sprintf(`{"timestamp":%q,"type":"session_meta","payload":{"cwd":%q,"timestamp":%q}}`+"\n",
			at.UTC().Format(time.RFC3339Nano), cwd, at.Add(-time.Second).UTC().Format(time.RFC3339Nano))
	}
	tc := `{"timestamp":"2026-10-15T01:00:00Z","type":"event_msg","payload":{"type":"task_started"}}` + "\n" +
		`{"timestamp":"2026-10-15T01:00:09Z","type":"event_msg","payload":{"type":"task_complete"}}` + "\n"
	w := func(name, body string) {
		_ = os.WriteFile(filepath.Join(sd, name), []byte(body), 0o644)
	}
	w("rollout-other.jsonl", meta("/elsewhere", start.Add(time.Second)))
	w("rollout-mine.jsonl", meta(dir, start.Add(time.Second))+tc)
	cl := findCodexLog(home, dir, start)
	if cl.Problem != "" || !strings.HasSuffix(cl.Path, "rollout-mine.jsonl") {
		t.Fatalf("应找到 cwd 匹配的唯一 rollout: %+v", cl)
	}
	if ok, _ := codexTaskComplete(cl.Content); !ok {
		t.Error("应判 task_complete")
	}
	if findCodexLog(home, "/nowhere", start).Problem == "" {
		t.Error("cwd 不匹配不应绑定到记录")
	}
	w("rollout-second.jsonl", meta(dir, start.Add(5*time.Second))+tc)
	if findCodexLog(home, dir, start).Problem == "" {
		t.Error("两个候选应有 Problem")
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
	t.Cleanup(func() { // 只杀隔离服务器，并等它真的退出，免得下一个测试撞上正在关闭的服务器
		_ = run("kill-server")
		for i := 0; i < 50 && run("list-sessions") == nil; i++ {
			time.Sleep(20 * time.Millisecond)
		}
		time.Sleep(50 * time.Millisecond)
	})
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
	if v != VerdictNoAgent || ps[0].State != Idle {
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
		in := PaneInput{Command: c.cmd, Procs: procs(c.procs...), ProcsKnown: c.known, Screen: "> "}
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
	got, reason := JudgePane(PaneInput{Command: "node", Procs: procs("node @openai/codex/bin/codex.js"), ProcsKnown: true, Screen: "esc to interrupt"})
	if got != Busy || !strings.Contains(reason, "命令行含 codex") {
		t.Errorf("node+codex busy: %s %s", got, reason)
	}
}

// 真实 codex rollout 的结构（脱敏，只留 type / payload.type / cwd；首行 session_meta，
// task_complete 之后还会有 thread_settings_applied 等事件）。
const realCodexShape = `{"timestamp":"2026-10-15T20:28:21.228Z","type":"session_meta","payload":{"cwd":"/work/ws","originator":"codex-tui"}}
{"timestamp":"2026-10-15T20:28:21.300Z","type":"event_msg","payload":{"type":"task_started"}}
{"timestamp":"2026-10-15T20:28:22.000Z","type":"response_item","payload":{"type":"message"}}
{"timestamp":"2026-10-15T20:28:23.000Z","type":"event_msg","payload":{"type":"token_count"}}
{"timestamp":"2026-10-15T20:28:24.000Z","type":"event_msg","payload":{"type":"task_complete"}}
{"timestamp":"2026-10-15T20:28:25.000Z","type":"event_msg","payload":{"type":"thread_settings_applied"}}
`

func TestCodexRealShape(t *testing.T) {
	if ok, ts := codexTaskComplete([]byte(realCodexShape)); !ok || ts.IsZero() {
		t.Error("真实结构应判 task_complete 且有时间戳")
	}
	home := t.TempDir()
	dir := t.TempDir()
	start := time.Now().Add(-time.Minute)
	sd := filepath.Join(home, ".codex", "sessions", "2026", "10", "15")
	_ = os.MkdirAll(sd, 0o755)
	at := start.Add(time.Second).UTC().Format(time.RFC3339Nano)
	first := fmt.Sprintf(`{"timestamp":%q,"type":"session_meta","payload":{"cwd":%q,"timestamp":%q,"base_instructions":{"text":%q}}}`+"\n", at, dir, at, strings.Repeat("x", 200<<10))
	_ = os.WriteFile(filepath.Join(sd, "rollout-a.jsonl"), []byte(first+realCodexShape[strings.Index(realCodexShape, "\n")+1:]), 0o644)
	l := findCodexLog(home, dir, start)
	if ok, _ := codexTaskComplete(l.Content); l.Problem != "" || !ok {
		t.Errorf("长首行的 rollout 应能按 cwd 找到并判 Done: %+v", l.Problem)
	}
}

// 起一个名叫 claude 的假 agent（拷贝 sleep 改名，不产生子进程），用于集成测试。
func fakeClaude(t *testing.T, run func(args ...string) error, ws, session string) {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "claude")
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("没有 sleep")
	}
	data, err := os.ReadFile(sleep)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, data, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := run("new-session", "-d", "-s", session, "-c", ws, bin+" 300"); err != nil {
		t.Fatal(err)
	}
}

func TestJudgeFakeClaudeFinished(t *testing.T) {
	tm, run := isolated(t)
	ws := canon(t.TempDir())
	home := t.TempDir()
	opt := Options{Tmux: tm, Home: home, Recheck: noRecheck()}
	fakeClaude(t, run, ws, "c1")

	// 假 agent 启动后才写入记录
	waitCmd(t, opt, ws, "claude")
	time.Sleep(1100 * time.Millisecond) // 记录修改时间须严格晚于进程启动（ps 启动时间只到秒）
	claudeFile(home, ws, "s.jsonl", time.Now(), ws)
	v, ps := waitCmd(t, opt, ws, "claude")
	if v != VerdictFinished || ps[0].State != Done {
		t.Fatalf("应 Finished: %s %+v", v, ps)
	}
	// 同目录再来一个启动后的记录文件 → 无法确定 → Unknown
	claudeFile(home, ws, "t.jsonl", time.Now(), ws)
	v, ps = waitCmd(t, opt, ws, "claude")
	if v != VerdictUnknown || !strings.Contains(ps[0].Reason, "2 个") {
		t.Fatalf("两个记录应 Unknown: %s %+v", v, ps)
	}
}

func noRecheck() *time.Duration { d := time.Duration(0); return &d }

// 子孙进程分类（纯函数层）：agent 启动 + 30 秒内的启动链/常驻辅助与白名单进程不影响；
// 之后启动且仍存活的（如本轮中途起的构建）→ Unknown。
func TestLateProcess(t *testing.T) {
	agentAt := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	done := agentAt.Add(10 * time.Minute)
	log := fmt.Sprintf(`{"type":"user","timestamp":"2026-10-16T12:00:00Z"}`+"\n"+
		`{"type":"assistant","timestamp":%q,"message":{"stop_reason":"end_turn"}}`+"\n", done.Format(time.RFC3339Nano))
	mk := func(extra ...Proc) PaneInput {
		ps := []Proc{{PID: 1, Args: "bash", Start: agentAt.Add(-time.Hour), StartKnown: true, Root: true},
			{PID: 2, Args: "claude", Start: agentAt, StartKnown: true}}
		return PaneInput{Command: "claude", Screen: "> ", Procs: append(ps, extra...), ProcsKnown: true,
			Log: LogInput{Found: true, Content: []byte(log)}}
	}
	resident := Proc{PID: 3, Args: "mcp-server --stdio", Start: agentAt.Add(10 * time.Second), StartKnown: true}
	edge := Proc{PID: 6, Args: "helper-at-edge", Start: agentAt.Add(30 * time.Second), StartKnown: true}
	midTurn := Proc{PID: 4, Args: "npm run build", Start: agentAt.Add(60 * time.Second), StartKnown: true}
	whitelisted := Proc{PID: 7, Args: "/usr/bin/xxyi-msg mcp", Start: agentAt.Add(5 * time.Minute), StartKnown: true}
	noStart := Proc{PID: 5, Args: "mystery"}

	if st, r := JudgePane(mk()); st != Done {
		t.Errorf("无额外进程应 Done: %s %s", st, r)
	}
	if st, r := JudgePane(mk(resident, edge)); st != Done {
		t.Errorf("agent 启动后 30 秒内的常驻子进程不应影响: %s %s", st, r)
	}
	if st, r := JudgePane(mk(whitelisted)); st != Done {
		t.Errorf("白名单进程不应影响: %s %s", st, r)
	}
	if st, r := JudgePane(mk(resident, midTurn)); st != Unknown || !strings.Contains(r, "仍有任务进程在运行：npm run build") {
		t.Errorf("中途启动且仍存活的子孙进程应 Unknown: %s %s", st, r)
	}
	if st, _ := JudgePane(mk(noStart)); st != Unknown {
		t.Error("子孙进程启动时间未知应 Unknown")
	}
	// 读不到 agent 自己的启动时间
	in := mk(resident)
	in.Procs[1].StartKnown = false
	if st, _ := JudgePane(in); st != Unknown {
		t.Error("agent 启动时间未知应 Unknown")
	}
}

// 记录修改时间与进程启动的先后：旧记录不绑定、同一秒无法判断、严格晚于才绑定。
func TestLogBindingStrict(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "proj")
	_ = os.MkdirAll(dir, 0o755)
	start := time.Now().Truncate(time.Second).Add(-time.Minute) // ps 启动时间只到秒
	set := func(home, name string, mod time.Time) {
		p := claudeFile(home, dir, name, mod, dir)
		_ = os.Chtimes(p, mod, mod)
	}
	// 旧记录刚结束（早于启动 3 秒）：不绑定；新 agent 没有自己的记录 → Unknown
	home := t.TempDir()
	set(home, "old.jsonl", start.Add(-3*time.Second))
	l := findClaudeLog(home, dir, start)
	if l.Path != "" || l.Problem == "" {
		t.Errorf("旧记录不应绑定: %+v", l)
	}
	if st, _ := JudgePane(PaneInput{Command: "claude", Screen: "> ", Log: l}); st != Unknown {
		t.Error("新 agent 无自身记录应 Unknown")
	}
	// 同一秒（含 .9 秒）→ Unknown
	home = t.TempDir()
	set(home, "same.jsonl", start.Add(900*time.Millisecond))
	l = findClaudeLog(home, dir, start)
	if l.Path != "" || !strings.Contains(l.Problem, "同一秒") {
		t.Errorf("同一秒应无法判断: %+v", l)
	}
	if st, _ := JudgePane(PaneInput{Command: "claude", Screen: "> ", Log: l}); st != Unknown {
		t.Error("同一秒应 Unknown")
	}
	// 严格晚于（下一秒）→ 绑定
	home = t.TempDir()
	set(home, "new.jsonl", start.Add(time.Second))
	if l = findClaudeLog(home, dir, start); l.Problem != "" || l.Path == "" {
		t.Errorf("晚一秒应绑定: %+v", l)
	}
	// codex 同理
	chome := t.TempDir()
	sd := filepath.Join(chome, ".codex", "sessions", "2026", "10", "16")
	_ = os.MkdirAll(sd, 0o755)
	cp := filepath.Join(sd, "rollout-a.jsonl")
	_ = os.WriteFile(cp, []byte(fmt.Sprintf(`{"type":"session_meta","payload":{"cwd":%q}}`+"\n", dir)), 0o644)
	_ = os.Chtimes(cp, start.Add(500*time.Millisecond), start.Add(500*time.Millisecond))
	if l = findCodexLog(chome, dir, start); !strings.Contains(l.Problem, "同一秒") {
		t.Errorf("codex 同一秒应无法判断: %+v", l)
	}
	_ = os.Chtimes(cp, start.Add(-2*time.Second), start.Add(-2*time.Second))
	if l = findCodexLog(chome, dir, start); l.Path != "" {
		t.Errorf("codex 旧记录不应绑定: %+v", l)
	}
}

// fakeSetup 起假 claude 窗格并写好属于它的记录；返回窗格 pid 与记录路径。
func fakeSetup(t *testing.T) (opt Options, ws string, panePID int, logPath string) {
	t.Helper()
	tm, run := isolated(t)
	ws = canon(t.TempDir())
	home := t.TempDir()
	opt = Options{Tmux: tm, Home: home, Recheck: noRecheck()}
	fakeClaude(t, run, ws, "c1")
	_, ps := waitCmd(t, opt, ws, "claude")
	panePID = ps[0].PID
	time.Sleep(1100 * time.Millisecond) // 记录修改时间须严格晚于进程启动（ps 启动时间只到秒）
	logPath = claudeFile(home, ws, "s.jsonl", time.Now(), ws)
	return
}

// 在真实进程表上给窗格挂一个假的子进程。
func withChild(panePID int, args string, start time.Time) func() (*ProcTable, error) {
	return func() (*ProcTable, error) {
		tab, err := psTree()
		if err != nil {
			return nil, err
		}
		const fake = 99999999
		tab.Children[panePID] = append(tab.Children[panePID], fake)
		tab.Args[fake] = args
		tab.Start[fake] = start
		return tab, nil
	}
}

func TestJudgeLateChildIntegration(t *testing.T) {
	opt, ws, pid, _ := fakeSetup(t)
	if v, ps, _ := Judge(context.Background(), ws, opt); v != VerdictFinished {
		t.Fatalf("基线应 Finished: %s %+v", v, ps)
	}
	opt.ProcTree = withChild(pid, "make test", time.Now().Add(time.Minute))
	v, ps, _ := Judge(context.Background(), ws, opt)
	if v != VerdictUnknown || !strings.Contains(ps[0].Reason, "仍有任务进程在运行：make test") {
		t.Errorf("本轮后启动的子进程应 Unknown: %s %+v", v, ps)
	}
	opt.ProcTree = withChild(pid, "mcp-server", time.Now().Add(-time.Hour))
	if v, ps, _ := Judge(context.Background(), ws, opt); v != VerdictFinished {
		t.Errorf("常驻子进程不应影响: %s %+v", v, ps)
	}
}

func TestJudgeOtherWriters(t *testing.T) {
	opt, ws, pid, _ := fakeSetup(t)
	tree := withChild(pid, "vim notes.txt", time.Now().Add(-time.Hour))
	opt.ProcTree = func() (*ProcTable, error) {
		tab, err := tree()
		if err == nil {
			tab.Args[88888888] = "vim main.go" // 不属于任何窗格的进程
			tab.Start[88888888] = time.Now().Add(-time.Hour)
		}
		return tab, err
	}
	// 窗格自己的进程（含子孙）在工作区里：不算
	opt.ProcCwd = func() (map[int]string, error) { return map[int]string{pid: ws, 99999999: ws + "/sub"}, nil }
	if v, ps, _ := Judge(context.Background(), ws, opt); v != VerdictFinished {
		t.Errorf("窗格自己的进程不应算别处写入者: %s %+v", v, ps)
	}
	// 工作区外的进程：不算
	opt.ProcCwd = func() (map[int]string, error) { return map[int]string{88888888: "/tmp"}, nil }
	if v, _, _ := Judge(context.Background(), ws, opt); v != VerdictFinished {
		t.Errorf("区外进程不应影响: %s", v)
	}
	// 工作区里不属于窗格的进程 → Unknown，Notes 列出命令
	opt.ProcCwd = func() (map[int]string, error) { return map[int]string{88888888: ws + "/pkg"}, nil }
	r, err := JudgeDetail(context.Background(), ws, opt)
	if err != nil || r.Verdict != VerdictUnknown || len(r.Notes) != 1 || !strings.Contains(r.Notes[0], "vim main.go") {
		t.Errorf("别处写入者应 Unknown: %+v %v", r, err)
	}
	// 读不到进程目录 → Unknown
	opt.ProcCwd = func() (map[int]string, error) { return nil, fmt.Errorf("lsof 不可用") }
	if v, _, _ := Judge(context.Background(), ws, opt); v != VerdictUnknown {
		t.Errorf("读不到进程目录应 Unknown: %s", v)
	}
}

func TestJudgeDoubleSample(t *testing.T) {
	opt, ws, _, logPath := fakeSetup(t)
	d := time.Millisecond
	opt.Recheck = &d
	opt.ProcCwd = func() (map[int]string, error) { return map[int]string{}, nil }

	// 两次之间没变化 → Finished
	opt.sleep = func(time.Duration) {}
	if r, _ := JudgeDetail(context.Background(), ws, opt); r.Verdict != VerdictFinished {
		t.Fatalf("没变化应 Finished: %+v", r)
	}
	// 两次之间记录被追加 → Unknown
	opt.sleep = func(time.Duration) {
		f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Error(err)
			return
		}
		defer f.Close()
		_, _ = f.WriteString(`{"type":"mode"}` + "\n")
	}
	r, _ := JudgeDetail(context.Background(), ws, opt)
	if r.Verdict != VerdictUnknown || len(r.Notes) == 0 || !strings.Contains(r.Notes[0], "有变化") {
		t.Errorf("记录变化应 Unknown: %+v", r)
	}
}

func TestHelperWhitelist(t *testing.T) {
	yes := []string{
		"/home/u/.codex/packages/x/bin/codex app-server --listen unix://",
		"/home/u/.codex/packages/x/bin/codex app-server daemon pid-update-loop",
		"/home/u/.codex/packages/x/bin/codex-code-mode-host",
		"/home/u/.local/bin/xxyi-msg mcp",
		"xxyi-msg state --event stop",
	}
	no := []string{
		"codex", "codex exec fix bug", "node /usr/bin/codex", "xxyi-msg send hi", "xxyi-msg",
		"vim app-server", "/usr/bin/python3 mcp", "cp codex-code-mode-host-backup x",
	}
	for _, c := range yes {
		if !isHelperProcess(c) {
			t.Errorf("应在白名单: %s", c)
		}
	}
	for _, c := range no {
		if isHelperProcess(c) {
			t.Errorf("不应在白名单: %s", c)
		}
	}
}

func TestJudgeOtherWritersWhitelist(t *testing.T) {
	opt, ws, pid, _ := fakeSetup(t)
	opt.ProcTree = func() (*ProcTable, error) {
		tab, err := withChild(pid, "mcp-server", time.Now().Add(-time.Hour))()
		if err == nil {
			tab.Args[88888881] = "/h/.codex/bin/codex app-server --listen unix://"
			tab.Args[88888882] = "/h/.local/bin/xxyi-msg mcp"
			tab.Args[88888883] = "vim main.go"
		}
		return tab, err
	}
	opt.ProcCwd = func() (map[int]string, error) {
		return map[int]string{88888881: ws, 88888882: ws + "/sub"}, nil
	}
	r, _ := JudgeDetail(context.Background(), ws, opt)
	if r.Verdict != VerdictFinished || len(r.Notes) != 1 || !strings.Contains(r.Notes[0], "已忽略") ||
		!strings.Contains(r.Notes[0], "app-server") || !strings.Contains(r.Notes[0], "xxyi-msg mcp") {
		t.Errorf("白名单进程不应影响，且应列出: %+v", r)
	}
	opt.ProcCwd = func() (map[int]string, error) {
		return map[int]string{88888881: ws, 88888883: ws}, nil
	}
	r, _ = JudgeDetail(context.Background(), ws, opt)
	if r.Verdict != VerdictUnknown || !strings.Contains(strings.Join(r.Notes, "|"), "vim main.go") {
		t.Errorf("白名单外进程应 Unknown: %+v", r)
	}
}
