# G1h 修 mac_* 等待逻辑两处（G1g 评审 P2，只改 go/cmd/xxyi-msg 的 mac_* 代码）
1. 每次 /exec/job 查询返回、取消查询 context 之后，**先**检查总截止：已到就正常返回 job id，再处理 failed/timeout/错误（macexec.go:391–400 附近）。
2. 收到父 ctx.Done() 时先判断总截止是否已到：到了才按「预算用完」返回 job id；没到就返回包装了 ctx.Err() 的错误并附 job id（macexec.go:379–380 附近）。
验收：cd go && go build ./... && go vet ./cmd/xxyi-msg/ && go test ./cmd/xxyi-msg/。不要 git commit。
