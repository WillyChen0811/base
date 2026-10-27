package wgtund

// projfiles_switch_test.go：评审2 修复 1/2/4 的行为测试——
//   1. 改了设置文件里的 work_dir/sync_dir：下一轮 acquire 之后整套重建（搬运器、调度器都换成新目录那套）；
//      重建之前 unit/start、unit/stop、unit/create、/fresh 一律拒绝（含「改了设置但 tick 还没跑到」的窗口）；
//   2. 首次 build 并发（tick 与 getFresh 同时）只建出一套（在测试机 -race 下跑）；
//   4. unit/stop 在单元锁被「取最新」占住时等锁、不同时进行（用调度器导出的同一把锁验证）。
// 搬运器一律用桩（Linux 上真搬运器不支持），验证到「交给搬运器的目录」这一层。

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/WillyChen0811/base/go/projsync"
)

// pfRecMovers 记下每次 build 交出来的目录与搬运器实例（每次都给新实例，用来验「换掉了旧的」）。
type pfRecMovers struct {
	mu    sync.Mutex
	cfgs  []projsync.PFUnitMoveConfig
	ms    []*projsync.PFUnitMover
	delay time.Duration
}

func (r *pfRecMovers) build(cfg projsync.PFUnitMoveConfig) (*projsync.PFUnitMover, error) {
	if r.delay > 0 {
		time.Sleep(r.delay)
	}
	m := &projsync.PFUnitMover{}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cfgs = append(r.cfgs, cfg)
	r.ms = append(r.ms, m)
	return m, nil
}

func (r *pfRecMovers) n() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.cfgs)
}

func (r *pfRecMovers) cfg(i int) projsync.PFUnitMoveConfig {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cfgs[i]
}

func (r *pfRecMovers) mover(i int) *projsync.PFUnitMover {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ms[i]
}

// busyReason 解 busy JSON 的 reason；不是 busy 应答返回 ("", false)。
func busyReason(b []byte) (string, bool) {
	var v struct {
		Busy   bool   `json:"busy"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(b, &v); err != nil || !v.Busy {
		return "", false
	}
	return v.Reason, true
}

// 修复 1：改设置里的 sync_dir → 重建前所有写接口拒绝；下一轮整套重建；之后 unit/start 用新目录那套。
func TestProjfilesDirSwitchRebuild(t *testing.T) {
	api, svc, clock, dir := pfAPIT(t, &pfFakeFresh{}, nil)
	srv := pfServer(t, api)
	rec := &pfRecMovers{}
	svc.buildMover = rec.build
	rr := &pfRounds{}
	rr.hook(svc)
	var freshBuilds atomic.Int64
	fresh := &pfFakeFresh{}
	api.newFresh = func(*projsync.PFSched, string) (projfilesFresh, error) {
		freshBuilds.Add(1)
		return fresh, nil
	}
	ctx := context.Background()
	svc.setNet(projsync.PFRemoteNetGood, clock.now())
	svc.tick(ctx, "timer")

	oldSync := filepath.Join(dir, "sync")
	newSync := filepath.Join(dir, "sync2")
	if rec.n() != 1 || rec.cfg(0).SyncDir != oldSync {
		t.Fatalf("第一轮应建成、搬旧目录: %v", rec.cfg(0))
	}
	if st := svc.status(); st.ActiveSyncDir != oldSync || st.SwitchBusy {
		t.Fatalf("状态应报在用的旧目录: %+v", st)
	}
	if resp, _ := pfHTTPDo(t, srv.URL, http.MethodPost, "/projfiles/fresh", `{"paths":["/s/a"]}`, ""); resp.StatusCode != 200 {
		t.Fatalf("没改目录时 fresh 应能用: %d", resp.StatusCode)
	}
	if got := freshBuilds.Load(); got != 1 {
		t.Fatalf("fresh 第一次要建: %d", got)
	}

	// 改设置文件里的 sync_dir（模拟 Python 服务写了新设置）
	pfWriteSettings(t, svc.prep.settingsPath, map[string]any{
		"work_dir": filepath.Join(dir, "work"), "sync_dir": newSync, "recycle_days": 3,
	})
	// 重建前：三个单元操作一律拒绝，原因如实
	for _, tc := range []struct{ path, body string }{
		{"/projfiles/unit/start", `{"rel":"proj/a"}`},
		{"/projfiles/unit/stop", `{"code":"AB1"}`},
		{"/projfiles/unit/create", `{"rel":"proj/new"}`},
	} {
		resp, b := pfHTTPDo(t, srv.URL, http.MethodPost, tc.path, tc.body, "")
		why, ok := busyReason(b)
		if resp.StatusCode != http.StatusConflict || !ok || !strings.Contains(why, "刚改过") {
			t.Fatalf("%s 切换期应 409 说「刚改过正在切换」: %d %s", tc.path, resp.StatusCode, b)
		}
	}
	if resp, b := pfHTTPDo(t, srv.URL, http.MethodPost, "/projfiles/fresh", `{"paths":["/s/a"]}`, ""); resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(b), "刚改过") {
		t.Fatalf("切换期 fresh 应 503 说清楚: %d %s", resp.StatusCode, b)
	}
	if st := svc.status(); st.ActiveSyncDir != oldSync {
		t.Errorf("重建前状态报的必须还是实际在用的旧目录: %+v", st)
	}
	if svc.isRunning() {
		t.Error("拒绝不该占住轮次")
	}

	// 下一轮：acquire 之后整套重建
	svc.tick(ctx, "timer")
	if rec.n() != 2 || rec.cfg(1).SyncDir != newSync {
		t.Fatalf("第二轮必须用新目录重建搬运器: %+v", rec.cfg(1))
	}
	if svc.moverNow() != rec.mover(1) {
		t.Error("发布出来的必须是新目录那套搬运器")
	}
	if st := svc.status(); st.ActiveSyncDir != newSync || st.SwitchBusy {
		t.Fatalf("重建后状态要报新目录、切换期结束: %+v", st)
	}

	// 之后 unit/start 不再被拒，交给的是新目录那套搬运器
	resp, b := pfHTTPDo(t, srv.URL, http.MethodPost, "/projfiles/unit/start", `{"rel":"proj/a"}`, "")
	if resp.StatusCode == http.StatusConflict {
		t.Fatalf("重建后 unit/start 不该还被切换期挡住: %d %s", resp.StatusCode, b)
	}
	if why, ok := busyReason(b); ok && strings.Contains(why, "刚改过") {
		t.Fatalf("不该再说切换: %s", b)
	}
	// 缓存的 fresh 已随代次作废，绑的是重建后的新调度器
	if resp, b := pfHTTPDo(t, srv.URL, http.MethodPost, "/projfiles/fresh", `{"paths":["/s/a"]}`, ""); resp.StatusCode != 200 {
		t.Fatalf("重建后 fresh 要能用: %d %s", resp.StatusCode, b)
	}
	if got := freshBuilds.Load(); got != 2 {
		t.Fatalf("重建后 fresh 必须重新建一套（不能再绑旧的）: %d", got)
	}
}

// 修复 2：首次 build 并发（tick 与 getFresh 同时进来）只建出一套。
func TestProjfilesConcurrentBuildOneSet(t *testing.T) {
	api, svc, clock, _ := pfAPIT(t, nil, nil)
	rec := &pfRecMovers{delay: 30 * time.Millisecond}
	svc.buildMover = rec.build
	rr := &pfRounds{}
	rr.hook(svc)
	var freshBuilds atomic.Int64
	api.newFresh = func(*projsync.PFSched, string) (projfilesFresh, error) {
		freshBuilds.Add(1)
		return &pfFakeFresh{}, nil
	}
	svc.setNet(projsync.PFRemoteNetGood, clock.now())
	ctx := context.Background()

	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; svc.tick(ctx, "timer") }()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := api.getFresh(); err != nil {
				t.Errorf("并发里 getFresh 不该失败: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if rec.n() != 1 {
		t.Fatalf("并发 build 只该建一套搬运器，建了 %d 套", rec.n())
	}
	if got := freshBuilds.Load(); got != 1 {
		t.Fatalf("并发里 fresh 只该建一次: %d", got)
	}
	if got := svc.builtGen.Load(); got != 1 {
		t.Fatalf("只发布了一代: %d", got)
	}
	if svc.moverNow() != rec.mover(0) {
		t.Error("并发等待者必须复用同一套")
	}
}

// 修复 4（wgtund 侧）：单元锁被占（模拟「取最新」在线程里跑，PFFresh.online 拿的就是同一把调度器单元锁）时，
// unit/stop 必须等锁、不同时进行；松锁后才轮到它。
func TestProjfilesUnitWaitsUnitLock(t *testing.T) {
	api, svc, clock, _ := pfAPIT(t, nil, nil)
	srv := pfServer(t, api)
	rr := &pfRounds{}
	rr.hook(svc)
	ctx := context.Background()
	svc.setNet(projsync.PFRemoteNetGood, clock.now())
	svc.tick(ctx, "timer")
	sched := svc.getSched()
	if sched == nil {
		t.Fatal("tick 之后调度器应已建起来")
	}
	unlock, ok := sched.LockUnit(ctx, "AB1", 0)
	if !ok {
		t.Fatal("拿单元锁")
	}
	done := make(chan struct{})
	var code int
	var body []byte
	go func() {
		defer close(done)
		var resp *http.Response
		resp, body = pfHTTPDo(t, srv.URL, http.MethodPost, "/projfiles/unit/stop", `{"code":"AB1"}`, "")
		code = resp.StatusCode
	}()
	select {
	case <-done:
		t.Fatalf("单元锁还被「取最新」占着，unit/stop 不该已经返回（会同时动同一个单元）: %s", body)
	case <-time.After(300 * time.Millisecond):
	}
	unlock()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("松锁后 unit/stop 必须能往下走")
	}
	if code == http.StatusConflict && strings.Contains(string(body), "单元锁") {
		t.Fatalf("应该等锁而不是直接拒: %d %s", code, body)
	}
	if svc.isRunning() {
		t.Error("单元操作结束后必须放开轮次")
	}
}
