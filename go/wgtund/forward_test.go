package wgtund

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/WillyChen0811/base/go/internal/sshtest"
)

// forwardView 是 /forwards 的一项：字段与 hostView 一致，外加 listen。
type forwardView struct {
	Name          string      `json:"name"`
	Listen        string      `json:"listen"`
	Tunnel        string      `json:"tunnel"`
	Connected     bool        `json:"connected"`
	Error         string      `json:"error"`
	Routes        []routeView `json:"routes"`
	Active        int         `json:"active"`
	PreferredDown bool        `json:"preferred_down"`
	PreferredBack bool        `json:"preferred_back"`
}

func forwardsOf(t *testing.T, st started) []forwardView {
	t.Helper()
	resp, b := get(t, st, "/forwards", true)
	if resp.StatusCode != 200 {
		t.Fatalf("/forwards 状态 %d: %s", resp.StatusCode, b)
	}
	var fs []forwardView
	if err := json.Unmarshal(b, &fs); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	return fs
}

// postFwdRoute 发 POST /forwards/route；origin 为空则不带 Origin 头。
func postFwdRoute(t *testing.T, st started, query, origin string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, "http://"+st.HTTP+"/forwards/route?token="+st.Token+"&"+query, nil)
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

// waitFwd 轮询 /forwards 直到条件成立（超时即失败）。
func waitFwd(t *testing.T, st started, want func(forwardView) bool) forwardView {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		fs := forwardsOf(t, st)
		if len(fs) != 1 {
			t.Fatalf("/forwards = %+v", fs)
		}
		if want(fs[0]) {
			return fs[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("等不到期望状态: %+v", fs[0])
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// fwdUpstream 是本地假上游：逐行回显（带 tag 前缀），记录收到的行和连接次数；
// 每条接受的连接结束（读 EOF/出错）时关掉对应的 done 通道，供测试从上游一侧验证连接确实断开。
type fwdUpstream struct {
	ln     net.Listener
	tag    string
	mu     sync.Mutex
	got    []string
	conns  int
	closed []chan struct{}
}

func startFwdUpstream(t *testing.T, tag string) *fwdUpstream {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	u := &fwdUpstream{ln: ln, tag: tag}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			done := make(chan struct{})
			u.mu.Lock()
			u.conns++
			u.closed = append(u.closed, done)
			u.mu.Unlock()
			go func(c net.Conn) {
				defer close(done)
				defer c.Close()
				br := bufio.NewReader(c)
				for {
					l, err := br.ReadString('\n')
					if err != nil {
						return
					}
					u.mu.Lock()
					u.got = append(u.got, l)
					u.mu.Unlock()
					io.WriteString(c, tag+":"+l)
				}
			}(c)
		}
	}()
	return u
}

func (u *fwdUpstream) addr() string { return u.ln.Addr().String() }

func (u *fwdUpstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.conns
}

// connDone 取上游第 i 条接受的连接「已结束（读 EOF/出错）」的信号；还没出现则等，超时即失败。
func (u *fwdUpstream) connDone(t *testing.T, i int) <-chan struct{} {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		u.mu.Lock()
		if i < len(u.closed) {
			ch := u.closed[i]
			u.mu.Unlock()
			return ch
		}
		u.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatalf("上游 %s 没有第 %d 条连接", u.tag, i+1)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// readClosed 读一个字节，要求在时限内因确定的断连（EOF/重置等）而失败；
// 读超时不算关闭——那只说明这段时间没有新数据，连接可能根本没关。
func readClosed(t *testing.T, c net.Conn, within time.Duration) {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(within))
	n, err := c.Read(make([]byte, 1))
	if err == nil {
		t.Errorf("连接还读到 %d 字节，应已关闭", n)
		return
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Errorf("读超时（连接可能没关、只是没新数据）: %v", err)
	}
}

// dialFwd 连到转发端口，带超时保护。
func dialFwd(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(5 * time.Second))
	return c
}

// freeAddr 取一个没人监听的回环地址（关掉后端口空出来）。
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := ln.Addr().String()
	ln.Close()
	return a
}

// listenAgain 重新占回刚释放的地址（内核可能暂时不给出，重试）。
func listenAgain(t *testing.T, addr string) net.Listener {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			return ln
		}
		if time.Now().After(deadline) {
			t.Fatalf("占回地址 %s 失败: %v", addr, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// addFwd 往配置里写 forwards 段。
func addFwd(cfg map[string]any, fw ...map[string]any) {
	cfg["forwards"] = fw
}

func fwdConf(name, listen string, routes ...map[string]any) map[string]any {
	return map[string]any{"name": name, "listen": listen, "routes": routes}
}

func fwdRoute(label, tunnel, addr string) map[string]any {
	m := map[string]any{"tunnel": tunnel, "addr": addr}
	if label != "" {
		m["label"] = label
	}
	return m
}

// (a) 转发把数据双向送到上游
func TestForwardReachesUpstream(t *testing.T) {
	u := startFwdUpstream(t, "A")
	f := newFixture(t, func(cfg map[string]any) {
		addFwd(cfg, fwdConf("omlx", "127.0.0.1:0", fwdRoute("内网", "", u.addr())))
	})
	st, _, _ := run(t, f.dir)
	fs := forwardsOf(t, st)
	if len(fs) != 1 || fs[0].Name != "omlx" || fs[0].Connected || fs[0].Error != "" {
		t.Fatalf("/forwards = %+v", fs)
	}
	if !strings.HasPrefix(fs[0].Listen, "127.0.0.1:") || strings.HasSuffix(fs[0].Listen, ":0") {
		t.Errorf("应报告实际监听端口: %s", fs[0].Listen)
	}
	c := dialFwd(t, fs[0].Listen)
	br := bufio.NewReader(c)
	io.WriteString(c, "ping\n")
	l, err := br.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if l != "A:ping\n" {
		t.Errorf("回程 = %q，期望 A:ping", l)
	}
	io.WriteString(c, "pong\n") // 再来一回，确认不是单向
	if l, err := br.ReadString('\n'); err != nil || l != "A:pong\n" {
		t.Errorf("第二回 = %q, %v", l, err)
	}
	fs = forwardsOf(t, st)
	if !fs[0].Connected || fs[0].Error != "" || fs[0].Active != 0 || fs[0].Tunnel != "" {
		t.Errorf("连上后 /forwards = %+v", fs[0])
	}
}

// 半关闭：客户端写完请求后 CloseWrite，上游读到 EOF 才回复，客户端必须收到完整响应
func TestForwardHalfClose(t *testing.T) {
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
				req, _ := io.ReadAll(c) // 读到 EOF（客户端半关闭）后才回复
				time.Sleep(100 * time.Millisecond)
				io.WriteString(c, "resp:"+string(req))
			}()
		}
	}()
	f := newFixture(t, func(cfg map[string]any) {
		addFwd(cfg, fwdConf("omlx", "127.0.0.1:0", fwdRoute("内网", "", ln.Addr().String())))
	})
	st, _, _ := run(t, f.dir)
	fs := forwardsOf(t, st)
	c := dialFwd(t, fs[0].Listen)
	io.WriteString(c, "request-body")
	if err := c.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(c)
	if err != nil || string(got) != "resp:request-body" {
		t.Errorf("响应 = %q, %v，期望 resp:request-body", got, err)
	}
}

// (b) /forwards 的字段名与 /hosts 的一项一致（外加 listen）
func TestForwardFieldsMatchHosts(t *testing.T) {
	u := startFwdUpstream(t, "A")
	f := newFixture(t, func(cfg map[string]any) {
		addFwd(cfg, fwdConf("omlx", "127.0.0.1:0",
			fwdRoute("内网", "", u.addr()), fwdRoute("", "home", "10.99.0.1:8192")))
	})
	st, _, _ := run(t, f.dir)
	_, hb := get(t, st, "/hosts", true)
	_, fb := get(t, st, "/forwards", true)
	var hs, raw []map[string]any
	if err := json.Unmarshal(hb, &hs); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(fb, &raw); err != nil {
		t.Fatal(err)
	}
	if len(hs) != 1 || len(raw) != 1 {
		t.Fatalf("/hosts=%s /forwards=%s", hb, fb)
	}
	for k := range hs[0] {
		if _, ok := raw[0][k]; !ok {
			t.Errorf("/forwards 项缺 /hosts 有的字段 %q: %s", k, fb)
		}
	}
	var items []forwardView
	if err := json.Unmarshal(fb, &items); err != nil {
		t.Fatal(err)
	}
	v := items[0]
	// label 省略时照主机线路补全（隧道名 / 直连）
	if v.Name != "omlx" || v.Active != 0 || v.PreferredDown || v.PreferredBack || v.Connected || v.Error != "" {
		t.Errorf("/forwards 项 = %+v", v)
	}
	if len(v.Routes) != 2 || v.Routes[0].Label != "内网" || v.Routes[1].Label != "home" || v.Routes[1].Tunnel != "home" {
		t.Errorf("线路 = %+v", v.Routes)
	}
}

// 没配置 forwards 时 /forwards 返回空数组
func TestForwardNotConfigured(t *testing.T) {
	f := newFixture(t, nil)
	st, _, _ := run(t, f.dir)
	resp, b := get(t, st, "/forwards", true)
	if resp.StatusCode != 200 || strings.TrimSpace(string(b)) != "[]" {
		t.Errorf("没配置应返回空数组: %d %s", resp.StatusCode, b)
	}
}

// (c) /forwards/route 的参数校验 + 切换后新连接走新线路、旧连接被关
func TestForwardSwitchRoute(t *testing.T) {
	a := startFwdUpstream(t, "A")
	b := startFwdUpstream(t, "B")
	f := newFixture(t, func(cfg map[string]any) {
		addFwd(cfg, fwdConf("omlx", "127.0.0.1:0", fwdRoute("A", "", a.addr()), fwdRoute("B", "", b.addr())))
	})
	st, _, _ := run(t, f.dir)

	cases := []struct {
		name, query, origin string
		method              string
		want                int
	}{
		{"没有 Origin", "name=omlx&route=1", "", "POST", 403},
		{"Origin 不在白名单", "name=omlx&route=1", "http://evil.example", "POST", 403},
		{"route 不是整数", "name=omlx&route=abc", okOrigin, "POST", 400},
		{"route 越界", "name=omlx&route=5", okOrigin, "POST", 400},
		{"route 为负", "name=omlx&route=-1", okOrigin, "POST", 400},
		{"未知转发名", "name=nope&route=0", okOrigin, "POST", 404},
	}
	for _, c := range cases {
		if resp, body := postFwdRoute(t, st, c.query, c.origin); resp.StatusCode != c.want {
			t.Errorf("%s: 状态 %d，期望 %d（%s）", c.name, resp.StatusCode, c.want, body)
		}
	}
	if resp, _ := get(t, st, "/forwards/route?name=omlx&route=1", true); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET 应 405，得到 %d", resp.StatusCode)
	}
	if fs := forwardsOf(t, st); fs[0].Active != 0 {
		t.Fatalf("被拒的请求不应切线路: %+v", fs[0])
	}

	// 老连接先到首选 A，并保持打开
	fs := forwardsOf(t, st)
	old := dialFwd(t, fs[0].Listen)
	io.WriteString(old, "1\n")
	if l, err := bufio.NewReader(old).ReadString('\n'); err != nil || l != "A:1\n" {
		t.Fatalf("首选线路回程 = %q, %v", l, err)
	}
	aDone := a.connDone(t, 0) // A 上就这一条连接

	resp, body := postFwdRoute(t, st, "name=omlx&route=1", okOrigin)
	if resp.StatusCode != 200 {
		t.Fatalf("切换应 200，得到 %d: %s", resp.StatusCode, body)
	}
	var v forwardView
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatal(err)
	}
	if v.Active != 1 || v.PreferredDown || v.PreferredBack || v.Error != "" || v.Name != "omlx" {
		t.Errorf("切换后 = %+v", v)
	}

	// 旧连接被关掉：上游一侧要在短时间内看到连接结束，本侧读到确定断连错误——
	// 只等「任意读错误」不行：连接没关、单纯没新数据也会以读超时的形式"通过"。
	select {
	case <-aDone:
	case <-time.After(2 * time.Second):
		t.Error("切换 2 秒后旧上游仍未见连接结束：旧连接没被关")
	}
	readClosed(t, old, 2*time.Second)
	// 新连接去 B
	c2 := dialFwd(t, fs[0].Listen)
	io.WriteString(c2, "2\n")
	if l, err := bufio.NewReader(c2).ReadString('\n'); err != nil || l != "B:2\n" {
		t.Fatalf("新连接应到 B，得到 %q, %v", l, err)
	}
	if a.count() != 1 || b.count() != 1 {
		t.Errorf("上游连接数 A=%d B=%d，期望各 1", a.count(), b.count())
	}
	if fs := forwardsOf(t, st); fs[0].Active != 1 || !fs[0].Connected {
		t.Errorf("切换后状态 = %+v", fs[0])
	}
}

// (d) 首选连不上：标 preferred_down 记 error，不自动切换
func TestForwardPreferredDownIsOnlyMarked(t *testing.T) {
	dead := freeAddr(t)
	b := startFwdUpstream(t, "B")
	f := newFixture(t, func(cfg map[string]any) {
		addFwd(cfg, fwdConf("omlx", "127.0.0.1:0", fwdRoute("坏线", "", dead), fwdRoute("好线", "", b.addr())))
	})
	st, _, _ := run(t, f.dir)
	fs := forwardsOf(t, st)
	if fs[0].Active != 0 || fs[0].PreferredDown || fs[0].Connected {
		t.Fatalf("启动时 = %+v", fs[0])
	}
	// 连转发口：上游拨不通，两端都会被关掉
	c, err := net.DialTimeout("tcp", fs[0].Listen, 5*time.Second)
	if err != nil {
		t.Fatal(err) // 监听本身是开着的
	}
	defer c.Close()
	// 必须是确定的断连（且很快），不能拿读超时当"已关闭"
	readClosed(t, c, 2*time.Second)
	v := waitFwd(t, st, func(v forwardView) bool { return v.Error != "" })
	if !v.PreferredDown || v.Connected || v.Active != 0 || v.PreferredBack {
		t.Errorf("首选不通应只标记不切换: %+v", v)
	}
	if b.count() != 0 {
		t.Errorf("不该自动用到备用线路: %d", b.count())
	}
}

// 走备用期间探测首选：通了只标 preferred_back，不自动切回
func TestForwardPreferredBackDetected(t *testing.T) {
	old := routeProbeInterval
	routeProbeInterval = 100 * time.Millisecond
	t.Cleanup(func() { routeProbeInterval = old })

	dead := freeAddr(t)
	b := startFwdUpstream(t, "B")
	f := newFixture(t, func(cfg map[string]any) {
		addFwd(cfg, fwdConf("omlx", "127.0.0.1:0", fwdRoute("坏线", "", dead), fwdRoute("好线", "", b.addr())))
	})
	st, _, _ := run(t, f.dir)
	if resp, body := postFwdRoute(t, st, "name=omlx&route=1", okOrigin); resp.StatusCode != 200 {
		t.Fatalf("切换失败 %d: %s", resp.StatusCode, body)
	}
	time.Sleep(300 * time.Millisecond)
	if fs := forwardsOf(t, st); fs[0].PreferredBack {
		t.Fatalf("首选还没恢复，不该标 preferred_back: %+v", fs[0])
	}

	ln := listenAgain(t, dead)
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close() // 只测 TCP 能不能连上
		}
	}()
	v := waitFwd(t, st, func(v forwardView) bool { return v.PreferredBack })
	if v.Active != 1 {
		t.Errorf("只标记不切换，active 应仍是 1: %+v", v)
	}
	resp, body := postFwdRoute(t, st, "name=omlx&route=0", okOrigin)
	if resp.StatusCode != 200 {
		t.Fatalf("切回失败 %d: %s", resp.StatusCode, body)
	}
	var back forwardView
	json.Unmarshal(body, &back)
	if back.Active != 0 || back.PreferredBack {
		t.Errorf("切回首选后 = %+v", back)
	}
}

// ctrlDialer 是可控的假拨号器：每次 DialContext 建一对 net.Pipe，一端返回、
// 另一端留下当假上游；gate 非 nil 时卡在 gate 直到被关闭或 ctx 取消（可控的拨号完成/取消点）。
type ctrlDialer struct {
	gate     chan struct{} // nil 表示立即拨成
	mu       sync.Mutex
	attempts int
	peers    []net.Conn
}

func (d *ctrlDialer) DialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	d.mu.Lock()
	d.attempts++
	d.mu.Unlock()
	if d.gate != nil {
		select {
		case <-d.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	c, peer := net.Pipe()
	d.mu.Lock()
	d.peers = append(d.peers, peer)
	d.mu.Unlock()
	return c, nil
}

func (d *ctrlDialer) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.attempts
}

func (d *ctrlDialer) lastPeer(t *testing.T) net.Conn {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.peers) == 0 {
		t.Fatal("假拨号器还没拨出过连接")
	}
	return d.peers[len(d.peers)-1]
}

// waitAttempts 等假拨号器确实接到过 n 次拨号（确认「在途」，不靠 sleep 猜）。
func waitAttempts(t *testing.T, d *ctrlDialer, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if d.count() >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("假拨号器只接到 %d 次拨号，期望至少 %d", d.count(), n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitNoFwdGoroutines 在时限内等栈上带 forwardSvc 的协程全部退出，超时把残留栈打出来。
func waitNoFwdGoroutines(t *testing.T, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	buf := make([]byte, 4<<20)
	for {
		n := runtime.Stack(buf, true)
		dump := string(buf[:n])
		if !strings.Contains(dump, "forwardSvc") {
			return
		}
		if time.Now().After(deadline) {
			var left strings.Builder
			for _, blk := range strings.Split(dump, "\n\n") {
				if strings.Contains(blk, "forwardSvc") {
					left.WriteString(blk)
					left.WriteString("\n")
				}
			}
			t.Fatalf("Close 后 %v 内仍有转发协程未退出:\n%s", within, left.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// P1 回归：按 serve 的合法调度顺序走 dial → switchRoute → track——
// dial 检查看到 gen 没变、返回连接之后线路才切换完，track 必须拒绝登记旧线路的连接，
// 并当场关掉两端（本机客户端和旧上游都读到 EOF），数据不能经旧线路传输。
func TestForwardOldRouteConnRefusedAfterSwitch(t *testing.T) {
	d := &ctrlDialer{}
	f := newForward(forwardConf{Name: "x", Listen: "127.0.0.1:0",
		Routes: []routeConf{{Label: "A", Addr: "a:1"}, {Label: "B", Addr: "b:2"}}}, nil, nil)
	f.dialers[0] = d
	t.Cleanup(func() { f.Close() })

	// net.Pipe 的另一端没人读时 Write 会一直等（没有默认 deadline），
	// 所以每一端都要有 Cleanup 兜底：中途 Fatal 也不能把 pipe 晾在协程里。
	clientEnd, fwdEnd := net.Pipe() // 模拟本机客户端与 serve 接受的转发入口
	t.Cleanup(func() { clientEnd.Close() })
	t.Cleanup(func() { fwdEnd.Close() })

	up, gen, err := f.dial() // 旧线路 A 拨成
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { up.Close() })
	// 这条拨号的假上游端（ctrlDialer 留下的另一半），当场取定，别等 d.lastPeer——
	// 后面 switchRoute 会起首选探测，再取就可能取到探测那条的半边。
	peer := d.lastPeer(t)
	t.Cleanup(func() { peer.Close() })
	if gen != 0 {
		t.Fatalf("拨号代际 = %d，期望 0", gen)
	}
	if err := f.switchRoute(1); err != nil { // track 尚未登记，切换先完成
		t.Fatal(err)
	}
	if f.track(gen, fwdEnd, up) {
		t.Fatal("切换完成后，旧线路（gen=0）的连接不应被登记进新集合")
	}
	// 两端当场关闭：本机客户端与旧上游都确定断连（不是读超时）。
	// readClosed 只 Errorf 不中断，这里失败必须停：后面的无界写不可信。
	readClosed(t, clientEnd, 2*time.Second)
	if t.Failed() {
		t.Fatal("本机客户端没读到确定断连，停止后续检查")
	}
	readClosed(t, peer, 2*time.Second)
	if t.Failed() {
		t.Fatal("旧上游没读到确定断连，停止后续检查")
	}
	// 没有数据能经旧线路：入口已关，写入应立刻失败。写超时算失败——
	// 那只说明连接还活着、暂时没人读，不等于「不能转发」。
	clientEnd.SetWriteDeadline(time.Now().Add(time.Second))
	n, err := clientEnd.Write([]byte("x"))
	if err == nil {
		t.Fatalf("旧线路连接已拒登，入口写入竟成功 %d 字节（可能继续转发）", n)
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Fatalf("入口写入超时（连接没关、只是没人读）: %v", err)
	}
}

// P2 补充：Close 与进行中的拨号/探测并发——Close 取消上下文后，
// 卡着的探测和拨号都要被叫醒，3 秒内不允许有转发协程残留在栈上。
func TestForwardCloseStopsAllGoroutines(t *testing.T) {
	old := routeProbeInterval
	routeProbeInterval = 100 * time.Millisecond
	t.Cleanup(func() { routeProbeInterval = old })

	probeStuck := &ctrlDialer{gate: make(chan struct{})} // 首选：探测永远挂到这被取消
	serveStuck := &ctrlDialer{gate: make(chan struct{})} // 备用：业务拨号挂到这被取消
	f := newForward(forwardConf{Name: "x", Listen: "127.0.0.1:0",
		Routes: []routeConf{{Label: "A", Addr: "a:1"}, {Label: "B", Addr: "b:2"}}}, nil, nil)
	f.dialers[0], f.dialers[1] = probeStuck, serveStuck
	t.Cleanup(func() { f.Close() }) // 已停则幂等

	if err := f.start(); err != nil {
		t.Fatal(err)
	}
	if err := f.switchRoute(1); err != nil { // 走备用 B，起首选 A 的探测
		t.Fatal(err)
	}
	waitAttempts(t, probeStuck, 1) // 探测确实在途

	c, err := net.DialTimeout("tcp", f.item().Listen, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	waitAttempts(t, serveStuck, 1) // 业务连接的拨号也在途

	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	readClosed(t, c, 2*time.Second) // 入口侧被关
	waitNoFwdGoroutines(t, 3*time.Second)
}

// P2 补充：监听端口被占用时 start 返回错误，不能谎报起来了。
func TestForwardStartPortOccupied(t *testing.T) {
	// 直接占住一个系统给的回环端口并持有到测试结束：
	// 不用 freeAddr 那种「先释放再抢回」，那中间端口可能被别的进程抢走。
	hold, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hold.Close() })
	addr := hold.Addr().String()
	f := newForward(forwardConf{Name: "busy", Listen: addr,
		Routes: []routeConf{{Label: "直连", Addr: "10.99.0.1:8192"}}}, nil, nil)
	t.Cleanup(func() { f.Close() })
	if err := f.start(); err == nil {
		t.Fatal("监听端口被占用时 start 应返回错误")
	}
	if v := f.item(); v.Listen != "" {
		t.Errorf("监听失败不应报告监听地址: %+v", v)
	}
}

// (e) 配置校验：不合规一律启动失败
func TestForwardBadConfig(t *testing.T) {
	good := func() map[string]any {
		return fwdConf("omlx", "127.0.0.1:0", fwdRoute("内网", "", "192.168.147.196:8192"))
	}
	cases := map[string]struct {
		mutate func(map[string]any)
		want   string
	}{
		"监听非回环地址": {func(c map[string]any) {
			f := good()
			f["listen"] = "0.0.0.0:18192"
			addFwd(c, f)
		}, "回环"},
		"监听不是主机:端口": {func(c map[string]any) {
			f := good()
			f["listen"] = "127.0.0.1"
			addFwd(c, f)
		}, "主机:端口"},
		"隧道不存在": {func(c map[string]any) {
			f := good()
			f["routes"] = []map[string]any{fwdRoute("", "nope", "10.99.0.1:8192")}
			addFwd(c, f)
		}, "不存在的隧道"},
		"与主机重名": {func(c map[string]any) {
			f := good()
			f["name"] = "ubuntu"
			addFwd(c, f)
		}, "与主机重名"},
		"名字为空": {func(c map[string]any) {
			f := good()
			f["name"] = ""
			addFwd(c, f)
		}, "转发名"},
		"名字互相重复": {func(c map[string]any) {
			addFwd(c, good(), good())
		}, "重复"},
		"空 routes": {func(c map[string]any) {
			f := good()
			f["routes"] = []map[string]any{}
			addFwd(c, f)
		}, "至少 1 条线路"},
		"线路地址没端口": {func(c map[string]any) {
			f := good()
			f["routes"] = []map[string]any{fwdRoute("", "", "192.168.147.196")}
			addFwd(c, f)
		}, "主机:端口"},
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

// via 线路：fwdVia 造一条经主机 SSH 的线路。
func fwdVia(label, via, addr string) map[string]any {
	m := map[string]any{"via": via, "addr": addr}
	if label != "" {
		m["label"] = label
	}
	return m
}

func roundTrip(c net.Conn, br *bufio.Reader, msg string) (string, error) {
	if _, err := io.WriteString(c, msg+"\n"); err != nil {
		return "", err
	}
	return br.ReadString('\n')
}

// via：经主机的共用 SSH 连接（direct-tcpip）转到上游，label 缺省「经 主机」，/forwards 带 via
func TestForwardViaSSHReachesUpstream(t *testing.T) {
	u := startFwdUpstream(t, "U")
	f := newFixtureOpts(t, func(cfg map[string]any) {
		addFwd(cfg, fwdConf("chat", "127.0.0.1:0", fwdVia("", "ubuntu", u.addr())))
	}, sshtest.Options{Forward: true})
	st, _, _ := run(t, f.dir)
	fs := forwardsOf(t, st)
	if len(fs) != 1 || len(fs[0].Routes) != 1 || fs[0].Routes[0].Label != "经 ubuntu" || fs[0].Routes[0].Via != "ubuntu" {
		t.Fatalf("/forwards = %+v", fs)
	}
	c := dialFwd(t, fs[0].Listen)
	br := bufio.NewReader(c)
	if l, err := roundTrip(c, br, "ping"); err != nil || l != "U:ping\n" {
		t.Fatalf("回程 = %q, %v，期望 U:ping", l, err)
	}
	// 再开一条转发连接，仍共用同一条 SSH 连接
	c2 := dialFwd(t, fs[0].Listen)
	if l, err := roundTrip(c2, bufio.NewReader(c2), "pong"); err != nil || l != "U:pong\n" {
		t.Fatalf("第二条 = %q, %v", l, err)
	}
	if n := f.ssh.Conns(); n != 1 {
		t.Errorf("SSH 连接数 = %d，应复用同一条", n)
	}
	if fs = forwardsOf(t, st); !fs[0].Connected || fs[0].Error != "" {
		t.Errorf("连上后 /forwards = %+v", fs[0])
	}
}

// via 指向不存在的主机、via 与 tunnel 同写：启动失败
func TestForwardViaConfigErrors(t *testing.T) {
	u := startFwdUpstream(t, "U")
	cases := map[string]map[string]any{
		"不存在的主机":       fwdVia("", "nope", u.addr()),
		"via 与 tunnel": {"via": "ubuntu", "tunnel": "home", "addr": u.addr()},
	}
	for name, route := range cases {
		f := newFixture(t, func(cfg map[string]any) {
			addFwd(cfg, fwdConf("chat", "127.0.0.1:0", route))
		})
		err := runErr(t, f.dir)
		if err == nil || !strings.Contains(err.Error(), "via") {
			t.Errorf("%s：err = %v，期望提到 via 的配置错误", name, err)
		}
	}
}

// SSH 断开后，新进来的转发连接经重拨的新连接可用
func TestForwardViaSSHReconnect(t *testing.T) {
	u := startFwdUpstream(t, "U")
	f := newFixtureOpts(t, func(cfg map[string]any) {
		addFwd(cfg, fwdConf("chat", "127.0.0.1:0", fwdVia("", "ubuntu", u.addr())))
	}, sshtest.Options{Forward: true})
	st, _, _ := run(t, f.dir)
	listen := forwardsOf(t, st)[0].Listen
	c := dialFwd(t, listen)
	br := bufio.NewReader(c)
	if l, err := roundTrip(c, br, "one"); err != nil || l != "U:one\n" {
		t.Fatalf("断线前 = %q, %v", l, err)
	}
	f.ssh.CloseConns()
	readClosed(t, c, 3*time.Second) // 在途连接随旧连接结束
	deadline := time.Now().Add(10 * time.Second)
	for {
		c2, err := net.DialTimeout("tcp", listen, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		c2.SetDeadline(time.Now().Add(5 * time.Second))
		l, err := roundTrip(c2, bufio.NewReader(c2), "two")
		c2.Close()
		if err == nil && l == "U:two\n" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("断线重连后仍不可用: %q, %v", l, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if n := f.ssh.Conns(); n != 2 {
		t.Errorf("SSH 连接数 = %d，期望重拨后共 2 条", n)
	}
}
