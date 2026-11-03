package wgtund

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/WillyChen0811/base/go/sshterm"
	"golang.org/x/crypto/ssh"
)

const (
	dialTimeout = 15 * time.Second // 拨号超时
	runTimeout  = 10 * time.Second // 单个远端短命令超时
)

var (
	routeProbeInterval = 30 * time.Second // 走备用线路期间探测首选的间隔（测试里会改小）
	routeProbeTimeout  = 5 * time.Second  // 单次探测首选的超时
)

// routeError 是切换线路时的参数错误；notFound 表示主机不存在（HTTP 层回 404），否则是线路下标越界（400）。
type routeError struct {
	msg      string
	notFound bool
}

func (e *routeError) Error() string { return e.msg }

// routeView 是线路的对外展示（label 已补全）。
type routeView struct {
	Label  string `json:"label"`
	Tunnel string `json:"tunnel"`
	Addr   string `json:"addr"`
	Via    string `json:"via,omitempty"`
}

// routeState 是主机线路状态的快照，给 /hosts 用。
type routeState struct {
	Routes        []routeView `json:"routes"`
	Active        int         `json:"active"`
	PreferredDown bool        `json:"preferred_down"`
	PreferredBack bool        `json:"preferred_back"`
}

// sshPool 是主机 SSH 连接池：每台主机最多一条共用连接，第一次用到时才拨号。
// 多台主机的请求互不阻塞；同一主机的并发请求共用同一次拨号（singleflight）。
type sshPool struct {
	ctx        context.Context
	cancel     context.CancelFunc
	knownHosts ssh.HostKeyCallback

	mu    sync.Mutex
	hosts map[string]*hostConn
}

func newSSHPool(knownHosts ssh.HostKeyCallback) *sshPool {
	ctx, cancel := context.WithCancel(context.Background())
	return &sshPool{ctx: ctx, cancel: cancel, knownHosts: knownHosts, hosts: map[string]*hostConn{}}
}

// add 登记一台主机；dialers 与 hostRoutes(h) 一一对应（隧道或直连）。
func (p *sshPool) add(h hostConf, dialers []sshterm.Dialer) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hosts[h.Name] = &hostConn{h: h, routes: hostRoutes(h), dialers: dialers, pool: p}
}

// Close 关掉所有连接并让进行中的拨号放弃。可重复调用。
func (p *sshPool) Close() error {
	p.cancel()
	p.mu.Lock()
	entries := make([]*hostConn, 0, len(p.hosts))
	for _, e := range p.hosts {
		entries = append(entries, e)
	}
	p.mu.Unlock()
	for _, e := range entries {
		e.drop(nil)
		e.mu.Lock()
		cs := e.takeCodeConnsLocked()
		e.mu.Unlock()
		for _, c := range cs {
			c.Close()
		}
	}
	return nil
}

func (p *sshPool) entry(host string) (*hostConn, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.hosts[host]
	return e, ok
}

// info 返回主机当前状态，给 /hosts 用。
func (p *sshPool) info(host string) (connected bool, lastErr string) {
	e, ok := p.entry(host)
	if !ok {
		return false, ""
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.connected, e.lastErr
}

// routeInfo 返回主机当前的线路状态；未知主机返回空状态。
func (p *sshPool) routeInfo(host string) routeState {
	e, ok := p.entry(host)
	if !ok {
		return routeState{Routes: []routeView{}}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	views := make([]routeView, len(e.routes))
	for i, r := range e.routes {
		views[i] = routeView{r.Label, r.Tunnel, r.Addr, ""}
	}
	return routeState{views, e.active, e.preferredDown, e.preferredBack}
}

// switchRoute 把主机切到第 idx 条线路（idx 等于当前线路时相当于强制重连）。
// 旧连接会被关掉：终端会话随之结束，协助服务会自己经新线路重连。
func (p *sshPool) switchRoute(host string, idx int) error {
	e, ok := p.entry(host)
	if !ok {
		return &routeError{msg: fmt.Sprintf("未知主机 %q", host), notFound: true}
	}
	e.mu.Lock()
	if idx < 0 || idx >= len(e.routes) {
		e.mu.Unlock()
		return &routeError{msg: fmt.Sprintf("没有第 %d 条线路", idx)}
	}
	logf("主机 %s 切换线路 %s → %s", host, e.routes[e.active].Label, e.routes[idx].Label)
	e.active = idx
	e.gen++          // 在途拨号走的是旧线路：完成时作废
	e.inflight = nil // 之后的请求不再等旧线路的拨号，直接在新线路上拨
	e.preferredDown = false
	e.preferredBack = false
	old := e.client
	e.client = nil
	e.connected = false
	e.lastErr = ""
	if idx != 0 && len(e.routes) > 1 && !e.probing {
		e.probing = true
		go e.probeLoop(routeProbeInterval, routeProbeTimeout)
	}
	codeConns := e.takeCodeConnsLocked()
	e.mu.Unlock()
	if old != nil {
		old.Close()
	}
	for _, c := range codeConns {
		c.Close()
	}
	return nil
}

// dialActive 在当前线路上拨一条到主机地址的原始 TCP 连接（git 转发口用），超时 dialTimeout。
func (p *sshPool) dialActive(ctx context.Context, host string) (net.Conn, error) {
	e, ok := p.entry(host)
	if !ok {
		return nil, fmt.Errorf("未知主机 %q", host)
	}
	e.mu.Lock()
	idx := e.active
	addr, d, multi := e.routes[idx].Addr, e.dialers[idx], len(e.routes) > 1
	e.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil && idx == 0 && multi {
		e.mu.Lock()
		if e.active == idx {
			setFlagLog(&e.preferredDown, true, "preferred_down =", "主机 "+host)
		}
		e.mu.Unlock()
	}
	return c, err
}

// conn 取一条可用连接：缓存里没有（或已断）就拨号；并发的请求等同一次拨号的结果。
// 拿到的连接若 Done() 已关闭，丢掉重拨一次。
func (p *sshPool) conn(host string) (*sshterm.Client, error) {
	e, ok := p.entry(host)
	if !ok {
		return nil, fmt.Errorf("未知主机 %q", host)
	}
	c, err := e.conn()
	if err != nil {
		return nil, err
	}
	select {
	case <-c.Done():
		e.drop(c)
		return e.conn() // 只重拨这一次
	default:
		return c, nil
	}
}

// brokenGrace 是操作出错后等连接被标记断开的宽限：底层传输刚断时，
// 操作的错误可能比 Done 关闭早几毫秒到达。
const brokenGrace = 500 * time.Millisecond

// connBroken 判断出错后连接是否真断了（Done 已关闭，或宽限内关闭）。
func connBroken(c *sshterm.Client) bool {
	select {
	case <-c.Done():
		return true
	default:
	}
	t := time.NewTimer(brokenGrace)
	defer t.Stop()
	select {
	case <-c.Done():
		return true
	case <-t.C:
		return false
	}
}

// session 在共用连接上开终端会话；只有连接真断了（Done 已关闭）才丢掉重拨一次再试，
// 连接健康时开失败原样返回（比如远端没装 tmux，重拨多少次都一样）。
func (p *sshPool) session(ctx context.Context, host, command string, cols, rows int) (*sshterm.Session, error) {
	c, err := p.conn(host)
	if err != nil {
		return nil, err
	}
	s, err := c.OpenSession(ctx, command, cols, rows)
	if err == nil {
		return s, nil
	}
	if !connBroken(c) {
		return nil, err // 连接健康（含 ctx 取消）：不重试，原样返回
	}
	p.drop(host, c)
	c2, err := p.conn(host)
	if err != nil {
		return nil, err
	}
	return c2.OpenSession(ctx, command, cols, rows)
}

// run 在共用连接上执行短命令（超时 runTimeout）。
// 只有连接真断了（Done 已关闭）才丢掉重拨一次再试；
// 连接健康时的错误（ExitError、超时、输出超限等）原样返回，不重拨。
func (p *sshPool) run(ctx context.Context, host, command string) ([]byte, error) {
	c, err := p.conn(host)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()
	out, err := c.Run(ctx, command)
	if err == nil {
		return out, nil
	}
	// 远端命令跑完了只是退出码非零（会话已存在/不存在等）、或输出超限：连接肯定是好的，不必等宽限
	var ee *sshterm.ExitError
	if errors.As(err, &ee) || errors.Is(err, sshterm.ErrOutputTooLarge) || !connBroken(c) {
		return nil, err // 连接健康：不重试，原样返回
	}
	p.drop(host, c)
	c2, err := p.conn(host)
	if err != nil {
		return nil, err
	}
	return c2.Run(ctx, command)
}

// drop 丢掉指定主机的缓存连接（只当它还指向 c 时）。
func (p *sshPool) drop(host string, c *sshterm.Client) {
	if e, ok := p.entry(host); ok {
		e.drop(c)
	}
}

// hostConn 管一台主机：一条共用连接、至多一次在途拨号，外加给 /hosts 的状态。
type hostConn struct {
	h       hostConf
	routes  []routeConf
	dialers []sshterm.Dialer // 与 routes 一一对应
	pool    *sshPool

	mu        sync.Mutex
	client    *sshterm.Client
	inflight  *dialCall
	connected bool
	lastErr   string

	// 多线路状态（都受 mu 保护）
	active        int  // 当前线路下标，启动总是 0
	preferredDown bool // 首选线路连不上（只标记，不自动切换）
	preferredBack bool // 走备用期间探测到首选已恢复（只标记，由用户决定是否切回）
	probing       bool // 已有探测协程在跑
	gen           int  // 每次切换线路加一，用来作废切换前发起的拨号
	dialed        bool // 拨过号了（再拨记为重拨）

	codeConns map[net.Conn]struct{} // 经代理口「主机代号」建立的连接（见 proxy_host.go），切换线路时一起关
}

// dialCall 是一次在途拨号，等它的人从 done 读同一个结果。
type dialCall struct {
	done   chan struct{}
	client *sshterm.Client
	err    error
}

func (e *hostConn) conn() (*sshterm.Client, error) {
	e.mu.Lock()
	if c := e.client; c != nil {
		select {
		case <-c.Done():
			e.recordDeadLocked(c)
		default:
			e.mu.Unlock()
			return c, nil
		}
	}
	if call := e.inflight; call != nil {
		e.mu.Unlock()
		select {
		case <-call.done:
			return call.client, call.err
		case <-e.pool.ctx.Done():
			return nil, errors.New("助手正在退出")
		}
	}
	call := &dialCall{done: make(chan struct{})}
	e.inflight = call
	idx, gen := e.active, e.gen
	label, redial := e.routes[idx].Label, e.dialed
	e.dialed = true
	e.mu.Unlock()

	if redial {
		logf("主机 %s 重拨（线路 %s）", e.h.Name, label)
	} else {
		logf("主机 %s 开始拨号（线路 %s）", e.h.Name, label)
	}
	c, err := e.dial(idx)

	e.mu.Lock()
	if e.inflight == call { // 切换线路时 switchRoute 已把它清掉，新拨号可能已经占了这个位置
		e.inflight = nil
	}
	if e.gen != gen {
		// 拨号期间用户切了线路：这条连接走的是旧线路，不能留
		e.mu.Unlock()
		if c != nil {
			c.Close()
		}
		err = errors.New("线路已切换，请重试")
		call.client, call.err = nil, err
		close(call.done)
		return nil, err
	}
	if err != nil {
		e.connected = false
		e.lastErr = err.Error()
		sysLog.logThrottled("host:"+e.h.Name, err.Error(), "主机 %s 拨号失败（线路 %s）：%v", e.h.Name, label, err)
		if idx == 0 && len(e.routes) > 1 {
			setFlagLog(&e.preferredDown, true, "preferred_down =", "主机 "+e.h.Name)
		}
	} else {
		sysLog.clearThrottle("host:" + e.h.Name)
		logf("主机 %s 已连上（线路 %s）", e.h.Name, label)
		if idx == 0 {
			setFlagLog(&e.preferredDown, false, "preferred_down =", "主机 "+e.h.Name)
		}
		e.client = c
		e.connected = true
		e.lastErr = ""
		go e.watch(c)
	}
	e.mu.Unlock()

	call.client, call.err = c, err
	close(call.done)
	return c, err
}

// dial 在第 idx 条线路上拨一条新连接；私钥每次拨时重新加载（agent 连接握手完就关）。
func (e *hostConn) dial(idx int) (*sshterm.Client, error) {
	signers, agentConn, keyErrs := loadSigners(e.h.IdentityFiles)
	if agentConn != nil {
		defer agentConn.Close()
	}
	if len(signers) == 0 {
		if len(keyErrs) > 0 {
			return nil, fmt.Errorf("没有可用的私钥：%s", strings.Join(keyErrs, "；"))
		}
		return nil, errors.New("没有可用的私钥（identity_files 为空，ssh-agent 也没有密钥）")
	}
	ctx, cancel := context.WithTimeout(e.pool.ctx, dialTimeout)
	defer cancel()
	return sshterm.Dial(ctx, e.dialers[idx],
		sshterm.Target{Addr: e.routes[idx].Addr, User: e.h.User},
		sshterm.Auth{Signers: signers, HostKeyCallback: e.pool.knownHosts})
}

// probeLoop 在走备用线路期间定时探测首选线路，只记结果（preferredBack），不自动切换。
// 间隔和超时由启动它的人传入（不在协程里读包级变量）。同一主机最多一个；回到首选或助手退出时清掉标记并结束。
func (e *hostConn) probeLoop(interval, timeout time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
		case <-e.pool.ctx.Done():
			e.mu.Lock()
			e.probing = false
			e.mu.Unlock()
			return
		}
		e.mu.Lock()
		if e.active == 0 {
			e.probing = false // 清标记与判断同在一把锁里，switchRoute 不会漏起新协程
			e.mu.Unlock()
			return
		}
		addr, d := e.routes[0].Addr, e.dialers[0]
		e.mu.Unlock()

		ctx, cancel := context.WithTimeout(e.pool.ctx, timeout)
		c, err := d.DialContext(ctx, "tcp", addr)
		cancel()
		if err == nil {
			c.Close() // 只测 TCP 能不能连上
		}
		e.mu.Lock()
		if e.active != 0 {
			setFlagLog(&e.preferredBack, err == nil, "preferred_back =", "主机 "+e.h.Name)
		}
		e.mu.Unlock()
	}
}

// watch 盯着连接断开，把状态记下来给 /hosts 用。
func (e *hostConn) watch(c *sshterm.Client) {
	<-c.Done()
	e.mu.Lock()
	if e.client == c {
		e.recordDeadLocked(c)
	}
	e.mu.Unlock()
}

// recordDeadLocked 记下连接已断（调用方持锁）。
func (e *hostConn) recordDeadLocked(c *sshterm.Client) {
	e.client = nil
	e.connected = false
	if err := c.Err(); err != nil {
		e.lastErr = err.Error()
	} else {
		e.lastErr = "连接已断开"
	}
	logf("主机 %s 断开（线路 %s）：%s", e.h.Name, e.routes[e.active].Label, e.lastErr)
}

// drop 丢掉 c 指向的连接（c 为 nil 时丢掉当前缓存的任意一条）。
// 只有缓存里还是 c（或没指向别的）才把它从缓存摘掉；
// 若缓存已经换成别人刚重拨的新连接，绝不能关那条新连接，只关 c 自己（幂等）。
func (e *hostConn) drop(c *sshterm.Client) {
	e.mu.Lock()
	cur := e.client
	match := c == nil || cur == c
	if match {
		e.client = nil
	}
	e.mu.Unlock()
	if match && cur != nil {
		cur.Close()
	}
	if c != nil && !match {
		c.Close() // 旧连接自己也关（幂等）
	}
	if c != nil && match && cur != nil {
		logf("主机 %s 丢弃连接（操作出错，下次使用时重拨）", e.h.Name)
	}
}
