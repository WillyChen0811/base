package wgtund

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/WillyChen0811/base/go/tmux"
	"github.com/coder/websocket"
)

// ── 假 tmux：按 tmux 包拼出来的命令字符串在内存里维护会话表 ──

type fakeTmux struct {
	mu       sync.Mutex
	sessions map[string]*fakeSess
	nextTS   int64
}

type fakeSess struct {
	created, activity int64
	windows           int
	content           string
}

func newFakeTmux() *fakeTmux {
	return &fakeTmux{sessions: map[string]*fakeSess{}, nextTS: 1_760_000_000}
}

// unQuote 还原 tmux.Quote 的单引号参数。
func unQuote(s string) (string, bool) {
	if len(s) < 2 || s[0] != '\'' || s[len(s)-1] != '\'' {
		return "", false
	}
	return strings.ReplaceAll(s[1:len(s)-1], `'\''`, `'`), true
}

// argAfter 取 cmd 中 flag 后面那个（可能被单引号包住的）参数。
func argAfter(cmd, flag string) (string, bool) {
	i := strings.Index(cmd, " "+flag+" ")
	if i < 0 {
		return "", false
	}
	rest := cmd[i+len(flag)+2:]
	// capture 的 -t 后面还有 " -S -N"
	if j := strings.Index(rest, " -S -"); j >= 0 {
		rest = rest[:j]
	}
	return unQuote(rest)
}

func (f *fakeTmux) exec(cmd string) (stdout, stderr string, code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case cmd == tmux.ListCommand():
		if len(f.sessions) == 0 {
			return "", "no server running on /tmp/tmux-1000/default", 1
		}
		names := make([]string, 0, len(f.sessions))
		for n := range f.sessions {
			names = append(names, n)
		}
		sort.Strings(names)
		var b strings.Builder
		for _, n := range names {
			s := f.sessions[n]
			fmt.Fprintf(&b, "0\t%d\t%d\t%d\t%s\n", s.windows, s.created, s.activity, n)
		}
		return b.String(), "", 0

	case strings.HasPrefix(cmd, "tmux new-session -d -s "):
		name, ok := argAfter(cmd, "-s")
		if !ok {
			return "", "fake tmux: 命令解析失败", 1
		}
		if _, dup := f.sessions[name]; dup {
			return "", "duplicate session: "+name, 1
		}
		f.nextTS++
		f.sessions[name] = &fakeSess{created: f.nextTS, activity: f.nextTS, windows: 1, content: "pane:" + name + "\n"}
		return "", "", 0

	case strings.HasPrefix(cmd, "tmux kill-session -t "):
		target, ok := argAfter(cmd, "-t")
		if !ok {
			return "", "fake tmux: 命令解析失败", 1
		}
		name := strings.TrimPrefix(target, "=")
		if _, ok := f.sessions[name]; !ok {
			return "", "can't find session: "+name, 1
		}
		delete(f.sessions, name)
		return "", "", 0 // 会话清空后 list 会报"无服务"，与真 tmux 一致

	case strings.HasPrefix(cmd, "tmux capture-pane "):
		target, ok := argAfter(cmd, "-t")
		if !ok {
			return "", "fake tmux: 命令解析失败", 1
		}
		name := strings.TrimSuffix(strings.TrimPrefix(target, "="), ":")
		s, ok := f.sessions[name]
		if !ok {
			return "", "can't find session: "+name, 1
		}
		return s.content, "", 0

	default:
		return "", "fake tmux: 不支持的命令 " + cmd, 127
	}
}

// ── 测试辅助 ──

func call(t *testing.T, st started, method, path string) (*http.Response, []byte) {
	t.Helper()
	u := "http://" + st.HTTP + path
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	// POST 不带请求体和自定义头：跨域时属于简单请求，浏览器不发预检
	req, err := http.NewRequest(method, u+sep+"token="+st.Token, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

// wsRoundTrip 写一行读一行，确认这个终端会话已经在远端启动。
func wsRoundTrip(t *testing.T, c *websocket.Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageBinary, []byte("x\n")); err != nil {
		t.Fatal(err)
	}
	for {
		typ, b, err := c.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if typ == websocket.MessageBinary && strings.Contains(string(b), "echo:") {
			return
		}
	}
}

type sessionItem struct {
	Name     string `json:"name"`
	Attached int    `json:"attached"`
	Windows  int    `json:"windows"`
	Created  int64  `json:"created"`
	Activity int64  `json:"activity"`
}

func listSessions(t *testing.T, st started) []sessionItem {
	t.Helper()
	resp, b := call(t, st, "GET", "/sessions?host=ubuntu")
	if resp.StatusCode != 200 {
		t.Fatalf("GET /sessions 应 200，得到 %d: %s", resp.StatusCode, b)
	}
	var out struct {
		Sessions []sessionItem `json:"sessions"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("解析失败 %v: %s", err, b)
	}
	return out.Sessions
}

func newSession(t *testing.T, st started, name string) *http.Response {
	t.Helper()
	resp, b := call(t, st, "POST", "/sessions/new?host=ubuntu&name="+name)
	t.Cleanup(func() { resp.Body.Close() })
	_ = b
	return resp
}

// ── 测试 ──

func TestSessionsListEmptyNoServer(t *testing.T) {
	f := newFakeTmux()
	fx := newFixtureExec(t, nil, f.exec)
	st, _, _ := run(t, fx.dir)
	resp, b := call(t, st, "GET", "/sessions?host=ubuntu")
	if resp.StatusCode != 200 {
		t.Fatalf("无服务应 200，得到 %d: %s", resp.StatusCode, b)
	}
	if !strings.Contains(string(b), `"sessions":[]`) {
		t.Errorf("空列表应是 [] 不是 null: %s", b)
	}
}

func TestSessionsNewListKill(t *testing.T) {
	f := newFakeTmux()
	fx := newFixtureExec(t, nil, f.exec)
	st, _, _ := run(t, fx.dir)

	if resp := newSession(t, st, "alpha"); resp.StatusCode != 200 {
		t.Fatalf("新建 alpha 应 200，得到 %d", resp.StatusCode)
	}
	if resp := newSession(t, st, "beta"); resp.StatusCode != 200 {
		t.Fatalf("新建 beta 应 200，得到 %d", resp.StatusCode)
	}
	list := listSessions(t, st)
	if len(list) != 2 {
		t.Fatalf("应有两个会话: %+v", list)
	}
	f.mu.Lock()
	tsA, tsB := f.sessions["alpha"].created, f.sessions["beta"].created
	f.mu.Unlock()
	want := map[string]sessionItem{
		"alpha": {Name: "alpha", Windows: 1, Created: tsA, Activity: tsA},
		"beta":  {Name: "beta", Windows: 1, Created: tsB, Activity: tsB},
	}
	for _, s := range list {
		w, ok := want[s.Name]
		if !ok {
			t.Errorf("意外会话 %q", s.Name)
			continue
		}
		if s != w {
			t.Errorf("会话 %+v，期望 %+v", s, w)
		}
	}

	// 重名 409
	if resp := newSession(t, st, "alpha"); resp.StatusCode != http.StatusConflict {
		t.Errorf("重名应 409，得到 %d", resp.StatusCode)
	}
	// 结束
	resp, b := call(t, st, "POST", "/sessions/kill?host=ubuntu&name=beta")
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"ok":true`) {
		t.Fatalf("kill 应 200 ok:true，得到 %d: %s", resp.StatusCode, b)
	}
	list = listSessions(t, st)
	if len(list) != 1 || list[0].Name != "alpha" {
		t.Errorf("kill 后应只剩 alpha: %+v", list)
	}
	// 不存在 404
	if resp, _ := call(t, st, "POST", "/sessions/kill?host=ubuntu&name=beta"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("kill 不存在的会话应 404，得到 %d", resp.StatusCode)
	}
}

func TestSessionsNewBadInput(t *testing.T) {
	f := newFakeTmux()
	fx := newFixtureExec(t, nil, f.exec)
	st, _, _ := run(t, fx.dir)

	for _, name := range []string{"a b", "-x"} {
		resp, b := call(t, st, "POST", "/sessions/new?host=ubuntu&name="+url.QueryEscape(name))
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("非法名 %q 应 400，得到 %d: %s", name, resp.StatusCode, b)
		}
	}
	if resp, b := call(t, st, "POST", "/sessions/new?host=nope&name=ok"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("未知主机应 404，得到 %d: %s", resp.StatusCode, b)
	}
	if resp, _ := call(t, st, "GET", "/sessions/new?host=ubuntu&name=x"); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /sessions/new 应 405，得到 %d", resp.StatusCode)
	}
	if resp, _ := call(t, st, "POST", "/sessions?host=ubuntu"); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /sessions 应 405，得到 %d", resp.StatusCode)
	}
}

func TestCapture(t *testing.T) {
	f := newFakeTmux()
	fx := newFixtureExec(t, nil, f.exec)
	st, _, _ := run(t, fx.dir)
	if resp := newSession(t, st, "work"); resp.StatusCode != 200 {
		t.Fatalf("新建失败 %d", resp.StatusCode)
	}
	resp, b := call(t, st, "GET", "/capture?host=ubuntu&name=work")
	if resp.StatusCode != 200 {
		t.Fatalf("capture 应 200，得到 %d: %s", resp.StatusCode, b)
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	if out.Text != "pane:work\n" {
		t.Errorf("text = %q", out.Text)
	}
	for _, lines := range []string{"0", "9999"} {
		if resp, _ := call(t, st, "GET", "/capture?host=ubuntu&name=work&lines="+lines); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("lines=%s 应 400，得到 %d", lines, resp.StatusCode)
		}
	}
	if resp, _ := call(t, st, "GET", "/capture?host=ubuntu&name=gone"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("capture 不存在的会话应 404，得到 %d", resp.StatusCode)
	}
}

func TestConnReuse(t *testing.T) {
	f := newFakeTmux()
	fx := newFixtureExec(t, nil, f.exec)
	st, _, _ := run(t, fx.dir)

	// 连续多次 HTTP 调用
	for i := 0; i < 3; i++ {
		listSessions(t, st)
	}
	if resp := newSession(t, st, "s1"); resp.StatusCode != 200 {
		t.Fatalf("新建 s1 应 200，得到 %d", resp.StatusCode)
	}
	// 两个终端共用同一条连接；写一行读一行，确认远端真的把 attach 跑起来了
	c1 := dialTerm(t, st, "host=ubuntu&session=s1&cols=80&rows=24")
	wsRoundTrip(t, c1)
	c2 := dialTerm(t, st, "host=ubuntu&session=s1&cols=100&rows=30")
	wsRoundTrip(t, c2)

	if n := fx.ssh.Conns(); n != 1 {
		t.Errorf("应只拨一次，Conns()=%d", n)
	}
	// attach 命令确实到达远端
	attach, err := tmux.AttachCommand("s1")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range fx.ssh.Commands() {
		if c == "exec:"+attach {
			found = true
		}
	}
	if !found {
		t.Errorf("远端应收到 %q，实际 %v", "exec:"+attach, fx.ssh.Commands())
	}
}

func TestReconnectAfterServerClose(t *testing.T) {
	f := newFakeTmux()
	fx := newFixtureExec(t, nil, f.exec)
	st, _, _ := run(t, fx.dir)
	if resp := newSession(t, st, "keep"); resp.StatusCode != 200 {
		t.Fatalf("新建失败 %d", resp.StatusCode)
	}
	if n := fx.ssh.Conns(); n != 1 {
		t.Fatalf("应先只有 1 条连接，Conns()=%d", n)
	}
	// 服务器端掐断这条连接
	fx.ssh.CloseConns()
	// 下一次请求应重拨成功
	list := listSessions(t, st)
	if len(list) != 1 || list[0].Name != "keep" {
		t.Errorf("重拨后列表 %+v", list)
	}
	if n := fx.ssh.Conns(); n != 2 {
		t.Errorf("重拨后 Conns() 应为 2，得到 %d", n)
	}
}

func TestHostsConnectedAndDialError(t *testing.T) {
	f := newFakeTmux()
	fx := newFixtureExec(t, func(cfg map[string]any) {
		hosts := cfg["hosts"].([]map[string]any)
		cfg["hosts"] = append(hosts, map[string]any{
			"name": "dead", "addr": "127.0.0.1:1", "user": "x",
			"identity_files": hosts[0]["identity_files"],
		})
	}, f.exec)
	st, _, _ := run(t, fx.dir)

	resp, b := call(t, st, "GET", "/sessions?host=ubuntu")
	if resp.StatusCode != 200 {
		t.Fatalf("列会话应 200，得到 %d: %s", resp.StatusCode, b)
	}
	resp, b = get(t, st, "/hosts", true)
	var items []map[string]any
	if err := json.Unmarshal(b, &items); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	byName := map[string]map[string]any{}
	for _, it := range items {
		byName[it["name"].(string)] = it
	}
	if byName["ubuntu"]["connected"] != true {
		t.Errorf("用过后 ubuntu.connected 应为 true: %v", byName["ubuntu"])
	}
	if byName["ubuntu"]["error"] != "" {
		t.Errorf("ubuntu 无错误应为空串: %v", byName["ubuntu"])
	}

	resp, b = call(t, st, "GET", "/sessions?host=dead")
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("拨不通应 502，得到 %d: %s", resp.StatusCode, b)
	}
	var e map[string]string
	if err := json.Unmarshal(b, &e); err != nil || e["error"] == "" {
		t.Errorf("错误正文应是 {\"error\":...}，得到 %s", b)
	}
	resp, b = get(t, st, "/hosts", true)
	items = nil
	json.Unmarshal(b, &items)
	for _, it := range items {
		if it["name"] == "dead" && it["error"] == "" {
			t.Errorf("dead 的 error 应非空: %v", it)
		}
	}
	if strings.Contains(string(b), "id_test") || strings.Contains(string(b), "PRIVATE") {
		t.Errorf("/hosts 不应暴露私钥: %s", b)
	}
}

func TestNewAPIsTokenAndCORS(t *testing.T) {
	f := newFakeTmux()
	fx := newFixtureExec(t, nil, f.exec)
	st, _, _ := run(t, fx.dir)

	for _, p := range []string{"/sessions?host=ubuntu", "/capture?host=ubuntu&name=x"} {
		if resp, _ := get(t, st, p, false); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s 无令牌应 401，得到 %d", p, resp.StatusCode)
		}
	}
	{
		req, _ := http.NewRequest("POST", "http://"+st.HTTP+"/sessions/new?host=ubuntu&name=cors1", nil)
		req.Header.Set("Origin", "http://127.0.0.1:8765")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "http://127.0.0.1:8765" {
			t.Errorf("POST /sessions/new ACAO = %q", got)
		}
	}
	{
		req, _ := http.NewRequest("POST", "http://"+st.HTTP+"/sessions/new?host=ubuntu&name=x", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("POST 无令牌应 401，得到 %d", resp.StatusCode)
		}
	}
	{
		req, _ := http.NewRequest("GET", "http://"+st.HTTP+"/sessions?host=ubuntu&token="+st.Token, nil)
		req.Header.Set("Origin", "http://evil.example")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("白名单外的 Origin 不该给 ACAO，得到 %q", got)
		}
	}
}

func TestTermAttachBadSession(t *testing.T) {
	f := newFakeTmux()
	fx := newFixtureExec(t, nil, f.exec)
	st, _, _ := run(t, fx.dir)
	// 名字含控制字符：open 失败，收到 error 控制消息
	c := dialTerm(t, st, "host=ubuntu&session=a%01b&cols=80&rows=24")
	if m := readCtrl(t, c); m["type"] != "error" {
		t.Errorf("非法会话名应回 error: %v", m)
	}
}

