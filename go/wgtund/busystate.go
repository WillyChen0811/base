package wgtund

// 会话忙闲灯：远端（Ubuntu）上 Claude/Codex 的钩子只往本机 ~/.peerctl-state/<tmux 会话名>.json 写文件，
// 不联网（见 cmd/peerctl/busystate.go，目录名与文件格式两边要一致）。
// /sessions 列完 tmux 会话后，在同一条 SSH 连接上再跑一条命令把这些文件读回来，给每个会话挂上 busy / busy_time。
// 读状态是锦上添花：命令失败、超时、文件坏了都只是少一个灯，绝不让 /sessions 失败。

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/WillyChen0811/base/go/tmux"
)

// busyReadTimeout 是读状态命令的时限：/sessions 要快，读不到就算了。
const busyReadTimeout = 3 * time.Second

// maxBusyFileBytes 是每个状态文件最多读多少字节：正常只有几十字节，防有人塞大文件撑爆输出。
const maxBusyFileBytes = 2048

// busyReadScript 在远端的 sh 里跑：每个 ~/.peerctl-state/*.json 输出一行 "文件名<TAB>内容"（内容里的换行去掉）。
// 目录不存在、没有匹配的文件、单个文件读不了都直接跳过；总是退出 0。写成一行，免得换行在各种登录 shell 里出岔子。
// 文件名由 peerctl 按 tmux 会话名生成（只含字母数字 . _ -），不会有制表符。
const busyReadScript = `d="$HOME/.peerctl-state"; for f in "$d"/*.json; do [ -f "$f" ] || continue; printf '%s\t' "${f##*/}"; head -c 2048 "$f" 2>/dev/null | tr -d '\r\n'; echo; done; exit 0`

// busyReadCommand 是 pool.run 要执行的完整命令（经 SSH 交给远端登录 shell，再由 sh -c 执行脚本）。
func busyReadCommand() string {
	return "sh -c " + tmux.Quote(busyReadScript)
}

// busyInfo 是一个会话的忙闲：State 是 "busy" 或 "idle"，Time 是钩子写入时的 unix 秒。
type busyInfo struct {
	State string
	Time  int64
}

// parseBusyStates 解析 busyReadCommand 的输出：会话名 → 忙闲。
// 坏行（没有制表符、不是 .json、JSON 坏了、state 不是 busy/idle）一律跳过。
func parseBusyStates(out []byte) map[string]busyInfo {
	m := map[string]busyInfo{}
	for _, line := range strings.Split(string(out), "\n") {
		file, content, ok := strings.Cut(strings.TrimRight(line, "\r"), "\t")
		if !ok {
			continue
		}
		name, ok := strings.CutSuffix(file, ".json")
		if !ok || name == "" || len(content) > maxBusyFileBytes {
			continue
		}
		var rec struct {
			State string `json:"state"`
			Time  int64  `json:"time"`
		}
		if json.Unmarshal([]byte(content), &rec) != nil {
			continue
		}
		if rec.State != "busy" && rec.State != "idle" {
			continue
		}
		m[name] = busyInfo{State: rec.State, Time: rec.Time}
	}
	return m
}

// readBusyStates 经连接池读远端的忙闲状态；任何失败都返回空表（调用方照常出列表）。
func readBusyStates(ctx context.Context, pool *sshPool, host string) map[string]busyInfo {
	ctx, cancel := context.WithTimeout(ctx, busyReadTimeout)
	defer cancel()
	out, err := pool.run(ctx, host, busyReadCommand())
	if err != nil {
		return map[string]busyInfo{}
	}
	return parseBusyStates(out)
}
