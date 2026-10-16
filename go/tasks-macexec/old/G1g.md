# G1g 修 mac_* 长任务等待与重试（G1f 评审 P2 ×3，只改 go/cmd/xxyi-msg 的 mac_* 代码）
1. 9 分钟要从调用开始算：进入工具时就定一个总截止时间，/exec/start（含重试）、每次 /exec/job、节拍等待都受它约束；已有 job id 时预算用完正常返回该 id。
2. 预算耗尽不能算失败：每次查询返回后先看总预算，用完就正常返回 job id，再处理错误与失败计数；节拍等待用「下一节拍」与「总截止」中较早者。
3. 正文读取中断（unexpected EOF、连接重置）也要进入「重读入口 + 重试 + 失败容忍」；保留 HTTP 状态码分类（401/5xx 正文读一半也按状态码处理）。不要改变 xxyi_* 工具的重试行为。
不写测试。验收：cd go && go build ./... && go vet ./cmd/xxyi-msg/ && go test ./cmd/xxyi-msg/。不要 git commit。
