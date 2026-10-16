# Mac 执行器（macexec）：让 Ubuntu 上的 Claude 在 Mac 上编译、测试、打包、运行、截图

分支：base 仓库 `feat/macexec`（从 feat/msg 4ae7791 开出），工作区 `~/Documents/codes/base-macexec`。指挥：ubuntu:websubmit（用户 10/16 指定为总指挥）。

## 目标
Ubuntu 上的 Claude 写 Mac 界面代码时，自己能在 Mac 上：同步代码 → swift build → 自检 → 打包 → 重启 XXYI.app → 截图看界面 → 看日志，
不用再靠消息转给 Mac 上的会话。思考和 tokens 都在 Ubuntu，Mac 只是执行器。

## 结构（复用现有通道，不新开端口）
- **Mac 端**：wgtund 协助服务的远端接口（`support/service.go` 的 `remoteMux`，已有令牌校验、只在「协助」开着时可用）里加 `/exec/*`。
- **Ubuntu 端**：`cmd/xxyi-msg` 的 MCP 服务（就是 Claude 现在用的 `xxyi_*` 工具那个）加 `mac_*` 工具，经同一个远端地址和令牌调用 `/exec/*`。
- **授权**：每个 `mac_*` 工具在 Claude Code 里是独立工具，按工具设允许/每次问；"每次问"走现有授权钩子到 XXYI 由用户点。

## 安全边界（硬规则）
- **不提供任意命令执行**，也**不提供写文件**。只有下面固定的动作，参数只是选项（仓库名、文件名、行数），由 Mac 端拼命令。
- 改代码的路径永远是：Ubuntu 改 → git 提交 → 同步到 Mac（`/sync` 或项目同步）→ Mac 上编译。Mac 工作区保持干净。
- 仓库只认 `~/xxyi-work/` 下已登记的项目（与项目同步同一份清单）；路径参数一律校验在仓库内，拒绝 `..`、绝对路径、符号链接逃逸、`.git/`。
- 同一时间只跑一个 exec 任务，其余返回 409「Mac 上正有任务在跑」。
- 每个动作有超时与输出上限；完整输出存 `~/.xxyi/macexec/<job>.log`（保留最近 50 个），接口只回尾部。
- 每次调用写进协助日志（`logEvent("exec", …)`），用户在 XXYI 里看得到。

## 动作清单（第一版）
| 动作 | Mac 上做什么 | 超时 |
|---|---|---|
| `git_status` | 仓库当前分支、HEAD、是否干净、与 Ubuntu 侧同名分支是否一致 | 30s |
| `read_file` | 读仓库内文本文件（offset/limit 按行，上限 256KB） | 10s |
| `swift_build` | `swift build`（xxyi/mac） | 15min |
| `selftest` | `swift run xxyi-selftest` | 15min |
| `node_test` | `node tests/js/<名字>.test.js`（只认 tests/js 下已有文件） | 5min |
| `build_app` | `./build.sh`（xxyi/mac） | 20min |
| `install_app` | 退出 XXYI → ditto `build/XXYI.app` 覆盖 `~/Desktop/XXYI.app` → 校验签名 → 打开（见下"自重启"） | 2min |
| `screenshot` | `screencapture -x` 整屏或 XXYI 主窗口，`sips -Z 1600` 缩小，返回 PNG | 30s |
| `logs` | `~/.xxyi/service.log`、wgtund 日志、XXYI 界面日志的尾部 N 行（N ≤ 500） | 10s |
| `job` | 查某个任务的状态与输出尾部 | — |
（同步代码沿用现有 `/sync`。）

## 自重启问题（必须先解决）
wgtund 是 XXYI.app 拉起的；`install_app` 退出 XXYI 会连带杀掉 wgtund，远端通道随之断开。要求：
1. `install_app` 由 wgtund 起一个**脱离进程组**的小脚本执行（`setsid`/`nohup`，父进程退出不受影响），接口立即返回 job id。
2. XXYI 重开、wgtund 重启后，**若重启前协助开着且未到期，协助自动恢复**（同一令牌、同一到期时间，从磁盘恢复会话状态），Ubuntu 侧不需要用户再点开始。
3. 脚本结果写进 `~/.xxyi/macexec/<job>.log`，wgtund 重启后 `job` 能查到最终结果。
先做调研（任务 T3a）：现在协助会话的状态存在哪、wgtund 重启后会怎样，汇报后再定实现。

## Ubuntu 端 MCP 工具
`mac_git_status`、`mac_read_file`、`mac_build`、`mac_selftest`、`mac_node_test`、`mac_build_app`、`mac_install_app`、`mac_screenshot`（返回 MCP image 内容，Claude 能直接看图）、`mac_logs`、`mac_job`。
长任务：工具内部轮询最多 9 分钟，还没完就返回 job id，用 `mac_job` 接着查。

## 以后（不在第一版）
模拟点击/按键/拖放（macOS 辅助功能），让 Claude 自己验证交互。

## 任务拆分（每个任务 ≤20 分钟，实现与测试分开）
- T1 Mac 端 `/exec/*` 骨架：job 注册、单任务锁、超时、输出上限与日志、`git_status`/`read_file`/`logs`/`job` 四个动作 + 路径校验。
- T2 T1 的测试。
- T3a 调研协助会话重启后的状态（只读，汇报）。
- T3b 自重启与协助恢复（按 T3a 结论再写说明）。
- T4 编译类动作：`swift_build`/`selftest`/`node_test`/`build_app`。
- T5 `install_app`（依赖 T3b）、`screenshot`。
- T6 Ubuntu 端 xxyi-msg 的 `mac_*` 工具。
- T7 T4–T6 的测试。
- 部署：交叉编译 Mac 版 wgtund，做成自删脚本（参照 ~/xw2.sh），用户在 Mac 上跑一次；Ubuntu 重编 xxyi-msg。
