package projsync

import "testing"

func TestClaudeDirName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/Users/weiyichen/Documents/codes/ai-browser", "-Users-weiyichen-Documents-codes-ai-browser"},
		{"/tmp/claude-1000/x_y.z", "-tmp-claude-1000-x-y-z"},
		{"中文", "--"}, // 中文字符每个 rune 换成一个 -
		{"a/b", "a-b"},
	}
	for _, c := range cases {
		if got := ClaudeDirName(c.in); got != c.want {
			t.Errorf("ClaudeDirName(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}
