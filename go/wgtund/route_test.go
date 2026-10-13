package wgtund

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/WillyChen0811/base/go/sshterm"
	"github.com/coder/websocket"
)

// twoRoutes 把夹具里的主机改成 routes 写法：第一条（坏线）没人监听，第二条（好线）才是 SSH 服务。
func twoRoutes(cfg map[string]any) {
	h := cfg["hosts"].([]map[string]any)[0]
	delete(h, "tunnel")
	delete(h, "addr")
	h["routes"] = []map[string]any{
		{"label": "坏线", "tunnel": "home", "addr": "10.99.0.1:2222"},
		{"label": "好线", "tunnel": "home", "addr": "10.99.0.1:22"},
	}
}

type hostView struct {
	Name          string      `json:"name"`
	Tunnel        string      `json:"tunnel"`
	Connected     bool        `json:"connected"`
	Error         string      `json:"error"`
	Routes        []routeView `json:"routes"`
	Active        int         `json:"active"`
	PreferredDown bool        `json:"preferred_down"`
	PreferredBack bool        `json:"preferred_back"`
}

func hostsOf(t *testing.T, st started) []hostView {
	t.Helper()
	resp, b := get(t, st, "/hosts", true)
	if resp.StatusCode != 200 {
		t.Fatalf("/hosts 状态 %d: %s", resp.StatusCode, b)
	}
	var hs []hostView
	if err := json.Unmarshal(b, &hs); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	return hs
}

// postRoute 发 POST /hosts/route；origin 为空则不带 Origin 头。
func postRoute(t *testing.T, st started, query, origin string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, "http://"+st.HTTP+"/hosts/route?token="+st.Token+"&"+query, nil)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

const okOrigin = "http://127.0.0.1:8765"

func TestHostsLegacyRouteFields(t *testing.T) {
	f := newFixture(t, nil)
	st, _, _ := run(t, f.dir)
	hs := hostsOf(t, st)
	if len(hs) != 1 || len(hs[0].Routes) != 1 || hs[0].Active != 0 || hs[0].PreferredDown || hs[0].PreferredBack {
		t.Fatalf("旧配置的 /hosts = %+v", hs)
	}
	if r := hs[0].Routes[0]; r.Label != "home" || r.Tunnel != "home" || r.Addr != "10.99.0.1:22" {
		t.Errorf("线路 = %+v", r)
	}
	if _, b := get(t, st, "/hosts", true); strings.Contains(string(b), "id_test") {
		t.Errorf("不应暴露私钥路径: %s", b)
	}
}

func TestPreferredDownIsOnlyMarked(t *testing.T) {
	f := newFixture(t, twoRoutes)
	st, _, _ := run(t, f.dir)
	hs := hostsOf(t, st)
	if hs[0].Active != 0 || hs[0].Tunnel != "home" || len(hs[0].Routes) != 2 || hs[0].Routes[0].Label != "坏线" {
		t.Fatalf("启动时 /hosts = %+v", hs)
	}
	c := dialTerm(t, st, "host=ubuntu&cols=80&rows=24")
	if m := readCtrl(t, c); m["type"] != "error" {
		t.Fatalf("首选不通应回 error: %v", m)
	}
	hs = hostsOf(t, st)
	if !hs[0].PreferredDown || hs[0].Connected || hs[0].Active != 0 {
		t.Errorf("首选不通后应只标记 preferred_down、不自动切换: %+v", hs[0])
	}
}

func TestSwitchRouteValidation(t *testing.T) {
	f := newFixture(t, twoRoutes)
	st, _, _ := run(t, f.dir)
	cases := []struct {
		name, query, origin string
		method              string
		want                int
	}{
		{"没有 Origin", "host=ubuntu&route=1", "", "POST", 403},
		{"Origin 不在白名单", "host=ubuntu&route=1", "http://evil.example", "POST", 403},
		{"route 不是整数", "host=ubuntu&route=abc", okOrigin, "POST", 400},
		{"route 越界", "host=ubuntu&route=5", okOrigin, "POST", 400},
		{"route 为负", "host=ubuntu&route=-1", okOrigin, "POST", 400},
		{"未知主机", "host=nope&route=0", okOrigin, "POST", 404},
	}
	for _, c := range cases {
		if resp, b := postRoute(t, st, c.query, c.origin); resp.StatusCode != c.want {
			t.Errorf("%s: 状态 %d，期望 %d（%s）", c.name, resp.StatusCode, c.want, b)
		}
	}
	if resp, _ := get(t, st, "/hosts/route?host=ubuntu&route=1", true); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET 应 405，得到 %d", resp.StatusCode)
	}
	if hs := hostsOf(t, st); hs[0].Active != 0 {
		t.Errorf("被拒的请求不应切线路: %+v", hs[0])
	}
}

func TestSwitchRouteThenTerminal(t *testing.T) {
	f := newFixture(t, twoRoutes)
	st, _, _ := run(t, f.dir)
	// 先让首选失败，标出 preferred_down
	c0 := dialTerm(t, st, "host=ubuntu&cols=80&rows=24")
	readCtrl(t, c0)

	resp, b := postRoute(t, st, "host=ubuntu&route=1", okOrigin)
	if resp.StatusCode != 200 {
		t.Fatalf("切换应 200，得到 %d: %s", resp.StatusCode, b)
	}
	var h hostView
	if err := json.Unmarshal(b, &h); err != nil {
		t.Fatal(err)
	}
	if h.Active != 1 || h.PreferredDown || h.Name != "ubuntu" {
		t.Errorf("切换后 = %+v", h)
	}

	c := dialTerm(t, st, "host=ubuntu&cols=100&rows=30")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c.Write(ctx, websocket.MessageBinary, []byte("hi\n"))
	var buf strings.Builder
	for !strings.Contains(buf.String(), "\n") {
		typ, b, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("读终端输出失败: %v（已读 %q）", err, buf.String())
		}
		if typ == websocket.MessageText {
			t.Fatalf("意外的控制消息: %s", b)
		}
		buf.Write(b)
	}
	if strings.TrimSpace(buf.String()) != "echo:hi" {
		t.Errorf("got %q", buf.String())
	}
	if hs := hostsOf(t, st); !hs[0].Connected || hs[0].PreferredDown {
		t.Errorf("备用线路连上后 = %+v", hs[0])
	}
}

func TestPreferredBackDetected(t *testing.T) {
	old := routeProbeInterval
	routeProbeInterval = 100 * time.Millisecond
	t.Cleanup(func() { routeProbeInterval = old })

	f := newFixture(t, twoRoutes)
	st, _, _ := run(t, f.dir)
	if resp, b := postRoute(t, st, "host=ubuntu&route=1", okOrigin); resp.StatusCode != 200 {
		t.Fatalf("切换失败 %d: %s", resp.StatusCode, b)
	}
	time.Sleep(300 * time.Millisecond)
	if hs := hostsOf(t, st); hs[0].PreferredBack {
		t.Fatalf("首选还没恢复，不该标 preferred_back: %+v", hs[0])
	}

	ln, err := f.remote.Listen("tcp", "10.99.0.1:2222")
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
			c.Close()
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		hs := hostsOf(t, st)
		if hs[0].PreferredBack {
			if hs[0].Active != 1 {
				t.Errorf("只标记不切换，active 应仍是 1: %+v", hs[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("5 秒内没标出首选已恢复")
		}
		time.Sleep(50 * time.Millisecond)
	}
	resp, b := postRoute(t, st, "host=ubuntu&route=0", okOrigin)
	if resp.StatusCode != 200 {
		t.Fatalf("切回失败 %d: %s", resp.StatusCode, b)
	}
	var h hostView
	json.Unmarshal(b, &h)
	if h.Active != 0 || h.PreferredBack {
		t.Errorf("切回首选后 = %+v", h)
	}
}

func TestRoutesBadConfig(t *testing.T) {
	cases := map[string]struct {
		mutate func(map[string]any)
		want   string
	}{
		"routes 与 addr 同时写": {func(c map[string]any) {
			twoRoutes(c)
			c["hosts"].([]map[string]any)[0]["addr"] = "10.99.0.1:22"
		}, "只能写一种"},
		"routes 与 tunnel 同时写": {func(c map[string]any) {
			twoRoutes(c)
			c["hosts"].([]map[string]any)[0]["tunnel"] = "home"
		}, "只能写一种"},
		"线路地址没端口": {func(c map[string]any) {
			twoRoutes(c)
			c["hosts"].([]map[string]any)[0]["routes"].([]map[string]any)[1]["addr"] = "10.99.0.1"
		}, "第 2 条线路"},
		"线路引用不存在的隧道": {func(c map[string]any) {
			twoRoutes(c)
			c["hosts"].([]map[string]any)[0]["routes"].([]map[string]any)[0]["tunnel"] = "nope"
		}, "不存在的隧道"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, tc.mutate)
			var out strings.Builder
			err := Run(context.Background(), f.dir, strings.NewReader(""), &out)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("应报含 %q 的错，得到 %v", tc.want, err)
			}
			if out.Len() != 0 {
				t.Errorf("出错时不应打印启动信息: %q", out.String())
			}
		})
	}
}

func TestHostRoutesLabels(t *testing.T) {
	got := hostRoutes(hostConf{Routes: []routeConf{{Tunnel: "de", Addr: "a:1"}, {Addr: "b:2"}, {Label: "家", Tunnel: "home", Addr: "c:3"}}})
	if got[0].Label != "de" || got[1].Label != "直连" || got[2].Label != "家" {
		t.Errorf("label 补全 = %+v", got)
	}
	if one := hostRoutes(hostConf{Tunnel: "home", Addr: "x:22"}); len(one) != 1 || one[0].Addr != "x:22" || one[0].Tunnel != "home" {
		t.Errorf("旧配置应视为一条线路: %+v", one)
	}
}

// 协助 + 多线路：写 git_tunnel 要报错，不写则正常启动
func TestSupportMultiRoute(t *testing.T) {
	multi := func(f *fixture) {
		p := filepath.Join(f.dir, "hosts.json")
		b, _ := os.ReadFile(p)
		var cfg map[string]any
		json.Unmarshal(b, &cfg)
		h := cfg["hosts"].([]any)[0].(map[string]any)
		delete(h, "tunnel")
		delete(h, "addr")
		h["routes"] = []any{
			map[string]any{"tunnel": "home", "addr": "10.99.0.1:2222"},
			map[string]any{"tunnel": "home", "addr": "10.99.0.1:22"},
		}
		b, _ = json.Marshal(cfg)
		os.WriteFile(p, b, 0o600)
	}
	f, _, _ := supportFixture(t, func(s, _ map[string]any) { s["git_tunnel"] = "home" })
	multi(f)
	if err := runErr(t, f.dir); err == nil || !strings.Contains(err.Error(), "git_tunnel 不用写") {
		t.Errorf("多线路写 git_tunnel 应报错，得到 %v", err)
	}

	f, _, _ = supportFixture(t, nil)
	multi(f)
	st, _, _ := run(t, f.dir)
	if resp, b := get(t, st, "/support", true); resp.StatusCode != 200 {
		t.Fatalf("多线路、不写 git_tunnel 应能启动: %d %s", resp.StatusCode, b)
	}
}

func TestGitSSHCommandVia(t *testing.T) {
	got := gitSSHCommandVia("127.0.0.1:34567", "/h/known_hosts", "/h/id key")
	for _, w := range []string{"nc 127.0.0.1 34567", "StrictHostKeyChecking=yes", "-i '/h/id key'", "IdentitiesOnly=yes", "GIT_SSH_COMMAND=ssh"} {
		if !strings.Contains(got, w) {
			t.Errorf("缺少 %q: %s", w, got)
		}
	}
	if strings.Contains(got, "-X 5") {
		t.Errorf("不应再走 SOCKS: %s", got)
	}
}

// 直连两条线路：第 0 条没人监听，第 1 条是个会先吐 SSH 版本行的服务
func routePool(t *testing.T) (*sshPool, string) {
	t.Helper()
	dead, _ := net.Listen("tcp", "127.0.0.1:0")
	deadAddr := dead.Addr().String()
	dead.Close()
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
			io.WriteString(c, "SSH-2.0-routetest\r\n")
			c.Close()
		}
	}()
	p := newSSHPool(nil)
	t.Cleanup(func() { p.Close() })
	d := &net.Dialer{Timeout: time.Second}
	p.add(hostConf{Name: "h", Routes: []routeConf{{Addr: deadAddr}, {Addr: ln.Addr().String()}}}, []sshterm.Dialer{d, d})
	return p, ln.Addr().String()
}

func TestDialActiveFollowsRoute(t *testing.T) {
	p, _ := routePool(t)
	ctx := context.Background()
	if _, err := p.dialActive(ctx, "h"); err == nil {
		t.Fatal("首选没人监听，应拨号失败")
	}
	if st := p.routeInfo("h"); !st.PreferredDown {
		t.Errorf("首选拨号失败应标 preferred_down: %+v", st)
	}
	if err := p.switchRoute("h", 1); err != nil {
		t.Fatal(err)
	}
	c, err := p.dialActive(ctx, "h")
	if err != nil {
		t.Fatalf("切到备用后应能拨通: %v", err)
	}
	defer c.Close()
	l, _ := bufio.NewReader(c).ReadString('\n')
	if !strings.HasPrefix(l, "SSH-") {
		t.Errorf("got %q", l)
	}
	if err := p.switchRoute("nope", 0); err == nil {
		t.Error("未知主机应报错")
	}
	if err := p.switchRoute("h", 2); err == nil {
		t.Error("越界应报错")
	}
}

// 转发口把进来的连接接到当前线路
func TestRouteForwarderReachesActiveRoute(t *testing.T) {
	p, _ := routePool(t)
	fwd, err := routeForwarder(p, "h")
	if err != nil {
		t.Fatal(err)
	}
	defer fwd.Close()
	if !strings.HasPrefix(fwd.Addr().String(), "127.0.0.1:") {
		t.Errorf("转发口应只监听 127.0.0.1: %s", fwd.Addr())
	}
	p.switchRoute("h", 1)
	c, err := net.DialTimeout("tcp", fwd.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	l, _ := bufio.NewReader(c).ReadString('\n')
	if !strings.HasPrefix(l, "SSH-") {
		t.Errorf("经转发口应读到当前线路的 SSH 版本行，得到 %q", l)
	}
}
