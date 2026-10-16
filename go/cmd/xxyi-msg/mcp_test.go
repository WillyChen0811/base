package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mcpCall 是一条请求的线格式。
func mcpCall(id int, method string, params any) string {
	raw, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	})
	if err != nil {
		panic(err)
	}
	return string(raw) + "\n"
}

// mcpRound 把整段输入喂给 serveMCP，按行收回应答（通知不该占一行）。
func mcpRound(t *testing.T, input string) []map[string]any {
	t.Helper()
	out := &bytes.Buffer{}
	if err := serveMCP(strings.NewReader(input), out); err != nil {
		t.Fatalf("serveMCP 返回错误：%v", err)
	}
	var resps []map[string]any
	for _, line := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("回应不是合法 JSON：%q：%v", line, err)
		}
		resps = append(resps, m)
	}
	return resps
}

// mcpResultText 取 tools/call 回应里的文字与 isError。
func mcpResultText(t *testing.T, resp map[string]any) (string, bool) {
	t.Helper()
	if e, ok := resp["error"]; ok {
		t.Fatalf("工具失败不该用 JSON-RPC error，得到 %v", e)
	}
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("回应里没有 result：%v", resp)
	}
	isErr, _ := result["isError"].(bool)
	content, ok := result["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("result.content 不对：%v", result)
	}
	first, ok := content[0].(map[string]any)
	if !ok || first["type"] != "text" {
		t.Fatalf("content[0] 不是文字：%v", content[0])
	}
	text, _ := first["text"].(string)
	return text, isErr
}

func TestMCPEndpointAndInitialize(t *testing.T) {
	var ep captured
	srv := httptest.NewServer(ep.handler())
	defer srv.Close()
	useEndpoint(t, srv.URL)
	t.Setenv("XXYI_ADDR", "ubuntu:cw")

	resps := mcpRound(t,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n"+ // 通知：不该有回应
			mcpCall(1, "initialize", map[string]any{"protocolVersion": "2024-11-05"})+
			mcpCall(2, "initialize", map[string]any{})+
			mcpCall(3, "ping", nil))
	if len(resps) != 3 {
		t.Fatalf("通知占了回应，共 %d 条：%v", len(resps), resps)
	}
	init := resps[0]["result"].(map[string]any)
	if init["protocolVersion"] != "2024-11-05" {
		t.Errorf("协议版本没回显：%v", init["protocolVersion"])
	}
	if init["serverInfo"] == nil || init["capabilities"] == nil {
		t.Errorf("initialize 缺字段：%v", init)
	}
	si, _ := init["serverInfo"].(map[string]any)
	if si["name"] != "xxyi" || si["version"] != "1" {
		t.Errorf("serverInfo %v", si)
	}
	caps, _ := init["capabilities"].(map[string]any)
	if _, ok := caps["tools"]; !ok {
		t.Errorf("capabilities 里要有 tools：%v", caps)
	}
	if inst, _ := init["instructions"].(string); !strings.Contains(inst, "xxyi_reply") || !strings.Contains(inst, "ask_user") {
		t.Errorf("instructions %q", inst)
	}
	// 没给 protocolVersion 用缺省
	if got := resps[1]["result"].(map[string]any)["protocolVersion"]; got != mcpDefaultProtocolVersion {
		t.Errorf("缺省协议版本 %v", got)
	}
	// ping 返回空对象
	if got, ok := resps[2]["result"].(map[string]any); !ok || len(got) != 0 {
		t.Errorf("ping 回应 %v", resps[2])
	}
	// id 原样回显
	for i, want := range []float64{1, 2, 3} {
		if got, _ := resps[i]["id"].(float64); got != want {
			t.Errorf("第 %d 条 id %v", i, resps[i]["id"])
		}
	}
}

func TestMCPToolsList(t *testing.T) {
	resps := mcpRound(t, mcpCall(9, "tools/list", nil))
	if len(resps) != 1 {
		t.Fatalf("回应条数 %d", len(resps))
	}
	tools := resps[0]["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 14 { // 5 个消息工具 + 9 个 mac_*（见 tasks-macexec/CONTRACT.md）
		t.Fatalf("工具数 %d", len(tools))
	}
	names := make([]string, 0, len(tools))
	for _, raw := range tools {
		tool := raw.(map[string]any)
		name, _ := tool["name"].(string)
		names = append(names, name)
		if desc, _ := tool["description"].(string); desc == "" {
			t.Errorf("%s 没有 description", name)
		}
		schema, ok := tool["inputSchema"].(map[string]any)
		if !ok || schema["type"] != "object" {
			t.Errorf("%s 的 inputSchema 不对：%v", name, tool["inputSchema"])
			continue
		}
		if _, ok := schema["properties"].(map[string]any); !ok {
			t.Errorf("%s 的 properties 不对", name)
		}
		if req, ok := schema["required"].([]any); !ok {
			t.Errorf("%s 没有 required：%v", name, schema)
		} else if name == "xxyi_peers" && len(req) != 0 {
			t.Errorf("xxyi_peers 不该有必填参数：%v", req)
		}
	}
	want := []string{"xxyi_send", "xxyi_reply", "xxyi_inbox", "xxyi_verify", "xxyi_peers",
		"mac_git_status", "mac_read_file", "mac_logs", "mac_job",
		"mac_build", "mac_selftest", "mac_node_test", "mac_build_app", "mac_screenshot"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("工具名 %v", names)
	}
	// verify 必填 id
	vsch := tools[3].(map[string]any)["inputSchema"].(map[string]any)
	if r := fmt.Sprint(vsch["required"]); !strings.Contains(r, "id") {
		t.Errorf("xxyi_verify 必填 %v", r)
	}
	// send 的 kind 枚举不含 reply
	send := tools[0].(map[string]any)["inputSchema"].(map[string]any)["properties"].(map[string]any)["kind"].(map[string]any)
	enum := fmt.Sprint(send["enum"])
	if !strings.Contains(enum, "request") || !strings.Contains(enum, "notice") || !strings.Contains(enum, "code") || strings.Contains(enum, "reply") {
		t.Errorf("kind 枚举 %v", send["enum"])
	}
	if r := fmt.Sprint(tools[0].(map[string]any)["inputSchema"].(map[string]any)["required"]); !strings.Contains(r, "to") {
		t.Errorf("xxyi_send 必填 %v", r)
	}
}

func TestMCPCallSendReplyPeers(t *testing.T) {
	var cap captured
	cap.respID = "id42"
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()
	useEndpoint(t, srv.URL)
	t.Setenv("XXYI_ADDR", "ubuntu:cw")

	resps := mcpRound(t,
		mcpCall(1, "tools/call", map[string]any{"name": "xxyi_send", "arguments": map[string]any{
			"to": "mac:boss", "kind": "request", "text": "帮我把这个跑一遍", "ask_user": true,
		}})+
			mcpCall(2, "tools/call", map[string]any{"name": "xxyi_reply", "arguments": map[string]any{
				"id": "id42", "text": "已经跑完",
			}})+
			mcpCall(3, "tools/call", map[string]any{"name": "xxyi_peers", "arguments": map[string]any{}}))
	if len(resps) != 3 {
		t.Fatalf("回应条数 %d", len(resps))
	}

	text, isErr := mcpResultText(t, resps[0])
	if isErr || text != "已排队 #id42" {
		t.Fatalf("send 结果 %q isError=%v", text, isErr)
	}
	cap.mu.Lock()
	reqs := append([]capturedReq(nil), cap.reqs...)
	cap.mu.Unlock()
	if len(reqs) != 3 {
		t.Fatalf("假服务收到 %d 个请求", len(reqs))
	}
	got := reqs[0]
	if got.path != "/msg/send" || got.body.Kind != "request" || !got.body.AskUser || got.body.To != "mac:boss" || got.body.From != "ubuntu:cw" {
		t.Errorf("send 正文 %+v", got.body)
	}
	if !clientIDPat.MatchString(got.body.ClientID) {
		t.Errorf("send 的 client_id %q，该是 16 字节随机 hex", got.body.ClientID)
	}
	if got.auth != "Bearer tk" {
		t.Errorf("Ubuntu 侧该带令牌头：%q", got.auth)
	}

	text, isErr = mcpResultText(t, resps[1])
	if isErr || text != "已排队 #id42" {
		t.Fatalf("reply 结果 %q isError=%v", text, isErr)
	}
	got = reqs[1]
	if got.body.Kind != "reply" || got.body.ReplyTo != "id42" || got.body.To != "" {
		t.Errorf("reply 正文 %+v", got.body)
	}
	if !clientIDPat.MatchString(got.body.ClientID) || got.body.ClientID == reqs[0].body.ClientID {
		t.Errorf("reply 的 client_id %q，该是独立于 send 的随机 hex", got.body.ClientID)
	}

	text, isErr = mcpResultText(t, resps[2])
	if isErr {
		t.Fatalf("peers 失败：%q", text)
	}
	want := "mac:a idle\nubuntu:b busy\nubuntu:c idle"
	if text != want {
		t.Errorf("peers %q，期望 %q", text, want)
	}
}

func TestMCPCallInbox(t *testing.T) {
	var cap captured
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()
	ca := useEndpoint(t, srv.URL)
	cap.respInbox = fakeInboxResp(t, ca,
		[]fakeMsg{{ID: "m1", From: "mac:boss", To: "ubuntu:cw", Text: "在吗", State: "delivered", Kind: "notice", Signed: true}},
		[]fakeMsg{{ID: "m0", From: "mac:boss", To: "ubuntu:cw", Text: "推一下", State: "delivered", Kind: "request", Signed: true}})
	t.Setenv("XXYI_ADDR", "ubuntu:cw")

	resps := mcpRound(t,
		mcpCall(1, "tools/call", map[string]any{"name": "xxyi_inbox", "arguments": map[string]any{}})+
			mcpCall(2, "tools/call", map[string]any{"name": "xxyi_inbox", "arguments": map[string]any{"n": 5}}))
	if len(resps) != 2 {
		t.Fatalf("回应条数 %d", len(resps))
	}
	text, isErr := mcpResultText(t, resps[0])
	if isErr {
		t.Fatalf("inbox 失败：%q", text)
	}
	want := "✓ 未回复 #m0 来自 mac:boss：推一下\n✓ #m1 notice mac:boss → ubuntu:cw delivered：在吗"
	if text != want {
		t.Errorf("inbox 文字 %q，期望 %q", text, want)
	}
	// 默认 n=20，显式 n=5 要传下去
	qs := cap.queries()
	if !strings.Contains(qs[0], "n=20") || !strings.Contains(qs[1], "n=5") {
		t.Errorf("inbox 查询 %v", qs)
	}
}

func TestMCPToolErrors(t *testing.T) {
	var cap captured
	cap.respPeers = `{"peers":{}}`
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()
	useEndpoint(t, srv.URL)
	t.Setenv("XXYI_ADDR", "ubuntu:cw")

	cases := []struct {
		name   string
		args   map[string]any
		wantIn string
	}{
		{"xxyi_send", map[string]any{"kind": "notice", "text": "hi"}, "缺少参数 to"},
		{"xxyi_send", map[string]any{"to": "mac:boss", "text": "hi"}, "缺少参数 kind"},
		{"xxyi_send", map[string]any{"to": "mac:boss", "kind": "reply", "text": "hi"}, "kind"},
		{"xxyi_send", map[string]any{"to": "mac:boss", "kind": "code", "text": "代码"}, "必须带 --repo 和 --branch"},
		{"xxyi_send", map[string]any{"to": "mac:boss", "kind": "notice", "text": "   "}, "text 不能为空"},
		{"xxyi_reply", map[string]any{"text": "好了"}, "缺少参数 id"},
		{"xxyi_inbox", map[string]any{"n": 0}, "必须 1..100"},
		{"xxyi_verify", map[string]any{}, "缺少参数 id"},
		{"xxyi_nope", map[string]any{}, "没有这个工具"},
	}
	for _, c := range cases {
		resps := mcpRound(t, mcpCall(1, "tools/call", map[string]any{"name": c.name, "arguments": c.args}))
		if len(resps) != 1 {
			t.Fatalf("%v 回应条数 %d", c.name, len(resps))
		}
		text, isErr := mcpResultText(t, resps[0])
		if !isErr {
			t.Errorf("%v %v 该 isError，得到 %q", c.name, c.args, text)
		}
		if !strings.Contains(text, c.wantIn) {
			t.Errorf("%v %v 原因 %q，期望含 %q", c.name, c.args, text, c.wantIn)
		}
	}

	// 对端表为空时的说法
	resps := mcpRound(t, mcpCall(1, "tools/call", map[string]any{"name": "xxyi_peers"}))
	if text, isErr := mcpResultText(t, resps[0]); isErr || text != "还没有对端上报状态" {
		t.Errorf("空忙闲表 %q isError=%v", text, isErr)
	}
}

// 拿不到自己的地址：工具报错但服务不退出，后面的请求照常应答。
func TestMCPCallWithoutSelfAddr(t *testing.T) {
	var cap captured
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()
	useEndpoint(t, srv.URL)
	t.Setenv("XXYI_ADDR", "")
	t.Setenv("TMUX", "")
	// 第三步兜底只吃注入（绝不真执行 tmux）：链上没有 pane 命中 → 还是"不知道自己的地址"。
	oldPP, oldLP := parentPID, listPanes
	t.Cleanup(func() { parentPID, listPanes = oldPP, oldLP })
	parentPID = func(int) (int, error) { return 1, nil }
	listPanes = func() (string, error) { return "55 other", nil }

	resps := mcpRound(t,
		mcpCall(1, "tools/call", map[string]any{"name": "xxyi_send", "arguments": map[string]any{
			"to": "mac:boss", "kind": "notice", "text": "hi",
		}})+
			mcpCall(2, "ping", nil))
	if len(resps) != 2 {
		t.Fatalf("回应条数 %d", len(resps))
	}
	text, isErr := mcpResultText(t, resps[0])
	if !isErr || !strings.Contains(text, "不知道自己的地址") {
		t.Errorf("没地址时 %q isError=%v", text, isErr)
	}
	if n := cap.count(); n != 0 {
		t.Errorf("没地址时不该发请求，已发 %d", n)
	}
}

func TestMCPBadJSONAndUnknownMethod(t *testing.T) {
	resps := mcpRound(t,
		`{"jsonrpc":"2.0","id":1,"method":"ping"`+"\n"+ // 缺右括号
			`not json at all`+"\n"+
			mcpCall(7, "tools/run", nil))
	if len(resps) != 3 {
		t.Fatalf("回应条数 %d", len(resps))
	}
	bad := resps[0]["error"].(map[string]any)
	if code, _ := bad["code"].(float64); code != rpcParseError {
		t.Errorf("解析失败错误码 %v", bad["code"])
	}
	if resps[0]["id"] != nil {
		t.Errorf("解析失败时 id 要是 null，得到 %v", resps[0]["id"])
	}
	if code, _ := resps[1]["error"].(map[string]any)["code"].(float64); code != rpcParseError {
		t.Errorf("非 JSON 行错误码 %v", resps[1]["error"])
	}
	if resps[1]["id"] != nil {
		t.Errorf("非 JSON 行 id 要是 null")
	}
	unknown := resps[2]["error"].(map[string]any)
	if code, _ := unknown["code"].(float64); code != rpcMethodNotFound {
		t.Errorf("未知方法错误码 %v", unknown["code"])
	}
	if msg, _ := unknown["message"].(string); msg != "method not found" {
		t.Errorf("message %q", msg)
	}
	if got, _ := resps[2]["id"].(float64); got != 7 {
		t.Errorf("id %v", resps[2]["id"])
	}
}

// 标准输入 EOF：serveMCP 正常返回 nil（run 退出 0）。
func TestMCPEOIReturnsNil(t *testing.T) {
	if err := serveMCP(strings.NewReader(""), &bytes.Buffer{}); err != nil {
		t.Fatalf("空输入应返回 nil，得到 %v", err)
	}
}

// file_path 与命令行共用 loadAttach：目录外的附件 isError 且不发消息，工作目录下的正常通过。
func TestMCPAttachPathRestriction(t *testing.T) {
	var cap captured
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()
	useEndpoint(t, srv.URL)
	t.Setenv("XXYI_ADDR", "ubuntu:cw")

	proj := t.TempDir()
	t.Chdir(proj)
	good := filepath.Join(proj, "a.txt")
	if err := os.WriteFile(good, []byte("A"), 0o600); err != nil {
		t.Fatal(err)
	}
	outsideFile := filepath.Join(t.TempDir(), "b.txt")
	if err := os.WriteFile(outsideFile, []byte("B"), 0o600); err != nil {
		t.Fatal(err)
	}

	resps := mcpRound(t,
		mcpCall(1, "tools/call", map[string]any{"name": "xxyi_send", "arguments": map[string]any{
			"to": "mac:boss", "kind": "notice", "text": "带附件", "file_path": good,
		}})+
			mcpCall(2, "tools/call", map[string]any{"name": "xxyi_reply", "arguments": map[string]any{
				"id": "id42", "text": "带目录外附件", "file_path": outsideFile,
			}}))
	if len(resps) != 2 {
		t.Fatalf("回应条数 %d", len(resps))
	}
	text, isErr := mcpResultText(t, resps[0])
	if isErr {
		t.Fatalf("工作目录内附件该通过：%q", text)
	}
	got := cap.last(t)
	if got.body.FileName != "a.txt" || string(got.body.File) != "A" {
		t.Errorf("附件 %q %q", got.body.FileName, got.body.File)
	}
	text, isErr = mcpResultText(t, resps[1])
	if !isErr || !strings.Contains(text, "附件只能是当前项目目录或 ~/xxyi-work 下的文件") {
		t.Errorf("目录外附件 %q isError=%v", text, isErr)
	}
	if n := cap.count(); n != 1 {
		t.Errorf("目录外附件不该发请求，共收到 %d", n)
	}
}
