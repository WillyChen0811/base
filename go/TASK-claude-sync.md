# 任务：把「对话记录同步」并入项目同步 v2（工作目录 ~/Documents/codes/base-msg/go，分支 feat/msg）

## 背景
项目同步 v1 的 `projsyncSvc.syncClaude`（wgtund/projsync.go 约第 315 行）会用 `projsync.SyncDirs` 双向补齐两边的 Claude 对话目录（会话 jsonl、memory/），逻辑已完成并有测试，**不要改它，也不要改 projsync/files.go、projsync/fs.go 里的任何逻辑**。
v2 的一轮在 `projsyncV2Svc.execute`（wgtund/projsync_v2.go 约第 171 行）：调用 `s.round(ctx, s.env(t, force))` 得到 `projsync.RoundResult`，记入 s.last 并写状态文件。v2 目前**不同步对话记录**。我们要在 v2 每一轮的代码同步之后补做一次对话记录同步，之后 v1 才能下架。

## 要做的事（只这一项）
1. `projsync/round.go` 的 `RoundResult` 加两个字段（放在 Notes 之后，保持 json 标签风格）：
   - `Claude *FileResult `json:"claude,omitempty"``
   - `ClaudeError string `json:"claude_error,omitempty"``
2. `projsyncV2Svc` 加字段 `claudeSync func(ctx context.Context, t projsyncTarget) (*projsync.FileResult, error)`；`newProjsyncV2Svc` 里默认设为 `v1.syncClaude`（方法值），测试可替换。
3. `execute` 里，`s.round(...)` 返回后、记入结果之前：
   - 仅当 `!res.Skipped && !res.OutOfScope` 时才做对话记录同步（没拿到锁或超出范围的那一轮什么都不写，对话记录也不碰）。
   - 调 `s.claudeSync(ctx, t)`，**用 defer/recover 包住，任何 panic 都变成 ClaudeError，不能影响后面的流程**。
   - 成功：`res.Claude = fr`；失败：`res.ClaudeError = err.Error()`。
   - **对话记录同步失败/冲突绝对不能改变代码同步的结果**：不改 `res.Error`、`res.Stopped`、`res.NeedsAttention`、`res.Attention`、`res.Summary`。只往 `res.Notes` 追加一条中文说明：成功时形如「对话记录：拷到对方 N 个、拉回 M 个、覆盖 K 个、冲突 C 个」（字段名以 projsync/fs.go 的 FileResult 为准，先读它，写成能读懂的中文）；失败时「对话记录同步失败：<原因>（代码同步不受影响）」。
   - `s.claudeSync` 为 nil 时直接跳过（测试里 v1.pool 为 nil 的情况不要崩）。
4. 注意：对话记录同步用的 ctx 与代码同步共用 `execute` 里那个带超时的 ctx，不要另外无限等待。

## 禁止
- 不改 v1 的任何行为（/projsync 接口、syncClaude 本身）。
- 不改 SyncDirs 的规则。不要写删除文件的代码。
- 不 git commit / push / checkout / stash（权限会拦；做完留在工作区，由指挥提交）。
- 不要改别的文件、不要顺手重构。

## 测试（在 wgtund/projsync_v2_test.go 里加，沿用该文件已有的假 round / 假服务写法；先读已有测试学风格）
- 正常：假 round 返回普通结果、假 claudeSync 返回带若干计数的 FileResult → `last` 里的结果 `Claude` 非空、`Notes` 里有对话记录那一条、`ClaudeError` 为空。
- 失败不影响代码结果：假 claudeSync 返回 error → `ClaudeError` 有内容、`res.Error`/`NeedsAttention`/`Summary` 与不做对话记录时完全相同。
- panic：假 claudeSync panic → 不崩，`ClaudeError` 有内容，代码结果不变。
- 跳过：假 round 返回 `Skipped:true` → claudeSync **没被调用**；返回 `OutOfScope:true` → 同样没被调用。
- claudeSync 为 nil → 不崩，`Claude`/`ClaudeError` 都为空。
- 状态接口的 JSON 里能看到 `claude` 字段（序列化一次，断言包含）。

## 验证命令（要单独执行，不要用 && 拼接）
- `env -u TMUX -u TMUX_PANE go build ./...`
- `env -u TMUX -u TMUX_PANE go vet ./...`
- `env -u TMUX -u TMUX_PANE go test -count=1 ./wgtund/ ./projsync/`
- `gofmt -l .` 应无输出。
全部通过再结束。

## 怎么回复
用中文写：改了哪些文件、新增了哪些测试、四条验证命令的结果（贴最后几行输出）、你不确定或没做的地方。不确定就停下并说明，不要猜。
