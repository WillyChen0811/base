# B1g 修 runner.go：主进程先退出后超时失效（P1）
场景：build.sh 起了继承 stdout/stderr 的后台子进程后自己退出；cmd.Wait() 等管道关闭时，CommandContext 的取消监视已结束，之后超时不会触发，子进程继续跑、执行器一直 ErrBusy。
修法：自己起一个截止计时器，直到输出收集与清理全部结束前都能对进程组发 SIGKILL；同时设 `cmd.WaitDelay`（如 5 秒）兜底关闭管道；清理完成后才释放单任务占用。超时的 State=timeout。
只改 go/macexec/runner*.go，不写测试。验收：cd go && go build ./... && GOOS=darwin GOARCH=arm64 go build ./... && go vet ./macexec/。不要 git commit。
