package main

// 忙闲灯：Claude/Codex 的钩子（UserPromptSubmit/Stop/SessionStart/SessionEnd）跑
// `peerctl state busy|idle|gone`，这里只把状态写进本机文件 ~/.peerctl-state/<tmux 会话名>.json，
// 永不联网——XXYI 不在线时钩子照样安静地跑完。Mac 一侧的 wgtund 列 tmux 会话时顺带经 SSH 读这些文件
// （见 wgtund/busystate.go，目录名与文件格式两边要一致）。
//
// 文件内容是一行 JSON：{"state":"busy|idle","time":<unix 秒>,"agent":"claude|codex"}，
// agent 认不出就不写这个字段。目录 700、文件 600，先写临时文件再 rename，读的人不会读到半截。
// gone 删掉文件（不存在不算错）。认不出自己住在哪个 tmux 会话（不在 tmux 里）就什么都不写。
//
// 钩子语义：任何情况都退出 0，标准输出一个字也不打（Claude 会把钩子的标准输出当钩子响应）。
// 命令行参数与旧版兼容：钩子配置里的 `idle --check`、`idle --start` 照收；
// --check 以前会向服务端查欠回复并拦住 Stop，消息系统拆除后不再检查、不拦。

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// 忙闲状态字面量。
const (
	stateBusy = "busy"
	stateIdle = "idle"
	stateGone = "gone"
)

// busyStateDirName 是状态目录名（放在家目录下）；wgtund 读状态时用同一个名字。
const busyStateDirName = ".peerctl-state"

// busyRecord 是状态文件的内容。
type busyRecord struct {
	State string `json:"state"`
	Time  int64  `json:"time"`
	Agent string `json:"agent,omitempty"`
}

// sessionSourceCompact 是 SessionStart 钩子 JSON 的 source 之一：上下文压缩触发的"重新开始"。
const sessionSourceCompact = "compact"

// sessionStartSource 读 SessionStart 钩子塞进来的 JSON，取 source。
// goroutine 里限时读（钩子不关标准输入也拖不住这条命令），读不到返回空串（照常写状态）。
func sessionStartSource(ctx context.Context, stdin io.Reader) string {
	readCtx, cancel := context.WithTimeout(ctx, stopHookWait)
	defer cancel()
	ch := make(chan string, 1)
	go func() {
		var h struct {
			Source string `json:"source"`
		}
		if json.NewDecoder(stdin).Decode(&h) != nil {
			ch <- ""
			return
		}
		ch <- h.Source
	}()
	select {
	case got := <-ch:
		return got
	case <-readCtx.Done():
		return ""
	}
}

// selfSessionName 取自己所在的 tmux 会话名：在 tmux 里（TMUX 变量）问 tmux，
// 否则（Codex 跑钩子不带环境变量）沿进程树找。不看 XXYI_ADDR：状态文件按 tmux 会话名对号入座。
func selfSessionName() (string, error) {
	if os.Getenv("TMUX") != "" {
		return tmuxEnvSession()
	}
	return procTreeSession()
}

// procCmdline 取某进程的命令行（参数以空格连接）；包级变量便于测试替换。
var procCmdline = defaultProcCmdline

func defaultProcCmdline(pid int) (string, error) {
	if pid < 1 {
		return "", errors.New("非法 pid")
	}
	if runtime.GOOS != "linux" {
		ctx, cancel := context.WithTimeout(context.Background(), tmuxTimeout)
		defer cancel()
		out, err := exec.CommandContext(ctx, "ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
		return strings.TrimSpace(string(out)), err
	}
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(strings.ReplaceAll(string(raw), "\x00", " ")), nil
}

// scriptRunners 是能把脚本当第一个参数跑的解释器：claude/codex 用 npm 装的时候命令行是 `node …/codex.js`。
var scriptRunners = map[string]bool{"node": true, "nodejs": true, "bun": true, "deno": true}

// agentOfCmdline 判断命令行属于哪种 agent："claude"、"codex" 或 ""。
// 认三种写法：可执行文件本身叫 claude/codex；解释器（node 等）跑的脚本叫 claude(.js)/codex(.js)；
// 命令行里带 npm 包名（@anthropic-ai/claude-code、@openai/codex）。
// `vim claude`、`bash -lc claude` 这类不算；同时命中两种就不认。
func agentOfCmdline(cmdline string) string {
	args := strings.Fields(cmdline)
	is := func(name, pkg string) bool {
		if strings.Contains(cmdline, pkg) {
			return true
		}
		if len(args) == 0 {
			return false
		}
		base := func(a string) string {
			b := filepath.Base(a)
			for _, ext := range []string{".js", ".mjs", ".cjs"} {
				b = strings.TrimSuffix(b, ext)
			}
			return b
		}
		if filepath.Base(args[0]) == name {
			return true
		}
		return len(args) > 1 && scriptRunners[filepath.Base(args[0])] && base(args[1]) == name
	}
	c, x := is("claude", "@anthropic-ai/claude-code"), is("codex", "@openai/codex")
	switch {
	case c && !x:
		return "claude"
	case x && !c:
		return "codex"
	}
	return ""
}

// selfAgent 沿进程树往上找最近的 claude/codex 进程；找不到再看 Claude Code 给钩子设的环境变量。
// 认不出返回空串（状态文件就不写 agent 字段）。
func selfAgent() string {
	pid := os.Getpid()
	for depth := 0; depth < maxProcTreeDepth && pid > 1; depth++ {
		if cl, err := procCmdline(pid); err == nil {
			if k := agentOfCmdline(cl); k != "" {
				return k
			}
		}
		ppid, err := parentPID(pid)
		if err != nil {
			break
		}
		pid = ppid
	}
	if os.Getenv("CLAUDECODE") != "" || os.Getenv("CLAUDE_PROJECT_DIR") != "" {
		return "claude"
	}
	return ""
}

// busyStateDir 是状态目录的路径。
func busyStateDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", errors.New("找不到家目录")
	}
	return filepath.Join(home, busyStateDirName), nil
}

// writeBusyState 原子写 <dir>/<session>.json：目录 700、文件 600，先写同目录临时文件再 rename。
func writeBusyState(dir, session string, rec busyRecord) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*") // 0600；名字不以 .json 结尾，读的人不会当成状态文件
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	_, werr := tmp.Write(raw)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		os.Remove(tmpName)
		if werr != nil {
			return werr
		}
		return cerr
	}
	if err := os.Rename(tmpName, filepath.Join(dir, session+".json")); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// cmdState 给 Claude/Codex 钩子用：只写本机状态文件，任何错误都静默退出 0，标准输出不打任何东西。
// "--start"（SessionStart 钩子）读标准输入取 source：compact 是压缩中途，Claude 正在干活，什么都不做；
// 其余（startup/resume/clear/读不到）照常写。其余情况绝不碰标准输入，免得等 EOF 卡住钩子。
func cmdState(args []string, stdin io.Reader, _ io.Writer) int {
	state := ""
	start := false
	for _, a := range args {
		switch a {
		case "--check":
			// 旧版在这里向服务端查欠回复并拦住 Stop；消息系统拆除后照收参数、不检查、不拦。
		case "--start":
			start = true
		case stateBusy, stateIdle, stateGone:
			if state != "" {
				return exitOK // 给了两个状态值：当参数不合法处理
			}
			state = a
		default:
			return exitOK
		}
	}
	if state == "" {
		return exitOK
	}
	if start {
		ctx, cancel := context.WithTimeout(context.Background(), stateDeadline)
		defer cancel()
		if sessionStartSource(ctx, stdin) == sessionSourceCompact {
			return exitOK // 压缩触发的 SessionStart：不改状态，免得把干活中的 Claude 记成空闲
		}
	}
	session, err := selfSessionName()
	if err != nil {
		return exitOK // 不在 tmux 里：没处可写
	}
	dir, err := busyStateDir()
	if err != nil {
		return exitOK
	}
	if state == stateGone {
		_ = os.Remove(filepath.Join(dir, session+".json")) // 不存在不算错
		return exitOK
	}
	_ = writeBusyState(dir, session, busyRecord{State: state, Time: time.Now().Unix(), Agent: selfAgent()})
	return exitOK
}
