// Package agentstate 判断在某个工作区里干活的 agent 是否已做完。
// 只读、零 token：只看 tmux 窗格、屏幕文字、进程树和本机的会话记录文件，不调用任何模型。
// 规则见 go/PLAN-sync-v2.md 第十四、十五节与 go/PLAN-observer.md「状态判定」。
// 总原则：任何读取失败都算 Unknown，绝不当成 Done。
package agentstate

import (
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
	Tmux     string                        // tmux 命令，默认 "tmux"；测试用 "tmux -L 名字"
	Home     string                        // 用于找 ~/.claude 与 ~/.codex，默认当前用户主目录
	ProcTree func() (map[int][]int, error) // 返回 父 pid → 子 pid 列表；默认读 ps
}

// ── 纯判定 ────────────────────────────────────────────

// PaneInput 是纯函数 JudgePane 的输入。
type PaneInput struct {
	Command string
	Dead    bool

	// 进程树：ChildrenKnown=false 表示读不到，此时不能断言"无子进程"。
	ChildrenKnown bool
	HasChildren   bool

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
	if cmd != "claude" && cmd != "codex" {
		return Unknown, "前台程序 " + in.Command + " 不是已知的 agent"
	}
	if in.ScreenErr {
		return Unknown, "读不到窗格屏幕"
	}
	if strings.Contains(in.Screen, escMark) {
		return Busy, "屏幕有 esc to interrupt"
	}
	if !in.Log.Found {
		return Unknown, "找不到会话记录"
	}
	if in.Log.ReadErr {
		return Unknown, "会话记录读不了"
	}
	if cmd == "claude" {
		if claudeEndTurn(in.Log.Content) {
			return Done, "屏幕无 esc to interrupt，会话记录最后一条 end_turn"
		}
		return Unknown, "屏幕无 esc to interrupt，但会话记录最后不是 end_turn"
	}
	if codexTaskComplete(in.Log.Content) {
		return Done, "屏幕无 esc to interrupt，会话记录最后一个 task_started 之后有 task_complete"
	}
	return Unknown, "屏幕无 esc to interrupt，但会话记录里最后一个任务未见 task_complete"
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
	var tree map[int][]int
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
		if !p.Dead && isShell(cmd) {
			if !treeLoaded {
				tree, treeErr = procTree()
				treeLoaded = true
			}
			if treeErr == nil && tree != nil {
				in.ChildrenKnown = true
				in.HasChildren = len(tree[pid]) > 0
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
func psTree() (map[int][]int, error) {
	cmd := exec.Command("ps", "-A", "-o", "pid=,ppid=")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	tree := map[int][]int{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		pid, e1 := strconv.Atoi(f[0])
		ppid, e2 := strconv.Atoi(f[1])
		if e1 != nil || e2 != nil {
			continue
		}
		tree[ppid] = append(tree[ppid], pid)
	}
	return tree, nil
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
	buf := make([]byte, 64<<10)
	n, _ := f.Read(buf)
	line := buf[:n]
	if i := bytes.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
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
