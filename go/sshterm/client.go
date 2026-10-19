package sshterm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// keepalive 参数；测试里调小。
var (
	keepAliveInterval = 15 * time.Second
	// 单次请求超过 keepAliveTimeout 无响应立即判定断开。程序内隧道会成串丢包、偶尔卡十几秒，
	// 10 秒会把卡顿误判成断线、踢掉终端；放宽到 30 秒，真断线最晚约 45 秒发现（传输出错仍立即发现）。
	keepAliveTimeout  = 30 * time.Second
	keepAliveMaxFails = 3 // 连续这么多次请求出错才判定断开
)

// failCounter 统计连续失败：成功清零，达到 max 次连续失败返回 true。
type failCounter struct{ n, max int }

func (f *failCounter) record(ok bool) (dead bool) {
	if ok {
		f.n = 0
		return false
	}
	f.n++
	return f.n >= f.max
}

// runGrace 是 ctx 到期后等 Run 自己返回的宽限；超过则放弃等待（不关连接）。测试里调小。
var runGrace = 2 * time.Second

// Run 的输出上限。
const (
	maxRunStdout = 1 << 20 // 标准输出最多收 1MB
	maxRunStderr = 4 << 10 // 报错时带出的标准错误前 4KB
)

// RunInput 的输出上限（stdout、stderr 各自）。
const maxRunInputOut = 8 << 20

// ErrOutputTooLarge 表示 Run 的标准输出超过 1MB 上限。
var ErrOutputTooLarge = errors.New("sshterm: 标准输出超过 1MB")

// ExitError 是 Run 遇到的非零退出码。
type ExitError struct {
	Code   int
	Stderr string
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("sshterm: 远端命令退出码 %d: %s", e.Code, e.Stderr)
}

// Client 是一条可复用的 SSH 连接：上面可以同时开多个终端会话、执行短命令。并发安全。
type Client struct {
	client *ssh.Client

	done      chan struct{}
	closeOnce sync.Once

	mu  sync.Mutex
	err error
}

// Dial 经 d 连 t.Addr、用 t.User 和 a 完成握手，起 keepalive。t.Command 在这里不用。
func Dial(ctx context.Context, d Dialer, t Target, a Auth) (*Client, error) {
	cc, err := dialConn(ctx, d, t, a)
	if err != nil {
		return nil, err
	}
	c := &Client{client: cc, done: make(chan struct{})}
	// 同步快照 keepalive 参数再传给 goroutine，避免 goroutine 里读全局变量与测试改值竞争
	go c.keepAlive(keepAliveInterval, keepAliveTimeout)
	// 底层传输一结束（对端关、传输出错、本地 Close）就标记断开并关掉 Done，
	// 不用等 keepalive 的下一次探测；fail 幂等，与 Close/keepalive 共用同一次收尾。
	go func() {
		werr := cc.Wait()
		if werr == nil {
			werr = errors.New("连接已结束")
		}
		c.fail(werr)
	}()
	return c, nil
}

// OpenSession 在这条连接上开一个 PTY 会话并启动 command（空则登录 shell）。
// Close 只关这个会话，不关连接。ctx 只管开会话这几步，到期则放弃并返回 ctx 错误（不关连接）。
func (c *Client) OpenSession(ctx context.Context, command string, cols, rows int) (*Session, error) {
	if err := c.live(); err != nil {
		return nil, err
	}
	sess, err := c.client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("sshterm: 开会话失败: %w", err)
	}
	in, out, err := startPtySession(ctx, sess, command, cols, rows)
	if err != nil {
		sess.Close()
		return nil, err
	}
	return &Session{sess: sess, in: in, out: out}, nil
}

// startPtySession 申请 PTY、取管道、启动程序；这几步 ctx 到期就放弃（由调用方关会话，不关连接）。
func startPtySession(ctx context.Context, sess *ssh.Session, command string, cols, rows int) (io.WriteCloser, io.Reader, error) {
	stop := context.AfterFunc(ctx, func() { sess.Close() })
	defer stop()
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 115200,
		ssh.TTY_OP_OSPEED: 115200,
	}
	if err := sess.RequestPty("xterm-256color", rows, cols, modes); err != nil {
		return nil, nil, ctxOr(ctx, fmt.Errorf("sshterm: 申请 PTY 失败: %w", err))
	}
	// StdinPipe/StdoutPipe 必须在 Start/Shell 之前取。
	in, err := sess.StdinPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("sshterm: 取 stdin 失败: %w", err)
	}
	out, err := sess.StdoutPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("sshterm: 取 stdout 失败: %w", err)
	}
	if command != "" {
		err = sess.Start(command)
	} else {
		err = sess.Shell()
	}
	if err != nil {
		return nil, nil, ctxOr(ctx, fmt.Errorf("sshterm: 启动远端程序失败: %w", err))
	}
	return in, out, nil
}

// interruptible 在 goroutine 里跑 work，保证 ctx 到期后调用方一定能返回。
// 正常情况下 work 自己会因 ctx 到期关会话而返回；但对端失联（线路切换、睡眠）时，
// 本地关会话不会唤醒阻塞的读/Wait，开会话也可能一直等不到回应。
// 所以 ctx 到期后再给 runGrace 的宽限，仍未返回就放弃等待，返回带「超时」字样、保留 ctx.Err() 链的错误。
// 取舍：这里绝不关共用连接。OpenSSH 对 exec 通道的 close 要等远端子进程退出才回，
// 慢命令很容易超过宽限；若因此关连接，会连带断掉所有终端会话、消息投递和项目同步。
// 连接是否真死了只交给 keepalive 判断；被放弃的 goroutine 留着，work 内部已在 ctx 到期时
// 关掉了它自己的 ssh session，连接真断时（keepalive 或传输层出错关连接）自然返回而被回收。
func (c *Client) interruptible(ctx context.Context, work func() error) error {
	done := make(chan error, 1)
	go func() { done <- work() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}
	t := time.NewTimer(runGrace)
	defer t.Stop()
	select {
	case err := <-done:
		return err
	case <-t.C:
		return fmt.Errorf("sshterm: 远端命令超时（无响应，已放弃等待，连接保留）: %w", ctx.Err())
	}
}

// ctxOr 返回 ctx 的错误（优先，便于调用方识别取消），否则返回原错误。
func ctxOr(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return err
}

// Run 不申请 PTY 执行 command，返回标准输出。退出码非 0 时返回 *ExitError（带退出码和标准错误前 4KB）。
// 标准输出最多收 1MB，超出返回错误。ctx 到期关掉这个会话（不关连接）并返回 ctx 错误。
func (c *Client) Run(ctx context.Context, command string) ([]byte, error) {
	var out []byte
	err := c.interruptible(ctx, func() (err error) {
		out, err = c.run(ctx, command)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) run(ctx context.Context, command string) ([]byte, error) {
	if err := c.live(); err != nil {
		return nil, err
	}
	sess, err := c.client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("sshterm: 开会话失败: %w", err)
	}
	stop := context.AfterFunc(ctx, func() { sess.Close() })
	defer stop()

	out, err := sess.StdoutPipe()
	if err != nil {
		sess.Close()
		return nil, fmt.Errorf("sshterm: 取 stdout 失败: %w", err)
	}
	errPipe, err := sess.StderrPipe()
	if err != nil {
		sess.Close()
		return nil, fmt.Errorf("sshterm: 取 stderr 失败: %w", err)
	}
	if err := sess.Start(command); err != nil {
		sess.Close()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("sshterm: 启动远端命令失败: %w", err)
	}

	stdout, err := io.ReadAll(io.LimitReader(out, maxRunStdout+1))
	if err != nil {
		sess.Close()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("sshterm: 读远端输出失败: %w", err)
	}
	if len(stdout) > maxRunStdout {
		sess.Close() // 关会话即丢弃剩余输出，不会阻塞；连接不受影响
		return nil, ErrOutputTooLarge
	}
	stderr, _ := io.ReadAll(io.LimitReader(errPipe, maxRunStderr))
	// 剩下的 stderr 必须读完丢弃：远端 stderr 很大时不读完，远端写满管道会阻塞、进程不退出，Wait 挂到超时。
	io.Copy(io.Discard, errPipe)

	werr := sess.Wait()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if werr != nil {
		var ee *ssh.ExitError
		if errors.As(werr, &ee) {
			return nil, &ExitError{Code: ee.ExitStatus(), Stderr: string(stderr)}
		}
		return nil, fmt.Errorf("sshterm: 远端命令失败: %w", werr)
	}
	return stdout, nil
}

// RunInput 同 Run，但把 stdin 写给远端命令（写完关闭输入），并同时返回标准输出、标准错误和退出码。
// 退出码非 0 不算错误（exit 里给出）；连接/会话/超时/输出超限才返回 err。
// 输出上限：stdout 与 stderr 各 maxRunInputOut。命令串由调用方负责安全，需要传的数据请走 stdin。
func (c *Client) RunInput(ctx context.Context, command string, stdin []byte) (stdout, stderr []byte, exit int, err error) {
	var so, se []byte // 局部变量：超时放弃后 goroutine 才返回时不会写到已返回函数的结果上
	var ex int
	err = c.interruptible(ctx, func() (e error) {
		so, se, ex, e = c.runInput(ctx, command, stdin)
		return e
	})
	if err != nil {
		return nil, nil, -1, err
	}
	return so, se, ex, nil
}

func (c *Client) runInput(ctx context.Context, command string, stdin []byte) (stdout, stderr []byte, exit int, err error) {
	if err := c.live(); err != nil {
		return nil, nil, -1, err
	}
	sess, err := c.client.NewSession()
	if err != nil {
		return nil, nil, -1, fmt.Errorf("sshterm: 开会话失败: %w", err)
	}
	defer sess.Close()
	stop := context.AfterFunc(ctx, func() { sess.Close() })
	defer stop()

	in, err := sess.StdinPipe()
	if err != nil {
		return nil, nil, -1, fmt.Errorf("sshterm: 取 stdin 失败: %w", err)
	}
	out, err := sess.StdoutPipe()
	if err != nil {
		return nil, nil, -1, fmt.Errorf("sshterm: 取 stdout 失败: %w", err)
	}
	errPipe, err := sess.StderrPipe()
	if err != nil {
		return nil, nil, -1, fmt.Errorf("sshterm: 取 stderr 失败: %w", err)
	}
	if err := sess.Start(command); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, nil, -1, ctxErr
		}
		return nil, nil, -1, fmt.Errorf("sshterm: 启动远端命令失败: %w", err)
	}
	wc := make(chan error, 1)
	go func() { // 输入写完就关；写失败（远端提前退出、连接断）记下来，最后显式返回
		_, werr := in.Write(stdin)
		cerr := in.Close()
		if werr == nil {
			werr = cerr
		}
		wc <- werr
	}()
	type res struct {
		b   []byte
		err error
	}
	ec := make(chan res, 1)
	go func() {
		b, e := io.ReadAll(io.LimitReader(errPipe, maxRunInputOut+1))
		io.Copy(io.Discard, errPipe)
		ec <- res{b, e}
	}()
	stdout, err = io.ReadAll(io.LimitReader(out, maxRunInputOut+1))
	if err == nil && len(stdout) > maxRunInputOut {
		err = ErrOutputTooLarge
	}
	if err != nil {
		sess.Close()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, nil, -1, ctxErr
		}
		return nil, nil, -1, fmt.Errorf("sshterm: 读远端输出失败: %w", err)
	}
	io.Copy(io.Discard, out)
	er := <-ec
	inErr := <-wc
	werr := sess.Wait()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, nil, -1, ctxErr
	}
	if er.err != nil {
		return nil, nil, -1, fmt.Errorf("sshterm: 读远端 stderr 失败: %w", er.err)
	}
	if inErr != nil {
		// 请求可能没完整送达；不论远端退出码如何都按失败返回（附带远端 stderr 便于排查）。
		msg := er.b
		if len(msg) > 1024 {
			msg = msg[:1024]
		}
		return nil, nil, -1, fmt.Errorf("sshterm: 向远端写 stdin 失败（请求可能未完整送达）: %w; 远端 stderr: %s", inErr, msg)
	}
	if len(er.b) > maxRunInputOut {
		return nil, nil, -1, ErrOutputTooLarge
	}
	if werr != nil {
		var ee *ssh.ExitError
		if errors.As(werr, &ee) {
			return stdout, er.b, ee.ExitStatus(), nil
		}
		return nil, nil, -1, fmt.Errorf("sshterm: 远端命令失败: %w", werr)
	}
	return stdout, er.b, 0, nil
}

// Listen 让远端在 addr（形如 "127.0.0.1:0"）上监听，连进来的连接经这条 SSH 连接转回本地。
// 返回的 Listener.Addr() 是远端实际监听的地址。连接断开后 Accept 返回错误。连接已断时直接返回错误。
func (c *Client) Listen(addr string) (net.Listener, error) {
	if err := c.live(); err != nil {
		return nil, err
	}
	ln, err := c.client.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("sshterm: 远端监听 %s 失败: %w", addr, err)
	}
	return ln, nil
}

// DialContext 经这条 SSH 连接开 direct-tcpip 通道连到远端视角下的 addr（等同 ssh -L）。
// ctx 到期则放弃等待并返回 ctx 错误（后到的连接会被关掉）；连接已断时直接返回错误。
func (c *Client) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if err := c.live(); err != nil {
		return nil, err
	}
	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() {
		conn, err := c.client.Dial(network, addr)
		ch <- res{conn, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, fmt.Errorf("sshterm: 经 SSH 连 %s 失败: %w", addr, r.err)
		}
		return r.c, nil
	case <-ctx.Done():
		go func() {
			if r := <-ch; r.c != nil {
				r.c.Close()
			}
		}()
		return nil, ctx.Err()
	}
}

// Done 在连接断开（对端关、keepalive 失败）或 Close 后关闭。
func (c *Client) Done() <-chan struct{} { return c.done }

// Err 返回连接断开的原因（还连着时为 nil）。
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Close 关连接，上面所有会话随之结束；可重复调用。
func (c *Client) Close() error {
	c.fail(errors.New("sshterm: 连接已关闭"))
	return nil
}

// fail 记录断开原因并保证只收尾一次。
func (c *Client) fail(err error) {
	c.mu.Lock()
	if c.err == nil {
		c.err = err
	}
	c.mu.Unlock()
	c.closeOnce.Do(func() {
		c.client.Close()
		close(c.done)
	})
}

// live 检查连接是否还可用。
func (c *Client) live() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return fmt.Errorf("sshterm: %w", c.err)
	}
	return nil
}

// SFTP 在这条连接上开一个 SFTP 会话；用完调用方负责 Close。连接已断时报错。
func (c *Client) SFTP() (*sftp.Client, error) {
	if err := c.live(); err != nil {
		return nil, err
	}
	return sftp.NewClient(c.client)
}

// keepAlive 定期发全局请求，失败或超时说明隧道已经断了（TCP 可能还"连着"），关掉连接让上层察觉。
func (c *Client) keepAlive(interval, timeout time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	fc := failCounter{max: keepAliveMaxFails}
	for {
		select {
		case <-c.done:
			return
		case <-t.C:
			res := make(chan error, 1)
			go func() {
				_, _, err := c.client.SendRequest("keepalive@openssh.com", true, nil)
				res <- err
			}()
			select {
			case err := <-res:
				if fc.record(err == nil) {
					c.fail(fmt.Errorf("sshterm: keepalive 连续失败: %w", err))
					return
				}
			case <-time.After(timeout):
				c.fail(errors.New("sshterm: keepalive 无回应"))
				return
			}
		}
	}
}
