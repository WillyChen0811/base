package wgtund

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/WillyChen0811/base/go/internal/sshtest"
	"github.com/WillyChen0811/base/go/projsync"
	"github.com/WillyChen0811/base/go/sshterm"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// ── 测试辅助 ─────────────────────────────────────────────

// gitRepoDir 建一个 git 仓库顶层目录（projsync 的 local 校验用）。
func gitRepoDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitInit(t, dir)
	return dir
}

// projConfT 直接拼一份带 projsync 段的 fileConf；mut 可改 projsync 整体、第一个项目和主机。
func projConfT(t *testing.T, mut func(pj, p0 map[string]any, h *hostConf)) *fileConf {
	t.Helper()
	pj := map[string]any{"host": "ubuntu",
		"projects": []any{map[string]any{"name": "alpha", "local": gitRepoDir(t), "remote": "/home/chenwy/alpha"}}}
	h := hostConf{Name: "ubuntu", Addr: "127.0.0.1:22", User: "chenwy", IdentityFiles: []string{"/id"}}
	if mut != nil {
		mut(pj, pj["projects"].([]any)[0].(map[string]any), &h)
	}
	b, err := json.Marshal(pj)
	if err != nil {
		t.Fatal(err)
	}
	var pc projsyncConf
	if err := json.Unmarshal(b, &pc); err != nil {
		t.Fatal(err)
	}
	return &fileConf{KnownHosts: "/kh", Hosts: []hostConf{h}, Projsync: &pc}
}

// patchHosts 改已写好的 hosts.json（夹具建好之后补 projsync 段）。
func patchHosts(t *testing.T, dir string, fn func(cfg map[string]any)) {
	t.Helper()
	p := filepath.Join(dir, "hosts.json")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	fn(cfg)
	if b, err = json.Marshal(cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func pollUntil(t *testing.T, d time.Duration, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等到超时: %s", what)
}

type apiItem struct {
	Name    string          `json:"name"`
	Running bool            `json:"running"`
	Last    *projsyncResult `json:"last"`
}

func getProjects(t *testing.T, body []byte) []apiItem {
	t.Helper()
	var st struct {
		Projects []apiItem `json:"projects"`
	}
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatalf("解析 /projsync 响应失败: %v\n%s", err, body)
	}
	return st.Projects
}

// ── 配置校验 ─────────────────────────────────────────────

func TestProjsyncConfigValidation(t *testing.T) {
	long := strings.Repeat("x", 65)
	cases := []struct {
		name string
		mut  func(pj, p0 map[string]any, h *hostConf)
		want string
	}{
		{"主机不存在", func(pj, _ map[string]any, _ *hostConf) { pj["host"] = "nope" }, "不在 hosts 里"},
		{"主机没有私钥", func(_, _ map[string]any, h *hostConf) { h.IdentityFiles = nil }, "identity_files"},
		{"项目名含空格", func(_, p0 map[string]any, _ *hostConf) { p0["name"] = "bad name" }, "项目名"},
		{"项目名含斜杠", func(_, p0 map[string]any, _ *hostConf) { p0["name"] = "a/b" }, "项目名"},
		{"项目名超长", func(_, p0 map[string]any, _ *hostConf) { p0["name"] = long }, "1..64"},
		{"项目名重复", func(pj, p0 map[string]any, _ *hostConf) {
			pj["projects"] = []any{p0, map[string]any{"name": "alpha", "local": p0["local"], "remote": "/r/2"}}
		}, "重复"},
		{"local 不是 git 顶层", func(_, p0 map[string]any, _ *hostConf) { p0["local"] = t.TempDir() }, "不是 git 仓库顶层"},
		{"local 不存在", func(_, p0 map[string]any, _ *hostConf) { p0["local"] = "/nonexistent/x" }, "不存在"},
		{"local 为空", func(_, p0 map[string]any, _ *hostConf) { p0["local"] = "" }, "缺少 local"},
		{"remote 不以 / 开头", func(_, p0 map[string]any, _ *hostConf) { p0["remote"] = "home/chenwy/alpha" }, "以 / 开头"},
		{"remote 含 ..", func(_, p0 map[string]any, _ *hostConf) { p0["remote"] = "/home/../etc" }, ".."},
		{"remote 含非法字符", func(_, p0 map[string]any, _ *hostConf) { p0["remote"] = "/home/a b" }, "不合规"},
		{"local_name 大写", func(pj, _ map[string]any, _ *hostConf) { pj["local_name"] = "Mac" }, "local_name"},
		{"local_name 含下划线", func(pj, _ map[string]any, _ *hostConf) { pj["local_name"] = "my_mac" }, "local_name"},
		{"remote_name 含下划线", func(pj, _ map[string]any, _ *hostConf) { pj["remote_name"] = "ubuntu_2" }, "remote_name"},
		{"remote_claude 含 ..", func(_, p0 map[string]any, _ *hostConf) { p0["remote_claude"] = "../x" }, "remote_claude"},
		{"remote_claude 含非法字符", func(_, p0 map[string]any, _ *hostConf) { p0["remote_claude"] = "a b" }, "remote_claude"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := projConfT(t, tc.mut)
			_, err := prepareProjsync(c)
			if err == nil {
				t.Fatalf("应报错，得到 nil")
			}
			if !strings.HasPrefix(err.Error(), "hosts.json: projsync") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("应报含 %q 的错误，得到 %v", tc.want, err)
			}
		})
	}
}

func TestProjsyncDefaults(t *testing.T) {
	var repo string
	c := projConfT(t, func(pj, p0 map[string]any, h *hostConf) {
		repo = p0["local"].(string)
		p0["name"] = "ai-browser"
		p0["remote"] = "/home/chenwy/Documents/codes/ai-browser"
	})
	p, err := prepareProjsync(c)
	if err != nil {
		t.Fatal(err)
	}
	if p.localName != "mac" {
		t.Errorf("local_name 默认应为 mac，得到 %q", p.localName)
	}
	if p.remoteName != "ubuntu" { // 默认主机名
		t.Errorf("remote_name 默认应为主机名，得到 %q", p.remoteName)
	}
	if p.identity != "/id" || p.knownHosts != "/kh" { // 主机第一个私钥（expand 后）
		t.Errorf("identity=%q knownHosts=%q", p.identity, p.knownHosts)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	canon, err := canonPath(repo)
	if err != nil {
		t.Fatal(err)
	}
	t0 := p.targets[0]
	if want := filepath.Join(home, ".claude", "projects", projsync.ClaudeDirName(canon)); t0.localClaude != want {
		t.Errorf("local_claude 默认 = %q，期望 %q", t0.localClaude, want)
	}
	if want := ".claude/projects/" + projsync.ClaudeDirName("/home/chenwy/Documents/codes/ai-browser"); t0.remoteClaude != want {
		t.Errorf("remote_claude 默认 = %q，期望 %q", t0.remoteClaude, want)
	}
	if t0.local != canon {
		t.Errorf("local 应是解开的绝对路径 %q，得到 %q", canon, t0.local)
	}
	// 显式覆盖：~/ 展开、远端相对路径原样
	c = projConfT(t, func(_, p0 map[string]any, _ *hostConf) {
		p0["local_claude"] = "~/my-claude"
		p0["remote_claude"] = "conv/ai"
	})
	p, err = prepareProjsync(c)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.targets[0].localClaude; got != filepath.Join(home, "my-claude") {
		t.Errorf("local_claude 覆盖后 = %q，期望 ~/ 展开", got)
	}
	if got := p.targets[0].remoteClaude; got != "conv/ai" {
		t.Errorf("remote_claude 覆盖后 = %q", got)
	}
	// 主机名不能当命名空间时（大写），默认值校验要报错
	c = projConfT(t, func(pj, _ map[string]any, h *hostConf) {
		h.Name = "Ubuntu"
		pj["host"] = "Ubuntu"
	})
	if _, err := prepareProjsync(c); err == nil || !strings.Contains(err.Error(), "remote_name") {
		t.Errorf("主机名大写做默认 remote_name 应报错，得到 %v", err)
	}
}

// 坏配置让 wgtund 启动失败（校验发生在 prepareSupport 之后、起隧道之前）。
func TestProjsyncBadConfigStopsRun(t *testing.T) {
	f := newFixture(t, func(cfg map[string]any) {
		cfg["projsync"] = map[string]any{"host": "nope",
			"projects": []any{map[string]any{"name": "a", "local": "/x", "remote": "/r/a"}}}
	})
	if err := runErr(t, f.dir); err == nil || !strings.Contains(err.Error(), "hosts.json: projsync") ||
		!strings.Contains(err.Error(), "不在 hosts 里") {
		t.Fatalf("应因 projsync 配置错误退出，得到 %v", err)
	}
}

// ── 本机接口（假执行） ───────────────────────────────────

// mkAPI 建两个项目（alpha/beta）的 svc，run 换成可控的假执行；返回 svc、进入通道和放行闸门。
func mkAPI(t *testing.T) (*projsyncSvc, chan string, chan struct{}) {
	t.Helper()
	c := projConfT(t, func(pj, p0 map[string]any, _ *hostConf) {
		pj["projects"] = []any{
			p0,
			map[string]any{"name": "beta", "local": p0["local"], "remote": "/home/chenwy/beta"},
		}
	})
	p, err := prepareProjsync(c)
	if err != nil {
		t.Fatal(err)
	}
	svc := newProjsyncSvc(p, nil, "127.0.0.1:9")
	entered := make(chan string, 8)
	gate := make(chan struct{})
	svc.run = func(ctx context.Context, tt projsyncTarget) *projsyncResult {
		entered <- tt.name
		<-gate
		return &projsyncResult{Time: time.Now(), Git: &projsync.GitResult{
			Branches:   []projsync.BranchState{{Branch: "main", Ahead: 1, Remote: true}},
			RemoteOnly: []string{},
		}, Files: &projsync.FileResult{Uploaded: []string{"x.jsonl"}, Downloaded: []string{}, Same: 1, Conflicts: []projsync.Conflict{}, Errors: []string{}}}
	}
	return svc, entered, gate
}

func TestProjsyncLocalAPI(t *testing.T) {
	svc, entered, gate := mkAPI(t)
	srv := httptest.NewServer(svc.localHandler(context.Background(), []string{"127.0.0.1:8765"}))
	defer srv.Close()
	do := func(method, path, origin string) (*http.Response, []byte) {
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

	// GET 结构：按配置顺序、running=false、last=null
	resp, b := do(http.MethodGet, "/projsync", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET 应 200: %d %s", resp.StatusCode, b)
	}
	items := getProjects(t, b)
	if len(items) != 2 || items[0].Name != "alpha" || items[1].Name != "beta" {
		t.Fatalf("项目列表顺序不对: %s", b)
	}
	for _, it := range items {
		if it.Running || it.Last != nil {
			t.Fatalf("未跑过应 running=false last=null: %+v", it)
		}
	}
	if !strings.Contains(string(b), `"last":null`) {
		t.Fatalf("原始 JSON 应含 \"last\":null: %s", b)
	}

	// 方法不对
	if resp, _ := do(http.MethodGet, "/projsync/run?name=alpha", testOrigin); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /projsync/run 应 405: %d", resp.StatusCode)
	}
	if resp, _ := do(http.MethodPost, "/projsync", testOrigin); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /projsync 应 405: %d", resp.StatusCode)
	}

	// Origin：没有 / 不在白名单都 403
	if resp, _ := do(http.MethodPost, "/projsync/run?name=alpha", ""); resp.StatusCode != http.StatusForbidden {
		t.Errorf("无 Origin 应 403: %d", resp.StatusCode)
	}
	if resp, _ := do(http.MethodPost, "/projsync/run?name=alpha", "http://evil.example"); resp.StatusCode != http.StatusForbidden {
		t.Errorf("白名单外 Origin 应 403: %d", resp.StatusCode)
	}

	// 未知项目 404
	if resp, b := do(http.MethodPost, "/projsync/run?name=zzz", testOrigin); resp.StatusCode != http.StatusNotFound ||
		!strings.Contains(string(b), "未知项目") {
		t.Errorf("未知项目应 404: %d %s", resp.StatusCode, b)
	}

	// 合法发起：202；同步中 GET running=true；期间再来一次 409
	resp, b = do(http.MethodPost, "/projsync/run?name=alpha", testOrigin)
	if resp.StatusCode != http.StatusAccepted || !strings.Contains(string(b), `"started":true`) {
		t.Fatalf("应 202 started:true，得到 %d %s", resp.StatusCode, b)
	}
	if n := <-entered; n != "alpha" {
		t.Fatalf("假执行收到的项目名 = %q", n)
	}
	_, b = do(http.MethodGet, "/projsync", "")
	items = getProjects(t, b)
	if !items[0].Running || items[1].Running || items[0].Last != nil {
		t.Fatalf("alpha 应在同步中且无 last: %+v", items)
	}
	resp, b = do(http.MethodPost, "/projsync/run?name=alpha", testOrigin)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(b), "正在同步") {
		t.Fatalf("第二次应 409 正在同步: %d %s", resp.StatusCode, b)
	}
	// 另一个项目不受 alpha 阻塞，可以并行发起
	if resp, _ := do(http.MethodPost, "/projsync/run?name=beta", testOrigin); resp.StatusCode != http.StatusAccepted {
		t.Errorf("beta 并行发起应 202: %d", resp.StatusCode)
	}
	if n := <-entered; n != "beta" {
		t.Fatalf("假执行第二个项目 = %q", n)
	}

	close(gate) // 放行两个后台执行
	pollUntil(t, 5*time.Second, "两次同步结束", func() bool {
		_, b := do(http.MethodGet, "/projsync", "")
		items := getProjects(t, b)
		return !items[0].Running && !items[1].Running
	})
	_, b = do(http.MethodGet, "/projsync", "")
	items = getProjects(t, b)
	for _, it := range items {
		if it.Last == nil || it.Last.Git == nil || len(it.Last.Git.Branches) != 1 ||
			it.Last.Git.Branches[0].Branch != "main" || it.Last.Files == nil {
			t.Fatalf("%s 的 last 不对: %+v", it.Name, it.Last)
		}
		if it.Last.GitError != "" || it.Last.FilesError != "" {
			t.Fatalf("不应有错误: %+v", it.Last)
		}
	}
}

// 同一项目的同步串行：Run 进行中再来 Run 直接 errProjsyncBusy；整个 Run 套 30 分钟超时。
func TestProjsyncBusyAndTimeout(t *testing.T) {
	svc, entered, gate := mkAPI(t)
	deadlines := make(chan time.Time, 2)
	base := svc.run
	svc.run = func(ctx context.Context, tt projsyncTarget) *projsyncResult {
		if dl, ok := ctx.Deadline(); ok {
			deadlines <- dl
		}
		return base(ctx, tt)
	}
	done := make(chan *projsyncResult, 1)
	go func() {
		res, err := svc.Run(context.Background(), "alpha")
		if err != nil {
			t.Errorf("Run 出错: %v", err)
		}
		done <- res
	}()
	<-entered
	if _, err := svc.Run(context.Background(), "alpha"); !errors.Is(err, errProjsyncBusy) {
		t.Fatalf("第二次 Run 应 errProjsyncBusy，得到 %v", err)
	}
	if _, err := svc.Run(context.Background(), "nope"); !errors.Is(err, errProjsyncUnknown) {
		t.Fatalf("未知项目应 errProjsyncUnknown，得到 %v", err)
	}
	dl := <-deadlines
	if left := time.Until(dl); left < 29*time.Minute || left > projsyncRunTimeout+5*time.Second {
		t.Errorf("Run 超时应在 30 分钟左右，得到剩 %v", left)
	}
	close(gate)
	res := <-done
	if res == nil || res.Git == nil {
		t.Fatalf("第一次 Run 应返回结果: %+v", res)
	}
	st := svc.status()
	items := st["projects"].([]projsyncItem)
	if items[0].Running || items[0].Last == nil {
		t.Errorf("结束后应 running=false 且有 last: %+v", items[0])
	}
	// 结束之后可以再跑（不再忙）：闸门已关，直接放行
	res2, err := svc.Run(context.Background(), "alpha")
	if err != nil || res2 == nil {
		t.Errorf("空闲后再 Run 应成功: %v %+v", err, res2)
	}
}

// 后台执行的 ctx 跟随 wgtund 的 Run ctx：父 ctx 取消能传到执行函数。
func TestProjsyncStartFollowsCtx(t *testing.T) {
	svc, entered, _ := mkAPI(t)
	ctx, cancel := context.WithCancel(context.Background())
	svc.run = func(ctx context.Context, tt projsyncTarget) *projsyncResult {
		entered <- tt.name
		<-ctx.Done()
		return &projsyncResult{Time: time.Now(), GitError: "取消:" + ctx.Err().Error()}
	}
	if err := svc.Start(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	<-entered
	if err := svc.Start(ctx, "alpha"); !errors.Is(err, errProjsyncBusy) {
		t.Errorf("进行中 Start 应忙: %v", err)
	}
	cancel()
	pollUntil(t, 5*time.Second, "后台执行观察到取消", func() bool {
		svc.mu.Lock()
		last := svc.last["alpha"]
		svc.mu.Unlock()
		return last != nil && strings.HasPrefix(last.GitError, "取消:")
	})
}

// ── 线路与 git 地址 ──────────────────────────────────────

func TestProjsyncGitTarget(t *testing.T) {
	pool := newSSHPool(nil)
	defer pool.Close()
	d := &net.Dialer{Timeout: time.Second}
	h := hostConf{Name: "ubuntu", User: "chenwy", Routes: []routeConf{
		{Label: "隧道", Addr: "10.99.0.1:22"},
		{Label: "直连", Addr: "192.168.65.2:2222"},
	}}
	pool.add(h, []sshterm.Dialer{d, d})
	p := &projsyncPrep{
		host: "ubuntu", user: "chenwy", identity: "/home/u/.ssh/id_ed", knownHosts: "/home/u/.ssh/known_hosts",
		localName: "mac", remoteName: "ubuntu",
		targets: []projsyncTarget{{name: "alpha", remote: "/home/chenwy/alpha"}},
	}
	svc := newProjsyncSvc(p, pool, "127.0.0.1:34567")
	tt, _ := svc.target("alpha")
	url, env, err := svc.gitTarget(tt)
	if err != nil {
		t.Fatal(err)
	}
	if want := "ssh://chenwy@10.99.0.1:22/home/chenwy/alpha"; url != want { // 端口 22 也照写
		t.Errorf("git 地址 = %q，期望 %q", url, want)
	}
	if len(env) != 1 || !strings.HasPrefix(env[0], "GIT_SSH_COMMAND=ssh") ||
		!strings.Contains(env[0], "nc 127.0.0.1 34567") ||
		!strings.Contains(env[0], "StrictHostKeyChecking=yes") ||
		!strings.Contains(env[0], "-i '/home/u/.ssh/id_ed'") {
		t.Errorf("git 环境不对: %v", env)
	}
	if err := pool.switchRoute("ubuntu", 1); err != nil {
		t.Fatal(err)
	}
	url, _, err = svc.gitTarget(tt)
	if err != nil {
		t.Fatal(err)
	}
	if want := "ssh://chenwy@192.168.65.2:2222/home/chenwy/alpha"; url != want { // 跟随当前线路
		t.Errorf("切换线路后 git 地址 = %q，期望 %q", url, want)
	}
}

// ── 完整 wgtund 流程（假执行） ──────────────────────────

func TestProjsyncFullRun(t *testing.T) {
	t.Cleanup(func() { projsyncRunForTest = nil })

	// 没配置：GET 和 POST 都 404
	f0 := newFixture(t, nil)
	st0, _, _ := run(t, f0.dir)
	if resp, b := get(t, st0, "/projsync", true); resp.StatusCode != http.StatusNotFound ||
		!strings.Contains(string(b), "没有配置项目同步") {
		t.Fatalf("没配置应 404: %d %s", resp.StatusCode, b)
	}
	if resp, _ := supportPost(t, st0, "/projsync/run?name=a", testOrigin); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("没配置 POST 应 404: %d", resp.StatusCode)
	}

	repo := gitRepoDir(t)
	entered := make(chan string, 4)
	gate := make(chan struct{})
	projsyncRunForTest = func(ctx context.Context, _ *projsyncSvc, tt projsyncTarget) *projsyncResult {
		entered <- tt.name
		<-gate
		return &projsyncResult{Time: time.Now(), GitError: "fake 执行", Files: &projsync.FileResult{
			Uploaded: []string{}, Downloaded: []string{}, Conflicts: []projsync.Conflict{}, Errors: []string{}}}
	}

	f := newFixture(t, func(cfg map[string]any) {
		cfg["projsync"] = map[string]any{"host": "ubuntu",
			"projects": []any{
				map[string]any{"name": "alpha", "local": repo, "remote": "/home/chenwy/alpha"},
				map[string]any{"name": "beta", "local": repo, "remote": "/home/chenwy/beta"},
			}}
	})
	st, _, _ := run(t, f.dir)

	if resp, _ := get(t, st, "/projsync", false); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("不带主令牌应 401: %d", resp.StatusCode)
	}
	resp, b := get(t, st, "/projsync", true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET 应 200: %d %s", resp.StatusCode, b)
	}
	items := getProjects(t, b)
	if len(items) != 2 || items[0].Name != "alpha" || items[1].Name != "beta" || items[0].Last != nil {
		t.Fatalf("初始结构不对: %s", b)
	}
	if resp, _ := supportPost(t, st, "/projsync/run?name=alpha", ""); resp.StatusCode != http.StatusForbidden {
		t.Errorf("POST 无 Origin 应 403: %d", resp.StatusCode)
	}
	resp, b = supportPost(t, st, "/projsync/run?name=alpha", testOrigin)
	if resp.StatusCode != http.StatusAccepted || !strings.Contains(string(b), `"started":true`) {
		t.Fatalf("应 202: %d %s", resp.StatusCode, b)
	}
	if n := <-entered; n != "alpha" {
		t.Fatalf("执行项目 = %q", n)
	}
	_, b = get(t, st, "/projsync", true)
	items = getProjects(t, b)
	if !items[0].Running {
		t.Fatalf("发起后 GET 应显示 running: %s", b)
	}
	if resp, _ := supportPost(t, st, "/projsync/run?name=alpha", testOrigin); resp.StatusCode != http.StatusConflict {
		t.Errorf("同步中再来应 409: %d", resp.StatusCode)
	}
	close(gate)
	pollUntil(t, 5*time.Second, "同步结束并记下 last", func() bool {
		_, b := get(t, st, "/projsync", true)
		items := getProjects(t, b)
		return !items[0].Running && items[0].Last != nil && items[0].Last.GitError == "fake 执行"
	})
}

// ── 协助接线：远端发起 → 用户允许 → proj.Run ────────────

func TestProjsyncSupportWired(t *testing.T) {
	t.Cleanup(func() { projsyncRunForTest = nil })
	f, lg, _ := supportFixture(t, nil)
	repo := gitRepoDir(t)
	patchHosts(t, f.dir, func(cfg map[string]any) {
		cfg["projsync"] = map[string]any{"host": "ubuntu",
			"projects": []any{map[string]any{"name": "ai-browser", "local": repo, "remote": "/home/chenwy/ai-browser"}}}
	})
	called := make(chan string, 4)
	projsyncRunForTest = func(ctx context.Context, _ *projsyncSvc, tt projsyncTarget) *projsyncResult {
		called <- tt.name
		return &projsyncResult{Time: time.Now(), GitError: "fake 执行"}
	}
	st, _, _ := run(t, f.dir)

	// /support 状态带项目名（ProjSyncNames 已接线）
	_, b := get(t, st, "/support", true)
	if !strings.Contains(string(b), `"projects":["ai-browser"]`) {
		t.Fatalf("/support 应含 projects: %s", b)
	}

	resp, b := supportPost(t, st, "/support/start?minutes=5", testOrigin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("start 应 200: %d %s", resp.StatusCode, b)
	}
	cmd := lg.wait(t, "endpoint.json.tmp")
	rest := strings.TrimPrefix(cmd, "umask 077 && mkdir -p ~/.xxyi-support && printf '%s' ")
	q := rest[:strings.Index(rest, " > ")]
	js, ok := unQuote(q)
	if !ok {
		t.Fatalf("入口 JSON 没被单引号包住: %s", q)
	}
	var ep struct {
		Addr  string `json:"addr"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(js), &ep); err != nil || ep.Addr == "" || ep.Token == "" {
		t.Fatalf("入口 JSON 不对 %q: %v", js, err)
	}

	// 远端发起同步：进待批队列，等用户允许
	type remoteRes struct {
		code int
		body []byte
	}
	rr := make(chan remoteRes, 1)
	go func() {
		req, err := http.NewRequest(http.MethodPost, "http://"+ep.Addr+"/projsync?name=ai-browser", nil)
		if err != nil {
			rr <- remoteRes{0, nil}
			return
		}
		req.Header.Set("Authorization", "Bearer "+ep.Token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			rr <- remoteRes{0, []byte(err.Error())}
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		rr <- remoteRes{resp.StatusCode, body}
	}()
	var id string
	pollUntil(t, 5*time.Second, "ProjectSync 待批出现", func() bool {
		_, b := get(t, st, "/support", true)
		var ls struct {
			Pending []struct {
				ID   string `json:"id"`
				Tool string `json:"tool"`
			} `json:"pending"`
		}
		if err := json.Unmarshal(b, &ls); err != nil {
			return false
		}
		for _, p := range ls.Pending {
			if p.Tool == "ProjectSync" {
				id = p.ID
				return true
			}
		}
		return false
	})
	if resp, b := supportPost(t, st, "/support/decide?id="+id+"&allow=1", testOrigin); resp.StatusCode != http.StatusOK {
		t.Fatalf("批准应 200: %d %s", resp.StatusCode, b)
	}
	select {
	case n := <-called:
		if n != "ai-browser" {
			t.Fatalf("ProjSync 调用的项目 = %q", n)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("批准后没调到 ProjSync")
	}
	select {
	case r := <-rr:
		if r.code != http.StatusOK || !strings.Contains(string(r.body), "fake 执行") {
			t.Fatalf("远端应拿到同步结果 200: %d %s", r.code, r.body)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("等远端 projsync 响应超时")
	}
	pollUntil(t, 5*time.Second, "GET /projsync 出现 last", func() bool {
		_, b := get(t, st, "/projsync", true)
		items := getProjects(t, b)
		return len(items) == 1 && !items[0].Running && items[0].Last != nil
	})
}

// ── 端到端（对话目录）：真 pool + 真 SFTP（sshtest） ────
// git 半边用本地路径的裸仓库代替 ssh 远端（真 push/fetch）。

func TestProjsyncE2EFiles(t *testing.T) {
	remoteRoot := t.TempDir()
	remoteClaude := ".claude/projects/projA" // 相对 sshtest 的登录目录（= remoteRoot）
	os.MkdirAll(filepath.Join(remoteRoot, filepath.FromSlash(remoteClaude), "sub"), 0o755)
	remoteFile := func(rel string, b []byte) {
		if err := os.WriteFile(filepath.Join(remoteRoot, filepath.FromSlash(remoteClaude), filepath.FromSlash(rel)), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	remoteFile("remote-only.txt", []byte("remote only\n"))
	shared := []byte("same content\n")
	remoteFile("shared.jsonl", shared)
	growLong := []byte("session line one\nsession line two\n")
	remoteFile("grow.jsonl", growLong)
	growMT := time.Now().Add(-3 * time.Hour)
	os.Chtimes(filepath.Join(remoteRoot, filepath.FromSlash(remoteClaude), "grow.jsonl"), growMT, growMT)

	localClaude := t.TempDir()
	os.MkdirAll(filepath.Join(localClaude, "sub"), 0o755)
	localFile := func(rel string, b []byte) {
		if err := os.WriteFile(filepath.Join(localClaude, filepath.FromSlash(rel)), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	localFile("sub/local-only.jsonl", []byte("local only\n"))
	localFile("shared.jsonl", shared)
	localFile("grow.jsonl", []byte("session line one\n"))

	// 真 SSH：sshtest 起在本机回环，SFTP 根在 remoteRoot，exec 用 sh -c 在 remoteRoot 下执行
	clientKey, clientPriv := sshtest.NewKey()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.Addr().String()
	var shMu sync.Mutex
	var shCmds []string
	execFn := func(cmd string) (string, string, int) {
		shMu.Lock()
		shCmds = append(shCmds, cmd)
		shMu.Unlock()
		c := exec.Command("sh", "-c", cmd)
		c.Dir = remoteRoot
		var o, e bytes.Buffer
		c.Stdout, c.Stderr = &o, &e
		if err := c.Run(); err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				return o.String(), e.String(), ee.ExitCode()
			}
			return "", err.Error(), 1
		}
		return o.String(), e.String(), 0
	}
	srv := sshtest.ServeWith(ln, clientKey.PublicKey(), sshtest.Options{Exec: execFn, SFTPRoot: remoteRoot})

	dir := t.TempDir()
	pemBlock, err := ssh.MarshalPrivateKey(clientPriv, "")
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "id_test")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(pemBlock), 0o600); err != nil {
		t.Fatal(err)
	}
	khPath := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(khPath, []byte(knownhosts.Line([]string{knownhosts.Normalize(addr)}, srv.HostKey.PublicKey())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	khCb, err := knownhosts.New(khPath)
	if err != nil {
		t.Fatal(err)
	}

	pool := newSSHPool(khCb)
	defer pool.Close()
	h := hostConf{Name: "ubuntu", Addr: addr, User: "chenwy", IdentityFiles: []string{keyPath}}
	pool.add(h, []sshterm.Dialer{&net.Dialer{Timeout: 5 * time.Second}})

	prep := &projsyncPrep{
		host: "ubuntu", user: "chenwy", identity: keyPath, knownHosts: khPath,
		localName: "mac", remoteName: "ubuntu",
		targets: []projsyncTarget{{
			name: "projA", local: gitRepoDir(t), remote: "/home/chenwy/projA",
			localClaude: localClaude, remoteClaude: remoteClaude,
		}},
	}
	svc := newProjsyncSvc(prep, pool, "127.0.0.1:9")
	// git 半边：远端换成本地路径的裸仓库（真 git push/fetch，不走 ssh）；地址/环境拼装由 TestProjsyncGitTarget 覆盖
	bare := filepath.Join(t.TempDir(), "remote.git")
	if out, err := exec.Command("git", "init", "-q", "--bare", bare).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v %s", err, out)
	}
	gitIn := func(dir string, args ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	repo := prep.targets[0].local
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(repo, "add", "a.txt")
	gitIn(repo, "commit", "-q", "-m", "init")
	svc.gitOverride = func(projsyncTarget) (string, []string, error) { return bare, nil, nil }

	res, err := svc.Run(context.Background(), "projA")
	if err != nil {
		t.Fatal(err)
	}
	if res.FilesError != "" {
		t.Fatalf("对话同步失败: %s", res.FilesError)
	}
	f := res.Files
	if len(f.Errors) != 0 || len(f.Conflicts) != 0 {
		t.Fatalf("不该有冲突/错误: %+v", f)
	}
	if len(f.Uploaded) != 1 || f.Uploaded[0] != "sub/local-only.jsonl" {
		t.Errorf("Uploaded = %v", f.Uploaded)
	}
	if len(f.Downloaded) != 2 || f.Downloaded[0] != "grow.jsonl" || f.Downloaded[1] != "remote-only.txt" {
		t.Errorf("Downloaded = %v", f.Downloaded)
	}
	if f.Same != 1 {
		t.Errorf("Same = %d，应为 1（shared.jsonl）", f.Same)
	}
	if res.GitError != "" || res.Git == nil || len(res.Git.Branches) != 1 {
		t.Errorf("git 半边应成功: %q %+v", res.GitError, res.Git)
	}
	if out, err := exec.Command("git", "-C", bare, "for-each-ref", "refs/remotes/mac/").CombinedOutput(); err != nil || !strings.Contains(string(out), "refs/remotes/mac/") {
		t.Errorf("对方应有 refs/remotes/mac/*: %q %v", out, err)
	}
	shMu.Lock()
	cmds := strings.Join(shCmds, "\n")
	shMu.Unlock()
	if !strings.Contains(cmds, "sha256sum") || !strings.Contains(cmds, "head -c") {
		t.Errorf("远端没执行 hash 命令: %s", cmds)
	}
	// 落盘核对
	got, err := os.ReadFile(filepath.Join(localClaude, "remote-only.txt"))
	if err != nil || string(got) != "remote only\n" {
		t.Errorf("本机没拿到 remote-only.txt: %q %v", got, err)
	}
	got, err = os.ReadFile(filepath.Join(localClaude, "grow.jsonl"))
	if err != nil || !bytes.Equal(got, growLong) {
		t.Errorf("前缀规则没生效，本机 grow.jsonl = %q %v", got, err)
	}
	if fi, err := os.Stat(filepath.Join(localClaude, "grow.jsonl")); err != nil || fi.ModTime().Unix() != growMT.Unix() {
		t.Errorf("下载应保留源文件修改时间: %v %v", fi, err)
	}
	got, err = os.ReadFile(filepath.Join(remoteRoot, filepath.FromSlash(remoteClaude), "sub", "local-only.jsonl"))
	if err != nil || string(got) != "local only\n" {
		t.Errorf("远端没收到 sub/local-only.jsonl: %q %v", got, err)
	}

	// 再跑一次：全部一致，不再有上传/下载
	res2, err := svc.Run(context.Background(), "projA")
	if err != nil {
		t.Fatal(err)
	}
	if res2.FilesError != "" || len(res2.Files.Uploaded) != 0 || len(res2.Files.Downloaded) != 0 {
		t.Fatalf("第二次应全部跳过: %+v %s", res2.Files, res2.FilesError)
	}
}
