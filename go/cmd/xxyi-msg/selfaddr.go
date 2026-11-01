package main

// 找自己住在哪个 tmux 会话里的兜底（state 钩子用）。
// Codex 之类的程序跑钩子时不传会话环境变量（TMUX、TMUX_PANE 都没有），
// 只能沿进程树往上爬：自己、父进程、祖父进程……
// 哪一级正好是 `tmux list-panes` 报出来的 pane_pid，就住在哪个会话里。
// parentPID/listPanes 是包级函数变量：测试里一律替换，绝不真执行 tmux。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// maxProcTreeDepth 是进程树上溯的最大层数：到顶也不是祖先了（也防 pid 复用造出的环）。
const maxProcTreeDepth = 30

// parentPID 取某进程的父进程 pid；做成包级变量便于测试替换。
var parentPID = defaultParentPID

// defaultParentPID：Linux 读 /proc/<pid>/stat；其它（macOS）用 ps -o ppid=。
func defaultParentPID(pid int) (int, error) {
	if pid < 1 {
		return 0, fmt.Errorf("非法 pid %d", pid)
	}
	if runtime.GOOS != "linux" {
		ctx, cancel := context.WithTimeout(context.Background(), tmuxTimeout)
		defer cancel()
		out, err := exec.CommandContext(ctx, "ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			return 0, err
		}
		ppid, err := strconv.Atoi(strings.TrimSpace(string(out)))
		if err != nil {
			return 0, fmt.Errorf("ps 输出的父进程号看不懂：%q", strings.TrimSpace(string(out)))
		}
		return ppid, nil
	}
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, err
	}
	return parentPIDFromProcStat(raw)
}

// parentPIDFromProcStat 解析 /proc/<pid>/stat 里的父进程字段（第 4 个）。
// 第 2 个字段是进程名，自带括号、名字里还能有空格和括号（如 "(kworker/0:1-events)"、"(my (weird) proc)"），
// 所以按最后一个 ')' 切开再数：')' 之后第一个是状态（第 3 字段），第二个才是父 pid（第 4 字段）。
func parentPIDFromProcStat(raw []byte) (int, error) {
	s := string(raw)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, errors.New("stat 行里没有进程名括号")
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 2 {
		return 0, fmt.Errorf("stat 行括号之后的字段不全：%q", s)
	}
	return strconv.Atoi(f[1])
}

// tmuxExtraPaths 是 PATH 之外再试的常见安装位置：钩子环境的 PATH 可能很干净（macOS 的 homebrew 首当其冲）。
var tmuxExtraPaths = []string{"/opt/homebrew/bin/tmux", "/usr/local/bin/tmux", "/usr/bin/tmux"}

// lookupTmux 找 tmux 可执行文件：先 PATH，再常见安装位置。
func lookupTmux() (string, error) {
	if p, err := exec.LookPath("tmux"); err == nil {
		return p, nil
	}
	for _, p := range tmuxExtraPaths {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", errors.New("找不到 tmux")
}

// listPanes 列全部 pane 的 "pane_pid 会话名"；做成包级变量便于测试替换，绝不真执行 tmux。
var listPanes = defaultListPanes

func defaultListPanes() (string, error) {
	tmux, err := lookupTmux()
	if err != nil {
		return "", err
	}
	// 默认 tmux 实例之外，还要查 XXYI 在 Mac 上建本机会话用的独立实例（tmux -L xxyi）：
	// Codex 起的子进程清掉了 TMUX，只能靠这里找到自己住在哪个会话。
	var all strings.Builder
	var firstErr error
	for _, extra := range [][]string{nil, {"-L", xxyiTmuxSocket}} {
		ctx, cancel := context.WithTimeout(context.Background(), tmuxTimeout)
		args := append(append([]string{}, extra...), "list-panes", "-a", "-F", "#{pane_pid} #{session_name}")
		out, err := exec.CommandContext(ctx, tmux, args...).Output()
		cancel()
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		all.Write(out)
	}
	if all.Len() == 0 && firstErr != nil {
		return "", firstErr
	}
	return all.String(), nil
}

// xxyiTmuxSocket 是 XXYI 在 Mac 上建本机会话用的 tmux 实例名（与 XxyiCore.tmuxSocket 一致）。
const xxyiTmuxSocket = "xxyi"

// paneSessionsOf 把 list-panes 的输出解析成 pane_pid→会话名；解析不了的行跳过。
// 会话名按"第一个空格之后的全部"取（带空格的怪名字留着让 checkSessionName 去拒，不在这里截断）。
func paneSessionsOf(out string) map[int]string {
	m := map[int]string{}
	for _, line := range strings.Split(out, "\n") {
		pidStr, name, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok || pidStr == "" {
			continue
		}
		pid, err := strconv.Atoi(pidStr)
		if err != nil || pid < 1 {
			continue
		}
		m[pid] = strings.TrimSpace(name)
	}
	return m
}

// findPaneSession 从 pid 起沿进程树上溯（含自身），返回第一个命中 panes 的会话名；
// 到 pid 1 还没命中就停（返回空串，不是错误）。取父进程失败原样返回错误。
func findPaneSession(pid int, panes map[int]string) (string, error) {
	for depth := 0; depth < maxProcTreeDepth; depth++ {
		if name, ok := panes[pid]; ok {
			return name, nil
		}
		if pid <= 1 {
			return "", nil
		}
		ppid, err := parentPID(pid)
		if err != nil {
			return "", err
		}
		pid = ppid
	}
	return "", nil
}

// procTreeSession 沿进程树找自己所在的 tmux 会话。
// 拿不到就回 errNoSession（带一步原因）。会话名已校验过（checkSessionName）。
func procTreeSession() (string, error) {
	out, err := listPanes()
	if err != nil {
		return "", fmt.Errorf("%w（列 tmux pane 失败：%v）", errNoSession, err)
	}
	panes := paneSessionsOf(out)
	if len(panes) == 0 {
		return "", errNoSession
	}
	name, err := findPaneSession(os.Getpid(), panes)
	if err != nil {
		return "", fmt.Errorf("%w（沿进程树找 tmux 会话失败：%v）", errNoSession, err)
	}
	if name == "" {
		return "", errNoSession
	}
	if err := checkSessionName(name); err != nil {
		return "", err
	}
	return name, nil
}
