#!/bin/bash
# macexec 任务流水线：Studio 空闲就按 QUEUE 顺序派给本地模型；每个任务做完 → 编译/测试 → 提交 →
# 后台起 Codex 只读评审（在独立 worktree 里，不占 Studio），同时下一个任务接着用 Studio。
# 出错（模型报错/用尽轮数、编译或测试不过）就停下等指挥处理。
set -u
REPO=/home/chenwy/Documents/codes/base-macexec
Q=$REPO/go/tasks-macexec
ST=$Q/state; mkdir -p "$ST"
log(){ echo "$(date '+%m-%d %H:%M:%S') $*" | tee -a "$ST/runner.log"; }
studio_free(){
  # 让路开关：存在 PAUSE 文件就不开新任务（入件等优先任务用 Studio 时由指挥创建）
  [ -f "$Q/PAUSE" ] && return 1
  # 只看本机有没有本地模型任务在跑；Mac 会话显示 busy 不代表在用 Studio（用户 10/16 指出），不再据此等待
  pgrep -f '/\.omlx-agent/omlx-agent ' >/dev/null && return 1
  return 0
}
review(){ # $1=id $2=base $3=commit；导出到临时目录评审，不建 git 工作区（工作区数量计入同步上限）
  local d=/tmp/claude-1000/macexec-rev-$1
  rm -rf "$d"; mkdir -p "$d"
  git -C "$REPO" archive "$3" | tar -x -C "$d" || { log "$1 导出失败，跳过评审"; return; }
  git -C "$REPO" diff "$2" "$3" > "$d/REVIEW.diff"
  ( cd "$d" && env -u TMUX -u TMUX_PANE timeout 1800 codex exec --dangerously-bypass-approvals-and-sandbox \
      -o "$ST/$1.review.md" "只读评审，不要修改任何文件。当前目录是改动后的代码快照，本次改动的 diff 在 REVIEW.diff。需求见 go/tasks-macexec/$1.md 与 go/tasks-macexec/CONTRACT.md、go/PLAN-macexec.md。重点：与约定是否一致、安全边界（不得任意命令/写文件、路径逃逸）、并发与超时、资源泄漏、错误处理。按严重程度列出问题（P1/P2/P3），每条给文件:行号、触发场景、建议修法；没有问题就写「无问题」。用中文。" >/dev/null 2>&1 )
  rm -rf "$d"
  log "$1 评审完成：$ST/$1.review.md"
}
next_id(){ local i; for i in $(cat "$Q/QUEUE"); do [ -f "$ST/$i.done" ] || { echo "$i"; return 0; }; done; return 1; }
while id=$(next_id); do
  until studio_free; do sleep 60; done
  base=$(git -C "$REPO" rev-parse HEAD)
  log "$id 开始（base $base）"
  ~/.omlx-agent/omlx-agent "$REPO" "按 go/tasks-macexec/$id.md 做（先读它，再读 go/tasks-macexec/CONTRACT.md）。不要 git commit。" > "$ST/$id.out" 2>&1
  rc=$?
  for k in 1 2; do  # 用尽轮数就接着同一会话做，最多两次
    grep -q 'subtype=error_max_turns' "$ST/$id.out" || break
    sid=$(grep -o 'session_id=[0-9a-f-]*' "$ST/$id.out" | tail -1 | cut -d= -f2)
    [ -n "$sid" ] || break
    log "$id 用尽轮数，第 $k 次接着做"
    ~/.omlx-agent/omlx-agent --resume "$sid" "$REPO" "接着把 go/tasks-macexec/$id.md 做完。已改的保留；先编译看还差什么。不要 git commit。" > "$ST/$id.out" 2>&1
    rc=$?
  done
  if grep -q 'is_error=True' "$ST/$id.out" || [ $rc -ne 0 ]; then log "$id 模型没做完（rc=$rc），停下"; exit 1; fi
  if grep -q "^# .*只读" "$Q/$id.md"; then touch "$ST/$id.done"; log "$id 调研完成：$ST/$id.out"; continue; fi
  ( cd "$REPO/go" && env -u TMUX -u TMUX_PANE go build ./... && env -u TMUX -u TMUX_PANE go vet ./macexec/ ./support/ ./cmd/xxyi-msg/ \
    && env -u TMUX -u TMUX_PANE go test ./macexec/ ./cmd/xxyi-msg/ && env -u TMUX -u TMUX_PANE go test ./support/ -run Exec ) > "$ST/$id.verify" 2>&1
  if [ $? -ne 0 ]; then log "$id 编译/测试不过，停下：$ST/$id.verify"; exit 1; fi
  git -C "$REPO" add -A && git -C "$REPO" commit -q -m "wip(macexec): $id（本地模型完成，待评审）

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01D3ZUfDjCvHzpd8EyoH6EPU"
  c=$(git -C "$REPO" rev-parse HEAD)
  touch "$ST/$id.done"; log "$id 已提交 $c，后台评审中"
  review "$id" "$base" "$c" &
done
wait
log "队列全部完成"
