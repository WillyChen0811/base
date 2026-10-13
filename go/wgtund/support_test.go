package wgtund

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
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
)

// 假 claude：读一行输出两行事件，循环。
const fakeClaude = `#!/bin/sh
[ -n "$CLAUDE_ARGS_OUT" ] && printf '%s\n' "$@" > "$CLAUDE_ARGS_OUT"
while read -r line; do
  echo '{"type":"system","subtype":"init","session_id":"s1"}'
  echo '{"type":"result","subtype":"success","result":"ok"}'
done
`

// gitInit 在 dir 建一个空 git 仓库。
func gitInit(t *testing.T, dir string) {
	t.Helper()
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
}

// patchSupport 在夹具建好后改 hosts.json 的 support 段。
func patchSupport(t *testing.T, dir string, fn func(sup map[string]any)) {
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
	fn(cfg["support"].(map[string]any))
	b, _ = json.Marshal(cfg)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func goodSettings() map[string]any {
	return map[string]any{
		"model":   "sonnet",
		"sandbox": map[string]any{"enabled": true, "allowUnsandboxedCommands": false, "failIfUnavailable": true, "autoAllowBashIfSandboxed": true},
		"hooks": map[string]any{
			"PreToolUse":        []any{map[string]any{"matcher": "Bash"}},
			"PermissionRequest": []any{map[string]any{"matcher": "old"}},
		},
	}
}

type execLog struct {
	mu   sync.Mutex
	cmds []string
}

func (l *execLog) exec(cmd string) (string, string, int) {
	l.mu.Lock()
	l.cmds = append(l.cmds, cmd)
	l.mu.Unlock()
	return "", "", 0
}

func (l *execLog) find(sub string) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, c := range l.cmds {
		if strings.Contains(c, sub) {
			return c, true
		}
	}
	return "", false
}

func (l *execLog) wait(t *testing.T, sub string) string {
	t.Helper()
	for i := 0; i < 200; i++ {
		if c, ok := l.find(sub); ok {
			return c
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("远端没收到含 %q 的命令", sub)
	return ""
}

// supportFixture 搭好带 support 段的配置；tweak 可改 support 段和 claude 设置。
func supportFixture(t *testing.T, tweak func(sup map[string]any, settings map[string]any)) (*fixture, *execLog, string) {
	t.Helper()
	lg := &execLog{}
	tmp := t.TempDir()
	workdir := filepath.Join(tmp, "work")
	os.MkdirAll(workdir, 0o755)
	gitInit(t, workdir) // repos 的 dir 必须是 git 仓库顶层
	bin := filepath.Join(tmp, "fakeclaude")
	os.WriteFile(bin, []byte(fakeClaude), 0o755)
	settings := goodSettings()
	sup := map[string]any{
		"host": "ubuntu", "claude_bin": bin, "workdir": workdir,
		"claude_args": []string{"--model", "sonnet"},
		"repos":       map[string]any{"xxyi": map[string]any{"dir": workdir, "remote": "ssh://x@h/r"}},
	}
	if tweak != nil {
		tweak(sup, settings)
	}
	sp := filepath.Join(t.TempDir(), "settings.json") // 设置文件目录不能包含工作目录
	if _, ok := sup["claude_settings"]; !ok {
		sup["claude_settings"] = sp
	}
	b, _ := json.Marshal(settings)
	os.WriteFile(sp, b, 0o600)
	f := newFixtureOpts(t, func(cfg map[string]any) { cfg["support"] = sup }, sshtest.Options{Exec: lg.exec, Forward: true})
	return f, lg, tmp
}

// runErr 直接调用 Run，返回它的错误（配置不合规时应很快返回）。
func runErr(t *testing.T, dir string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pr, pw := io.Pipe()
	defer pw.Close()
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- Run(ctx, dir, pr, &out) }()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("Run 没有因配置错误退出")
		return nil
	}
}

func setDomains(st map[string]any, ds ...string) {
	l := []any{}
	for _, d := range ds {
		l = append(l, d)
	}
	st["sandbox"].(map[string]any)["network"] = map[string]any{"allowedDomains": l}
}

func TestSupportConfigValidation(t *testing.T) {
	cases := []struct {
		name  string
		tweak func(sup, settings map[string]any)
	}{
		{"缺 workdir", func(s, _ map[string]any) { delete(s, "workdir") }},
		{"workdir 不存在", func(s, _ map[string]any) { s["workdir"] = "/nonexistent-xxyi-dir" }},
		{"host 不存在", func(s, _ map[string]any) { s["host"] = "nope" }},
		{"repo 缺 remote", func(s, _ map[string]any) { s["repos"] = map[string]any{"a": map[string]any{"dir": "/x"}} }},
		{"禁用参数 skip", func(s, _ map[string]any) { s["claude_args"] = []string{"--dangerously-skip-permissions"} }},
		{"禁用参数 permission-mode=", func(s, _ map[string]any) { s["claude_args"] = []string{"--permission-mode=plan"} }},
		{"禁用参数 allowedTools", func(s, _ map[string]any) { s["claude_args"] = []string{"--allowedTools", "Bash"} }},
		{"禁用参数 tools=", func(s, _ map[string]any) { s["claude_args"] = []string{"--tools=Bash"} }},
		{"禁用参数 add-dir", func(s, _ map[string]any) { s["claude_args"] = []string{"--add-dir", "/"} }},
		{"禁用参数 mcp-config", func(s, _ map[string]any) { s["claude_args"] = []string{"--mcp-config", "x"} }},
		{"claude_tools 非法", func(s, _ map[string]any) { s["claude_tools"] = "Bash;rm" }},
		{"禁用参数 settings", func(s, _ map[string]any) { s["claude_args"] = []string{"--settings", "x"} }},
		{"沙盒没开", func(_, st map[string]any) { st["sandbox"].(map[string]any)["enabled"] = false }},
		{"沙盒缺失", func(_, st map[string]any) { delete(st, "sandbox") }},
		{"allowUnsandboxedCommands 不是 false", func(_, st map[string]any) {
			st["sandbox"].(map[string]any)["allowUnsandboxedCommands"] = true
		}},
		{"failIfUnavailable 不是 true", func(_, st map[string]any) {
			st["sandbox"].(map[string]any)["failIfUnavailable"] = false
		}},
		{"defaultMode=bypassPermissions", func(_, st map[string]any) {
			st["permissions"] = map[string]any{"defaultMode": "bypassPermissions"}
		}},
		{"claude_args 不认识的参数", func(s, _ map[string]any) { s["claude_args"] = []string{"--foo", "bar"} }},
		{"claude_args --plugin-url", func(s, _ map[string]any) { s["claude_args"] = []string{"--plugin-url", "http://x"} }},
		{"claude_args --worktree", func(s, _ map[string]any) { s["claude_args"] = []string{"--worktree"} }},
		{"claude_args 位置参数", func(s, _ map[string]any) { s["claude_args"] = []string{"--model", "sonnet", "do-something"} }},
		{"claude_args --model 缺值", func(s, _ map[string]any) { s["claude_args"] = []string{"--model"} }},
		{"claude_args 值以 - 开头", func(s, _ map[string]any) { s["claude_args"] = []string{"--model", "--effort"} }},
		{"claude_args --model= 空值", func(s, _ map[string]any) { s["claude_args"] = []string{"--model="} }},
		{"autoAllowBashIfSandboxed 缺省", func(_, st map[string]any) { delete(st["sandbox"].(map[string]any), "autoAllowBashIfSandboxed") }},
		{"autoAllowBashIfSandboxed 不是 bool", func(_, st map[string]any) {
			st["sandbox"].(map[string]any)["autoAllowBashIfSandboxed"] = "true"
		}},
		{"allowLocalBinding", func(_, st map[string]any) {
			st["sandbox"].(map[string]any)["network"] = map[string]any{"allowLocalBinding": true}
		}},
		{"allowUnixSockets 非空", func(_, st map[string]any) {
			st["sandbox"].(map[string]any)["network"] = map[string]any{"allowUnixSockets": []any{"/var/run/x.sock"}}
		}},
		{"allowAllUnixSockets", func(_, st map[string]any) {
			st["sandbox"].(map[string]any)["network"] = map[string]any{"allowAllUnixSockets": true}
		}},
		{"enableWeakerNetworkIsolation", func(_, st map[string]any) {
			st["sandbox"].(map[string]any)["enableWeakerNetworkIsolation"] = true
		}},
		{"enableWeakerNestedSandbox", func(_, st map[string]any) {
			st["sandbox"].(map[string]any)["enableWeakerNestedSandbox"] = true
		}},
		{"filesystem.disabled", func(_, st map[string]any) {
			st["sandbox"].(map[string]any)["filesystem"] = map[string]any{"disabled": true}
		}},
		{"allowedDomains localhost:端口", func(_, st map[string]any) { setDomains(st, "example.com", "LocalHost:8080") }},
		{"allowedDomains *.localhost", func(_, st map[string]any) { setDomains(st, "*.localhost") }},
		{"allowedDomains 127.0.0.1", func(_, st map[string]any) { setDomains(st, "127.0.0.1:3000") }},
		{"allowedDomains [::1]", func(_, st map[string]any) { setDomains(st, "[::1]:80") }},
		{"allowedDomains ::1", func(_, st map[string]any) { setDomains(st, "::1") }},
		{"allowedDomains 0.0.0.0", func(_, st map[string]any) { setDomains(st, "0.0.0.0") }},
		{"disableAllHooks", func(_, st map[string]any) { st["disableAllHooks"] = true }},
		{"allowManagedHooksOnly", func(_, st map[string]any) { st["allowManagedHooksOnly"] = true }},
		{"repo.dir 不存在", func(s, _ map[string]any) {
			s["repos"] = map[string]any{"a": map[string]any{"dir": "/nonexistent-xxyi-repo", "remote": "r"}}
		}},
		{"repo.dir 不是 git 仓库", func(s, _ map[string]any) {
			s["repos"] = map[string]any{"a": map[string]any{"dir": t.TempDir(), "remote": "r"}}
		}},
		{"repo.dir 是仓库子目录而非顶层", func(s, _ map[string]any) {
			d := t.TempDir()
			gitInit(t, d)
			os.MkdirAll(filepath.Join(d, "sub"), 0o755)
			s["repos"] = map[string]any{"a": map[string]any{"dir": filepath.Join(d, "sub"), "remote": "r"}}
		}},
		{"两个仓库名指向同一目录", func(s, _ map[string]any) {
			d := t.TempDir()
			gitInit(t, d)
			s["repos"] = map[string]any{"a": map[string]any{"dir": d, "remote": "r"}, "b": map[string]any{"dir": d, "remote": "r"}}
		}},
		{"git_tunnel 不在 socks", func(s, _ map[string]any) { s["git_tunnel"] = "other" }},
		{"设置文件不存在", func(s, _ map[string]any) { s["claude_settings"] = "/nonexistent-xxyi.json" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, _, _ := supportFixture(t, c.tweak)
			err := runErr(t, f.dir)
			if err == nil || !strings.HasPrefix(err.Error(), "hosts.json: support") {
				t.Fatalf("应返回 hosts.json: support… 错误，得到 %v", err)
			}
		})
	}
}

func TestSupportMergedSettings(t *testing.T) {
	f, _, _ := supportFixture(t, func(_, st map[string]any) {
		st["permissions"] = map[string]any{"defaultMode": "default"}
		st["allowManagedHooksOnly"] = false
		st["disableAllHooks"] = false
		st["sandbox"].(map[string]any)["filesystem"] = map[string]any{"denyRead": []any{"~/.aws", "~/.ssh"}}
	})
	st, _, _ := run(t, f.dir)
	p := filepath.Join(f.dir, "support-claude.merged.json")
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("合并文件权限 %v，应为 0600", fi.Mode().Perm())
	}
	b, _ := os.ReadFile(p)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["model"] != "sonnet" {
		t.Errorf("原有字段丢失: %v", m)
	}
	if v, ok := jsonPath(m, "sandbox", "enabled"); !ok || v != true {
		t.Errorf("sandbox 丢失: %v", m)
	}
	if m["disableAllHooks"] != false {
		t.Errorf("disableAllHooks 应被强制为 false: %v", m["disableAllHooks"])
	}
	if _, ok := m["allowManagedHooksOnly"]; ok {
		t.Errorf("allowManagedHooksOnly 应被删除: %v", m)
	}
	deny, _ := jsonPath(m, "sandbox", "filesystem", "denyRead")
	var denyList []string
	for _, d := range deny.([]any) {
		denyList = append(denyList, d.(string))
	}
	exe, _ := os.Executable()
	cfgDir, _ := canonPath(f.dir)
	exeDir, _ := canonPath(filepath.Dir(exe))
	for _, w := range []string{"~/.aws", "~/.ssh", cfgDir, exeDir} {
		n := 0
		for _, d := range denyList {
			if d == w {
				n++
			}
		}
		if n != 1 {
			t.Errorf("denyRead 里 %q 应恰好出现一次，实际 %d：%v", w, n, denyList)
		}
	}
	if len(denyList) < 5 || denyList[0] != "~/.aws" || denyList[1] != "~/.ssh" {
		t.Errorf("denyRead 应保留原有条目在前并追加: %v", denyList)
	}
	hooks := m["hooks"].(map[string]any)
	if _, ok := hooks["PreToolUse"]; !ok {
		t.Errorf("其他 hooks 丢失: %v", hooks)
	}
	pr := hooks["PermissionRequest"].([]any)
	if len(pr) != 1 {
		t.Fatalf("PermissionRequest 应被覆盖为一项: %v", pr)
	}
	ent := pr[0].(map[string]any)
	h := ent["hooks"].([]any)[0].(map[string]any)
	cmd, _ := h["command"].(string)
	if ent["matcher"] != "*" || h["type"] != "command" || h["onFailure"] != "block" || h["timeout"] != float64(600) {
		t.Errorf("钩子项不对: %v", ent)
	}
	if !strings.Contains(cmd, " support-hook 'http://"+st.HTTP+"/support/hook?token=") {
		t.Fatalf("钩子命令不对: %s", cmd)
	}
	// 钩子地址里的令牌是对的：协助没开时，真实走一遍子命令应得到拒绝；错令牌 401
	url := cmd[strings.Index(cmd, "'http://")+1 : len(cmd)-1]
	var out bytes.Buffer
	RunHook([]string{url}, strings.NewReader(`{"hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{}}`), &out, func(string) string { return "" })
	if !strings.Contains(out.String(), "协助未开启") {
		t.Errorf("正确令牌应到达服务并得到「协助未开启」: %s", out.String())
	}
	resp, err := http.Post("http://"+st.HTTP+"/support/hook?token=wrong", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("错的钩子令牌应 401，得到 %d", resp.StatusCode)
	}
}

func TestSupportAuthAndDisabled(t *testing.T) {
	f, _, _ := supportFixture(t, nil)
	st, _, _ := run(t, f.dir)
	if resp, _ := get(t, st, "/support", false); resp.StatusCode != 401 {
		t.Errorf("没带主令牌 /support 应 401，得到 %d", resp.StatusCode)
	}
	if resp, _ := get(t, st, "/support/stop", false); resp.StatusCode != 401 {
		t.Errorf("没带主令牌 /support/stop 应 401，得到 %d", resp.StatusCode)
	}
	if resp, b := get(t, st, "/support", true); resp.StatusCode != 200 || !strings.Contains(string(b), `"active":false`) {
		t.Errorf("/support 应 200 且未开启: %d %s", resp.StatusCode, b)
	}
	// 钩子路径带的是主令牌不是钩子令牌：同样 401
	if resp, _ := get(t, st, "/support/hook/", false); resp.StatusCode != 401 {
		t.Errorf("/support/hook/ 不是钩子入口，应走主令牌校验得到 401，得到 %d", resp.StatusCode)
	}

	// 没配置 support
	f2 := newFixture(t, nil)
	st2, _, _ := run(t, f2.dir)
	resp, b := get(t, st2, "/support", true)
	if resp.StatusCode != 404 || !strings.Contains(string(b), "没有配置协助") {
		t.Errorf("没配置 support 时应 404: %d %s", resp.StatusCode, b)
	}
	if resp, _ := get(t, st2, "/support/start?minutes=5", true); resp.StatusCode != 404 {
		t.Errorf("没配置 support 时 /support/start 应 404，得到 %d", resp.StatusCode)
	}
	if resp, _ := get(t, st2, "/support", false); resp.StatusCode != 401 {
		t.Errorf("没配置 support 且没令牌应 401，得到 %d", resp.StatusCode)
	}
}

func TestSupportEndToEnd(t *testing.T) {
	f, lg, _ := supportFixture(t, nil)
	st, _, _ := run(t, f.dir)

	resp, b := supportPost(t, st, "/support/start?minutes=5", testOrigin)
	if resp.StatusCode != 200 {
		t.Fatalf("start 应 200，得到 %d: %s", resp.StatusCode, b)
	}
	cmd := lg.wait(t, "endpoint.json.tmp")
	if !strings.HasPrefix(cmd, "umask 077 && mkdir -p ~/.xxyi-support && printf '%s' ") || !strings.Contains(cmd, "mv -f ~/.xxyi-support/endpoint.json.tmp ~/.xxyi-support/endpoint.json") {
		t.Fatalf("写入口文件的命令不对: %s", cmd)
	}
	rest := strings.TrimPrefix(cmd, "umask 077 && mkdir -p ~/.xxyi-support && printf '%s' ")
	q := rest[:strings.Index(rest, " > ")]
	js, ok := unQuote(q)
	if !ok {
		t.Fatalf("JSON 参数没有被单引号包住: %s", q)
	}
	var ep struct {
		Addr  string `json:"addr"`
		Token string `json:"token"`
		Until int64  `json:"until"`
	}
	if err := json.Unmarshal([]byte(js), &ep); err != nil || ep.Addr == "" || ep.Token == "" {
		t.Fatalf("入口 JSON 不对 %q: %v", js, err)
	}
	if now := time.Now().Unix(); ep.Until < now+4*60 || ep.Until > now+6*60 {
		t.Fatalf("入口 JSON 的 until = %d，要在 5 分钟后附近: %s", ep.Until, js)
	}
	found := false
	for _, a := range f.ssh.Forwards() {
		found = found || a == ep.Addr
	}
	if !found {
		t.Fatalf("Forwards %v 里没有 %s", f.ssh.Forwards(), ep.Addr)
	}

	req, _ := http.NewRequest("GET", "http://"+ep.Addr+"/status", nil)
	req.Header.Set("Authorization", "Bearer "+ep.Token)
	r2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	sb, _ := io.ReadAll(r2.Body)
	r2.Body.Close()
	if r2.StatusCode != 200 || !strings.Contains(string(sb), `"active":true`) {
		t.Fatalf("远端 /status 应 200 active=true: %d %s", r2.StatusCode, sb)
	}

	resp, b = supportPost(t, st, "/support/stop", testOrigin)
	if resp.StatusCode != 200 {
		t.Fatalf("stop 应 200，得到 %d: %s", resp.StatusCode, b)
	}
	lg.wait(t, "rm -f ~/.xxyi-support/endpoint.json")
	var derr error
	for i := 0; i < 100; i++ {
		var c net.Conn
		if c, derr = net.DialTimeout("tcp", ep.Addr, time.Second); derr != nil {
			break
		}
		c.Close()
		time.Sleep(50 * time.Millisecond)
	}
	if derr == nil {
		t.Error("stop 后远端地址应连不上")
	}
}

func TestSupportGitTunnelEnv(t *testing.T) {
	f, _, _ := supportFixture(t, func(s, _ map[string]any) { s["git_tunnel"] = "home" })
	st, _, _ := run(t, f.dir)
	if resp, b := get(t, st, "/support", true); resp.StatusCode != 200 {
		t.Fatalf("带 git_tunnel 的配置应能启动: %d %s", resp.StatusCode, b)
	}
}

func TestGitSSHCommand(t *testing.T) {
	got := gitSSHCommand("127.0.0.1:1080", "/h/it's/known_hosts", "/h/id key")
	want := `GIT_SSH_COMMAND=ssh -o ProxyCommand='nc -X 5 -x 127.0.0.1:1080 %h %p' -o UserKnownHostsFile='/h/it'\''s/known_hosts' -o StrictHostKeyChecking=yes -o IdentitiesOnly=yes -i '/h/id key'`
	if got != want {
		t.Errorf("得到\n%s\n期望\n%s", got, want)
	}
}

func TestClaudeArgsRestricted(t *testing.T) {
	sc := &supportConf{
		ClaudeArgs: []string{"--model", "sonnet"},
		Repos:      map[string]supportRepo{"b": {Dir: "/r/b"}, "a": {Dir: "/r/a"}},
	}
	got := strings.Join(claudeArgs(sc, "/m.json"), " ")
	want := "--restricted --permission-prompts none --strict-mcp-config --tools Bash,Read,Edit,Write,Glob,Grep --settings /m.json --add-dir /r/a --add-dir /r/b --model sonnet"
	if got != want {
		t.Errorf("得到 %s\n期望 %s", got, want)
	}
	sc.ClaudeTools = "Read,Grep"
	if g := strings.Join(claudeArgs(sc, "/m.json"), " "); !strings.Contains(g, "--tools Read,Grep ") {
		t.Errorf("claude_tools 未生效: %s", g)
	}
}

// 端到端：Runner 真正启动假 claude 时收到受限模式参数。
func TestSupportRunnerReceivesArgs(t *testing.T) {
	argsOut := filepath.Join(t.TempDir(), "args.txt")
	t.Setenv("CLAUDE_ARGS_OUT", argsOut) // CLAUDE_ 前缀的环境变量会传给 claude
	f, lg, _ := supportFixture(t, nil)
	st, _, _ := run(t, f.dir)
	if resp, b := supportPost(t, st, "/support/start?minutes=5", testOrigin); resp.StatusCode != 200 {
		t.Fatalf("start: %d %s", resp.StatusCode, b)
	}
	cmd := lg.wait(t, "endpoint.json.tmp")
	rest := strings.TrimPrefix(cmd, "umask 077 && mkdir -p ~/.xxyi-support && printf '%s' ")
	js, _ := unQuote(rest[:strings.Index(rest, " > ")])
	var ep struct {
		Addr  string `json:"addr"`
		Token string `json:"token"`
	}
	json.Unmarshal([]byte(js), &ep)
	req, _ := http.NewRequest("POST", "http://"+ep.Addr+"/task", strings.NewReader("hi"))
	req.Header.Set("Authorization", "Bearer "+ep.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var b []byte
	for i := 0; i < 200 && len(b) == 0; i++ {
		time.Sleep(25 * time.Millisecond)
		b, _ = os.ReadFile(argsOut)
	}
	got := string(b)
	merged := filepath.Join(f.dir, "support-claude.merged.json")
	for _, w := range []string{"--restricted\n", "--strict-mcp-config\n", "--tools\nBash,Read,Edit,Write,Glob,Grep\n", "--settings\n" + merged + "\n", "--add-dir\n", "--model\nsonnet\n"} {
		if !strings.Contains(got, w) {
			t.Errorf("参数里缺 %q: %q", w, got)
		}
	}
}

const testOrigin = "http://127.0.0.1:8765"

// supportPost 带主令牌 POST；origin 非空时带上 Origin 头。
func supportPost(t *testing.T, st started, path, origin string) (*http.Response, []byte) {
	t.Helper()
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	req, err := http.NewRequest("POST", "http://"+st.HTTP+path+sep+"token="+st.Token, nil)
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

func TestSupportMutatingRoutesNeedOrigin(t *testing.T) {
	f, _, _ := supportFixture(t, nil)
	st, _, _ := run(t, f.dir)
	for _, p := range []string{"/support/start?minutes=5", "/support/stop", "/support/decide?id=x&allow=1"} {
		if resp, b := supportPost(t, st, p, ""); resp.StatusCode != 403 || !strings.Contains(string(b), "需要从 xxyi 页面操作") {
			t.Errorf("无 Origin 的 POST %s 应 403: %d %s", p, resp.StatusCode, b)
		}
		if resp, _ := supportPost(t, st, p, "http://evil.example"); resp.StatusCode != 403 {
			t.Errorf("不在白名单的 Origin POST %s 应 403，得到 %d", p, resp.StatusCode)
		}
	}
	if resp, b := supportPost(t, st, "/support/start?minutes=5", testOrigin); resp.StatusCode != 200 {
		t.Errorf("带允许的 Origin 应 200: %d %s", resp.StatusCode, b)
	}
	// decide 带 Origin 通过检查（没有这条待批所以 404，而不是 403）
	if resp, _ := supportPost(t, st, "/support/decide?id=x&allow=1", testOrigin); resp.StatusCode != 404 {
		t.Errorf("带 Origin 的 decide 应进入业务逻辑得到 404，得到 %d", resp.StatusCode)
	}
	if resp, b := supportPost(t, st, "/support/stop", testOrigin); resp.StatusCode != 200 {
		t.Errorf("带 Origin 的 stop 应 200: %d %s", resp.StatusCode, b)
	}
	// GET /support 不要求 Origin
	if resp, _ := get(t, st, "/support", true); resp.StatusCode != 200 {
		t.Errorf("GET /support 不应要求 Origin，得到 %d", resp.StatusCode)
	}
}

func TestCheckClaudeArgs(t *testing.T) {
	ok := [][]string{
		nil,
		{"--model", "sonnet"},
		{"--max-budget-usd=5"},
		{"--model", "sonnet", "--effort=high", "--max-turns", "20", "--fallback-model", "haiku", "--append-system-prompt", "hi"},
	}
	for _, a := range ok {
		if err := checkClaudeArgs(a); err != nil {
			t.Errorf("%v 应通过: %v", a, err)
		}
	}
	bad := [][]string{
		{"--plugin-url", "http://x"},
		{"--worktree"},
		{"positional"},
		{"--model"},
		{"--model", "--effort"},
		{"--model="},
		{"--permission-prompts", "none"},
		{"--dangerously-skip-permissions"},
		{"--model", "sonnet", "extra"},
	}
	for _, a := range bad {
		err := checkClaudeArgs(a)
		if err == nil || !strings.Contains(err.Error(), "claude_args 只允许") {
			t.Errorf("%v 应报「claude_args 只允许」错误，得到 %v", a, err)
		}
	}
}

func TestMergedSettingsDoesNotMutateBase(t *testing.T) {
	base := goodSettings()
	base["sandbox"].(map[string]any)["filesystem"] = map[string]any{"denyRead": []any{"~/x"}, "allowWrite": []any{"/w"}}
	before, _ := json.Marshal(base)
	out := mergedSettings(base, "cmd", []string{"~/.ssh", "~/x", "/cfg"})
	after, _ := json.Marshal(base)
	if string(before) != string(after) {
		t.Errorf("mergedSettings 改动了入参:\n%s\n%s", before, after)
	}
	fsm := out["sandbox"].(map[string]any)["filesystem"].(map[string]any)
	if got, _ := json.Marshal(fsm["denyRead"]); string(got) != `["~/x","~/.ssh","/cfg"]` {
		t.Errorf("denyRead 合并结果不对: %s", got)
	}
	if fsm["allowWrite"] == nil {
		t.Errorf("filesystem 其他键丢失: %v", fsm)
	}
	// 没有 filesystem 时也能建出来
	out2 := mergedSettings(goodSettings(), "cmd", []string{"/cfg"})
	if v, ok := jsonPath(out2, "sandbox", "filesystem", "denyRead"); !ok || len(v.([]any)) != 1 {
		t.Errorf("无 filesystem 时应新建 denyRead: %v", out2["sandbox"])
	}
}

// 目录隔离：配置目录/设置文件目录/wgtund 目录不能与工作目录或仓库互相包含。
func TestSupportDirIsolation(t *testing.T) {
	t.Run("workdir 就是配置目录", func(t *testing.T) {
		f, _, _ := supportFixture(t, nil)
		patchSupport(t, f.dir, func(sup map[string]any) {
			sup["workdir"] = f.dir
			sup["repos"] = map[string]any{}
		})
		err := runErr(t, f.dir)
		if err == nil || !strings.HasPrefix(err.Error(), "hosts.json: support") || !strings.Contains(err.Error(), "互相包含") {
			t.Fatalf("应报互相包含: %v", err)
		}
	})
	t.Run("workdir 在配置目录之内", func(t *testing.T) {
		f, _, _ := supportFixture(t, nil)
		sub := filepath.Join(f.dir, "work")
		os.MkdirAll(sub, 0o755)
		patchSupport(t, f.dir, func(sup map[string]any) {
			sup["workdir"] = sub
			sup["repos"] = map[string]any{}
		})
		if err := runErr(t, f.dir); err == nil || !strings.Contains(err.Error(), "互相包含") {
			t.Fatalf("应报互相包含: %v", err)
		}
	})
	t.Run("repo.dir 包含配置目录", func(t *testing.T) {
		f, _, _ := supportFixture(t, nil)
		parent := filepath.Dir(f.dir)
		gitInit(t, parent)
		patchSupport(t, f.dir, func(sup map[string]any) {
			sup["repos"] = map[string]any{"r": map[string]any{"dir": parent, "remote": "x"}}
		})
		err := runErr(t, f.dir)
		if err == nil || !strings.HasPrefix(err.Error(), "hosts.json: support") || !strings.Contains(err.Error(), "互相包含") {
			t.Fatalf("应报互相包含: %v", err)
		}
	})
	t.Run("设置文件目录包含工作目录", func(t *testing.T) {
		f, _, tmp := supportFixture(t, nil)
		// 把设置文件挪到 tmp（工作目录 tmp/work 的上一级）
		sp := filepath.Join(tmp, "settings.json")
		b, _ := json.Marshal(goodSettings())
		os.WriteFile(sp, b, 0o600)
		patchSupport(t, f.dir, func(sup map[string]any) { sup["claude_settings"] = sp })
		if err := runErr(t, f.dir); err == nil || !strings.Contains(err.Error(), "互相包含") {
			t.Fatalf("应报互相包含: %v", err)
		}
	})
	t.Run("路径按分段比较而非字符串前缀", func(t *testing.T) {
		if overlaps("/a/work", "/a/workdir") || !overlaps("/a/work", "/a/work/x") || !overlaps("/a/work/x", "/a/work") {
			t.Fatal("overlaps 判断不对")
		}
	})
}

// autoAllowBashIfSandboxed 写 false 同样合规；合法的 claude_args 能启动。
func TestSupportAutoAllowFalseAndArgsAccepted(t *testing.T) {
	f, _, _ := supportFixture(t, func(s, st map[string]any) {
		st["sandbox"].(map[string]any)["autoAllowBashIfSandboxed"] = false
		s["claude_args"] = []string{"--model", "sonnet", "--max-budget-usd=5"}
	})
	st, _, _ := run(t, f.dir)
	if resp, b := get(t, st, "/support", true); resp.StatusCode != 200 {
		t.Fatalf("应能启动: %d %s", resp.StatusCode, b)
	}
}
