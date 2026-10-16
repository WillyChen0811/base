# G1f mac_* 工具每次调用重新读远端入口
背景（D1 调研结论）：wgtund 重启后协助会按 autostart 自动新开，令牌与地址会变，新的入口发布在 Ubuntu 的 ~/.xxyi-support/endpoint.json；已建好的客户端仍持有旧地址和令牌。
要求：go/cmd/xxyi-msg 里 mac_* 工具每次调用都重新读入口（复用现有读 endpoint 的函数）；连接失败或 401 时再读一次、重试一次。mac_job 轮询期间同样适用（install_app 会重启 wgtund）。
只改 go/cmd/xxyi-msg 里 mac_* 相关代码，不改 xxyi_* 工具行为。不写测试（G2 会覆盖）。
验收：cd go && go build ./... && go vet ./cmd/xxyi-msg/。不要 git commit。
另外：mac_job 轮询中单次 /exec/job 失败（连接错误、401、5xx）不要立即报错，重读入口后重试，连续失败 3 次（或累计 60 秒）再报错并附 job id。
