//go:build unix

package support

import (
	"os/exec"
	"syscall"
)

// setupProc 让子进程自成一个进程组，信号可以整组发。
func setupProc(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// interruptGroup 向子进程所在进程组发 SIGINT。Setpgid 之后组号等于子进程 pid。
func interruptGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGINT)
}

// killGroup 向子进程所在进程组发 SIGKILL。
func killGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

// exitCodeOf 取退出码；被信号杀死记成 128+信号号（ExitCode() 对信号退出只给 -1，信息会丢）。
func exitCodeOf(cmd *exec.Cmd) int {
	ps := cmd.ProcessState
	if ps == nil {
		return -1
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ps.ExitCode()
}
