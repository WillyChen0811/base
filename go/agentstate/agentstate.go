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
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

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
}

// Options 是 Judge 的可注入项。
type Options struct {
	Tmux     string                     // tmux 命令，默认 "tmux"；测试用 "tmux -L 名字"
	Home     string                     // 用于找 ~/.claude 与 ~/.codex，默认当前用户主目录
	ProcTree func() (*ProcTable, error) // 读进程树与命令行；默认读 ps
}

// ProcTable 是进程表：父→子 pid 列表，以及每个进程的完整命令行（空格连接）。
type ProcTable struct {
	Children map[int][]int
	Args     map[int]string
}

// ── 纯判定 ────────────────────────────────────────────

// PaneInput 是纯函数 JudgePane 的输入。
type PaneInput struct {
	Command string
	Dead    bool

	// 进程树：ChildrenKnown=false 表示读不到，此时不能断言"无子进程"。
	ChildrenKnown bool
	HasChildren   bool

	// 窗格进程及其子孙的完整命令行；ProcsKnown=false 表示读不到。
	Procs      []string
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
	if !in.Log.Found {
		return Unknown, why + "找不到会话记录"
	}
	if in.Log.ReadErr {
		return Unknown, why + "会话记录读不了"
	}
	if cmd == "claude" {
		if claudeEndTurn(in.Log.Content) {
			return Done, why + "屏幕无 esc to interrupt，会话记录最后一条 end_turn"
		}
		return Unknown, why + "屏幕无 esc to interrupt，但会话记录最后不是 end_turn"
	}
	if codexTaskComplete(in.Log.Content) {
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
func identifyAgent(procs []string) (kind, why, ambiguous string) {
	found := map[string]bool{}
	for _, p := range procs {
		k, both := procKind(p)
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

// claudeEndTurn：最后一条 type=assistant 的 stop_reason 为 end_turn，且其后没有 user/tool_result 记录。
func claudeEndTurn(content []byte) bool {
	lastAssistant := -1
	endTurn := false
	n := 0
	var afterUser bool
	for _, line := range bytes.Split(content, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var rec struct {
			Type    string `json:"type"`
			Message struct {
				StopReason *string `json:"stop_reason"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &rec) != nil {
			continue // 截断的首行或坏行
		}
		switch rec.Type {
		case "assistant":
			lastAssistant = n
			endTurn = rec.Message.StopReason != nil && *rec.Message.StopReason == "end_turn"
			afterUser = false
		case "user":
			if lastAssistant >= 0 {
				afterUser = true
			}
		}
		n++
	}
	return lastAssistant >= 0 && endTurn && !afterUser
}

// codexTaskComplete：最后一个 task_started 之后有 task_complete。
func codexTaskComplete(content []byte) bool {
	started, complete := false, false
	for _, line := range bytes.Split(content, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var rec struct {
			Payload struct {
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
			}
		}
	}
	return started && complete
}

// Summarize 由各窗格状态得出工作区结论（纯函数）。
func Summarize(panes []Pane) Verdict {
	if len(panes) == 0 {
		return VerdictNoAgent
	}
	unknown := false
	for _, p := range panes {
		switch p.State {
		case Busy:
			return VerdictBusy
		case Idle, Done:
		default:
			unknown = true
		}
	}
	if unknown {
		return VerdictUnknown
	}
	return VerdictFinished
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

// Judge 判断 worktree 里的 agent 是否做完。
func Judge(ctx context.Context, worktree string, opt Options) (Verdict, []Pane, error) {
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
			return VerdictNoAgent, nil, nil // tmux 在，但没有服务器 = 没有任何窗格
		}
		return VerdictUnknown, nil, fmt.Errorf("tmux list-panes 失败: %v: %s", err, strings.TrimSpace(stderr))
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
				} else if cmd != "claude" && cmd != "codex" {
					in.ProcsKnown = true
					in.Procs = descendantArgs(tree, pid)
				}
			}
			if in.ProcsKnown {
				cmd, _, _ = identifyAgent(in.Procs)
			}
		}
		if !p.Dead && (cmd == "claude" || cmd == "codex") {
			scr, _, cerr := runTmux(ctx, tmuxCmd, "capture-pane", "-p", "-t", p.ID)
			in.Screen, in.ScreenErr = scr, cerr != nil
			if cmd == "claude" {
				in.Log = findClaudeLog(home, p.Path)
			} else {
				in.Log = findCodexLog(home, p.Path)
			}
		}
		p.State, p.Reason = JudgePane(in)
		panes = append(panes, p)
	}
	return Summarize(panes), panes, nil
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

// psTree 用 `ps -A -o pid=,ppid=` 建 父→子 表（Linux 与 macOS 通用）。
func psTree() (*ProcTable, error) {
	cmd := exec.Command("ps", "-A", "-o", "pid=,ppid=,args=")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	tab := &ProcTable{Children: map[int][]int{}, Args: map[int]string{}}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		pid, e1 := strconv.Atoi(f[0])
		ppid, e2 := strconv.Atoi(f[1])
		if e1 != nil || e2 != nil {
			continue
		}
		tab.Children[ppid] = append(tab.Children[ppid], pid)
		tab.Args[pid] = strings.Join(f[2:], " ")
	}
	if len(tab.Args) == 0 {
		return nil, errors.New("ps 没有输出")
	}
	return tab, nil
}

// descendantArgs 返回 pid 自己及其所有子孙的命令行。
func descendantArgs(t *ProcTable, pid int) []string {
	var res []string
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
			res = append(res, a)
		}
		stack = append(stack, t.Children[p]...)
	}
	return res
}

const tailBytes = 8 << 20

// readTail 读文件末尾最多 tailBytes 字节。
func readTail(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	off := int64(0)
	if st.Size() > tailBytes {
		off = st.Size() - tailBytes
	}
	buf := make([]byte, st.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil && len(buf) > 0 {
		return nil, err
	}
	return buf, nil
}

type fileMod struct {
	path string
	mod  int64
}

func newestFirst(fs []fileMod) {
	sort.Slice(fs, func(i, j int) bool { return fs[i].mod > fs[j].mod })
}

func findClaudeLog(home, paneDir string) LogInput {
	dir := filepath.Join(home, ".claude", "projects", projsync.ClaudeDirName(paneDir))
	ents, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return LogInput{}
		}
		return LogInput{Found: true, ReadErr: true}
	}
	var fs []fileMod
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		fs = append(fs, fileMod{filepath.Join(dir, e.Name()), info.ModTime().UnixNano()})
	}
	if len(fs) == 0 {
		return LogInput{}
	}
	newestFirst(fs)
	b, err := readTail(fs[0].path)
	if err != nil {
		return LogInput{Found: true, ReadErr: true}
	}
	return LogInput{Found: true, Content: b}
}

// findCodexLog 在 ~/.codex/sessions 下找 session_meta 的 cwd 等于窗格目录的最近一个 rollout。
func findCodexLog(home, paneDir string) LogInput {
	root := filepath.Join(home, ".codex", "sessions")
	var fs []fileMod
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		n := d.Name()
		if strings.HasPrefix(n, "rollout-") && strings.HasSuffix(n, ".jsonl") {
			if info, e := d.Info(); e == nil {
				fs = append(fs, fileMod{p, info.ModTime().UnixNano()})
			}
		}
		return nil
	})
	newestFirst(fs)
	want := canon(paneDir)
	for _, f := range fs {
		cwd, ok := codexCwd(f.path)
		if !ok || canon(cwd) != want {
			continue
		}
		b, err := readTail(f.path)
		if err != nil {
			return LogInput{Found: true, ReadErr: true}
		}
		return LogInput{Found: true, Content: b}
	}
	return LogInput{}
}

func codexCwd(path string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	// 首行 session_meta 含很长的 base_instructions（真实样本约 22KB），按行完整读，上限 4MB。
	r := bufio.NewReaderSize(f, 64<<10)
	var line []byte
	for {
		chunk, isPrefix, err := r.ReadLine()
		if err != nil {
			return "", false
		}
		line = append(line, chunk...)
		if !isPrefix {
			break
		}
		if len(line) > 4<<20 {
			return "", false
		}
	}
	var rec struct {
		Type    string `json:"type"`
		Payload struct {
			Cwd string `json:"cwd"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &rec) != nil || rec.Type != "session_meta" || rec.Payload.Cwd == "" {
		return "", false
	}
	return rec.Payload.Cwd, true
}
