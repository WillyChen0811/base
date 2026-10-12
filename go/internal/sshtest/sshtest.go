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
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
)

// Options 是服务器可选行为。
type Options struct {
	// Exec 不为 nil 时，没有申请 PTY 的 exec 请求交给它处理：返回标准输出、标准错误和退出码。
	// 申请了 PTY 的 exec/shell 仍走回显（现有行为）。
	Exec func(cmd string) (stdout, stderr string, code int)
}

type Server struct {
	HostKey ssh.Signer
	opts    Options
	mu      sync.Mutex
	cmds    []string
	conns   int
	live    map[net.Conn]struct{}
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
	s := &Server{HostKey: NewSigner(), opts: opts, live: map[net.Conn]struct{}{}}
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
					if req.Type == "exec" && !pty && s.opts.Exec != nil {
						go exec(ch, s.opts.Exec, e.Command)
					} else {
						go echo(ch)
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
