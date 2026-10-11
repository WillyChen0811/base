// Package socks 提供只监听回环地址的 SOCKS5 代理服务。
package socks

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	socks5 "github.com/things-go/go-socks5"
)

// Dialer 是能建连接的东西；*wgtunnel.Tunnel 满足它。
type Dialer interface {
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
}

// Options 是服务的可选配置。
type Options struct {
	// Username / Password 都为空表示不要认证；只设一个视为配置错误。
	Username, Password string

	// Logf 输出日志；nil 不输出。
	Logf func(format string, args ...any)
}

// Server 是一个 SOCKS5 代理服务器。
type Server struct {
	ln       net.Listener
	connMu   sync.Mutex
	conns    map[net.Conn]struct{}
	closed   bool // 由 connMu 保护
	logf     func(format string, args ...any)
	socksSrv *socks5.Server
	wg       sync.WaitGroup
}

// addrIsLoopback 检查 addr 的主机部分是否为回环 IP。
// addr 格式：host:port，或纯 host（port 回退到 "0"）。
func addrIsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// addr == "" 表示 ":" 即全部接口，非回环
		if addr == "" {
			return false
		}
		host = addr
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false // 主机名或异常地址不算回环
}

// Listen 启动 SOCKS5 服务。addr 的主机部分必须是回环 IP，否则报错。
func Listen(addr string, d Dialer, opts Options) (*Server, error) {
	if !addrIsLoopback(addr) {
		return nil, fmt.Errorf("socks: 必须监听回环地址，收到 %q", addr)
	}
	if hasCred(opts.Username) && !hasCred(opts.Password) ||
		!hasCred(opts.Username) && hasCred(opts.Password) {
		return nil, fmt.Errorf("socks: Username 和 Password 必须同时提供或都为空")
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}

	s := &Server{
		ln:       ln,
		conns:    make(map[net.Conn]struct{}),
		logf:     opts.Logf,
		socksSrv: buildSocks5(d, opts),
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.acceptLoop()
	}()

	return s, nil
}

// acceptLoop 接受连接并交给 SOCKS5 服务处理，同时跟踪每个连接。
func (s *Server) acceptLoop() {
	const minDelay, maxDelay = 5 * time.Millisecond, time.Second
	retryDelay := minDelay
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			if !s.isClosed() {
				if s.logf != nil {
					s.logf("accept 失败，%s 后重试: %v", retryDelay, err)
				}
				time.Sleep(retryDelay)
				retryDelay *= 2
				if retryDelay > maxDelay {
					retryDelay = maxDelay
				}
				continue
			}
			return
		}
		retryDelay = minDelay

		// 登记和 wg.Add 都在锁里做：Close 拿到锁之后，不会再有新连接漏网
		s.connMu.Lock()
		if s.closed {
			s.connMu.Unlock()
			conn.Close()
			continue
		}
		s.conns[conn] = struct{}{}
		s.wg.Add(1)
		s.connMu.Unlock()

		go func() {
			defer s.wg.Done()
			defer func() {
				s.connMu.Lock()
				delete(s.conns, conn)
				s.connMu.Unlock()
				conn.Close()
			}()
			if err := s.socksSrv.ServeConn(conn); err != nil && s.logf != nil {
				s.logf("ServeConn %s: %v", conn.RemoteAddr(), err)
			}
		}()
	}
}

func (s *Server) isClosed() bool {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	return s.closed
}

// buildSocks5 构建 go-socks5 服务配置。
//
// 关键技巧：对于客户端请求的域名，本机不能先解析。
// passThroughResolver 不调 DNS 解析，返回 nil IP 让 Dialer 取原始地址。
// WithDialAndRequest 在 handleConnect 的 dial 阶段被调用，它从 request.RawDestAddr
// 取原始地址（含 FQDN），传给底层 Dialer。这样域名原样传给隧道侧去解析。
func buildSocks5(d Dialer, opts Options) *socks5.Server {
	config := []socks5.Option{
		// 不调本机 DNS，域名原样透传
		socks5.WithResolver(passThroughResolver{}),
		// 只许 CONNECT：BIND / UDP ASSOCIATE 会绕过隧道走本机网络
		socks5.WithRule(&socks5.PermitCommand{EnableConnect: true, EnableBind: false, EnableAssociate: false}),
	}
	if opts.Logf != nil {
		config = append(config, socks5.WithLogger(loggerFunc{f: opts.Logf}))
	}
	if hasCred(opts.Username) && hasCred(opts.Password) {
		config = append(config, socks5.WithCredential(socks5.StaticCredentials{
			opts.Username: opts.Password,
		}))
	}
	// Dialer 回调：从 RawDestAddr 取原始地址（含 FQDN），传给底层 Dialer
	// handleConnect 会优先调用 dialWithRequest（如果有）
	config = append(config, socks5.WithDialAndRequest(
		func(ctx context.Context, network, addr string, req *socks5.Request) (net.Conn, error) {
			return d.DialContext(ctx, network, req.RawDestAddr.String())
		},
	))

	return socks5.NewServer(config...)
}

// --- pass-through resolver ---

// passThroughResolver 不调本机 DNS 解析域名。
// 返回 nil IP + 原 ctx，让 DialAndRequest 从 RawDestAddr 取原始地址。
type passThroughResolver struct{}

func (passThroughResolver) Resolve(ctx context.Context, name string) (context.Context, net.IP, error) {
	return ctx, nil, nil
}

// --- logger adapter ---

type loggerFunc struct {
	f func(format string, args ...any)
}

func (l loggerFunc) Errorf(format string, args ...any) {
	l.f(format, args...)
}

// hasCred 判断凭证是否已配置（非空表示已设）。
func hasCred(s string) bool { return s != "" }

// Addr 返回服务器监听地址。
func (s *Server) Addr() net.Addr {
	return s.ln.Addr()
}

// Close 停止服务并断开所有已建立的连接。可重复调用。
func (s *Server) Close() error {
	s.connMu.Lock()
	if s.closed {
		s.connMu.Unlock()
		return nil
	}
	s.closed = true
	s.ln.Close()
	for c := range s.conns {
		delete(s.conns, c)
		c.Close()
	}
	s.connMu.Unlock()

	s.wg.Wait()
	return nil
}
