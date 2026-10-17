// forward.go：本机端口转发——在固定回环端口监听，每个进来的连接拨到「当前线路」的远端地址。
// 线路规则和主机线路一致：启动从首选开始、不记忆上次，拨不通只标 preferred_down 不自动切换，
// 走备用期间定时探测首选、通了标 preferred_back，切换由用户在页面上决定。
package wgtund

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/WillyChen0811/base/go/sshterm"
	"github.com/WillyChen0811/base/go/wgtunnel"
)

// forwardConf 是 hosts.json 里 forwards 的一条：在本机 listen 上监听，经 routes 的当前线路到 addr。
type forwardConf struct {
	Name   string      `json:"name"`
	Listen string      `json:"listen"`
	Routes []routeConf `json:"routes"`
}

// validate 校验一条转发配置；cfgs 是已解析的隧道，hostNames 是 hosts 里已有的主机名。
// listen 只许回环（127.0.0.1 或 localhost，端口可以是 0 让系统挑）；线路要求同主机多线路。
func (fc forwardConf) validate(cfgs map[string]*wgtunnel.Config, hostNames map[string]bool) error {
	if fc.Name == "" || hostNames[fc.Name] {
		return fmt.Errorf("hosts.json: 转发名 %q 为空或与主机重名", fc.Name)
	}
	host, _, err := net.SplitHostPort(fc.Listen)
	if err != nil || (host != "127.0.0.1" && !strings.EqualFold(host, "localhost")) {
		return fmt.Errorf("hosts.json: 转发 %q 的监听地址 %q 必须是回环地址（127.0.0.1 或 localhost）的 主机:端口", fc.Name, fc.Listen)
	}
	if len(fc.Routes) == 0 {
		return fmt.Errorf("hosts.json: 转发 %q 没有 routes（至少 1 条线路）", fc.Name)
	}
	for i, r := range fc.Routes {
		if _, _, err := net.SplitHostPort(r.Addr); err != nil {
			return fmt.Errorf("hosts.json: 转发 %q 第 %d 条线路的地址 %q 要写成 主机:端口", fc.Name, i+1, r.Addr)
		}
		if r.Via != "" && r.Tunnel != "" {
			return fmt.Errorf("hosts.json: 转发 %q 第 %d 条线路的 via 与 tunnel 不能同时写", fc.Name, i+1)
		}
		if r.Via != "" && !hostNames[r.Via] {
			return fmt.Errorf("hosts.json: 转发 %q 第 %d 条线路的 via 引用了不存在的主机 %q", fc.Name, i+1, r.Via)
		}
		if r.Tunnel != "" && cfgs[r.Tunnel] == nil {
			return fmt.Errorf("hosts.json: 转发 %q 第 %d 条线路引用了不存在的隧道 %q", fc.Name, i+1, r.Tunnel)
		}
	}
	return nil
}

// forwardItem 是 /forwards 的一项；字段名与 /hosts 的一项一致（网页的线路条当同一种数据渲染），多了 listen。
type forwardItem struct {
	Name      string `json:"name"`
	Listen    string `json:"listen"`
	Tunnel    string `json:"tunnel"`
	Connected bool   `json:"connected"`
	Error     string `json:"error"`
	routeState
}

// forwardSvc 是一条带多线路的本机端口转发。
type forwardSvc struct {
	name    string
	listen  string
	routes  []routeConf
	dialers []sshterm.Dialer // 与 routes 一一对应（隧道或直连）

	ctx    context.Context
	cancel context.CancelFunc

	mu            sync.Mutex
	ln            net.Listener
	addr          string // 实际监听地址（start 成功后填，listen 写 :0 时是系统挑的端口）
	closed        bool
	active        int  // 当前线路下标，启动总是 0
	connected     bool // 最近一次拨号是否成功（还没拨过为 false）
	lastErr       string
	preferredDown bool                  // 首选线路连不上（只标记，不自动切换）
	preferredBack bool                  // 走备用期间探测到首选已恢复（只标记，由用户决定是否切回）
	probing       bool                  // 已有探测协程在跑
	gen           int                   // 每次切换线路加一，用来作废切换前发起的拨号
	conns         map[net.Conn]struct{} // 已建立的转发连接（两端都在里面，切换时一起关）
}

// newForward 按配置建转发（不监听）：dialers 照主机线路的建法（隧道为空则直连），label 补全照 hostRoutes。
func newForward(fc forwardConf, tunnels map[string]*wgtunnel.Tunnel, pool *sshPool) *forwardSvc {
	ctx, cancel := context.WithCancel(context.Background())
	s := &forwardSvc{
		name:    fc.Name,
		listen:  fc.Listen,
		routes:  make([]routeConf, len(fc.Routes)),
		dialers: make([]sshterm.Dialer, len(fc.Routes)),
		ctx:     ctx,
		cancel:  cancel,
		conns:   map[net.Conn]struct{}{},
	}
	for i, r := range fc.Routes {
		if r.Label == "" {
			if r.Via != "" {
				r.Label = "经 " + r.Via
			} else if r.Tunnel != "" {
				r.Label = r.Tunnel
			} else {
				r.Label = "直连"
			}
		}
		s.routes[i] = r
		var d sshterm.Dialer = &net.Dialer{Timeout: dialTimeout}
		if r.Via != "" {
			d = &viaDialer{pool: pool, host: r.Via}
		} else if r.Tunnel != "" {
			d = tunnels[r.Tunnel]
		}
		s.dialers[i] = d
	}
	return s
}

// viaDialer 经主机的共用 SSH 连接拨号（direct-tcpip）：每次拨号现取连接，断了由连接池重拨，
// 所以重连后的新转发连接自然用新连接，在途的随旧连接结束。
type viaDialer struct {
	pool *sshPool
	host string
}

func (v *viaDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	c, err := v.pool.conn(v.host)
	if err != nil {
		return nil, err
	}
	return c.DialContext(ctx, network, addr)
}

// start 按 listen 开监听并起接受循环；监听失败返回错误（调用方当启动失败处理）。
func (f *forwardSvc) start() error {
	host, port, err := net.SplitHostPort(f.listen)
	if err != nil {
		return err
	}
	if host != "127.0.0.1" {
		host = "127.0.0.1" // localhost 也固定绑回环 IPv4，别拿到 ::1
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(host, port))
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.ln, f.addr = ln, ln.Addr().String()
	f.mu.Unlock()
	go f.acceptLoop(ln)
	return nil
}

func (f *forwardSvc) acceptLoop(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return // 监听已关闭（退出或 Close）
		}
		go f.serve(c)
	}
}

// serve 把进来的连接拨到当前线路的 addr 再双向拷贝。某方向读到 EOF 时只对另一侧 CloseWrite（TCP 半关闭），
// 让反向的响应继续拷完；两个方向都结束才关两条连接。不支持 CloseWrite 或拷贝出错则退回完全关闭。
// 切换线路/退出时 switchRoute/Close 强制关已登记的连接，两个拷贝随之结束。
func (f *forwardSvc) serve(c net.Conn) {
	up, gen, err := f.dial()
	if err != nil {
		c.Close()
		return
	}
	if !f.track(gen, c, up) {
		return // 已 Close，或拨完还没登记这段时间里切了线路：track 把两边都关了，旧线路不开始转发
	}
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, err := io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok && err == nil {
			if cw.CloseWrite() != nil {
				dst.Close()
				src.Close()
			}
		} else {
			dst.Close()
			src.Close()
		}
		done <- struct{}{}
	}
	go cp(up, c)
	go cp(c, up)
	<-done
	<-done
	f.untrack(c, up)
	c.Close()
	up.Close()
}

// track 登记一对 gen 线路上建立的转发连接；「查状态」和「登记」在同一把锁里：
// 已关闭、或拨号之后线路已切换（gen 与当前不一致）都拒绝登记并直接关掉两边，
// 保证切换完成后仍卡在 dial→track 之间的旧线路连接不会漏进新集合继续转发。
func (f *forwardSvc) track(gen int, cs ...net.Conn) bool {
	f.mu.Lock()
	if f.closed || gen != f.gen {
		f.mu.Unlock()
		for _, c := range cs {
			c.Close()
		}
		return false
	}
	for _, c := range cs {
		f.conns[c] = struct{}{}
	}
	f.mu.Unlock()
	return true
}

func (f *forwardSvc) untrack(cs ...net.Conn) {
	f.mu.Lock()
	for _, c := range cs {
		delete(f.conns, c)
	}
	f.mu.Unlock()
}

// dial 在当前线路上拨到远端 addr（超时 dialTimeout），成败都记进状态给 /forwards；
// 拨号期间用户切了线路（gen 变了）就把这条旧线路的连接关掉（同主机 conn 的防护）。
// 同时返回拨号所属的代际 gen，交给 track 在同一把锁里验过再登记——堵住「dial 已返回、
// track 未登记」的窗口（那期间 switchRoute 关不到这条还没进集合的连接）。
func (f *forwardSvc) dial() (net.Conn, int, error) {
	f.mu.Lock()
	idx, gen := f.active, f.gen
	addr, d := f.routes[idx].Addr, f.dialers[idx]
	f.mu.Unlock()

	ctx, cancel := context.WithTimeout(f.ctx, dialTimeout)
	defer cancel()
	c, err := d.DialContext(ctx, "tcp", addr)

	f.mu.Lock()
	if f.gen != gen {
		f.mu.Unlock()
		if c != nil {
			c.Close()
		}
		return nil, gen, errors.New("线路已切换，请重试")
	}
	f.connected = err == nil
	if err != nil {
		f.lastErr = err.Error()
		if idx == 0 && len(f.routes) > 1 {
			f.preferredDown = true
		}
	} else {
		f.lastErr = ""
		if idx == 0 {
			f.preferredDown = false
		}
	}
	f.mu.Unlock()
	if err != nil && c != nil {
		c.Close() // 自写 Dialer 万一返回了半连接也别漏
	}
	return c, gen, err
}

// switchRoute 切到第 idx 条线路：清状态并关掉当前所有已转发的连接（之后的新连接走新线路）。
func (f *forwardSvc) switchRoute(idx int) error {
	f.mu.Lock()
	if idx < 0 || idx >= len(f.routes) {
		f.mu.Unlock()
		return &routeError{msg: fmt.Sprintf("没有第 %d 条线路", idx)}
	}
	f.active = idx
	f.gen++ // 在途拨号走的是旧线路：完成时作废
	f.preferredDown = false
	f.preferredBack = false
	f.connected = false
	f.lastErr = ""
	old := make([]net.Conn, 0, len(f.conns))
	for c := range f.conns {
		old = append(old, c)
	}
	f.conns = map[net.Conn]struct{}{}
	if idx != 0 && len(f.routes) > 1 && !f.probing {
		f.probing = true
		go f.probeLoop(routeProbeInterval, routeProbeTimeout)
	}
	f.mu.Unlock()
	for _, c := range old {
		c.Close()
	}
	return nil
}

// probeLoop 在走备用线路期间定时探测首选线路，只记结果（preferredBack），不自动切换。
// 行为与主机线路的 probeLoop 一致；间隔和超时由启动它的人传入（不在协程里读包级变量）。
func (f *forwardSvc) probeLoop(interval, timeout time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
		case <-f.ctx.Done():
			f.mu.Lock()
			f.probing = false
			f.mu.Unlock()
			return
		}
		f.mu.Lock()
		if f.active == 0 {
			f.probing = false // 清标记与判断同在一把锁里，switchRoute 不会漏起新协程
			f.mu.Unlock()
			return
		}
		addr, d := f.routes[0].Addr, f.dialers[0]
		f.mu.Unlock()

		ctx, cancel := context.WithTimeout(f.ctx, timeout)
		c, err := d.DialContext(ctx, "tcp", addr)
		cancel()
		if err == nil {
			c.Close() // 只测 TCP 能不能连上
		}
		f.mu.Lock()
		if f.active != 0 {
			f.preferredBack = err == nil
		}
		f.mu.Unlock()
	}
}

// item 是给 /forwards 的状态快照。
func (f *forwardSvc) item() forwardItem {
	f.mu.Lock()
	defer f.mu.Unlock()
	views := make([]routeView, len(f.routes))
	for i, r := range f.routes {
		views[i] = routeView{r.Label, r.Tunnel, r.Addr, r.Via}
	}
	return forwardItem{f.name, f.addr, f.routes[f.active].Tunnel, f.connected, f.lastErr,
		routeState{views, f.active, f.preferredDown, f.preferredBack}}
}

// Close 停监听、作废在途拨号并关掉所有已转发的连接；可重复调用。
func (f *forwardSvc) Close() error {
	f.cancel()
	f.mu.Lock()
	f.closed = true
	ln := f.ln
	conns := make([]net.Conn, 0, len(f.conns))
	for c := range f.conns {
		conns = append(conns, c)
	}
	f.conns = map[net.Conn]struct{}{}
	f.mu.Unlock()
	if ln != nil {
		ln.Close()
	}
	for _, c := range conns {
		c.Close()
	}
	return nil
}
