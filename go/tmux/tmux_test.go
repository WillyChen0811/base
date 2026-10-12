package tmux

import (
	"testing"
	"time"
)

func TestValidNewName(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"空字符串", "", true},
		{"单字符", "a", false},
		{"下划线开头", "_session", false},
		{"63字符", "a", false}, // 构造63个a
		{"64字符", "a", false}, // 构造64个a
		{"65字符", "a", true},  // 构造65个a
		{"以-开头", "-a", true},
		{"含点", "a.b", true},
		{"含冒号", "a:b", true},
		{"中文", "中文名", false},
		{"字母数字下划线连字符", "my-session_1", false},
	}

	for _, tt := range tests {
		name := tt.name
		input := tt.input

		// 构造特定长度的测试
		if name == "63字符" {
			input = makeString('a', 63)
		} else if name == "64字符" {
			input = makeString('a', 64)
		} else if name == "65字符" {
			input = makeString('a', 65)
		}

		err := ValidNewName(input)
		if tt.wantErr && err == nil {
			t.Errorf("ValidNewName(%q) 期望报错但没有", input)
		}
		if !tt.wantErr && err != nil {
			t.Errorf("ValidNewName(%q) 期望通过但报错: %v", input, err)
		}
	}

	// 长度按字符数计，不是字节数
	if err := ValidNewName(makeString('中', 64)); err != nil {
		t.Errorf("64 个中文字符应该通过，但报错: %v", err)
	}
	if err := ValidNewName(makeString('中', 65)); err == nil {
		t.Error("65 个中文字符应该报错")
	}
}

func TestValidTarget(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"空字符串", "", true},
		{"普通名字", "session1", false},
		{"含空格", "my session", false},
		{"中文", "中文会话", false},
		{"含冒号", "a:x", true},
		{"含点", "a.b", true},
		{"含换行", "session\nname", true},
		{"含制表符", "session\tname", true},
		{"含DEL", string([]byte{'s', 'e', 's', 's', 'i', 'o', 'n', 0x7f, 'n', 'a', 'm', 'e'}), true},
		{"非法UTF-8", string([]byte{255, 254}), true},
	}

	for _, tt := range tests {
		err := ValidTarget(tt.input)
		if tt.wantErr && err == nil {
			t.Errorf("ValidTarget(%q) 期望报错但没有", tt.input)
		}
		if !tt.wantErr && err != nil {
			t.Errorf("ValidTarget(%q) 期望通过但报错: %v", tt.input, err)
		}
	}

	// 256 字节边界
	long := makeString('a', 256)
	if err := ValidTarget(long); err != nil {
		t.Errorf("256字节应该通过，但报错: %v", err)
	}
	toolong := makeString('a', 257)
	if err := ValidTarget(toolong); err == nil {
		t.Error("257字节应该报错")
	}
}

func makeString(r rune, n int) string {
	b := make([]rune, n)
	for i := range b {
		b[i] = r
	}
	return string(b)
}

func TestQuote(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"plain", "'plain'"},
		{"has space", "'has space'"},
		{"has ' quote", "'has '\\'' quote'"},
		{"含中文", "'含中文'"},
		{"混排: a'b c", `'混排: a'\''b c'`},
		{"含单引号", `'含单引号'`},
		{`中文'a'名字`, `'中文'\''a'\''名字'`},
	}
	for _, tt := range tests {
		got := Quote(tt.in)
		if got != tt.want {
			t.Errorf("Quote(%q) = %q，期望 %q", tt.in, got, tt.want)
		}
	}
}

func TestListCommand(t *testing.T) {
	got := ListCommand()
	want := "tmux -u list-sessions -F '#{session_attached} #{session_windows} #{session_created} #{session_activity} #{session_name}'"
	if got != want {
		t.Errorf("ListCommand() =\n%q\n期望\n%q", got, want)
	}
}

func TestNewCommand(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
		err   bool
	}{
		{"普通", "ab", "tmux -u new-session -d -s 'ab'", false},
		{"含中文", "中文名", "tmux -u new-session -d -s '中文名'", false},
		{"空", "", "", true},
		{"以-开头", "-a", "", true},
	}
	for _, tt := range tests {
		got, err := NewCommand(tt.input)
		if tt.err && err == nil {
			t.Errorf("NewCommand(%q) 期望报错但没有", tt.input)
		}
		if !tt.err && err != nil {
			t.Errorf("NewCommand(%q) 期望通过但报错: %v", tt.input, err)
		}
		if !tt.err && got != tt.want {
			t.Errorf("NewCommand(%q) =\n%q\n期望\n%q", tt.input, got, tt.want)
		}
	}
}

func TestAttachCommand(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
		err   bool
	}{
		{"普通", "ab", "tmux -u attach-session -t '=ab'", false},
		{"含单引号", "a'b", "tmux -u attach-session -t '=a" + "'\\''" + "b'", false},
		{"含空格", "my session", "tmux -u attach-session -t '=my session'", false},
		{"含中文", "中文名", "tmux -u attach-session -t '=中文名'", false},
		{"空", "", "", true},
	}
	for _, tt := range tests {
		got, err := AttachCommand(tt.input)
		if tt.err && err == nil {
			t.Errorf("AttachCommand(%q) 期望报错但没有", tt.input)
		}
		if !tt.err && err != nil {
			t.Errorf("AttachCommand(%q) 期望通过但报错: %v", tt.input, err)
		}
		if !tt.err && got != tt.want {
			t.Errorf("AttachCommand(%q) =\n%q\n期望\n%q", tt.input, got, tt.want)
		}
	}
}

func TestKillCommand(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
		err   bool
	}{
		{"普通", "ab", "tmux -u kill-session -t '=ab'", false},
		{"含单引号", "a'b", "tmux -u kill-session -t '=a" + "'\\''" + "b'", false},
		{"含中文", "中文名", "tmux -u kill-session -t '=中文名'", false},
		{"空", "", "", true},
	}
	for _, tt := range tests {
		got, err := KillCommand(tt.input)
		if tt.err && err == nil {
			t.Errorf("KillCommand(%q) 期望报错但没有", tt.input)
		}
		if !tt.err && err != nil {
			t.Errorf("KillCommand(%q) 期望通过但报错: %v", tt.input, err)
		}
		if !tt.err && got != tt.want {
			t.Errorf("KillCommand(%q) =\n%q\n期望\n%q", tt.input, got, tt.want)
		}
	}
}

func TestCaptureCommand(t *testing.T) {
	tests := []struct {
		name  string
		input string
		lines int
		want  string
		err   bool
	}{
		{"普通 10行", "ab", 10, "tmux -u capture-pane -p -J -t '=ab:' -S -10", false},
		{"含单引号 100行", "a'b", 100, "tmux -u capture-pane -p -J -t '=a" + "'\\''" + "b:' -S -100", false},
		{"中文 5000行", "中文名", 5000, "tmux -u capture-pane -p -J -t '=中文名:' -S -5000", false},
		{"行数为0", "ab", 0, "", true},
		{"行数5001", "ab", 5001, "", true},
		{"空会话名", "", 10, "", true},
	}
	for _, tt := range tests {
		got, err := CaptureCommand(tt.input, tt.lines)
		if tt.err && err == nil {
			t.Errorf("CaptureCommand(%q, %d) 期望报错但没有", tt.input, tt.lines)
		}
		if !tt.err && err != nil {
			t.Errorf("CaptureCommand(%q, %d) 期望通过但报错: %v", tt.input, tt.lines, err)
		}
		if !tt.err && got != tt.want {
			t.Errorf("CaptureCommand(%q, %d) =\n%q\n期望\n%q", tt.input, tt.lines, got, tt.want)
		}
	}
}

func TestParseList(t *testing.T) {
	// 真实 tmux 输出样例（字段间是制表符）
	base := "0 1 1791761526 1791761526 ab c\n"

	tests := []struct {
		name  string
		input string
		want  []Session
		err   bool
	}{
		{
			name:  "正常单行",
			input: base,
			want: []Session{
				{Name: "ab c", Attached: 0, Windows: 1,
					Created: time.Unix(1791761526, 0), Activity: time.Unix(1791761526, 0)},
			},
			err: false,
		},
		{
			name:  "正常多行",
			input: "1 3 1791761526 1791761526 main\n0 1 1791761530 1791761530 ab c\n",
			want: []Session{
				{Name: "main", Attached: 1, Windows: 3,
					Created: time.Unix(1791761526, 0), Activity: time.Unix(1791761526, 0)},
				{Name: "ab c", Attached: 0, Windows: 1,
					Created: time.Unix(1791761530, 0), Activity: time.Unix(1791761530, 0)},
			},
			err: false,
		},
		{
			name:  "空输出",
			input: "",
			want:  []Session{},
			err:   false,
		},
		{
			name:  "\r\n 换行",
			input: "0 1 1791761526 1791761526 ab c\r\n",
			want: []Session{
				{Name: "ab c", Attached: 0, Windows: 1,
					Created: time.Unix(1791761526, 0), Activity: time.Unix(1791761526, 0)},
			},
			err: false,
		},
		{
			name:  "名字含空格",
			input: "0 1 1791761526 1791761526 my session name\n",
			want: []Session{
				{Name: "my session name", Attached: 0, Windows: 1,
					Created: time.Unix(1791761526, 0), Activity: time.Unix(1791761526, 0)},
			},
			err: false,
		},
		{
			name:  "名字含制表符",
			input: "0 1 1791761526 1791761526 names\twith\ttabs\n",
			want: []Session{
				{Name: "names\twith\ttabs", Attached: 0, Windows: 1,
					Created: time.Unix(1791761526, 0), Activity: time.Unix(1791761526, 0)},
			},
			err: false,
		},
		{
			name:  "名字首尾空格保留",
			input: "0 1 1791761526 1791761526  ab \n",
			want: []Session{
				{Name: " ab ", Attached: 0, Windows: 1,
					Created: time.Unix(1791761526, 0), Activity: time.Unix(1791761526, 0)},
			},
			err: false,
		},
		{
			name:  "字段数不够（仅4段，缺会话名）",
			input: "0 1 1791761526 abc",
			want:  nil,
			err:   true,
		},
		{
			name:  "字段数不够（仅3个字段）",
			input: "0 1 1791761526\n",
			want:  nil,
			err:   true,
		},
		{
			name:  "attached 不是整数",
			input: "x 1 1791761526 1791761526 ab\n",
			want:  nil,
			err:   true,
		},
		{
			name:  "created 不是整数",
			input: "0 1 x 1791761526 ab\n",
			want:  nil,
			err:   true,
		},
	}

	for _, tt := range tests {
		got, err := ParseList([]byte(tt.input))
		if tt.err && err == nil {
			t.Errorf("ParseList(%q) 期望报错但没有", tt.name)
		}
		if !tt.err && err != nil {
			t.Errorf("ParseList(%q) 期望通过但报错: %v", tt.name, err)
		}
		if !tt.err {
			if len(got) != len(tt.want) {
				t.Errorf("ParseList(%q) 返回 %d 个会话，期望 %d 个", tt.name, len(got), len(tt.want))
				continue
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("ParseList(%q) 第 %d 行: got %+v, want %+v",
						tt.name, i, got[i], tt.want[i])
				}
			}
		}
	}
}

func TestParseList_空白行(t *testing.T) {
	// 多行输出末尾有换行，Split 会产生空行
	input := "0 1 1791761526 1791761526 ab\n\n"
	got, err := ParseList([]byte(input))
	if err != nil {
		t.Errorf("ParseList 应跳过空白行，但报错: %v", err)
	}
	if len(got) != 1 || got[0].Name != "ab" {
		t.Errorf("ParseList 应返回 1 个会话，实际: %+v", got)
	}
}

func TestNoServer(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"no server running on /tmp/tmux-1000/default", true},
		{"error connecting to /tmp/tmux-1000/default (No such file or directory)", true},
		{"正常输出", false},
		{"空字符串", false},
	}
	for _, tt := range tests {
		got := NoServer(tt.input)
		if got != tt.want {
			t.Errorf("NoServer(%q) = %v, 期望 %v", tt.input, got, tt.want)
		}
	}
}

func TestNotFound(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"can't find session: ab", true},
		{"正常输出", false},
		{"duplicate session: ab", false},
	}
	for _, tt := range tests {
		got := NotFound(tt.input)
		if got != tt.want {
			t.Errorf("NotFound(%q) = %v, 期望 %v", tt.input, got, tt.want)
		}
	}
}

func TestDuplicate(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"duplicate session: abc", true},
		{"正常输出", false},
		{"can't find session: abc", false},
	}
	for _, tt := range tests {
		got := Duplicate(tt.input)
		if got != tt.want {
			t.Errorf("Duplicate(%q) = %v, 期望 %v", tt.input, got, tt.want)
		}
	}
}
