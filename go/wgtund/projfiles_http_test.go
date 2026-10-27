package wgtund

// projfiles_http_test.go：项目文件同步本机接口的测试（T11b）——
// 令牌、Origin 放行、参数校验、忙、取最新（假 PFFresh）、单元轮次占用、api.json、每日回收清理只一次。
// 一律不连真机器：假 Runner、假 SFTP、假搬运器、假 PFFresh、假时钟。

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/WillyChen0811/base/go/projsync"
)

// ── 夹具 ───────────────────────────────────────────────

// pfHTTPDo 发一个请求：body 原样发（可以是坏 JSON，用来验 400）；origin 非空时带 Origin 头。
func pfHTTPDo(t *testing.T, base, method, path, body, origin string) (*http.Response, []byte) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, base+path, rd)
	if err != nil {
		t.Fatal(err)
	}
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

// pfFakeFresh 是假的「取最新」：记下调用与预算，返回备好的结果。
type pfFakeFresh struct {
	mu       sync.Mutex
	pathsN   int
	touchN   int
	budgets  []time.Duration
	paths    [][]string
	res      projsync.PFFreshResult
	codes    []string
	buildErr error
}

func (f *pfFakeFresh) Paths(ctx context.Context, paths []string, budget time.Duration) projsync.PFFreshResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pathsN++
	f.budgets = append(f.budgets, budget)
	f.paths = append(f.paths, append([]string(nil), paths...))
	return f.res
}

func (f *pfFakeFresh) Touched(paths []string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touchN++
	f.paths = append(f.paths, append([]string(nil), paths...))
	return f.codes
}

func (f *pfFakeFresh) calls() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pathsN, f.touchN
}

func (f *pfFakeFresh) lastBudget() time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.budgets) == 0 {
		return 0
	}
	return f.budgets[len(f.budgets)-1]
}

// pfAPIT 造一套本机接口：不连真机器，设置落在临时目录，work_dir/sync_dir 有默认（可被 mut 改）。
func pfAPIT(t *testing.T, fresh projfilesFresh, mut func(st map[string]any)) (*projfilesAPI, *projfilesSvc, *pfClock, string) {
	t.Helper()
	svc, clock, dir := pfSvcT(t, nil)
	pfWriteSettings(t, svc.prep.settingsPath, map[string]any{
		"work_dir": filepath.Join(dir, "work"), "sync_dir": filepath.Join(dir, "sync"),
		"recycle_days": 3,
	})
	if mut != nil {
		m := map[string]any{"work_dir": filepath.Join(dir, "work"), "sync_dir": filepath.Join(dir, "sync"), "recycle_days": 3}
		mut(m)
		pfWriteSettings(t, svc.prep.settingsPath, m)
	}
	// 搬运器换成不碰真目录的桩：这里只验接口的排队与返回，搬运本身有 projsync 的测试。
	svc.buildMover = func(projsync.PFUnitMoveConfig) (*projsync.PFUnitMover, error) {
		return &projsync.PFUnitMover{}, nil
	}
	// 设置装进服务（相当于每轮开始时的读设置）：状态接口报的是「已生效」的那份
	st, _ := loadProjfilesSettings(svc.prep.settingsPath)
	svc.applySettings(st, nil)
	rec := newProjfilesRecycler(svc)
	api := newProjfilesAPI(svc, rec)
	if fresh != nil {
		api.newFresh = func(*projsync.PFSched, string) (projfilesFresh, error) { return fresh, nil }
	}
	return api, svc, clock, dir
}

func pfServer(t *testing.T, api *projfilesAPI) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(api.localHandler(context.Background()))
	t.Cleanup(srv.Close)
	return srv
}

// ── 状态与网络上报 ─────────────────────────────────────

func TestProjfilesHTTPStatus(t *testing.T) {
	api, _, _, _ := pfAPIT(t, nil, nil)
	srv := pfServer(t, api)

	resp, b := pfHTTPDo(t, srv.URL, http.MethodGet, "/projfiles/status", "", "")
	if resp.StatusCode != 200 {
		t.Fatalf("GET status: %d %s", resp.StatusCode, b)
	}
	var st projfilesStatusView
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatalf("状态不是合法 JSON: %v %s", err, b)
	}
	if !st.Enabled || st.Host != "ubuntu" || st.RemoteRoot != "/srv/XXYI-sync" {
		t.Fatalf("状态字段不对: %+v", st)
	}
	if st.Settings.RecycleDays != 3 {
		t.Fatalf("应带上设置文件的 recycle_days: %+v", st.Settings)
	}
	if resp, _ := pfHTTPDo(t, srv.URL, http.MethodPost, "/projfiles/status", "", testOrigin); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /projfiles/status 应 405: %d", resp.StatusCode)
	}
	if resp, _ := pfHTTPDo(t, srv.URL, http.MethodGet, "/projfiles/nothing", "", ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("未知子路径应 404: %d", resp.StatusCode)
	}
}

func TestProjfilesHTTPNetReport(t *testing.T) {
	api, svc, clock, _ := pfAPIT(t, nil, nil)
	srv := pfServer(t, api)

	// 合法上报（不带 Origin 也放行：cmdproto 子进程调用只认令牌）
	resp, b := pfHTTPDo(t, srv.URL, http.MethodPost, "/projfiles/net", `{"net":"good"}`, "http://evil.example")
	if resp.StatusCode != 200 {
		t.Fatalf("上报 good 应 200: %d %s", resp.StatusCode, b)
	}
	if svc.effNet() != projsync.PFRemoteNetGood {
		t.Fatalf("setNet 没转出去: %s", svc.effNet())
	}
	// 带 at（RFC3339，用假时钟当下，免得过了有效期被判未知）
	at := clock.now().Format(time.RFC3339)
	if resp, b := pfHTTPDo(t, srv.URL, http.MethodPost, "/projfiles/net", `{"net":"expensive","at":"`+at+`"}`, ""); resp.StatusCode != 200 {
		t.Fatalf("带 at 应 200: %d %s", resp.StatusCode, b)
	}
	if svc.effNet() != projsync.PFRemoteNetExpensive {
		t.Fatalf("应记成 expensive: %s", svc.effNet())
	}
	for _, bad := range []string{`{"net":"wifi"}`, `{"net":""}`, `{"net":"GOOD"}`, `{"nope":1}`} {
		if resp, _ := pfHTTPDo(t, srv.URL, http.MethodPost, "/projfiles/net", bad, ""); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s 应 400: %d", bad, resp.StatusCode)
		}
	}
	if resp, _ := pfHTTPDo(t, srv.URL, http.MethodPost, "/projfiles/net", `{"net":"good","at":"昨天"}`, ""); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("at 不是 RFC3339 应 400: %d", resp.StatusCode)
	}
	if resp, _ := pfHTTPDo(t, srv.URL, http.MethodPost, "/projfiles/net", `{坏`, ""); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("坏 JSON 应 400: %d", resp.StatusCode)
	}
	if resp, _ := pfHTTPDo(t, srv.URL, http.MethodGet, "/projfiles/net", "", ""); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /projfiles/net 应 405: %d", resp.StatusCode)
	}
}

// ── 立即同步与忙 ───────────────────────────────────────

func TestProjfilesHTTPSyncBusy(t *testing.T) {
	api, svc, _, _ := pfAPIT(t, nil, nil)
	entered, gate := make(chan struct{}, 4), make(chan struct{})
	svc.runRound = func(ctx context.Context, opt projsync.PFSchedRoundOptions) *projsync.PFSchedReport {
		entered <- struct{}{}
		<-gate
		return &projsync.PFSchedReport{Outcome: projsync.PFSchedDone, Net: projsync.PFRemoteNetGood}
	}
	srv := pfServer(t, api)

	if resp, b := pfHTTPDo(t, srv.URL, http.MethodPost, "/projfiles/sync", "", ""); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("sync 应 202: %d %s", resp.StatusCode, b)
	}
	<-entered
	if !svc.isRunning() {
		t.Fatal("进了 runRound 就该占住轮次")
	}
	resp, b := pfHTTPDo(t, srv.URL, http.MethodPost, "/projfiles/sync", "", "")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("正在跑应 busy(409): %d %s", resp.StatusCode, b)
	}
	var busy map[string]any
	if err := json.Unmarshal(b, &busy); err != nil || busy["busy"] != true {
		t.Fatalf("busy 应答 {\"busy\":true}: %s", b)
	}
	gate <- struct{}{}
	pollUntil(t, 3*time.Second, "跑完", func() bool { return !svc.isRunning() })
	// 跑完之后可以再来一次
	if resp, _ := pfHTTPDo(t, srv.URL, http.MethodPost, "/projfiles/sync", "", ""); resp.StatusCode != http.StatusAccepted {
		t.Errorf("空下来再 sync 应 202: %d", resp.StatusCode)
	}
	<-entered
	gate <- struct{}{}
	// 等第二轮连状态落盘一起收尾，不然临时目录删除会跟写入打架
	pollUntil(t, 3*time.Second, "第二轮收尾", func() bool {
		s := svc.status()
		return !s.Running && strings.HasPrefix(s.LastAction, "跑了")
	})
	if resp, _ := pfHTTPDo(t, srv.URL, http.MethodGet, "/projfiles/sync", "", ""); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /projfiles/sync 应 405: %d", resp.StatusCode)
	}
}

// ── 取最新 / touched ───────────────────────────────────

func TestProjfilesHTTPFresh(t *testing.T) {
	fake := &pfFakeFresh{
		res: projsync.PFFreshResult{
			Items: []projsync.PFFreshItem{{Path: "/s/a/x.md", State: "aligned", Unit: "AB1"}, {Path: "/s/b", State: "stale"}},
			Note:  "对方更新，已拉回 1 个",
		},
		codes: []string{"AB1"},
	}
	var built int
	api, _, _, _ := pfAPIT(t, fake, nil)
	real := api.newFresh
	api.newFresh = func(s *projsync.PFSched, dir string) (projfilesFresh, error) {
		built++
		return real(s, dir)
	}
	srv := pfServer(t, api)

	resp, b := pfHTTPDo(t, srv.URL, http.MethodPost, "/projfiles/fresh", `{"paths":["/s/a/x.md","/s/b"]}`, "")
	if resp.StatusCode != 200 {
		t.Fatalf("fresh: %d %s", resp.StatusCode, b)
	}
	var res projsync.PFFreshResult
	if err := json.Unmarshal(b, &res); err != nil {
		t.Fatalf("结果不是 PFFreshResult: %v %s", err, b)
	}
	if len(res.Items) != 2 || res.Items[0].Path != "/s/a/x.md" || res.Items[1].State != "stale" || res.Note == "" {
		t.Fatalf("结果应原样返回: %s", b)
	}
	if got := fake.lastBudget(); got != projfilesFreshDefaultBudgetS*time.Second {
		t.Fatalf("缺省预算应 %ds，得到 %v", projfilesFreshDefaultBudgetS, got)
	}
	// 指定预算
	if resp, _ := pfHTTPDo(t, srv.URL, http.MethodPost, "/projfiles/fresh", `{"paths":["/s/a"],"budget_s":30}`, ""); resp.StatusCode != 200 {
		t.Errorf("budget_s=30: %d", resp.StatusCode)
	}
	if got := fake.lastBudget(); got != 30*time.Second {
		t.Fatalf("预算应是 30s，得到 %v", got)
	}
	// 上限 120
	if resp, _ := pfHTTPDo(t, srv.URL, http.MethodPost, "/projfiles/fresh", `{"paths":["/s/a"],"budget_s":120}`, ""); resp.StatusCode != 200 {
		t.Errorf("budget_s=120 应放行: %d", resp.StatusCode)
	}
	for _, bad := range []string{`{"paths":[]}`, `{"paths":["rel/x.md"]}`, `{"paths":["/ok",""]}`,
		`{"paths":["/ok"],"budget_s":0}`, `{"paths":["/ok"],"budget_s":-1}`, `{"paths":["/ok"],"budget_s":121}`, `{坏`} {
		if resp, _ := pfHTTPDo(t, srv.URL, http.MethodPost, "/projfiles/fresh", bad, ""); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s 应 400: %d", bad, resp.StatusCode)
		}
	}
	if n, _ := fake.calls(); n != 3 {
		t.Fatalf("合法调用应 3 次，得到 %d", n)
	}
	if built != 1 {
		t.Fatalf("PFFresh 只该建一次，建了 %d 次", built)
	}
	// touched
	resp, b = pfHTTPDo(t, srv.URL, http.MethodPost, "/projfiles/touched", `{"paths":["/s/a/x.md"]}`, "")
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"AB1"`) {
		t.Fatalf("touched: %d %s", resp.StatusCode, b)
	}
	if _, n := fake.calls(); n != 1 {
		t.Fatalf("touched 应调一次，得到 %d", n)
	}
	if resp, _ := pfHTTPDo(t, srv.URL, http.MethodPost, "/projfiles/touched", `{"paths":[]}`, ""); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("touched 空 paths 应 400: %d", resp.StatusCode)
	}
}

func TestProjfilesHTTPFreshNoSyncDir(t *testing.T) {
	api, _, _, _ := pfAPIT(t, &pfFakeFresh{}, func(m map[string]any) { delete(m, "sync_dir") })
	srv := pfServer(t, api)
	resp, b := pfHTTPDo(t, srv.URL, http.MethodPost, "/projfiles/fresh", `{"paths":["/s/a"]}`, "")
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(b), "sync_dir") {
		t.Fatalf("没有 sync_dir 应 503 说清楚: %d %s", resp.StatusCode, b)
	}
}

// ── 单元操作 ───────────────────────────────────────────

func TestProjfilesHTTPUnitBusy(t *testing.T) {
	api, svc, _, _ := pfAPIT(t, nil, nil)
	srv := pfServer(t, api)

	// 轮次被占（定时的一轮或别的单元操作）→ busy
	if !svc.acquire() {
		t.Fatal("夹具里应先占住轮次")
	}
	for _, tc := range []struct{ path, body string }{
		{"/projfiles/unit/start", `{"rel":"proj/a"}`},
		{"/projfiles/unit/stop", `{"code":"AB1"}`},
		{"/projfiles/unit/create", `{"rel":"proj/new"}`},
	} {
		resp, b := pfHTTPDo(t, srv.URL, http.MethodPost, tc.path, tc.body, "")
		if resp.StatusCode != http.StatusConflict {
			t.Errorf("%s 占不到轮次应 busy: %d %s", tc.path, resp.StatusCode, b)
		}
	}
	svc.release()
	// 参数不合法时也不该占住轮次
	if resp, _ := pfHTTPDo(t, srv.URL, http.MethodPost, "/projfiles/unit/start", `{"rel":"../x"}`, ""); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("占不到轮次时坏参数仍应 400: %d", resp.StatusCode)
	}
	if svc.isRunning() {
		t.Error("busy/400 都不该把轮次占住")
	}
}

func TestProjfilesHTTPUnitArgs(t *testing.T) {
	api, svc, _, dir := pfAPIT(t, nil, nil)
	srv := pfServer(t, api)

	for _, body := range []string{`{}`, `{"rel":""}`, `{"rel":"/abs/path"}`, `{"rel":"a/../b"}`, `{坏`} {
		if resp, _ := pfHTTPDo(t, srv.URL, http.MethodPost, "/projfiles/unit/start", body, ""); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("unit/start %s 应 400: %d", body, resp.StatusCode)
		}
		if resp, _ := pfHTTPDo(t, srv.URL, http.MethodPost, "/projfiles/unit/create", body, ""); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("unit/create %s 应 400: %d", body, resp.StatusCode)
		}
	}
	for _, body := range []string{`{}`, `{"code":""}`, `{"code":"ab1"}`, `{"code":"a/b"}`, `{"code":"../x"}`} {
		if resp, _ := pfHTTPDo(t, srv.URL, http.MethodPost, "/projfiles/unit/stop", body, ""); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("unit/stop %s 应 400: %d", body, resp.StatusCode)
		}
	}
	for _, q := range []string{"", "code=", "code=ab1", "code=a%2Fb", "code=" + strings.Repeat("A", 65)} {
		if resp, _ := pfHTTPDo(t, srv.URL, http.MethodGet, "/projfiles/unit?"+q, "", ""); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("GET unit?%s 应 400: %d", q, resp.StatusCode)
		}
	}
	// 合法编号但没记录 → 404
	if resp, _ := pfHTTPDo(t, srv.URL, http.MethodGet, "/projfiles/unit?code=AB1", "", ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("没记录应 404: %d", resp.StatusCode)
	}
	// 写了本机单元状态 → 返回结果与 Aligned
	p := projsync.PFRoundStatePath(filepath.Join(dir, "projfiles"), "AB1")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{"version": projsync.PFRoundSavedVersion, "unit": "AB1",
		"round": "r1", "outcome": "done", "at": "2026-10-27T12:00:00Z", "aligned": true})
	if err := os.WriteFile(p, append(b, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	resp, b := pfHTTPDo(t, srv.URL, http.MethodGet, "/projfiles/unit?code=AB1", "", "")
	if resp.StatusCode != 200 {
		t.Fatalf("有记录应 200: %d %s", resp.StatusCode, b)
	}
	var out struct {
		Code    string          `json:"code"`
		Aligned bool            `json:"aligned"`
		Last    json.RawMessage `json:"last"`
	}
	if err := json.Unmarshal(b, &out); err != nil || out.Code != "AB1" || !out.Aligned || len(out.Last) == 0 {
		t.Fatalf("应答 code/aligned/last: %s", b)
	}
	// 以后的接口先 501，且不许 GET
	for _, p := range []string{"/projfiles/unit/release", "/projfiles/unit/restore", "/projfiles/conflict/resolve"} {
		if resp, _ := pfHTTPDo(t, srv.URL, http.MethodPost, p, `{}`, ""); resp.StatusCode != http.StatusNotImplemented {
			t.Errorf("%s 应 501: %d", p, resp.StatusCode)
		}
		if resp, _ := pfHTTPDo(t, srv.URL, http.MethodGet, p, "", ""); resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s GET 应 405: %d", p, resp.StatusCode)
		}
	}
	if svc.isRunning() {
		t.Error("以上请求都不该占住轮次")
	}
}

func TestProjfilesHTTPUnitNoMover(t *testing.T) {
	api, svc, _, _ := pfAPIT(t, nil, func(m map[string]any) {
		delete(m, "work_dir")
		delete(m, "sync_dir")
	})
	srv := pfServer(t, api)
	resp, b := pfHTTPDo(t, srv.URL, http.MethodPost, "/projfiles/unit/start", `{"rel":"proj/a"}`, "")
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(b), "work_dir") {
		t.Fatalf("搬运器没建起来应 503 说清楚: %d %s", resp.StatusCode, b)
	}
	if svc.isRunning() {
		t.Error("503 之后必须放开轮次")
	}
}

// ── 每日回收清理 ───────────────────────────────────────

func TestProjfilesRecycleDailyOnce(t *testing.T) {
	_, svc, clock, dir := pfAPIT(t, nil, nil)
	var mu sync.Mutex
	var localCalls, remoteCalls int
	var localDays, remoteDays []int
	rec := newProjfilesRecycler(svc)
	rec.cleanLocal = func(rd string, days int) (projsync.PFRecycleCleanResult, error) {
		mu.Lock()
		defer mu.Unlock()
		localCalls++
		localDays = append(localDays, days)
		if want := projsync.PFRoundMacRecycleDir(filepath.Join(dir, "projfiles")); rd != want {
			return projsync.PFRecycleCleanResult{}, errors.New("回收区路径不对: " + rd)
		}
		return projsync.PFRecycleCleanResult{Days: days, Removed: []string{"2026-09-01"}, Files: 2, Bytes: 7}, nil
	}
	rec.cleanRemote = func(ctx context.Context, days int) (*projsync.PFRemoteApplyResult, error) {
		mu.Lock()
		defer mu.Unlock()
		remoteCalls++
		remoteDays = append(remoteDays, days)
		return &projsync.PFRemoteApplyResult{}, nil
	}
	// 设置里 recycle_days=3（pfAPIT 写的），少于 7 按 7
	st, _ := loadProjfilesSettings(svc.prep.settingsPath)
	svc.applySettings(st, nil)

	ran := 0
	round := rec.wrapRound(func(ctx context.Context, opt projsync.PFSchedRoundOptions) *projsync.PFSchedReport {
		ran++
		return &projsync.PFSchedReport{Outcome: projsync.PFSchedDone}
	})
	ctx := context.Background()
	round(ctx, projsync.PFSchedRoundOptions{})
	round(ctx, projsync.PFSchedRoundOptions{})
	mu.Lock()
	n1, r1 := localCalls, remoteCalls
	d1 := append([]int(nil), localDays...)
	mu.Unlock()
	if ran != 2 || n1 != 1 || r1 != 1 {
		t.Fatalf("同一天两轮只该清一次：轮 %d 本机 %d Ubuntu %d", ran, n1, r1)
	}
	if len(d1) != 1 || d1[0] != 7 {
		t.Fatalf("recycle_days=3 应按 7 算: %v", d1)
	}
	// 状态里带结果
	api := newProjfilesAPI(svc, rec)
	sv := api.statusView()
	if sv.Recycle == nil || sv.Recycle.Days != 7 || len(sv.Recycle.LocalRemoved) != 1 || sv.Recycle.LocalError != "" {
		t.Fatalf("状态应记下清理结果: %+v", sv.Recycle)
	}

	// 第二天：下一轮再清
	clock.add(24 * time.Hour)
	round(ctx, projsync.PFSchedRoundOptions{})
	mu.Lock()
	n2 := localCalls
	mu.Unlock()
	if n2 != 2 {
		t.Fatalf("第二天应再清一次，本机清理次数 %d", n2)
	}
	// 退出中不清
	clock.add(24 * time.Hour)
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	round(cctx, projsync.PFSchedRoundOptions{})
	mu.Lock()
	n3 := localCalls
	mu.Unlock()
	if n3 != 2 {
		t.Fatalf("ctx 已取消不该清理，次数 %d", n3)
	}
}

func TestProjfilesRecycleLocalFailureStillRecords(t *testing.T) {
	_, svc, _, _ := pfAPIT(t, nil, nil)
	rec := newProjfilesRecycler(svc)
	rec.cleanLocal = func(string, int) (projsync.PFRecycleCleanResult, error) { return projsync.PFRecycleCleanResult{}, errors.New("没权限") }
	rec.cleanRemote = func(ctx context.Context, days int) (*projsync.PFRemoteApplyResult, error) {
		return nil, errors.New("没连上 Ubuntu")
	}
	rec.afterRound(context.Background())
	api := newProjfilesAPI(svc, rec)
	sv := api.statusView()
	if sv.Recycle == nil || sv.Recycle.LocalError == "" || sv.Recycle.RemoteError == "" {
		t.Fatalf("失败要记进状态: %+v", sv.Recycle)
	}
	// 失败的也算今天清过，同一天不再试
	before := sv.Recycle
	rec.afterRound(context.Background())
	if api.statusView().Recycle != before {
		t.Error("同一天失败不该再清第二次")
	}
}

// ── api.json ───────────────────────────────────────────

func TestProjfilesAPIFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, projfilesAPIFileName)
	if err := writeProjfilesAPI(p, "http://127.0.0.1:8080", "tok123"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("api.json 权限应 0600，得到 %v", fi.Mode().Perm())
	}
	b, _ := os.ReadFile(p)
	var v map[string]string
	if err := json.Unmarshal(b, &v); err != nil || v["url"] != "http://127.0.0.1:8080" || v["token"] != "tok123" {
		t.Fatalf("内容不对: %s", b)
	}
	if err := writeProjfilesAPI(p, "http://127.0.0.1:9090", "tok456"); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(p)
	if !strings.Contains(string(b), "tok456") || strings.Contains(string(b), "tok123") {
		t.Fatalf("重写应整体换掉: %s", b)
	}
	if ents, err := os.ReadDir(dir); err != nil || len(ents) != 1 {
		t.Fatalf("不该留下临时文件: %v", ents)
	}
	t.Setenv("HOME", dir)
	want := filepath.Join(dir, ".xxyi", projfilesAPIFileName)
	if got, err := projfilesAPIPath(); err != nil || got != want {
		t.Errorf("默认路径应是 %s，得到 %s (%v)", want, got, err)
	}
}

// ── 整体验证（跑真 Run）：令牌、Origin 放行、api.json ──

func TestProjfilesHTTPWiredIntoRun(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // 接口文件与状态不得落到真实家目录
	f := newFixture(t, func(cfg map[string]any) {
		cfg["projfiles"] = map[string]any{"enabled": false, "host": "ubuntu",
			"remote_root": "/srv/XXYI-sync", "remote_meta": "/srv/.xxyi/projfiles"}
	})
	st, _, _ := run(t, f.dir)

	if resp, _ := get(t, st, "/projfiles/status", false); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("不带令牌应 401: %d", resp.StatusCode)
	}
	resp, b := pfHTTPDo(t, "http://"+st.HTTP, http.MethodGet, "/projfiles/status?token=wrongwrongwrong", "", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("令牌错应 401: %d %s", resp.StatusCode, b)
	}
	resp, b = pfHTTPDo(t, "http://"+st.HTTP, http.MethodGet, "/projfiles/status?token="+st.Token, "", "http://evil.example")
	if resp.StatusCode != 200 {
		t.Fatalf("带令牌的 GET 应 200: %d %s", resp.StatusCode, b)
	}
	// cmdproto 子进程：Origin 不对但带令牌 → 放行
	resp, b = pfHTTPDo(t, "http://"+st.HTTP, http.MethodPost, "/projfiles/net?token="+st.Token, `{"net":"good"}`, "http://evil.example")
	if resp.StatusCode != 200 {
		t.Fatalf("Origin 不对的 cmdproto 调用应放行: %d %s", resp.StatusCode, b)
	}
	if resp, b := pfHTTPDo(t, "http://"+st.HTTP, http.MethodPost, "/projfiles/net?token="+st.Token, `{"net":"wifi"}`, ""); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("net 不合法应 400: %d %s", resp.StatusCode, b)
	}
	if resp, _ := pfHTTPDo(t, "http://"+st.HTTP, http.MethodPost, "/projfiles/unit/release?token="+st.Token, `{}`, ""); resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("unit/release 应 501: %d", resp.StatusCode)
	}

	p := filepath.Join(os.Getenv("HOME"), ".xxyi", projfilesAPIFileName)
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatalf("启动应写出 %s: %v", p, err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("%s 权限应 0600，得到 %v", p, fi.Mode().Perm())
	}
	b, _ = os.ReadFile(p)
	var v map[string]string
	if err := json.Unmarshal(b, &v); err != nil || v["url"] != "http://"+st.HTTP || v["token"] != st.Token {
		t.Fatalf("%s 内容不对: %s", p, b)
	}
}

func TestProjfilesHTTPNotConfigured(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	f := newFixture(t, nil)
	st, _, _ := run(t, f.dir)
	// 没 projfiles 段不再 404：状态照常回，enabled=false（设置文件缺省关）并说清缺什么
	resp, b := pfHTTPDo(t, "http://"+st.HTTP, http.MethodGet, "/projfiles/status?token="+st.Token, "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("没配这段 /projfiles/status 也不该 404: %d %s", resp.StatusCode, b)
	}
	var sv map[string]any
	if err := json.Unmarshal(b, &sv); err != nil {
		t.Fatalf("状态不是 JSON: %s", b)
	}
	if sv["enabled"] != false {
		t.Errorf("没这段时 enabled 应缺省关: %v", sv["enabled"])
	}
	if gap, _ := sv["config_gap"].(string); !strings.Contains(gap, "host") {
		t.Errorf("config_gap 应说清缺 host: %v", sv["config_gap"])
	}
	if from, _ := sv["host_from"].(string); from != "settings" {
		t.Errorf("没这段时 host 来源应是 settings: %v", sv["host_from"])
	}
	// api.json 照样写：XXYI 的窗口靠它找到接口
	p := filepath.Join(os.Getenv("HOME"), ".xxyi", projfilesAPIFileName)
	if _, err := os.Stat(p); err != nil {
		t.Errorf("没配置 projfiles 时也该写 api.json 让窗口能用: %v", err)
	}
}
