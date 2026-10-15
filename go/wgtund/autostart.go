package wgtund

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/WillyChen0811/base/go/support"
)

// autostartRetry 是自动开协助失败后的重试间隔（测试里可改）。
var autostartRetry = 30 * time.Second

// supportStarter 是自动开协助用到的 support.Service 子集。
type supportStarter interface {
	Start(minutes int) error
	Status() support.Status
}

// autostarter 在后台把协助开成 minutes 分钟（0 不限时）：失败（如 Ubuntu 暂时连不上）按间隔重试，
// 直到成功、用户手动开/关过、或被 Close。手动操作之后本次运行内不再自动开。
type autostarter struct {
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
	manual atomic.Bool
}

// startAutostart 起后台 goroutine，开启时长 minutes 分钟（0 不限时）。retry<=0 时用 autostartRetry。
func startAutostart(svc supportStarter, retry time.Duration, minutes int) *autostarter {
	if retry <= 0 {
		retry = autostartRetry
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := &autostarter{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(a.done)
		for {
			if a.manual.Load() || svc.Status().Active {
				return
			}
			if err := svc.Start(minutes); err == nil {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(retry):
			}
		}
	}()
	return a
}

// markManual 记下用户手动开/关过协助：自动开从此放弃。
func (a *autostarter) markManual() {
	if a != nil {
		a.manual.Store(true)
	}
}

// Close 停掉后台 goroutine 并等它退出。
func (a *autostarter) Close() error {
	a.once.Do(a.cancel)
	<-a.done
	return nil
}
