package macexec

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// ErrBusy 表示已有一个任务在跑（同一时间只跑一个）。
var ErrBusy = errors.New("Mac 上正有任务在跑")

// Job 的 State 取值。
const (
	StateRunning = "running"
	StateDone    = "done"
	StateFailed  = "failed"
	StateTimeout = "timeout"
)

// MaxOutputBytes 是单个 job 的 stdout+stderr 合计上限；超过就杀掉进程组、任务标 failed。
const MaxOutputBytes int64 = 32 << 20

// waitDelay 是 cmd.WaitDelay：进程退出（或取消）之后输出管道最多再等这么久就强制关闭。
// 后台子进程继承 stdout/stderr 又不退出时，没有它 cmd.Wait 会永远卡在管道拷贝上。
const waitDelay = 5 * time.Second

// Spec 描述一次要执行的命令：argv 直接 exec（不经 shell），Env 为空则继承当前环境。
type Spec struct {
	Action  string        `json:"action"`
	Dir     string        `json:"dir"`
	Argv    []string      `json:"argv"`
	Env     []string      `json:"env"`
	Timeout time.Duration `json:"timeout"`
}

// Job 是一次任务的状态快照；State ∈ running|done|failed|timeout。
type Job struct {
	ID       string    `json:"id"`
	Action   string    `json:"action"`
	State    string    `json:"state"`
	ExitCode int       `json:"exitCode"`
	Started  time.Time `json:"started"`
	Ended    time.Time `json:"ended"`
	Note     string    `json:"note"`
}

// Runner 串行执行任务：dir 里存 <id>.log（完整输出）与 <id>.json（Job），
// 只保留最近 keep 个 job 的文件；内存没查到的 Job 回落到读 json（进程重启后也能查）。
type Runner struct {
	mu      sync.Mutex
	dir     string
	keep    int
	jobs    map[string]*Job // 内存里的 Job（含正在跑的）
	running *Job          // 当前 running 的任务，nil 表示空闲
}

// jobID 形如 20261016-093424-a1b2c3d4：时间（字典序即时间序）+ 8 位随机 hex。
var jobIDPattern = regexp.MustCompile(`^\d{8}-\d{6}-[0-9a-f]{8}$`)

// NewRunner 创建 Runner；dir 不存在会创建，keep 必须为正。
func NewRunner(dir string, keep int) (*Runner, error) {
	if keep <= 0 {
		return nil, fmt.Errorf("keep 必须为正整数，收到 %d", keep)
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("runner 目录无法转成绝对路径 %q: %w", dir, err)
	}
	if err := os.MkdirAll(absDir, 0o700); err != nil {
		return nil, fmt.Errorf("runner 目录创建失败 %q: %w", absDir, err)
	}
	r := &Runner{dir: absDir, keep: keep, jobs: map[string]*Job{}}
	r.recoverStaleRunning()
	return r, nil
}

// recoverStaleRunning 清算上个进程留下的 running 任务：一律改成 failed 并注明
// 「执行器重启，任务中断」——不去接管已失控的进程，只把状态记实。
// 在 NewRunner 里调用，此时 Runner 还没交给别人，不加锁。
func (r *Runner) recoverStaleRunning() {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		if !jobIDPattern.MatchString(id) {
			continue
		}
		data, err := os.ReadFile(r.jsonPath(id))
		if err != nil {
			continue
		}
		var job Job
		if err := json.Unmarshal(data, &job); err != nil {
			continue
		}
		if job.State != StateRunning {
			continue
		}
		job.State = StateFailed
		job.Note = "执行器重启，任务中断"
		if job.Ended.IsZero() {
			job.Ended = time.Now()
		}
		_ = r.persistLocked(&job)
	}
}

// Start 启动一个任务；已有 running 的返回 ErrBusy。
// stdout+stderr 一起写 <id>.log，进程结束后把 Job 原子写进 <id>.json。
func (r *Runner) Start(s Spec) (Job, error) {
	if len(s.Argv) == 0 || s.Argv[0] == "" {
		return Job{}, fmt.Errorf("Argv 为空，没有可执行的命令")
	}
	r.mu.Lock()
	if r.running != nil {
		running := *r.running
		r.mu.Unlock()
		return running, fmt.Errorf("%w（正在跑: %s %s）", ErrBusy, running.ID, running.Action)
	}
	id, err := newJobID()
	if err != nil {
		r.mu.Unlock()
		return Job{}, err
	}
	job := &Job{ID: id, Action: s.Action, State: StateRunning, Started: time.Now()}

	logPath := r.logPath(id)
	logFile, err := os.Create(logPath)
	if err != nil {
		r.mu.Unlock()
		return Job{}, fmt.Errorf("日志文件创建失败 %q: %w", logPath, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	if s.Timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, s.Timeout)
	}
	cmd := exec.CommandContext(ctx, s.Argv[0], s.Argv[1:]...)
	cmd.Dir = s.Dir
	if len(s.Env) > 0 {
		cmd.Env = s.Env
	}
	// 子进程放进独立进程组：build.sh/swift build 会派生子进程，只杀主进程它们还会跑。
	setNewProcessGroup(cmd)
	// 超时/取消时杀整个进程组（默认只杀主进程）。
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	// 兜底关闭管道：主进程先退出、后台子进程还继承着 stdout/stderr 时，
	// Wait 最多在进程退出后再等 waitDelay 就强制关掉父端管道，不会永挂。
	cmd.WaitDelay = waitDelay
	// stdout/stderr 共用一个计数写入器（exec 会为两路管道各起一个拷贝 goroutine，Write 会并发进来）。
	w := &cappedWriter{f: logFile, limit: MaxOutputBytes}
	w.onLimit = func() { _ = killProcessGroup(cmd) }
	cmd.Stdout = w
	cmd.Stderr = w

	if err := cmd.Start(); err != nil {
		logFile.Close()
		cancel()
		job.State = StateFailed
		job.ExitCode = -1
		job.Ended = time.Now()
		job.Note = "进程启动失败: " + err.Error()
		perr := r.persistLocked(job)
		r.rotateLocked()
		r.mu.Unlock()
		if perr != nil {
			return *job, fmt.Errorf("%v（且任务记录落盘失败: %v）", err, perr)
		}
		return *job, err
	}

	r.jobs[id] = job
	r.running = job
	// Start 返回前先落一份 State=running 的 json：执行器中途重启才能发现并清算这个任务。
	if perr := r.persistLocked(job); perr != nil {
		job.Note = "running 状态落盘失败: " + perr.Error()
	}
	// 自管截止计时器：CommandContext 的取消监视在主进程先退出那一刻就结束了
	// （watchCtx 把结果交给 Wait 后退出），之后若后台子进程继承着输出管道，
	// 超时不会再有任何人执行。这个计时器活到输出收集与清理全部结束，
	// 到点直接对进程组发 SIGKILL（杀不掉/组已空由 Wait + WaitDelay 兜底返回）。
	var deadlineTimer *time.Timer
	if s.Timeout > 0 {
		deadlineTimer = time.NewTimer(s.Timeout)
	}

	started := *job
	r.rotateLocked()
	r.mu.Unlock()

	go r.wait(ctx, cancel, cmd, w, job, deadlineTimer)
	return started, nil
}

// wait 在后台等进程结束，把最终状态写进内存与 json；
// 超时判定不依赖 CommandContext 的取消监视（它在主进程先退出时就已结束），
// 而是自己盯 deadlineTimer：到点先对进程组发 SIGKILL，再靠 Wait + WaitDelay 兜底返回；
// 返回后还对进程组补一次清理（扫掉继承输出管道还活着的后台子进程），
// 全部清理完成后才释放单任务占用。
func (r *Runner) wait(ctx context.Context, cancel context.CancelFunc, cmd *exec.Cmd, w *cappedWriter, job *Job, deadlineTimer *time.Timer) {
	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()

	var err error
	timedOut := false
	if deadlineTimer != nil {
		select {
		case err = <-waitErr:
			deadlineTimer.Stop()
		case <-deadlineTimer.C:
			timedOut = true
			// 进程组里还有活口（主进程没退，或后台子进程占着管道）：直接 SIGKILL 整组。
			_ = killProcessGroup(cmd)
			// 主进程收不掉时（setsid 逃逸等）由 WaitDelay 强制关管道，保证这里能返回。
			err = <-waitErr
		}
	} else {
		err = <-waitErr
	}

	// 清理：主进程已回收，进程组里若还有继承输出管道的后台进程（如 build.sh 起的守护），
	// 补一发 SIGKILL 扫掉，不留孤儿；组已空时 ESRCH 不算错。
	_ = killProcessGroup(cmd)
	w.close()
	defer cancel()

	r.mu.Lock()
	defer r.mu.Unlock()

	job.Ended = time.Now()
	job.ExitCode = exitCodeOf(cmd, err)
	switch {
	case w.overLimit():
		job.State = StateFailed
		job.Note = fmt.Sprintf("输出超过上限 %d 字节", MaxOutputBytes)
	case w.failErr() != nil:
		job.State = StateFailed
		job.Note = "日志写入失败: " + w.failErr().Error()
	case timedOut || ctx.Err() == context.DeadlineExceeded:
		job.State = StateTimeout
		job.Note = "超时被终止"
	case errors.Is(err, exec.ErrWaitDelay):
		// 主进程正常退出，只是后台子进程占着管道没退，由 WaitDelay 强制关闭并清理。
		job.State = StateDone
		job.Note = "主进程已退出，输出管道由 WaitDelay 强制关闭，残留后台进程已清理"
	case err != nil:
		job.State = StateFailed
		job.Note = err.Error()
	default:
		job.State = StateDone
	}
	r.running = nil
	if perr := r.persistLocked(job); perr != nil {
		if job.Note != "" {
			job.Note += "；"
		}
		job.Note += "任务记录落盘失败: " + perr.Error()
	}
	r.rotateLocked()
}

// Get 先查内存，没有就读 <id>.json（进程重启后也能查已完成的任务）。
func (r *Runner) Get(id string) (Job, error) {
	if !jobIDPattern.MatchString(id) {
		return Job{}, fmt.Errorf("任务 ID 不合法: %q", id)
	}
	r.mu.Lock()
	if j, ok := r.jobs[id]; ok {
		job := *j
		r.mu.Unlock()
		return job, nil
	}
	r.mu.Unlock()

	data, err := os.ReadFile(r.jsonPath(id))
	if err != nil {
		return Job{}, fmt.Errorf("任务不存在 %q: %w", id, err)
	}
	var job Job
	if err := json.Unmarshal(data, &job); err != nil {
		return Job{}, fmt.Errorf("任务记录损坏 %q: %w", id, err)
	}
	r.mu.Lock()
	if _, ok := r.jobs[id]; !ok {
		r.jobs[id] = &job
	}
	r.mu.Unlock()
	return job, nil
}

// cappedWriter 给日志文件计数：stdout/stderr 两路拷贝并发写入，所以要带锁。
// 累计超过 limit 时只写到 limit 为止，随后触发 onLimit（杀进程组）并置 exceeded；
// 写文件本身出错同样触发 onLimit 并记住错误，由 wait 把任务标 failed。
type cappedWriter struct {
	mu       sync.Mutex
	f        *os.File
	limit    int64
	total    int64
	exceeded bool
	writeErr error
	onLimit  func()
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.exceeded || w.writeErr != nil {
		// 已定局：数据直接丢弃（进程正在被杀，别让管道拷贝卡住）。
		return len(p), nil
	}
	n := int64(len(p))
	if w.total+n > w.limit {
		n = w.limit - w.total
		if n > 0 {
			if _, err := w.f.Write(p[:n]); err != nil {
				w.writeErr = err
				w.killNow()
				return 0, err
			}
			w.total += n
		}
		w.exceeded = true
		w.killNow()
		return len(p), nil
	}
	if _, err := w.f.Write(p); err != nil {
		w.writeErr = err
		w.killNow()
		return 0, err
	}
	w.total += n
	return len(p), nil
}

func (w *cappedWriter) killNow() {
	if w.onLimit != nil {
		w.onLimit()
	}
}

func (w *cappedWriter) close() error { return w.f.Close() }

func (w *cappedWriter) overLimit() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.exceeded
}

func (w *cappedWriter) failErr() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writeErr
}

// persistLocked 把 Job 原子写进 <dir>/<id>.json（临时文件 + rename）；须持有 r.mu。
func (r *Runner) persistLocked(job *Job) error {
	data, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		return fmt.Errorf("任务记录序列化失败: %w", err)
	}
	tmp, err := os.CreateTemp(r.dir, "."+job.ID+"-*.json.tmp")
	if err != nil {
		return fmt.Errorf("临时文件创建失败: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("临时文件写入失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("临时文件关闭失败: %w", err)
	}
	if err := os.Rename(tmpName, r.jsonPath(job.ID)); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("任务记录落盘失败: %w", err)
	}
	return nil
}

// rotateLocked 只保留最近 keep 个 job 的文件：按文件 mtime（真实创建顺序；同一秒内
// ID 靠随机后缀分先后，字典序不可靠）找出最旧的任务，删掉它们的 log/json 并从内存移除；
// 正在跑的任务无条件保住。须持有 r.mu。
func (r *Runner) rotateLocked() {
	if r.keep <= 0 {
		return
	}
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return
	}
	// 每个 job ID 取其 log/json 里最新的 mtime。
	youngest := map[string]time.Time{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		var stem string
		switch {
		case strings.HasSuffix(name, ".log"):
			stem = strings.TrimSuffix(name, ".log")
		case strings.HasSuffix(name, ".json"):
			stem = strings.TrimSuffix(name, ".json")
		default:
			continue
		}
		if !jobIDPattern.MatchString(stem) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if ts, ok := youngest[stem]; !ok || info.ModTime().After(ts) {
			youngest[stem] = info.ModTime()
		}
	}
	if len(youngest) <= r.keep {
		return
	}
	type jobFile struct {
		id string
		ts time.Time
	}
	jobs := make([]jobFile, 0, len(youngest))
	for id, ts := range youngest {
		jobs = append(jobs, jobFile{id, ts})
	}
	sort.Slice(jobs, func(i, j int) bool {
		if !jobs[i].ts.Equal(jobs[j].ts) {
			return jobs[i].ts.Before(jobs[j].ts)
		}
		return jobs[i].id < jobs[j].id
	})
	for _, j := range jobs[:len(jobs)-r.keep] {
		// 正在跑的任务必须保住，否则刚创建的 log 会被自己这轮轮转删掉。
		if r.running != nil && r.running.ID == j.id {
			continue
		}
		os.Remove(r.logPath(j.id))
		os.Remove(r.jsonPath(j.id))
		delete(r.jobs, j.id)
	}
}

func (r *Runner) logPath(id string) string  { return filepath.Join(r.dir, id+".log") }
func (r *Runner) jsonPath(id string) string { return filepath.Join(r.dir, id+".json") }

// newJobID 生成"时间+随机"的任务 ID。
func newJobID() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("任务 ID 随机部分生成失败: %w", err)
	}
	return time.Now().Format("20060102-150405") + "-" + hex.EncodeToString(b), nil
}

// exitCodeOf 取退出码：优先看进程自身的 ProcessState——Wait 因 WaitDelay 强制关管道时
// err 是 ErrWaitDelay 而不是 ExitError，但主进程可能正常退出 0，退出码不能因此丢成 -1；
// 取不到（进程没被收掉等）记 -1（被信号杀时 ProcessState.ExitCode 本身也是 -1）。
func exitCodeOf(cmd *exec.Cmd, err error) int {
	if cmd.ProcessState != nil {
		return cmd.ProcessState.ExitCode()
	}
	if err != nil {
		return -1
	}
	return 0
}

// Tail 读文件末尾：最多 maxBytes 字节里的最后 lines 行。
// 窗口从文件中间开始时丢掉开头残缺的一行；文件比窗口大时只回窗口内的内容。
func Tail(path string, lines, maxBytes int) (string, error) {
	if lines <= 0 {
		return "", fmt.Errorf("lines 必须为正整数，收到 %d", lines)
	}
	if maxBytes <= 0 {
		return "", fmt.Errorf("maxBytes 必须为正整数，收到 %d", maxBytes)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("文件打开失败 %q: %w", path, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("文件信息获取失败 %q: %w", path, err)
	}
	if st.IsDir() {
		return "", fmt.Errorf("路径是目录不是文件: %q", path)
	}
	size := st.Size()
	if size == 0 {
		return "", nil
	}
	window := size
	if window > int64(maxBytes) {
		window = int64(maxBytes)
	}
	buf := make([]byte, window)
	n, err := f.ReadAt(buf, size-window)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("文件读取失败 %q: %w", path, err)
	}
	buf = buf[:n]
	if size-window > 0 {
		// 窗口从行中间开始：丢掉开头残缺的一段；整窗没有换行说明最后一行本身就超长，原样保留。
		if i := bytes.IndexByte(buf, '\n'); i >= 0 {
			buf = buf[i+1:]
		}
	}
	text := strings.TrimSuffix(string(buf), "\n")
	if text == "" {
		return "", nil
	}
	ls := strings.Split(text, "\n")
	if len(ls) > lines {
		ls = ls[len(ls)-lines:]
	}
	return strings.Join(ls, "\n"), nil
}
