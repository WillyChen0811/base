// Package agentstate 判断在某个工作区里干活的 agent 是否已做完。
// 只读、零 token：只看 tmux 窗格、屏幕文字、进程树和本机的会话记录文件，不调用任何模型。
// 规则见 go/PLAN-sync-v2.md 第十四、十五节与 go/PLAN-observer.md「状态判定」。
// 总原则：任何读取失败都算 Unknown，绝不当成 Done。
package agentstate

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/WillyChen0811/base/go/projsync"
)

// PaneState 是单个窗格的状态。
type PaneState string

const (
	Idle    PaneState = "idle"    // 没有 agent 在跑（shell 空闲或窗格已死）
	Busy    PaneState = "busy"    // 正在工作
	Done    PaneState = "done"    // agent 本轮已结束
	Unknown PaneState = "unknown" // 无法确定
)

// Verdict 是整个工作区的结论。Go 里同包不能重名，故带 Verdict 前缀。
type Verdict string

const (
	VerdictFinished Verdict = "finished" // 全部 Idle/Done：只有它允许自动提交
	VerdictBusy     Verdict = "busy"
	VerdictUnknown  Verdict = "unknown"
	VerdictNoAgent  Verdict = "no_agent" // 没有任何窗格在该工作区：视为不确定
)

// Pane 是工作区内的一个窗格及其判定。
type Pane struct {
	ID      string
	Path    string
	Command string
	Dead    bool
	PID     int
	State   PaneState
	Reason  string // 中文，写明依据
	sig     string // 所用会话记录的大小+修改时间，双采样比对用
}

// Options 是 Judge 的可注入项。
type Options struct {
	Tmux     string                     // tmux 命令，默认 "tmux"；测试用 "tmux -L 名字"
	Home     string                     // 用于找 ~/.claude 与 ~/.codex，默认当前用户主目录
	ProcTree func() (*ProcTable, error) // 读进程树与命令行；默认读 ps
}

// ProcTable 是进程表：父→子 pid 列表、每个进程的完整命令行（空格连接）与启动时间。
type ProcTable struct {
	Children map[int][]int
	Args     map[int]string
	Start    map[int]time.Time // 读不到启动时间的进程没有条目
}

// Proc 是窗格进程树里的一个进程。
type Proc struct {
	PID        int
	Args       string
	Start      time.Time
	StartKnown bool
	Root       bool // 窗格进程本身
}

// ── 纯判定 ────────────────────────────────────────────

// PaneInput 是纯函数 JudgePane 的输入。
type PaneInput struct {
	Command string
	Dead    bool

	// 进程树：ChildrenKnown=false 表示读不到，此时不能断言"无子进程"。
	ChildrenKnown bool
	HasChildren   bool

	// 窗格进程（Root）及其子孙；ProcsKnown=false 表示读不到进程表。
	Procs      []Proc
	ProcsKnown bool

	Screen    string
	ScreenErr bool // capture-pane 失败

	// 会话记录：Found=false 表示没找到；ReadErr 表示找到但读不了。
	Log LogInput
}

// LogInput 是会话记录文件的内容。
type LogInput struct {
	Found   bool
	ReadErr bool
	Content []byte
	// Problem 非空表示无法把记录文件可靠地绑定到这个 agent 进程（多个候选、cwd 不符、
	// 读不到进程启动时间等），内容就是原因；此时一律 Unknown。
	Problem string
	Path    string
	Sig     string // 大小+修改时间，用于前后两次采样比对
}

const escMark = "esc to interrupt"

func isShell(cmd string) bool {
	switch strings.TrimPrefix(cmd, "-") {
	case "bash", "zsh", "sh", "fish":
		return true
	}
	return false
}

// JudgePane 按规则给单个窗格定状态（纯函数）。
func JudgePane(in PaneInput) (PaneState, string) {
	if in.Dead {
		return Idle, "窗格进程已结束"
	}
	cmd := strings.TrimPrefix(in.Command, "-")
	if isShell(cmd) {
		if !in.ChildrenKnown {
			return Unknown, "前台是 shell，但读不到进程树"
		}
		if in.HasChildren {
			return Unknown, "前台是 shell，但窗格进程下有子进程在跑"
		}
		return Idle, "前台是 shell 且没有子进程"
	}
	why := ""
	if cmd != "claude" && cmd != "codex" {
		// 前台命令名认不出（如 node、版本号）：看窗格进程及子孙的命令行。
		if !in.ProcsKnown {
			return Unknown, "前台程序 " + in.Command + " 认不出，且读不到进程命令行"
		}
		var amb string
		cmd, why, amb = identifyAgent(in.Procs)
		if cmd == "" {
			if amb != "" {
				return Unknown, amb
			}
			return Unknown, "前台程序 " + in.Command + " 不是已知的 agent"
		}
		why += "；"
	}
	if in.ScreenErr {
		return Unknown, why + "读不到窗格屏幕"
	}
	if strings.Contains(in.Screen, escMark) {
		return Busy, why + "屏幕有 esc to interrupt"
	}
	if in.Log.Problem != "" {
		return Unknown, why + in.Log.Problem
	}
	if !in.Log.Found {
		return Unknown, why + "找不到会话记录"
	}
	if in.Log.ReadErr {
		return Unknown, why + "会话记录读不了"
	}
	if !lastLineComplete(in.Log.Content) {
		return Unknown, why + "会话记录最后一行不完整（可能正在写入）"
	}
	if cmd == "claude" {
		if ok, _ := claudeEndTurn(in.Log.Content); ok {
			return Done, why + "屏幕无 esc to interrupt，会话记录最后一条 end_turn"
		}
		return Unknown, why + "屏幕无 esc to interrupt，但会话记录最后不是 end_turn"
	}
	if ok, _ := codexTaskComplete(in.Log.Content); ok {
		return Done, why + "屏幕无 esc to interrupt，会话记录最后一个 task_started 之后有 task_complete"
	}
	return Unknown, why + "屏幕无 esc to interrupt，但会话记录里最后一个任务未见 task_complete"
}

// procKind 判断一条命令行属于哪种 agent：返回 "claude"、"codex" 或 ""（都不是），
// both=true 表示同一条命令行里两者都出现。
// 只看前两个参数（可执行文件与解释器的脚本）以及包名，免得 `vim claude` 之类误判。
func procKind(cmdline string) (kind string, both bool) {
	args := strings.Fields(cmdline)
	is := func(name, pkg string) bool {
		if strings.Contains(cmdline, pkg) {
			return true
		}
		for i := 0; i < len(args) && i < 2; i++ {
			b := filepath.Base(args[i])
			if b == name || strings.TrimSuffix(b, ".js") == name {
				return true
			}
		}
		return false
	}
	c, x := is("claude", "@anthropic-ai/claude-code"), is("codex", "@openai/codex")
	switch {
	case c && x:
		return "", true
	case c:
		return "claude", false
	case x:
		return "codex", false
	}
	return "", false
}

// identifyAgent 从窗格进程及其子孙的命令行认 agent。两种都出现或认不出 → 空（Unknown）。
func identifyAgent(procs []Proc) (kind, why, ambiguous string) {
	found := map[string]bool{}
	for _, p := range procs {
		k, both := procKind(p.Args)
		if both {
			return "", "", "命令行里同时出现 claude 与 codex，无法确定"
		}
		if k != "" {
			found[k] = true
		}
	}
	switch {
	case found["claude"] && found["codex"]:
		return "", "", "进程命令行里同时出现 claude 与 codex，无法确定"
	case found["claude"]:
		return "claude", "命令行含 claude", ""
	case found["codex"]:
		return "codex", "命令行含 codex", ""
	}
	return "", "", ""
}

// lastLineComplete：记录必须以换行结尾，且最后一个非空行是完整 JSON；否则可能正在写入。
func lastLineComplete(content []byte) bool {
	if len(content) == 0 || content[len(content)-1] != '\n' {
		return false
	}
	lines := bytes.Split(bytes.TrimRight(content, "\r\n \t"), []byte("\n"))
	return json.Valid(bytes.TrimSpace(lines[len(lines)-1]))
}

func parseTS(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// claudeEndTurn：最后一条 type=assistant 的 stop_reason 为 end_turn，且其后没有 user/tool_result 记录。
// 同时返回那条 end_turn 记录的时间戳（没有则为零值）。
func claudeEndTurn(content []byte) (bool, time.Time) {
	seen, endTurn, afterUser := false, false, false
	var ts time.Time
	for _, line := range bytes.Split(content, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var rec struct {
			Type      string `json:"type"`
			Timestamp string `json:"timestamp"`
			Message   struct {
				StopReason *string `json:"stop_reason"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &rec) != nil {
			continue // 截断的首行或坏行（末行是否完整由 lastLineComplete 另查）
		}
		switch rec.Type {
		case "assistant":
			seen = true
			endTurn = rec.Message.StopReason != nil && *rec.Message.StopReason == "end_turn"
			ts = parseTS(rec.Timestamp)
			afterUser = false
		case "user":
			if seen {
				afterUser = true
			}
		}
	}
	return seen && endTurn && !afterUser, ts
}

// codexTaskComplete：最后一个 task_started 之后有 task_complete；同时返回 task_complete 的时间戳。
func codexTaskComplete(content []byte) (bool, time.Time) {
	started, complete := false, false
	var ts time.Time
	for _, line := range bytes.Split(content, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var rec struct {
			Timestamp string `json:"timestamp"`
			Payload   struct {
				Type string `json:"type"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		switch rec.Payload.Type {
		case "task_started":
			started, complete = true, false
		case "task_complete":
			if started {
				complete = true
				ts = parseTS(rec.Timestamp)
			}
		}
	}
	return started && complete, ts
}

// Summarize 由各窗格状态得出工作区结论（纯函数）。
// 任一 Busy → Busy；任一 Unknown → Unknown；至少一个 Done 且其余都是 Idle/Done → Finished；
// 没有窗格，或只有空闲 shell / 死窗格（没有任何 Done 的 agent）→ NoAgent。
func Summarize(panes []Pane) Verdict {
	unknown, done := false, false
	for _, p := range panes {
		switch p.State {
		case Busy:
			return VerdictBusy
		case Idle:
		case Done:
			done = true
		default:
			unknown = true
		}
	}
	switch {
	case unknown:
		return VerdictUnknown
	case done:
		return VerdictFinished
	}
	return VerdictNoAgent
}

// ── 路径匹配 ──────────────────────────────────────────

func canon(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return filepath.Clean(p)
}

// InWorktree 判断 dir 是否等于 root 或在其子目录下（先解析符号链接，按路径分段比较）。
func InWorktree(root, dir string) bool {
	if root == "" || dir == "" {
		return false
	}
	rel, err := filepath.Rel(canon(root), canon(dir))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// ── 采集 ──────────────────────────────────────────────

// Result 是 JudgeDetail 的完整结果。
type Result struct {
	Verdict Verdict
	Panes   []Pane
	Notes   []string // 工作区级别的依据（别处写入者、双采样不一致等），中文
}

// Judge 判断 worktree 里的 agent 是否做完。
func Judge(ctx context.Context, worktree string, opt Options) (Verdict, []Pane, error) {
	r, err := JudgeDetail(ctx, worktree, opt)
	return r.Verdict, r.Panes, err
}

// JudgeDetail 同 Judge，但额外返回工作区级别的依据。
func JudgeDetail(ctx context.Context, worktree string, opt Options) (Result, error) {
	return judgeOnce(ctx, worktree, opt)
}

func judgeOnce(ctx context.Context, worktree string, opt Options) (Result, error) {
	tmuxCmd := strings.Fields(opt.Tmux)
	if len(tmuxCmd) == 0 {
		tmuxCmd = []string{"tmux"}
	}
	home := opt.Home
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	procTree := opt.ProcTree
	if procTree == nil {
		procTree = psTree
	}

	out, stderr, err := runTmux(ctx, tmuxCmd, "list-panes", "-a", "-F",
		"#{pane_id}\t#{pane_pid}\t#{pane_dead}\t#{pane_current_command}\t#{pane_current_path}")
	if err != nil {
		if noServer(stderr) {
			return Result{Verdict: VerdictNoAgent}, nil // tmux 在，但没有服务器 = 没有任何窗格
		}
		return Result{Verdict: VerdictUnknown}, fmt.Errorf("tmux list-panes 失败: %v: %s", err, strings.TrimSpace(stderr))
	}

	var panes []Pane
	var tree *ProcTable
	var treeErr error
	treeLoaded := false
	for _, line := range strings.Split(out, "\n") {
		f := strings.SplitN(line, "\t", 5)
		if len(f) != 5 || !InWorktree(worktree, f[4]) {
			continue
		}
		pid, _ := strconv.Atoi(f[1])
		p := Pane{ID: f[0], PID: pid, Dead: f[2] == "1", Command: f[3], Path: f[4]}
		in := PaneInput{Command: p.Command, Dead: p.Dead}
		cmd := strings.TrimPrefix(p.Command, "-")
		if !p.Dead {
			if !treeLoaded {
				tree, treeErr = procTree()
				treeLoaded = true
			}
			if treeErr == nil && tree != nil {
				if isShell(cmd) {
					in.ChildrenKnown = true
					in.HasChildren = len(tree.Children[pid]) > 0
				} else {
					in.ProcsKnown = true
					in.Procs = descendantProcs(tree, pid)
				}
			}
			if in.ProcsKnown && cmd != "claude" && cmd != "codex" {
				cmd, _, _ = identifyAgent(in.Procs)
			}
		}
		if !p.Dead && (cmd == "claude" || cmd == "codex") {
			scr, _, cerr := runTmux(ctx, tmuxCmd, "capture-pane", "-p", "-t", p.ID)
			in.Screen, in.ScreenErr = scr, cerr != nil
			start, ok := agentStart(in.Procs, cmd)
			switch {
			case !ok:
				in.Log = LogInput{Found: true, Problem: "读不到 agent 进程的启动时间，无法绑定会话记录"}
			case cmd == "claude":
				in.Log = findClaudeLog(home, p.Path, start)
			default:
				in.Log = findCodexLog(home, p.Path, start)
			}
		}
		p.State, p.Reason = JudgePane(in)
		p.sig = in.Log.Sig
		panes = append(panes, p)
	}
	return Result{Verdict: Summarize(panes), Panes: panes}, nil
}

func noServer(stderr string) bool {
	return strings.Contains(stderr, "no server running") ||
		(strings.Contains(stderr, "error connecting to") && strings.Contains(stderr, "No such file or directory"))
}

// runTmux 执行 tmux，并从环境里删掉 TMUX / TMUX_PANE，只连 tmuxCmd 指定的服务器。
func runTmux(ctx context.Context, base []string, args ...string) (string, string, error) {
	full := append(append([]string{}, base[1:]...), args...)
	cmd := exec.CommandContext(ctx, base[0], full...)
	cmd.Env = cleanEnv()
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	return so.String(), se.String(), err
}

func cleanEnv() []string {
	var env []string
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "TMUX=") || strings.HasPrefix(e, "TMUX_PANE=") {
			continue
		}
		env = append(env, e)
	}
	return env
}

// psTree 用 `ps -A -o pid=,ppid=,lstart=,args=` 建进程表（Linux 与 macOS 通用）。
func psTree() (*ProcTable, error) {
	cmd := exec.Command("ps", "-A", "-o", "pid=,ppid=,lstart=,args=")
	cmd.Env = append(cleanEnv(), "LC_ALL=C", "LANG=C")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	tab := &ProcTable{Children: map[int][]int{}, Args: map[int]string{}, Start: map[int]time.Time{}}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 7 {
			continue
		}
		pid, e1 := strconv.Atoi(f[0])
		ppid, e2 := strconv.Atoi(f[1])
		if e1 != nil || e2 != nil {
			continue
		}
		tab.Children[ppid] = append(tab.Children[ppid], pid)
		tab.Args[pid] = strings.Join(f[7:], " ")
		if t, err := time.ParseInLocation("Mon Jan _2 15:04:05 2006", strings.Join(f[2:7], " "), time.Local); err == nil {
			tab.Start[pid] = t
		}
	}
	if len(tab.Args) == 0 {
		return nil, errors.New("ps 没有输出")
	}
	return tab, nil
}

// descendantProcs 返回 pid 自己（Root）及其所有子孙。
func descendantProcs(t *ProcTable, pid int) []Proc {
	var res []Proc
	seen := map[int]bool{}
	stack := []int{pid}
	for len(stack) > 0 {
		p := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[p] {
			continue
		}
		seen[p] = true
		if a, ok := t.Args[p]; ok {
			st, known := t.Start[p]
			res = append(res, Proc{PID: p, Args: a, Start: st, StartKnown: known, Root: p == pid})
		}
		stack = append(stack, t.Children[p]...)
	}
	return res
}

// agentStart 返回命令行属于 kind 的进程里最早的启动时间。
func agentStart(procs []Proc, kind string) (time.Time, bool) {
	var best time.Time
	ok := false
	for _, p := range procs {
		if k, _ := procKind(p.Args); k != kind || !p.StartKnown {
			continue
		}
		if !ok || p.Start.Before(best) {
			best, ok = p.Start, true
		}
	}
	return best, ok
}

const (
	tailBytes = 8 << 20
	headBytes = 4 << 20
	// 记录文件的首条时间戳允许比进程启动时间早这么多（时钟取整与启动先后的误差）。
	startSlack = 5 * time.Second
)

// readTail 读文件末尾最多 tailBytes 字节，并返回读取前的大小与修改时间组成的签名。
func readTail(path string) ([]byte, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, "", err
	}
	off := int64(0)
	if st.Size() > tailBytes {
		off = st.Size() - tailBytes
	}
	buf := make([]byte, st.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil && len(buf) > 0 {
		return nil, "", err
	}
	return buf, fmt.Sprintf("%d|%d", st.Size(), st.ModTime().UnixNano()), nil
}

// readHeadLines 读文件开头最多 headBytes 字节的完整行。
func readHeadLines(path string) ([][]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, headBytes)
	n, err := f.Read(buf)
	if err != nil && n == 0 {
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		return nil, err
	}
	buf = buf[:n]
	if n == headBytes { // 丢掉被截断的最后一行
		if i := bytes.LastIndexByte(buf, '\n'); i >= 0 {
			buf = buf[:i]
		}
	}
	return bytes.Split(buf, []byte("\n")), nil
}

type fileMod struct {
	path string
	mod  time.Time
}

// findClaudeLog 找属于该 agent 进程的会话记录：目录里首条带时间戳的记录
// ≥ 进程启动时间 − 5 秒的 .jsonl，恰好一个才用；零个或多个 → Problem。
// 记录里的首个 cwd 必须等于窗格目录。
func findClaudeLog(home, paneDir string, start time.Time) LogInput {
	dir := filepath.Join(home, ".claude", "projects", projsync.ClaudeDirName(paneDir))
	ents, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return LogInput{}
		}
		return LogInput{Found: true, ReadErr: true}
	}
	type cand struct {
		path string
		cwd  string
	}
	var cs []cand
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().Before(start.Add(-startSlack)) {
			continue // 最后修改早于进程启动，不可能是它的
		}
		path := filepath.Join(dir, e.Name())
		lines, err := readHeadLines(path)
		if err != nil {
			return LogInput{Found: true, ReadErr: true}
		}
		var first time.Time
		cwd := ""
		for _, ln := range lines {
			var rec struct {
				Timestamp string `json:"timestamp"`
				Cwd       string `json:"cwd"`
			}
			if json.Unmarshal(ln, &rec) != nil {
				continue
			}
			if first.IsZero() {
				first = parseTS(rec.Timestamp)
			}
			if cwd == "" {
				cwd = rec.Cwd
			}
			if !first.IsZero() && cwd != "" {
				break
			}
		}
		if first.IsZero() || first.Before(start.Add(-startSlack)) {
			continue
		}
		cs = append(cs, cand{path, cwd})
	}
	paths := make([]string, len(cs))
	for i, c := range cs {
		paths[i] = c.path
	}
	return pickLog(paths, func(i int) string { return cs[i].cwd }, paneDir)
}

// pickLog 在候选里挑唯一的一个并读出内容；cwd 为空表示记录里没有 cwd 字段，不核对。
func pickLog(paths []string, cwdOf func(int) string, paneDir string) LogInput {
	switch len(paths) {
	case 0:
		return LogInput{Found: true, Problem: "没有晚于 agent 启动的会话记录，无法绑定"}
	case 1:
	default:
		return LogInput{Found: true, Problem: fmt.Sprintf("有 %d 个会话记录晚于 agent 启动，无法确定哪个属于它", len(paths))}
	}
	if c := cwdOf(0); c != "" && canon(c) != canon(paneDir) {
		return LogInput{Found: true, Problem: "会话记录里的 cwd（" + c + "）与窗格目录不符"}
	}
	b, sig, err := readTail(paths[0])
	if err != nil {
		return LogInput{Found: true, ReadErr: true}
	}
	return LogInput{Found: true, Content: b, Path: paths[0], Sig: sig}
}

// findCodexLog 在 ~/.codex/sessions 下找 session_meta 的 cwd 等于窗格目录、
// 且开始时间 ≥ 进程启动时间 − 5 秒的 rollout，恰好一个才用。
func findCodexLog(home, paneDir string, start time.Time) LogInput {
	root := filepath.Join(home, ".codex", "sessions")
	var fs []fileMod
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		n := d.Name()
		if strings.HasPrefix(n, "rollout-") && strings.HasSuffix(n, ".jsonl") {
			if info, e := d.Info(); e == nil && !info.ModTime().Before(start.Add(-startSlack)) {
				fs = append(fs, fileMod{p, info.ModTime()})
			}
		}
		return nil
	})
	want := canon(paneDir)
	var paths []string
	for _, f := range fs {
		cwd, ts, ok := codexMeta(f.path)
		if !ok || canon(cwd) != want || ts.Before(start.Add(-startSlack)) {
			continue
		}
		paths = append(paths, f.path)
	}
	if len(paths) == 0 && len(fs) == 0 {
		return LogInput{}
	}
	return pickLog(paths, func(int) string { return "" }, paneDir)
}

// codexMeta 读首行 session_meta 的 payload.cwd 与开始时间（payload.timestamp，缺则取外层 timestamp）。
func codexMeta(path string) (string, time.Time, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", time.Time{}, false
	}
	defer f.Close()
	// 首行 session_meta 含很长的 base_instructions（真实样本约 22KB），按行完整读，上限 4MB。
	r := bufio.NewReaderSize(f, 64<<10)
	var line []byte
	for {
		chunk, isPrefix, err := r.ReadLine()
		if err != nil {
			return "", time.Time{}, false
		}
		line = append(line, chunk...)
		if !isPrefix {
			break
		}
		if len(line) > 4<<20 {
			return "", time.Time{}, false
		}
	}
	var rec struct {
		Type      string `json:"type"`
		Timestamp string `json:"timestamp"`
		Payload   struct {
			Cwd       string `json:"cwd"`
			Timestamp string `json:"timestamp"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &rec) != nil || rec.Type != "session_meta" || rec.Payload.Cwd == "" {
		return "", time.Time{}, false
	}
	ts := parseTS(rec.Payload.Timestamp)
	if ts.IsZero() {
		ts = parseTS(rec.Timestamp)
	}
	if ts.IsZero() {
		return "", time.Time{}, false
	}
	return rec.Payload.Cwd, ts, true
}
