package wgtund

import (
	"io"
	"testing"
	"time"

	"github.com/WillyChen0811/base/go/sshterm"
)

// codeProxy 起一个认主机代号的代理口：主机 ubuntu 两条线路，各自拨到上游 A / B 并记下被要求拨的目的。
func codeProxy(t *testing.T, fs ...*forwardSvc) (string, *sshPool, []*recDialer) {
	t.Helper()
	a, b := startFwdUpstream(t, "A"), startFwdUpstream(t, "B")
	da, db := &recDialer{up: a}, &recDialer{up: b}
	pool := newSSHPool(nil)
	t.Cleanup(func() { pool.Close() })
	pool.add(hostConf{Name: "ubuntu", Routes: []routeConf{
		{Label: "内网", Addr: "192.168.147.115:22"},
		{Label: "家里", Tunnel: "home", Addr: "192.168.65.2:22"},
	}}, []sshterm.Dialer{da, db})
	m := map[string]*forwardSvc{}
	for _, f := range fs {
		m[f.name] = f
	}
	p := newProxy("127.0.0.1:0", "", m, pool)
	p.start()
	t.Cleanup(func() { p.Close() })
	return p.status().Addr, pool, []*recDialer{da, db}
}

// 目的主机是 hosts 里的名字：换成当前线路 addr 的 IP、端口用请求里的；切线路后新连接走新线路，旧连接被关掉
func TestProxyHostCodeFollowsSelectedRoute(t *testing.T) {
	addr, pool, ds := codeProxy(t)

	c := connectOK(t, addr, "ubuntu", 22)
	if l := askOnce(t, c, "hi"); l != "A:hi\n" {
		t.Errorf("首选线路回程 = %q", l)
	}
	c8 := connectOK(t, addr, "ubuntu", 8770)
	if l := askOnce(t, c8, "x"); l != "A:x\n" {
		t.Errorf("别的端口回程 = %q", l)
	}
	if g := ds[0].got(); len(g) != 2 || g[0] != "192.168.147.115:22" || g[1] != "192.168.147.115:8770" {
		t.Errorf("首选线路拨的目的 = %v", g)
	}

	if err := pool.switchRoute("ubuntu", 1); err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadAll(c); err != nil {
		t.Errorf("切线路后旧连接应被关掉（读到 EOF），得到 %v", err)
	}
	c2 := connectOK(t, addr, "ubuntu", 22)
	if l := askOnce(t, c2, "yo"); l != "B:yo\n" {
		t.Errorf("切线路后回程 = %q", l)
	}
	if g := ds[1].got(); len(g) != 1 || g[0] != "192.168.65.2:22" {
		t.Errorf("备用线路拨的目的 = %v", g)
	}
}

// 转发的 target 规则优先于主机代号（比如 ubuntu:8770 写成经 SSH 的转发）
func TestProxyRuleBeatsHostCode(t *testing.T) {
	up := startFwdUpstream(t, "F")
	f, fd := recFwd(t, "chat", []string{"ubuntu:8770"}, up)
	addr, _, ds := codeProxy(t, f)

	c := connectOK(t, addr, "ubuntu", 8770)
	if l := askOnce(t, c, "q"); l != "F:q\n" {
		t.Errorf("回程 = %q（应走 chat 转发）", l)
	}
	if len(fd[0].got()) != 1 || len(ds[0].got()) != 0 {
		t.Errorf("chat 转发拨了 %v，主机线路拨了 %v", fd[0].got(), ds[0].got())
	}
}

// 不是主机名的域名照旧走 proxy_default（不认成代号）
func TestProxyUnknownNameNotCode(t *testing.T) {
	addr, _, ds := codeProxy(t)
	c, rep := socksConnect(t, addr, "no-such-host.invalid", 22)
	c.Close()
	if rep == repOK {
		t.Errorf("不认识的名字不该拨通")
	}
	if len(ds[0].got())+len(ds[1].got()) != 0 {
		t.Errorf("不认识的名字不该走主机线路：%v %v", ds[0].got(), ds[1].got())
	}
}
