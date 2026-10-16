package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

func recallReq(t *testing.T, cap *captured) map[string]string {
	t.Helper()
	r := cap.last(t)
	if r.method != "POST" || r.path != "/msg/recall" {
		t.Fatalf("最后一个请求不是 POST /msg/recall：%s %s", r.method, r.path)
	}
	var m map[string]string
	if err := json.Unmarshal(r.raw, &m); err != nil {
		t.Fatalf("正文不是 JSON：%v", err)
	}
	return m
}

func TestCLIRecall(t *testing.T) {
	var cap captured
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()
	useEndpoint(t, srv.URL)
	t.Setenv("XXYI_ADDR", "ubuntu:cw")

	out := &bytes.Buffer{}
	if code := run([]string{"recall", "ab12"}, nil, out, io.Discard); code != exitOK {
		t.Fatalf("退出码 %d，输出 %q", code, out.String())
	}
	m := recallReq(t, &cap)
	if m["id"] != "ab12" || m["from"] != "ubuntu:cw" {
		t.Errorf("正文 %v", m)
	}
	if cap.last(t).xxauth == "" {
		t.Errorf("recall 应带证书签名头")
	}
	if !strings.Contains(out.String(), "已撤回 #ab12") {
		t.Errorf("输出 %q", out.String())
	}
	// 缺 id
	if code := run([]string{"recall"}, nil, io.Discard, io.Discard); code != exitErr {
		t.Errorf("缺 id 应失败，退出码 %d", code)
	}
}

func TestCLIRecallServerError(t *testing.T) {
	var cap captured
	cap.respErr = "只有原发件人能撤回自己的消息"
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()
	useEndpoint(t, srv.URL)
	t.Setenv("XXYI_ADDR", "ubuntu:cw")
	errOut := &bytes.Buffer{}
	if code := run([]string{"recall", "ab12"}, nil, io.Discard, errOut); code != exitErr {
		t.Fatalf("退出码 %d", code)
	}
	if !strings.Contains(errOut.String(), "撤回失败") || !strings.Contains(errOut.String(), "只有原发件人") {
		t.Errorf("错误输出 %q", errOut.String())
	}
}

func TestMCPRecall(t *testing.T) {
	var cap captured
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()
	useEndpoint(t, srv.URL)
	t.Setenv("XXYI_ADDR", "ubuntu:cw")
	resps := mcpRound(t,
		mcpCall(1, "tools/call", map[string]any{"name": "xxyi_recall", "arguments": map[string]any{"id": "#ab12"}})+
			mcpCall(2, "tools/call", map[string]any{"name": "xxyi_recall", "arguments": map[string]any{}}))
	text, isErr := mcpResultText(t, resps[0])
	if isErr || !strings.Contains(text, "已撤回 #ab12") {
		t.Fatalf("结果 %q isError=%v", text, isErr)
	}
	cap.mu.Lock()
	var got map[string]string
	for _, r := range cap.reqs {
		if r.path == "/msg/recall" {
			_ = json.Unmarshal(r.raw, &got)
		}
	}
	cap.mu.Unlock()
	if got["id"] != "ab12" || got["from"] != "ubuntu:cw" {
		t.Errorf("正文 %v", got)
	}
	if _, isErr := mcpResultText(t, resps[1]); !isErr {
		t.Errorf("缺 id 应 isError")
	}
}
