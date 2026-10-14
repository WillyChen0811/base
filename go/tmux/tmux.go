// Package tmux 提供 tmux 命令拼接、输出解析与会话名校验。
// 只做字符串处理，不做任何网络或进程调用。
package tmux

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Session 是 tmux list-sessions 输出的一行。
type Session struct {
	Name     string
	Attached int // 当前连接的客户端数
	Windows  int
	Created  time.Time // 秒级 Unix 时间
	Activity time.Time
}

// ── 校验 ──────────────────────────────────────────────

// ValidNewName 校验"新建会话"的名字：
// 1..64 个字符；只允许 Unicode 字母、数字、'-'、'_'；不能以 '-' 开头。
func ValidNewName(name string) error {
	if name == "" {
		return fmt.Errorf("会话名不能为空")
	}
	if n := utf8.RuneCountInString(name); n > 64 {
		return fmt.Errorf("会话名不能超过 64 个字符（当前 %d）", n)
	}
	for i, r := range name {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' && r != '_' {
			return fmt.Errorf("会话名只能包含字母、数字、'-' 和 '_'，非法字符 '%c' 在第 %d 位", r, i+1)
		}
	}
	if name[0] == '-' {
		return fmt.Errorf("会话名不能以 '-' 开头")
	}
	return nil
}

// ValidTarget 校验"操作已有会话"的名字：
// 非空、≤256 字节、合法 UTF-8、不含控制字符（< 0x20 和 0x7f）、不含 ':' 和 '.'
// （tmux 建会话时会把它们换成 _，真实会话名不含；而 "=a:x" 会被解析成会话 a 的窗口 x）。
func ValidTarget(name string) error {
	if name == "" {
		return fmt.Errorf("目标会话名不能为空")
	}
	if len([]byte(name)) > 256 {
		return fmt.Errorf("目标会话名不能超过 256 字节（当前 %d）", len([]byte(name)))
	}
	for i, r := range name {
		if r == ':' || r == '.' {
			return fmt.Errorf("目标会话名不能包含 %q（在第 %d 位）", r, i+1)
		}
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("目标会话名包含控制字符 U+%04X 在第 %d 位", r, i+1)
		}
	}
	if !utf8.ValidString(name) {
		return fmt.Errorf("目标会话名不是合法的 UTF-8")
	}
	return nil
}

// ── Shell 转义 ────────────────────────────────────────

// Quote 把任意字符串做成单引号包住的 shell 参数。
// 原理：' 不能直接被转义，改为断言末尾单引号 → 插入转义单引号 → 续上剩余部分。
// 例: a'b → 'a'\”b'
func Quote(s string) string {
	q := "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
	return q
}

// ── 命令构造 ──────────────────────────────────────────
// 所有命令都带 -u：经 SSH 执行时远端往往没有 LANG，tmux 会把中文等非 ASCII 字符显示成 _（会话名和终端内容都会坏）。

// ListCommand 返回列会话的完整命令字符串。
// 字段间用空格分隔（tmux 3.4 会把 -F 输出里的制表符等控制字符换成 _），会话名放在最后，名字里有空格也能正确切出。
func ListCommand() string {
	return "tmux -u list-sessions -F '#{session_attached} #{session_windows} #{session_created} #{session_activity} #{session_name}'"
}

// NewCommand 返回创建新会话的命令：tmux new-session -d -s '名'
func NewCommand(name string) (string, error) {
	if err := ValidNewName(name); err != nil {
		return "", err
	}
	return "tmux -u new-session -d -s " + Quote(name), nil
}

// AttachCommand 返回附加会话的命令：tmux attach-session -t '=名'
// = 前缀表示精确匹配，避免前缀误中。
func AttachCommand(name string) (string, error) {
	if err := ValidTarget(name); err != nil {
		return "", err
	}
	return "tmux -u attach-session -t " + Quote("="+name), nil
}

// KillCommand 返回终止会话的命令：tmux kill-session -t '=名'
func KillCommand(name string) (string, error) {
	if err := ValidTarget(name); err != nil {
		return "", err
	}
	return "tmux -u kill-session -t " + Quote("="+name), nil
}

// bufName 校验临时缓冲区名：只允许 ASCII 字母数字、'_'、'-'，1..64 字节。
func bufName(name string) error {
	if len(name) == 0 || len(name) > 64 {
		return fmt.Errorf("缓冲区名长度必须 1..64 字节，当前 %d", len(name))
	}
	if m := bufIllegal.FindString(name); m != "" {
		return fmt.Errorf("缓冲区名包含非法字符 %q，只允许字母、数字、'-' 和 '_'", m)
	}
	return nil
}

var bufIllegal = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// PasteCommand 返回把标准输入里的文字以括号粘贴方式注入会话、再按回车的 shell 命令。
// 文字本身不进命令行，由调用方经 SSH 的 stdin 喂入。buf 是临时缓冲区名（调用方给，如 "xxyi-<消息id>"）。
// -p 走括号粘贴（文字不被逐字符当成按键，多行/带特殊字符都安全），-d 粘贴完删掉缓冲区。
// 粘贴和回车之间 sleep 0.3：粘贴内容落到 shell 输入行需要时间，立刻回车可能没提交。
func PasteCommand(session, buf string) (string, error) {
	if err := ValidTarget(session); err != nil {
		return "", err
	}
	if err := bufName(buf); err != nil {
		return "", err
	}
	qBuf := Quote(buf)
	qTarget := Quote("=" + session + ":")
	return "tmux -u load-buffer -b " + qBuf + " -" +
		" && tmux -u paste-buffer -p -d -b " + qBuf + " -t " + qTarget +
		" && sleep 0.3 && tmux -u send-keys -t " + qTarget + " Enter", nil
}

// CaptureCommand 返回抓取窗格内容的命令。
// lines: 1..5000，超出返回错误。读取从底部向上 lines 行。
func CaptureCommand(name string, lines int) (string, error) {
	if err := ValidTarget(name); err != nil {
		return "", err
	}
	if lines < 1 || lines > 5000 {
		return "", fmt.Errorf("行数必须在 1..5000 之间，当前 %d", lines)
	}
	return fmt.Sprintf("tmux -u capture-pane -p -J -t %s -S -%d", Quote("="+name+":"), lines), nil
}

// PaneCommandCommand 返回查询会话活动窗格前台程序名的命令（如 claude、node、bash）。
// 注入消息前用它确认窗格里跑的是 Claude，不是就别往里灌文字。
func PaneCommandCommand(session string) (string, error) {
	if err := ValidTarget(session); err != nil {
		return "", err
	}
	return "tmux -u display-message -p -t " + Quote("="+session+":") + " " + Quote("#{pane_current_command}"), nil
}

// ── 输出解析 ──────────────────────────────────────────

// ParseList 解析 ListCommand 的标准输出。
// - 空输入返回空切片（非 nil）
// - 某行格式不对返回带行号的错误
// - 行尾 \r\n 和 \n 都能处理
func ParseList(out []byte) ([]Session, error) {
	lines := strings.Split(string(out), "\n")
	// 末尾换行会产生空行，丢弃
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return []Session{}, nil
	}

	var sessions []Session
	for i, line := range lines {
		line = strings.TrimRight(line, "\r") // 处理 \r\n
		if line == "" {
			continue // 跳过空白行
		}

		parts := strings.SplitN(line, " ", 5) // 前 4 个是数字，名字放最后，名字里有空格也不怕
		if len(parts) < 5 {
			return nil, fmt.Errorf("第 %d 行字段数不足（期望 5 个空格分隔的字段，实际 %d）", i+1, len(parts)-1)
		}

		s := new(Session)

		var err error
		s.Attached, err = strconv.Atoi(parts[0])
		if err != nil {
			return nil, fmt.Errorf("第 %d 行 attached '%s' 不是整数", i+1, parts[0])
		}

		s.Windows, err = strconv.Atoi(parts[1])
		if err != nil {
			return nil, fmt.Errorf("第 %d 行 windows '%s' 不是整数", i+1, parts[1])
		}

		created, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("第 %d 行 created '%s' 不是时间戳", i+1, parts[2])
		}
		s.Created = time.Unix(created, 0)

		activity, err := strconv.ParseInt(parts[3], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("第 %d 行 activity '%s' 不是时间戳", i+1, parts[3])
		}
		s.Activity = time.Unix(activity, 0)

		// 第 5 部分（索引 4）是会话名；名字原样保留，不 trim
		s.Name = parts[4]
		if s.Name == "" {
			return nil, fmt.Errorf("第 %d 行会话名为空", i+1)
		}

		sessions = append(sessions, *s)
	}
	return sessions, nil
}

// ── 错误模式匹配 ──────────────────────────────────────

// NoServer 判断 tmux 的 stderr 是否表示"tmux 服务没起"。
func NoServer(stderr string) bool {
	return strings.Contains(stderr, "no server running") ||
		strings.Contains(stderr, "error connecting to")
}

// NotFound 判断 stderr 是否表示会话不存在。
func NotFound(stderr string) bool {
	return strings.Contains(stderr, "can't find session")
}

// Duplicate 判断 stderr 是否表示会话已存在。
func Duplicate(stderr string) bool {
	return strings.Contains(stderr, "duplicate session")
}
