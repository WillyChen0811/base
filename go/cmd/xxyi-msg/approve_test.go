package main

// approve-hook 的测试：一律走 run() 真实入口（子命令分发漏接时这些用例就会红）。
// 假服务用 run_test.go 的 captured + useEndpoint/setPaths（端点与 CA 现签的客户端证书包），
// 审批响应由 captured.respApproval 给。约定：没拿到决定就空输出退出 0（退回终端确认），
// 拿到决定才打印 support.HookOutput 那一行 JSON。

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/WillyChen0811/base/go/support"
)

// hookStdin 是 Claude Code 的 PermissionRequest 钩子往标准输入里塞的那种 JSON。
const hookStdin = `{"session_id":"s1","hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"ls -l"},"cwd":"/tmp/proj"}`

// approvalAskBody 是 POST /approval/ask 的正文（跟服务端 approvalAskReq 同字段）。
type approvalAskBody struct {
	Addr  string          `json:"addr"`
	Agent string          `json:"agent"`
	Cwd   string          `json:"cwd"`
	Hook  json.RawMessage `json:"hook"`
}

// runApprove 跑一次 approve-hook，返回退出码、标准输出、标准错误。
func runApprove(t *testing.T, args []string, stdin string) (int, string, string) {
	t.Helper()
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	code := run(append([]string{"approve-hook"}, args...), strings.NewReader(stdin), out, errOut)
	return code, out.String(), errOut.String()
}

// quietOK 断言「没拿到决定」的约定：空输出、空标准错误、退出 0。
func quietOK(t *testing.T, code int, out, errOut string) {
	t.Helper()
	if code != exitOK || out != "" || errOut != "" {
		t.Fatalf("退出码 %d，stdout %q，stderr %q（该空输出退出 0）", code, out, errOut)
	}
}

// wantDecision 断言标准输出正好是 support.HookOutput(want) 那一行（顺带解开看清决定本身）。
func wantDecision(t *testing.T, out string, want support.Decision) {
	t.Helper()
	if out != string(support.HookOutput(want))+"\n" {
		t.Fatalf("输出 %q，want %q", out, string(support.HookOutput(want)))
	}
	var parsed struct {
		HookSpecificOutput struct {
			HookEventName string `json:"hookEventName"`
			Decision      struct {
				Behavior string `json:"behavior"`
				Message  string `json:"message"`
			} `json:"decision"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("输出不是合法 JSON：%v（%q）", err, out)
	}
	behavior := "deny"
	if want.Allow {
		behavior = "allow"
	}
	got := parsed.HookSpecificOutput
	if got.HookEventName != "PermissionRequest" || got.Decision.Behavior != behavior {
		t.Errorf("决定 %s/%s，want %s", got.HookEventName, got.Decision.Behavior, behavior)
	}
}

// TestApproveHookAllow 用户批准：打印 allow，请求带证书签名与地址/agent/cwd/钩子原文。
func TestApproveHookAllow(t *testing.T) {
	var cap captured
	cap.respApproval = `{"decided":true,"allow":true}`
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()
	useEndpoint(t, srv.URL)
	t.Setenv("XXYI_ADDR", "ubuntu:cw")

	code, out, errOut := runApprove(t, []string{"--agent", "codex"}, hookStdin)
	if code != exitOK || errOut != "" {
		t.Fatalf("退出码 %d，stderr %q", code, errOut)
	}
	wantDecision(t, out, support.Decision{Allow: true})

	req := cap.last(t)
	if req.method != http.MethodPost || req.path != "/approval/ask" {
		t.Errorf("请求 %s %s", req.method, req.path)
	}
	if req.xxauth == "" {
		t.Error("审批请求必须带 X-XXYI-Auth（客户端证书签名）")
	}
	var body approvalAskBody
	if err := json.Unmarshal(req.raw, &body); err != nil {
		t.Fatalf("正文看不懂：%v（%s）", err, req.raw)
	}
	if body.Addr != "ubuntu:cw" || body.Agent != "codex" || body.Cwd != "/tmp/proj" {
		t.Errorf("正文 %+v", body)
	}
	if !strings.Contains(string(body.Hook), `"tool_name":"Bash"`) {
		t.Errorf("钩子原文没原样带过去：%s", body.Hook)
	}
}

// TestApproveHookDeny 用户拒绝：打印 deny；没写原因时用本地兜底文案。
func TestApproveHookDeny(t *testing.T) {
	cases := []struct {
		name string
		resp string
		want support.Decision
	}{
		{"带原因", `{"decided":true,"allow":false,"message":"太危险"}`, support.Decision{Allow: false, Message: "太危险"}},
		{"没写原因", `{"decided":true,"allow":false,"message":""}`, support.Decision{Allow: false, Message: defaultApproveDeny}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var cap captured
			cap.respApproval = c.resp
			srv := httptest.NewServer(cap.handler())
			defer srv.Close()
			useEndpoint(t, srv.URL)
			t.Setenv("XXYI_ADDR", "ubuntu:cw")

			code, out, errOut := runApprove(t, []string{"--agent", "claude"}, hookStdin)
			if code != exitOK || errOut != "" {
				t.Fatalf("退出码 %d，stderr %q", code, errOut)
			}
			wantDecision(t, out, c.want)
		})
	}
}

// TestApproveHookUndecided 服务端没给出决定（超时/取消）：空输出退出 0，请求确实发出去了。
func TestApproveHookUndecided(t *testing.T) {
	var cap captured // respApproval 留空 ⇒ 假服务回 {"decided":false}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()
	useEndpoint(t, srv.URL)
	t.Setenv("XXYI_ADDR", "ubuntu:cw")

	code, out, errOut := runApprove(t, []string{"--agent", "claude"}, hookStdin)
	quietOK(t, code, out, errOut)
	if n := cap.count(); n != 1 {
		t.Errorf("发了 %d 个请求，want 1", n)
	}
}

// TestApproveHookConnFails 连不上服务：空输出退出 0，原因进 msg-hook.log。
func TestApproveHookConnFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".xxyi"), 0o700); err != nil {
		t.Fatal(err)
	}
	useEndpoint(t, "http://"+listenFreeAddr(t)) // 端点写好了，那头没人听
	t.Setenv("XXYI_ADDR", "ubuntu:cw")

	code, out, errOut := runApprove(t, []string{"--agent", "claude"}, hookStdin)
	quietOK(t, code, out, errOut)

	raw, err := os.ReadFile(filepath.Join(home, ".xxyi", hookLogName))
	if err != nil {
		t.Fatalf("排查日志读不了：%v", err)
	}
	line := strings.TrimSpace(string(raw))
	// 端点文件在、那头没人听：失败原因记在「审批请求失败：连不上 …」里（行首是时间戳）
	if !strings.Contains(line, "approve-hook --agent claude addr=ubuntu:cw 审批请求失败：连不上") {
		t.Errorf("日志 %q", line)
	}
}

// TestApproveHookNoCertOrAddr 没装客户端证书 / 取不到自己地址：空输出退出 0，且不碰网络。
func TestApproveHookNoCertOrAddr(t *testing.T) {
	t.Run("没有证书", func(t *testing.T) {
		var cap captured
		cap.respApproval = `{"decided":true,"allow":true}`
		srv := httptest.NewServer(cap.handler())
		defer srv.Close()
		useEndpoint(t, srv.URL)
		clientBundlePathUbuntu = filepath.Join(t.TempDir(), "absent.json") // setPaths 已登记还原
		t.Setenv("XXYI_ADDR", "ubuntu:cw")

		code, out, errOut := runApprove(t, []string{"--agent", "claude"}, hookStdin)
		quietOK(t, code, out, errOut)
		if n := cap.count(); n != 0 {
			t.Errorf("没证书也不该发请求，已发 %d", n)
		}
	})

	t.Run("没有地址", func(t *testing.T) {
		var cap captured
		cap.respApproval = `{"decided":true,"allow":true}`
		srv := httptest.NewServer(cap.handler())
		defer srv.Close()
		useEndpoint(t, srv.URL)
		t.Setenv("HOME", t.TempDir())
		t.Setenv("XXYI_ADDR", "")
		t.Setenv("TMUX", "")
		useFakeTree(t, map[int]int{os.Getpid(): 90, 90: 80, 80: 1}, "55 other\n") // 链上没有自己的 pane

		code, out, errOut := runApprove(t, []string{"--agent", "claude"}, hookStdin)
		quietOK(t, code, out, errOut)
		if n := cap.count(); n != 0 {
			t.Errorf("没地址也不该发请求，已发 %d", n)
		}
	})
}

// TestApproveHookBadArgs 参数不合法：空输出退出 0，不碰网络。
func TestApproveHookBadArgs(t *testing.T) {
	var cap captured
	cap.respApproval = `{"decided":true,"allow":true}`
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()
	useEndpoint(t, srv.URL)
	t.Setenv("XXYI_ADDR", "ubuntu:cw")

	for _, args := range [][]string{
		{"--agent"},        // --agent 后面没值
		{"--agent=claude"}, // 等号写法不认
		{"claude"},         // 多余位置参数
		{"--agent", "claude", "extra"},
	} {
		code, out, errOut := runApprove(t, args, hookStdin)
		quietOK(t, code, out, errOut)
	}
	if n := cap.count(); n != 0 {
		t.Errorf("参数不合法也不该发请求，已发 %d", n)
	}
}

// TestApproveHookAgentDefaults 未传 --agent 时不报错，按钩子输入里的 agent、再退到 claude
// （usage 里 --agent 是可选的；值不由客户端把门，交给界面显示）。
func TestApproveHookAgentDefaults(t *testing.T) {
	cases := []struct {
		name  string
		args  []string
		stdin string
		want  string
	}{
		{"都没给", nil, hookStdin, "claude"},
		{"用钩子输入里的", nil, strings.Replace(hookStdin, `"session_id"`, `"agent":"codex","session_id"`, 1), "codex"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var cap captured
			cap.respApproval = `{"decided":true,"allow":true}`
			srv := httptest.NewServer(cap.handler())
			defer srv.Close()
			useEndpoint(t, srv.URL)
			t.Setenv("XXYI_ADDR", "ubuntu:cw")

			code, out, errOut := runApprove(t, c.args, c.stdin)
			if code != exitOK || errOut != "" {
				t.Fatalf("退出码 %d，stderr %q", code, errOut)
			}
			wantDecision(t, out, support.Decision{Allow: true})
			var body approvalAskBody
			if err := json.Unmarshal(cap.last(t).raw, &body); err != nil {
				t.Fatal(err)
			}
			if body.Agent != c.want {
				t.Errorf("agent %q，want %q", body.Agent, c.want)
			}
		})
	}
}

// TestApproveHookBadStdin 标准输入是空的／不是合法 JSON：空输出退出 0，不碰网络。
// （钩子事件是不是 PermissionRequest 由服务端 ParseHookInput 把门，客户端只把原文交出去。）
func TestApproveHookBadStdin(t *testing.T) {
	var cap captured
	cap.respApproval = `{"decided":true,"allow":true}`
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()
	useEndpoint(t, srv.URL)
	t.Setenv("XXYI_ADDR", "ubuntu:cw")

	for _, stdin := range []string{"", "   ", "not json"} {
		code, out, errOut := runApprove(t, []string{"--agent", "claude"}, stdin)
		quietOK(t, code, out, errOut)
	}
	if n := cap.count(); n != 0 {
		t.Errorf("钩子输入不合法也不该发请求，已发 %d", n)
	}
}

// TestRunUnknownCommandStillErrors 接上 approve-hook 之后，未知命令的报错不能跟着变松。
func TestRunUnknownCommandStillErrors(t *testing.T) {
	errOut := &bytes.Buffer{}
	if code := run([]string{"approve"}, nil, io.Discard, errOut); code != exitErr {
		t.Fatalf("退出码 %d", code)
	}
	if !strings.Contains(errOut.String(), "未知命令") {
		t.Errorf("stderr %q", errOut.String())
	}
}
