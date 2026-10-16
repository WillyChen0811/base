# G1f mac_* 工具每次调用重新读远端入口
背景（D1 调研结论）：wgtund 重启后协助会按 autostart 自动新开，令牌与地址会变，新的入口发布在 Ubuntu 的 ~/.xxyi-support/endpoint.json；已建好的客户端仍持有旧地址和令牌。
要求：go/cmd/xxyi-msg 里 mac_* 工具每次调用都重新读入口（复用现有读 endpoint 的函数）；连接失败或 401 时再读一次、重试一次。mac_job 轮询期间同样适用（install_app 会重启 wgtund）。
只改 go/cmd/xxyi-msg 里 mac_* 相关代码，不改 xxyi_* 工具行为。不写测试（G2 会覆盖）。
验收：cd go && go build ./... && go vet ./cmd/xxyi-msg/。不要 git commit。
另外：mac_job 轮询中单次 /exec/job 失败（连接错误、401、5xx）不要立即报错，重读入口后重试，连续失败 3 次（或累计 60 秒）再报错并附 job id。

## 同时修 G1 评审的 4 个问题
1. P2 缺 `mac_install_app`：按 CONTRACT 补注册与 install_app 动作（POST /exec/start {action:"install_app"}，长任务轮询；轮询期间 wgtund 会重启，靠上面的重读入口+重试撑过去）；mcp_test.go 的工具总数与名单同步为 15 个。
2. P2 9 分钟上限不是硬上限：用独立的截止时间；每次 /exec/job 查询的 context 限制在剩余预算内；预算用完正常返回 job id（不是报错）；轮询间隔按固定 5 秒节拍算，不叠加请求耗时。
3. P2 截图中断被当成功：run.go 的 doLimited 处理并返回正文读取错误；截图校验完整 8 字节 PNG 签名，并用 image/png 解码一次验证完整性。
4. P3 read_file 行数多算：返回文本以换行结尾时不额外加一（或直接用服务端返回的 lines_total/实际行数）。
