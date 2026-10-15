# 消息功能复核报告（2026-10-15，代码同步后，Opus 只读复核）

接上一轮 REPORT-msg-diagnosis-2026-10-15.md。证据：Hub `/msg/inbox` 原始 JSON（含 Reason/Synced/Now/Attempts/Report）、Mac `~/.xxyi/support-log.jsonl`（状态变化与隧道事件）、Mac `pmset -g log`（睡眠/唤醒）、Ubuntu 钩子日志、本会话对话记录。时区：Ubuntu 为 UTC，Mac 与消息时间为 +08；上一轮的「05:30–10:44」是 UTC，即 Mac 13:30–18:44。快照（14:41 UTC）：原 5 条 sync_failed 已被用户重试并真同步成功（delivered，Synced=true），refs/xxyi 已清空。

## 一、相对上一轮的变化
1. **新增（严重）：「不同步直接送」会对收件人谎报「代码已同步」。** #ca37461b、#0facc696、#3e566bfc 注入文字都是「（代码已同步：仓库 xxyi 分支 main）」，而当时 Ubuntu main 并没有这些提交。上一轮说会附「代码没有同步」是错的。
2. 补齐：5 条 sync_failed 的 Reason 全是「Ubuntu 上 main 有 Mac 没有的提交，不能快进」；F1/F2 确认是先 sync_failed 再被「不同步直接送」；那条 notice sync_failed 是因为发件人给它带了 repo/branch，而 Hub 只要带仓库就同步、不看类型。
3. 「立即送达点不了」：点击确实到了后端（F1 约 16 次、F2 约 15 次、#ca37461b 至少 17 次），但「立即送达」不跳过同步也不跳过退避，所以点了没反应；转 sync_failed 后按钮消失。不是「按钮被重建、点击落空」。
4. 隧道中断主因：13:35 合盖睡眠；醒来后约 1 小时连不上 192.168.150.3:22；14:49–15:13 线路连着但很慢，钩子 2 秒超时；15:32 wgtund 关闭，到 18:44 才重新服务（前一分钟 hosts.json 被改过，没起来的原因未确认）。
5. 降级：上一轮第 5 条（ack 超时重复注入）在 ubuntu:xxyi 收件箱 53 条里一次都没出现，目前只是源码推断；第 1 条（钩子上报失败）是隧道断的后果，不是根因。

## 二、按症状归类
**S1 收件人被告知「代码已同步」其实没同步（高）**：support/msg.go:1445-1446 skip_sync 只设 Synced=true；Display（msg.go:1291-1297）只看 Synced；带仓库的消息送达时 Synced 必为真，「没有同步」分支执行不到（测试 msgproto_test.go:392 只测了构造的 Synced=false）。影响所有被「不同步直接送」的 code 消息（今天 feat/native-wip 上的 #3f4b985e、#8b4a76f8、#0c3e0615、#bc41231e、#81c272fa 很可能也是）。修法选项：(a) 独立字段「已跳过同步」，Display/界面据此显示未同步（要兼容旧快照）；(b) skip_sync 不动 Synced，另设跳过标记让 pumpOne 不再同步；(c) 跳过时改成 notice 并在正文追加说明（涉及签名）。
**S2 「立即送达」点了没反应随后按钮消失（高）**：now 只设 Now=true（msg.go:1435-1441），pumpOne 照样遵守退避（:1752）、照样先同步（:1760、:1791）；notff 一轮 5 次约 35–40 秒后转 sync_failed；界面 sync_failed 只给重试/不同步直接送/改送/拒绝（Messages.swift:208），后端也不接受对 sync_failed 做 now（msg.go:1416）；点击成功无反馈（MessagePanelView.swift:487-491）；queued 一律「等对方空闲」（Messages.swift:174）而失败报告记的对端是 idle。用户对 #ca37461b 先后点了 6 次重试（7 轮、35 次注定失败的同步），最后用「不同步直接送」放行。修法选项：(a) 带仓库的 now = 立即重试同步并清退避，界面明说要先选不同步；(b) queued 直接提供「不同步直接送」、sync_failed 也保留「立即送达」并标注会跳过同步（必须先修 S1）；(c) 点击后明确回执、请求返回前置灰（治标）。
**S3 代码同步失败（notff），重试空转（高）**：同步是单向（Mac 分支推到 refs/xxyi/mac/<分支> 再在 Ubuntu ff-only，support/gitpush.go:62-116），Ubuntu 有 Mac 没有的提交就必然失败，却按临时错误重试 5 次（msg.go:1819-1829）。今天同一 Reason 出现 60 次。另：wgtund/msg.go:101-104 中 PushRef 的连接失败（如 08:52 那次 Connection closed，共 9 次）没标 ErrLinkDown，消耗了重试预算而不是暂停线路（可直接修）。修法选项：(a) notff/dirty/busy 第一次就转 sync_failed 并写出两端 HEAD 与建议（失去自愈）；(b) notff 时在 Ubuntu 自动建 xxyi/mac/<分支> 保存 Mac 提交、消息照常送达并注明需合并（多出分支要人合并）；(c) 约定 Ubuntu 不在共享分支提交或合并后必须推回 Mac（靠守规矩，需配合 a）。
**S4 notice 也卡在代码同步（高）**：Send 只禁止回复带仓库、只要求 code 必须带仓库（msg.go:1028-1033），pumpOne 只要 Repo 非空就同步（:1760）。#bbc648f9 就是这样。修法选项：(a) Send 拒绝 notice/request 带仓库（要改提示和 MCP 说明）；(b) notice 带仓库只作参考不同步；(c) 维持并在界面单独标出。
**S5 隧道或 Hub 长时间不可用（中高）**：时间线见上。由此：上报只等 2 秒（cmd/xxyi-msg/run.go:26）且失败即丢；wgtund 退出时撤销 endpoint.json 失败，Ubuntu 分不清协助关了还是线路断了；Mac 上没有 LaunchAgent，wgtund 退出没人拉起。修法选项：(a) 钩子待补报文件 + Hub 记最近上报时间、界面显示「N 分钟前上报」；(b) wgtund 交给 launchd KeepAlive，配置错误写固定日志（要和界面启动方式协调）；(c) 合盖期间阻止睡眠，或线路连续失败后自动换线（耗电、换线逻辑要验证）。
**S6 状态显示误导、原因被藏（高）**：queued 一律「等对方空闲」；状态列只在 failed/sync_failed 显示 Reason（MessagePanelView.swift:418）；详情区只在需人工处理或有失败报告时列尝试记录（:556），而重试会清掉失败报告（msg.go:608）；「已送达」对 Now 消息只代表注入成功（msg.go:1981-1982、:1640-1641）。修法选项：(a) queued 附细分原因（等对方空闲/同步重试第 N 次/退避到某时/线路暂停/等确认）；(b) 详情区始终显示尝试记录；(c) 区分「已注入」与「已确认」，与 PLAN-observer 合并做。
**S7 失败后重试，旧消息很晚才送到（中）**：F3–F6 与收尾 notice 在 22:37 被重试后集中送出，内容已过时。修法选项：(a) 迟到送达时正文前加「（迟到 N 分钟）」；(b) 发件人可撤回（PLAN-observer 已有）；(c) sync_failed 超过一定时间提示用户拒绝还是重试。
**S8 重复注入、假忙 30 分钟（低到中，理论推断）**：源码逻辑成立（msg.go:734-771），但 ubuntu:xxyi 收件箱 53 条无一次 ack/deliver 阶段尝试；发往 mac: 的看不到；mac:codex1 残留 busy 仍在。修法：Hub 推测出来的 busy 带「推测」标记且时效更短，或由观察者核对。
**S9 排查困难（高）**：Hub 日志只记状态不记操作与触发者；收件箱不返回送达时间与跳过同步标记；钩子成功行不带端口；「tmux 会话名为空」仍在发生（如 14:31:44Z 的 --start），来源进程不明；macsup ask 只返回最终回复。修法选项：(a) logMsg 增加 action/来源/尝试次数/是否跳过同步；(b) 只读「消息时间线」接口；(c) 钩子日志写全端口、耗时、TMUX/TMUX_PANE、父进程名。

## 三、「立即送达点不了」复现步骤
准备（两边 Claude 做，用户不敲命令）：Ubuntu 在 xxyi 的测试分支 msgtest 上提交一个 Mac 没有的提交；Mac 在同名分支提交一个不同的提交，再发 kind=code、repo=xxyi、branch=msgtest 给 ubuntu:xxyi。用户：1) 消息面板选中它，状态显示「等对方空闲」（Ubuntu 其实空闲）；2) 35 秒内连点「立即送达」，无提示、状态不变；3) 约 35–40 秒后变「同步失败：…不能快进」，「立即送达」消失；4) 点「重试」再空转 5 次回到同步失败；5) 点「不同步直接送」马上送达，Ubuntu 终端里消息末尾写「（代码已同步…）」——错误，同时复现 S1。对照：Mac 上 grep 该消息 id 的 support-log.jsonl，每点一次「立即送达」多一行 queued。

## 四、仍未确认与需补的日志
1. 15:32–18:44 wgtund 为什么没恢复：读 ~/.xxyi/service.log、ui.log 该时段；对比 hosts.json.bak-1015172524 与 hosts.json.bak-1015153112 看 15:31 改了什么；查清谁改了 hosts.json。
2. 13:42–14:44 为什么连不上 192.168.150.3:22：线路切换与探测结果写进 support-log；重连失败记所用线路与本机网络接口。
3. 14:49–15:13 线路慢：记录每次请求耗时。
4. 「tmux 会话名为空」来源：钩子记父进程链与 TMUX/TMUX_PANE。
5. S8 在 mac: 收件方是否出现：读 Mac msg-store.json 的 stage=ack 尝试。
6. 今天 feat/native-wip 上那几条 code 消息是真同步还是被直接送出：查 support-log.jsonl 的「sync_failed→queued→delivered」序列。
7. 引用源码为 base-msg 的 feat/msg；Mac 实际运行的 wgtund 版本未直接核对，但 S1、S2 行为与日志、对话记录一致。
