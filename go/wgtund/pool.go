package wgtund

import (
	"context"
	"errors"
	"fmt"
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

// add 登记一台主机；d 是它的 Dialer（隧道或直连）。
func (p *sshPool) add(h hostConf, d sshterm.Dialer) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hosts[h.Name] = &hostConn{h: h, dialer: d, pool: p}
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

// session 在共用连接上开终端会话；开失败时丢掉旧连接重拨一次再试
// （调用方 ctx 已取消的不算，直接返回原错误）。
func (p *sshPool) session(ctx context.Context, host, command string, cols, rows int) (*sshterm.Session, error) {
	c, err := p.conn(host)
	if err != nil {
		return nil, err
	}
	s, err := c.OpenSession(ctx, command, cols, rows)
	if err == nil {
		return s, nil
	}
	if ctx.Err() != nil {
		return nil, err
	}
	p.drop(host, c)
	c2, err := p.conn(host)
	if err != nil {
		return nil, err
	}
	return c2.OpenSession(ctx, command, cols, rows)
}

// run 在共用连接上执行短命令（超时 runTimeout）。
// Run 返回非 ExitError 的错误时丢掉旧连接重拨一次再试
// （ExitError 说明命令真跑了、只是退出码非零，不是连接问题）。
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
	var ee *sshterm.ExitError
	if errors.As(err, &ee) || ctx.Err() != nil {
		return nil, err
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
	h      hostConf
	dialer sshterm.Dialer
	pool   *sshPool

	mu        sync.Mutex
	client    *sshterm.Client
	inflight  *dialCall
	connected bool
	lastErr   string
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
	e.mu.Unlock()

	c, err := e.dial()

	e.mu.Lock()
	e.inflight = nil
	if err != nil {
		e.connected = false
		e.lastErr = err.Error()
	} else {
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

// dial 拨一条新连接；私钥每次拨时重新加载（agent 连接握手完就关）。
func (e *hostConn) dial() (*sshterm.Client, error) {
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
	return sshterm.Dial(ctx, e.dialer,
		sshterm.Target{Addr: e.h.Addr, User: e.h.User},
		sshterm.Auth{Signers: signers, HostKeyCallback: e.pool.knownHosts})
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
}

// drop 丢掉 c 指向的连接（c 为 nil 时丢掉当前缓存的任意一条）。
func (e *hostConn) drop(c *sshterm.Client) {
	e.mu.Lock()
	cur := e.client
	if c == nil || cur == c {
		e.client = nil
	}
	e.mu.Unlock()
	if cur != nil {
		cur.Close()
	}
}
