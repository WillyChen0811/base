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

type Server struct {
	HostKey ssh.Signer
	mu      sync.Mutex
	cmds    []string
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
	s := &Server{HostKey: NewSigner()}
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
			for req := range creqs {
				switch req.Type {
				case "pty-req", "window-change":
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
					if !started {
						started = true
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
