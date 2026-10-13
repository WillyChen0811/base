package support

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// stopGrace 是 Stop 里 SIGINT 之后、升级 SIGKILL 之前的宽限时间；测试可调小。
var stopGrace = 5 * time.Second

const (
	defaultMaxEvents = 2000    // RunnerConfig.MaxEvents<=0 时的保留条数
	defaultLimit     = 500     // Events 的 limit<=0 时的默认值
	maxLineBytes     = 8 << 20 // 单行事件上限，超出截断并注明
	stderrTailBytes  = 4 << 10 // exited 事件带的标准错误末尾字节数
	stdoutBufBytes   = 1 << 16 // 读标准输出的 bufio 缓冲区
)

// RunnerConfig 是怎么起 claude 无界面进程。零值不自带任何项目默认值。
type RunnerConfig struct {
	Bin       string
	Dir       string
	Args      []string
	Env       []string
	MaxEvents int // 最坏内存约 MaxEvents×8MB（单行最多 maxLineBytes）
}

// Event 是一行标准输出。Data 是原样的一行 JSON；非 JSON 或超长截断的行包成 {"type":"raw",...}。
type Event struct {
	Seq  int64           `json:"seq"`
	Time time.Time       `json:"time"`
	Data json.RawMessage `json:"data"`
}

// RunnerState 是 Runner 的当前快照。
type RunnerState struct {
	Running   bool   `json:"running"`
	Busy      bool   `json:"busy"`
	SessionID string `json:"session_id"`
	LastError string `json:"error"`
	Turns     int    `json:"turns"`
}

// Runner 管一个长驻的 `claude -p --input-format stream-json --output-format stream-json --verbose`
// 进程：发消息、按序号取事件、查状态、停止；退出后再发消息用 --resume 重启。并发安全。
type Runner struct {
	cfg RunnerConfig

	mu      sync.Mutex
	stdinMu sync.Mutex // 串行化标准输入写入，避免并发时半行交错

	running   bool
	busy      bool
	stopping  bool // Stop 进行中：期间进程退出不算异常，不记 LastError
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdout    io.ReadCloser // 标准输出读端，Stop 兜底时强制关掉唤醒读 goroutine
	events    []Event
	seq       int64
	sessionID string
	lastError string
	turns     int
	notify    chan struct{} // 有新事件时关闭并换新，唤醒 Events 的等待者
	procDone  chan struct{} // 当前进程退出且读输出的 goroutine 结束后关闭
}

// NewRunner 按 cfg 建一个 Runner，不启动进程。
func NewRunner(cfg RunnerConfig) *Runner {
	return &Runner{
		cfg:    cfg,
		notify: make(chan struct{}),
	}
}

// Send 发一条用户消息；进程不在跑就先启动（已知会话号则带 --resume）。
// 发出后 Busy=true，等这一轮的 result 事件落回。文字为空返回错误。
func (r *Runner) Send(text string) error {
	if text == "" {
		return errors.New("support: 消息内容为空")
	}
	var msg struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	}
	msg.Type = "user"
	msg.Message.Role = "user"
	msg.Message.Content = text
	line, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("support: 编码用户消息失败: %w", err)
	}
	line = append(line, '\n')

	r.mu.Lock()
	if !r.running {
		if err := r.startLocked(); err != nil {
			r.lastError = err.Error()
			r.mu.Unlock()
			return err
		}
	}
	stdin := r.stdin
	r.busy = true // 在同一把锁里置位，防止 result 先到后被这里改回 true
	r.mu.Unlock()

	r.stdinMu.Lock()
	_, werr := stdin.Write(line)
	r.stdinMu.Unlock()
	if werr != nil {
		r.mu.Lock()
		r.busy = false // 消息没发出去，回滚
		r.mu.Unlock()
		return fmt.Errorf("support: 写标准输入失败: %w", werr)
	}
	return nil
}

// Events 返回 Seq > after 的事件（最多 limit 条，<=0 视为 500）和当前最新 Seq。
// 没有新事件且 wait>0 时最多等 wait（或 ctx 结束）。after 早于缓冲最旧事件时从最旧的给。
func (r *Runner) Events(ctx context.Context, after int64, limit int, wait time.Duration) ([]Event, int64) {
	if limit <= 0 {
		limit = defaultLimit
	}
	// 每轮最多再等一个 wait：被别人的广播空叫醒时不会重新吃满一份 wait。
	deadline := time.Now().Add(wait)
	for remaining := time.Until(deadline); remaining > 0; remaining = time.Until(deadline) {
		r.mu.Lock()
		ev, seq := r.collectLocked(after, limit)
		var ch chan struct{}
		if len(ev) == 0 {
			ch = r.notify // 在同一把锁里取通道，不会错过紧随其后的广播
		}
		r.mu.Unlock()
		if len(ev) > 0 {
			return ev, seq
		}
		timer := time.NewTimer(remaining)
		select {
		case <-ch:
			timer.Stop()
			// 被广播叫醒；事件可能已被别人取走，回循环用剩余时间重查
		case <-ctx.Done():
			timer.Stop()
			r.mu.Lock()
			defer r.mu.Unlock()
			return r.collectLocked(after, limit)
		case <-timer.C:
			r.mu.Lock()
			defer r.mu.Unlock()
			return r.collectLocked(after, limit)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.collectLocked(after, limit)
}

// State 返回当前状态快照。
func (r *Runner) State() RunnerState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return RunnerState{
		Running:   r.running,
		Busy:      r.busy,
		SessionID: r.sessionID,
		LastError: r.lastError,
		Turns:     r.turns,
	}
}

// Stop 结束进程：关标准输入、对整个进程组发 SIGINT，等 stopGrace，
// 还没退就 SIGKILL。返回时进程已退出、读输出的 goroutine 已结束。
// 没在跑时返回 nil；可重复调用；之后还能再 Send（带 --resume 重启）。
func (r *Runner) Stop() error {
	grace := stopGrace
	r.mu.Lock()
	cmd, stdin, stdout, done := r.cmd, r.stdin, r.stdout, r.procDone
	if cmd != nil {
		r.stopping = true
	}
	r.mu.Unlock()
	if cmd == nil {
		return nil
	}
	if stdin != nil {
		// 不拿 stdinMu：Send 阻塞写时持有它，这里拿会互等。
		// StdinPipe 的写端是可 poll 管道，Close 会 evict 叫醒阻塞中的 Write。
		_ = stdin.Close()
	}
	select {
	case <-done:
		return nil // 已经退干净，不再发信号，避免 pid 复用打错进程组
	default:
	}
	interruptGroup(cmd)
	select {
	case <-done:
		return nil
	case <-time.After(grace):
		select {
		case <-done:
			return nil // 宽限期刚到时恰好退出，别再发 SIGKILL
		default:
		}
	}
	killGroup(cmd)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		// 兜底：SIGKILL 后还有孙进程占着管道时，关掉读端强制读 goroutine 退出
		if stdout != nil {
			_ = stdout.Close()
		}
		<-done
	}
	return nil
}

// startLocked 启动进程并挂上读输出的 goroutine。调用方持有 r.mu。
func (r *Runner) startLocked() error {
	args := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose"}
	if r.sessionID != "" {
		args = append(args, "--resume", r.sessionID)
	}
	args = append(args, r.cfg.Args...)

	cmd := exec.Command(r.cfg.Bin, args...)
	cmd.Dir = r.cfg.Dir
	cmd.Env = append(claudeEnv(os.Environ()), r.cfg.Env...)
	setupProc(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("support: 建标准输入管道失败: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("support: 建标准输出管道失败: %w", err)
	}
	stderr := &tailBuffer{max: stderrTailBytes}
	cmd.Stderr = stderr // Wait 会等这份复制结束

	// 逃出进程组的孙进程可能一直占着 stderr/stdout 管道，没有 WaitDelay 时 Wait 会永远不返回。
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("support: 启动 claude 失败: %w", err)
	}
	r.cmd = cmd
	r.stdin = stdin
	r.stdout = stdout
	r.running = true
	r.stopping = false
	r.lastError = "" // 重新启动成功，旧的异常记录清零
	r.procDone = make(chan struct{})

	go func() {
		// 必须读完标准输出再 Wait（StdoutPipe 的约定）；stderr 是自定义 Writer，Wait 自己等。
		// 阻塞的读可被 Close 打断（Stop 的兜底依赖这一点），所以最先做读、其次 Wait。
		r.readStdout(stdout)
		cmd.Wait() // 退出码通过 cmd.ProcessState 取，这里的 error 不用
		r.finish(cmd, stderr)
	}()
	return nil
}

// claudeEnv 从环境里挑出该传给 Claude 的变量（白名单），其余（如各种密钥）不继承。
func claudeEnv(environ []string) []string {
	keep := map[string]bool{
		"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "SHELL": true,
		"TMPDIR": true, "LANG": true, "TERM": true, "TZ": true,
		"http_proxy": true, "https_proxy": true, "no_proxy": true, "all_proxy": true,
		"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true, "ALL_PROXY": true,
	}
	var out []string
	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")
		if keep[k] || strings.HasPrefix(k, "LC_") || strings.HasPrefix(k, "ANTHROPIC_") || strings.HasPrefix(k, "CLAUDE_") {
			out = append(out, kv)
		}
	}
	return out
}

// finish 在进程退出后落 exited 事件、更新状态、关掉 procDone。
func (r *Runner) finish(cmd *exec.Cmd, stderr *tailBuffer) {
	code := exitCodeOf(cmd)
	tail := stderr.String()

	r.mu.Lock()
	if r.cmd == cmd { // 旧的退出别碰新进程的状态
		r.cmd = nil
		r.stdin = nil
		r.stdout = nil
		r.running = false
		r.busy = false
		if code == 0 || r.stopping {
			r.lastError = "" // 正常退出和主动 Stop 都不算异常
		} else {
			r.lastError = fmt.Sprintf("claude 退出码 %d：%s", code, lastLine(tail))
		}
	}
	data, err := json.Marshal(struct {
		Type   string `json:"type"`
		Code   int    `json:"code"`
		Stderr string `json:"stderr"`
	}{Type: "exited", Code: code, Stderr: tail})
	if err == nil {
		r.appendEventLocked(data)
	}
	close(r.procDone)
	r.mu.Unlock()
}

// readStdout 逐行读标准输出；单行超上限时只留前 maxLineBytes 并注明截断，然后继续。
func (r *Runner) readStdout(rc io.Reader) {
	br := bufio.NewReaderSize(rc, stdoutBufBytes)
	for {
		line, truncated, err := readCappedLine(br)
		if len(line) > 0 || truncated {
			r.handleLine(line, truncated)
		}
		if err != nil {
			return
		}
	}
}

// readCappedLine 读一行（不含换行符）；超过 maxLineBytes 截断，但仍把本行剩余部分吃掉。
func readCappedLine(br *bufio.Reader) ([]byte, bool, error) {
	var buf []byte
	truncated := false
	for {
		chunk, prefix, err := br.ReadLine()
		if !truncated && len(chunk) > 0 {
			if len(buf)+len(chunk) > maxLineBytes {
				buf = append(buf, chunk[:maxLineBytes-len(buf)]...)
				truncated = true
			} else {
				buf = append(buf, chunk...)
			}
		}
		if err != nil {
			return buf, truncated, err
		}
		if !prefix {
			return buf, truncated, nil
		}
	}
}

// handleLine 把一行输出变成事件；顺带更新 session_id 和 Busy/Turns。
// JSON 的校验/解析/封装全在锁外做（单行可达 8MB），锁内只更新状态和追加事件。
func (r *Runner) handleLine(line []byte, truncated bool) {
	var data json.RawMessage
	var eventType, sessionID string
	if !truncated && json.Valid(line) {
		data = append(json.RawMessage(nil), line...)
		var head struct {
			Type      string `json:"type"`
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal(line, &head) == nil {
			eventType, sessionID = head.Type, head.SessionID
		}
	} else {
		// 非 JSON 或截断的行：包成 raw，截断的注明
		var err error
		data, err = json.Marshal(struct {
			Type      string `json:"type"`
			Text      string `json:"text"`
			Truncated bool   `json:"truncated,omitempty"`
		}{Type: "raw", Text: string(line), Truncated: truncated})
		if err != nil {
			return
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if sessionID != "" {
		r.sessionID = sessionID
	}
	if eventType == "result" {
		r.turns++
		r.busy = false
	}
	r.appendEventLocked(data)
}

// appendEventLocked 追加事件（编号从 1 起严格递增）、超额丢最旧、广播唤醒等待者。
func (r *Runner) appendEventLocked(data json.RawMessage) {
	r.seq++
	r.events = append(r.events, Event{Seq: r.seq, Time: time.Now(), Data: data})
	max := r.cfg.MaxEvents
	if max <= 0 {
		max = defaultMaxEvents
	}
	if over := len(r.events) - max; over > 0 {
		r.events = append(r.events[:0], r.events[over:]...)
	}
	close(r.notify)
	r.notify = make(chan struct{})
}

// collectLocked 取 Seq > after 的前 limit 条，并带上最新 Seq。
func (r *Runner) collectLocked(after int64, limit int) ([]Event, int64) {
	var out []Event
	for _, e := range r.events {
		if e.Seq > after {
			out = append(out, e)
			if len(out) == limit {
				break
			}
		}
	}
	return out, r.seq
}

// tailBuffer 只保留最后 max 字节的写入内容。
type tailBuffer struct {
	buf []byte
	max int
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.buf = append(b.buf, p...)
	if over := len(b.buf) - b.max; over > 0 {
		b.buf = append(b.buf[:0], b.buf[over:]...)
	}
	return len(p), nil
}

func (b *tailBuffer) String() string { return string(b.buf) }

// lastLine 取文本最后一行（去掉尾部空行）；全空时原样返回。
func lastLine(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return s
}
