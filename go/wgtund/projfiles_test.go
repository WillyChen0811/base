package wgtund

// projfiles_test.go：项目文件同步接入 wgtund 的测试（T11a）——配置校验、设置文件回落、定时、网络变化、状态落盘。
// 远端一律用假 Runner / 假一轮执行，不连真机器；定时用注入的假时钟，不真 sleep。

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/WillyChen0811/base/go/projsync"
)

// ── 夹具 ───────────────────────────────────────────────

// pfConfT 造一份带 projfiles 段的 fileConf；mut 可以改配置段与主机。
func pfConfT(t *testing.T, mut func(pf map[string]any, h *hostConf)) *fileConf {
	t.Helper()
	pf := map[string]any{"enabled": true, "host": "ubuntu",
		"remote_root": "/srv/XXYI-sync", "remote_meta": "/srv/.xxyi/projfiles"}
	h := hostConf{Name: "ubuntu", Addr: "127.0.0.1:22", User: "chenwy", IdentityFiles: []string{"/id"}}
	if mut != nil {
		mut(pf, &h)
	}
	b, err := json.Marshal(pf)
	if err != nil {
		t.Fatal(err)
	}
	var pc projfilesConf
	if err := json.Unmarshal(b, &pc); err != nil {
		t.Fatal(err)
	}
	return &fileConf{KnownHosts: "/kh", Hosts: []hostConf{h}, Projfiles: &pc}
}

// pfClock 是注入的假时钟。
type pfClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *pfClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *pfClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// pfStep 是脚本里的一次 wait：先执行 act（改时钟、改网络），再给出 reason。
type pfStep struct {
	reason string
	act    func()
}

// pfScriptedWait 按脚本决定每次 wait 的结果；脚本跑完就等 ctx 结束。
func pfScriptedWait(t *testing.T, s *projfilesSvc, c *pfClock, steps []pfStep) func(ctx context.Context, d time.Duration) (string, error) {
	t.Helper()
	var mu sync.Mutex
	i := 0
	return func(ctx context.Context, d time.Duration) (string, error) {
		mu.Lock()
		if i >= len(steps) {
			mu.Unlock()
			return "", context.Canceled // 脚本跑完就让循环收尾
		}
		st := steps[i]
		i++
		mu.Unlock()
		if st.act != nil {
			st.act()
		}
		return st.reason, nil
	}
}

// pfSvcT 造一个不连真机器的服务：假 Runner、假 SFTP、假时钟，状态与设置都落在临时目录。
func pfSvcT(t *testing.T, mut func(pf map[string]any, h *hostConf)) (*projfilesSvc, *pfClock, string) {
	t.Helper()
	svc, clock, dir := pfSvcTRaw(t, mut)
	svc.runner = &pfFakeRunner{}
	svc.attachSFTP = func() error { return nil }
	return svc, clock, dir
}

func pfSvcTRaw(t *testing.T, mut func(pf map[string]any, h *hostConf)) (*projfilesSvc, *pfClock, string) {
	t.Helper()
	c := pfConfT(t, mut)
	prep, err := prepareProjfiles(c)
	if err != nil {
		t.Fatalf("配置应合格: %v", err)
	}
	if prep == nil {
		t.Fatal("配了 projfiles 段，prep 不该是 nil")
	}
	dir := t.TempDir()
	prep.metaDir = filepath.Join(dir, "projfiles")
	prep.settingsPath = filepath.Join(dir, "projfiles-settings.json")
	prep.statusPath = filepath.Join(dir, "projfiles", "status", "_round.json")
	clock := &pfClock{t: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
	s := newProjfilesSvc(prep, nil)
	s.now = clock.now
	return s, clock, dir
}

// pfFakeRunner 是假的 SSH 远程执行（生产由 sshRemoteRunner 走共用连接池，每次另开会话、可并发）。
type pfFakeRunner struct {
	mu sync.Mutex
	n  int
}

func (r *pfFakeRunner) Run(ctx context.Context, cmd string, stdin []byte) ([]byte, []byte, int, error) {
	r.mu.Lock()
	r.n++
	r.mu.Unlock()
	return nil, nil, -1, errors.New("测试里不连真机器")
}

// pfWriteSettings 写设置文件。
func pfWriteSettings(t *testing.T, path string, v map[string]any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// pfRounds 记-download一轮的调用（并发安全）。
type pfRounds struct {
	mu    sync.Mutex
	opts  []projsync.PFSchedRoundOptions
	at    []time.Time
	block chan struct{}
}

func (r *pfRounds) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.opts)
}

func (r *pfRounds) all() []projsync.PFSchedRoundOptions {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]projsync.PFSchedRoundOptions(nil), r.opts...)
}

func (r *pfRounds) times() []time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Time(nil), r.at...)
}

// hook 装到服务上：记下每一轮的选项与时间；block 非 nil 时先等它。
func (r *pfRounds) hook(s *projfilesSvc) {
	s.runRound = func(ctx context.Context, opt projsync.PFSchedRoundOptions) *projsync.PFSchedReport {
		if r.block != nil {
			<-r.block
		}
		r.mu.Lock()
		r.opts = append(r.opts, opt)
		r.at = append(r.at, s.now())
		r.mu.Unlock()
		return &projsync.PFSchedReport{StartedAt: r.at[len(r.at)-1], EndedAt: s.now(),
			Net: projsync.PFRemoteNetGood, Outcome: projsync.PFSchedDone}
	}
}

// ── 配置 ───────────────────────────────────────────────

func TestProjfilesConfig(t *testing.T) {
	// 没配这段：nil，服务不启动
	c := &fileConf{KnownHosts: "/kh", Hosts: []hostConf{{Name: "ubuntu", IdentityFiles: []string{"/id"}}}}
	if p, err := prepareProjfiles(c); err != nil || p != nil {
		t.Fatalf("没配 projfiles 段应返回 nil: %v %+v", err, p)
	}
	// 正常一段：字段、路径、默认值
	c = pfConfT(t, nil)
	p, err := prepareProjfiles(c)
	if err != nil {
		t.Fatal(err)
	}
	if !p.enabled || p.host != "ubuntu" || p.remoteRoot != "/srv/XXYI-sync" || p.remoteMeta != "/srv/.xxyi/projfiles" {
		t.Fatalf("配置没照抄: %+v", p)
	}
	if p.xxyiMsg != defaultProjfilesXxyiMsg || p.coordID == "" || p.coordHost == "" {
		t.Fatalf("缺省值不对: %+v", p)
	}
	if !strings.HasSuffix(p.metaDir, filepath.Join(".xxyi", "projfiles")) ||
		!strings.HasSuffix(p.settingsPath, "projfiles-settings.json") ||
		!strings.HasSuffix(p.statusPath, filepath.Join("projfiles", "status", "_round.json")) {
		t.Fatalf("目录不对: %+v", p)
	}
	// enabled=false：照样校验，但服务不起协程
	c = pfConfT(t, func(pf map[string]any, _ *hostConf) { pf["enabled"] = false })
	if p, err := prepareProjfiles(c); err != nil || p == nil || p.enabled {
		t.Fatalf("enabled=false: %v %+v", err, p)
	}
	// enabled=false 且字段不合法：仍然报错，不静默放过
	c = pfConfT(t, func(pf map[string]any, _ *hostConf) { pf["enabled"] = false; pf["remote_root"] = "相对/路径" })
	if _, err := prepareProjfiles(c); err == nil {
		t.Fatal("enabled=false 时段里的坏字段也要挡住")
	}
	// 主机不在 hosts 里 / 没有私钥
	for name, mut := range map[string]func(pf map[string]any, h *hostConf){
		"主机不在 hosts 里": func(pf map[string]any, _ *hostConf) { pf["host"] = "别的机器" },
		"主机没有私钥":       func(pf map[string]any, h *hostConf) { h.IdentityFiles = nil },
	} {
		if _, err := prepareProjfiles(pfConfT(t, mut)); err == nil || !strings.Contains(err.Error(), "hosts.json: projfiles") {
			t.Errorf("%s 应被拒绝", name)
		}
	}
	// 远端路径
	for _, bad := range []string{"", "srv/x", "/srv/../x", "/srv/x y", "/srv/中文"} {
		for _, key := range []string{"remote_root", "remote_meta"} {
			key := key
			_, err := prepareProjfiles(pfConfT(t, func(pf map[string]any, _ *hostConf) { pf[key] = bad }))
			if err == nil || !strings.Contains(err.Error(), key) {
				t.Errorf("%s=%q 应被拒绝: %v", key, bad, err)
			}
		}
	}
	// 结尾多余的 / 去掉
	p, err = prepareProjfiles(pfConfT(t, func(pf map[string]any, _ *hostConf) { pf["remote_root"] = "/srv/XXYI-sync//" }))
	if err != nil || p.remoteRoot != "/srv/XXYI-sync" {
		t.Fatalf("结尾斜杠没去掉: %v %+v", err, p)
	}
	// xxyi_msg
	for _, bad := range []string{"a b", "x;rm -rf /", "$(id)", "-x", "a'b", "a\nb"} {
		if _, err := prepareProjfiles(pfConfT(t, func(pf map[string]any, _ *hostConf) { pf["xxyi_msg"] = bad })); err == nil {
			t.Errorf("xxyi_msg=%q 应被拒绝", bad)
		}
	}
	p, err = prepareProjfiles(pfConfT(t, func(pf map[string]any, _ *hostConf) { pf["xxyi_msg"] = "/opt/bin/xxyi-msg_2.x~" }))
	if err != nil || p.xxyiMsg != "/opt/bin/xxyi-msg_2.x~" {
		t.Fatalf("%v %+v", err, p)
	}
	// coord_id
	for _, bad := range []string{".mac", "-mac", "a.b", strings.Repeat("x", 65), ""} {
		if bad == "" {
			continue // 空 = 用主机名，单独测
		}
		if _, err := prepareProjfiles(pfConfT(t, func(pf map[string]any, _ *hostConf) { pf["coord_id"] = bad })); err == nil {
			t.Errorf("coord_id=%q 应被拒绝", bad)
		}
	}
	p, err = prepareProjfiles(pfConfT(t, func(pf map[string]any, _ *hostConf) { pf["coord_id"] = "mac-book_1" }))
	if err != nil || p.coordID != "mac-book_1" {
		t.Fatalf("%v %+v", err, p)
	}
}

func TestProjfilesCoordFromHost(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"macbook", "macbook"},
		{"MacBook-Pro.local", "MacBook-Pro-local"},
		{"m\\\\1", "m--1"},
		{"...x", "x"},
		{"-_", ""},
		{"", ""},
	} {
		if got := projfilesCoordFromHost(c.in); got != c.want {
			t.Errorf("coordFromHost(%q)=%q，应为 %q", c.in, got, c.want)
		}
	}
}

// ── 设置文件 ───────────────────────────────────────────

func TestProjfilesSettingsMissingUsesDefaults(t *testing.T) {
	s, _, dir := pfSvcT(t, nil)
	st, problems := loadProjfilesSettings(s.prep.settingsPath)
	if len(problems) != 1 || !strings.Contains(problems[0], "不存在") {
		t.Fatalf("应报一条不存在: %#v", problems)
	}
	if !st.Enabled {
		t.Error("enabled 缺省应为开（真开关在 hosts.json）")
	}
	if st.PollSeconds != 60 || st.PollSecondsExpensive != 300 || st.MaxSecondsExpensive != 20 || st.MaxSecondsUser != 60 {
		t.Errorf("秒数缺省不对: %+v", st)
	}
	if st.LimitKbpsGood != 0 || st.LimitKbpsExpensive != 0 || st.RecycleDays != 30 || st.QuietSeconds != 10 {
		t.Errorf("缺省不对: %+v", st)
	}
	// 缺省值与问题要进状态
	s.applySettings(st, problems)
	if got := s.status(); got.Settings.PollSeconds != 60 || len(got.SettingsProblems) != 1 {
		t.Fatalf("状态里没反映缺省: %+v", got)
	}
	_ = dir
}

func TestProjfilesSettingsBadJSONUsesDefaults(t *testing.T) {
	_, _, dir := pfSvcT(t, nil)
	p := filepath.Join(dir, "projfiles-settings.json")
	if err := os.WriteFile(p, []byte("{不是 JSON"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, problems := loadProjfilesSettings(p)
	if len(problems) != 1 || !strings.Contains(problems[0], "不是合法的 JSON") {
		t.Fatalf("应报一条 JSON 问题: %#v", problems)
	}
	if st.PollSeconds != 60 || st.RecycleDays != 30 || !st.Enabled {
		t.Errorf("整份应回落缺省: %+v", st)
	}
	// 空文件、是目录
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if st, problems := loadProjfilesSettings(p); len(problems) == 0 || st.PollSeconds != 60 {
		t.Errorf("空文件应回落缺省并报告: %+v %#v", st, problems)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if st, problems := loadProjfilesSettings(filepath.Join(dir, "sub")); len(problems) != 1 || st.PollSeconds != 60 {
		t.Errorf("读不了应回落缺省并报告: %+v %#v", st, problems)
	}
}

func TestProjfilesSettingsValidAndPerKeyFallback(t *testing.T) {
	s, _, dir := pfSvcT(t, nil)
	p := filepath.Join(dir, "projfiles-settings.json")
	pfWriteSettings(t, p, map[string]any{
		"enabled": true, "work_dir": "~/Desktop/Projects", "sync_dir": "/srv/sync",
		"poll_seconds": 45, "poll_seconds_expensive": 900, "max_seconds_expensive": 5,
		"max_seconds_user": 120, "limit_kbps_good": 800, "limit_kbps_expensive": 64,
		"recycle_days": 7, "quiet_seconds": 3,
	})
	st, problems := loadProjfilesSettings(p)
	if len(problems) != 0 {
		t.Fatalf("合格的设置不该报问题: %#v", problems)
	}
	s.applySettings(st, problems)
	if got := s.status(); got.Settings.RecycleDays != 7 || got.Settings.QuietSeconds != 3 || got.Settings.SyncDir != "/srv/sync" {
		t.Fatalf("状态里的设置不对: %+v", got)
	}
	if st.PollSeconds != 45 || st.PollSecondsExpensive != 900 || st.MaxSecondsExpensive != 5 || st.MaxSecondsUser != 120 ||
		st.LimitKbpsGood != 800 || st.LimitKbpsExpensive != 64 || st.RecycleDays != 7 || st.QuietSeconds != 3 {
		t.Fatalf("值没照抄: %+v", st)
	}
	if st.WorkDir != filepath.Join(expand("~/Desktop/Projects")) || st.SyncDir != "/srv/sync" {
		t.Fatalf("路径没展开: %+v", st)
	}
	// 单个键不合法：只有那个键回落，其余保持
	pfWriteSettings(t, p, map[string]any{
		"enabled": false, "poll_seconds": 1, "poll_seconds_expensive": -9, "recycle_days": 0,
		"limit_kbps_good": -5, "work_dir": "相对/目录", "quiet_seconds": 99999999,
	})
	st, problems = loadProjfilesSettings(p)
	if st.Enabled {
		t.Error("enabled=false 应生效")
	}
	if st.PollSeconds != 60 || st.PollSecondsExpensive != 300 || st.RecycleDays != 30 ||
		st.LimitKbpsGood != 0 || st.QuietSeconds != 10 || st.WorkDir != "" {
		t.Fatalf("坏键没回落缺省: %+v", st)
	}
	for _, want := range []string{"poll_seconds", "poll_seconds_expensive", "recycle_days", "limit_kbps_good", "work_dir", "quiet_seconds"} {
		found := false
		for _, m := range problems {
			if strings.Contains(m, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("状态里应报 %s 的问题: %#v", want, problems)
		}
	}
}

// ── 定时与网络 ─────────────────────────────────────────

func TestProjfilesDisabledDoesNotRun(t *testing.T) {
	s, clock, _ := pfSvcT(t, func(pf map[string]any, _ *hostConf) { pf["enabled"] = false })
	var r pfRounds
	r.hook(s)
	done := make(chan struct{})
	go func() { s.loop(context.Background()); close(done) }()
	clock.add(time.Hour)
	s.kick()
	time.Sleep(30 * time.Millisecond)
	if r.len() != 0 {
		t.Fatalf("hosts.json 里没启用却跑了 %d 轮", r.len())
	}
	// tick 直接调也照样不跑（enabled 在设置里关掉的情形在下面单独测）
	s.Close()
}

func TestProjfilesCadenceGoodExpensiveOffline(t *testing.T) {
	s, clock, _ := pfSvcT(t, nil)
	pfWriteSettings(t, s.prep.settingsPath, map[string]any{"enabled": true, "poll_seconds": 60, "poll_seconds_expensive": 300})
	var rr pfRounds
	rr.hook(s)
	s.setNet(projsync.PFRemoteNetGood, clock.now())
	steps := []pfStep{
		{"timer", func() { clock.add(projfilesInitialDelay) }},                                                   // 启动后 10 秒：第一轮
		{"timer", func() { clock.add(300 * time.Second); s.setNet(projsync.PFRemoteNetExpensive, clock.now()) }}, // 转热点
		{"timer", func() { clock.add(300 * time.Second) }},                                                       // 热点按贵的间隔
		{"timer", func() { clock.add(3600 * time.Second); s.setNet(projsync.PFRemoteNetOffline, clock.now()) }},  // 断网：不跑
	}
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.wait = pfScriptedWait(t, s, clock, steps)
	go func() { s.loop(ctx); close(done) }()
	<-done
	tm := rr.times()
	if len(tm) != 3 {
		t.Fatalf("应跑三轮（断网那轮不跑），跑了 %d 轮: %v", len(tm), tm)
	}
	if d := tm[1].Sub(tm[0]); d != 300*time.Second {
		t.Errorf("热点下间隔应是 300 秒，实为 %v", d)
	}
	if d := tm[2].Sub(tm[1]); d != 300*time.Second {
		t.Errorf("热点下第二轮间隔应是 300 秒，实为 %v", d)
	}
	opts := rr.all()
	if !opts[0].Full {
		t.Error("第一轮应是全量核对")
	}
	st := s.status()
	if st.Net != projsync.PFRemoteNetOffline || !strings.Contains(st.LastAction, "断网") {
		t.Fatalf("断网那轮应只报告: %+v", st)
	}
	if st.IntervalS != 300 {
		t.Errorf("断网时下次间隔应报贵的 300 秒，实为 %d", st.IntervalS)
	}
}

func TestProjfilesGoodNetUsesPollSeconds(t *testing.T) {
	s, clock, _ := pfSvcT(t, nil)
	pfWriteSettings(t, s.prep.settingsPath, map[string]any{"enabled": true, "poll_seconds": 60, "poll_seconds_expensive": 300})
	var rr pfRounds
	rr.hook(s)
	s.setNet(projsync.PFRemoteNetGood, clock.now())
	steps := []pfStep{
		{"timer", func() { clock.add(projfilesInitialDelay) }},
		{"timer", func() { clock.add(60 * time.Second) }},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	s.wait = pfScriptedWait(t, s, clock, steps)
	go func() { s.loop(ctx); close(done) }()
	<-done
	tm := rr.times()
	if len(tm) != 2 {
		t.Fatalf("应跑两轮: %v", tm)
	}
	if d := tm[1].Sub(tm[0]); d != 60*time.Second {
		t.Errorf("网好间隔应是 60 秒，实为 %v", d)
	}
	if opts := rr.all(); opts[1].Full {
		t.Error("第二分钟不该是全量核对")
	}
	// next_run 与状态里的间隔
	if got := s.status(); got.IntervalS != 60 || got.NextRun == nil || !got.NextRun.Equal(tm[1].Add(60*time.Second)) {
		t.Fatalf("状态里的下次时间不对: %+v", got)
	}
}

func TestProjfilesNetImprovedRunsImmediately(t *testing.T) {
	s, clock, _ := pfSvcT(t, nil)
	pfWriteSettings(t, s.prep.settingsPath, map[string]any{"enabled": true, "poll_seconds": 60, "poll_seconds_expensive": 300})
	var rr pfRounds
	rr.hook(s)
	s.setNet(projsync.PFRemoteNetExpensive, clock.now())
	// 第 1 轮跑完；网络转断网；贵的间隔还没到就变好 → 立刻跑，不等 300 秒
	steps := []pfStep{
		{"timer", func() { clock.add(projfilesInitialDelay) }},
		{"timer", func() { clock.add(100 * time.Second); s.setNet(projsync.PFRemoteNetOffline, clock.now()) }},
		{"wake", func() { clock.add(1 * time.Second); s.setNet(projsync.PFRemoteNetGood, clock.now()) }},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	s.wait = pfScriptedWait(t, s, clock, steps)
	go func() { s.loop(ctx); close(done) }()
	<-done
	tm := rr.times()
	if len(tm) != 2 {
		t.Fatalf("断网那一轮不该跑，变好后要立刻跑：跑了 %d 轮 %v", len(tm), tm)
	}
	if d := tm[1].Sub(tm[0]); d != 101*time.Second {
		t.Errorf("变好后应立刻跑（不等满 300 秒），实为 %v", d)
	}
}

func TestProjfilesSetNetForwardsAndKicks(t *testing.T) {
	s, clock, _ := svcWithRealSched(t)
	// 非法网络当未知
	s.setNet("翻车了", clock.now())
	if got := s.status(); got.Net != projsync.PFRemoteNetUnknown {
		t.Fatalf("非法网络应算未知: %+v", got)
	}
	if len(s.wake) != 0 {
		t.Error("没变好不该投唤醒")
	}
	// 差→好：投一次唤醒
	s.setNet(projsync.PFRemoteNetGood, clock.now())
	select {
	case <-s.wake:
	default:
		t.Fatal("网络变好应立刻投唤醒")
	}
	// 好→好：不重复投
	s.setNet(projsync.PFRemoteNetGood, clock.now())
	if len(s.wake) != 0 {
		t.Error("一直是好不该再投唤醒")
	}
	// 转嫁到 PFSched：它算出的网络应与之一致（过有效期后自动算未知）
	sched := s.getSched()
	if sched == nil {
		t.Fatal("调度器没建成")
	}
	clock.add(4 * time.Minute)
	if got := s.status(); got.Net != projsync.PFRemoteNetUnknown {
		t.Fatalf("上报过期应算未知: %+v", got)
	}
	s.Close()
}

// svcWithRealSched 造一个真建起调度器（假 Runner）的服务，用来验转嫁与 touched。
func svcWithRealSched(t *testing.T) (*projfilesSvc, *pfClock, string) {
	t.Helper()
	s, clock, dir := pfSvcT(t, nil)
	pfWriteSettings(t, s.prep.settingsPath, map[string]any{"enabled": true})
	if err := s.build(s.settings); err != nil {
		t.Fatalf("零件没建成: %v", err)
	}
	return s, clock, dir
}

func TestProjfilesOnlyOneRoundAtATime(t *testing.T) {
	s, clock, _ := pfSvcT(t, nil)
	pfWriteSettings(t, s.prep.settingsPath, map[string]any{"enabled": true})
	rr := pfRounds{block: make(chan struct{})}
	rr.hook(s)
	s.setNet(projsync.PFRemoteNetGood, clock.now())
	first := make(chan struct{})
	go func() { s.tick(context.Background(), "timer"); close(first) }()
	deadline := time.After(2 * time.Second)
	for !s.status().Running {
		select {
		case <-deadline:
			t.Fatal("第一轮没进入运行状态")
		case <-time.After(time.Millisecond):
		}
	}
	s.tick(context.Background(), "wake") // 上一轮还在跑
	if got := s.status(); !strings.Contains(got.LastAction, "上一轮还在进行") {
		t.Fatalf("第二次应跳过: %+v", got)
	}
	close(rr.block)
	<-first
	if rr.len() != 1 {
		t.Fatalf("只该跑一轮，跑了 %d 轮", rr.len())
	}
	if got := s.status(); !strings.Contains(got.LastAction, "跑了") || got.LastRound == nil {
		t.Fatalf("第一轮结果没记下: %+v", got)
	}
	s.Close()
}

func TestProjfilesTouchedTriggersRound(t *testing.T) {
	s, clock, _ := svcWithRealSched(t)
	var rr pfRounds
	rr.hook(s)
	s.setNet(projsync.PFRemoteNetGood, clock.now())
	s.tick(context.Background(), "timer") // 第一轮：全量（touched 还没记）
	sched := s.getSched()
	sched.MarkTouched("BETA", "ALPHA")
	clock.add(11 * time.Second) // 去抖到期
	if got := sched.PendingTouched(); len(got) != 2 {
		t.Fatalf("两条 touched 都应到期: %v", got)
	}
	s.tick(context.Background(), "wake")
	opts := rr.all()
	if len(opts) != 2 {
		t.Fatalf("应跑两轮: %+v", opts)
	}
	if strings.Join(opts[1].Only, ",") != "ALPHA,BETA" {
		t.Fatalf("touched 那轮应只跑这两个单元: %v", opts[1].Only)
	}
	if got := s.status(); !strings.Contains(got.LastAction, "跑了") {
		t.Fatalf("状态应记跑了: %+v", got)
	}
	// OnTouchedDue 会把服务叫醒（不阻塞）
	select {
	case <-s.wake:
	default:
		t.Error("touched 去抖到期应唤醒调度循环")
	}
	s.Close()
}

func TestProjfilesDisabledInSettingsSkipsRound(t *testing.T) {
	s, clock, _ := pfSvcT(t, nil)
	pfWriteSettings(t, s.prep.settingsPath, map[string]any{"enabled": true})
	var rr pfRounds
	rr.hook(s)
	s.setNet(projsync.PFRemoteNetGood, clock.now())
	s.tick(context.Background(), "timer")
	pfWriteSettings(t, s.prep.settingsPath, map[string]any{"enabled": false})
	s.tick(context.Background(), "timer")
	if rr.len() != 1 {
		t.Fatalf("设置关掉后不该跑: %d 轮", rr.len())
	}
	if got := s.status(); !strings.Contains(got.LastAction, "enabled=false") {
		t.Fatalf("状态要说清为什么没跑: %+v", got)
	}
	// 参数已转给调度器
	if got := s.getSched(); got == nil {
		t.Fatal("调度器没建成")
	}
	s.Close()
}

func TestProjfilesLimitsAndSettingsForward(t *testing.T) {
	s, clock, _ := pfSvcT(t, nil)
	pfWriteSettings(t, s.prep.settingsPath, map[string]any{
		"enabled": true, "max_seconds_expensive": 5, "max_seconds_user": 120,
		"limit_kbps_good": 800, "limit_kbps_expensive": 64, "quiet_seconds": 3,
	})
	var rr pfRounds
	rr.hook(s)
	s.setNet(projsync.PFRemoteNetGood, clock.now())
	s.tick(context.Background(), "timer")
	tr := s.transfer
	if tr == nil {
		t.Fatal("传输器没建成")
	}
	for _, d := range []projsync.PFDirection{projsync.PFUpload, projsync.PFDownload} {
		if got := tr.Limit(d); got != 800*1024 {
			t.Errorf("%s 限速应是 800KB/s，实为 %d", d, got)
		}
	}
	// 转热点：换贵的限速
	s.setNet(projsync.PFRemoteNetExpensive, clock.now())
	s.tick(context.Background(), "timer")
	for _, d := range []projsync.PFDirection{projsync.PFUpload, projsync.PFDownload} {
		if got := tr.Limit(d); got != 64*1024 {
			t.Errorf("热点下 %s 限速应是 64KB/s，实为 %d", d, got)
		}
	}
	// 断网不跑，也不改限速
	s.setNet(projsync.PFRemoteNetOffline, clock.now())
	s.tick(context.Background(), "timer")
	for _, d := range []projsync.PFDirection{projsync.PFUpload, projsync.PFDownload} {
		if got := tr.Limit(d); got != 64*1024 {
			t.Errorf("断网不该动限速: %d", got)
		}
	}
	if rr.len() != 2 {
		t.Fatalf("断网那轮不该跑: %d", rr.len())
	}
	s.Close()
}

// T14d：限速设置真正生效——good=0 不限、expensive/unknown 按 300KB/s；setNet 切换立刻改（不等下一轮）；
// 改设置文件下一轮生效；状态里 limit_bps 能看到当前生效值。
func TestProjfilesLimitFollowsNetAndSettingsFile(t *testing.T) {
	s, clock, _ := pfSvcT(t, nil)
	pfWriteSettings(t, s.prep.settingsPath, map[string]any{
		"enabled": true, "limit_kbps_good": 0, "limit_kbps_expensive": 300,
	})
	var rr pfRounds
	rr.hook(s)
	dirs := []projsync.PFDirection{projsync.PFUpload, projsync.PFDownload}
	s.setNet(projsync.PFRemoteNetGood, clock.now())
	s.tick(context.Background(), "timer")
	tr := s.transfer
	if tr == nil {
		t.Fatal("传输器没建成")
	}
	for _, d := range dirs {
		if got := tr.Limit(d); got != 0 {
			t.Errorf("good 网络 %s 应不限速，实为 %d", d, got)
		}
	}
	if got := s.status().LimitBps; got != 0 {
		t.Errorf("状态 limit_bps 应是 0，实为 %d", got)
	}
	// 切热点：setNet 当下就把限速刷上，不用等下一轮
	s.setNet(projsync.PFRemoteNetExpensive, clock.now())
	for _, d := range dirs {
		if got := tr.Limit(d); got != 300*1024 {
			t.Errorf("切热点后 %s 限速应是 300KB/s，实为 %d", d, got)
		}
	}
	if got := s.status().LimitBps; got != 300*1024 {
		t.Errorf("状态 limit_bps 应是 300KB/s，实为 %d", got)
	}
	// unknown 按 expensive
	s.setNet(projsync.PFRemoteNetUnknown, clock.now())
	for _, d := range dirs {
		if got := tr.Limit(d); got != 300*1024 {
			t.Errorf("unknown 应按 expensive，%s 实为 %d", d, got)
		}
	}
	// 改设置文件：下一轮生效
	pfWriteSettings(t, s.prep.settingsPath, map[string]any{
		"enabled": true, "limit_kbps_good": 0, "limit_kbps_expensive": 120,
	})
	s.tick(context.Background(), "timer")
	for _, d := range dirs {
		if got := tr.Limit(d); got != 120*1024 {
			t.Errorf("改设置后下一轮 %s 限速应是 120KB/s，实为 %d", d, got)
		}
	}
	if got := s.status().LimitBps; got != 120*1024 {
		t.Errorf("状态 limit_bps 应跟着刷成 120KB/s，实为 %d", got)
	}
	// 切回 good：0 = 不限
	s.setNet(projsync.PFRemoteNetGood, clock.now())
	for _, d := range dirs {
		if got := tr.Limit(d); got != 0 {
			t.Errorf("切回 good 后 %s 应不限速，实为 %d", d, got)
		}
	}
	if rr.len() < 2 {
		t.Errorf("两轮都该跑: %d", rr.len())
	}
	s.Close()
}

// ── 状态文件 ───────────────────────────────────────────

func TestProjfilesStatusFileWritten(t *testing.T) {
	s, clock, _ := pfSvcT(t, nil)
	var rr pfRounds
	rr.hook(s)
	s.setNet(projsync.PFRemoteNetGood, clock.now())
	s.startupOnce(context.Background())
	s.tick(context.Background(), "timer")
	b, err := os.ReadFile(s.prep.statusPath)
	if err != nil {
		t.Fatalf("状态文件没写出来: %v", err)
	}
	var got projfilesStatus
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("状态文件不是合法 JSON: %v", err)
	}
	if got.Host != "ubuntu" || got.RemoteRoot != "/srv/XXYI-sync" || got.RemoteMeta != "/srv/.xxyi/projfiles" || got.CoordID == "" {
		t.Fatalf("配置没进状态: %+v", got)
	}
	if got.LastRound == nil || got.LastRound.Outcome != projsync.PFSchedDone {
		t.Fatalf("最近一轮没记进状态: %+v", got)
	}
	if len(got.SettingsProblems) == 0 || got.Settings.PollSeconds != 60 {
		t.Fatalf("设置缺省与问题没进状态: %+v", got)
	}
	if !got.StartedUp {
		t.Errorf("启动恢复没记: %+v", got)
	}
	// 0600
	if fi, err := os.Stat(s.prep.statusPath); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("状态文件权限应是 0600: %v %v", fi.Mode(), err)
	}
	// 目录里没有残留的临时文件
	st, err := os.ReadDir(filepath.Dir(s.prep.statusPath))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range st {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("留下临时文件 %s", e.Name())
		}
	}
	// 启动恢复的结果：假 Runner 报错时也要如实记下
	if got.StartupError != "" {
		t.Errorf("启动恢复没碰远端，不该有错误: %s", got.StartupError)
	}
	s.Close()
}

func TestProjfilesStartupReportsRecovery(t *testing.T) {
	s, _, _ := pfSvcT(t, nil)
	s.startUp = func(ctx context.Context) ([]projsync.PFUnitMoveResult, error) {
		return []projsync.PFUnitMoveResult{{Status: projsync.PFUnitMoveDone}}, nil
	}
	s.runRound = func(ctx context.Context, opt projsync.PFSchedRoundOptions) *projsync.PFSchedReport {
		return &projsync.PFSchedReport{Outcome: projsync.PFSchedDone, Net: projsync.PFRemoteNetGood}
	}
	s.startupOnce(context.Background())
	got := s.status()
	if !got.StartedUp || len(got.Startup) != 1 || got.StartupError != "" {
		t.Fatalf("启动恢复结果没进状态: %+v", got)
	}
	if !strings.Contains(got.LastAction, "启动恢复") {
		t.Fatalf("状态没说清启动做了什么: %+v", got)
	}
	// 只恢复一次
	n := 0
	s.startUp = func(ctx context.Context) ([]projsync.PFUnitMoveResult, error) { n++; return nil, nil }
	s.startupOnce(context.Background())
	if n != 0 {
		t.Error("启动恢复应只做一次")
	}
	// 恢复失败也要如实记下
	s2, _, _ := pfSvcT(t, nil)
	s2.startUp = func(ctx context.Context) ([]projsync.PFUnitMoveResult, error) {
		return nil, errors.New("登记表读不了")
	}
	s2.runRound = func(ctx context.Context, opt projsync.PFSchedRoundOptions) *projsync.PFSchedReport {
		return &projsync.PFSchedReport{Outcome: projsync.PFSchedDone}
	}
	s2.startupOnce(context.Background())
	if got := s2.status(); got.StartupError != "登记表读不了" {
		t.Fatalf("启动恢复失败没记下: %+v", got)
	}
	s.Close()
	s2.Close()
}

func TestProjfilesLoopStopsCleanly(t *testing.T) {
	s, clock, _ := pfSvcT(t, nil)
	var rr pfRounds
	rr.hook(s)
	s.setNet(projsync.PFRemoteNetGood, clock.now())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.wait = func(ctx context.Context, d time.Duration) (string, error) { <-ctx.Done(); return "", ctx.Err() }
	go func() { s.loop(ctx); close(done) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("退出时没干净停止")
	}
	if rr.len() != 0 || s.status().Running {
		t.Fatalf("没跑过就该是干净的: %+v", s.status())
	}
	_ = clock
	s.Close()
}

// ── remote_root 从设置读（PF-remote-root）────────────────

// 设置文件里 remote_root 的解析：合法就用（结尾斜杠去掉）、空/没写就留空（交回 hosts）、坏值只报这一个问题。
func TestProjfilesSettingsRemoteRoot(t *testing.T) {
	p := filepath.Join(t.TempDir(), "projfiles-settings.json")
	// 没写这个键：空串，不报错（老配置继续用 hosts.json 的 remote_root）
	pfWriteSettings(t, p, map[string]any{"enabled": true, "sync_dir": "/srv/sync"})
	if st, problems := loadProjfilesSettings(p); st.RemoteRoot != "" || len(problems) != 0 {
		t.Fatalf("没写 remote_root 应留空不报错: %+v %#v", st, problems)
	}
	// 合法：照抄（并去掉结尾多余斜杠，与 hosts 段同一处理）
	pfWriteSettings(t, p, map[string]any{"remote_root": "/srv/XXYI-ubuntu-sync//"})
	if st, problems := loadProjfilesSettings(p); st.RemoteRoot != "/srv/XXYI-ubuntu-sync" || len(problems) != 0 {
		t.Fatalf("合法的 remote_root 没照抄/没去斜杠: %+v %#v", st, problems)
	}
	// 空串：不算问题（表示交回 hosts）
	pfWriteSettings(t, p, map[string]any{"remote_root": "   "})
	if st, problems := loadProjfilesSettings(p); st.RemoteRoot != "" || len(problems) != 0 {
		t.Fatalf("空 remote_root 应留空不报错: %+v %#v", st, problems)
	}
	// 坏值：只报这一个问题、留空；其它键照常
	for _, bad := range []string{"srv/x", "/srv/../x", "/srv/x y", "/srv/中文"} {
		pfWriteSettings(t, p, map[string]any{"remote_root": bad, "sync_dir": "/srv/sync", "recycle_days": 7})
		st, problems := loadProjfilesSettings(p)
		if st.RemoteRoot != "" {
			t.Errorf("坏值 remote_root=%q 不该照抄: %+v", bad, st)
		}
		if st.SyncDir != "/srv/sync" || st.RecycleDays != 7 {
			t.Errorf("remote_root 坏了不该连累别的键: %+v", st)
		}
		if len(problems) != 1 || !strings.Contains(problems[0], "remote_root") {
			t.Errorf("应只报一条 remote_root 的问题: %#v", problems)
		}
	}
}

// 生效的 remote_root 与来源：没写时用 hosts 的（来源 hosts）、写了用设置里的（来源 settings），
// 状态里 remote_root / remote_root_from 与 active_remote_root 都要跟着对。
func TestProjfilesRemoteRootEffectiveAndSource(t *testing.T) {
	s, _, _ := pfSvcT(t, nil) // hosts.json 的 remote_root = /srv/XXYI-sync
	// 没读设置时：生效的是 hosts 里的
	s.applySettings(projfilesDefaults(), nil)
	if st := s.status(); st.RemoteRoot != "/srv/XXYI-sync" || st.RemoteRootFrom != "hosts" {
		t.Fatalf("没配设置里的 remote_root 时应报 hosts 的来源: %+v", st)
	}
	// 设置里写了合法的：生效值换成它、来源变 settings
	path := filepath.Join(t.TempDir(), "s.json")
	pfWriteSettings(t, path, map[string]any{"remote_root": "/srv/XXYI-new"})
	st, problems := loadProjfilesSettings(path)
	if len(problems) != 0 {
		t.Fatalf("合格设置不该报问题: %#v", problems)
	}
	s.applySettings(st, problems)
	if got := s.status(); got.RemoteRoot != "/srv/XXYI-new" || got.RemoteRootFrom != "settings" {
		t.Fatalf("设置里的 remote_root 应生效并标来源 settings: %+v", got)
	}
	// build 之后：Ubuntu 客户端绑的是生效的那个根，active_remote_root 报的是它
	if err := s.build(st); err != nil {
		t.Fatalf("build 失败: %v", err)
	}
	if r := s.remoteNow(); r == nil || r.Root != "/srv/XXYI-new" {
		t.Fatalf("PFRemoteClient.Root 没绑生效的根: %+v", r)
	}
	if ar, arf := s.activeRemote(); ar != "/srv/XXYI-new" || arf != "settings" {
		t.Fatalf("状态里的在用同步根不对: %q %q", ar, arf)
	}
	if got := s.status(); got.ActiveRemoteRoot != "/srv/XXYI-new" || got.ActiveRemoteRootFrom != "settings" {
		t.Fatalf("状态没报在用的根与来源: %+v", got)
	}
	s.Close()
}

// 改了设置里的 remote_root：跟 sync_dir 一样进切换期（写接口拒绝）、下一轮 acquire 后整套重建，
// 重建后绑的是新根、来源变 settings、切换期结束。
func TestProjfilesRemoteRootSwitchRebuild(t *testing.T) {
	s, clock, dir := pfSvcT(t, nil)
	rec := &pfRecMovers{}
	s.buildMover = rec.build
	rr := &pfRounds{}
	rr.hook(s)
	s.setNet(projsync.PFRemoteNetGood, clock.now())
	// 第一轮：设置里没写 remote_root → 用 hosts 的根
	pfWriteSettings(t, s.prep.settingsPath, map[string]any{
		"work_dir": filepath.Join(dir, "work"), "sync_dir": filepath.Join(dir, "sync"), "recycle_days": 3,
	})
	s.tick(context.Background(), "timer")
	if got := s.status(); got.RemoteRootFrom != "hosts" || got.ActiveRemoteRoot != "/srv/XXYI-sync" || got.SwitchBusy {
		t.Fatalf("第一轮应绑 hosts 的根、无切换: %+v", got)
	}
	// 改设置里的 remote_root：立刻要重建（含「改了但 tick 还没跑到」的窗口）
	pfWriteSettings(t, s.prep.settingsPath, map[string]any{
		"work_dir": filepath.Join(dir, "work"), "sync_dir": filepath.Join(dir, "sync"),
		"remote_root": "/srv/XXYI-ubuntu-sync", "recycle_days": 3,
	})
	st, _ := loadProjfilesSettings(s.prep.settingsPath)
	if !s.needRebuild(st) {
		t.Fatal("改了 remote_root 应触发整套重建")
	}
	// 重建前状态报的必须还是在用的旧根
	if got := s.status(); got.ActiveRemoteRoot != "/srv/XXYI-sync" {
		t.Fatalf("重建前 active_remote_root 必须还是旧根: %+v", got)
	}
	// 下一轮：整套重建，绑新根、来源 settings、切换期结束
	s.tick(context.Background(), "timer")
	if got := s.status(); got.ActiveRemoteRoot != "/srv/XXYI-ubuntu-sync" || got.ActiveRemoteRootFrom != "settings" ||
		got.RemoteRoot != "/srv/XXYI-ubuntu-sync" || got.RemoteRootFrom != "settings" || got.SwitchBusy {
		t.Fatalf("重建后要绑新根、来源 settings、切换期结束: %+v", got)
	}
	if r := s.remoteNow(); r == nil || r.Root != "/srv/XXYI-ubuntu-sync" {
		t.Fatalf("重建后 PFRemoteClient.Root 必须是新根: %+v", r)
	}
	if n := rec.n(); n != 2 {
		t.Fatalf("两轮各建一套搬运器，实为 %d", n)
	}
	_ = rr
	s.Close()
}
