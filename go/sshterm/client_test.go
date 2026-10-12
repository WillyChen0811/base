package sshterm

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/WillyChen0811/base/go/internal/sshtest"
	"golang.org/x/crypto/ssh"
)

// ---------- 测试脚手架 ----------

// trackedConn 记录服务端这侧连接是否已 EOF（对端关掉后可据此验证"连接结束了"）。
type trackedConn struct {
	net.Conn
	eof atomic.Bool
}

func (c *trackedConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if errors.Is(err, io.EOF) {
		c.eof.Store(true)
	}
	return n, err
}

type trackedListener struct {
	net.Listener
	mu    sync.Mutex
	conns []*trackedConn
}

func (l *trackedListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	tc := &trackedConn{Conn: c}
	l.mu.Lock()
	l.conns = append(l.conns, tc)
	l.mu.Unlock()
	return tc, nil
}

func (l *trackedListener) allEOF() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.conns) == 0 {
		return false
	}
	for _, c := range l.conns {
		if !c.eof.Load() {
			return false
		}
	}
	return true
}

func (l *trackedListener) closeAll() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, c := range l.conns {
		c.Close()
	}
}

type cliEnv struct {
	srv   *sshtest.Server
	tln   *trackedListener
	addr  string
	ck    ssh.Signer
	block chan struct{} // "hang" 命令卡在这里，Cleanup 时放行
	once  sync.Once
}

func newCliEnv(t *testing.T) *cliEnv {
	t.Helper()
	ck := newSigner(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	e := &cliEnv{tln: &trackedListener{Listener: ln}, addr: ln.Addr().String(), ck: ck, block: make(chan struct{})}
	e.srv = sshtest.ServeWith(e.tln, ck.PublicKey(), sshtest.Options{
		Exec: func(cmd string) (string, string, int) {
			switch cmd {
			case "ok":
				return "hello\n", "", 0
			case "fail":
				return "", "boom\n", 3
			case "big":
				return strings.Repeat("x", maxRunStdout+16), "", 0
			case "hang":
				<-e.block
				return "", "", 0
			}
			return "ran:" + cmd + "\n", "warn:" + cmd + "\n", 0
		},
	})
	t.Cleanup(func() { e.once.Do(func() { close(e.block) }) })
	return e
}

func (e *cliEnv) dial(t *testing.T) *Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Dial(ctx, netDialer{}, Target{Addr: e.addr, User: "chenwy"},
		Auth{Signers: []ssh.Signer{e.ck}, HostKeyCallback: ssh.FixedHostKey(e.srv.HostKey.PublicKey())})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func (e *cliEnv) unblock() { e.once.Do(func() { close(e.block) }) }

// ---------- 多会话复用 ----------

func TestClientMultiSession(t *testing.T) {
	e := newCliEnv(t)
	c := e.dial(t)

	s1, err := c.OpenSession(context.Background(), "cat", 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := c.OpenSession(context.Background(), "", 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	if n := e.srv.Conns(); n != 1 {
		t.Errorf("两个会话应共用一条连接，服务器端连了 %d 条", n)
	}

	// 各自回显互不串
	io.WriteString(s1, "a\n")
	if got := readLine(t, s1); got != "echo:a" {
		t.Errorf("会话1 got %q", got)
	}
	io.WriteString(s2, "b\n")
	if got := readLine(t, s2); got != "echo:b" {
		t.Errorf("会话2 got %q", got)
	}

	// 关掉一个，另一个还能用；还能再开新的
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}
	io.WriteString(s2, "c\n")
	if got := readLine(t, s2); got != "echo:c" {
		t.Errorf("关掉会话1后会话2 got %q", got)
	}
	s3, err := c.OpenSession(context.Background(), "cat", 80, 24)
	if err != nil {
		t.Fatalf("关掉一个会话后应还能再开: %v", err)
	}
	io.WriteString(s3, "d\n")
	if got := readLine(t, s3); got != "echo:d" {
		t.Errorf("会话3 got %q", got)
		s3.Close()
	}
	if n := e.srv.Conns(); n != 1 {
		t.Errorf("始终应只有一条连接，得到 %d", n)
	}
}

// ---------- Run ----------

func TestRunSuccess(t *testing.T) {
	e := newCliEnv(t)
	c := e.dial(t)
	out, err := c.Run(context.Background(), "ok")
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "hello\n" {
		t.Errorf("got %q", out)
	}
	if n := e.srv.Conns(); n != 1 {
		t.Errorf("连接数 %d", n)
	}
}

func TestRunExitCode(t *testing.T) {
	e := newCliEnv(t)
	c := e.dial(t)
	_, err := c.Run(context.Background(), "fail")
	var ee *ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("应返回 *ExitError，得到 %v", err)
	}
	if ee.Code != 3 {
		t.Errorf("退出码 %d，应为 3", ee.Code)
	}
	if !strings.Contains(ee.Stderr, "boom") {
		t.Errorf("Stderr %q，应带 boom", ee.Stderr)
	}
	if c.Err() != nil {
		t.Errorf("非零退出码不该算连接断开: %v", c.Err())
	}
}

func TestRunStdoutLimit(t *testing.T) {
	e := newCliEnv(t)
	c := e.dial(t)
	if _, err := c.Run(context.Background(), "big"); err == nil {
		t.Fatal("超过 1MB 应报错")
	}
	// 超限后连接还能用
	out, err := c.Run(context.Background(), "ok")
	if err != nil {
		t.Fatalf("超限后还能跑: %v", err)
	}
	if string(out) != "hello\n" {
		t.Errorf("got %q", out)
	}
}

func TestRunContextTimeout(t *testing.T) {
	e := newCliEnv(t)
	c := e.dial(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := c.Run(ctx, "hang")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("应返回 ctx 超时错误，得到 %v", err)
	}
	if c.Err() != nil {
		t.Errorf("Run 超时不该关连接: %v", c.Err())
	}
	e.unblock()
	out, err := c.Run(context.Background(), "ok")
	if err != nil {
		t.Fatalf("超时后 Client 还能 Run: %v", err)
	}
	if string(out) != "hello\n" {
		t.Errorf("got %q", out)
	}
}

// ---------- Client.Close ----------

func TestClientClose(t *testing.T) {
	e := newCliEnv(t)
	c := e.dial(t)
	s, err := c.OpenSession(context.Background(), "cat", 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(s, "x\n")
	if got := readLine(t, s); got != "echo:x" {
		t.Errorf("got %q", got)
	}
	readErr := make(chan error, 1)
	go func() {
		buf := make([]byte, 64)
		_, err := s.Read(buf)
		readErr <- err
	}()
	waitErr := make(chan error, 1)
	go func() {
		_, err := s.Wait()
		waitErr <- err
	}()

	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("重复 Close 不应报错: %v", err)
	}
	select {
	case <-c.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Close 后 Done 应关闭")
	}
	if c.Err() == nil {
		t.Error("断开后 Err 应非空")
	}
	select {
	case err := <-readErr:
		if err == nil {
			t.Error("连接断开后 Read 应返回错误")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("连接断开后 Read 应返回")
	}
	select {
	case <-waitErr: // Wait 返回即可（连接断开时多半带错误）
	case <-time.After(5 * time.Second):
		t.Fatal("连接断开后 Wait 应返回")
	}
	if _, err := c.OpenSession(context.Background(), "cat", 80, 24); err == nil {
		t.Error("关闭后 OpenSession 应报错")
	}
	if _, err := c.Run(context.Background(), "ok"); err == nil {
		t.Error("关闭后 Run 应报错")
	}
}

// ---------- 对端断开 / keepalive ----------

func TestPeerGoneClosesDone(t *testing.T) {
	oldInt, oldTo := keepAliveInterval, keepAliveTimeout
	keepAliveInterval, keepAliveTimeout = 100*time.Millisecond, 300*time.Millisecond
	defer func() { keepAliveInterval, keepAliveTimeout = oldInt, oldTo }()

	e := newCliEnv(t)
	c := e.dial(t)
	eventually(t, "服务器握手完成", func() bool { return e.srv.Conns() == 1 })

	e.tln.Close()
	e.tln.closeAll()

	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("对端断开后 Done 应在合理时间内关闭")
	}
	if c.Err() == nil {
		t.Error("Err 应说明断开原因")
	}
}

// 对端"装死"（TCP 通但不回话）：keepalive 超时后连接应被关掉。
func TestKeepaliveTimeout(t *testing.T) {
	oldInt, oldTo := keepAliveInterval, keepAliveTimeout
	keepAliveInterval, keepAliveTimeout = 100*time.Millisecond, 300*time.Millisecond
	defer func() { keepAliveInterval, keepAliveTimeout = oldInt, oldTo }()

	e := newCliEnv(t)

	var mu sync.Mutex
	var cur *swallowConn
	d := swallowDialer{
		onDial: func(c *swallowConn) { mu.Lock(); cur = c; mu.Unlock() },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Dial(ctx, d, Target{Addr: e.addr, User: "chenwy"},
		Auth{Signers: []ssh.Signer{e.ck}, HostKeyCallback: ssh.FixedHostKey(e.srv.HostKey.PublicKey())})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	mu.Lock()
	sc := cur
	mu.Unlock()
	sc.swallow.Store(true) // 从这里起客户端写全被吞，服务器再也回不了话

	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("keepalive 超时后 Done 应关闭")
	}
	if !strings.Contains(c.Err().Error(), "keepalive") {
		t.Errorf("Err 应提到 keepalive: %v", c.Err())
	}
}

type swallowConn struct {
	net.Conn
	swallow atomic.Bool
}

func (c *swallowConn) Write(p []byte) (int, error) {
	if c.swallow.Load() {
		return len(p), nil
	}
	return c.Conn.Write(p)
}

type swallowDialer struct {
	onDial func(*swallowConn)
}

func (d swallowDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	var nd net.Dialer
	c, err := nd.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	sc := &swallowConn{Conn: c}
	d.onDial(sc)
	return sc, nil
}

// ---------- Open 兼容 ----------

// Open 的会话是专用的：Close 后底层连接也结束（服务器端读到 EOF）。
func TestOpenSessionCloseClosesConn(t *testing.T) {
	e := newCliEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := Open(ctx, netDialer{}, Target{Addr: e.addr, User: "chenwy", Command: "cat"},
		Auth{Signers: []ssh.Signer{e.ck}, HostKeyCallback: ssh.FixedHostKey(e.srv.HostKey.PublicKey())}, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "服务器握手完成", func() bool { return e.srv.Conns() == 1 })
	io.WriteString(s, "y\n")
	if got := readLine(t, s); got != "echo:y" {
		t.Errorf("got %q", got)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	eventually(t, "Open 的会话关闭后服务器端连接结束", func() bool { return e.tln.allEOF() })
}
