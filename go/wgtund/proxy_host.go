// proxy_host.go：代理口的「主机代号」——CONNECT 目的的主机部分正好是 hosts 里某台主机的名字（如 ubuntu:22）、
// 又没有任何转发的 target 规则命中时，按这台主机**当前选中的线路**换成该线路 addr 的 IP，端口用请求里的，
// 经这条线路的隧道（或直连）拨过去。程序里只写代号，用户在页面上切主机线路，新连接就跟着走新线路。
//
//   - 优先级：转发的 target 规则（含别名）> 主机代号 > proxy_default；要让某个端口走别的路（比如服务只听回环、
//     要经 SSH 的 via 转发），照旧给那个 代号:端口 写一条转发 target 即可；
//   - 不回落别的线路：线路由用户选，拨不通就是拨不通（首选拨不通照主机的规矩标 preferred_down）；
//   - 切换主机线路时，经代号建立的连接一起关掉（和终端会话一样），调用方重连就走新线路；
//   - 只管 TCP（CONNECT）；UDP 对代号不做特殊处理。流量计入这台主机。
package wgtund

import (
	"context"
	"fmt"
	"io"
	"net"
)

// hasHost 报告 name 是不是 hosts 里的主机名（大小写敏感，和配置里写的一致）。
func (p *sshPool) hasHost(name string) bool {
	_, ok := p.entry(name)
	return ok
}

// dialCode 在主机当前线路上拨「线路 addr 的主机部分 + port」，超时 dialTimeout。返回拨号所属的代际 gen（交给 trackCode）。
func (p *sshPool) dialCode(ctx context.Context, host, port string) (net.Conn, int, error) {
	e, ok := p.entry(host)
	if !ok {
		return nil, 0, fmt.Errorf("未知主机 %q", host)
	}
	e.mu.Lock()
	idx, gen := e.active, e.gen
	r, d, multi := e.routes[idx], e.dialers[idx], len(e.routes) > 1
	e.mu.Unlock()

	ip, _, err := net.SplitHostPort(r.Addr)
	if err != nil || ip == "" {
		return nil, gen, fmt.Errorf("主机 %s 的线路 %s 地址 %q 不是 主机:端口", host, r.Label, r.Addr)
	}
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(ip, port))
	if err != nil {
		err = fmt.Errorf("主机 %s 线路 %s 拨 %s 失败：%w", host, r.Label, net.JoinHostPort(ip, port), err)
		sysLog.logThrottled("code:"+host, err.Error(), "%v", err)
		if idx == 0 && multi {
			e.mu.Lock()
			if e.active == idx {
				setFlagLog(&e.preferredDown, true, "preferred_down =", "主机 "+host)
			}
			e.mu.Unlock()
		}
		return nil, gen, err
	}
	if sysLog.clearThrottle("code:" + host) {
		logf("主机 %s 经代号拨号恢复（线路 %s）", host, r.Label)
	}
	return c, gen, nil
}

// trackCode 登记经代号建立的一对连接；拨号之后线路已切换（gen 变了）就关掉两边、返回 false。
func (p *sshPool) trackCode(host string, gen int, cs ...net.Conn) bool {
	e, ok := p.entry(host)
	if ok {
		e.mu.Lock()
		if e.gen == gen {
			if e.codeConns == nil {
				e.codeConns = map[net.Conn]struct{}{}
			}
			for _, c := range cs {
				e.codeConns[c] = struct{}{}
			}
			e.mu.Unlock()
			return true
		}
		e.mu.Unlock()
	}
	for _, c := range cs {
		c.Close()
	}
	return false
}

func (p *sshPool) untrackCode(host string, cs ...net.Conn) {
	e, ok := p.entry(host)
	if !ok {
		return
	}
	e.mu.Lock()
	for _, c := range cs {
		delete(e.codeConns, c)
	}
	e.mu.Unlock()
}

// takeCodeConnsLocked 取走并清空经代号建立的连接（调用方持 e.mu，放锁后再关）。
func (e *hostConn) takeCodeConnsLocked() []net.Conn {
	out := make([]net.Conn, 0, len(e.codeConns))
	for c := range e.codeConns {
		out = append(out, c)
	}
	e.codeConns = nil
	return out
}

// serveCode 处理目的是主机代号的 CONNECT：当前线路拨号 → 应答 → 双向拷贝。
func (p *proxySvc) serveCode(c net.Conn, host, port string) {
	up, gen, err := p.pool.dialCode(p.ctx, host, port)
	if err != nil {
		socksReply(c, dialRep(err))
		c.Close()
		return
	}
	if socksReply(c, repOK) != nil {
		up.Close()
		c.Close()
		return
	}
	if !p.pool.trackCode(host, gen, c, up) {
		return
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
	p.pool.untrackCode(host, c, up)
	c.Close()
	up.Close()
}
