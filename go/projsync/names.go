package projsync

import "strings"

// ClaudeDirName 按 Claude Code 的规则把项目路径映射成对话记录目录名：
// 路径里每个不是 ASCII 字母或数字的字符（按 rune 计）替换成 `-`。
// 例：/Users/weiyichen/Documents/codes/ai-browser → -Users-weiyichen-Documents-codes-ai-browser
func ClaudeDirName(path string) string {
	var b strings.Builder
	for _, r := range path {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}
