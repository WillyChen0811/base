# 计划：Mac 协助服务（让 Ubuntu 上的 Claude 借 Mac 做测试）

## 目标
用户在 Mac 的 xxyi 里点「开始协助」（选时长），Mac 就主动经现有的隧道+SSH 连接，在 Ubuntu 上开一个只给本机用的入口。
Ubuntu 上的 Claude 通过这个入口：
1. 让 Mac 从 Ubuntu 同步代码（git，只允许配置里列出的仓库，只快进）；
2. 给 Mac 上的一个 Claude Code（无界面、开沙盒、权限保守）派任务，实时看它的输出；
3. Mac 上的 Claude Code 需要授权时，请求挂起，弹到 xxyi 里由用户点「允许/拒绝」——Ubuntu 这边只能看、不能批。
到点或用户点「停止」：入口关闭、Mac 上的 Claude Code 停掉、所有待批请求一律拒绝。

## 结构（全部在 base 的 go 模块，xxyi 只做界面）
- `go/support`（新包，无网络依赖、可单测）
  - `approval.go`：待批队列（加入并等待 / 列出 / 决定 / 全部取消）。
  - `hook.go`：解析 Claude Code 的 PermissionRequest 钩子输入、生成钩子输出。
  - `claude.go`：管一个长驻的 `claude -p --input-format stream-json --output-format stream-json --verbose` 进程：发消息、按序号取事件、状态、停止；进程退出后再发消息用 `--resume <会话号>` 重启。
  - `gitsync.go`：按分支同步仓库（工作区必须干净、只快进、分支名校验）。
  - `service.go`：把以上组装成三组 HTTP 接口（本机界面用、钩子用、远端用，各自令牌），加上时限、审计日志。
- `go/sshterm`：`Client.Listen`（在远端开监听，经 SSH 反向转发回来）；`internal/sshtest` 支持 tcpip-forward。
- `go/wgtund`：读 hosts.json 的 `support` 配置；在指定主机的共用连接上开反向监听；把远端入口地址和令牌写到远端 `~/.xxyi-support/endpoint.json`（600）；`wgtund support-hook` 子命令供 Claude Code 钩子调用。
- `go/cmd/macsup`（Ubuntu 端命令行）：`status`、`sync <仓库> <分支>`、`ask <任务>`（发出后持续打印输出直到这一轮结束）、`events`、`stop-claude`。
- xxyi：协助面板（开始/停止/倒计时/待批列表/允许拒绝/最近日志）。先做在网页控制台里，原生界面以后再换。

## 接口约定
本机（wgtund 主令牌，和现有接口同一套 CORS 规则）：
- `GET /support` → 状态：`{active, until, host, claude:{running,busy,session_id,error}, pending:[{id,tool,summary,created}]}`
- `POST /support/start?minutes=N`（1..240）、`POST /support/stop`、`POST /support/decide?id=&allow=1|0`
钩子（单独的钩子令牌，只能提交并等待，不能批）：
- `POST /support/hook`（正文=钩子收到的 JSON）→ 阻塞到用户决定或超时 → 返回钩子输出 JSON。协助没开时立刻拒绝。
远端（单独的远端令牌，只在反向监听上提供；不含 start/stop/decide）：
- `GET /status`、`POST /task`（正文=任务文字）、`GET /events?after=&wait=`、`POST /sync?repo=&branch=`、`POST /stop-claude`

## 安全要点
- Mac 主动连出，Mac 不开任何对外端口；远端监听只绑 Ubuntu 的 127.0.0.1，靠远端令牌。
- 批准只能在 Mac 本机界面做；钩子令牌、远端令牌都不能批。
- Mac 上 Claude Code：开沙盒、禁止不进沙盒的重试、沙盒起不来就退出；权限模式默认（不能是 bypass/auto）；预算上限。设置文件由用户维护，wgtund 只往里合并钩子。
- 钩子失败一律按拒绝处理（钩子配置 onFailure=block，子命令出错也输出拒绝）。
- 审计日志：任务、授权请求和决定、同步、开始/停止，都写进 `support-log.jsonl`。

## 任务与执行者
| # | 内容 | 执行 | 评审 |
|---|---|---|---|
| S1 | support/approval.go + hook.go + 测试 | 35B | Flash Next |
| S2 | support/gitsync.go + 测试（本地临时仓库） | 35B | Flash Next |
| S3 | support/claude.go + 测试（假 claude 子进程） | Flash Next | Sonnet |
| S4 | sshtest 支持反向转发 + sshterm.Client.Listen + 测试 | Flash Next | Sonnet |
| S5 | support/service.go：三组接口、时限、审计 + 测试 | Flash Next | Sonnet |
| S6 | wgtund 接线：配置、反向监听、端点文件、support-hook 子命令、README | Flash Next | Sonnet |
| S7 | cmd/macsup + 测试 | 35B | Flash Next |
| S8 | xxyi 协助面板 + 浏览器测试 | Flash Next | Sonnet |
| S9 | Ubuntu 上端到端联调（测试 sshd 当远端、假 claude） | 我 | — |
| S10 | Mac 实机验收 | 用户 + 我 | — |
每步：执行（含自写测试）→ 换模型 /code-review → 作者按意见改 → 我跑测试复核、提交。全部完成后 Sonnet 审整个分支，我终审。
