package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
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

	"github.com/WillyChen0811/base/go/xxyica"
)

// captured 记假服务收到的请求；假服务在别的 goroutine 里写，测试读要加锁。
type captured struct {
	mu          sync.Mutex
	reqs        []capturedReq
	respID      string
	respErr     string // 非空则所有接口回 400 + {"error":...}
	respSendErr string // 非空则只有 /msg/send 回 400（模拟补发被拒）

	// 各接口的响应正文，留空用默认值（都在建服务之前设好，不与处理协程并发写）。
	respPeers string
	respInbox string
	// 审批接口（/approval/ask）的响应正文，留空回「没拿到决定」。
	respApproval string
}

func (c *captured) peersResp() string {
	if c.respPeers != "" {
		return c.respPeers
	}
	return `{"peers":{"ubuntu:b":"busy","mac:a":"idle","ubuntu:c":"idle"}}`
}

func (c *captured) inboxResp() string {
	if c.respInbox != "" {
		return c.respInbox
	}
	return `{"messages":[],"unanswered":[]}`
}

// approvalResp 是 /approval/ask 的响应正文：没设就回「没拿到决定」（钩子据此退回终端确认）。
func (c *captured) approvalResp() string {
	if c.respApproval != "" {
		return c.respApproval
	}
	return `{"decided":false}`
}

type capturedReq struct {
	method string
	path   string
	auth   string
	xxauth string // X-XXYI-Auth 请求头（客户端证书签名）
	query  string
	body   sendBody
	raw    []byte // /msg/send 的原始正文，用来断言字段有没有出现在 JSON 里
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

// queries 按顺序返回收到的请求的查询串。
func (c *captured) queries() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.reqs))
	for _, r := range c.reqs {
		out = append(out, r.path+"?"+r.query)
	}
	return out
}

// handler 统一处理 /msg/send（reply 也走它）、/msg/inbox、/msg/who、/msg。
func (c *captured) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rec := capturedReq{
			method: r.Method, path: r.URL.Path,
			auth:   r.Header.Get("Authorization"),
			xxauth: r.Header.Get("X-XXYI-Auth"),
			query:  r.URL.RawQuery,
		}
		if r.Method == http.MethodPost && (r.URL.Path == "/msg/send" || r.URL.Path == "/msg/recall" || r.URL.Path == "/approval/ask") {
			b, _ := io.ReadAll(r.Body)
			rec.raw = b
			if r.URL.Path == "/msg/send" {
				_ = json.Unmarshal(b, &rec.body)
			}
		}
		c.add(rec)
		if c.respErr != "" {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, `{"error":%q}`, c.respErr)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/msg/send":
			if c.respSendErr != "" {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprintf(w, `{"error":%q}`, c.respSendErr)
				return
			}
			id := c.respID
			if id == "" {
				id = "ab12"
			}
			fmt.Fprintf(w, `{"id":%q}`, id)
		case "/msg/recall":
			fmt.Fprint(w, `{"ok":true}`)
		case "/msg/inbox":
			fmt.Fprint(w, c.inboxResp())
		case "/msg/who", "/msg":
			fmt.Fprint(w, c.peersResp())
		case "/approval/ask":
			fmt.Fprint(w, c.approvalResp())
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":"没有这个路径"}`)
		}
	}
}

// writeTestBundle 给一侧造一份合法证书包（CA 现签，perm 按侧默认）。
func writeTestBundle(t *testing.T, ca *xxyica.CA, path, side string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := ca.Issue(side+"-client", pub, xxyica.DefaultPerm(side), 90*24*time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := xxyica.WriteClientBundle(path, cert, priv, ca.Public()); err != nil {
		t.Fatal(err)
	}
}

// setPaths 把配置/证书/暂存路径指到临时目录并保证还原，两侧的证书包都预先装好，返回 CA。
// 不想装证书的测试拿返回值之后把 clientBundlePath* 指到别处即可。
func setPaths(t *testing.T, ep, cfg string) *xxyica.CA {
	t.Helper()
	oldEP, oldCfg := endpointPath, xxyiConfigPath
	oldBU, oldBM := clientBundlePathUbuntu, clientBundlePathMac
	oldSU, oldSM := spoolDirUbuntu, spoolDirMac
	t.Cleanup(func() {
		endpointPath, xxyiConfigPath = oldEP, oldCfg
		clientBundlePathUbuntu, clientBundlePathMac = oldBU, oldBM
		spoolDirUbuntu, spoolDirMac = oldSU, oldSM
	})
	endpointPath, xxyiConfigPath = ep, cfg
	dir := filepath.Dir(ep)
	clientBundlePathUbuntu = filepath.Join(dir, "ubuntu-client.json")
	clientBundlePathMac = filepath.Join(dir, "mac-client.json")
	spoolDirUbuntu = filepath.Join(dir, "spool-ubuntu")
	spoolDirMac = filepath.Join(dir, "spool-mac")
	ca, err := xxyica.NewCA()
	if err != nil {
		t.Fatal(err)
	}
	writeTestBundle(t, ca, clientBundlePathUbuntu, "ubuntu")
	writeTestBundle(t, ca, clientBundlePathMac, "mac")
	return ca
}

// fakeMsg 是一条假收件箱消息；Signed 为真时按签名域带上 CA 签名。
type fakeMsg struct {
	ID      string
	From    string
	To      string
	Kind    string
	State   string
	Text    string
	ReplyTo string
	Created time.Time
	Signed  bool
}

// fakeInboxResp 渲染 GET /msg/inbox 的响应 JSON（messages + unanswered）。
func fakeInboxResp(t *testing.T, ca *xxyica.CA, msgs, unanswered []fakeMsg) string {
	t.Helper()
	render := func(list []fakeMsg) []map[string]any {
		items := make([]map[string]any, 0, len(list))
		for _, m := range list {
			created := m.Created
			if created.IsZero() {
				created = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
			}
			item := map[string]any{
				"id": m.ID, "from": m.From, "to": m.To, "kind": m.Kind,
				"state": m.State, "text": m.Text, "reply_to": m.ReplyTo, "created": created,
			}
			if m.Signed {
				if ca == nil {
					t.Fatalf("消息 %s 要签名但没 CA", m.ID)
				}
				sig := ca.Sign(xxyica.MessageSigningBytes(xxyica.MsgFields{
					ID: m.ID, From: m.From, To: m.To, Kind: m.Kind, ReplyTo: m.ReplyTo,
					Text: m.Text, Created: created,
				}))
				item["sig"] = base64.StdEncoding.EncodeToString(sig)
			}
			items = append(items, item)
		}
		return items
	}
	raw, err := json.Marshal(map[string]any{"messages": render(msgs), "unanswered": render(unanswered)})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// listenFreeAddr 要一个没人占的本地端口（服务端断开后还能在原地址重开，模拟通道恢复）。
func listenFreeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// startServerAt 在固定地址起假服务（重复 Close 无害）。
func startServerAt(t *testing.T, addr string, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("监听 %s 失败：%v", addr, err)
	}
	srv := httptest.NewUnstartedServer(h)
	srv.Listener = ln
	srv.Start()
	t.Cleanup(func() { srv.Close() })
	return srv
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

// useEndpoint 写一个指向假服务的 endpoint.json（Ubuntu 一侧），返回装好证书包的 CA。
func useEndpoint(t *testing.T, srvURL string) *xxyica.CA {
	t.Helper()
	dir := t.TempDir()
	ep := filepath.Join(dir, "endpoint.json")
	if err := os.WriteFile(ep, []byte(fmt.Sprintf(`{"addr":%q,"token":"tk"}`, authority(srvURL))), 0o600); err != nil {
		t.Fatal(err)
	}
	return setPaths(t, ep, filepath.Join(dir, "config.json"))
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
	t.Chdir(dir) // 附件必须在当前工作目录下

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
	if !clientIDPat.MatchString(got.body.ClientID) {
		t.Errorf("client_id %q，该是 16 字节随机 hex", got.body.ClientID)
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

	// 连不上：不再直接报错，本地暂存并退出 0（补发见 spool_test.go）。
	out := &bytes.Buffer{}
	if code := run([]string{"send", "mac:boss", "hi"}, strings.NewReader(""), out, errOut); code != exitOK {
		t.Fatalf("退出码 %d，stderr %q", code, errOut.String())
	}
	if out.String() != "已暂存，通道恢复后自动补发（暂存 1 条）\n" {
		t.Errorf("stdout %q", out.String())
	}
	if files := spoolList(spoolDirUbuntu); len(files) != 1 {
		t.Errorf("暂存 %d 条", len(files))
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

	// 只有 TMUX：用注入的会话名；前缀按选侧规则，这里让 HOME 里有 endpoint.json 判成 ubuntu 侧
	t.Setenv("XXYI_ADDR", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".xxyi-support"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".xxyi-support", "endpoint.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"send", "mac:boss", "hi"}, strings.NewReader(""), io.Discard, io.Discard); code != exitOK {
		t.Fatalf("退出码 %d", code)
	}
	if got := cap.last(t); got.body.From != "ubuntu:sess1" {
		t.Errorf("from %q", got.body.From)
	}
	if !tmuxCalled {
		t.Error("TMUX 设了应该取会话名")
	}

	// 都没有：send 退出 2
	// 第三步兜底同样只吃注入（绝不真执行 tmux）：链上没有 pane 命中 → 还是"不知道自己的地址"。
	t.Setenv("TMUX", "")
	tmuxCalled = false
	oldPP, oldLP := parentPID, listPanes
	t.Cleanup(func() { parentPID, listPanes = oldPP, oldLP })
	parentPID = func(int) (int, error) { return 1, nil } // 一路到 init，链上谁也不是 pane
	listPanes = func() (string, error) { return "55 other", nil }
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
}

// "--" 之后的文字即使本身以 "--" 开头也照原文发出。
func TestSendDoubleDashLiteralText(t *testing.T) {
	var cap captured
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()
	useEndpoint(t, srv.URL)
	t.Setenv("XXYI_ADDR", "ubuntu:a")

	out := &bytes.Buffer{}
	if code := run([]string{"send", "mac:x", "--", "--help me"}, strings.NewReader(""), out, io.Discard); code != exitOK {
		t.Fatalf("退出码 %d，stdout %q", code, out.String())
	}
	if got := cap.last(t); got.body.Text != "--help me" {
		t.Errorf("text = %q，want %q", got.body.Text, "--help me")
	}
}

// tmux 会话名带中文/空格：send 当场退出 2 并提示改名、不发请求。
func TestSelfAddrIllegalSessionName(t *testing.T) {
	var cap captured
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()
	useEndpoint(t, srv.URL)
	t.Setenv("XXYI_ADDR", "")
	t.Setenv("TMUX", "1")
	oldTmux := tmuxSession
	t.Cleanup(func() { tmuxSession = oldTmux })
	tmuxSession = func() (string, error) { return "中文 会话", nil }

	errOut := &bytes.Buffer{}
	if code := run([]string{"send", "mac:boss", "hi"}, strings.NewReader(""), io.Discard, errOut); code != exitErr {
		t.Fatalf("会话名不合法时 send 退出码 %d，stderr %q", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "tmux rename-session") {
		t.Errorf("stderr 要提示 tmux rename-session：%q", errOut.String())
	}
	if n := cap.count(); n != 0 {
		t.Errorf("地址不合法时不该发请求，已发 %d", n)
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
	t.Chdir(dir) // 附件必须在当前工作目录下，才能走到"超限"这一步
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

func TestSendKindAndAskUser(t *testing.T) {
	var cap captured
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()
	useEndpoint(t, srv.URL)
	t.Setenv("XXYI_ADDR", "ubuntu:cw")

	out := &bytes.Buffer{}
	if code := run([]string{"send", "mac:boss", "帮我改", "--kind", "request", "--ask-user"}, strings.NewReader(""), out, io.Discard); code != exitOK {
		t.Fatalf("退出码 %d，stdout %q", code, out.String())
	}
	got := cap.last(t)
	if got.body.Kind != "request" || !got.body.AskUser {
		t.Errorf("正文 %+v", got.body)
	}
	if got.body.To != "mac:boss" || got.body.ReplyTo != "" {
		t.Errorf("正文 %+v", got.body)
	}
	if !strings.Contains(string(got.raw), `"ask_user":true`) {
		t.Errorf("原始正文 %s", got.raw)
	}

	// 不带 --kind 默认 notice
	out.Reset()
	if code := run([]string{"send", "mac:boss", "只是说一声"}, strings.NewReader(""), out, io.Discard); code != exitOK {
		t.Fatalf("退出码 %d", code)
	}
	got = cap.last(t)
	if got.body.Kind != "notice" {
		t.Errorf("默认 kind %q", got.body.Kind)
	}
	if got.body.AskUser {
		t.Error("不带 --ask-user 不该是 true")
	}

	// kind=code 缺 repo/branch：本地就拦下，不发请求
	n := cap.count()
	errOut := &bytes.Buffer{}
	if code := run([]string{"send", "mac:boss", "代码好了", "--kind", "code"}, strings.NewReader(""), io.Discard, errOut); code != exitErr {
		t.Fatalf("退出码 %d", code)
	}
	if !strings.Contains(errOut.String(), "交代码（--kind code）必须带 --repo 和 --branch") {
		t.Errorf("stderr %q", errOut.String())
	}
	if cap.count() != n {
		t.Errorf("本地拦下后不该发请求，已发 %d", cap.count()-n)
	}

	// kind=code 带齐 repo/branch 能发
	out.Reset()
	if code := run([]string{"send", "mac:boss", "代码好了", "--kind", "code", "--repo", "base", "--branch", "feat"}, strings.NewReader(""), out, io.Discard); code != exitOK {
		t.Fatalf("退出码 %d，stdout %q", code, out.String())
	}
	got = cap.last(t)
	if got.body.Kind != "code" || got.body.Repo != "base" || got.body.Branch != "feat" {
		t.Errorf("正文 %+v", got.body)
	}

	// reply 不许走 send
	errOut.Reset()
	if code := run([]string{"send", "mac:boss", "hi", "--kind", "reply"}, strings.NewReader(""), io.Discard, errOut); code != exitErr {
		t.Fatalf("退出码 %d", code)
	}
	if !strings.Contains(errOut.String(), "xxyi-msg reply") {
		t.Errorf("stderr %q", errOut.String())
	}

	// --kind 值不认识
	errOut.Reset()
	if code := run([]string{"send", "mac:boss", "hi", "--kind", "urgent"}, strings.NewReader(""), io.Discard, errOut); code != exitErr {
		t.Fatalf("退出码 %d", code)
	}
	if !strings.Contains(errOut.String(), "--kind") {
		t.Errorf("stderr %q", errOut.String())
	}
}

func TestReplyCommand(t *testing.T) {
	var cap captured
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()
	useEndpoint(t, srv.URL)
	t.Setenv("XXYI_ADDR", "ubuntu:cw")

	out := &bytes.Buffer{}
	if code := run([]string{"reply", "ab12", "已改好，测试跑过了"}, strings.NewReader(""), out, io.Discard); code != exitOK {
		t.Fatalf("退出码 %d，stdout %q", code, out.String())
	}
	if out.String() != "已排队 ab12\n" {
		t.Errorf("输出 %q", out.String())
	}
	got := cap.last(t)
	if got.path != "/msg/send" || got.method != http.MethodPost {
		t.Errorf("%s %s", got.method, got.path)
	}
	if got.body.Kind != "reply" || got.body.ReplyTo != "ab12" {
		t.Errorf("正文 %+v", got.body)
	}
	if got.body.To != "" || !strings.Contains(string(got.raw), `"to":""`) {
		t.Errorf("回复不该带收件人，正文 %s", got.raw)
	}
	if got.body.From != "ubuntu:cw" || got.body.Text != "已改好，测试跑过了" {
		t.Errorf("正文 %+v", got.body)
	}

	// 标准输入 + 附件
	dir := t.TempDir()
	file := filepath.Join(dir, "patch.txt")
	if err := os.WriteFile(file, []byte("附件"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir) // 附件必须在当前工作目录下
	out.Reset()
	if code := run([]string{"reply", "ab12", "-", "--file", file}, strings.NewReader("管道正文\n"), out, io.Discard); code != exitOK {
		t.Fatalf("退出码 %d", code)
	}
	got = cap.last(t)
	if got.body.Text != "管道正文\n" || got.body.FileName != "patch.txt" || string(got.body.File) != "附件" {
		t.Errorf("正文 %+v", got.body)
	}
	if got.body.Kind != "reply" || got.body.ReplyTo != "ab12" {
		t.Errorf("正文 %+v", got.body)
	}

	// 缺 id / 空文字 / 未知选项
	for _, args := range [][]string{
		{"reply"},
		{"reply", "ab12"},
		{"reply", "ab12", "hi", "--nope", "x"},
	} {
		errOut := &bytes.Buffer{}
		if code := run(args, strings.NewReader(""), io.Discard, errOut); code != exitErr {
			t.Errorf("%v 退出码 %d", args, code)
		}
		if errOut.Len() == 0 {
			t.Errorf("%v 没有报错", args)
		}
	}
}

func TestInboxCommand(t *testing.T) {
	var cap captured
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()
	ca := useEndpoint(t, srv.URL)
	cap.respInbox = fakeInboxResp(t, ca,
		[]fakeMsg{
			{ID: "m2", From: "mac:boss", To: "ubuntu:cw", Text: "收到\n别的", State: "queued", Kind: "reply", ReplyTo: "m1", Signed: true},
			{ID: "m1", From: "ubuntu:cw", To: "mac:boss", Text: "帮我改\n细节", State: "delivered", Kind: "request", Signed: true},
		},
		[]fakeMsg{
			{ID: "m1", From: "ubuntu:cw", To: "mac:boss", Text: "帮我改\n细节", State: "delivered", Kind: "request", Signed: true},
		})
	t.Setenv("XXYI_ADDR", "ubuntu:cw")

	out := &bytes.Buffer{}
	if code := run([]string{"inbox"}, nil, out, io.Discard); code != exitOK {
		t.Fatalf("退出码 %d，stderr %q", code, out.String())
	}
	want := "✓ 未回复 #m1 来自 ubuntu:cw：帮我改\n" +
		"✓ #m2 reply mac:boss → ubuntu:cw queued：收到\n" +
		"✓ #m1 request ubuntu:cw → mac:boss delivered：帮我改\n"
	if out.String() != want {
		t.Errorf("inbox 输出 %q，期望 %q", out.String(), want)
	}
	got := cap.last(t)
	if got.path != "/msg/inbox" || !strings.Contains(got.query, "addr=ubuntu%3Acw") || !strings.Contains(got.query, "n=20") {
		t.Errorf("inbox 请求 %s?%s", got.path, got.query)
	}

	// 指定条数
	if code := run([]string{"inbox", "3"}, nil, io.Discard, io.Discard); code != exitOK {
		t.Fatalf("退出码 %d", code)
	}
	if got := cap.last(t); !strings.Contains(got.query, "n=3") {
		t.Errorf("查询 %s", got.query)
	}

	// 条数不合法：本地拦下
	for _, bad := range []string{"0", "101", "abc"} {
		errOut := &bytes.Buffer{}
		if code := run([]string{"inbox", bad}, nil, io.Discard, errOut); code != exitErr {
			t.Errorf("inbox %s 退出码 %d", bad, code)
		}
		if errOut.Len() == 0 {
			t.Errorf("inbox %s 没有报错", bad)
		}
	}
}

func TestInboxEmptyAndWhoGone(t *testing.T) {
	var cap captured
	cap.respInbox = `{"messages":[],"unanswered":[]}`
	cap.respPeers = `{"peers":{"ubuntu:b":"busy","mac:a":"gone","mac:c":"idle"}}`
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()
	useEndpoint(t, srv.URL)
	t.Setenv("XXYI_ADDR", "ubuntu:cw")

	out := &bytes.Buffer{}
	if code := run([]string{"inbox"}, nil, out, io.Discard); code != exitOK {
		t.Fatalf("退出码 %d", code)
	}
	if out.String() != "收件箱是空的\n" {
		t.Errorf("空收件箱输出 %q", out.String())
	}

	out.Reset()
	if code := run([]string{"who"}, nil, out, io.Discard); code != exitOK {
		t.Fatalf("退出码 %d", code)
	}
	// gone 原样打印，不再把非 busy 的补成 idle
	if want := "mac:a gone\nmac:c idle\nubuntu:b busy\n"; out.String() != want {
		t.Errorf("who 输出 %q，期望 %q", out.String(), want)
	}
}

// 附件位置限制：必须写绝对路径（相对路径当场拒收），解开符号链接后只能是
// 当前工作目录或 ~/xxyi-work 之下、必须普通文件；超限的报错文案不变。
func TestAttachPathRestrictions(t *testing.T) {
	var cap captured
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()
	useEndpoint(t, srv.URL)
	t.Setenv("XXYI_ADDR", "ubuntu:cw")

	proj := t.TempDir()
	t.Chdir(proj)
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "out.txt")
	if err := os.WriteFile(outsideFile, []byte("外面"), 0o600); err != nil {
		t.Fatal(err)
	}

	n := cap.count()
	errOut := &bytes.Buffer{}
	if code := run([]string{"send", "mac:boss", "hi", "--file", outsideFile}, strings.NewReader(""), io.Discard, errOut); code != exitErr {
		t.Fatalf("工作目录外附件退出码 %d", code)
	}
	if !strings.Contains(errOut.String(), "附件只能是当前项目目录或 ~/xxyi-work 下的文件") {
		t.Errorf("stderr %q", errOut.String())
	}
	if cap.count() != n {
		t.Errorf("附件被拦下不该发请求，已发 %d", cap.count()-n)
	}

	// 符号链接指向目录外：解开后拦下
	link := filepath.Join(proj, "link.txt")
	if err := os.Symlink(outsideFile, link); err != nil {
		t.Fatal(err)
	}
	errOut.Reset()
	if code := run([]string{"send", "mac:boss", "hi", "--file", link}, strings.NewReader(""), io.Discard, errOut); code != exitErr {
		t.Fatalf("指向目录外的符号链接退出码 %d", code)
	}
	if !strings.Contains(errOut.String(), "附件只能是当前项目目录或 ~/xxyi-work 下的文件") {
		t.Errorf("符号链接 stderr %q", errOut.String())
	}

	// /dev/null：设备文件。工作目录设在 / 让位置检查先放行，才能验到"必须普通文件"这一步
	t.Chdir("/")
	errOut.Reset()
	if code := run([]string{"send", "mac:boss", "hi", "--file", "/dev/null"}, strings.NewReader(""), io.Discard, errOut); code != exitErr {
		t.Fatalf("/dev/null 退出码 %d", code)
	}
	if !strings.Contains(errOut.String(), "附件必须是普通文件") {
		t.Errorf("/dev/null stderr %q", errOut.String())
	}

	// 相对路径：当场拒收并提示写绝对路径（相对跟着当时的当前目录走，不能猜）
	t.Chdir(proj)
	okFile := filepath.Join(proj, "ok.txt")
	if err := os.WriteFile(okFile, []byte("好的"), 0o600); err != nil {
		t.Fatal(err)
	}
	n2 := cap.count()
	errOut.Reset()
	if code := run([]string{"send", "mac:boss", "hi", "--file", "ok.txt"}, strings.NewReader(""), io.Discard, errOut); code != exitErr {
		t.Fatalf("相对路径附件退出码 %d，stderr %q", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "附件路径要写绝对路径") {
		t.Errorf("相对路径 stderr %q", errOut.String())
	}
	if cap.count() != n2 {
		t.Errorf("相对路径被拦下不该发请求，已发 %d", cap.count()-n2)
	}

	// 工作目录下的普通文件（绝对路径）通过
	out := &bytes.Buffer{}
	if code := run([]string{"send", "mac:boss", "hi", "--file", okFile}, strings.NewReader(""), out, io.Discard); code != exitOK {
		t.Fatalf("正常附件退出码 %d，输出 %q", code, out.String())
	}
	got := cap.last(t)
	if got.body.FileName != "ok.txt" || string(got.body.File) != "好的" {
		t.Errorf("附件 %q %q", got.body.FileName, got.body.File)
	}
}

func TestPathUnderRejectsHidden(t *testing.T) {
	cases := []struct {
		abs  string
		want bool
	}{
		{"/home/u", true},
		{"/home/u/proj/a.txt", true},
		{"/home/u/.ssh/id_ed25519", false},
		{"/home/u/proj/.env", false},
		{"/home/u/proj/.git/config", false},
		{"/home/other/a.txt", false},
		{"/home/u/..x/a.txt", false},
	}
	for _, c := range cases {
		if got := pathUnder("/home/u", c.abs); got != c.want {
			t.Errorf("pathUnder(/home/u, %s) = %v，应为 %v", c.abs, got, c.want)
		}
	}
}

func TestTmuxSessionArgsOrder(t *testing.T) {
	if got := strings.Join(tmuxSessionArgs("%3"), " "); got != "display-message -p -t %3 #S" {
		t.Errorf("参数 %q：-t 要在格式串前面", got)
	}
	if got := strings.Join(tmuxSessionArgs(""), " "); got != "display-message -p #S" {
		t.Errorf("参数 %q", got)
	}
}

func TestUbuntuSupportOffMessage(t *testing.T) {
	dir := t.TempDir()
	setPaths(t, filepath.Join(dir, "endpoint.json"), filepath.Join(dir, "config.json"))
	bundle := filepath.Join(dir, "client.json")
	if err := os.WriteFile(bundle, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := clientBundlePathUbuntu
	clientBundlePathUbuntu = bundle
	t.Cleanup(func() { clientBundlePathUbuntu = old })
	t.Setenv("XXYI_ADDR", "ubuntu:a")
	errOut := &bytes.Buffer{}
	if code := run([]string{"who"}, strings.NewReader(""), io.Discard, errOut); code != exitErr {
		t.Fatalf("退出码 %d", code)
	}
	if !strings.Contains(errOut.String(), "没有协助端点") || strings.Contains(errOut.String(), "8765") {
		t.Errorf("stderr %q", errOut.String())
	}
}

// 设了 TMUX、没有 XXYI_ADDR，选侧规则判成 mac 时地址前缀必须是 mac:（与 connect 共用同一规则）。
func TestSelfAddrTmuxMacSide(t *testing.T) {
	home := t.TempDir() // 空家目录：选侧规则判 mac
	t.Setenv("HOME", home)
	t.Setenv("XXYI_ADDR", "")
	t.Setenv("TMUX", "/tmp/tmux-1000/default,123,0")
	oldTmux := tmuxSession
	t.Cleanup(func() { tmuxSession = oldTmux })
	tmuxSession = func() (string, error) { return "sess1", nil }
	if ubuntuSide() {
		t.Fatal("前置条件：空家目录应判 mac 侧")
	}
	addr, err := selfAddr()
	if err != nil || addr != "mac:sess1" {
		t.Errorf("selfAddr = %q, %v，期望 mac:sess1", addr, err)
	}
}
