// Package sshterm 经任意 Dialer 建立 SSH 连接并开交互终端，
// 以及把终端会话桥接到 WebSocket（供网页终端使用）。
package sshterm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"
)

// Dialer 是能建 TCP 连接的东西；wgtunnel 隧道和 net.Dialer 都满足它。
type Dialer interface {
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
}

// Target 描述远端要连什么、跑什么。
type Target struct {
	Addr    string // host:port
	User    string
	Command string // 例如 "tmux new -A -s main"；为空时开登录 shell
}

// Auth 是认证配置；HostKeyCallback 必填，绝不默认放行。
type Auth struct {
	Signers         []ssh.Signer
	HostKeyCallback ssh.HostKeyCallback
}

// Session 是一个远端终端会话：读到的是终端输出（stdout+stderr），写入的是键盘输入。
type Session struct {
	client *ssh.Client
	sess   *ssh.Session
	in     io.WriteCloser // StdinPipe，必须在启动前取
	out    io.Reader      // StdoutPipe；PTY 下 stderr 也走这里

	mu     sync.Mutex
	closed bool
}

// Open 经 d 建 TCP 连接并完成 SSH 握手，然后开 PTY 会话。
// 握手期间 ctx 到期会关掉连接并返回 ctx 的错误。
func Open(ctx context.Context, d Dialer, t Target, a Auth, cols, rows int) (*Session, error) {
	if a.HostKeyCallback == nil {
		return nil, errors.New("sshterm: 必须提供 HostKeyCallback，不能默认放行主机密钥")
	}
	conn, err := d.DialContext(ctx, "tcp", t.Addr)
	if err != nil {
		return nil, fmt.Errorf("sshterm: 连接 %s 失败: %w", t.Addr, err)
	}
	cfg := &ssh.ClientConfig{
		User:            t.User,
		HostKeyCallback: a.HostKeyCallback,
	}
	if len(a.Signers) > 0 {
		cfg.Auth = append(cfg.Auth, ssh.PublicKeys(a.Signers...))
	}

	// 握手放在 goroutine 里，ctx 到期就关 conn 打断它。
	type hsResult struct {
		cc    ssh.Conn
		chans <-chan ssh.NewChannel
		reqs  <-chan *ssh.Request
		err   error
	}
	done := make(chan hsResult, 1)
	go func() {
		cc, chans, reqs, err := ssh.NewClientConn(conn, t.Addr, cfg)
		done <- hsResult{cc, chans, reqs, err}
	}()
	var hs hsResult
	select {
	case hs = <-done:
	case <-ctx.Done():
		conn.Close()
		<-done
		return nil, ctx.Err()
	}
	if hs.err != nil {
		conn.Close()
		return nil, fmt.Errorf("sshterm: SSH 握手失败: %w", hs.err)
	}
	client := ssh.NewClient(hs.cc, hs.chans, hs.reqs)
	// 开会话、申请 PTY、启动程序这几步对端也可能不回话：ctx 到期就关连接
	stop := context.AfterFunc(ctx, func() { client.Close() })
	defer stop()

	sess, err := client.NewSession()
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("sshterm: 开会话失败: %w", err)
	}
	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 115200,
		ssh.TTY_OP_OSPEED: 115200,
	}
	if err := sess.RequestPty("xterm-256color", rows, cols, modes); err != nil {
		client.Close()
		return nil, fmt.Errorf("sshterm: 申请 PTY 失败: %w", err)
	}
	// StdinPipe/StdoutPipe 必须在 Start/Shell 之前取。
	in, err := sess.StdinPipe()
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("sshterm: 取 stdin 失败: %w", err)
	}
	out, err := sess.StdoutPipe()
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("sshterm: 取 stdout 失败: %w", err)
	}
	if t.Command != "" {
		err = sess.Start(t.Command)
	} else {
		err = sess.Shell()
	}
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("sshterm: 启动远端程序失败: %w", err)
	}
	return &Session{client: client, sess: sess, in: in, out: out}, nil
}

// Read 读远端终端输出。
func (s *Session) Read(p []byte) (int, error) {
	return s.out.Read(p)
}

// Write 写键盘输入到远端；会话已关闭时报错。
func (s *Session) Write(p []byte) (int, error) {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return 0, errors.New("sshterm: 会话已关闭")
	}
	return s.in.Write(p)
}

// Resize 通知远端终端窗口大小变了。
func (s *Session) Resize(cols, rows int) error {
	if !validSize(cols, rows) {
		return fmt.Errorf("sshterm: 窗口大小不合理: %dx%d", cols, rows)
	}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return errors.New("sshterm: 会话已关闭")
	}
	return s.sess.WindowChange(rows, cols)
}

// Wait 等远端程序结束，返回退出码；正常结束（含非零退出码）时 err 为 nil。
func (s *Session) Wait() (int, error) {
	err := s.sess.Wait()
	if err == nil {
		return 0, nil
	}
	var ee *ssh.ExitError
	if errors.As(err, &ee) {
		if ee.Signal() != "" && ee.ExitStatus() == 0 {
			return 128 + signalNumber(ee.Signal()), nil // 和 shell 一样：被信号杀死 = 128+信号号
		}
		return ee.ExitStatus(), nil
	}
	return 0, err
}

// Close 关掉 SSH 连接；可重复调用，之后 Write 报错。
func (s *Session) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	s.sess.Close()
	return s.client.Close()
}

// BridgeOptions 是 WebSocket 桥的可选配置。
type BridgeOptions struct {
	// OriginPatterns 是额外允许的 Origin 主机名（同源默认允许，其他一律拒绝）。
	OriginPatterns []string
}

// Handler 把 HTTP 升级为 WebSocket，从查询参数 cols、rows 取终端初始大小，
// 调 open 建会话并双向转发：
//   - 浏览器→服务：二进制帧 = 键盘输入；文本帧 = JSON 控制消息（{"type":"resize",...}）
//   - 服务→浏览器：二进制帧 = 终端输出；远端结束时发 {"type":"exit","code":N} 后正常关闭
//   - open 失败：发 {"type":"error","message":...} 后关闭
//   - 浏览器断开：关掉 SSH 会话
func Handler(open func(r *http.Request, cols, rows int) (*Session, error), opts BridgeOptions) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cols := queryInt(r.URL.Query(), "cols", 80)
		rows := queryInt(r.URL.Query(), "rows", 24)

		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			OriginPatterns: opts.OriginPatterns,
		})
		if err != nil {
			return // Accept 已写好 HTTP 错误
		}
		defer ws.CloseNow()
		ws.SetReadLimit(1 << 20) // 默认 32KB，大段粘贴会被断开

		sess, err := open(r, cols, rows)
		if err != nil {
			writeJSON(ws, map[string]any{"type": "error", "message": err.Error()})
			ws.Close(websocket.StatusNormalClosure, "")
			return
		}
		defer sess.Close()

		ctx := context.Background()

		// 远端输出 → 浏览器；发退出帧前要等这里收干净，保证顺序。
		outDone := make(chan struct{})
		go func() {
			defer close(outDone)
			buf := make([]byte, 32*1024)
			for {
				n, rerr := sess.Read(buf)
				if n > 0 {
					if werr := ws.Write(ctx, websocket.MessageBinary, buf[:n]); werr != nil {
						return
					}
				}
				if rerr != nil {
					return
				}
			}
		}()

		// 远端程序结束 → 退出/错误帧，然后正常关闭。
		go func() {
			code, werr := sess.Wait()
			<-outDone
			if werr != nil {
				writeJSON(ws, map[string]any{"type": "error", "message": werr.Error()})
			} else {
				writeJSON(ws, map[string]any{"type": "exit", "code": code})
			}
			ws.Close(websocket.StatusNormalClosure, "")
		}()

		// 浏览器 → 远端；断开后 defer 里关掉 SSH 会话。
		for {
			typ, b, err := ws.Read(ctx)
			if err != nil {
				return
			}
			switch typ {
			case websocket.MessageBinary:
				if _, err := sess.Write(b); err != nil {
					return
				}
			case websocket.MessageText:
				var m struct {
					Type       string `json:"type"`
					Cols, Rows int
				}
				if json.Unmarshal(b, &m) == nil && m.Type == "resize" {
					sess.Resize(m.Cols, m.Rows)
				}
			}
		}
	})
}

// writeJSON 发一个文本帧，失败就不管（连接多半已经断了）。
func writeJSON(ws *websocket.Conn, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws.Write(ctx, websocket.MessageText, b)
}

// queryInt 取查询参数里的整数，缺失或非法时用 def。
func queryInt(q map[string][]string, name string, def int) int {
	vs := q[name]
	if len(vs) == 0 {
		return def
	}
	n, err := strconv.Atoi(vs[0])
	if err != nil || n <= 0 || n > maxSize {
		return def
	}
	return n
}

const maxSize = 1000

func validSize(cols, rows int) bool {
	return cols > 0 && rows > 0 && cols <= maxSize && rows <= maxSize
}

// signalNumber 把 SSH 协议里的信号名换成常见的信号号（POSIX 编号）
func signalNumber(name string) int {
	switch name {
	case "HUP":
		return 1
	case "INT":
		return 2
	case "QUIT":
		return 3
	case "ILL":
		return 4
	case "ABRT":
		return 6
	case "FPE":
		return 8
	case "KILL":
		return 9
	case "SEGV":
		return 11
	case "PIPE":
		return 13
	case "ALRM":
		return 14
	case "TERM":
		return 15
	}
	return 0
}
