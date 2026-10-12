package sshterm

import (
	"context"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/WillyChen0811/base/go/internal/sshtest"
	"golang.org/x/crypto/ssh"
)

// fwdClient 起一台测试服务器并连上，返回客户端和服务器。
func fwdClient(t *testing.T, forward bool) (*Client, *sshtest.Server) {
	t.Helper()
	ck := newSigner(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	srv := sshtest.ServeWith(ln, ck.PublicKey(), sshtest.Options{Forward: forward})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Dial(ctx, netDialer{}, Target{Addr: ln.Addr().String(), User: "chenwy"},
		Auth{Signers: []ssh.Signer{ck}, HostKeyCallback: ssh.FixedHostKey(srv.HostKey.PublicKey())})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c, srv
}

// accept 带超时地 Accept。
func accept(t *testing.T, ln net.Listener) net.Conn {
	t.Helper()
	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() { c, err := ln.Accept(); ch <- res{c, err} }()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatal(r.err)
		}
		return r.c
	case <-time.After(5 * time.Second):
		t.Fatal("Accept 超时")
		return nil
	}
}

// roundTrip 从 a 写 msg、从 b 读回。SSH 通道不支持 deadline，
// 所以读写都放进 goroutine，用超时兜底，回归时快速失败而不是挂住。
func roundTrip(t *testing.T, a, b net.Conn, msg string) {
	t.Helper()
	errc := make(chan error, 1)
	got := make(chan string, 1)
	go func() {
		if _, err := a.Write([]byte(msg)); err != nil {
			errc <- err
		}
	}()
	go func() {
		buf := make([]byte, len(msg))
		if _, err := io.ReadFull(b, buf); err != nil {
			errc <- err
			return
		}
		got <- string(buf)
	}()
	select {
	case g := <-got:
		if g != msg {
			t.Fatalf("收到 %q; 期望 %q", g, msg)
		}
	case err := <-errc:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatalf("roundTrip 超时: %q", msg)
	}
}

func TestListenForward(t *testing.T) {
	c, srv := fwdClient(t, true)
	ln, err := c.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if _, port, _ := net.SplitHostPort(addr); port == "0" || port == "" {
		t.Fatalf("端口无效: %s", addr)
	}
	if !slices.Contains(srv.Forwards(), addr) {
		t.Fatalf("Forwards()=%v 不含 %s", srv.Forwards(), addr)
	}

	d1, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer d1.Close()
	a1 := accept(t, ln)
	defer a1.Close()
	d2, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	a2 := accept(t, ln)
	defer a2.Close()

	// 两条连接互不串
	roundTrip(t, d1, a1, "hello-1")
	roundTrip(t, a1, d1, "reply-1")
	roundTrip(t, d2, a2, "hello-2")
	roundTrip(t, a2, d2, "reply-2")
}

func TestListenCloseStopsForward(t *testing.T) {
	c, srv := fwdClient(t, true)
	ln, err := c.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	eventually(t, "转发监听被撤销", func() bool { return !slices.Contains(srv.Forwards(), addr) })
	eventually(t, "Dial 失败", func() bool {
		cn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			cn.Close()
		}
		return err != nil
	})
}

func TestListenClientCloseStopsAll(t *testing.T) {
	c, srv := fwdClient(t, true)
	ln, err := c.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	c.Close()
	errc := make(chan error, 1)
	go func() { _, err := ln.Accept(); errc <- err }()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("Close 后 Accept 应返回错误")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Accept 未返回")
	}
	eventually(t, "服务器关掉全部监听", func() bool { return len(srv.Forwards()) == 0 })
	if _, err := c.Listen("127.0.0.1:0"); err == nil {
		t.Fatal("连接已断时 Listen 应失败")
	}
}

func TestListenRejected(t *testing.T) {
	c, srv := fwdClient(t, false)
	if _, err := c.Listen("127.0.0.1:0"); err == nil {
		t.Fatal("Forward=false 时 Listen 应失败")
	}
	if len(srv.Forwards()) != 0 {
		t.Fatal("不应有转发监听")
	}

	c2, srv2 := fwdClient(t, true)
	if _, err := c2.Listen("0.0.0.0:0"); err == nil {
		t.Fatal("0.0.0.0 应被拒")
	}
	if len(srv2.Forwards()) != 0 {
		t.Fatal("不应有转发监听")
	}
}

// 本地一端写完请求后 CloseWrite，另一端仍能把响应完整读回（半关闭）。
func TestListenHalfClose(t *testing.T) {
	c, _ := fwdClient(t, true)
	ln, err := c.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	d, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	a := accept(t, ln)
	defer a.Close()

	resp := strings.Repeat("response-", 1000)
	srvDone := make(chan error, 1)
	go func() {
		req, err := io.ReadAll(a) // 等到对端 CloseWrite 的 EOF
		if err != nil || string(req) != "request" {
			srvDone <- fmt.Errorf("请求 %q, %v", req, err)
			return
		}
		_, err = a.Write([]byte(resp))
		a.Close()
		srvDone <- err
	}()

	if _, err := d.Write([]byte("request")); err != nil {
		t.Fatal(err)
	}
	d.(*net.TCPConn).CloseWrite()
	d.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := io.ReadAll(d)
	if err != nil || string(got) != resp {
		t.Fatalf("响应长度 %d, %v; 期望 %d", len(got), err, len(resp))
	}
	select {
	case err := <-srvDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("服务端协程未结束")
	}
}
