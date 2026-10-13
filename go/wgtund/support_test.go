package wgtund

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/WillyChen0811/base/go/internal/sshtest"
)

// 假 claude：读一行输出两行事件，循环。
const fakeClaude = `#!/bin/sh
[ -n "$ARGS_OUT" ] && printf '%s\n' "$@" > "$ARGS_OUT"
while read -r line; do
  echo '{"type":"system","subtype":"init","session_id":"s1"}'
  echo '{"type":"result","subtype":"success","result":"ok"}'
done
`

func goodSettings() map[string]any {
	return map[string]any{
		"model":   "sonnet",
		"sandbox": map[string]any{"enabled": true, "allowUnsandboxedCommands": false, "failIfUnavailable": true},
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
	sp := filepath.Join(tmp, "settings.json")
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

	resp, b := call(t, st, "POST", "/support/start?minutes=5")
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
	var ep struct{ Addr, Token string }
	if err := json.Unmarshal([]byte(js), &struct {
		A *string `json:"addr"`
		T *string `json:"token"`
	}{&ep.Addr, &ep.Token}); err != nil || ep.Addr == "" || ep.Token == "" {
		t.Fatalf("入口 JSON 不对 %q: %v", js, err)
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

	resp, b = call(t, st, "POST", "/support/stop")
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
	want := "--restricted --strict-mcp-config --tools Bash,Read,Edit,Write,Glob,Grep --settings /m.json --add-dir /r/a --add-dir /r/b --model sonnet"
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
	t.Setenv("ARGS_OUT", argsOut)
	f, lg, _ := supportFixture(t, nil)
	st, _, _ := run(t, f.dir)
	if resp, b := call(t, st, "POST", "/support/start?minutes=5"); resp.StatusCode != 200 {
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
