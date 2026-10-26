package main

// 进程树找 tmux 会话的测试：parentPID/listPanes 一律注入替换，绝不真执行 tmux。

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// TestMain 把 HOME 统一指到临时目录：忙闲状态文件不许写进真实家目录；
// 同时让 procCmdline 默认读不到任何进程（认 agent 不去碰真进程），用例需要再自己注入。
// 需要不同家目录布局的用例再 t.Setenv("HOME", t.TempDir()) 覆盖。
func TestMain(m *testing.M) {
	procCmdline = func(int) (string, error) { return "", errors.New("测试里不读真进程命令行") }
	home, err := os.MkdirTemp("", "xxyi-msg-home")
	if err != nil {
		panic(err)
	}
	if err := os.Setenv("HOME", home); err != nil {
		panic(err)
	}
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

// useFakeTree 注入进程链与 pane 列表，测完还原。
func useFakeTree(t *testing.T, parents map[int]int, panes string) {
	t.Helper()
	oldPP, oldLP := parentPID, listPanes
	t.Cleanup(func() { parentPID, listPanes = oldPP, oldLP })
	parentPID = func(pid int) (int, error) {
		if p, ok := parents[pid]; ok {
			return p, nil
		}
		return 0, errors.New("不该问到这个 pid")
	}
	listPanes = func() (string, error) { return panes, nil }
}

func TestParentPIDFromProcStat(t *testing.T) {
	cases := []struct {
		name string
		stat string
		want int
		err  bool
	}{
		{"普通", "1234 (node) S 99 1234 1234 0 -1 0 100 0 0 0 5 0 0 0 20 0 1 0 555\n", 99, false},
		{"进程名带空格和括号", "42 (my (weird) proc) R 7 42 6 0 -1\n", 7, false},
		{"名字里全是括号", "9 ((kworker/0:1-events)) S 8\n", 8, false},
		{"括号后字段不全", "1234 (node) S\n", 0, true},
		{"没有括号", "1234 node S 99", 0, true},
		{"父 pid 不是数字", "1234 (node) S xx 1", 0, true},
	}
	for _, c := range cases {
		got, err := parentPIDFromProcStat([]byte(c.stat))
		if c.err {
			if err == nil {
				t.Errorf("%s：该报错，得到 %d", c.name, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%s：得到 %d,%v，want %d", c.name, got, err, c.want)
		}
	}
}

func TestFindPaneSessionWalk(t *testing.T) {
	oldPP := parentPID
	t.Cleanup(func() { parentPID = oldPP })
	parentPID = func(pid int) (int, error) {
		switch pid {
		case 100:
			return 90, nil
		case 90:
			return 80, nil
		case 80:
			return 1, nil
		}
		return 0, errors.New("链断了")
	}
	panes := map[int]string{80: "codex1", 55: "other"}
	name, err := findPaneSession(100, panes)
	if err != nil || name != "codex1" {
		t.Fatalf("沿 100→90→80 该命中 codex1，得到 %q,%v", name, err)
	}
	// 链上无命中：走到 init 停，空串不是错误
	name, err = findPaneSession(100, map[int]string{777: "nope"})
	if err != nil || name != "" {
		t.Fatalf("无命中该空串收场，得到 %q,%v", name, err)
	}
	// pid 环/无限上涨：到 maxProcTreeDepth 层必须自己停
	bumps := 0
	parentPID = func(pid int) (int, error) { bumps++; return pid + 1, nil }
	name, err = findPaneSession(100, map[int]string{999999: "远在天边"})
	if err != nil || name != "" {
		t.Fatalf("超层数该停，得到 %q,%v", name, err)
	}
	if bumps > maxProcTreeDepth {
		t.Errorf("上溯 %d 次，超过上限 %d", bumps, maxProcTreeDepth)
	}
	// 取父进程失败原样带出
	parentPID = func(int) (int, error) { return 0, errors.New("读不到") }
	if _, err := findPaneSession(100, panes); err == nil {
		t.Error("取父进程失败该报错")
	}
}

func TestPaneSessionsOf(t *testing.T) {
	m := paneSessionsOf("80 codex1\n55 other\n\n垃圾行\nabc x\n")
	if len(m) != 2 || m[80] != "codex1" || m[55] != "other" {
		t.Errorf("解析得到 %v", m)
	}
	// 会话名带空格：完整留着，让 checkSessionName 去拒
	m = paneSessionsOf("80 中文 会话\n")
	if m[80] != "中文 会话" {
		t.Errorf("带空格会话名 %q", m[80])
	}
}

// TMUX 为空：沿进程树命中 pane → 会话名。
func TestProcTreeSessionHit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMUX", "")
	me := os.Getpid()
	useFakeTree(t, map[int]int{me: 90, 90: 80, 80: 1}, "80 codex1\n55 other")
	name, err := procTreeSession()
	if err != nil || name != "codex1" {
		t.Fatalf("得到 %q,%v，want codex1", name, err)
	}
}

// 链上无命中：报 errNoSession。
func TestProcTreeSessionNoHit(t *testing.T) {
	me := os.Getpid()
	useFakeTree(t, map[int]int{me: 90, 90: 80, 80: 1}, "55 other\n77 second")
	if _, err := procTreeSession(); !errors.Is(err, errNoSession) {
		t.Fatalf("得到 %v，want errNoSession", err)
	}
	// 一个 pane 都列不出来：同样。
	useFakeTree(t, map[int]int{me: 1}, "")
	if _, err := procTreeSession(); !errors.Is(err, errNoSession) {
		t.Fatalf("空 pane 列表得到 %v，want errNoSession", err)
	}
}

// listPanes 失败：同样报错，原因带进错误里。
func TestProcTreeSessionListPanesFails(t *testing.T) {
	oldLP := listPanes
	t.Cleanup(func() { listPanes = oldLP })
	listPanes = func() (string, error) { return "", errors.New("找不到 tmux") }
	_, err := procTreeSession()
	if !errors.Is(err, errNoSession) || !strings.Contains(err.Error(), "找不到 tmux") {
		t.Errorf("得到 %v", err)
	}
}

// 取父进程失败：同样报错。
func TestProcTreeSessionParentFails(t *testing.T) {
	useFakeTree(t, map[int]int{}, "80 codex1\n") // 自己的父进程查不到
	if _, err := procTreeSession(); !errors.Is(err, errNoSession) {
		t.Errorf("得到 %v", err)
	}
}

// 兜底取到的会话名不合法：跟 TMUX 分支同一套文案。
func TestProcTreeSessionIllegalName(t *testing.T) {
	me := os.Getpid()
	useFakeTree(t, map[int]int{me: 80, 80: 1}, "80 中文 会话\n")
	_, err := procTreeSession()
	if err == nil || !strings.Contains(err.Error(), "tmux rename-session") {
		t.Errorf("得到 %v，该提示改名", err)
	}
}
