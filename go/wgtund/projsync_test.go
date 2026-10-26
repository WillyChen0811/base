package wgtund

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
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

// 坏配置让 wgtund 启动失败（校验发生在起隧道之前）。
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

// ── 完整 wgtund 流程：v1 旧路径已移除 ───────────────────

func TestProjsyncV1PathsGone(t *testing.T) {
	repo := gitRepoDir(t)
	for _, withConf := range []bool{false, true} {
		var mut func(cfg map[string]any)
		if withConf {
			mut = func(cfg map[string]any) {
				cfg["projsync"] = map[string]any{"host": "ubuntu",
					"projects": []any{map[string]any{"name": "alpha", "local": repo, "remote": "/home/chenwy/alpha"}}}
			}
		}
		f := newFixture(t, mut)
		st, _, _ := run(t, f.dir)
		if resp, b := get(t, st, "/projsync", true); resp.StatusCode != http.StatusNotFound {
			t.Errorf("withConf=%v: GET /projsync 应 404: %d %s", withConf, resp.StatusCode, b)
		}
		if resp, _ := postOrigin(t, st, "/projsync/run?name=alpha", testOrigin); resp.StatusCode != http.StatusNotFound {
			t.Errorf("withConf=%v: POST /projsync/run 应 404: %d", withConf, resp.StatusCode)
		}
		// v2 仍在
		if resp, b := get(t, st, "/projsync/v2", true); withConf && resp.StatusCode != http.StatusOK {
			t.Errorf("有配置时 GET /projsync/v2 应 200: %d %s", resp.StatusCode, b)
		}
	}
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
	tg := prep.targets[0]
	f, err := svc.syncClaude(context.Background(), tg)
	if err != nil {
		t.Fatalf("对话同步失败: %v", err)
	}
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
	f2, err := svc.syncClaude(context.Background(), tg)
	if err != nil {
		t.Fatal(err)
	}
	if len(f2.Uploaded) != 0 || len(f2.Downloaded) != 0 {
		t.Fatalf("第二次应全部跳过: %+v", f2)
	}
}
