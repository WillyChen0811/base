# B1f 修 go/macexec/runner.go 的评审问题（三条都要修）
1. P1 超时只杀主进程：`build.sh`/`swift build` 派生的子进程会继续跑。修法：`cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}`，超时或取消时 `syscall.Kill(-pid, SIGKILL)` 杀整个进程组；等清理完成才释放单任务占用。（非 unix 平台用 build tag 退化为只杀主进程。）
2. P2 日志无上限：单个 job 输出上限 32MB（常量）。stdout/stderr 经一个计数写入器写文件，超额时终止任务（杀进程组）、State=failed、Note 写「输出超过上限」；写文件出错同样标 failed 并记 Note。
3. P2 运行中的 job 没落盘：Start 返回前先原子写一份 State=running 的 json。NewRunner 启动时扫描目录，遗留的 running 一律改成 failed、Note「执行器重启，任务中断」（不尝试接管进程）。
只改 runner.go（可新增 runner_unix.go/runner_other.go 放平台相关代码）。不写测试（B2 会覆盖）。
验收：cd go && go build ./... && GOOS=darwin GOARCH=arm64 go build ./... && go vet ./macexec/。不要 git commit。
