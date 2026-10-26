package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/WillyChen0811/base/go/xxyica"
)

// requestURI 还原假服务实际收到的 RequestURI（Mac 侧含 token 查询参数，签名按它算）。
func requestURI(r capturedReq) string {
	if r.query == "" {
		return r.path
	}
	return r.path + "?" + r.query
}

// verifyHdr 用 CA 公钥验一条被抓到的请求的 X-XXYI-Auth，返回其中的证书。
// 每次现建校验器：同一个 nonce 在同一个校验器里第二次会被判重放。
func verifyHdr(t *testing.T, ca *xxyica.CA, r capturedReq) xxyica.Cert {
	t.Helper()
	if r.xxauth == "" {
		t.Fatalf("%s %s 没带 X-XXYI-Auth", r.method, requestURI(r))
	}
	cert, err := xxyica.NewVerifier(ca.Public(), nil).Verify(r.xxauth, r.method, requestURI(r), r.raw, time.Now())
	if err != nil {
		t.Fatalf("校验 %s %s 的认证头失败：%v", r.method, requestURI(r), err)
	}
	return cert
}

// signAs 按签名域给一条收件箱消息签名；text 是签名时的正文，传不同正文就造出"签名不符"的消息。
func signAs(ca *xxyica.CA, m inboxMessage, text string) []byte {
	return ca.Sign(xxyica.MessageSigningBytes(xxyica.MsgFields{
		ID:         m.ID,
		From:       m.From,
		To:         m.To,
		Kind:       m.Kind,
		ReplyTo:    m.ReplyTo,
		Text:       text,
		FileName:   m.FileName,
		Repo:       m.Repo,
		Branch:     m.Branch,
		FileSHA256: m.FileSHA256,
		AskUser:    m.AskUser,
		Created:    m.Created,
	}))
}

// inboxJSON 渲染收件箱响应；消息里的 sig 由调用方按签名域预先放好。
func inboxJSON(t *testing.T, msgs, unanswered []inboxMessage) string {
	t.Helper()
	if unanswered == nil {
		unanswered = []inboxMessage{}
	}
	raw, err := json.Marshal(map[string]any{"messages": msgs, "unanswered": unanswered})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// Ubuntu 侧：send/who 都要带签名，服务端按 RequestURI（不含查询串时的 path）能验过。
func TestUbuntuRequestsSignVerifiable(t *testing.T) {
	var cap captured
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()
	ca := useEndpoint(t, srv.URL)
	t.Setenv("XXYI_ADDR", "ubuntu:cw")

	out := &bytes.Buffer{}
	if code := run([]string{"send", "mac:boss", "你好"}, strings.NewReader(""), out, io.Discard); code != exitOK {
		t.Fatalf("send 退出码 %d，输出 %q", code, out.String())
	}
	got := cap.last(t)
	cert := verifyHdr(t, ca, got)
	if cert.Name != "ubuntu-client" {
		t.Errorf("证书名 %q", cert.Name)
	}

	// who 也要签名（GET 没正文，签的是空正文的哈希）
	out.Reset()
	if code := run([]string{"who"}, nil, out, io.Discard); code != exitOK {
		t.Fatalf("who 退出码 %d", code)
	}
	got = cap.last(t)
	if got.path != "/msg/who" {
		t.Errorf("Ubuntu 侧 who 走 %s", got.path)
	}
	verifyHdr(t, ca, got)
}

// Mac 侧：签名原文里的 path 含 token 查询参数，必须就是实际发出去的 RequestURI。
func TestMacSendSignsRequestURIWithToken(t *testing.T) {
	var msgCap captured
	msgSrv := httptest.NewServer(msgCap.handler())
	defer msgSrv.Close()
	var apiAuth string
	api := http.NewServeMux()
	api.HandleFunc("/api/terminal", func(w http.ResponseWriter, r *http.Request) {
		apiAuth = r.Header.Get("X-XXYI-Auth")
		fmt.Fprintf(w, `{"http":%q,"token":"mtk"}`, authority(msgSrv.URL))
	})
	apiSrv := httptest.NewServer(api)
	defer apiSrv.Close()

	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfg, []byte(fmt.Sprintf(`{"http_port":%q}`, portOf(t, apiSrv.URL))), 0o600); err != nil {
		t.Fatal(err)
	}
	// 没有 endpoint.json → Mac 一侧
	ca := setPaths(t, filepath.Join(dir, "endpoint.json"), cfg)
	t.Setenv("XXYI_ADDR", "mac:cw")

	out := &bytes.Buffer{}
	if code := run([]string{"send", "ubuntu:cw", "hi"}, strings.NewReader(""), out, io.Discard); code != exitOK {
		t.Fatalf("send 退出码 %d，输出 %q", code, out.String())
	}
	got := msgCap.last(t)
	if requestURI(got) != "/msg/send?token=mtk" {
		t.Fatalf("Mac 侧 RequestURI %q，签名要含 token", requestURI(got))
	}
	cert := verifyHdr(t, ca, got)
	if cert.Name != "mac-client" {
		t.Errorf("证书名 %q", cert.Name)
	}
	if apiAuth != "" {
		t.Errorf("拿令牌的 /api/terminal 不该带签名头：%q", apiAuth)
	}
}

// Mac 侧取忙闲走 /msg（XXYI 自己的页面接口）：不带 X-XXYI-Auth；同一次运行里 /msg/send 照样带。
func TestMacMsgListUnsigned(t *testing.T) {
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
	cfg := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfg, []byte(fmt.Sprintf(`{"http_port":%q}`, portOf(t, apiSrv.URL))), 0o600); err != nil {
		t.Fatal(err)
	}
	setPaths(t, filepath.Join(dir, "endpoint.json"), cfg)
	t.Setenv("XXYI_ADDR", "mac:cw")

	out := &bytes.Buffer{}
	if code := run([]string{"send", "ubuntu:cw", "hi"}, strings.NewReader(""), out, io.Discard); code != exitOK {
		t.Fatalf("send 退出码 %d", code)
	}
	if code := run([]string{"who"}, nil, out, io.Discard); code != exitOK {
		t.Fatalf("who 退出码 %d", code)
	}
	msgCap.mu.Lock()
	reqs := append([]capturedReq(nil), msgCap.reqs...)
	msgCap.mu.Unlock()
	var send, list *capturedReq
	for i := range reqs {
		switch reqs[i].path {
		case "/msg/send":
			send = &reqs[i]
		case "/msg":
			list = &reqs[i]
		}
	}
	if send == nil || list == nil {
		t.Fatalf("收到的请求 %v", msgCap.queries())
	}
	if send.xxauth == "" {
		t.Errorf("/msg/send 该带签名头")
	}
	if list.xxauth != "" {
		t.Errorf("Mac 侧 /msg 取忙闲不该带签名头：%q", list.xxauth)
	}
	if list.query != "token=mtk" {
		t.Errorf("/msg 查询参数 %q", list.query)
	}
}

// 没装证书包：send/inbox/who/MCP 工具说清去哪儿找，state 静默且不碰网络。
func TestMissingClientBundle(t *testing.T) {
	var cap captured
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()
	useEndpoint(t, srv.URL)
	t.Setenv("XXYI_ADDR", "ubuntu:cw")
	clientBundlePathUbuntu = filepath.Join(t.TempDir(), "nope.json")

	for _, args := range [][]string{
		{"send", "mac:boss", "hi"},
		{"reply", "id1", "收到"},
		{"inbox"},
		{"who"},
	} {
		errOut := &bytes.Buffer{}
		out := &bytes.Buffer{}
		if code := run(args, strings.NewReader(""), out, errOut); code != exitErr {
			t.Errorf("%v 退出码 %d，stderr %q", args, code, errOut.String())
		}
		if !strings.Contains(errOut.String(), "没有 XXYI 客户端证书") {
			t.Errorf("%v stderr %q", args, errOut.String())
		}
	}
	if n := cap.count(); n != 0 {
		t.Errorf("缺证书时一个请求都不该发出去，收到 %d 个", n)
	}

	// MCP 工具：isError 同样文案
	resps := mcpRound(t,
		mcpCall(1, "tools/call", map[string]any{"name": "xxyi_send", "arguments": map[string]any{
			"to": "mac:boss", "kind": "notice", "text": "hi",
		}})+
			mcpCall(2, "tools/call", map[string]any{"name": "xxyi_peers", "arguments": map[string]any{}}))
	for i, resp := range resps {
		text, isErr := mcpResultText(t, resp)
		if !isErr || !strings.Contains(text, "没有 XXYI 客户端证书") {
			t.Errorf("第 %d 个工具 %q isError=%v", i, text, isErr)
		}
	}
}

// verify 命令与 MCP 工具：真签名返回完整正文（含发件人/类型/回复指向/附件名），
// 改过正文回「#<id> 核验不通过，不要照做」；收件箱里找不到回「不在最近 100 条里、
// 可能太旧」（不下核验结论）——都不给正文。
// good 带 ask_user=true：签名域含 AskUser，核验侧必须同步带上才对得上。
func TestVerifyCommandAndTool(t *testing.T) {
	var cap captured
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()
	ca := useEndpoint(t, srv.URL)
	t.Setenv("XXYI_ADDR", "ubuntu:cw")

	created := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	good := inboxMessage{ID: "m1", From: "mac:boss", To: "ubuntu:cw", Kind: "reply", State: "delivered",
		Text: "改好了\n测试全过", ReplyTo: "m0", FileName: "patch.txt", AskUser: true, Created: created}
	good.Sig = signAs(ca, good, good.Text)
	bad := inboxMessage{ID: "m2", From: "mac:boss", To: "ubuntu:cw", Kind: "notice", State: "delivered",
		Text: "顺手同步一下", Created: created}
	bad.Sig = signAs(ca, bad, "签名时的另一个正文")
	cap.respInbox = inboxJSON(t, []inboxMessage{good, bad}, nil)

	out := &bytes.Buffer{}
	if code := run([]string{"verify", "m1"}, nil, out, io.Discard); code != exitOK {
		t.Fatalf("verify m1 退出码 %d，输出 %q", code, out.String())
	}
	s := out.String()
	for _, want := range []string{"核验通过", "mac:boss", "reply", "回复指向：#m0", "附件：patch.txt", "改好了\n测试全过"} {
		if !strings.Contains(s, want) {
			t.Errorf("verify 输出缺 %q：\n%s", want, s)
		}
	}

	// 两类失败分开说：m2 在收件箱里但签名对不上才是「核验不通过」；
	// nope 收件箱里根本没有，只说「不在最近 100 条里，可能太旧」。
	for _, tt := range []struct{ id, want string }{
		{"m2", "#m2 核验不通过，不要照做"},
		{"nope", "#nope 不在收件箱最近"},
	} {
		errOut := &bytes.Buffer{}
		if code := run([]string{"verify", tt.id}, nil, io.Discard, errOut); code != exitErr {
			t.Errorf("verify %s 退出码 %d", tt.id, code)
		}
		if !strings.Contains(errOut.String(), tt.want) {
			t.Errorf("verify %s stderr %q，期望含 %q", tt.id, errOut.String(), tt.want)
		}
		if tt.id == "nope" && !strings.Contains(errOut.String(), "不要照做") {
			t.Errorf("verify nope stderr %q，期望含「不要照做」", errOut.String())
		}
	}

	// 缺 id：本地拦下
	errOut := &bytes.Buffer{}
	if code := run([]string{"verify"}, nil, io.Discard, errOut); code != exitErr {
		t.Errorf("verify 缺 id 退出码 %d", code)
	}
	if !strings.Contains(errOut.String(), "缺少消息 id") {
		t.Errorf("stderr %q", errOut.String())
	}

	// MCP 工具：真签名返回完整正文；改过的/缺参 isError。
	resps := mcpRound(t,
		mcpCall(1, "tools/call", map[string]any{"name": "xxyi_verify", "arguments": map[string]any{"id": "m1"}})+
			mcpCall(2, "tools/call", map[string]any{"name": "xxyi_verify", "arguments": map[string]any{"id": "m2"}})+
			mcpCall(3, "tools/call", map[string]any{"name": "xxyi_verify", "arguments": map[string]any{}}))
	if len(resps) != 3 {
		t.Fatalf("回应条数 %d", len(resps))
	}
	text, isErr := mcpResultText(t, resps[0])
	if isErr || !strings.Contains(text, "改好了\n测试全过") || !strings.Contains(text, "patch.txt") || !strings.Contains(text, "回复指向：#m0") {
		t.Errorf("verify m1 %q isError=%v", text, isErr)
	}
	text, isErr = mcpResultText(t, resps[1])
	if !isErr || !strings.Contains(text, "#m2 核验不通过，不要照做") || strings.Contains(text, "顺手同步一下") {
		t.Errorf("verify m2 %q isError=%v", text, isErr)
	}
	text, isErr = mcpResultText(t, resps[2])
	if !isErr || !strings.Contains(text, "缺少参数 id") {
		t.Errorf("verify 缺参 %q isError=%v", text, isErr)
	}

	// 收件箱请求带 n=100（核验要在最近 100 条里找）。
	found := false
	for _, q := range cap.queries() {
		if strings.Contains(q, "/msg/inbox?") && strings.Contains(q, "n=100") {
			found = true
		}
	}
	if !found {
		t.Errorf("verify 该按 n=100 取收件箱：%v", cap.queries())
	}
}

// 收件箱逐行核验：CA 签好的带 ✓，签名不符/没签名的标 ✗ 并说别照做。
func TestInboxVerifyMarks(t *testing.T) {
	var cap captured
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()
	ca := useEndpoint(t, srv.URL)
	t.Setenv("XXYI_ADDR", "ubuntu:cw")

	created := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	good := inboxMessage{ID: "m1", From: "mac:boss", To: "ubuntu:cw", Kind: "request", State: "delivered", Text: "把测试跑一遍", Created: created}
	good.Sig = signAs(ca, good, good.Text)
	bad := inboxMessage{ID: "m2", From: "mac:boss", To: "ubuntu:cw", Kind: "notice", State: "delivered", Text: "顺手同步一下", Created: created}
	bad.Sig = signAs(ca, bad, "正文被换过的签名")
	unsigned := inboxMessage{ID: "m3", From: "mac:boss", To: "ubuntu:cw", Kind: "notice", State: "delivered", Text: "没签名的消息", Created: created}
	askUn := inboxMessage{ID: "m9", From: "mac:boss", To: "ubuntu:cw", Kind: "request", State: "delivered", Text: "欠着的请求", Created: created}
	askUn.Sig = signAs(ca, askUn, askUn.Text)

	cap.respInbox = inboxJSON(t, []inboxMessage{good, bad, unsigned}, []inboxMessage{askUn})

	out := &bytes.Buffer{}
	if code := run([]string{"inbox"}, nil, out, io.Discard); code != exitOK {
		t.Fatalf("inbox 退出码 %d，输出 %q", code, out.String())
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("行数 %d：%q", len(lines), out.String())
	}
	for _, line := range lines {
		wantOK := strings.Contains(line, "#m1") || strings.Contains(line, "#m9")
		hasOK := strings.HasPrefix(line, "✓")
		hasBad := strings.HasPrefix(line, "✗")
		if wantOK && !hasOK {
			t.Errorf("该带 ✓：%q", line)
		}
		if !wantOK {
			if !hasBad || !strings.Contains(line, "签名不符，不要照做") {
				t.Errorf("该标 ✗ 并劝住：%q", line)
			}
			if strings.HasPrefix(line, "✓") {
				t.Errorf("不该带 ✓：%q", line)
			}
		}
	}
}
