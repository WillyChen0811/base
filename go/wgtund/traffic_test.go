package wgtund

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTrafficRingKeepsLast600(t *testing.T) {
	c := newTrafficCounter()
	base := time.Unix(1000, 0)
	for i := 0; i < trafficKeep+50; i++ {
		c.up.Add(10)
		c.sample(base.Add(time.Duration(i) * time.Second))
	}
	pts := c.points(trafficKeep + 100)
	if len(pts) != trafficKeep {
		t.Fatalf("点数 = %d，要 %d", len(pts), trafficKeep)
	}
	if pts[0].T != 1050 || pts[len(pts)-1].T != int64(1000+trafficKeep+49) {
		t.Fatalf("首尾时间不对: %d %d", pts[0].T, pts[len(pts)-1].T)
	}
	for _, p := range pts {
		if p.UpBps != 10 || p.DownBps != 0 {
			t.Fatalf("每秒应为 10/0: %+v", p)
		}
	}
	if got := c.points(5); len(got) != 5 || got[4].T != pts[len(pts)-1].T {
		t.Fatalf("最近 5 个点不对: %+v", got)
	}
}

func TestTrafficViewAveragesRecentSeconds(t *testing.T) {
	c := newTrafficCounter()
	if v := c.view(); v != (trafficView{}) {
		t.Fatalf("空计数器应全 0: %+v", v)
	}
	now := time.Unix(2000, 0)
	for i, n := range []int64{1000, 300, 600, 900} { // 窗口 3：只取后三个，平均 600
		c.up.Add(n)
		c.down.Add(n * 2)
		c.sample(now.Add(time.Duration(i) * time.Second))
	}
	v := c.view()
	if v.UpBps != 600 || v.DownBps != 1200 || v.UpTotal != 2800 || v.DownTotal != 5600 {
		t.Fatalf("view = %+v", v)
	}
}

func TestCountConnCountsBothDirections(t *testing.T) {
	a, b := net.Pipe()
	c := newTrafficCounter()
	cc := wrapCount(a, c)
	go func() { b.Write([]byte("hello")); io.Copy(io.Discard, b) }()
	buf := make([]byte, 16)
	if n, _ := cc.Read(buf); n != 5 {
		t.Fatalf("读到 %d", n)
	}
	if _, err := cc.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	cc.Close()
	if c.down.Load() != 5 || c.up.Load() != 3 {
		t.Fatalf("down=%d up=%d", c.down.Load(), c.up.Load())
	}
	if _, ok := cc.(interface{ CloseWrite() error }); ok {
		t.Fatal("net.Pipe 不支持 CloseWrite，包装后也不该有")
	}
}

func TestCountConnKeepsCloseWrite(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		io.Copy(io.Discard, c)
		c.Close()
	}()
	conn, err := (&countDialer{&net.Dialer{}, newTrafficCounter()}).DialContext(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	cw, ok := conn.(interface{ CloseWrite() error })
	if !ok || cw.CloseWrite() != nil {
		t.Fatal("TCP 连接包装后应保留 CloseWrite")
	}
}

// 经转发的真实流量被记到这条转发上，/forwards 的 item 带 traffic。
func TestForwardCountsTraffic(t *testing.T) {
	echo, _ := net.Listen("tcp", "127.0.0.1:0")
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	f := newForward(forwardConf{Name: "e", Listen: "127.0.0.1:0",
		Routes: []routeConf{{Addr: echo.Addr().String()}}}, nil, nil)
	if err := f.start(); err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	waitFwdCond(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.addr != "" }, "监听")
	c, err := net.Dial("tcp", f.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	msg := []byte("0123456789")
	c.Write(msg)
	io.ReadFull(c, make([]byte, len(msg)))
	waitFwdCond(t, func() bool { return f.tc.up.Load() == 10 && f.tc.down.Load() == 10 }, "计数到 10/10")
	f.tc.sample(time.Unix(3000, 0))
	tv := f.item().Traffic
	if tv.UpTotal != 10 || tv.DownTotal != 10 || tv.UpBps != 10 || tv.DownBps != 10 {
		t.Fatalf("item.traffic = %+v", tv)
	}
}

func TestTrafficEndpoint(t *testing.T) {
	h := newTrafficHub()
	hc := h.host("same")
	fc := newTrafficCounter()
	h.forward("same", fc) // 与主机重名
	h.forward("only-fwd", newTrafficCounter())
	hc.up.Add(100)
	fc.down.Add(7)
	for i := 0; i < 5; i++ {
		h.tick(time.Unix(int64(100+i), 0))
	}
	type resp struct {
		Name, Kind string
		Points     []trafficPoint
	}
	do := func(q string) (int, resp) {
		rec := httptest.NewRecorder()
		h.serveHTTP(rec, httptest.NewRequest(http.MethodGet, "/traffic?"+q, nil))
		var r resp
		json.Unmarshal(rec.Body.Bytes(), &r)
		return rec.Code, r
	}
	if code, r := do("name=same"); code != 200 || r.Kind != "host" || len(r.Points) != 5 || r.Points[0].UpBps != 100 {
		t.Fatalf("默认先主机: %d %+v", code, r)
	}
	if code, r := do("name=same&kind=forward&seconds=2"); code != 200 || r.Kind != "forward" || len(r.Points) != 2 || r.Points[0].T != 103 {
		t.Fatalf("forward: %d %+v", code, r)
	}
	if code, r := do("name=only-fwd"); code != 200 || r.Kind != "forward" {
		t.Fatalf("只有转发时无 kind 也能找到: %d %+v", code, r)
	}
	if code, r := do("name=same&seconds=99999"); code != 200 || len(r.Points) != 5 {
		t.Fatalf("seconds 超上限应截到可用点数: %d %d", code, len(r.Points))
	}
	for _, q := range []string{"name=nope", "name=only-fwd&kind=host", "name="} {
		if code, _ := do(q); code != 404 {
			t.Fatalf("%q 应 404，得 %d", q, code)
		}
	}
	for _, q := range []string{"name=same&seconds=abc", "name=same&seconds=0", "name=same&kind=x"} {
		if code, _ := do(q); code != 400 {
			t.Fatalf("%q 应 400，得 %d", q, code)
		}
	}
}
