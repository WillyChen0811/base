package macexec

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"time"
)

// ErrBusy 表示已有任务在跑：同一时间只跑一个，第二个调用立刻被拒，不排队等。
var ErrBusy = errors.New("Mac 上正有任务在跑")

// tailLimitBytes：stdout+stderr 合计在内存里只留最后 64KB；
// waitDelay：进程退出后输出管道最多再等这么久就强制关闭（防继承管道的后台子进程卡死 Wait）。
const (
	tailLimitBytes = 64 << 10
	waitDelay      = 5 * time.Second
)

var busy sync.Mutex // 进程级单任务闸门：Run 从起进程到收完输出都持着它，返回即释放。

// Spec 描述一次要执行的命令：argv 直接 exec（不经 shell）；Timeout 为 0 时不自设截止，只受 ctx 约束。
type Spec struct {
	Action  string
	Dir     string
	Argv    []string
	Timeout time.Duration
}

// Result 是一次同步执行的结果：非零退出码与超时都是数据（此时 error 为 nil）；error 只表示没跑成。
type Result struct {
	ExitCode int
	TimedOut bool
	Seconds  float64
	Tail     string
}

// Run 同步执行一条命令并返回结果；超时或 ctx 取消时杀整个进程组（Cancel=killProcessGroup，
// 子进程经 setNewProcessGroup 自成一组，swift build 派生的子进程不残留）。拿不到锁立刻返回 ErrBusy。
func Run(ctx context.Context, s Spec) (Result, error) {
	if len(s.Argv) == 0 || s.Argv[0] == "" {
		return Result{}, fmt.Errorf("Argv 为空，没有可执行的命令")
	}
	if !busy.TryLock() {
		return Result{}, ErrBusy
	}
	defer busy.Unlock()
	runCtx := ctx
	if s.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, s.Timeout)
		defer cancel() // 进程已回收，这里只释放计时器资源。
	}
	cmd := exec.CommandContext(runCtx, s.Argv[0], s.Argv[1:]...)
	cmd.Dir = s.Dir
	setNewProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = waitDelay
	tw := &tailWriter{}
	cmd.Stdout, cmd.Stderr = tw, tw
	started := time.Now()
	if err := cmd.Start(); err != nil {
		return Result{Seconds: time.Since(started).Seconds()}, fmt.Errorf("进程启动失败 %q: %w", s.Argv[0], err)
	}
	waitErr := cmd.Wait()
	_ = killProcessGroup(cmd) // 补杀残留：主进程先退时取消监视已收工、只剩 WaitDelay 兜底；组已空 ESRCH 不算错。

	res := Result{Seconds: time.Since(started).Seconds(), Tail: tw.String()}
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}
	switch {
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		res.TimedOut = true // 超时是结果，不是错误
		return res, nil
	case errors.Is(runCtx.Err(), context.Canceled):
		return res, fmt.Errorf("执行被取消: %w", runCtx.Err())
	case cmd.ProcessState != nil: // 跑完了：非零退出码与 ErrWaitDelay 都是数据，不当错误
		return res, nil
	default:
		return res, waitErr
	}
}

// tailWriter 把 stdout+stderr 缓存进内存并只留尾部 tailLimitBytes：两路拷贝 goroutine 并发 Write 要带锁。
type tailWriter struct {
	mu  sync.Mutex
	buf []byte
}

func (w *tailWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	if over := len(w.buf) - tailLimitBytes; over > 0 {
		w.buf = append([]byte(nil), w.buf[over:]...)
	}
	return len(p), nil
}

func (w *tailWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(w.buf)
}
