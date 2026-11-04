package wgtund

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/WillyChen0811/base/go/projsync"
)

// ── 配置 ─────────────────────────────────────────────

func TestProjsyncV2Config(t *testing.T) {
	// 没有 v2 段：关闭，间隔 5，缺省 xxyi-msg 路径
	p, err := prepareProjsync(projConfT(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if p.v2.enabled || p.v2.interval != 5*time.Minute || p.v2.xxyiMsg != "~/.local/bin/xxyi-msg" {
		t.Fatalf("缺省: %+v", p.v2)
	}
	// v2 段里没写 enabled：仍然关闭
	p, err = prepareProjsync(projConfT(t, func(pj, _ map[string]any, _ *hostConf) { pj["v2"] = map[string]any{"interval_minutes": 9} }))
	if err != nil || p.v2.enabled || p.v2.interval != 9*time.Minute {
		t.Fatalf("enabled 缺省应为 false: %v %+v", err, p.v2)
	}
	// 小于 5 一律按 5（含 0、负数、1、4）；5 与更大的保持
	for _, c := range []struct{ in, want int }{{0, 5}, {-3, 5}, {1, 5}, {4, 5}, {5, 5}, {7, 7}, {60, 60}} {
		p, err := prepareProjsync(projConfT(t, func(pj, _ map[string]any, _ *hostConf) {
			pj["v2"] = map[string]any{"enabled": true, "interval_minutes": c.in}
		}))
		if err != nil || !p.v2.enabled || p.v2.interval != time.Duration(c.want)*time.Minute {
			t.Fatalf("interval_minutes=%d: %v %+v", c.in, err, p.v2)
		}
	}
	// xxyi_msg 字符集
	for _, bad := range []string{"a b", "x;rm -rf /", "$(id)", "-x", "a'b", "a\nb"} {
		_, err := prepareProjsync(projConfT(t, func(pj, _ map[string]any, _ *hostConf) { pj["v2"] = map[string]any{"xxyi_msg": bad} }))
		if err == nil || !strings.Contains(err.Error(), "hosts.json: projsync 的 v2") || !strings.Contains(err.Error(), "xxyi_msg") {
			t.Errorf("xxyi_msg=%q 应被拒绝: %v", bad, err)
		}
	}
	p, err = prepareProjsync(projConfT(t, func(pj, _ map[string]any, _ *hostConf) {
		pj["v2"] = map[string]any{"xxyi_msg": "/opt/bin/xxyi-msg_2.x~"}
	}))
	if err != nil || p.v2.xxyiMsg != "/opt/bin/xxyi-msg_2.x~" {
		t.Fatalf("%v %+v", err, p.v2)
	}
}

// ── 调度 ─────────────────────────────────────────────

func v2Svc(t *testing.T, enabled bool) *projsyncV2Svc {
	t.Helper()
	c := projConfT(t, func(pj, p0 map[string]any, _ *hostConf) {
		pj["projects"] = []any{p0,
			map[string]any{"name": "beta", "local": p0["local"], "remote": "/home/chenwy/beta"}}
		pj["v2"] = map[string]any{"enabled": enabled, "interval_minutes": 5}
	})
	p, err := prepareProjsync(c)
	if err != nil {
		t.Fatal(err)
	}
	s := newProjsyncV2Svc(newProjsyncSvc(p, nil, "127.0.0.1:9"), nil)
	s.statusPath = filepath.Join(t.TempDir(), "sub", projsyncV2StatusFile)
	return s
}

// 分支对账基线目录固定在 ~/.xxyi/projsync/base，并传给每一轮。
func TestProjsyncV2EnvCarriesBaseDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	s := v2Svc(t, false)
	env := s.env(s.v1.prep.targets[0], nil)
	if want := filepath.Join(home, ".xxyi", "projsync", "base"); env.BaseDir != want {
		t.Fatalf("BaseDir=%q，期望 %q", env.BaseDir, want)
	}
}

func TestProjsyncV2DisabledDoesNotRunAutomatically(t *testing.T) {
	s := v2Svc(t, false)
	var n int
	s.round = func(ctx context.Context, env projsync.RoundEnv) projsync.RoundResult {
		n++
		return projsync.RoundResult{}
	}
	s.initialDelay, s.interval = time.Millisecond, time.Millisecond
	done := make(chan struct{})
	go func() { s.loop(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("enabled=false 时 loop 应立即返回")
	}
	time.Sleep(30 * time.Millisecond)
	if n != 0 || s.status().NextRun != nil || s.status().Enabled {
		t.Fatalf("不应自动跑: n=%d %+v", n, s.status())
	}
	// 手动接口仍可用
	if err := s.Start(context.Background(), "alpha", nil); err != nil {
		t.Fatal(err)
	}
	pollUntil(t, 3*time.Second, "手动跑完", func() bool { return s.status().Projects[0].Last != nil })
	if n != 1 {
		t.Fatalf("n=%d", n)
	}
}

func TestProjsyncV2EnabledRunsProjectsSequentially(t *testing.T) {
	s := v2Svc(t, true)
	var mu sync.Mutex
	var order []string
	cur, maxCur := 0, 0
	s.round = func(ctx context.Context, env projsync.RoundEnv) projsync.RoundResult {
		mu.Lock()
		cur++
		if cur > maxCur {
			maxCur = cur
		}
		order = append(order, env.Project)
		mu.Unlock()
		time.Sleep(15 * time.Millisecond)
		mu.Lock()
		cur--
		mu.Unlock()
		return projsync.RoundResult{Project: env.Project, Summary: "ok"}
	}
	s.initialDelay, s.interval = 5*time.Millisecond, 30*time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.loop(ctx); close(done) }()
	pollUntil(t, 5*time.Second, "两个项目跑了两轮", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(order) >= 4
	})
	st := s.status()
	if st.NextRun == nil || !st.Enabled || st.Projects[0].Last == nil || st.Projects[1].Last == nil {
		t.Fatalf("%+v", st)
	}
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if maxCur != 1 {
		t.Fatalf("项目应依次进行，最大并发 %d", maxCur)
	}
	if order[0] != "alpha" || order[1] != "beta" || order[2] != "alpha" {
		t.Fatalf("顺序: %v", order)
	}
	// 状态文件：0600、内容是合法 JSON
	fi, err := os.Stat(s.statusPath)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("状态文件: %v %v", fi, err)
	}
	b, _ := os.ReadFile(s.statusPath)
	var back projsyncV2Status
	if err := json.Unmarshal(b, &back); err != nil || len(back.Projects) != 2 {
		t.Fatalf("状态文件内容: %v %s", err, b)
	}
	if ents, _ := os.ReadDir(filepath.Dir(s.statusPath)); len(ents) != 1 {
		t.Fatalf("不应留下临时文件: %v", ents)
	}
}

func TestProjsyncV2SkippedRoundRecordedSeparately(t *testing.T) {
	s := v2Svc(t, false)
	s.round = func(ctx context.Context, env projsync.RoundEnv) projsync.RoundResult {
		return projsync.RoundResult{Project: env.Project, Skipped: true, SkipReason: "上一轮还在进行"}
	}
	tt, _ := s.v1.target("alpha")
	s.acquire("alpha")
	s.execute(context.Background(), tt, nil)
	it := s.status().Projects[0]
	if it.Last != nil || it.LastSkip == nil || it.LastSkip.SkipReason != "上一轮还在进行" || it.Running {
		t.Fatalf("%+v", it)
	}
}

// ── 对话记录同步（每轮代码同步之后补做一次）────────────────

func v2OkRound(env0 projsync.RoundEnv) projsync.RoundResult {
	return projsync.RoundResult{Project: env0.Project, Summary: "代码同步 ok"}
}

func TestProjsyncV2ClaudeSyncAfterCodeRound(t *testing.T) {
	s := v2Svc(t, false)
	s.round = func(ctx context.Context, env projsync.RoundEnv) projsync.RoundResult { return v2OkRound(env) }
	tt, _ := s.v1.target("alpha")
	var gotTarget projsyncTarget
	s.claudeSync = func(ctx context.Context, tgt projsyncTarget) (*projsync.FileResult, error) {
		gotTarget = tgt
		return &projsync.FileResult{
			Uploaded:   []string{"a.jsonl"},
			Downloaded: []string{"b.jsonl", "c.jsonl"},
			Same:       2,
			Conflicts:  []projsync.Conflict{{Path: "d.jsonl", Reason: "两边都有对方没有的改动"}},
		}, nil
	}
	res := s.execute(context.Background(), tt, nil)
	if gotTarget.name != "alpha" {
		t.Fatalf("claudeSync 应拿到本项目: %+v", gotTarget)
	}
	if res.Claude == nil || res.ClaudeError != "" {
		t.Fatalf("Claude=%+v ClaudeError=%q", res.Claude, res.ClaudeError)
	}
	want := "对话记录：拷到对方 1 个、拉回 2 个、相同 2 个、冲突 1 个"
	if len(res.Notes) != 1 || res.Notes[0] != want {
		t.Fatalf("Notes: %v，期望只有一条 %q", res.Notes, want)
	}
	if res.Error != "" || res.Stopped || res.NeedsAttention || res.Summary != "代码同步 ok" {
		t.Fatalf("代码同步的结论被动过: %+v", res)
	}
	// 状态接口的 JSON 里能看到 claude 字段
	b, err := json.Marshal(s.status())
	if err != nil || !strings.Contains(string(b), `"claude"`) {
		t.Fatalf("状态 JSON 应含 claude: %v %s", err, b)
	}
	var back projsyncV2Status
	if err := json.Unmarshal(b, &back); err != nil || len(back.Projects) != 2 || back.Projects[0].Last == nil || back.Projects[0].Last.Claude == nil {
		t.Fatalf("状态反序列化后 Last.Claude 应在: %v %s", err, b)
	}
}

func TestProjsyncV2ClaudeSyncFailureKeepsCodeResult(t *testing.T) {
	s := v2Svc(t, false)
	s.round = func(ctx context.Context, env projsync.RoundEnv) projsync.RoundResult { return v2OkRound(env) }
	tt, _ := s.v1.target("alpha")
	// 先跑一次没有对话记录同步的，作为「代码结果应与它完全相同」的基线
	s.claudeSync = nil
	base := s.execute(context.Background(), tt, nil)
	s.claudeSync = func(ctx context.Context, _ projsyncTarget) (*projsync.FileResult, error) {
		return nil, fmt.Errorf("SFTP 连接断开")
	}
	res := s.execute(context.Background(), tt, nil)
	if res.Claude != nil || res.ClaudeError != "SFTP 连接断开" {
		t.Fatalf("Claude=%+v ClaudeError=%q", res.Claude, res.ClaudeError)
	}
	if res.Error != base.Error || res.Stopped != base.Stopped || res.NeedsAttention != base.NeedsAttention ||
		res.Summary != base.Summary || fmt.Sprint(res.Attention) != fmt.Sprint(base.Attention) {
		t.Fatalf("对话记录失败改了代码同步结论: %+v vs %+v", res, base)
	}
	want := "对话记录同步失败：SFTP 连接断开（代码同步不受影响）"
	if len(res.Notes) != len(base.Notes)+1 || res.Notes[len(res.Notes)-1] != want {
		t.Fatalf("Notes: %v，期望末尾是 %q", res.Notes, want)
	}
	if b, _ := json.Marshal(res); !strings.Contains(string(b), `"claude_error":"SFTP 连接断开"`) {
		t.Fatalf("JSON 应含 claude_error: %s", b)
	}
}

func TestProjsyncV2ClaudeSyncPanicBecomesClaudeError(t *testing.T) {
	s := v2Svc(t, false)
	s.round = func(ctx context.Context, env projsync.RoundEnv) projsync.RoundResult { return v2OkRound(env) }
	tt, _ := s.v1.target("alpha")
	s.claudeSync = func(ctx context.Context, _ projsyncTarget) (*projsync.FileResult, error) {
		panic("假崩溃")
	}
	res := s.execute(context.Background(), tt, nil) // 不能 panic 出来
	if res.Claude != nil || res.ClaudeError == "" || !strings.Contains(res.ClaudeError, "假崩溃") {
		t.Fatalf("Claude=%+v ClaudeError=%q", res.Claude, res.ClaudeError)
	}
	if res.Error != "" || res.Stopped || res.NeedsAttention || res.Summary != "代码同步 ok" {
		t.Fatalf("panic 改了代码同步结论: %+v", res)
	}
	last := res.Notes[len(res.Notes)-1]
	if !strings.Contains(last, "对话记录同步失败") || !strings.Contains(last, "假崩溃") || !strings.HasSuffix(last, "（代码同步不受影响）") {
		t.Fatalf("Notes 末尾: %q", last)
	}
}

func TestProjsyncV2ClaudeSyncNotRunOnSkippedOrOutOfScope(t *testing.T) {
	s := v2Svc(t, false)
	tt, _ := s.v1.target("alpha")
	var called int
	s.claudeSync = func(ctx context.Context, _ projsyncTarget) (*projsync.FileResult, error) { called++; return nil, nil }
	s.round = func(ctx context.Context, env projsync.RoundEnv) projsync.RoundResult {
		return projsync.RoundResult{Project: env.Project, Skipped: true, SkipReason: "上一轮还在进行", Summary: "上一轮还在进行"}
	}
	res := s.execute(context.Background(), tt, nil)
	if called != 0 || res.Claude != nil || res.ClaudeError != "" || len(res.Notes) != 0 {
		t.Fatalf("Skipped 轮不应碰对话记录: called=%d %+v", called, res)
	}
	s.round = func(ctx context.Context, env projsync.RoundEnv) projsync.RoundResult {
		return projsync.RoundResult{Project: env.Project, OutOfScope: true, Error: "超出自动同步范围，本轮不写任何东西", Summary: "超出自动同步范围，本轮不写任何东西"}
	}
	res = s.execute(context.Background(), tt, nil)
	if called != 0 || res.Claude != nil || res.ClaudeError != "" || len(res.Notes) != 0 {
		t.Fatalf("OutOfScope 轮不应碰对话记录: called=%d %+v", called, res)
	}
}

func TestProjsyncV2ClaudeSyncNilSkips(t *testing.T) {
	s := v2Svc(t, false)
	s.round = func(ctx context.Context, env projsync.RoundEnv) projsync.RoundResult { return v2OkRound(env) }
	tt, _ := s.v1.target("alpha")
	s.claudeSync = nil
	res := s.execute(context.Background(), tt, nil) // 不能崩
	if res.Claude != nil || res.ClaudeError != "" || len(res.Notes) != 0 {
		t.Fatalf("claudeSync 为 nil 应整体跳过: %+v", res)
	}
	if res.Summary != "代码同步 ok" {
		t.Fatalf("%+v", res)
	}
}

// ── 接口 ─────────────────────────────────────────────

func v2Do(t *testing.T, srv *httptest.Server, method, path, origin string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, nil)
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

func TestProjsyncV2LocalAPI(t *testing.T) {
	s := v2Svc(t, false)
	entered := make(chan projsync.RoundEnv, 4)
	gate := make(chan struct{})
	s.round = func(ctx context.Context, env projsync.RoundEnv) projsync.RoundResult {
		entered <- env
		<-gate
		return projsync.RoundResult{Project: env.Project, Summary: "假的一轮", Attention: []projsync.AttentionItem{}}
	}
	srv := httptest.NewServer(s.localHandler(context.Background(), []string{"127.0.0.1:8765"}))
	defer srv.Close()

	resp, b := v2Do(t, srv, http.MethodGet, "/projsync/v2", "")
	var st projsyncV2Status
	if resp.StatusCode != 200 || json.Unmarshal(b, &st) != nil || len(st.Projects) != 2 || st.Enabled || st.NextRun != nil {
		t.Fatalf("GET: %d %s", resp.StatusCode, b)
	}
	if !strings.Contains(string(b), `"last":null`) || !strings.Contains(string(b), `"next_run":null`) {
		t.Fatalf("未跑过应 last/next_run 为 null: %s", b)
	}
	// 方法
	if r, _ := v2Do(t, srv, http.MethodPost, "/projsync/v2", testOrigin); r.StatusCode != 405 {
		t.Errorf("POST /projsync/v2 应 405: %d", r.StatusCode)
	}
	for _, p := range []string{"/projsync/v2/run?name=alpha", "/projsync/v2/commit?name=alpha&side=mac&worktree=/x"} {
		if r, _ := v2Do(t, srv, http.MethodGet, p, testOrigin); r.StatusCode != 405 {
			t.Errorf("GET %s 应 405: %d", p, r.StatusCode)
		}
		// Origin：没有 / 白名单外都 403
		if r, _ := v2Do(t, srv, http.MethodPost, p, ""); r.StatusCode != 403 {
			t.Errorf("无 Origin %s 应 403: %d", p, r.StatusCode)
		}
		if r, _ := v2Do(t, srv, http.MethodPost, p, "http://evil.example"); r.StatusCode != 403 {
			t.Errorf("坏 Origin %s 应 403: %d", p, r.StatusCode)
		}
	}
	if r, _ := v2Do(t, srv, http.MethodPost, "/projsync/v2/run?name=zzz", testOrigin); r.StatusCode != 404 {
		t.Errorf("未知项目应 404: %d", r.StatusCode)
	}
	for _, q := range []string{"side=root&worktree=/x", "side=mac&worktree=rel/path", "side=mac&worktree=", "side=&worktree=/x"} {
		if r, _ := v2Do(t, srv, http.MethodPost, "/projsync/v2/commit?name=alpha&"+q, testOrigin); r.StatusCode != 400 {
			t.Errorf("%s 应 400: %d", q, r.StatusCode)
		}
	}
	// 合法 run：202；运行中 409；放行后出现 last
	if r, b := v2Do(t, srv, http.MethodPost, "/projsync/v2/run?name=alpha", testOrigin); r.StatusCode != 202 || !strings.Contains(string(b), `"started":true`) {
		t.Fatalf("run: %d %s", r.StatusCode, b)
	}
	env := <-entered
	if len(env.Force) != 0 || env.Project != "alpha" || env.XxyiMsg != "~/.local/bin/xxyi-msg" || env.RemoteNS != "ubuntu" {
		t.Fatalf("env: %+v", env)
	}
	if r, _ := v2Do(t, srv, http.MethodPost, "/projsync/v2/run?name=alpha", testOrigin); r.StatusCode != 409 {
		t.Errorf("同一项目运行中应 409: %d", r.StatusCode)
	}
	if r, _ := v2Do(t, srv, http.MethodPost, "/projsync/v2/commit?name=alpha&side=ubuntu&worktree=/x", testOrigin); r.StatusCode != 409 {
		t.Errorf("commit 运行中也应 409: %d", r.StatusCode)
	}
	_, b = v2Do(t, srv, http.MethodGet, "/projsync/v2", "")
	json.Unmarshal(b, &st)
	if !st.Projects[0].Running {
		t.Fatalf("应在运行: %s", b)
	}
	gate <- struct{}{}
	pollUntil(t, 3*time.Second, "跑完", func() bool { return s.status().Projects[0].Last != nil && !s.status().Projects[0].Running })

	// commit：Force 被带进这一轮的环境
	if r, b := v2Do(t, srv, http.MethodPost, "/projsync/v2/commit?name=beta&side=ubuntu&worktree=/home/chenwy/beta", testOrigin); r.StatusCode != 202 {
		t.Fatalf("commit: %d %s", r.StatusCode, b)
	}
	env = <-entered
	if env.Project != "beta" || len(env.Force) != 1 || env.Force[0].Side != projsync.SideUbuntu || env.Force[0].Worktree != "/home/chenwy/beta" {
		t.Fatalf("Force: %+v", env.Force)
	}
	gate <- struct{}{}
	pollUntil(t, 3*time.Second, "beta 跑完", func() bool { return s.status().Projects[1].Last != nil })
}

func TestProjsyncV2WiredIntoRun(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // 状态文件不得落到真实家目录
	repo := gitRepoDir(t)
	f := newFixture(t, func(cfg map[string]any) {
		cfg["projsync"] = map[string]any{"host": "ubuntu",
			"projects": []any{map[string]any{"name": "alpha", "local": repo, "remote": "/home/chenwy/alpha"}}}
	})
	st, _, _ := run(t, f.dir)
	if r, _ := get(t, st, "/projsync/v2", false); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("不带主令牌应 401: %d", r.StatusCode)
	}
	r, b := get(t, st, "/projsync/v2", true)
	var s projsyncV2Status
	if r.StatusCode != 200 || json.Unmarshal(b, &s) != nil || s.Enabled || len(s.Projects) != 1 {
		t.Fatalf("GET: %d %s", r.StatusCode, b)
	}
	for _, p := range []string{"/projsync/v2/run?name=alpha", "/projsync/v2/commit?name=alpha&side=mac&worktree=/x"} {
		if r, _ := postOrigin(t, st, p, ""); r.StatusCode != 403 {
			t.Errorf("无 Origin 应 403: %s %d", p, r.StatusCode)
		}
	}
	// 没配置 projsync：404
	f0 := newFixture(t, nil)
	st0, _, _ := run(t, f0.dir)
	if r, _ := get(t, st0, "/projsync/v2", true); r.StatusCode != http.StatusNotFound {
		t.Errorf("没配置应 404: %d", r.StatusCode)
	}
}

// ── 手动 commit 接口端到端：真 RunRound + 真仓库，Force 才能提交 ──

type v2UbuRunner struct{}

func (v2UbuRunner) Run(ctx context.Context, cmd string, stdin []byte) ([]byte, []byte, int, error) {
	var out []byte
	var err error
	switch strings.Fields(cmd)[1] {
	case projsync.SubcmdFacts:
		out, err = projsync.ServeFacts(ctx, stdin, func(ctx context.Context, wt string) projsync.AgentVerdict {
			return projsync.AgentVerdict{Verdict: "unknown", Notes: []string{"测试：不确定"}}
		})
	default:
		out, err = projsync.ServeApplyWith(ctx, stdin, func(ctx context.Context, wt string) (bool, string) { return false, "测试：不确定" })
	}
	if err != nil {
		return nil, []byte(err.Error()), 2, nil
	}
	return out, nil, 0, nil
}

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestProjsyncV2ManualCommitForcesCommit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(os.Getenv("HOME"), "xdg"))
	ubu := filepath.Join(t.TempDir(), "ubu")
	os.MkdirAll(ubu, 0o755)
	gitT(t, ubu, "init", "-q", "-b", "main")
	gitT(t, ubu, "config", "user.name", "t")
	gitT(t, ubu, "config", "user.email", "t@t")
	gitT(t, ubu, "config", "commit.gpgsign", "false")
	os.WriteFile(filepath.Join(ubu, "a.txt"), []byte("a\n"), 0o644)
	gitT(t, ubu, "add", ".")
	gitT(t, ubu, "commit", "-q", "-m", "init")
	mac := filepath.Join(t.TempDir(), "mac")
	if out, err := exec.Command("git", "clone", "-q", ubu, mac).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	canonMac, _ := filepath.EvalSymlinks(mac)
	os.WriteFile(filepath.Join(ubu, "wip.txt"), []byte("wip\n"), 0o644)

	c := projConfT(t, func(pj, p0 map[string]any, _ *hostConf) {
		p0["local"], p0["remote"] = canonMac, ubu
	})
	p, err := prepareProjsync(c)
	if err != nil {
		t.Fatal(err)
	}
	v1 := newProjsyncSvc(p, nil, "127.0.0.1:9")
	v1.gitOverride = func(projsyncTarget) (string, []string, error) { return ubu, nil, nil }
	s := newProjsyncV2Svc(v1, nil)
	s.runner = v2UbuRunner{}
	s.statusPath = filepath.Join(t.TempDir(), projsyncV2StatusFile)
	srv := httptest.NewServer(s.localHandler(context.Background(), []string{"127.0.0.1:8765"}))
	defer srv.Close()
	waitLast := func() *projsync.RoundResult {
		pollUntil(t, 20*time.Second, "一轮跑完", func() bool { it := s.status().Projects[0]; return it.Last != nil && !it.Running })
		return s.status().Projects[0].Last
	}

	// 先手动跑一轮：agent 状态不确定，不提交，事项带「提交并同步」
	if r, b := v2Do(t, srv, http.MethodPost, "/projsync/v2/run?name=alpha", testOrigin); r.StatusCode != 202 {
		t.Fatalf("%d %s", r.StatusCode, b)
	}
	res := waitLast()
	if len(res.Attention) != 1 || res.Attention[0].Kind != projsync.AttUncertainDirt || !res.Attention[0].CanCommit {
		t.Fatalf("%+v", res)
	}
	head0 := gitT(t, ubu, "rev-parse", "HEAD")
	if gitT(t, ubu, "status", "--porcelain") != "?? wip.txt" {
		t.Fatal("不确定时不应提交")
	}

	// 用户点「提交并同步」：Force=true，提交后同步到 Mac
	wt := res.Attention[0].Worktree
	s.mu.Lock()
	s.last = map[string]*projsync.RoundResult{}
	s.mu.Unlock()
	q := fmt.Sprintf("/projsync/v2/commit?name=alpha&side=ubuntu&worktree=%s", wt)
	if r, b := v2Do(t, srv, http.MethodPost, q, testOrigin); r.StatusCode != 202 {
		t.Fatalf("%d %s", r.StatusCode, b)
	}
	res = waitLast()
	head1 := gitT(t, ubu, "rev-parse", "HEAD")
	if head1 == head0 || gitT(t, ubu, "status", "--porcelain") != "" {
		t.Fatalf("Force 应已提交: %+v", res)
	}
	if gitT(t, mac, "rev-parse", "HEAD") != head1 || res.NeedsAttention {
		t.Fatalf("提交后应同步到 Mac: %+v", res)
	}
	if len(res.Worktrees) != 1 || res.Worktrees[0].Verdict != "用户指定" {
		t.Fatalf("%+v", res.Worktrees)
	}
}

// ── 超时与不被卡住 ─────────────────────────────────────

// 一个项目的 round 卡死（不理会 ctx）：execute 在超时 + 宽限后仍会返回，Error 含「超时」，
// runAll 里下一个项目照常执行。
func TestProjsyncV2HungRoundTimesOutAndNextProjectRuns(t *testing.T) {
	oldGrace := interruptGrace
	interruptGrace = 50 * time.Millisecond
	defer func() { interruptGrace = oldGrace }()

	s := v2Svc(t, false)
	s.runTimeout = 100 * time.Millisecond
	block := make(chan struct{})
	defer close(block)
	var mu sync.Mutex
	var order []string
	s.round = func(ctx context.Context, env projsync.RoundEnv) projsync.RoundResult {
		mu.Lock()
		order = append(order, env.Project)
		mu.Unlock()
		if env.Project == "alpha" {
			<-block // 永远不返回
		}
		return v2OkRound(env)
	}
	s.claudeSync = nil
	done := make(chan struct{})
	go func() { s.runAll(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("alpha 卡死时 runAll 也应在超时后返回")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[1] != "beta" {
		t.Fatalf("beta 应在 alpha 超时后照常执行: %v", order)
	}
	st := s.status()
	a, b := st.Projects[0], st.Projects[1]
	if a.Running || b.Running {
		t.Fatalf("占位应已释放: %+v", st)
	}
	if a.Last == nil || !strings.Contains(a.Last.Error, "超时") {
		t.Fatalf("alpha 的 Error 应含「超时」: %+v", a.Last)
	}
	if b.Last == nil || b.Last.Error != "" {
		t.Fatalf("beta 应正常: %+v", b.Last)
	}
}

// round 理会 ctx 但自己不写 Error：execute 补上含「超时」的说明。
func TestProjsyncV2RoundCtxExpiredMarksTimeout(t *testing.T) {
	s := v2Svc(t, false)
	s.runTimeout = 50 * time.Millisecond
	s.claudeSync = nil
	s.round = func(ctx context.Context, env projsync.RoundEnv) projsync.RoundResult {
		<-ctx.Done()
		return projsync.RoundResult{Project: env.Project}
	}
	tt, _ := s.v1.target("alpha")
	res := s.execute(context.Background(), tt, nil)
	if !strings.Contains(res.Error, "超时") {
		t.Fatalf("Error 应含「超时」: %+v", res)
	}
}

func TestProjsyncV2DefaultTimeouts(t *testing.T) {
	if projsyncV2RunTimeout != 8*time.Minute || projsyncV2ClaudeTimeout != 3*time.Minute {
		t.Fatal("v2 默认超时应为 8 分钟 / 对话记录 3 分钟")
	}
	if projsyncRunTimeout != 30*time.Minute {
		t.Fatal("v1 常量不应改动")
	}
	s := v2Svc(t, false)
	var dl time.Time
	s.round = func(ctx context.Context, env projsync.RoundEnv) projsync.RoundResult {
		dl, _ = ctx.Deadline()
		return v2OkRound(env)
	}
	s.claudeSync = nil
	tt, _ := s.v1.target("alpha")
	s.execute(context.Background(), tt, nil)
	if left := time.Until(dl); left < 7*time.Minute || left > 8*time.Minute+time.Second {
		t.Fatalf("v2 单项目上限应约 8 分钟，剩 %v", left)
	}
}

// 对话记录同步卡死：只记 ClaudeError（含「超时」），代码同步的结论不变，execute 仍返回。
func TestProjsyncV2ClaudeSyncTimeoutOnlyClaudeError(t *testing.T) {
	oldGrace := interruptGrace
	interruptGrace = 50 * time.Millisecond
	defer func() { interruptGrace = oldGrace }()

	s := v2Svc(t, false)
	s.claudeTimeout = 100 * time.Millisecond
	s.round = func(ctx context.Context, env projsync.RoundEnv) projsync.RoundResult { return v2OkRound(env) }
	block := make(chan struct{})
	defer close(block)
	s.claudeSync = func(ctx context.Context, tgt projsyncTarget) (*projsync.FileResult, error) {
		<-block
		return &projsync.FileResult{}, nil
	}
	tt, _ := s.v1.target("alpha")
	start := time.Now()
	res := s.execute(context.Background(), tt, nil)
	if time.Since(start) > 5*time.Second {
		t.Fatal("execute 应在对话记录超时后返回")
	}
	if !strings.Contains(res.ClaudeError, "超时") || res.Claude != nil {
		t.Fatalf("ClaudeError 应含「超时」: %+v", res)
	}
	if res.Error != "" || res.Summary != "代码同步 ok" {
		t.Fatalf("代码同步结论被动过: %+v", res)
	}
}

// runInterruptible：ctx 到期后调用 closeFn、在宽限内返回「超时」错误；work 的 panic 原样抛给调用方。
func TestRunInterruptible(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	block := make(chan struct{})
	defer close(block)
	closed := make(chan struct{})
	start := time.Now()
	err := runInterruptible(ctx, 50*time.Millisecond, func() { close(closed) }, func() error { <-block; return nil })
	if err == nil || !strings.Contains(err.Error(), "超时") || time.Since(start) > 3*time.Second {
		t.Fatalf("应带「超时」及时返回: %v (%v)", err, time.Since(start))
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("ctx 到期应调用 closeFn 关闭资源")
	}
	// 关闭资源让 work 返回错误：同样带「超时」
	unblock := make(chan struct{})
	ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel2()
	err = runInterruptible(ctx2, time.Second, func() { close(unblock) }, func() error { <-unblock; return errors.New("closed") })
	if err == nil || !strings.Contains(err.Error(), "超时") {
		t.Fatalf("got %v", err)
	}
	// 正常完成不受影响
	if err := runInterruptible(context.Background(), time.Second, func() {}, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("work 的 panic 应传给调用方")
			}
		}()
		runInterruptible(context.Background(), time.Second, func() {}, func() error { panic("x") })
	}()
}

// ── RunOnce（协助通道同步发起） ─────────────────────────

func TestProjsyncV2RunOnce(t *testing.T) {
	s := v2Svc(t, false)
	s.claudeSync = nil
	entered := make(chan struct{}, 2)
	gate := make(chan struct{})
	s.round = func(ctx context.Context, env projsync.RoundEnv) projsync.RoundResult {
		entered <- struct{}{}
		<-gate
		return projsync.RoundResult{Project: env.Project, Summary: "fake 一轮"}
	}
	if _, err := s.RunOnce(context.Background(), "nope"); !errors.Is(err, errProjsyncUnknown) {
		t.Fatalf("未知项目应 errProjsyncUnknown: %v", err)
	}
	type out struct {
		v   any
		err error
	}
	done := make(chan out, 1)
	go func() {
		v, err := s.RunOnce(context.Background(), "alpha")
		done <- out{v, err}
	}()
	<-entered
	if _, err := s.RunOnce(context.Background(), "alpha"); !errors.Is(err, errProjsyncBusy) {
		t.Fatalf("正在跑应 errProjsyncBusy: %v", err)
	}
	close(gate)
	o := <-done
	rr, ok := o.v.(projsync.RoundResult)
	if o.err != nil || !ok || rr.Project != "alpha" || rr.Summary != "fake 一轮" {
		t.Fatalf("应返回 RoundResult: %v %#v", o.err, o.v)
	}
	// 结束后可再跑
	if _, err := s.RunOnce(context.Background(), "alpha"); err != nil {
		t.Fatalf("空闲后再跑应成功: %v", err)
	}
}
