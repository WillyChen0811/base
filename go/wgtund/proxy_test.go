package wgtund

import (
	"bufio"
	"context"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// socksConnect 对代理口走一遍 SOCKS5 握手 + CONNECT，返回连接和应答码。
// host 是点分 IPv4 时用 IPv4 地址类型，否则用域名类型。
func socksConnect(t *testing.T, proxy, host string, port int) (net.Conn, byte) {
	t.Helper()
	c := dialFwd(t, proxy)
	c.Write([]byte{5, 1, 0})
	m := make([]byte, 2)
	if _, err := io.ReadFull(c, m); err != nil || m[0] != 5 || m[1] != 0 {
		t.Fatalf("方法协商应答 = %v, %v", m, err)
	}
	req := []byte{5, 1, 0}
	if ip := net.ParseIP(host).To4(); ip != nil {
		req = append(req, 1)
		req = append(req, ip...)
	} else {
		req = append(req, 3, byte(len(host)))
		req = append(req, host...)
	}
	req = append(req, byte(port>>8), byte(port))
	c.Write(req)
	rep := make([]byte, 10)
	if _, err := io.ReadFull(c, rep); err != nil {
		t.Fatalf("读 CONNECT 应答: %v", err)
	}
	return c, rep[1]
}

// recDialer 记下被要求拨的目的，实际连到固定的上游（假装「这条线路」能到任何地方）。
type recDialer struct {
	up    *fwdUpstream
	mu    sync.Mutex
	dests []string
}

func (d *recDialer) DialContext(ctx context.Context, _, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.dests = append(d.dests, addr)
	d.mu.Unlock()
	return (&net.Dialer{}).DialContext(ctx, "tcp", d.up.addr())
}

func (d *recDialer) got() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.dests...)
}

// recFwd 造一条转发：每条线路拨到各自的上游并记目的；dialers 与 routes 一一对应。
func recFwd(t *testing.T, name string, target []string, ups ...*fwdUpstream) (*forwardSvc, []*recDialer) {
	t.Helper()
	fc := forwardConf{Name: name, Target: target}
	for i := range ups {
		fc.Routes = append(fc.Routes, routeConf{Label: ups[i].tag, Addr: "route-addr:1"})
	}
	f := newForward(fc, nil, nil)
	t.Cleanup(func() { f.Close() })
	var ds []*recDialer
	for i, u := range ups {
		d := &recDialer{up: u}
		f.dialers[i] = &countDialer{d, f.tc}
		ds = append(ds, d)
	}
	return f, ds
}

// startProxy 用给定转发起一个代理口，返回代理地址和代理本身。
func startProxy(t *testing.T, dflt string, fs ...*forwardSvc) (string, *proxySvc) {
	t.Helper()
	m := map[string]*forwardSvc{}
	for _, f := range fs {
		m[f.name] = f
	}
	p := newProxy("127.0.0.1:0", dflt, m, nil)
	p.start()
	t.Cleanup(func() { p.Close() })
	return p.status().Addr, p
}

func askOnce(t *testing.T, c net.Conn, msg string) string {
	t.Helper()
	io.WriteString(c, msg+"\n")
	l, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatalf("读回程: %v", err)
	}
	return l
}

func connectOK(t *testing.T, proxy, host string, port int) net.Conn {
	t.Helper()
	c, rep := socksConnect(t, proxy, host, port)
	if rep != repOK {
		t.Fatalf("CONNECT %s:%d 应答码 %d", host, port, rep)
	}
	return c
}

// 命中规则：走该转发的当前线路，流量计在该转发上；精确 主机:端口 的 target 是别名（拨线路 addr），
// 不带端口的规则仍拨程序请求的真实目的（域名不先解析）
func TestProxyRuleUsesCurrentRouteToRealDest(t *testing.T) {
	a := startFwdUpstream(t, "A")
	f, ds := recFwd(t, "smb", []string{"10.116.7.250:445", "Intra.Example"}, a)
	addr, _ := startProxy(t, "", f)

	c := connectOK(t, addr, "10.116.7.250", 445)
	if l := askOnce(t, c, "hi"); l != "A:hi\n" {
		t.Errorf("回程 = %q", l)
	}
	c2 := connectOK(t, addr, "intra.example", 8080) // 域名规则不带端口：任意端口，大小写不敏感
	if l := askOnce(t, c2, "yo"); l != "A:yo\n" {
		t.Errorf("域名回程 = %q", l)
	}
	if g := ds[0].got(); len(g) != 2 || g[0] != "route-addr:1" || g[1] != "intra.example:8080" {
		t.Errorf("线路拨的目的 = %v（域名不该被先解析）", g)
	}
	// 上行 3+3，下行 5+5；计数在拷贝协程里记，比回程到达稍晚，轮询等齐
	deadline := time.Now().Add(2 * time.Second)
	for f.tc.view().UpTotal < 6 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if v := f.tc.view(); v.UpTotal != 6 || v.DownTotal != 10 {
		t.Errorf("流量计数 = %+v", v)
	}
}

// CIDR 命中，且最具体优先：精确 IP:端口 > 精确 IP > 长前缀 > 短前缀
func TestProxyMostSpecificWins(t *testing.T) {
	ups := map[string]*fwdUpstream{}
	var fs []*forwardSvc
	for name, rules := range map[string][]string{
		"p16":   {"10.116.0.0/16"},
		"p24":   {"10.116.7.0/24"},
		"ip":    {"10.116.7.250"},
		"ipprt": {"10.116.7.250:445"},
	} {
		ups[name] = startFwdUpstream(t, name)
		f, _ := recFwd(t, name, rules, ups[name])
		fs = append(fs, f)
	}
	other := startFwdUpstream(t, "dflt")
	d, _ := recFwd(t, "d", nil, other)
	addr, _ := startProxy(t, "d", append(fs, d)...)
	for _, c := range []struct {
		host string
		port int
		want string
	}{
		{"10.116.7.250", 445, "ipprt"}, {"10.116.7.250", 80, "ip"},
		{"10.116.7.9", 80, "p24"}, {"10.116.9.9", 80, "p16"}, {"10.117.0.1", 80, "dflt"},
	} {
		conn := connectOK(t, addr, c.host, c.port)
		if l := askOnce(t, conn, "x"); l != c.want+":x\n" {
			t.Errorf("%s:%d 回程 %q，期望走 %s", c.host, c.port, l, c.want)
		}
	}
}

// 切线路后新连接走新线路，旧连接被关
func TestProxySwitchRoute(t *testing.T) {
	a, b := startFwdUpstream(t, "A"), startFwdUpstream(t, "B")
	f, _ := recFwd(t, "smb", []string{"10.116.0.0/16"}, a, b)
	addr, _ := startProxy(t, "", f)
	old := connectOK(t, addr, "10.116.7.250", 445)
	if l := askOnce(t, old, "1"); l != "A:1\n" {
		t.Fatalf("首选回程 = %q", l)
	}
	aDone := a.connDone(t, 0)
	if err := f.switchRoute(1); err != nil {
		t.Fatal(err)
	}
	select {
	case <-aDone:
	case <-time.After(2 * time.Second):
		t.Error("旧上游仍未见连接结束")
	}
	readClosed(t, old, 2*time.Second)
	c2 := connectOK(t, addr, "10.116.7.250", 445)
	if l := askOnce(t, c2, "2"); l != "B:2\n" {
		t.Errorf("新连接应到 B，得到 %q", l)
	}
}

// 没命中规则：proxy_default 缺省为直连，真连到目的，计入「其他（直连）」
func TestProxyUnmatchedGoesDirect(t *testing.T) {
	a, real := startFwdUpstream(t, "A"), startFwdUpstream(t, "real")
	f, ds := recFwd(t, "smb", []string{"10.116.7.250"}, a)
	addr, p := startProxy(t, "", f)
	host, port, _ := net.SplitHostPort(real.addr())
	pn, _ := strconv.Atoi(port)
	c := connectOK(t, addr, host, pn)
	if l := askOnce(t, c, "d"); l != "real:d\n" {
		t.Errorf("回程 = %q", l)
	}
	if len(ds[0].got()) != 0 {
		t.Error("未命中的目的不该走规则转发")
	}
	if p.direct == nil || p.direct.name != directName {
		t.Fatalf("direct = %+v", p.direct)
	}
	if v := p.direct.tc.view(); v.UpTotal != 2 || v.DownTotal != 7 {
		t.Errorf("直连流量 = %+v", v)
	}
	// 域名目的没命中也走直连，由本机解析
	c2 := connectOK(t, addr, "localhost", pn)
	if l := askOnce(t, c2, "n"); l != "real:n\n" {
		t.Errorf("域名直连回程 = %q", l)
	}
	// 拨号失败回对应错误码
	h, pt, _ := net.SplitHostPort(freeAddr(t))
	n, _ := strconv.Atoi(pt)
	if _, rep := socksConnect(t, addr, h, n); rep != repRefused {
		t.Errorf("直连被拒应答码 %d，期望 %d", rep, repRefused)
	}
}

// proxy_default 指向某条转发：没命中的目的借用它的当前线路，且不再有「其他（直连）」
func TestProxyDefaultForward(t *testing.T) {
	a, b := startFwdUpstream(t, "A"), startFwdUpstream(t, "B")
	f1, _ := recFwd(t, "smb", []string{"10.116.7.250"}, a)
	f2, ds := recFwd(t, "gw", nil, b)
	addr, p := startProxy(t, "gw", f1, f2)
	c := connectOK(t, addr, "8.8.8.8", 53)
	if l := askOnce(t, c, "q"); l != "B:q\n" {
		t.Errorf("回程 = %q", l)
	}
	if g := ds[0].got(); len(g) != 1 || g[0] != "8.8.8.8:53" {
		t.Errorf("借用线路拨的目的 = %v", g)
	}
	if p.direct != nil {
		t.Error("proxy_default 指向转发时不该有直连计数")
	}
}

// 非 CONNECT 命令回「命令不支持」
func TestProxyRejectsBind(t *testing.T) {
	a := startFwdUpstream(t, "A")
	f, _ := recFwd(t, "smb", []string{"10.116.7.250"}, a)
	addr, _ := startProxy(t, "", f)
	c := dialFwd(t, addr)
	c.Write([]byte{5, 1, 0})
	io.ReadFull(c, make([]byte, 2))
	c.Write([]byte{5, 2, 0, 1, 10, 116, 7, 250, 1, 189})
	rep := make([]byte, 10)
	if _, err := io.ReadFull(c, rep); err != nil || rep[1] != repCmdNotSupp {
		t.Errorf("应答 = %v, %v", rep, err)
	}
}

// 隧道没配 DNS：域名目的失败并写进转发的 error，域名没有交给下层拨号器（即没有任何本机解析）；IP 目的不受影响
func TestProxyTunnelWithoutDNSRejectsDomain(t *testing.T) {
	a := startFwdUpstream(t, "A")
	f, ds := recFwd(t, "smb", []string{"intra.example", "10.116.0.0/16"}, a)
	f.dialers[0] = &countDialer{&dnsGuard{"home", ds[0], func() bool { return false }}, f.tc}
	addr, _ := startProxy(t, "", f)
	if _, rep := socksConnect(t, addr, "intra.example", 445); rep == repOK {
		t.Fatal("没配 DNS 的隧道不该放行域名")
	}
	if g := ds[0].got(); len(g) != 0 {
		t.Errorf("域名不该交给下层拨号器: %v", g)
	}
	if e := f.item().Error; !strings.Contains(e, "隧道 home 没配 DNS") {
		t.Errorf("error = %q", e)
	}
	connectOK(t, addr, "10.116.7.250", 445) // IP 目的照常
	if len(ds[0].got()) != 1 {
		t.Errorf("IP 目的应拨出: %v", ds[0].got())
	}
}

// CIDR 规则和带端口的 CIDR 不是别名：仍拨程序请求的真实目的；精确域名:端口 是别名
func TestProxyAliasOnlyForExactHostPort(t *testing.T) {
	a := startFwdUpstream(t, "A")
	f, ds := recFwd(t, "x", []string{"10.1.0.0/16", "10.2.0.0/16:80", "10.3.3.3", "ubuntu1st:22"}, a)
	addr, _ := startProxy(t, "", f)
	for _, d := range []struct {
		host string
		port int
	}{{"10.1.2.3", 22}, {"10.2.2.2", 80}, {"10.3.3.3", 9}, {"Ubuntu1st", 22}} {
		c := connectOK(t, addr, d.host, d.port)
		askOnce(t, c, "x")
		c.Close()
	}
	want := []string{"10.1.2.3:22", "10.2.2.2:80", "10.3.3.3:9", "route-addr:1"}
	g := ds[0].got()
	for i := range want {
		if len(g) != len(want) || g[i] != want[i] {
			t.Fatalf("线路拨的目的 = %v，期望 %v", g, want)
		}
	}
}
