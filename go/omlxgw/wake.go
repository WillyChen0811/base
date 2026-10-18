package omlxgw

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// waker 负责探测上游是否醒着、敲门唤醒，同一时刻只有一次唤醒在进行。
type waker struct {
	logger   *log.Logger
	upstream string
	unit     time.Duration // 配置里「秒」对应的时长，测试里改小

	probeTimeout time.Duration
	knockTimeout time.Duration
	poll         time.Duration
	fresh        time.Duration

	mu       sync.Mutex
	lastOK   time.Time
	cur      *wakeCall
	lastWake *time.Duration
}

type wakeCall struct {
	done chan struct{}
	err  error
}

func newWaker(upstream string, logger *log.Logger) *waker {
	return &waker{logger: logger, upstream: upstream, unit: time.Second,
		probeTimeout: 3 * time.Second, knockTimeout: 5 * time.Second, poll: 3 * time.Second, fresh: 30 * time.Second}
}

func (w *waker) markOK() {
	w.mu.Lock()
	w.lastOK = time.Now()
	w.mu.Unlock()
}

// probe 探上游：任何 HTTP 响应都算醒。
func (w *waker) probe(ctx context.Context) bool {
	pc, cancel := context.WithTimeout(ctx, w.probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(pc, http.MethodGet, w.upstream+"/v1/models", nil)
	if err != nil {
		return false
	}
	resp, err := (&http.Client{Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}}).Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	w.markOK()
	return true
}

func (w *waker) knock(cfg *WakeConfig) {
	for _, p := range cfg.KnockPorts {
		c, err := net.DialTimeout("tcp", net.JoinHostPort(cfg.Host, strconv.Itoa(p)), w.knockTimeout)
		if err == nil {
			c.Close()
		}
	}
}

// ensure 保证上游醒着。force 为真时忽略「最近成功」的缓存。
func (w *waker) ensure(ctx context.Context, cfg *WakeConfig, force bool) error {
	w.mu.Lock()
	if !force && w.cur == nil && time.Since(w.lastOK) < w.fresh && !w.lastOK.IsZero() {
		w.mu.Unlock()
		return nil
	}
	c := w.cur
	if c == nil {
		c = &wakeCall{done: make(chan struct{})}
		w.cur = c
		go w.run(c, cfg)
	}
	w.mu.Unlock()
	select {
	case <-c.done:
		return c.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *waker) run(c *wakeCall, cfg *WakeConfig) {
	maxWait := time.Duration(cfg.MaxWaitSeconds) * w.unit
	ctx, cancel := context.WithTimeout(context.Background(), maxWait)
	defer cancel()
	t0 := time.Now()
	defer func() {
		w.mu.Lock()
		w.cur = nil
		w.mu.Unlock()
		close(c.done)
	}()
	if w.probe(ctx) {
		return
	}
	w.logger.Printf("%s 唤醒开始 host=%s ports=%v", time.Now().Format("2006-01-02 15:04:05"), cfg.Host, cfg.KnockPorts)
	w.knock(cfg)
	for {
		select {
		case <-ctx.Done():
			c.err = fmt.Errorf("Studio 没叫醒（等了 %d 秒）", cfg.MaxWaitSeconds)
			w.logger.Printf("%s 唤醒超时 等了 %.1fs", time.Now().Format("2006-01-02 15:04:05"), time.Since(t0).Seconds())
			return
		case <-time.After(w.poll):
		}
		if w.probe(ctx) {
			d := time.Since(t0)
			w.mu.Lock()
			w.lastWake = &d
			w.mu.Unlock()
			w.logger.Printf("%s 唤醒成功 耗时 %.1fs", time.Now().Format("2006-01-02 15:04:05"), d.Seconds())
			return
		}
	}
}

func (w *waker) status() map[string]any {
	w.mu.Lock()
	defer w.mu.Unlock()
	var checked, last any
	if !w.lastOK.IsZero() {
		checked = time.Since(w.lastOK).Seconds()
	}
	if w.lastWake != nil {
		last = w.lastWake.Seconds()
	}
	return map[string]any{"awake_checked_ago_seconds": checked, "waking": w.cur != nil, "last_wake_seconds": last}
}

type wakeCtxKey struct{}

// wakeTransport 记录上游成功响应；对要唤醒的请求，dial 失败时唤醒后重试一次。
type wakeTransport struct {
	base http.RoundTripper
	g    *Gateway
}

func (t *wakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err == nil {
		t.g.waker.markOK()
		return resp, nil
	}
	var oe *net.OpError
	cfg := t.g.sched.config().Wake
	if cfg == nil || req.Context().Value(wakeCtxKey{}) == nil || !errors.As(err, &oe) || oe.Op != "dial" {
		return nil, err
	}
	if werr := t.g.waker.ensure(req.Context(), cfg, true); werr != nil {
		return nil, werr
	}
	if req.GetBody != nil {
		b, gerr := req.GetBody()
		if gerr != nil {
			return nil, err
		}
		req = req.Clone(req.Context())
		req.Body = b
	}
	resp, err = t.base.RoundTrip(req)
	if err == nil {
		t.g.waker.markOK()
	}
	return resp, err
}

// keeper 有活时周期性起防睡命令。
type keeper struct {
	logger *log.Logger
	cfgf   func() *KeepAwakeConfig
	unit   time.Duration
	tick   time.Duration

	mu       sync.Mutex
	lastAct  time.Time
	inflight int // 在途请求数，大于 0 时不算空闲
	lastRun  time.Time
	procs    map[*exec.Cmd]struct{}
	closed   bool
	stop     chan struct{}
	wg       sync.WaitGroup
}

func newKeeper(cfgf func() *KeepAwakeConfig, logger *log.Logger) *keeper {
	return &keeper{logger: logger, cfgf: cfgf, unit: time.Second, tick: time.Second,
		procs: map[*exec.Cmd]struct{}{}, stop: make(chan struct{})}
}

// begin 请求开始：在途 +1 并刷新活动时间。
func (k *keeper) begin() {
	k.mu.Lock()
	k.inflight++
	k.lastAct = time.Now()
	k.mu.Unlock()
}

// end 请求结束：在途 -1 并刷新活动时间，长请求结束后仍有一个空闲窗口。
func (k *keeper) end() {
	k.mu.Lock()
	if k.inflight > 0 {
		k.inflight--
	}
	k.lastAct = time.Now()
	k.mu.Unlock()
}

func (k *keeper) start() {
	k.wg.Add(1)
	go func() {
		defer k.wg.Done()
		t := time.NewTicker(k.tick)
		defer t.Stop()
		for {
			select {
			case <-k.stop:
				return
			case <-t.C:
				k.step()
			}
		}
	}()
}

func (k *keeper) step() {
	cfg := k.cfgf()
	if cfg == nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	now := time.Now()
	if k.closed || (k.inflight == 0 && (k.lastAct.IsZero() || now.Sub(k.lastAct) >= time.Duration(cfg.IdleSeconds)*k.unit)) {
		return
	}
	if !k.lastRun.IsZero() && now.Sub(k.lastRun) < time.Duration(cfg.RefreshSeconds)*k.unit {
		return
	}
	cmd := exec.Command(cfg.Command[0], cfg.Command[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		k.lastRun = now
		k.logger.Printf("%s 防睡启动失败: %v", now.Format("2006-01-02 15:04:05"), err)
		return
	}
	k.lastRun = now
	k.procs[cmd] = struct{}{}
	k.logger.Printf("%s 防睡起进程 pid=%d", now.Format("2006-01-02 15:04:05"), cmd.Process.Pid)
	k.wg.Add(1)
	go func() {
		defer k.wg.Done()
		err := cmd.Wait()
		k.mu.Lock()
		delete(k.procs, cmd)
		k.mu.Unlock()
		k.logger.Printf("%s 防睡进程退出 pid=%d 退出码=%d err=%v", time.Now().Format("2006-01-02 15:04:05"),
			cmd.Process.Pid, cmd.ProcessState.ExitCode(), err)
	}()
}

// close 停循环并杀掉还活着的子进程。
func (k *keeper) close() {
	k.mu.Lock()
	if !k.closed {
		k.closed = true
		close(k.stop)
	}
	for c := range k.procs {
		syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
	}
	k.mu.Unlock()
	k.wg.Wait()
}

func (k *keeper) status() map[string]any {
	cfg := k.cfgf()
	k.mu.Lock()
	defer k.mu.Unlock()
	var started any
	if !k.lastRun.IsZero() {
		started = time.Since(k.lastRun).Seconds()
	}
	active := cfg != nil && (k.inflight > 0 || (!k.lastAct.IsZero() && time.Since(k.lastAct) < time.Duration(cfg.IdleSeconds)*k.unit))
	return map[string]any{"active": active, "last_started_ago_seconds": started, "running": len(k.procs)}
}
