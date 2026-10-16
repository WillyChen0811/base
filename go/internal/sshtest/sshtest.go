// Package sshtest 是测试用的最小 SSH 服务器：只接受指定客户端公钥，
// 远端"程序"是回显（每行回 "echo:<行>"），记录收到的命令。
package sshtest

import (
	"bufio"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// Options 是服务器可选行为。
type Options struct {
	// Exec 不为 nil 时，没有申请 PTY 的 exec 请求交给它处理：返回标准输出、标准错误和退出码。
	// 申请了 PTY 的 exec/shell 仍走回显（现有行为）。
	Exec func(cmd string) (stdout, stderr string, code int)
	// ExecStdin 不为 nil 时优先于 Exec：读完客户端的标准输入（直到 EOF）再交给它处理。
	ExecStdin func(cmd string, stdin []byte) (stdout, stderr string, code int)
	// Forward 为 true 时支持远端端口转发（tcpip-forward）；默认拒绝。
	Forward bool
	// SFTPRoot 非空时，session 通道上的 "subsystem" 请求且名字是 "sftp" 时回 true，
	// 并用 sftp.NewServer 服务（Serve 结束后关通道）。为空时 subsystem 请求照旧回 false。
	SFTPRoot string
}

type Server struct {
	HostKey ssh.Signer
	opts    Options
	mu      sync.Mutex
	cmds    []string
	conns   int
	live    map[net.Conn]struct{}
	fwds    map[net.Listener]struct{} // 活着的转发监听
}

func NewSigner() ssh.Signer {
	s, _ := NewKey()
	return s
}

// NewKey 生成 ed25519 密钥，同时返回私钥原文（测试里要写成私钥文件）
func NewKey() (ssh.Signer, ed25519.PrivateKey) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		panic(err)
	}
	return s, priv
}

// Serve 在 ln 上接受连接直到 ln 关闭
func Serve(ln net.Listener, clientKey ssh.PublicKey) *Server {
	return ServeWith(ln, clientKey, Options{})
}

// ServeWith 同 Serve，但可配置 exec 行为。
func ServeWith(ln net.Listener, clientKey ssh.PublicKey, opts Options) *Server {
	s := &Server{HostKey: NewSigner(), opts: opts, live: map[net.Conn]struct{}{}, fwds: map[net.Listener]struct{}{}}
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
			if string(k.Marshal()) == string(clientKey.Marshal()) {
				return nil, nil
			}
			return nil, errors.New("拒绝")
		},
	}
	cfg.AddHostKey(s.HostKey)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c, cfg)
		}
	}()
	return s
}

// Conns 返回累计接受成功（握手完成）的 SSH 连接数，用来验证连接复用。
func (s *Server) Conns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conns
}

// CloseConns 从服务器端关掉当前所有已接受的连接（测试模拟断线用）。
func (s *Server) CloseConns() {
	s.mu.Lock()
	conns := make([]net.Conn, 0, len(s.live))
	for c := range s.live {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		c.Close()
	}
}

func (s *Server) Commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.cmds...)
}

// Forwards 返回当前活着的转发监听地址（"127.0.0.1:端口"）。
func (s *Server) Forwards() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for ln := range s.fwds {
		out = append(out, ln.Addr().String())
	}
	return out
}

// forwardReq 是 tcpip-forward / cancel-tcpip-forward 的负载。
type forwardReq struct {
	Addr string
	Port uint32
}

// forwardedPayload 是 forwarded-tcpip 通道的负载。
type forwardedPayload struct {
	Addr       string
	Port       uint32
	OriginAddr string
	OriginPort uint32
}

// globalReqs 处理一条连接的全局请求；连接结束时关掉它开的全部转发监听。
func (s *Server) globalReqs(sc *ssh.ServerConn, reqs <-chan *ssh.Request) {
	lns := map[uint32]net.Listener{} // 按实际端口索引，只在本 goroutine 里访问
	defer func() {
		for _, ln := range lns {
			s.closeFwd(ln)
		}
	}()
	for req := range reqs {
		ok := false
		var reply []byte
		switch {
		case req.Type == "tcpip-forward" && s.opts.Forward:
			var f forwardReq
			if ssh.Unmarshal(req.Payload, &f) != nil || (f.Addr != "127.0.0.1" && f.Addr != "localhost") {
				break
			}
			ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(f.Port))))
			if err != nil {
				break
			}
			port := uint32(ln.Addr().(*net.TCPAddr).Port)
			lns[port] = ln
			s.mu.Lock()
			s.fwds[ln] = struct{}{}
			s.mu.Unlock()
			go s.acceptFwd(sc, ln, f.Addr, port)
			ok, reply = true, ssh.Marshal(struct{ Port uint32 }{port})
		case req.Type == "cancel-tcpip-forward" && s.opts.Forward:
			var f forwardReq
			if ssh.Unmarshal(req.Payload, &f) != nil {
				break
			}
			if ln, found := lns[f.Port]; found {
				delete(lns, f.Port)
				s.closeFwd(ln)
				ok = true
			}
		}
		if req.WantReply {
			req.Reply(ok, reply)
		}
	}
}

func (s *Server) closeFwd(ln net.Listener) {
	ln.Close()
	s.mu.Lock()
	delete(s.fwds, ln)
	s.mu.Unlock()
}

// acceptFwd 把进入监听的连接经 forwarded-tcpip 通道转给客户端。
func (s *Server) acceptFwd(sc *ssh.ServerConn, ln net.Listener, addr string, port uint32) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			ra := c.RemoteAddr().(*net.TCPAddr)
			ch, reqs, err := sc.OpenChannel("forwarded-tcpip", ssh.Marshal(forwardedPayload{
				Addr: addr, Port: port, OriginAddr: ra.IP.String(), OriginPort: uint32(ra.Port),
			}))
			if err != nil {
				c.Close()
				return
			}
			go ssh.DiscardRequests(reqs)
			pipe(c, ch)
		}()
	}
}

// pipe 双向拷贝，半关闭语义：每个方向读到 EOF 就对另一端 CloseWrite，
// 两个方向都结束后才关两边，避免截断另一方向在途的数据。
func pipe(a io.ReadWriteCloser, b io.ReadWriteCloser) {
	done := make(chan struct{}, 2)
	cp := func(dst, src io.ReadWriteCloser) {
		io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		} else {
			dst.Close()
		}
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	<-done
	a.Close()
	b.Close()
}

func (s *Server) serve(c net.Conn, cfg *ssh.ServerConfig) {
	sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		c.Close()
		return
	}
	defer sc.Close()
	s.mu.Lock()
	s.conns++
	s.live[c] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.live, c)
		s.mu.Unlock()
	}()
	go s.globalReqs(sc, reqs)
	for nc := range chans {
		if nc.ChannelType() != "session" {
			nc.Reject(ssh.UnknownChannelType, "")
			continue
		}
		ch, creqs, err := nc.Accept()
		if err != nil {
			continue
		}
		go func() {
			started := false
			pty := false
			for req := range creqs {
				switch req.Type {
				case "pty-req":
					pty = true
					if req.WantReply {
						req.Reply(true, nil)
					}
				case "window-change":
					if req.WantReply {
						req.Reply(true, nil)
					}
				case "exec", "shell":
					var e struct{ Command string }
					if req.Type == "exec" {
						ssh.Unmarshal(req.Payload, &e)
					}
					s.mu.Lock()
					s.cmds = append(s.cmds, req.Type+":"+e.Command)
					s.mu.Unlock()
					req.Reply(true, nil)
					if started {
						continue
					}
					started = true
					if req.Type == "exec" && !pty && s.opts.ExecStdin != nil {
						cmd, fn := e.Command, s.opts.ExecStdin
						go func() {
							in, _ := io.ReadAll(ch)
							exec(ch, func(string) (string, string, int) { return fn(cmd, in) }, cmd)
						}()
					} else if req.Type == "exec" && !pty && s.opts.Exec != nil {
						go exec(ch, s.opts.Exec, e.Command)
					} else {
						go echo(ch)
					}
				case "subsystem":
					var e struct{ Name string }
					ssh.Unmarshal(req.Payload, &e)
					if e.Name == "sftp" && s.opts.SFTPRoot != "" {
						req.Reply(true, nil)
						go func() {
							srv, err := sftp.NewServer(ch, sftp.WithServerWorkingDirectory(s.opts.SFTPRoot))
							if err == nil {
								srv.Serve()
							}
							ch.Close()
						}()
					} else if req.WantReply {
						req.Reply(false, nil)
					}
				default:
					if req.WantReply {
						req.Reply(false, nil)
					}
				}
			}
		}()
	}
}

// exec 处理无 PTY 的 exec：标准输出/标准错误分开写回，发 exit-status 后关通道。
func exec(ch ssh.Channel, fn func(string) (string, string, int), cmd string) {
	stdout, stderr, code := fn(cmd)
	if stdout != "" {
		io.WriteString(ch, stdout)
	}
	if stderr != "" {
		io.WriteString(ch.Stderr(), stderr)
	}
	ch.SendRequest("exit-status", false, ssh.Marshal(struct{ C uint32 }{uint32(code)}))
	ch.Close()
}

func echo(ch ssh.Channel) {
	r := bufio.NewReader(ch)
	for {
		l, err := r.ReadString('\n')
		if err != nil || strings.TrimSpace(l) == "quit" {
			ch.SendRequest("exit-status", false, ssh.Marshal(struct{ C uint32 }{0}))
			ch.Close()
			return
		}
		io.WriteString(ch, "echo:"+l)
	}
}
