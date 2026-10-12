package sshterm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// keepalive 参数；测试里调小。
var (
	keepAliveInterval = 15 * time.Second
	keepAliveTimeout  = 10 * time.Second
)

// Run 的输出上限。
const (
	maxRunStdout = 1 << 20 // 标准输出最多收 1MB
	maxRunStderr = 4 << 10 // 报错时带出的标准错误前 4KB
)

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
	go c.keepAlive()
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
		sess.Close()
		return nil, errors.New("sshterm: 标准输出超过 1MB")
	}
	stderr, _ := io.ReadAll(io.LimitReader(errPipe, maxRunStderr))

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

// keepAlive 定期发全局请求，失败或超时说明隧道已经断了（TCP 可能还"连着"），关掉连接让上层察觉。
func (c *Client) keepAlive() {
	t := time.NewTicker(keepAliveInterval)
	defer t.Stop()
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
				if err != nil {
					c.fail(fmt.Errorf("sshterm: keepalive 失败: %w", err))
					return
				}
			case <-time.After(keepAliveTimeout):
				c.fail(errors.New("sshterm: keepalive 无回应"))
				return
			}
		}
	}
}
