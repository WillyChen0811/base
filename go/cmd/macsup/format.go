package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// truncRunes 按字符数截断。
func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// firstLine 取第一行。
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// lastLine 取最后一个非空行。
func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[i+1:])
	}
	return s
}

// contentBlocks 解析 message.content 为块列表；字符串内容视为单个 text 块。
func contentBlocks(msg json.RawMessage) []map[string]any {
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(msg, &m) != nil || len(m.Content) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return []map[string]any{{"type": "text", "text": s}}
	}
	var blocks []map[string]any
	_ = json.Unmarshal(m.Content, &blocks)
	return blocks
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// toolSummary 生成 tool_use 的摘要。
func toolSummary(name string, input any) string {
	in, _ := input.(map[string]any)
	if name == "Bash" {
		if c := str(in["command"]); c != "" {
			return c
		}
	}
	if p := str(in["file_path"]); p != "" {
		return p
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(input)
	return truncRunes(strings.TrimSuffix(buf.String(), "\n"), 120)
}

// resultText 取 tool_result 的文本内容（字符串或 text 块数组）。
func resultText(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var sb strings.Builder
		for _, it := range c {
			if m, ok := it.(map[string]any); ok {
				sb.WriteString(str(m["text"]))
			}
		}
		return sb.String()
	}
	return ""
}

// eventType 返回事件的 type 字段。
func eventType(data json.RawMessage) string {
	var h struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(data, &h)
	return h.Type
}

// formatEvent 把一条 stream-json 事件格式化为若干行；不认识的类型返回空切片。
func formatEvent(data json.RawMessage) []string {
	out := []string{}
	var e map[string]any
	if json.Unmarshal(data, &e) != nil {
		return out
	}
	msgRaw, _ := json.Marshal(e["message"])
	switch str(e["type"]) {
	case "system":
		if str(e["subtype"]) == "init" {
			out = append(out, fmt.Sprintf("== 会话 %s，模型 %s", str(e["session_id"]), str(e["model"])))
		}
	case "assistant":
		for _, b := range contentBlocks(msgRaw) {
			switch str(b["type"]) {
			case "text":
				out = append(out, str(b["text"]))
			case "tool_use":
				out = append(out, "→ "+str(b["name"])+" "+toolSummary(str(b["name"]), b["input"]))
			}
		}
	case "user":
		for _, b := range contentBlocks(msgRaw) {
			if str(b["type"]) != "tool_result" {
				continue
			}
			text := resultText(b["content"])
			if isErr, _ := b["is_error"].(bool); isErr {
				out = append(out, "  ✗ "+truncRunes(firstLine(text), 200))
			} else {
				out = append(out, fmt.Sprintf("  ✓ (%d 字)", len([]rune(text))))
			}
		}
	case "result":
		dur, _ := e["duration_ms"].(float64)
		cost, _ := e["total_cost_usd"].(float64)
		out = append(out, fmt.Sprintf("== 本轮结束：%s，%v 秒，$%.4f", str(e["subtype"]), dur/1000, cost))
		// 成功时 result 就是最后一段助手文字，前面已经打印过；只在出错时补上
		if r := str(e["result"]); r != "" && e["is_error"] == true {
			out = append(out, r)
		}
	case "exited":
		code := ""
		if e["code"] != nil {
			code = fmt.Sprint(e["code"])
		}
		out = append(out, strings.TrimSpace(fmt.Sprintf("== Mac 上的 Claude 已退出：code=%s %s", code, lastLine(str(e["stderr"])))))
	case "raw":
		if t := str(e["text"]); t != "" {
			out = append(out, t)
		}
	}
	return out
}
