// proxy_udp.go：代理口的 UDP（SOCKS5 UDP ASSOCIATE）——每条控制连接开一个回环 UDP 中继口，
// 客户端数据报带标准 SOCKS5 UDP 头（RSV、FRAG=0、ATYP、目的、端口），按 target 规则选转发后用它的当前线路发出。
// 线路怎么出去：隧道由隧道的 netstack 发（域名走隧道 DNS，没配则失败，由 dnsGuard 把关）；直连本机发；
// 经 SSH 的线路带不了 UDP，丢包并把原因写进该转发的 error。回包原样补上客户端当初的头发回去。
// 出站套接字按「客户端源地址+目的」复用，空闲 udpIdle 回收；FRAG≠0 直接丢；控制连接断开即关中继口和全部出站套接字。
package wgtund

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const cmdUDPAssociate = 3

// udpIdle 是出站套接字的空闲回收时间（测试里改小）。
var udpIdle = 60 * time.Second

// udpSock 是一条出站 UDP 套接字：对应一个客户端源地址到一个目的。
type udpSock struct {
	conn net.Conn
	hdr  []byte // 客户端请求里的 UDP 头（含目的），回包原样前置
	gen  int    // 建立时所属线路代际，线路切换后作废
	f    *forwardSvc
	last atomic.Int64 // 最近一次收发的时间（UnixNano）
}

type udpKey struct{ client, dest string }

// udpAssoc 是一条 UDP ASSOCIATE：一个中继口加它名下的出站套接字。
type udpAssoc struct {
	p     *proxySvc
	relay *net.UDPConn
	mu    sync.Mutex
	socks map[udpKey]*udpSock
	idle  time.Duration // 建立时取 udpIdle，之后不再读包级变量
}

// serveUDP 处理 UDP ASSOCIATE：开中继口并应答，之后阻塞到控制连接断开（或代理口关闭）再整体回收。
func (p *proxySvc) serveUDP(c net.Conn) {
	relay, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		socksReply(c, repFailure)
		c.Close()
		return
	}
	ra := relay.LocalAddr().(*net.UDPAddr)
	rep := []byte{5, repOK, 0, 1, 127, 0, 0, 1, byte(ra.Port >> 8), byte(ra.Port)}
	if _, err := c.Write(rep); err != nil {
		relay.Close()
		c.Close()
		return
	}
	a := &udpAssoc{p: p, relay: relay, socks: map[udpKey]*udpSock{}, idle: udpIdle}
	done := make(chan struct{})
	go func() { // 代理口整体关闭时也要放手
		select {
		case <-p.ctx.Done():
			c.Close()
		case <-done:
		}
	}()
	go a.readLoop()
	go a.janitor(done)
	io.Copy(io.Discard, c) // 控制连接上不会再有数据，读到断开为止
	close(done)
	c.Close()
	a.close()
}

func (a *udpAssoc) close() {
	a.relay.Close()
	a.mu.Lock()
	socks := a.socks
	a.socks = map[udpKey]*udpSock{}
	a.mu.Unlock()
	for _, s := range socks {
		s.conn.Close()
	}
}

// janitor 定时回收空闲出站套接字。
func (a *udpAssoc) janitor(done chan struct{}) {
	iv := a.idle / 4
	if iv < 10*time.Millisecond {
		iv = 10 * time.Millisecond
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-t.C:
		case <-done:
			return
		}
		now := time.Now().UnixNano()
		a.mu.Lock()
		for k, s := range a.socks {
			if now-s.last.Load() > int64(a.idle) {
				delete(a.socks, k)
				s.conn.Close()
			}
		}
		a.mu.Unlock()
	}
}

// readLoop 收客户端数据报，选转发、取（或建）出站套接字，发出载荷。
func (a *udpAssoc) readLoop() {
	buf := make([]byte, 65535)
	for {
		n, src, err := a.relay.ReadFromUDPAddrPort(buf)
		if err != nil {
			return // 中继口已关
		}
		hdrLen, dest, ok := parseUDPHeader(buf[:n])
		if !ok {
			continue // 分片或头不合法：丢
		}
		key := udpKey{src.String(), dest}
		f, target := a.p.pick(dest)
		s := a.sockFor(key, f, target, append([]byte(nil), buf[:hdrLen]...), src)
		if s == nil {
			continue
		}
		if _, err := s.conn.Write(buf[hdrLen:n]); err == nil {
			s.last.Store(time.Now().UnixNano())
		}
	}
}

// sockFor 取 key 对应的出站套接字；没有、或线路已切换就按转发当前线路新建。失败返回 nil（已记错）。
func (a *udpAssoc) sockFor(key udpKey, f *forwardSvc, target string, hdr []byte, src netip.AddrPort) *udpSock {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s := a.socks[key]; s != nil {
		if s.f == f && f.curGen() == s.gen {
			return s
		}
		delete(a.socks, key)
		s.conn.Close()
	}
	conn, gen, err := f.dialUDP(target)
	if err != nil {
		return nil
	}
	s := &udpSock{conn: conn, hdr: hdr, gen: gen, f: f}
	s.last.Store(time.Now().UnixNano())
	a.socks[key] = s
	go a.pump(key, s, src)
	return s
}

// pump 把出站套接字的回包封上头发回客户端；套接字关闭（空闲回收、线路切换、中继关闭）即退出。
func (a *udpAssoc) pump(key udpKey, s *udpSock, client netip.AddrPort) {
	buf := make([]byte, 65535)
	out := make([]byte, 0, 65535+len(s.hdr))
	for {
		n, err := s.conn.Read(buf)
		if err != nil {
			a.mu.Lock()
			if a.socks[key] == s {
				delete(a.socks, key)
			}
			a.mu.Unlock()
			s.conn.Close()
			return
		}
		s.last.Store(time.Now().UnixNano())
		out = append(append(out[:0], s.hdr...), buf[:n]...)
		a.relay.WriteToUDPAddrPort(out, client)
	}
}

// parseUDPHeader 解标准 SOCKS5 UDP 头，返回头长度和目的 主机:端口；FRAG≠0 或格式不对返回 false。
func parseUDPHeader(b []byte) (int, string, bool) {
	if len(b) < 4 || b[2] != 0 {
		return 0, "", false
	}
	var host string
	i := 4
	switch b[3] {
	case 1, 4:
		l := map[byte]int{1: 4, 4: 16}[b[3]]
		if len(b) < i+l+2 {
			return 0, "", false
		}
		host = net.IP(b[i : i+l]).String()
		i += l
	case 3:
		if len(b) < i+1 || len(b) < i+1+int(b[i])+2 {
			return 0, "", false
		}
		host = string(b[i+1 : i+1+int(b[i])])
		i += 1 + int(b[i])
	default:
		return 0, "", false
	}
	port := int(b[i])<<8 | int(b[i+1])
	return i + 2, net.JoinHostPort(host, strconv.Itoa(port)), true
}

func (f *forwardSvc) curGen() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gen
}

// dialUDP 在当前线路上建到 dest（空则线路自己的 addr，同 dialTo）的 UDP 套接字（计流量；经 SSH 的线路不支持，失败原因记进 error）。
func (f *forwardSvc) dialUDP(dest string) (net.Conn, int, error) {
	f.mu.Lock()
	idx, gen := f.active, f.gen
	r, d := f.routes[idx], f.dialers[idx]
	f.mu.Unlock()
	if dest == "" {
		dest = r.Addr
	}
	var c net.Conn
	var err error
	if r.Via != "" {
		err = errors.New("经 SSH 的线路不支持 UDP")
	} else {
		ctx, cancel := context.WithTimeout(f.ctx, dialTimeout)
		defer cancel()
		c, err = d.DialContext(ctx, "udp", dest)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err != nil {
		f.lastErr = "UDP：" + err.Error()
		sysLog.logThrottled("udp:"+f.name, f.lastErr, "转发 %s UDP 失败（线路 %s）：%v", f.name, r.Label, err)
		return nil, gen, err
	}
	if strings.HasPrefix(f.lastErr, "UDP：") {
		f.lastErr = ""
	}
	sysLog.clearThrottle("udp:" + f.name)
	if f.gen != gen { // 拨号期间切了线路：作废
		c.Close()
		return nil, gen, fmt.Errorf("线路已切换")
	}
	return c, gen, nil
}
