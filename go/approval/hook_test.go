package approval

import (
	"encoding/json"
	"testing"
)

func TestParseHookInput(t *testing.T) {
	in := `{"session_id":"abc","hook_event_name":"PermissionRequest","tool_name":"Bash",` +
		`"tool_input":{"command":"rm -rf node_modules","description":"x"},` +
		`"permission_suggestions":[{"type":"always"}]}`
	h, err := ParseHookInput([]byte(in))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if h.SessionID != "abc" || h.Tool != "Bash" {
		t.Fatalf("字段不对: %+v", h)
	}
	var ti struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(h.Input, &ti); err != nil || ti.Command != "rm -rf node_modules" {
		t.Fatalf("tool_input 不对: %s err=%v", h.Input, err)
	}
}

func TestParseHookInputErrors(t *testing.T) {
	tests := []struct{ name, in string }{
		{"非法 JSON", `{"session_id":`},
		{"事件不对", `{"session_id":"a","hook_event_name":"PreToolUse","tool_name":"Bash"}`},
		{"缺 tool_name", `{"session_id":"a","hook_event_name":"PermissionRequest"}`},
		{"空 tool_name", `{"hook_event_name":"PermissionRequest","tool_name":""}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParseHookInput([]byte(tt.in)); err == nil {
				t.Fatalf("应返回错误: %s", tt.in)
			}
		})
	}
}

func TestParseHookInputMissingInput(t *testing.T) {
	h, err := ParseHookInput([]byte(`{"hook_event_name":"PermissionRequest","tool_name":"Bash"}`))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if string(h.Input) != "{}" {
		t.Fatalf("缺失的 tool_input 应为 {}，实际 %q", h.Input)
	}
}

// outDecision 从钩子输出里解出 decision。
func outDecision(t *testing.T, b []byte) (behavior, message string) {
	t.Helper()
	var out struct {
		HookSpecificOutput struct {
			HookEventName string `json:"hookEventName"`
			Decision      struct {
				Behavior string `json:"behavior"`
				Message  string `json:"message"`
			} `json:"decision"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("输出不能被 json.Unmarshal 还原: %v (%s)", err, b)
	}
	if out.HookSpecificOutput.HookEventName != "PermissionRequest" {
		t.Fatalf("hookEventName 不对: %s", b)
	}
	return out.HookSpecificOutput.Decision.Behavior, out.HookSpecificOutput.Decision.Message
}

func TestHookOutput(t *testing.T) {
	b, msg := outDecision(t, HookOutput(Decision{Allow: true}))
	if b != "allow" || msg != "" {
		t.Fatalf("允许的输出不对: %s", HookOutput(Decision{Allow: true}))
	}

	b, msg = outDecision(t, HookOutput(Decision{Allow: false, Message: "太危险"}))
	if b != "deny" || msg != "太危险" {
		t.Fatalf("拒绝的输出不对: %s", HookOutput(Decision{Allow: false, Message: "太危险"}))
	}

	b, msg = outDecision(t, HookOutput(Decision{Allow: false}))
	if b != "deny" || msg != "用户拒绝" {
		t.Fatalf("空原因应兜底为用户拒绝: %s", HookOutput(Decision{Allow: false}))
	}
}

func TestHookOutputEscaping(t *testing.T) {
	tricky := "他说\"不行\"\n然后\\撤了 中文😅"
	_, msg := outDecision(t, HookOutput(Decision{Allow: false, Message: tricky}))
	if msg != tricky {
		t.Fatalf("消息未原样还原: %q", msg)
	}
}

func TestHookOutputExactJSON(t *testing.T) {
	allow := `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow"}}}`
	if got := string(HookOutput(Decision{Allow: true})); got != allow {
		t.Fatalf("允许输出不一致:\n got %s\nwant %s", got, allow)
	}
	deny := `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"deny","message":"太危险"}}}`
	if got := string(HookOutput(Decision{Allow: false, Message: "太危险"})); got != deny {
		t.Fatalf("拒绝输出不一致:\n got %s\nwant %s", got, deny)
	}
}
