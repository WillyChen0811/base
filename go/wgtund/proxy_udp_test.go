package wgtund

import (
	"context"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// udpEcho 起一个 UDP 回声桩：回 "tag:" + 载荷。
func udpEcho(t *testing.T, tag string) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			pc.WriteTo(append([]byte(tag+":"), buf[:n]...), from)
		}
	}()
	return pc.LocalAddr().String()
}

// udpDialer 记下被要求拨的目的，实际把 UDP 连到固定的桩（假装「这条线路」能到任何地方）。
type udpDialer struct {
	up    string
	mu    sync.Mutex
	dests []string
}

func (d *udpDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.dests = append(d.dests, network+" "+addr)
	d.mu.Unlock()
	return (&net.Dialer{}).DialContext(ctx, "udp", d.up)
}

func (d *udpDialer) got() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.dests...)
}

// udpFwd 造一条只有一条线路的转发，线路是 udpDialer。
func udpFwd(t *testing.T, name string, target []string, up string) (*forwardSvc, *udpDialer) {
	t.Helper()
	f := newForward(forwardConf{Name: name, Target: target, Routes: []routeConf{{Label: name, Addr: "x:1"}}}, nil, nil)
	t.Cleanup(func() { f.Close() })
	d := &udpDialer{up: up}
	f.dialers[0] = &countDialer{d, f.tc}
	return f, d
}

// associate 对代理口做 UDP ASSOCIATE，返回控制连接和已连到中继口的 UDP 套接字。
func associate(t *testing.T, proxy string) (net.Conn, *net.UDPConn) {
	t.Helper()
	c := dialFwd(t, proxy)
	c.Write([]byte{5, 1, 0})
	io.ReadFull(c, make([]byte, 2))
	c.Write([]byte{5, cmdUDPAssociate, 0, 1, 0, 0, 0, 0, 0, 0})
	rep := make([]byte, 10)
	if _, err := io.ReadFull(c, rep); err != nil || rep[1] != repOK || rep[3] != 1 {
		t.Fatalf("UDP ASSOCIATE 应答 = %v, %v", rep, err)
	}
	ra := &net.UDPAddr{IP: net.IP(rep[4:8]), Port: int(rep[8])<<8 | int(rep[9])}
	u, err := net.DialUDP("udp", nil, ra)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { u.Close() })
	return c, u
}

// udpPacket 封一个带标准 UDP 头的数据报。
func udpPacket(frag byte, host string, port int, payload string) []byte {
	b := []byte{0, 0, frag}
	if ip := net.ParseIP(host).To4(); ip != nil {
		b = append(b, 1)
		b = append(b, ip...)
	} else {
		b = append(b, 3, byte(len(host)))
		b = append(b, host...)
	}
	b = append(b, byte(port>>8), byte(port))
	return append(b, payload...)
}

// udpAsk 发一个数据报并读回载荷（超时返回 ok=false）；同时校验回包头与请求头一致。
func udpAsk(t *testing.T, u *net.UDPConn, host string, port int, msg string, wait time.Duration) (string, bool) {
	t.Helper()
	req := udpPacket(0, host, port, msg)
	u.Write(req)
	buf := make([]byte, 2048)
	u.SetReadDeadline(time.Now().Add(wait))
	n, err := u.Read(buf)
	if err != nil {
		return "", false
	}
	hl := len(req) - len(msg)
	if string(buf[:hl]) != string(req[:hl]) {
		t.Errorf("回包头 = %v，期望 %v", buf[:hl], req[:hl])
	}
	return string(buf[hl:n]), true
}

func hostPort(t *testing.T, addr string) (string, int) {
	h, p, _ := net.SplitHostPort(addr)
	n, err := strconv.Atoi(p)
	if err != nil {
		t.Fatal(err)
	}
	return h, n
}

// 没命中规则走直连：真发到目的（IP 与域名），回包带头，流量计入「其他（直连）」
func TestProxyUDPDirect(t *testing.T) {
	real := udpEcho(t, "real")
	a, _ := udpFwd(t, "smb", []string{"10.116.7.250"}, udpEcho(t, "A"))
	addr, p := startProxy(t, "", a)
	_, u := associate(t, addr)
	h, port := hostPort(t, real)
	if got, ok := udpAsk(t, u, h, port, "hi", 3*time.Second); !ok || got != "real:hi" {
		t.Errorf("直连回包 = %q, %v", got, ok)
	}
	if got, ok := udpAsk(t, u, "localhost", port, "dn", 3*time.Second); !ok || got != "real:dn" {
		t.Errorf("域名直连回包 = %q, %v", got, ok)
	}
	if v := p.direct.tc.view(); v.UpTotal != 4 || v.DownTotal != 14 { // 上行 2+2，下行 "real:hi"+"real:dn" 各 7
		t.Errorf("直连流量 = %+v", v)
	}
}

// 命中规则的转发用它的当前线路发 UDP，拨的是真实目的（域名不预先解析），流量计在该转发上
func TestProxyUDPRuleRoute(t *testing.T) {
	f, d := udpFwd(t, "smb", []string{"10.116.0.0/16", "intra.example"}, udpEcho(t, "A"))
	addr, _ := startProxy(t, "", f)
	_, u := associate(t, addr)
	if got, ok := udpAsk(t, u, "10.116.7.250", 53, "q1", 3*time.Second); !ok || got != "A:q1" {
		t.Errorf("回包 = %q, %v", got, ok)
	}
	if got, ok := udpAsk(t, u, "intra.example", 5353, "q2", 3*time.Second); !ok || got != "A:q2" {
		t.Errorf("域名回包 = %q, %v", got, ok)
	}
	udpAsk(t, u, "10.116.7.250", 53, "q3", 3*time.Second) // 同源同目的复用套接字，不再拨
	g := d.got()
	if len(g) != 2 || g[0] != "udp 10.116.7.250:53" || g[1] != "udp intra.example:5353" {
		t.Errorf("线路拨的目的 = %v", g)
	}
	if v := f.tc.view(); v.UpTotal != 6 || v.DownTotal != 12 {
		t.Errorf("转发流量 = %+v", v)
	}
}

// 经 SSH 的线路：丢包，error 写明不支持 UDP
func TestProxyUDPViaDrops(t *testing.T) {
	f, d := udpFwd(t, "smb", []string{"10.116.7.250"}, udpEcho(t, "A"))
	f.routes[0].Via = "jump"
	addr, _ := startProxy(t, "", f)
	_, u := associate(t, addr)
	if got, ok := udpAsk(t, u, "10.116.7.250", 53, "q", 400*time.Millisecond); ok {
		t.Errorf("经 SSH 不该有回包，得到 %q", got)
	}
	if e := f.item().Error; !strings.Contains(e, "经 SSH 的线路不支持 UDP") {
		t.Errorf("error = %q", e)
	}
	if len(d.got()) != 0 {
		t.Errorf("不该拨号: %v", d.got())
	}
}

// 隧道没配 DNS：域名目的丢包并记 error，IP 目的照常
func TestProxyUDPTunnelWithoutDNS(t *testing.T) {
	f, d := udpFwd(t, "smb", []string{"intra.example", "10.116.0.0/16"}, udpEcho(t, "A"))
	f.dialers[0] = &countDialer{&dnsGuard{"home", d, func() bool { return false }}, f.tc}
	addr, _ := startProxy(t, "", f)
	_, u := associate(t, addr)
	if _, ok := udpAsk(t, u, "intra.example", 53, "q", 400*time.Millisecond); ok {
		t.Error("没配 DNS 的隧道不该放行域名")
	}
	if e := f.item().Error; !strings.Contains(e, "隧道 home 没配 DNS") {
		t.Errorf("error = %q", e)
	}
	if got, ok := udpAsk(t, u, "10.116.7.250", 53, "ok", 3*time.Second); !ok || got != "A:ok" {
		t.Errorf("IP 目的回包 = %q, %v", got, ok)
	}
}

// 分片包直接丢；坏头不影响后续
func TestProxyUDPDropsFragment(t *testing.T) {
	f, d := udpFwd(t, "smb", []string{"10.116.7.250"}, udpEcho(t, "A"))
	addr, _ := startProxy(t, "", f)
	_, u := associate(t, addr)
	u.Write(udpPacket(1, "10.116.7.250", 53, "frag"))
	u.Write([]byte{0, 0})
	if got, ok := udpAsk(t, u, "10.116.7.250", 53, "ok", 3*time.Second); !ok || got != "A:ok" {
		t.Errorf("回包 = %q, %v", got, ok)
	}
	if len(d.got()) != 1 {
		t.Errorf("分片包不该触发拨号: %v", d.got())
	}
}

// 控制连接断开后中继口关闭
func TestProxyUDPRelayClosesWithControl(t *testing.T) {
	f, _ := udpFwd(t, "smb", []string{"10.116.7.250"}, udpEcho(t, "A"))
	addr, _ := startProxy(t, "", f)
	c, u := associate(t, addr)
	if _, ok := udpAsk(t, u, "10.116.7.250", 53, "a", 3*time.Second); !ok {
		t.Fatal("断开前应通")
	}
	c.Close()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		u.Write(udpPacket(0, "10.116.7.250", 53, "b"))
		u.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		if _, err := u.Read(make([]byte, 100)); err != nil {
			if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
				return // 端口不可达：中继口已关
			}
		}
	}
	t.Error("控制连接断开后中继口仍在")
}

// 空闲套接字被回收：之后同源同目的重新拨号
func TestProxyUDPIdleRecycle(t *testing.T) {
	old := udpIdle
	udpIdle = 100 * time.Millisecond
	defer func() { udpIdle = old }()
	f, d := udpFwd(t, "smb", []string{"10.116.7.250"}, udpEcho(t, "A"))
	addr, _ := startProxy(t, "", f)
	_, u := associate(t, addr)
	udpAsk(t, u, "10.116.7.250", 53, "1", 3*time.Second)
	time.Sleep(500 * time.Millisecond)
	if got, ok := udpAsk(t, u, "10.116.7.250", 53, "2", 3*time.Second); !ok || got != "A:2" {
		t.Errorf("回收后回包 = %q, %v", got, ok)
	}
	if n := len(d.got()); n != 2 {
		t.Errorf("拨号次数 = %d，期望 2", n)
	}
}

// 切线路后同一目的改走新线路
func TestProxyUDPSwitchRoute(t *testing.T) {
	fc := forwardConf{Name: "smb", Target: []string{"10.116.7.250"},
		Routes: []routeConf{{Label: "a", Addr: "x:1"}, {Label: "b", Addr: "x:1"}}}
	f := newForward(fc, nil, nil)
	t.Cleanup(func() { f.Close() })
	for i, tag := range []string{"A", "B"} {
		f.dialers[i] = &countDialer{&udpDialer{up: udpEcho(t, tag)}, f.tc}
	}
	addr, _ := startProxy(t, "", f)
	_, u := associate(t, addr)
	if got, _ := udpAsk(t, u, "10.116.7.250", 53, "1", 3*time.Second); got != "A:1" {
		t.Errorf("首选回包 = %q", got)
	}
	f.switchRoute(1)
	if got, _ := udpAsk(t, u, "10.116.7.250", 53, "2", 3*time.Second); got != "B:2" {
		t.Errorf("切线路后回包 = %q", got)
	}
}
