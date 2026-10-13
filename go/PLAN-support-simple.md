# 交接：协助服务简化（2026-10-13 用户定）

## 原则（只有两条）
1. **安全只保留一条：授权钩子必须在。** Mac 上的 Claude 是正常的 Claude Code，用它自己的整套安全设计；它本来会在终端问用户的授权，全部经 PermissionRequest 钩子转到 XXYI 协助页，用户亲自点允许/拒绝。钩子不通 = 拒绝。
2. **XXYI 是两个 Claude 之间的通信渠道。** Ubuntu 上的 Claude（我）把任务交给 Mac 上的 Claude，对方的进度、问题、结果传回来；它能问我，我能回答；代码经 git 双向流动（Ubuntu→Mac 用 sync，Mac→Ubuntu 用项目同步的 `mac/*` 分支）。

## 要拿掉（都是我自制的规则，造成了今天处处卡住、让用户手敲命令）
- `wgtund/support.go` 的 `checkSandboxSettings`：不再强制 sandbox.enabled / allowUnsandboxedCommands / failIfUnavailable / autoAllowBashIfSandboxed / defaultMode 等。
- 不加任何允许清单；`claude_args` 白名单、`--restricted`、`--tools` 固定工具集、`--strict-mcp-config` 是否保留要重新评估（目标：就是一个正常的 Claude Code + 钩子）。
- `claude_settings` 变成可选（没有就只写钩子）。
- 保留：合并设置里强制写入 PermissionRequest 钩子、`disableAllHooks=false`；钩子失败即拒绝。

## 要补
- 双向沟通：Mac 上的 Claude 能把问题发回 Ubuntu（现在 `macsup ask` 只能单向一问一答）。
- 任务以「目标」交给 Mac 上的 Claude（例如按 mac/tasks/proto1.md 做），它自己编码、编译、改错，做完提交，我复核。
- 时长放宽到 10 小时（用户要求）；无人值守模式另议。

## 当前状态
- base `feat/approve`（worktree `~/Documents/codes/base-approve`，基于 feat/projsync）有一份**未提交**的改动：每次开启协助重读设置（Config.Prepare）、时长上限 600、沙盒外请求摘要加「【沙盒外运行】」、强制 allowUnsandboxedCommands=true + ask 规则。后两项按新原则作废；前两项可留。测试通过。
- xxyi `feat/approve`（`~/Documents/codes/xxyi-approve`）还没改：协助页时长选项要加到 600。
- 原生终端 proto1 暂停：xxyi `feat/native-wip`（`~/Documents/codes/xxyi-native`），任务说明 `mac/tasks/proto1.md`；Mac 上 `~/xxyi-work/xxyi/mac` 已手动 `swift package resolve`，那里多了未跟踪的 `Package.resolved`（同步前要处理）。
- `~/.omlx-agent/bin/macbuild`：经协助在 Mac 编译的脚本（含 --disable-sandbox、空 git 模板），按新原则大概率不再需要这些绕法。

## 做法
编码交给 Sonnet（或 omlx agent），Opus 只做计划、跟用户商量规则、复核。改完需要用户在 Mac 上更新 wgtund 一次。
