package socks

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/proxy"
)

// fakeDialer 记录收到的目标地址，并把连接都转到本机的回显服务
type fakeDialer struct {
	mu     sync.Mutex
	got    []string
	target string
	fail   bool
}

func (d *fakeDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.got = append(d.got, network+" "+addr)
	fail := d.fail
	d.mu.Unlock()
	if fail {
		return nil, errors.New("隧道里连不上")
	}
	var nd net.Dialer
	return nd.DialContext(ctx, "tcp", d.target)
}

func (d *fakeDialer) addrs() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.got...)
}

func echo(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				r := bufio.NewReader(c)
				for {
					l, err := r.ReadString('\n')
					if err != nil {
						return
					}
					io.WriteString(c, "echo:"+l)
				}
			}()
		}
	}()
	return ln.Addr().String()
}

func start(t *testing.T, d Dialer, opts Options) *Server {
	t.Helper()
	s, err := Listen("127.0.0.1:0", d, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func via(t *testing.T, s *Server, auth *proxy.Auth, addr string) (net.Conn, error) {
	t.Helper()
	p, err := proxy.SOCKS5("tcp", s.Addr().String(), auth, &net.Dialer{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return p.Dial("tcp", addr)
}

func say(t *testing.T, c net.Conn, msg string) string {
	t.Helper()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(c, msg+"\n")
	l, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatalf("读回应失败: %v", err)
	}
	return strings.TrimSpace(l)
}

func TestConnectIP(t *testing.T) {
	d := &fakeDialer{target: echo(t)}
	s := start(t, d, Options{})
	c, err := via(t, s, nil, "10.1.2.3:80")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got := say(t, c, "hi"); got != "echo:hi" {
		t.Errorf("got %q", got)
	}
	if a := d.addrs(); len(a) != 1 || a[0] != "tcp 10.1.2.3:80" {
		t.Errorf("dialer 收到 %v", a)
	}
}

// 域名必须原样交给隧道去解析，本机不能先解析
func TestDomainNotResolvedLocally(t *testing.T) {
	d := &fakeDialer{target: echo(t)}
	s := start(t, d, Options{})
	c, err := via(t, s, nil, "svc.internal.invalid:8080")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	say(t, c, "x")
	if a := d.addrs(); len(a) != 1 || a[0] != "tcp svc.internal.invalid:8080" {
		t.Errorf("dialer 收到 %v，应原样收到域名", a)
	}
}

func TestOnlyLoopback(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:0", ":0", "[::]:0"} {
		if s, err := Listen(addr, &fakeDialer{}, Options{}); err == nil {
			s.Close()
			t.Errorf("Listen(%q) 应拒绝非回环地址", addr)
		}
	}
}

func TestAuth(t *testing.T) {
	d := &fakeDialer{target: echo(t)}
	s := start(t, d, Options{Username: "u", Password: "p"})
	if c, err := via(t, s, nil, "10.0.0.1:1"); err == nil {
		c.Close()
		t.Error("不带认证应被拒")
	}
	if c, err := via(t, s, &proxy.Auth{User: "u", Password: "wrong"}, "10.0.0.1:1"); err == nil {
		c.Close()
		t.Error("密码错应被拒")
	}
	c, err := via(t, s, &proxy.Auth{User: "u", Password: "p"}, "10.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got := say(t, c, "ok"); got != "echo:ok" {
		t.Errorf("got %q", got)
	}
}

// 隧道里连不上：客户端拿到失败，服务继续可用
func TestDialFailure(t *testing.T) {
	d := &fakeDialer{target: echo(t), fail: true}
	s := start(t, d, Options{})
	if c, err := via(t, s, nil, "10.0.0.9:22"); err == nil {
		c.Close()
		t.Fatal("应失败")
	}
	d.mu.Lock()
	d.fail = false
	d.mu.Unlock()
	c, err := via(t, s, nil, "10.0.0.9:22")
	if err != nil {
		t.Fatalf("失败一次后服务应仍可用: %v", err)
	}
	c.Close()
}

func TestConcurrent(t *testing.T) {
	d := &fakeDialer{target: echo(t)}
	s := start(t, d, Options{})
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p, _ := proxy.SOCKS5("tcp", s.Addr().String(), nil, &net.Dialer{Timeout: 5 * time.Second})
			c, err := p.Dial("tcp", fmt.Sprintf("10.0.0.%d:80", i+1))
			if err != nil {
				errs <- err
				return
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(5 * time.Second))
			msg := fmt.Sprintf("m%d", i)
			io.WriteString(c, msg+"\n")
			l, err := bufio.NewReader(c).ReadString('\n')
			if err != nil || strings.TrimSpace(l) != "echo:"+msg {
				errs <- fmt.Errorf("%d: %q %v", i, l, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// Close 后不再接新连接，已有连接也被断开；可重复调用
func TestClose(t *testing.T) {
	d := &fakeDialer{target: echo(t)}
	s, err := Listen("127.0.0.1:0", d, Options{})
	if err != nil {
		t.Fatal(err)
	}
	c, err := via(t, s, nil, "10.0.0.1:80")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	say(t, c, "before")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("重复 Close 不应报错: %v", err)
	}
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Error("Close 后已有连接应被断开")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Error("Close 后已有连接没有断开（读超时）")
	}
	if c2, err := via(t, s, nil, "10.0.0.1:80"); err == nil {
		c2.Close()
		t.Error("Close 后不应再接受连接")
	}
}

// 只许 CONNECT：UDP ASSOCIATE 会绕过隧道直接走本机网络，必须拒绝
func TestOnlyConnect(t *testing.T) {
	s := start(t, &fakeDialer{target: echo(t)}, Options{})
	for _, cmd := range []byte{2, 3} { // 2=BIND 3=UDP ASSOCIATE
		c, err := net.DialTimeout("tcp", s.Addr().String(), 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		c.SetDeadline(time.Now().Add(5 * time.Second))
		c.Write([]byte{5, 1, 0})
		greet := make([]byte, 2)
		if _, err := io.ReadFull(c, greet); err != nil || greet[1] != 0 {
			t.Fatalf("握手失败 %v %v", greet, err)
		}
		c.Write([]byte{5, cmd, 0, 1, 0, 0, 0, 0, 0, 0})
		rep := make([]byte, 2)
		if _, err := io.ReadFull(c, rep); err == nil && rep[1] == 0 {
			t.Errorf("命令 %d 应被拒绝，却回了成功", cmd)
		}
		c.Close()
	}
}

// 用户名和密码只设一个：配置错误，Listen 直接报错（否则空用户名就能登录）
func TestHalfCredentials(t *testing.T) {
	for _, o := range []Options{{Username: "u"}, {Password: "p"}} {
		if s, err := Listen("127.0.0.1:0", &fakeDialer{}, o); err == nil {
			s.Close()
			t.Errorf("%+v 应报错", o)
		}
	}
}
