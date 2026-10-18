// forward_dial.go：转发的 TCP 拨号——先拨当前线路，拨不通本次连接按 routes 顺序试其余线路（listen 口与代理口共用）。
package wgtund

import (
	"context"
	"errors"
	"net"
)

// dial 在当前线路上拨到线路的远端 addr；dialTo 可指定另一个目的（proxy 口用：线路只决定怎么出去）。
func (f *forwardSvc) dial() (net.Conn, int, error) { return f.dialTo("") }

// dialTo 在当前线路上拨到 dest（空则用线路自己的 addr；超时 dialTimeout），成败都记进状态给 /forwards；
// 拨号期间用户切了线路（gen 变了）就把这条旧线路的连接关掉（同主机 conn 的防护）。
// 当前线路拨不通（不可达/拒绝/超时）时，本次连接按 routes 顺序试其余线路，选中的线路不变、状态仍记当前线路的失败；
// 每条线路各自有 dialTimeout，不另加总时限。
// 同时返回拨号所属的代际 gen，交给 track 在同一把锁里验过再登记——堵住「dial 已返回、
// track 未登记」的窗口（那期间 switchRoute 关不到这条还没进集合的连接）。
func (f *forwardSvc) dialTo(dest string) (net.Conn, int, error) {
	f.mu.Lock()
	idx, gen := f.active, f.gen
	f.mu.Unlock()

	c, err := f.dialRoute(idx, dest)

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
		sysLog.logThrottled("fwd:"+f.name, f.lastErr, "转发 %s 拨号失败（线路 %s）：%v", f.name, f.routes[idx].Label, err)
		if idx == 0 && len(f.routes) > 1 {
			setFlagLog(&f.preferredDown, true, "preferred_down =", "转发 "+f.name)
		}
	} else {
		f.lastErr = ""
		if sysLog.clearThrottle("fwd:" + f.name) {
			logf("转发 %s 拨号恢复（线路 %s）", f.name, f.routes[idx].Label)
		}
		if idx == 0 {
			setFlagLog(&f.preferredDown, false, "preferred_down =", "转发 "+f.name)
		}
	}
	f.mu.Unlock()
	if err != nil && c != nil {
		c.Close() // 自写 Dialer 万一返回了半连接也别漏
	}
	if err != nil && f.ctx.Err() == nil {
		return f.dialFallback(idx, gen, dest, err)
	}
	return c, gen, err
}

// dialRoute 在第 idx 条线路上拨一次（dest 空则用该线路的 addr），不碰状态。
func (f *forwardSvc) dialRoute(idx int, dest string) (net.Conn, error) {
	addr := dest
	if addr == "" {
		addr = f.routes[idx].Addr
	}
	ctx, cancel := context.WithTimeout(f.ctx, dialTimeout)
	defer cancel()
	return f.dialers[idx].DialContext(ctx, "tcp", addr)
}

// dialFallback 在 failed 线路拨不通后，按 routes 顺序试其余线路；都不通返回最初的错误 first。
// 只管这一次连接：不改选中的线路、不动 connected/lastErr。拨成后若期间线路被切换（gen 变了）则作废。
func (f *forwardSvc) dialFallback(failed, gen int, dest string, first error) (net.Conn, int, error) {
	for j := range f.routes {
		if j == failed {
			continue
		}
		c, err := f.dialRoute(j, dest)
		if err != nil {
			if c != nil {
				c.Close()
			}
			if f.ctx.Err() != nil {
				break
			}
			continue
		}
		if f.curGen() != gen {
			c.Close()
			return nil, gen, errors.New("线路已切换，请重试")
		}
		f.mu.Lock()
		la, lb := f.routes[failed].Label, f.routes[j].Label
		f.mu.Unlock()
		logf("转发 %s 线路 %s 拨号失败，本次改走 %s", f.name, la, lb)
		return c, gen, nil
	}
	return nil, gen, first
}
