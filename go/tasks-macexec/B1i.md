# B1i 修 runner.go 两个被 B2 测出来的 bug（让 go test ./macexec/ 全过）
1. NewRunner 恢复遗留 running 任务时，recoverStaleRunning 里 persistLocked 的错误被 `_ =` 吞掉——要返回错误并由 NewRunner 传播（见 runner_test.go 的 TestNewRunnerRecoverWriteFailure）。
2. Tail 在 maxBytes 窗口里无条件丢掉首行：应先看窗口前一个字节是不是 '\n'，是就保留首行（见 TestTailMaxBytesWindow）。
只改 go/macexec/runner*.go，不改测试。验收：cd go && go test ./macexec/ 全过、go vet ./macexec/。不要 git commit。
