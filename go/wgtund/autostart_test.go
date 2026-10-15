package wgtund

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/WillyChen0811/base/go/support"
)

type fakeStarter struct {
	mu      sync.Mutex
	failN   int // 前 failN 次 Start 失败
	calls   []int
	active  bool
	started chan struct{}
}

func (f *fakeStarter) Start(m int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, m)
	if len(f.calls) <= f.failN {
		return errors.New("连不上")
	}
	f.active = true
	if f.started != nil {
		close(f.started)
		f.started = nil
	}
	return nil
}

func (f *fakeStarter) Status() support.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return support.Status{Active: f.active}
}

func (f *fakeStarter) nCalls() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.calls) }

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("超时：%s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestAutostartSuccess(t *testing.T) {
	f := &fakeStarter{}
	a := startAutostart(f, time.Hour, 0)
	a.Close() // 等 goroutine 退出（成功路径本就会自行退出）
	if f.nCalls() != 1 || f.calls[0] != 0 {
		t.Fatalf("应只调一次 Start(0): %v", f.calls)
	}
}

func TestAutostartRetryThenSuccess(t *testing.T) {
	f := &fakeStarter{failN: 2}
	a := startAutostart(f, 10*time.Millisecond, 0)
	waitFor(t, "重试后成功", func() bool { return f.Status().Active })
	a.Close()
	if n := f.nCalls(); n != 3 {
		t.Fatalf("应失败 2 次后第 3 次成功，得 %d 次", n)
	}
}

func TestAutostartManualStopPreventsRetry(t *testing.T) {
	f := &fakeStarter{failN: 1 << 30}
	a := startAutostart(f, 20*time.Millisecond, 0)
	waitFor(t, "至少失败一次", func() bool { return f.nCalls() >= 1 })
	a.markManual()
	<-a.done // 下一轮检查到 manual 后自行退出
	n := f.nCalls()
	time.Sleep(80 * time.Millisecond)
	if f.nCalls() != n {
		t.Fatalf("手动操作后不应再自动开: %d -> %d", n, f.nCalls())
	}
	a.Close()
}

func TestAutostartAlreadyActive(t *testing.T) {
	f := &fakeStarter{active: true}
	a := startAutostart(f, 10*time.Millisecond, 0)
	a.Close()
	if f.nCalls() != 0 {
		t.Fatalf("已在协助中不应再 Start: %v", f.calls)
	}
}

func TestAutostartCloseStopsGoroutine(t *testing.T) {
	f := &fakeStarter{failN: 1 << 30}
	a := startAutostart(f, time.Hour, 0) // 重试间隔很长：只有 Close 能让它退出
	waitFor(t, "首次失败", func() bool { return f.nCalls() >= 1 })
	done := make(chan struct{})
	go func() { a.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close 没让 goroutine 退出")
	}
	if f.nCalls() != 1 {
		t.Fatalf("Close 后不应再重试: %d", f.nCalls())
	}
}

func TestAutostartConfDefault(t *testing.T) {
	var c supportConf
	if !c.autostartOn() {
		t.Error("缺省应为 true")
	}
	for in, want := range map[string]bool{`{}`: true, `{"autostart":null}`: true, `{"autostart":true}`: true, `{"autostart":false}`: false} {
		var c supportConf
		if err := json.Unmarshal([]byte(in), &c); err != nil || c.autostartOn() != want {
			t.Errorf("%s: autostartOn=%v 期望 %v (%v)", in, c.autostartOn(), want, err)
		}
	}
}

// 端到端：不写 autostart 时 wgtund 启动后自动开成不限时；手动 stop 后不会再开。
func TestAutostartEndToEnd(t *testing.T) {
	old := autostartRetry
	autostartRetry = 20 * time.Millisecond
	t.Cleanup(func() { autostartRetry = old })
	f, lg, _ := supportFixture(t, func(s, _ map[string]any) { delete(s, "autostart") })
	st, _, _ := run(t, f.dir)
	cmd := lg.wait(t, "endpoint.json.tmp")
	if !strings.Contains(cmd, `"until":0`) {
		t.Fatalf("入口 JSON 的 until 应为 0: %s", cmd)
	}
	_, b := get(t, st, "/support", true)
	var ss struct {
		Active bool  `json:"active"`
		Until  int64 `json:"until"`
	}
	if json.Unmarshal(b, &ss) != nil || !ss.Active || ss.Until != 0 {
		t.Fatalf("应已自动开启且不限时: %s", b)
	}
	if resp, b := supportPost(t, st, "/support/stop", testOrigin); resp.StatusCode != 200 {
		t.Fatalf("stop: %d %s", resp.StatusCode, b)
	}
	time.Sleep(200 * time.Millisecond)
	_, b = get(t, st, "/support", true)
	if json.Unmarshal(b, &ss) != nil || ss.Active {
		t.Fatalf("手动 stop 后不应再自动开: %s", b)
	}
}

// autostart=false（fixture 默认）：不会自动开。
func TestAutostartDisabled(t *testing.T) {
	f, _, _ := supportFixture(t, nil)
	st, _, _ := run(t, f.dir)
	time.Sleep(150 * time.Millisecond)
	_, b := get(t, st, "/support", true)
	if strings.Contains(string(b), `"active":true`) {
		t.Fatalf("autostart=false 不应自动开: %s", b)
	}
}

// autostart_minutes=600：自动开启后 until 约为现在+600 分钟。
func TestAutostartMinutes(t *testing.T) {
	old := autostartRetry
	autostartRetry = 20 * time.Millisecond
	t.Cleanup(func() { autostartRetry = old })
	f, _, _ := supportFixture(t, func(s, _ map[string]any) { s["autostart"] = true; s["autostart_minutes"] = 600 })
	before := time.Now()
	st, _, _ := run(t, f.dir)
	var ss struct {
		Active bool  `json:"active"`
		Until  int64 `json:"until"`
	}
	waitFor(t, "自动开启", func() bool {
		_, b := get(t, st, "/support", true)
		return json.Unmarshal(b, &ss) == nil && ss.Active
	})
	want := before.Add(600 * time.Minute).Unix()
	if d := ss.Until - want; d < -5 || d > 30 {
		t.Fatalf("until=%d，期望约 %d（现在+600 分钟）", ss.Until, want)
	}
}

// startAutostart 把 minutes 原样交给 Start。
func TestAutostartPassesMinutes(t *testing.T) {
	f := &fakeStarter{}
	a := startAutostart(f, time.Hour, 600)
	defer a.Close()
	waitFor(t, "Start 被调用", func() bool { return f.nCalls() == 1 })
	if f.calls[0] != 600 {
		t.Fatalf("Start(%d)，期望 600", f.calls[0])
	}
}

// autostart_minutes 超出 0..600 时配置校验失败。
func TestAutostartMinutesInvalid(t *testing.T) {
	for _, m := range []int{601, -1} {
		f, _, _ := supportFixture(t, func(s, _ map[string]any) { s["autostart_minutes"] = m })
		err := runErr(t, f.dir)
		if err == nil || !strings.HasPrefix(err.Error(), "hosts.json: support") || !strings.Contains(err.Error(), "autostart_minutes") {
			t.Errorf("minutes=%d: 应返回 autostart_minutes 配置错误，得到 %v", m, err)
		}
	}
}
