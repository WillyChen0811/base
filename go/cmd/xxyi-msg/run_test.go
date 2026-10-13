package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// captured 记假服务收到的请求；假服务在别的 goroutine 里写，测试读要加锁。
type captured struct {
	mu      sync.Mutex
	reqs    []capturedReq
	respID  string
	respErr string // 非空则回 400 + {"error":...}
}

type capturedReq struct {
	method string
	path   string
	auth   string
	query  string
	body   sendBody
}

func (c *captured) add(r capturedReq) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reqs = append(c.reqs, r)
}

func (c *captured) last(t *testing.T) capturedReq {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.reqs) == 0 {
		t.Fatal("假服务没收到请求")
	}
	return c.reqs[len(c.reqs)-1]
}

func (c *captured) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.reqs)
}

// handler 统一处理 /msg/send、/msg/state、/msg/who、/msg。
func (c *captured) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rec := capturedReq{
			method: r.Method, path: r.URL.Path,
			auth:  r.Header.Get("Authorization"),
			query: r.URL.RawQuery,
		}
		if r.Method == http.MethodPost && r.URL.Path == "/msg/send" {
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &rec.body)
		}
		c.add(rec)
		if c.respErr != "" {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, `{"error":%q}`, c.respErr)
			return
		}
		switch r.URL.Path {
		case "/msg/send":
			id := c.respID
			if id == "" {
				id = "ab12"
			}
			fmt.Fprintf(w, `{"id":%q}`, id)
		case "/msg/state":
			fmt.Fprint(w, `{"ok":true}`)
		case "/msg/who", "/msg":
			fmt.Fprint(w, `{"peers":{"ubuntu:b":"busy","mac:a":"idle","ubuntu:c":"idle"}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":"没有这个路径"}`)
		}
	}
}

// setPaths 把配置文件路径指到临时目录并保证还原。
func setPaths(t *testing.T, ep, cfg string) {
	t.Helper()
	oldEP, oldCfg := endpointPath, xxyiConfigPath
	t.Cleanup(func() { endpointPath, xxyiConfigPath = oldEP, oldCfg })
	endpointPath, xxyiConfigPath = ep, cfg
}

// authority 取假服务 URL 的 host:port。
func authority(srvURL string) string {
	return strings.TrimPrefix(srvURL, "http://")
}

// portOf 取假服务 URL 的端口部分。
func portOf(t *testing.T, srvURL string) string {
	t.Helper()
	_, port, err := net.SplitHostPort(authority(srvURL))
	if err != nil {
		t.Fatalf("假服务地址不对 %q：%v", srvURL, err)
	}
	return port
}

// useEndpoint 写一个指向假服务的 endpoint.json（Ubuntu 一侧）。
func useEndpoint(t *testing.T, srvURL string) {
	t.Helper()
	dir := t.TempDir()
	ep := filepath.Join(dir, "endpoint.json")
	if err := os.WriteFile(ep, []byte(fmt.Sprintf(`{"addr":%q,"token":"tk"}`, authority(srvURL))), 0o600); err != nil {
		t.Fatal(err)
	}
	setPaths(t, ep, filepath.Join(dir, "config.json"))
}

func TestUbuntuSend(t *testing.T) {
	var cap captured
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()
	useEndpoint(t, srv.URL)
	t.Setenv("XXYI_ADDR", "ubuntu:cw")

	dir := t.TempDir()
	file := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(file, []byte("附件内容"), 0o600); err != nil {
		t.Fatal(err)
	}

	out := &bytes.Buffer{}
	// 选项写在位置参数前面也要能解析
	code := run([]string{"send", "--file", file, "mac:boss", "你好", "--repo", "base", "--branch", "feat"}, strings.NewReader(""), out, io.Discard)
	if code != exitOK {
		t.Fatalf("退出码 %d，输出 %q", code, out.String())
	}
	if out.String() != "已排队 ab12\n" {
		t.Fatalf("输出 %q", out.String())
	}
	got := cap.last(t)
	if got.auth != "Bearer tk" {
		t.Errorf("Authorization 头 %q", got.auth)
	}
	if got.path != "/msg/send" || got.method != http.MethodPost {
		t.Errorf("%s %s", got.method, got.path)
	}
	if got.body.From != "ubuntu:cw" || got.body.To != "mac:boss" || got.body.Text != "你好" {
		t.Errorf("正文 %+v", got.body)
	}
	if got.body.FileName != "note.txt" || string(got.body.File) != "附件内容" {
		t.Errorf("附件 %q %q", got.body.FileName, got.body.File)
	}
	if got.body.Repo != "base" || got.body.Branch != "feat" {
		t.Errorf("repo/branch %+v", got.body)
	}
}

func TestSendStdinAndEmptyText(t *testing.T) {
	var cap captured
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()
	useEndpoint(t, srv.URL)
	t.Setenv("XXYI_ADDR", "ubuntu:cw")

	out := &bytes.Buffer{}
	if code := run([]string{"send", "mac:boss"}, strings.NewReader("来自管道\n"), out, io.Discard); code != exitOK {
		t.Fatalf("退出码 %d", code)
	}
	if got := cap.last(t); got.body.Text != "来自管道\n" {
		t.Errorf("正文文字 %q", got.body.Text)
	}

	out.Reset()
	if code := run([]string{"send", "mac:boss", "-"}, strings.NewReader("abc"), out, io.Discard); code != exitOK {
		t.Fatalf("退出码 %d", code)
	}
	if got := cap.last(t); got.body.Text != "abc" {
		t.Errorf("正文文字 %q", got.body.Text)
	}

	errOut := &bytes.Buffer{}
	if code := run([]string{"send", "mac:boss"}, strings.NewReader(""), io.Discard, errOut); code != exitErr {
		t.Fatalf("空文字退出码 %d，stderr %q", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "消息文字为空") {
		t.Errorf("stderr %q", errOut.String())
	}
}

func TestSendServerErrorAndConnFail(t *testing.T) {
	var cap captured
	cap.respErr = "收件人不存在"
	srv := httptest.NewServer(cap.handler())
	useEndpoint(t, srv.URL)
	t.Setenv("XXYI_ADDR", "ubuntu:cw")

	errOut := &bytes.Buffer{}
	if code := run([]string{"send", "mac:nope", "hi"}, strings.NewReader(""), io.Discard, errOut); code != exitErr {
		t.Fatalf("退出码 %d", code)
	}
	if !strings.Contains(errOut.String(), "发送失败：收件人不存在") {
		t.Errorf("stderr %q", errOut.String())
	}
	srv.Close()

	errOut.Reset()
	if code := run([]string{"send", "mac:boss", "hi"}, strings.NewReader(""), io.Discard, errOut); code != exitErr {
		t.Fatalf("退出码 %d", code)
	}
	if !strings.Contains(errOut.String(), "连不上 XXYI：") || !strings.Contains(errOut.String(), "协助开着吗") {
		t.Errorf("stderr %q", errOut.String())
	}
}

func TestMacSide(t *testing.T) {
	var msgCap captured
	msgSrv := httptest.NewServer(msgCap.handler())
	defer msgSrv.Close()

	api := http.NewServeMux()
	api.HandleFunc("/api/terminal", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"http":%q,"token":"mtk"}`, authority(msgSrv.URL))
	})
	apiSrv := httptest.NewServer(api)
	defer apiSrv.Close()

	dir := t.TempDir()
	// 没有 endpoint.json → 走 Mac 一侧；http_port 用字符串覆盖缺省 8765
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(fmt.Sprintf(`{"http_port":%q}`, portOf(t, apiSrv.URL))), 0o600); err != nil {
		t.Fatal(err)
	}
	setPaths(t, filepath.Join(dir, "endpoint.json"), filepath.Join(dir, "config.json"))
	t.Setenv("XXYI_ADDR", "mac:cw")

	out := &bytes.Buffer{}
	if code := run([]string{"send", "ubuntu:cw", "hi"}, strings.NewReader(""), out, io.Discard); code != exitOK {
		t.Fatalf("退出码 %d", code)
	}
	got := msgCap.last(t)
	if got.query != "token=mtk" {
		t.Errorf("查询参数 %q，令牌应在查询参数里", got.query)
	}
	if got.auth != "" {
		t.Errorf("Mac 侧不该带 Authorization 头：%q", got.auth)
	}
	if got.body.From != "mac:cw" || got.body.Text != "hi" {
		t.Errorf("正文 %+v", got.body)
	}

	out.Reset()
	if code := run([]string{"who"}, nil, out, io.Discard); code != exitOK {
		t.Fatalf("退出码 %d", code)
	}
	who := msgCap.last(t)
	if who.path != "/msg" || who.query != "token=mtk" {
		t.Errorf("Mac 侧 who：%s?%s", who.path, who.query)
	}
	want := "mac:a idle\nubuntu:b busy\nubuntu:c idle\n"
	if out.String() != want {
		t.Errorf("who 输出 %q", out.String())
	}
}

func TestWhoSorted(t *testing.T) {
	var cap captured
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()
	useEndpoint(t, srv.URL)

	out := &bytes.Buffer{}
	if code := run([]string{"who"}, nil, out, io.Discard); code != exitOK {
		t.Fatalf("退出码 %d", code)
	}
	want := "mac:a idle\nubuntu:b busy\nubuntu:c idle\n"
	if out.String() != want {
		t.Errorf("who 输出 %q", out.String())
	}
	if got := cap.last(t); got.path != "/msg/who" {
		t.Errorf("who 路径 %q", got.path)
	}
}

func TestSelfAddr(t *testing.T) {
	var cap captured
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()
	useEndpoint(t, srv.URL)

	var tmuxCalled bool
	oldTmux := tmuxSession
	t.Cleanup(func() { tmuxSession = oldTmux })
	tmuxSession = func() (string, error) { tmuxCalled = true; return "sess1", nil }

	// XXYI_ADDR 优先，哪怕 TMUX 也设了
	t.Setenv("XXYI_ADDR", "ubuntu:env")
	t.Setenv("TMUX", "/tmp/tmux-1000/default,123,0")
	if code := run([]string{"send", "mac:boss", "hi"}, strings.NewReader(""), io.Discard, io.Discard); code != exitOK {
		t.Fatalf("退出码 %d", code)
	}
	if got := cap.last(t); got.body.From != "ubuntu:env" {
		t.Errorf("from %q", got.body.From)
	}
	if tmuxCalled {
		t.Error("XXYI_ADDR 有值时不该碰 tmux")
	}

	// 只有 TMUX：用注入的会话名
	t.Setenv("XXYI_ADDR", "")
	if code := run([]string{"send", "mac:boss", "hi"}, strings.NewReader(""), io.Discard, io.Discard); code != exitOK {
		t.Fatalf("退出码 %d", code)
	}
	if got := cap.last(t); got.body.From != "ubuntu:sess1" {
		t.Errorf("from %q", got.body.From)
	}
	if !tmuxCalled {
		t.Error("TMUX 设了应该取会话名")
	}

	// 都没有：send 退出 2，state 静默 0
	t.Setenv("TMUX", "")
	tmuxCalled = false
	errOut := &bytes.Buffer{}
	if code := run([]string{"send", "mac:boss", "hi"}, strings.NewReader(""), io.Discard, errOut); code != exitErr {
		t.Fatalf("退出码 %d", code)
	}
	if !strings.Contains(errOut.String(), "不知道自己的地址：请在 XXYI 的终端里运行，或设 XXYI_ADDR") {
		t.Errorf("stderr %q", errOut.String())
	}
	if n := cap.count(); n != 2 {
		t.Errorf("send 判失败前不该发请求，已发 %d", n)
	}
	if code := run([]string{"state", "busy"}, nil, io.Discard, io.Discard); code != exitOK {
		t.Fatalf("没地址时 state 退出码 %d", code)
	}
	if n := cap.count(); n != 2 {
		t.Errorf("没地址时 state 不该发请求，已发 %d", n)
	}
}

func TestStateReportsAndSucceeds(t *testing.T) {
	var cap captured
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()
	useEndpoint(t, srv.URL)
	t.Setenv("XXYI_ADDR", "ubuntu:cw")

	errOut := &bytes.Buffer{}
	if code := run([]string{"state", "busy"}, strings.NewReader(`{"huge":"钩子塞的 JSON，不该被读"}`), io.Discard, errOut); code != exitOK {
		t.Fatalf("退出码 %d", code)
	}
	if errOut.Len() != 0 {
		t.Errorf("state 出错前不该有输出：%q", errOut.String())
	}
	got := cap.last(t)
	if got.path != "/msg/state" || !strings.Contains(got.query, "addr=ubuntu%3Acw") || !strings.Contains(got.query, "state=busy") {
		t.Errorf("state 请求 %s?%s", got.path, got.query)
	}
}

func TestStateNeverRespondsReturnsZeroFast(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer srv.Close()
	useEndpoint(t, srv.URL)
	t.Setenv("XXYI_ADDR", "ubuntu:cw")

	start := time.Now()
	if code := run([]string{"state", "busy"}, nil, io.Discard, io.Discard); code != exitOK {
		t.Fatalf("退出码 %d", code)
	}
	if d := time.Since(start); d > 2500*time.Millisecond {
		t.Fatalf("耗时 %v，超过 2.5 秒", d)
	}
}

func TestSendArgValidation(t *testing.T) {
	var cap captured
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()
	useEndpoint(t, srv.URL)
	t.Setenv("XXYI_ADDR", "ubuntu:cw")

	cases := []struct {
		args []string
		want string
	}{
		{[]string{"send", "mac:boss", "hi", "--repo", "base"}, "--repo 和 --branch 必须一起给"},
		{[]string{"send", "boss", "hi"}, "必须形如 mac:<名>"},
		{[]string{"send"}, "缺少收件人"},
		{[]string{"send", "mac:boss", "hi", "--nope", "x"}, "未知选项"},
	}
	for _, c := range cases {
		errOut := &bytes.Buffer{}
		if code := run(c.args, strings.NewReader(""), io.Discard, errOut); code != exitErr {
			t.Errorf("%v 退出码 %d", c.args, code)
		}
		if !strings.Contains(errOut.String(), c.want) {
			t.Errorf("%v stderr %q，期望含 %q", c.args, errOut.String(), c.want)
		}
	}

	// 附件超 1MB：不发请求就报错
	dir := t.TempDir()
	big := filepath.Join(dir, "big.bin")
	if err := os.WriteFile(big, bytes.Repeat([]byte("x"), maxAttachBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	errOut := &bytes.Buffer{}
	n := cap.count()
	if code := run([]string{"send", "mac:boss", "hi", "--file", big}, strings.NewReader(""), io.Discard, errOut); code != exitErr {
		t.Fatalf("退出码 %d", code)
	}
	if !strings.Contains(errOut.String(), "附件超过 1MB") {
		t.Errorf("stderr %q", errOut.String())
	}
	if cap.count() != n {
		t.Errorf("附件超限不该发请求，已发 %d", cap.count()-n)
	}
}
