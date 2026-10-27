package wgtund

// projfiles_merge_test.go：合并接口（§14.3）与评审 14-1 的 SFTP 随主机换——都不连真机器。
//   - /projfiles/merge/plan、/merge/run：计划、执行、指纹不符退回、切换期 busy、来源是同步单元拒绝；
//   - SFTP 随主机换：注入假 openSFTP 记下开给哪台，换 host 后新一轮的会话必须属于新主机、旧主机那条被关掉。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/WillyChen0811/base/go/projsync"
)

// pfPutPast 在 base 下按 rel 建文件，mtime 设成很久以前（静止、能挪）。
func pfPutPast(t *testing.T, base, rel, content string, now time.Time) {
	t.Helper()
	p := filepath.Join(base, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
}

// ── 合并接口 ────────────────────────────────────────────

func TestProjfilesMergePlanRunHTTP(t *testing.T) {
	api, svc, clock, dir := pfAPIT(t, &pfFakeFresh{}, nil)
	srv := pfServer(t, api)
	now := clock.now()
	work := filepath.Join(dir, "work")
	// 来源：new.txt→move、same.txt→same、diff.txt→rename、sub/deep.txt→move（sub 会空）
	pfPutPast(t, work, "src/new.txt", "全新", now)
	pfPutPast(t, work, "src/same.txt", "一样", now)
	pfPutPast(t, work, "src/diff.txt", "AAA来源", now)
	pfPutPast(t, work, "src/sub/deep.txt", "深层", now)
	pfPutPast(t, work, "dst/same.txt", "一样", now)
	pfPutPast(t, work, "dst/diff.txt", "BBB目标", now) // 同字节数不同内容 → rename

	resp, b := pfHTTPDo(t, srv.URL, http.MethodPost, "/projfiles/merge/plan", `{"from_rel":"src","to_rel":"dst"}`, "")
	if resp.StatusCode != 200 {
		t.Fatalf("plan 应 200: %d %s", resp.StatusCode, b)
	}
	var plan projsync.PFMergePlanResult
	if err := json.Unmarshal(b, &plan); err != nil {
		t.Fatal(err)
	}
	act := map[string]string{}
	for _, it := range plan.Items {
		act[it.Rel] = string(it.Action)
	}
	for rel, wa := range map[string]string{"new.txt": "move", "same.txt": "same", "diff.txt": "rename", "sub/deep.txt": "move"} {
		if act[rel] != wa {
			t.Errorf("%s 处置应 %s，实为 %s", rel, wa, act[rel])
		}
	}

	resp, b = pfHTTPDo(t, srv.URL, http.MethodPost, "/projfiles/merge/run",
		`{"from_rel":"src","to_rel":"dst","fingerprint":"`+plan.Fingerprint+`"}`, "")
	if resp.StatusCode != 200 {
		t.Fatalf("run 应 200: %d %s", resp.StatusCode, b)
	}
	var res projsync.PFMergeRunResult
	if err := json.Unmarshal(b, &res); err != nil {
		t.Fatal(err)
	}
	if res.Rejected || len(res.Failed) != 0 {
		t.Fatalf("不该被拒/失败: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(work, "src", "new.txt")); !os.IsNotExist(err) {
		t.Errorf("new.txt 应已挪走: %v", err)
	}
	if _, err := os.Stat(filepath.Join(work, "src", "sub")); !os.IsNotExist(err) {
		t.Errorf("空的 sub 目录应已进回收区: %v", err)
	}
	// 回收区里找 same 那份
	rc, err := projsync.NewPFRecycle(projsync.PFRoundMacRecycleDir(svc.prep.metaDir), projsync.PFRecycleOptions{Now: clock.now})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	days, _ := rc.Days()
	for _, d := range days {
		es, _ := rc.ReadIndex(d)
		for _, e := range es {
			if e.Path == "same.txt" && e.Reason == projsync.PFMergeReasonSameRecycle {
				found = true
			}
		}
	}
	if !found {
		t.Error("回收区 index 里没有 same 那份（原因应为「合并时内容相同」）")
	}
}

func TestProjfilesMergeRunFingerprintMismatchHTTP(t *testing.T) {
	api, _, _, dir := pfAPIT(t, &pfFakeFresh{}, nil)
	srv := pfServer(t, api)
	now := pfClockNow(api)
	pfPutPast(t, filepath.Join(dir, "work"), "src/a.txt", "A", now)
	pfPutPast(t, filepath.Join(dir, "work"), "dst/b.txt", "B", now)

	_, b := pfHTTPDo(t, srv.URL, http.MethodPost, "/projfiles/merge/plan", `{"from_rel":"src","to_rel":"dst"}`, "")
	var plan projsync.PFMergePlanResult
	if err := json.Unmarshal(b, &plan); err != nil {
		t.Fatal(err)
	}
	// 计划之后动一下来源文件
	p := filepath.Join(dir, "work", "src", "a.txt")
	now2 := now.Add(time.Minute)
	if err := os.Chtimes(p, now2, now2); err != nil {
		t.Fatal(err)
	}
	resp, b := pfHTTPDo(t, srv.URL, http.MethodPost, "/projfiles/merge/run",
		`{"from_rel":"src","to_rel":"dst","fingerprint":"`+plan.Fingerprint+`"}`, "")
	if resp.StatusCode != 200 {
		t.Fatalf("指纹不符也应 200（带 rejected）: %d %s", resp.StatusCode, b)
	}
	var res projsync.PFMergeRunResult
	if err := json.Unmarshal(b, &res); err != nil {
		t.Fatal(err)
	}
	if !res.Rejected || !strings.Contains(res.Reason, "计划之后文件有变动") {
		t.Errorf("应拒绝并说清楚: %+v", res)
	}
	// 来源没被动过
	if got, _ := os.ReadFile(p); string(got) != "A" {
		t.Errorf("拒绝时不该动文件: %q", got)
	}
}

func TestProjfilesMergeSwitchBusyHTTP(t *testing.T) {
	dir0 := filepath.Join(pfTempBase(t), "sync2")
	api, _, _, dir := pfAPIT(t, &pfFakeFresh{}, nil)
	srv := pfServer(t, api)
	now := pfClockNow(api)
	pfPutPast(t, filepath.Join(dir, "work"), "src/a.txt", "A", now)
	pfPutPast(t, filepath.Join(dir, "work"), "dst/b.txt", "B", now)
	// 先建起来（plan 会 ensureBuilt）
	pfHTTPDo(t, srv.URL, http.MethodPost, "/projfiles/merge/plan", `{"from_rel":"src","to_rel":"dst"}`, "")
	// 改 sync_dir → 进切换期，两个接口都 busy
	pfWriteSettings(t, api.svc.prep.settingsPath, map[string]any{
		"work_dir": filepath.Join(dir, "work"), "sync_dir": dir0, "recycle_days": 3,
	})
	for _, tc := range []struct{ path, body string }{
		{"/projfiles/merge/plan", `{"from_rel":"src","to_rel":"dst"}`},
		{"/projfiles/merge/run", `{"from_rel":"src","to_rel":"dst","fingerprint":"x"}`},
	} {
		resp, b := pfHTTPDo(t, srv.URL, http.MethodPost, tc.path, tc.body, "")
		if why, ok := busyReason(b); resp.StatusCode != http.StatusConflict || !ok {
			t.Fatalf("%s 切换期应 busy: %d %s", tc.path, resp.StatusCode, b)
		} else if !strings.Contains(why, "刚改过") && !strings.Contains(why, "正在切换") {
			t.Errorf("busy 原因应说切换: %s", why)
		}
	}
}

func TestProjfilesMergeSourceRegisteredRejectedHTTP(t *testing.T) {
	api, svc, _, dir := pfAPIT(t, &pfFakeFresh{}, nil)
	srv := pfServer(t, api)
	now := pfClockNow(api)
	code := "260403J0812"
	rel := "2026/" + code + "-来源"
	pfPutPast(t, filepath.Join(dir, "work"), rel+"/a.txt", "A", now)
	pfPutPast(t, filepath.Join(dir, "work"), "dst/b.txt", "B", now)
	rec := projsync.PFUnitRecord{Code: code, Rel: rel, State: projsync.PFUnitSyncing,
		LinkPath: filepath.Join(dir, "work", filepath.ToSlash(rel)),
		SyncPath: filepath.Join(dir, "sync", filepath.ToSlash(rel)), StartedAt: now}
	pfWriteUnitsConf(t, svc.prep.metaDir, []projsync.PFUnitRecord{rec})

	resp, b := pfHTTPDo(t, srv.URL, http.MethodPost, "/projfiles/merge/plan",
		`{"from_rel":"`+rel+`","to_rel":"dst"}`, "")
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(b), "正在同步") {
		t.Fatalf("来源是同步单元应 400 说「正在同步」: %d %s", resp.StatusCode, b)
	}
}

// ── 评审 14-1：SFTP 会话随主机换 ────────────────────────

// pfFakeSess 是一条假的带主机 SFTP 会话；真传输不在这里跑，只验主机归属与开关。
type pfFakeSess struct {
	host   string
	closed atomic.Bool
}

func (s *pfFakeSess) Host() string  { return s.host }
func (s *pfFakeSess) Alive() bool   { return !s.closed.Load() }
func (s *pfFakeSess) Close() error  { s.closed.Store(true); return nil }
func (s *pfFakeSess) Open(string) (projsync.PFTransferFile, error) {
	return nil, errPfStubSess
}
func (s *pfFakeSess) OpenFile(string, int) (projsync.PFTransferFile, error) {
	return nil, errPfStubSess
}
func (s *pfFakeSess) Stat(string) (os.FileInfo, error) { return nil, errPfStubSess }
func (s *pfFakeSess) Remove(string) error               { return errPfStubSess }
func (s *pfFakeSess) MkdirAll(string) error            { return errPfStubSess }
func (s *pfFakeSess) ReadDir(string) ([]os.FileInfo, error) { return nil, errPfStubSess }
func (s *pfFakeSess) Chtimes(string, time.Time, time.Time) error {
	return errPfStubSess
}

var errPfStubSess = errors.New("测试假会话：不真连机器")

// pfSessRec 记下每次 openSFTP 开给了哪台、返回的会话（用来验换主机后归属与旧会话被关）。
type pfSessRec struct {
	mu     sync.Mutex
	hosts  []string
	sess   []*pfFakeSess
}

func (r *pfSessRec) open(host string) (projfilesSFTPSession, error) {
	s := &pfFakeSess{host: host}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hosts = append(r.hosts, host)
	r.sess = append(r.sess, s)
	return s, nil
}

func (r *pfSessRec) hostsSnapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.hosts...)
}

func TestProjfilesSFTPSessionFollowsHost(t *testing.T) {
	s, _, dir := pfSvcNoSection(t)
	rec := &pfSessRec{}
	s.attachSFTP = s.attachSFTPReal // 走真实逻辑（复用/换主机重开）
	s.openSFTP = rec.open
	var rounds pfRounds
	rounds.hook(s)

	pfWriteSettings(t, filepath.Join(dir, "projfiles-settings.json"), pfGoodSettings()) // host=ubuntu
	s.tick(context.Background(), "timer")
	if got := rec.hostsSnapshot(); len(got) != 1 || got[0] != "ubuntu" {
		t.Fatalf("第一轮应在 ubuntu 上开会话: %+v", got)
	}
	if c := s.sftpFS.cur(); c == nil || c.Host() != "ubuntu" {
		t.Fatalf("当前会话应属 ubuntu: %+v", c)
	}

	// 换主机 ubuntu→ubuntu2
	m := pfGoodSettings()
	m["host"] = "ubuntu2"
	m["remote_meta"] = "/srv/.xxyi/projfiles2"
	pfWriteSettings(t, filepath.Join(dir, "projfiles-settings.json"), m)
	s.tick(context.Background(), "timer")

	got := rec.hostsSnapshot()
	if len(got) < 2 || got[len(got)-1] != "ubuntu2" {
		t.Fatalf("换主机后新一轮必须在 ubuntu2 上开会话: %+v", got)
	}
	if c := s.sftpFS.cur(); c == nil || c.Host() != "ubuntu2" {
		t.Fatalf("换主机后当前会话必须属新主机 ubuntu2，实为 %v", func() any {
			if c == nil {
				return nil
			}
			return c.Host()
		}())
	}
	// 旧的 ubuntu 会话必须被关掉（不能留着往旧机器写）
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, ss := range rec.sess {
		if ss.host == "ubuntu" && !ss.closed.Load() {
			t.Error("换主机后旧的 ubuntu 会话应已被关闭")
		}
	}
}

// 同一台连续两轮：不该反复重开会话（复用）。
func TestProjfilesSFTPSessionReuseSameHost(t *testing.T) {
	s, _, dir := pfSvcNoSection(t)
	rec := &pfSessRec{}
	s.attachSFTP = s.attachSFTPReal
	s.openSFTP = rec.open
	var rounds pfRounds
	rounds.hook(s)
	pfWriteSettings(t, filepath.Join(dir, "projfiles-settings.json"), pfGoodSettings())

	s.tick(context.Background(), "timer")
	s.tick(context.Background(), "timer") // 同一台 ubuntu，第二轮不该重开
	if got := rec.hostsSnapshot(); len(got) != 1 {
		t.Fatalf("同一台还活着不该重开会话: %+v", got)
	}
}

// ── 小工具 ──────────────────────────────────────────────

func pfClockNow(api *projfilesAPI) time.Time { return api.svc.now() }

func pfTempBase(t *testing.T) string { return t.TempDir() }

func pfWriteUnitsConf(t *testing.T, meta string, us []projsync.PFUnitRecord) {
	t.Helper()
	b, err := projsync.PFMarshalUnits(us)
	if err != nil {
		t.Fatalf("序列化登记表: %v", err)
	}
	if err := os.MkdirAll(meta, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(meta, "units.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}
