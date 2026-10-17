// proxy.go：程序级接管的入口——回环上的 SOCKS5 代理口（无认证，只支持 CONNECT，地址类型 IPv4/域名/IPv6）。
// 程序照常写真实地址，收到 CONNECT 目的 D 后按各转发的 target 规则选最具体的一条（见 proxy_rule.go），
// 用它的当前线路拨到 D 本身，复用转发的登记、流量统计和切线路关连接；没命中任何规则走 proxy_default：
// direct（默认，计入「其他（直连）」）或某条转发的名字。域名不先解析，交给最终拨号的那条线路解析。
package wgtund

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// proxyStatus 是启动信息和 /status 里的 proxy 字段。
type proxyStatus struct {
	Addr  string `json:"addr"`
	Error string `json:"error"`
}

type proxySvc struct {
	laddr  string
	rules  []proxyRule
	dflt   *forwardSvc // 未命中规则时借用的转发（direct 时就是 direct 自己）
	direct *forwardSvc // 「其他（直连）」：只有一条直连线路、不监听；仅 proxy_default 为 direct 时有

	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	ln     net.Listener
	addr   string
	errMsg string
	closed bool
}

const directName = "其他（直连）"

func newProxy(listen, dflt string, forwards map[string]*forwardSvc) *proxySvc {
	ctx, cancel := context.WithCancel(context.Background())
	p := &proxySvc{ctx: ctx, cancel: cancel}
	host, port, _ := net.SplitHostPort(listen)
	if host != "127.0.0.1" {
		host = "127.0.0.1" // localhost 也固定绑回环 IPv4
	}
	p.laddr = net.JoinHostPort(host, port)
	for _, f := range forwards {
		for _, t := range f.target {
			r, _ := parseRule(t) // 配置校验过
			r.fwd = f
			p.rules = append(p.rules, r)
		}
	}
	if dflt == "" || dflt == "direct" {
		p.direct = newForward(forwardConf{Name: directName, Routes: []routeConf{{Label: "直连", Addr: "0.0.0.0:0"}}}, nil, nil)
		p.dflt = p.direct
	} else {
		p.dflt = forwards[dflt]
	}
	return p
}

// start 开监听；失败只记 errMsg 并每 forwardListenRetry 重试（与转发口一致），不影响 wgtund。
func (p *proxySvc) start() {
	if !p.tryListen() {
		go func() {
			t := time.NewTicker(forwardListenRetry)
			defer t.Stop()
			for {
				select {
				case <-t.C:
				case <-p.ctx.Done():
					return
				}
				if p.tryListen() {
					return
				}
			}
		}()
	}
}

func (p *proxySvc) tryListen() bool {
	ln, err := net.Listen("tcp", p.laddr)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		if ln != nil {
			ln.Close()
		}
		return true
	}
	if err != nil {
		p.errMsg = fmt.Sprintf("监听 %s 失败：%v", p.laddr, err)
		sysLog.logThrottled("listen:proxy", p.errMsg, "代理口 %s", p.errMsg)
		return false
	}
	p.ln, p.addr, p.errMsg = ln, ln.Addr().String(), ""
	sysLog.clearThrottle("listen:proxy")
	logf("代理口监听成功：%s（%d 条规则）", p.addr, len(p.rules))
	go p.acceptLoop(ln)
	return true
}

func (p *proxySvc) acceptLoop(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			p.mu.Lock()
			defer p.mu.Unlock()
			if !p.closed {
				p.errMsg = fmt.Sprintf("代理口监听异常退出：%v", err)
				logf("%s", p.errMsg)
			}
			return
		}
		go p.serve(c)
	}
}

func (p *proxySvc) status() proxyStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	return proxyStatus{p.addr, p.errMsg}
}

func (p *proxySvc) Close() error {
	p.cancel()
	if p.direct != nil {
		p.direct.Close()
	}
	p.mu.Lock()
	p.closed = true
	ln := p.ln
	p.mu.Unlock()
	if ln != nil {
		ln.Close()
	}
	return nil
}

// SOCKS5 应答码
const (
	repOK          = 0x00
	repFailure     = 0x01
	repNetUnreach  = 0x03
	repHostUnreach = 0x04
	repRefused     = 0x05
	repCmdNotSupp  = 0x07
	repAtypNotSupp = 0x08
)

// serve 处理一条代理连接：握手 → 找转发 → 当前线路拨号 → 应答 → 转交转发的 relay。
func (p *proxySvc) serve(c net.Conn) {
	c.SetDeadline(time.Now().Add(10 * time.Second)) // 只管握手阶段，拨号前清掉
	dest, rep := socksHandshake(c)
	if rep != repOK {
		if dest != "" || rep != repFailure {
			socksReply(c, rep)
		}
		c.Close()
		return
	}
	c.SetDeadline(time.Time{}) // 拨号有自己的 dialTimeout；慢线路上握手+拨号超过 10 秒也不该掐掉已拨通的连接
	f := p.pick(dest)
	up, gen, err := f.dialTo(dest)
	if err != nil {
		socksReply(c, dialRep(err))
		c.Close()
		return
	}
	if socksReply(c, repOK) != nil {
		up.Close()
		c.Close()
		return
	}
	f.relay(c, up, gen)
}

// dialRep 把拨号错误映射到 SOCKS5 应答码。
func dialRep(err error) byte {
	var ne net.Error
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return repRefused
	case errors.Is(err, syscall.ENETUNREACH):
		return repNetUnreach
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, context.DeadlineExceeded),
		errors.As(err, &ne) && ne.Timeout():
		return repHostUnreach
	}
	return repFailure
}

// socksHandshake 完成方法协商并读 CONNECT 请求，返回目的 主机:端口；失败返回应答码
// （dest 为空且 rep==repFailure 表示协议已乱，不必回应答）。
func socksHandshake(c net.Conn) (string, byte) {
	h := make([]byte, 2)
	if _, err := io.ReadFull(c, h); err != nil || h[0] != 5 {
		return "", repFailure
	}
	methods := make([]byte, h[1])
	if _, err := io.ReadFull(c, methods); err != nil {
		return "", repFailure
	}
	noAuth := false
	for _, m := range methods {
		noAuth = noAuth || m == 0
	}
	if !noAuth {
		c.Write([]byte{5, 0xff})
		return "", repFailure
	}
	if _, err := c.Write([]byte{5, 0}); err != nil {
		return "", repFailure
	}
	r := make([]byte, 4)
	if _, err := io.ReadFull(c, r); err != nil || r[0] != 5 {
		return "", repFailure
	}
	var host string
	switch r[3] {
	case 1, 4:
		ip := make([]byte, map[byte]int{1: 4, 4: 16}[r[3]])
		if _, err := io.ReadFull(c, ip); err != nil {
			return "", repFailure
		}
		host = net.IP(ip).String()
	case 3:
		l := make([]byte, 1)
		if _, err := io.ReadFull(c, l); err != nil {
			return "", repFailure
		}
		name := make([]byte, l[0])
		if _, err := io.ReadFull(c, name); err != nil {
			return "", repFailure
		}
		host = string(name)
	default:
		return "x", repAtypNotSupp
	}
	pb := make([]byte, 2)
	if _, err := io.ReadFull(c, pb); err != nil {
		return "", repFailure
	}
	dest := net.JoinHostPort(host, strconv.Itoa(int(pb[0])<<8|int(pb[1])))
	if r[1] != 1 {
		return dest, repCmdNotSupp
	}
	return dest, repOK
}

// socksReply 写应答（绑定地址固定填 0.0.0.0:0，客户端用不到）。
func socksReply(c net.Conn, rep byte) error {
	_, err := c.Write([]byte{5, rep, 0, 1, 0, 0, 0, 0, 0, 0})
	return err
}
