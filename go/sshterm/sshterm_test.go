package sshterm

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"
)

// ---------- 测试用 SSH 服务器 ----------

type ptyInfo struct {
	Term       string
	Cols, Rows uint32
}

type fakeServer struct {
	addr    string
	hostKey ssh.Signer
	mu      sync.Mutex
	cmds    []string
	ptys    []ptyInfo
	sizes   [][2]uint32
	conns   int
}

func (f *fakeServer) snapshot() (cmds []string, ptys []ptyInfo, sizes [][2]uint32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append(cmds, f.cmds...), append(ptys, f.ptys...), append(sizes, f.sizes...)
}

func newSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// startServer 起一个只接受 clientKey 的 SSH 服务器。
// 远端"命令"：exit3 直接以退出码 3 结束；其他命令和 shell 都是回显程序（每行回 "echo:<行>"），输入结束时退出码 0。
func startServer(t *testing.T, clientKey ssh.PublicKey) *fakeServer {
	t.Helper()
	f := &fakeServer{hostKey: newSigner(t)}
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
			if string(k.Marshal()) == string(clientKey.Marshal()) {
				return nil, nil
			}
			return nil, errors.New("拒绝")
		},
	}
	cfg.AddHostKey(f.hostKey)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	f.addr = ln.Addr().String()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c, cfg)
		}
	}()
	return f
}

func (f *fakeServer) serve(c net.Conn, cfg *ssh.ServerConfig) {
	sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		c.Close()
		return
	}
	defer sc.Close()
	f.mu.Lock()
	f.conns++
	f.mu.Unlock()
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "session" {
			nc.Reject(ssh.UnknownChannelType, "")
			continue
		}
		ch, creqs, err := nc.Accept()
		if err != nil {
			continue
		}
		go f.session(ch, creqs)
	}
}

func (f *fakeServer) session(ch ssh.Channel, reqs <-chan *ssh.Request) {
	started := false
	for req := range reqs {
		switch req.Type {
		case "pty-req":
			var p struct {
				Term             string
				Cols, Rows, W, H uint32
				Modes            string
			}
			ssh.Unmarshal(req.Payload, &p)
			f.mu.Lock()
			f.ptys = append(f.ptys, ptyInfo{p.Term, p.Cols, p.Rows})
			f.mu.Unlock()
			req.Reply(true, nil)
		case "window-change":
			var w struct{ Cols, Rows, W, H uint32 }
			ssh.Unmarshal(req.Payload, &w)
			f.mu.Lock()
			f.sizes = append(f.sizes, [2]uint32{w.Cols, w.Rows})
			f.mu.Unlock()
		case "exec", "shell":
			cmd := ""
			if req.Type == "exec" {
				var e struct{ Command string }
				ssh.Unmarshal(req.Payload, &e)
				cmd = e.Command
			}
			f.mu.Lock()
			f.cmds = append(f.cmds, req.Type+":"+cmd)
			f.mu.Unlock()
			req.Reply(true, nil)
			if !started {
				started = true
				go run(ch, cmd)
			}
		default:
			if req.WantReply {
				req.Reply(false, nil)
			}
		}
	}
}

func run(ch ssh.Channel, cmd string) {
	exit := func(code uint32) {
		ch.SendRequest("exit-status", false, ssh.Marshal(struct{ C uint32 }{code}))
		ch.Close()
	}
	if cmd == "exit3" {
		exit(3)
		return
	}
	r := bufio.NewReader(ch)
	for {
		l, err := r.ReadString('\n')
		if err != nil {
			exit(0)
			return
		}
		if strings.TrimSpace(l) == "quit" {
			exit(0)
			return
		}
		io.WriteString(ch, "echo:"+l)
	}
}

// ---------- 帮助函数 ----------

type netDialer struct{}

func (netDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, network, addr)
}

type env struct {
	srv    *fakeServer
	client ssh.Signer
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ck := newSigner(t)
	return &env{srv: startServer(t, ck.PublicKey()), client: ck}
}

func (e *env) open(t *testing.T, cmd string) (*Session, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return Open(ctx, netDialer{}, Target{Addr: e.srv.addr, User: "chenwy", Command: cmd},
		Auth{Signers: []ssh.Signer{e.client}, HostKeyCallback: ssh.FixedHostKey(e.srv.hostKey.PublicKey())}, 120, 40)
}

func readLine(t *testing.T, r io.Reader) string {
	t.Helper()
	done := make(chan string, 1)
	go func() {
		l, _ := bufio.NewReader(r).ReadString('\n')
		done <- strings.TrimSpace(l)
	}()
	select {
	case l := <-done:
		return l
	case <-time.After(10 * time.Second):
		t.Fatal("读超时")
		return ""
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等不到：%s", what)
}

// ---------- 会话 ----------

func TestOpenRunsCommandWithPTY(t *testing.T) {
	e := newEnv(t)
	s, err := e.open(t, "tmux new -A -s main")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	io.WriteString(s, "hello\n")
	if got := readLine(t, s); got != "echo:hello" {
		t.Errorf("got %q", got)
	}
	cmds, ptys, _ := e.srv.snapshot()
	if len(cmds) != 1 || cmds[0] != "exec:tmux new -A -s main" {
		t.Errorf("远端收到的命令 %v", cmds)
	}
	if len(ptys) != 1 || ptys[0] != (ptyInfo{"xterm-256color", 120, 40}) {
		t.Errorf("pty 请求 %+v", ptys)
	}
}

func TestEmptyCommandStartsShell(t *testing.T) {
	e := newEnv(t)
	s, err := e.open(t, "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	io.WriteString(s, "x\n")
	readLine(t, s)
	if cmds, _, _ := e.srv.snapshot(); len(cmds) != 1 || cmds[0] != "shell:" {
		t.Errorf("命令为空时应开登录 shell，远端收到 %v", cmds)
	}
}

func TestResize(t *testing.T) {
	e := newEnv(t)
	s, err := e.open(t, "cat")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Resize(200, 50); err != nil {
		t.Fatal(err)
	}
	eventually(t, "远端收到 200x50", func() bool {
		_, _, sz := e.srv.snapshot()
		return len(sz) > 0 && sz[len(sz)-1] == [2]uint32{200, 50}
	})
}

func TestExitCode(t *testing.T) {
	e := newEnv(t)
	s, err := e.open(t, "exit3")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	code, err := s.Wait()
	if err != nil {
		t.Fatalf("Wait 出错: %v", err)
	}
	if code != 3 {
		t.Errorf("退出码 %d，应为 3", code)
	}
}

func TestWrongHostKeyRejected(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	other := newSigner(t)
	_, err := Open(ctx, netDialer{}, Target{Addr: e.srv.addr, User: "chenwy", Command: "cat"},
		Auth{Signers: []ssh.Signer{e.client}, HostKeyCallback: ssh.FixedHostKey(other.PublicKey())}, 80, 24)
	if err == nil {
		t.Fatal("主机密钥不符必须拒绝连接")
	}
}

func TestMissingHostKeyCallback(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := Open(ctx, netDialer{}, Target{Addr: e.srv.addr, User: "chenwy"}, Auth{Signers: []ssh.Signer{e.client}}, 80, 24)
	if err == nil {
		t.Fatal("没给主机密钥校验时必须报错，不能默认放行")
	}
}

func TestWrongClientKeyRejected(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := Open(ctx, netDialer{}, Target{Addr: e.srv.addr, User: "chenwy"},
		Auth{Signers: []ssh.Signer{newSigner(t)}, HostKeyCallback: ssh.FixedHostKey(e.srv.hostKey.PublicKey())}, 80, 24)
	if err == nil {
		t.Fatal("客户端密钥不对应失败")
	}
}

// 连接卡住（对方不说话）时，Open 要在 ctx 到期时返回
func TestOpenHonorsContext(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close() // 接受但不说话
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	_, err = Open(ctx, netDialer{}, Target{Addr: ln.Addr().String(), User: "u"},
		Auth{Signers: []ssh.Signer{newSigner(t)}, HostKeyCallback: ssh.InsecureIgnoreHostKey()}, 80, 24)
	if err == nil {
		t.Fatal("应失败")
	}
	if time.Since(start) > 4*time.Second {
		t.Errorf("等了 %v", time.Since(start))
	}
}

func TestCloseIdempotent(t *testing.T) {
	e := newEnv(t)
	s, err := e.open(t, "cat")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("重复 Close 不应报错: %v", err)
	}
	if _, err := s.Write([]byte("x\n")); err == nil {
		t.Error("关闭后写应失败")
	}
}

// ---------- WebSocket 桥 ----------

func bridgeServer(t *testing.T, e *env, cmd string, opts BridgeOptions) *httptest.Server {
	t.Helper()
	h := Handler(func(r *http.Request, cols, rows int) (*Session, error) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		return Open(ctx, netDialer{}, Target{Addr: e.srv.addr, User: "chenwy", Command: cmd},
			Auth{Signers: []ssh.Signer{e.client}, HostKeyCallback: ssh.FixedHostKey(e.srv.hostKey.PublicKey())}, cols, rows)
	}, opts)
	hs := httptest.NewServer(h)
	t.Cleanup(hs.Close)
	return hs
}

func wsDial(t *testing.T, hs *httptest.Server, query string, origin string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	hdr := http.Header{}
	if origin != "" {
		hdr.Set("Origin", origin)
	}
	return websocket.Dial(ctx, "ws"+strings.TrimPrefix(hs.URL, "http")+"/?"+query, &websocket.DialOptions{HTTPHeader: hdr})
}

type ctrl struct {
	Type       string `json:"type"`
	Code       int    `json:"code"`
	Message    string `json:"message"`
	Cols, Rows int
}

// 读到下一条二进制帧，跳过其他
func readBinary(t *testing.T, c *websocket.Conn) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var buf strings.Builder
	for !strings.Contains(buf.String(), "\n") {
		typ, b, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("读帧失败: %v (已读 %q)", err, buf.String())
		}
		if typ == websocket.MessageBinary {
			buf.Write(b)
		}
	}
	return strings.TrimSpace(buf.String())
}

func TestBridgeEchoAndResize(t *testing.T) {
	e := newEnv(t)
	hs := bridgeServer(t, e, "tmux new -A -s main", BridgeOptions{})
	c, _, err := wsDial(t, hs, "cols=100&rows=30", "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	ctx := context.Background()
	c.Write(ctx, websocket.MessageBinary, []byte("hi\n"))
	if got := readBinary(t, c); got != "echo:hi" {
		t.Errorf("got %q", got)
	}
	if _, ptys, _ := e.srv.snapshot(); len(ptys) != 1 || ptys[0].Cols != 100 || ptys[0].Rows != 30 {
		t.Errorf("初始大小应取自 cols/rows 参数: %+v", ptys)
	}
	b, _ := json.Marshal(map[string]any{"type": "resize", "cols": 160, "rows": 48})
	c.Write(ctx, websocket.MessageText, b)
	eventually(t, "远端收到 160x48", func() bool {
		_, _, sz := e.srv.snapshot()
		return len(sz) > 0 && sz[len(sz)-1] == [2]uint32{160, 48}
	})
}

// 远端程序退出：先收到 {"type":"exit","code":N} 文本帧，然后连接正常关闭
func TestBridgeExit(t *testing.T) {
	e := newEnv(t)
	hs := bridgeServer(t, e, "exit3", BridgeOptions{})
	c, _, err := wsDial(t, hs, "cols=80&rows=24", "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var got *ctrl
	for got == nil {
		typ, b, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("没收到 exit 消息就断了: %v", err)
		}
		if typ == websocket.MessageText {
			var m ctrl
			if json.Unmarshal(b, &m) == nil && m.Type == "exit" {
				got = &m
			}
		}
	}
	if got.Code != 3 {
		t.Errorf("exit code = %d", got.Code)
	}
	_, _, err = c.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusNormalClosure {
		t.Errorf("之后应正常关闭，得到 %v", err)
	}
}

// 连不上远端：收到 {"type":"error"} 文本帧
func TestBridgeOpenError(t *testing.T) {
	h := Handler(func(r *http.Request, cols, rows int) (*Session, error) {
		return nil, fmt.Errorf("隧道没连上")
	}, BridgeOptions{})
	hs := httptest.NewServer(h)
	defer hs.Close()
	c, _, err := wsDial(t, hs, "cols=80&rows=24", "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	typ, b, err := c.Read(ctx)
	if err != nil || typ != websocket.MessageText {
		t.Fatalf("应收到文本帧: %v", err)
	}
	var m ctrl
	json.Unmarshal(b, &m)
	if m.Type != "error" || !strings.Contains(m.Message, "隧道没连上") {
		t.Errorf("got %s", b)
	}
}

// 跨站页面不能连本机终端；同源和白名单里的来源可以
func TestBridgeOrigin(t *testing.T) {
	e := newEnv(t)
	hs := bridgeServer(t, e, "cat", BridgeOptions{OriginPatterns: []string{"xxyi.local"}})
	if c, _, err := wsDial(t, hs, "cols=80&rows=24", "http://evil.example"); err == nil {
		c.CloseNow()
		t.Error("外站 Origin 应被拒")
	}
	if c, _, err := wsDial(t, hs, "cols=80&rows=24", "http://xxyi.local"); err != nil {
		t.Errorf("白名单 Origin 应放行: %v", err)
	} else {
		c.CloseNow()
	}
	if c, _, err := wsDial(t, hs, "cols=80&rows=24", hs.URL); err != nil {
		t.Errorf("同源应放行: %v", err)
	} else {
		c.CloseNow()
	}
}

// 网页关掉（WebSocket 断开）后，SSH 连接也要关掉，不留在远端
func TestBridgeClientGoneClosesSSH(t *testing.T) {
	e := newEnv(t)
	var mu sync.Mutex
	var sess *Session
	h := Handler(func(r *http.Request, cols, rows int) (*Session, error) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		s, err := Open(ctx, netDialer{}, Target{Addr: e.srv.addr, User: "chenwy", Command: "cat"},
			Auth{Signers: []ssh.Signer{e.client}, HostKeyCallback: ssh.FixedHostKey(e.srv.hostKey.PublicKey())}, cols, rows)
		mu.Lock()
		sess = s
		mu.Unlock()
		return s, err
	}, BridgeOptions{})
	hs := httptest.NewServer(h)
	defer hs.Close()
	c, _, err := wsDial(t, hs, "cols=80&rows=24", "")
	if err != nil {
		t.Fatal(err)
	}
	c.Write(context.Background(), websocket.MessageBinary, []byte("a\n"))
	readBinary(t, c)
	c.CloseNow()
	eventually(t, "SSH 会话随网页断开而关闭", func() bool {
		mu.Lock()
		s := sess
		mu.Unlock()
		if s == nil {
			return false
		}
		_, err := s.Write([]byte("x\n"))
		return err != nil
	})
}
