package wgtund

import (
	"context"
	"errors"
	"net"
	"sync"
	"syscall"
	"testing"
	"time"
)

// failDialer 总是拨号失败（假装系统那条网没开），并记下被要求拨的目的。
type failDialer struct {
	mu    sync.Mutex
	dests []string
}

func (d *failDialer) DialContext(_ context.Context, _, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.dests = append(d.dests, addr)
	d.mu.Unlock()
	return nil, syscall.EHOSTUNREACH
}

func (d *failDialer) got() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.dests...)
}

// failFwd 造两条线路的转发：首选 A 总失败，备用 B 连到 up；线路 addr 分别是 a-addr:1 / b-addr:2。
func failFwd(t *testing.T, name string, target []string, up *fwdUpstream) (*forwardSvc, *failDialer, *recDialer) {
	t.Helper()
	f := newForward(forwardConf{Name: name, Listen: "127.0.0.1:0", Target: target,
		Routes: []routeConf{{Label: "A", Addr: "a-addr:1"}, {Label: "B", Addr: "b-addr:2"}}}, nil, nil)
	t.Cleanup(func() { f.Close() })
	fd, rd := &failDialer{}, &recDialer{up: up}
	f.dialers[0] = &countDialer{fd, f.tc}
	f.dialers[1] = &countDialer{rd, f.tc}
	return f, fd, rd
}

// listen 口：选中线路拨不通，本次连接改走下一条；选中线路不变、状态仍记首选失败、流量计在该转发上
func TestForwardDialFallbackListen(t *testing.T) {
	b := startFwdUpstream(t, "B")
	f, fd, rd := failFwd(t, "smb", nil, b)
	if err := f.start(); err != nil {
		t.Fatal(err)
	}
	c := dialFwd(t, f.item().Listen)
	if l := askOnce(t, c, "hi"); l != "B:hi\n" {
		t.Errorf("回程 = %q", l)
	}
	if g := fd.got(); len(g) != 1 || g[0] != "a-addr:1" {
		t.Errorf("首选拨的目的 = %v", g)
	}
	if g := rd.got(); len(g) != 1 || g[0] != "b-addr:2" {
		t.Errorf("备用拨的目的 = %v（没指定目的时各拨各线路的 addr）", g)
	}
	it := f.item()
	if it.Active != 0 || !it.PreferredDown || it.Connected || it.Error == "" {
		t.Errorf("状态 = active %d preferredDown %v connected %v error %q（选中线路不变、仍记首选失败）",
			it.Active, it.PreferredDown, it.Connected, it.Error)
	}
	for i := 0; i < 200 && f.tc.view().UpTotal == 0; i++ { // 计数在拷贝协程里记，比回程稍晚
		time.Sleep(10 * time.Millisecond)
	}
	if v := f.tc.view(); v.UpTotal == 0 {
		t.Errorf("流量没计在该转发上: %+v", v)
	}
}

// 代理口：别名规则拨各线路自己的 addr，CIDR 规则的真实目的原样带给备用线路
func TestForwardDialFallbackProxy(t *testing.T) {
	b := startFwdUpstream(t, "B")
	f, fd, rd := failFwd(t, "x", []string{"ubuntu1st:22", "10.1.0.0/16"}, b)
	addr, _ := startProxy(t, "", f)
	c := connectOK(t, addr, "ubuntu1st", 22)
	if l := askOnce(t, c, "a"); l != "B:a\n" {
		t.Errorf("别名回程 = %q", l)
	}
	c2 := connectOK(t, addr, "10.1.2.3", 80)
	if l := askOnce(t, c2, "b"); l != "B:b\n" {
		t.Errorf("CIDR 回程 = %q", l)
	}
	if g := fd.got(); len(g) != 2 || g[0] != "a-addr:1" || g[1] != "10.1.2.3:80" {
		t.Errorf("首选拨的目的 = %v", g)
	}
	if g := rd.got(); len(g) != 2 || g[0] != "b-addr:2" || g[1] != "10.1.2.3:80" {
		t.Errorf("备用拨的目的 = %v", g)
	}
	if f.item().Active != 0 {
		t.Error("选中线路被改了")
	}
}

// 所有线路都拨不通：返回最初（选中线路）的错误，不卡住
func TestForwardDialAllFail(t *testing.T) {
	f, fd, _ := failFwd(t, "x", nil, startFwdUpstream(t, "B"))
	f.dialers[1] = &countDialer{&failDialer{}, f.tc}
	_, _, err := f.dial()
	if !errors.Is(err, syscall.EHOSTUNREACH) {
		t.Fatalf("err = %v", err)
	}
	if len(fd.got()) != 1 {
		t.Errorf("首选被拨了 %d 次，应只拨 1 次", len(fd.got()))
	}
}

// 选中备用线路（idx 1）时首选也会作为「其余线路」参与重试，顺序按 routes
func TestForwardDialFallbackOrder(t *testing.T) {
	a := startFwdUpstream(t, "A")
	f, _, _ := failFwd(t, "x", nil, a)
	// 选中 B 并让 B 失败，A 可用
	f.dialers[0] = &countDialer{&recDialer{up: a}, f.tc}
	f.dialers[1] = &countDialer{&failDialer{}, f.tc}
	if err := f.switchRoute(1); err != nil {
		t.Fatal(err)
	}
	c, _, err := f.dial()
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if f.item().Active != 1 {
		t.Error("选中线路被改了")
	}
}
