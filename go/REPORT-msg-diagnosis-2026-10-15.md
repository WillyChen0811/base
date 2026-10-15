# 消息功能「不顺」排查报告（2026-10-15，Sonnet 只读排查；代码未同步状态下做的，待同步后复核）

前提：消息中心 Hub 跑在 Mac 的 wgtund 里；Ubuntu 钩子连不上 = Ubuntu→Mac 隧道或 Mac 协助不可用。

## 根因摘要（按影响排序）
1. **忙闲上报单次、2 秒、失败即丢、无补报；Hub 只靠上报更新状态。** 钩子日志今天 05:30–10:44 共 110 次，104 次失败（05:45–07:27 为 context deadline exceeded；07:28–10:43 为 connection refused，端口 37335），期间 idle/gone 全丢，Hub 状态冻结在旧值（多半 busy），靠 30 分钟 busyStale 才失效。出处：go/cmd/xxyi-msg/run.go:1108 起 cmdState；msg.go:662 peerReadyLocked、:178 busyStale。
2. **界面把真实原因藏起来**：queued 一律显示「等对方空闲」（XxyiCore/Messages.swift:174），退避、线路暂停、同步重试、ack 超时、取走未确认等原因都被盖住；Reason 只在 failed/sync_failed 才显示（MessagePanelView.swift:418）。
3. **「立即送达」对 kind=code 不能跳过同步也不能绕过退避**：`now` 只设 Now=true，pumpOne 里 needSync 照旧先同步，同步失败重试 5 次（约 30 秒）才转 sync_failed，「不同步直接送」（只在 sync_failed 才给，Messages.swift:205–212）才出现。
4. **sync_failed 根因是两边都往 main 提交**：同步是单向 ff-only，一边有对方没有的提交就必然 not fast-forward；这是确定性错误却被当瞬时错误重试 5 次。今天 #3533f7d869101e6d、#3000a63ef3dc236e 就是 notff（Ubuntu 的 eae6828 与 Mac 的 fc2fa82/F3/F4 分叉）。
5. **ack 机制把「钩子上报丢了」误判成「对方没处理」**：注入后 30 秒内没收到钩子 busy 就回 queued 重投并把对端强置 busy，造成重复注入和最长 30 分钟的假 busy（msg.go:734 checkInjectedLocked、:1608 TakeOutbox）。

## 其他发现
- Hub 自己乐观置 busy（取走/注入/ack 超时），要靠钩子 idle 翻回，钩子丢了就卡 30 分钟；面板对超过 busyStale 的 busy 仍显示「忙」，应显示「疑似失联」（mac:codex1 就是残留 busy）。
- 取地址失败 28 条（tmux 会话名为空 18、不知道自己的地址 6、取 tmux 会话名失败 4），gone 上报也因此多次丢失；来源进程不明（可能是 codex exec/子代理继承了 TMUX）。
- injected 状态没有任何按钮，注入后最多 30 秒无法干预。
- 「不同步直接送」= skip_sync：Synced=true 并重新 queued，消息文本附「代码没有同步」，代码并没到对方仓库。
- sync_failed 原因类别：ErrDirty（对端工作区脏）、ErrNotFastForward、暂存引用被改、ErrRepoBusy、仓库未知/无 Ubuntu 目录、SSH 连不上（ErrLinkDown 不计失败只暂停线路）、同步或锁超时。
- 耗时：Pump 每 2 秒一轮，ack 窗口 30 秒，退避最长 5 分钟；inbox 不返回送达时间，今天的发出→送达耗时无法计算。
- 不确定：05:30–10:44 隧道为什么断（Mac 日志读不到）；F1/F2 是否被手动「不同步直接送」；#3533/#3000 Reason 原文；「立即送达点不了」是按钮被反复重建还是点了没效果（需实机复现）；面板「空闲」来源。

## 建议修法（只是思路，设计须用户拍板）
- 钩子失败写本地待补报文件，下次成功时补发最新状态；Hub 记最近上报时间并在界面显示年龄；Hub 自置的 busy 带「推测」标记且短时效；由观察者（PLAN-observer.md）核对。
- queued 状态附带 Reason（退避中、线路暂停、ack 回退、同步重试第几次）。
- queued 的 code 消息直接开放「不同步直接送」；notff/dirty 等确定性错误直接转 sync_failed，不重试 5 次；`now` 对 queued 也重置 nextTry。
- 约定 Ubuntu 在非 main 分支做改动，或 notff 时给出两端 HEAD 与合并建议；消息详情显示同步失败的 Reason 原文。
- 补日志：Hub 的 SetState（来源/旧状态）、peerReadyLocked 为 false 的原因、每条消息 injectedAt/deliveredAt/重投次数与原因、sync Reason 原文与 Decide 操作；钩子失败行带端口、耗时、endpoint.json 是否存在、TMUX/TMUX_PANE/父进程名；隧道与协助开关事件日志；inbox/list 返回送达时间与 Reason。
