# T1 Mac 端 /exec/* 骨架（base 仓库 feat/macexec，Go）

先读 `go/PLAN-macexec.md`（总体设计与安全边界），再读 `go/support/service.go`（remoteMux、guard、logEvent）、`go/support/msgapi.go`（mountMsgRemote 的写法可照抄）。

## 做
新文件 `go/support/macexec.go`，在 `remoteMux` 里 `s.mountExecRemote(mux)` 挂上：
- `POST /exec/start {action, repo?, path?, offset?, limit?, name?, lines?}` → `{job}`；同一时间只允许一个 running 的 job，否则 409「Mac 上正有任务在跑」。
- `GET /exec/job?id=` → `{id, action, state: running|done|failed|timeout, exit_code, started, ended, tail}`（tail ≤ 200 行）。
- job 输出完整写 `~/.xxyi/macexec/<job>.log`（目录用 HOME 现取；只保留最近 50 个，旧的删）；job 元数据也落盘（`<job>.json`），进程重启后 `/exec/job` 仍能查到已结束的 job。
- 本任务实现四个动作：
  - `git_status {repo}`：分支、HEAD、是否干净（`git status --porcelain` 行数）。
  - `read_file {repo, path, offset, limit}`：按行读，上限 256KB，非 UTF-8 报错。
  - `logs {name, lines}`：name 只认 `service`（~/.xxyi/service.log）、`wgtund`、`ui`（实际路径先在代码/现有日志里查清楚，查不到的名字返回 400 并列出可用名字）；lines ≤ 500。
  - `job`（即 GET /exec/job）。
- 仓库清单：只认 `~/xxyi-work/<名字>`，名字取项目同步 v2 的项目清单（在 wgtund/projsync_v2.go 里找现成的读取函数；拿不到就先写死 xxyi、base、websubmit、ai-browser，并在汇报里说明）。
- 路径校验写成独立函数：拒绝空、绝对路径、`..`、`.git` 段、解析符号链接后不在仓库内的。
- 每个动作有超时（git_status 30s、read_file/logs 10s），用 exec.CommandContext；只用固定命令和参数，不经 shell。
- 每次 start 与结束都 `s.logEvent("exec", ...)`（动作名、仓库、结果，不记文件内容）。
- 写纯函数便于测试：路径校验、tail 截取、日志轮转选择要删的文件。

## 不做
不加任意命令执行、不加写文件；不碰编译/打包/截图/重启（后续任务）；不改 Ubuntu 端 xxyi-msg。

## 验收
`cd go && env -u TMUX -u TMUX_PANE go build ./... && env -u TMUX -u TMUX_PANE go vet ./support/`。本任务只写实现，测试另起任务。不要 git commit。汇报改了哪些文件、接口请求/返回示例、拿不准的地方。
