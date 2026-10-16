# 待办队列（指挥维护，每次变动就更新）

更新：2026-10-16（下午）

## 等用户在 Mac 上做
1. 消息面板里处理遗留的 #5fc122a6（pending）、#3d326d84（发给 shell1 的部署命令，点拒绝以免重复注入）。
2. 决定 base 的 22 个旧工作区怎么处理（见下）。

## 项目同步 v2（PLAN-sync-v2.md，以第十四到十八节为准）
- [完成] 全部后端（base-msg feat/msg 到 0c4494f）、部署（两边已换新版）、控制中心面板（codex1，xxyi main 的 228f55a/d56dc04/f473c48）。
- [完成] 10/16 实测：ai-browser、xxyi、websubmit 同步正常（新建分支、快进、遇未提交改动跳过、冲突停下都符合预期）；ai-browser 的冲突由指挥合并（0f64a98）。websubmit 已加入项目列表（需 Mac 重开 XXYI 生效）。自动同步仍关闭，由用户定何时打开。
- [完成] base 22 个旧工作区已清理（16 个 wip/ 全部提交留存、目录移除，Sonnet 做、指挥核对）；base 首次同步成功（27 分支）。Mac 全局已忽略 .DS_Store（xds.sh）。
- [完成] 对话记录同步并入 v2（base-msg 3ac7c11，omlx 写、指挥复核）；wgtund 已部署到 Mac。待验证：用户点 xxyi 同步后跑 xcheck.sh 看 claude 字段。
- [进行中] 下架 v1 第一步：omlx3 去掉面板 v1（#9a701a21eb3d1059）。之后：后端删旧 /projsync 接口（保留 v2 复用的零件）；面板加对话记录一行。
- [待·用户新增] build.sh：打包后拷到 /tmp 清扩展属性再签名，签完覆盖回 build/XXYI.app，并再校验一次（避开同步软件干扰）。等 omlx3 做完面板精简后另派，一项一任务。
- [待] 验证通过后下架 v1（面板 v1 入口 + /projsync 旧接口）；面板加一行显示对话记录同步结果（Mac 执行者）。
- [待] 是否打开自动同步（xauto-on.sh 已备好，用户定）；websubmit 已加入列表。
- [完成] wgtund 打包进 XXYI.app + 固定开发证书签名（xxyi 2f07d5e，omlx3 做）。解决内网被 macOS「本地网络」权限挡住的问题。
- [待] 面板小毛病：数字 0 显示成 Q（字体/渲染问题，待查）。
- [待] tests/test_terminal.py 的 3 个测试用 /proc，Mac 上跑不了，应加平台判断。
- [待] omlx 会话模板里关掉 LSP 插件推荐提示。
- [待] 小改进：超出范围的分支单独跳过、其他分支继续。
- [待] 排查 codex1「不知道自己的地址」（errNoSelfAddr，selfAddr 找不到；Codex 启动 MCP 会清环境变量）。
- [待] Mac 上 xxyi、base 迁到 ~/Documents/codes（步骤先给用户过目）；bioflow、biogateway 加入同步。
- [待] 日志同步（第一版不做）。
- [待] XXYI 自动启动、停止 agent（让「做完」状态天然确定）。

## Mac 界面（执行者：没指定用 omlx，可指定 Codex/Sonnet）
- [完成] H1–H6、J1–J3、K 分屏全套、浮层三问题、Dock 崩溃、面板文字复制（0a93543）。
- [待] G1、G2：Dock 顶层「消息、审批、设置、更多」（任务原文 #440219b507538d0a，改写后重发）。
- [待] 界面美化第一步：设计令牌 + 琥珀配色，不动布局。
- [取消] 终端「鼠标选字」按钮；终端两端显示差异的修法（用户在 codex1 会话里取消）。

## 消息系统（设计细节由用户定）
- [待用户定] 状态第一、紧急程度、暂停马上送达、可结束进程；送达优先非终端通道（钩子），注入只兜底。见 PLAN-observer.md 与两份 10/15 报告。
- [待] 干净 omlx 会话实测钩子送消息（PostToolUse / Stop / PreToolUse）。
- 已知：普通 shell 终端收不到回执会被重复注入；以后应支持给终端发不带消息头的纯命令。

## 更后面
- 可直接修：PushRef 连接失败应标 ErrLinkDown（不消耗重试预算）。
- 全局审批钩子（xxyi-msg approve-hook）两边都还没装。
