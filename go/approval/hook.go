package approval

import (
	"encoding/json"
	"errors"
	"fmt"
)

// defaultDenyMessage 是拒绝但没给原因时的兜底文案。
const defaultDenyMessage = "用户拒绝"

// HookInput 是 Claude Code 的 PermissionRequest 钩子收到的标准输入。
type HookInput struct {
	SessionID string          `json:"session_id"`
	Event     string          `json:"hook_event_name"`
	Tool      string          `json:"tool_name"`
	Input     json.RawMessage `json:"tool_input"`
}

// ParseHookInput 解析钩子输入；非法 JSON、事件不是 PermissionRequest、
// tool_name 为空都返回错误。tool_input 缺失时补成 "{}"。
func ParseHookInput(b []byte) (HookInput, error) {
	var h HookInput
	if err := json.Unmarshal(b, &h); err != nil {
		return HookInput{}, fmt.Errorf("approval: 钩子输入不是合法 JSON: %w", err)
	}
	if h.Event != "PermissionRequest" {
		return HookInput{}, fmt.Errorf("approval: 钩子事件不是 PermissionRequest: %q", h.Event)
	}
	if h.Tool == "" {
		return HookInput{}, errors.New("approval: 钩子输入缺少 tool_name")
	}
	if len(h.Input) == 0 || string(h.Input) == "null" {
		h.Input = json.RawMessage("{}")
	}
	return h, nil
}

// 钩子标准输出的结构，用结构体生成保证转义正确。
type hookDecision struct {
	Behavior string `json:"behavior"`
	Message  string `json:"message,omitempty"`
}

type hookSpecificOutput struct {
	HookEventName string       `json:"hookEventName"`
	Decision      hookDecision `json:"decision"`
}

type hookOutput struct {
	HookSpecificOutput hookSpecificOutput `json:"hookSpecificOutput"`
}

// HookOutput 生成钩子的标准输出。允许时不带 message；
// 拒绝时 message 为空则用 "用户拒绝"。
func HookOutput(d Decision) []byte {
	ho := hookOutput{HookSpecificOutput: hookSpecificOutput{
		HookEventName: "PermissionRequest",
	}}
	if d.Allow {
		ho.HookSpecificOutput.Decision.Behavior = "allow"
	} else {
		ho.HookSpecificOutput.Decision.Behavior = "deny"
		ho.HookSpecificOutput.Decision.Message = d.Message
		if ho.HookSpecificOutput.Decision.Message == "" {
			ho.HookSpecificOutput.Decision.Message = defaultDenyMessage
		}
	}
	// 结构体只含字符串，Marshal 不会失败；兜底输出仍是拒绝，安全侧不倒向允许。
	b, err := json.Marshal(ho)
	if err != nil {
		return []byte(`{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"deny","message":"生成输出失败"}}}`)
	}
	return b
}
