# B1h 修 runner.go：两处落盘失败被吞掉（P2 ×2）
1. Start 时 running 状态 json 写失败仍返回成功：改成在启动进程前先写 running json，写失败直接返回错误、不启动；若已启动后才失败，杀进程组、等清理、关日志再返回错误。
2. NewRunner 恢复遗留 running 任务时改写失败被忽略：恢复函数返回错误，NewRunner 传播出去（不静默成功）；恢复成功的 job 要更新到内存。
只改 go/macexec/runner*.go，不写测试。验收：cd go && go build ./... && go vet ./macexec/。不要 git commit。
