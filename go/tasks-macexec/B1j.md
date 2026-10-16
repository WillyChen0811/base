# B1j 修 runner.go 两处状态判断（B1g/B1h 评审 P2）
1. running 状态必须在 cmd.Start() 之前落盘：写失败就关日志、取消 context、返回错误，不启动进程；启动后再落盘失败则杀进程组、等回收、关日志、返回错误。（B1h 评审：现在仍是只写 Note 继续跑，runner.go:193 附近）
2. 输出管道被 WaitDelay 强制关闭（exec.ErrWaitDelay）时不能报 done：标 failed，Note 写「输出收集被强制终止，后台进程已清理」；已到任务截止时间的保持 timeout。只有正常收完输出才报 done。（B1g 评审，runner.go:260 附近）
只改 go/macexec/runner*.go。若 runner_test.go 里有断言与第 2 条冲突（如期望 done），在汇报里列出，不要改测试。
验收：cd go && go build ./... && go vet ./macexec/。不要 git commit。
