// traffic.go：流量统计——按主机连接、按转发各记上行/下行字节，每秒采样进内存环形缓冲（不落盘）。
// 计数挂在拨号器上：拨出的每条 net.Conn 外包一层，Read/Write 里只做 atomic 加法，热路径无锁。
// 只记数和留曲线，不做任何限速、告警或禁用。
package wgtund

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/WillyChen0811/base/go/sshterm"
)

const (
	trafficKeep   = 600 // 环形缓冲点数（每秒一个，共 10 分钟）
	trafficWindow = 3   // bps 取最近几个采样点的平均
)

// trafficView 是 /hosts、/forwards 每项里的 traffic 字段；bps 的单位是 字节/秒。
type trafficView struct {
	UpBps     int64 `json:"up_bps"`
	DownBps   int64 `json:"down_bps"`
	UpTotal   int64 `json:"up_total"`
	DownTotal int64 `json:"down_total"`
}

// trafficPoint 是曲线上的一个点：t 为采样时刻的 Unix 秒，bps 是那一秒的字节数。
type trafficPoint struct {
	T       int64 `json:"t"`
	UpBps   int64 `json:"up_bps"`
	DownBps int64 `json:"down_bps"`
}

// trafficCounter 是一个对象（主机或转发）的计数器加采样环。up = 本机发往远端，down = 远端发来。
type trafficCounter struct {
	up, down atomic.Int64

	mu       sync.Mutex
	ring     [trafficKeep]trafficPoint
	n        int // 已写入的点总数；最新点在 (n-1)%trafficKeep
	prevUp   int64
	prevDown int64
}

func newTrafficCounter() *trafficCounter { return &trafficCounter{} }

// sample 记一个点：与上次采样的差值就是这一秒的字节数。
func (c *trafficCounter) sample(now time.Time) {
	u, d := c.up.Load(), c.down.Load()
	c.mu.Lock()
	c.ring[c.n%trafficKeep] = trafficPoint{now.Unix(), u - c.prevUp, d - c.prevDown}
	c.n++
	c.prevUp, c.prevDown = u, d
	c.mu.Unlock()
}

// view 给 /hosts、/forwards：累计取实时值，bps 取最近 trafficWindow 个采样点的平均（不足则有几个取几个）。
func (c *trafficCounter) view() trafficView {
	v := trafficView{UpTotal: c.up.Load(), DownTotal: c.down.Load()}
	pts := c.points(trafficWindow)
	if len(pts) == 0 {
		return v
	}
	var u, d int64
	for _, p := range pts {
		u += p.UpBps
		d += p.DownBps
	}
	v.UpBps, v.DownBps = u/int64(len(pts)), d/int64(len(pts))
	return v
}

// points 返回最近 n 个点（旧到新）；不足 n 个有几个给几个。
func (c *trafficCounter) points(n int) []trafficPoint {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n > trafficKeep {
		n = trafficKeep
	}
	if n > c.n {
		n = c.n
	}
	out := make([]trafficPoint, n)
	for i := 0; i < n; i++ {
		out[i] = c.ring[(c.n-n+i)%trafficKeep]
	}
	return out
}

// countConn 包一条连接统计字节：Write 记上行，Read 记下行。
type countConn struct {
	net.Conn
	c *trafficCounter
}

func (cc *countConn) Read(p []byte) (int, error) {
	n, err := cc.Conn.Read(p)
	if n > 0 {
		cc.c.down.Add(int64(n))
	}
	return n, err
}

func (cc *countConn) Write(p []byte) (int, error) {
	n, err := cc.Conn.Write(p)
	if n > 0 {
		cc.c.up.Add(int64(n))
	}
	return n, err
}

// countConnCW 给底层支持 CloseWrite（TCP 半关闭）的连接用：包装后仍要保留这个能力，转发靠它做半关闭。
type countConnCW struct{ *countConn }

func (cc countConnCW) CloseWrite() error {
	return cc.Conn.(interface{ CloseWrite() error }).CloseWrite()
}

func wrapCount(conn net.Conn, c *trafficCounter) net.Conn {
	cc := &countConn{conn, c}
	if _, ok := conn.(interface{ CloseWrite() error }); ok {
		return countConnCW{cc}
	}
	return cc
}

// countDialer 包一个拨号器，拨出的连接都记到 c 上。
type countDialer struct {
	d sshterm.Dialer
	c *trafficCounter
}

func (cd *countDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	conn, err := cd.d.DialContext(ctx, network, addr)
	if err != nil || conn == nil {
		return conn, err
	}
	return wrapCount(conn, cd.c), nil
}

// trafficHub 登记所有被统计的对象，并驱动每秒采样。主机和转发各一张表（名字空间互相独立）。
type trafficHub struct {
	mu       sync.Mutex
	hosts    map[string]*trafficCounter
	forwards map[string]*trafficCounter
}

func newTrafficHub() *trafficHub {
	return &trafficHub{hosts: map[string]*trafficCounter{}, forwards: map[string]*trafficCounter{}}
}

// host 取（没有则建）某主机的计数器。
func (h *trafficHub) host(name string) *trafficCounter { return h.get(h.hosts, name, nil) }

// forward 登记某转发自带的计数器。
func (h *trafficHub) forward(name string, c *trafficCounter) { h.get(h.forwards, name, c) }

func (h *trafficHub) get(m map[string]*trafficCounter, name string, set *trafficCounter) *trafficCounter {
	h.mu.Lock()
	defer h.mu.Unlock()
	if set != nil {
		m[name] = set
	}
	if m[name] == nil {
		m[name] = newTrafficCounter()
	}
	return m[name]
}

// tick 让所有计数器采一个点；测试直接调它，不依赖真实时钟。
func (h *trafficHub) tick(now time.Time) {
	h.mu.Lock()
	all := make([]*trafficCounter, 0, len(h.hosts)+len(h.forwards))
	for _, c := range h.hosts {
		all = append(all, c)
	}
	for _, c := range h.forwards {
		all = append(all, c)
	}
	h.mu.Unlock()
	for _, c := range all {
		c.sample(now)
	}
}

// run 每秒 tick 一次，ctx 结束退出。
func (h *trafficHub) run(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case now := <-t.C:
			h.tick(now)
		case <-ctx.Done():
			return
		}
	}
}

// find 按 kind（host|forward|空）和名字找计数器；kind 为空时先主机后转发。
func (h *trafficHub) find(kind, name string) (string, *trafficCounter) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if kind == "" || kind == "host" {
		if c, ok := h.hosts[name]; ok {
			return "host", c
		}
	}
	if kind == "" || kind == "forward" {
		if c, ok := h.forwards[name]; ok {
			return "forward", c
		}
	}
	return "", nil
}
