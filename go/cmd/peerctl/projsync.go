package main

// projsync-facts / projsync-apply：项目同步的远程协议子命令（Mac 的 wgtund 经 SSH 调用）。
// stdin 读一个 JSON 请求，stdout 写一个 JSON 响应；退出码 0 = 协议层成功（业务失败在响应里），
// 非 0 = 协议层失败（输入坏了、内部错误），此时 stdout 无输出、原因写 stderr。
// 不读环境里的其它配置，不访问网络。

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/WillyChen0811/base/go/agentstate"
	"github.com/WillyChen0811/base/go/projsync"
)

// judgeAgent 用 agentstate 判定工作区里 agent 的状态（默认选项）；失败按 unknown 报。
func judgeAgent(ctx context.Context, worktree string) projsync.AgentVerdict {
	r, err := agentstate.JudgeDetail(ctx, worktree, agentstate.Options{})
	av := projsync.AgentVerdict{Verdict: string(r.Verdict), Notes: r.Notes}
	if err != nil {
		av.Verdict = string(agentstate.VerdictUnknown)
		av.Notes = append(av.Notes, "判定失败: "+err.Error())
	}
	return av
}

// judgeFinished 供自动提交用：agentstate 判定为 Finished 才算做完；判定失败按未做完。why 带判定依据。
func judgeFinished(ctx context.Context, worktree string) (bool, string) {
	r, err := agentstate.JudgeDetail(ctx, worktree, agentstate.Options{})
	if err != nil {
		return false, "判定失败: " + err.Error()
	}
	return r.Verdict == agentstate.VerdictFinished, fmt.Sprintf("agent 状态 %s；%s", r.Verdict, strings.Join(r.Notes, "；"))
}

func cmdProjsync(sub string, stdin io.Reader, stdout io.Writer) error {
	in, err := io.ReadAll(io.LimitReader(stdin, projsync.MaxRemoteInput+1))
	if err != nil {
		return fmt.Errorf("读请求失败: %w", err)
	}
	if len(in) > projsync.MaxRemoteInput {
		return fmt.Errorf("请求超过 %d 字节上限", projsync.MaxRemoteInput)
	}
	var out []byte
	if sub == projsync.SubcmdFacts {
		out, err = projsync.ServeFacts(context.Background(), in, judgeAgent)
	} else {
		out, err = projsync.ServeApplyWith(context.Background(), in, judgeFinished)
	}
	if err != nil {
		return err
	}
	_, err = stdout.Write(append(out, '\n'))
	return err
}
