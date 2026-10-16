//go:build unix

package macexec

import (
	"os/exec"
	"syscall"
)

// setNewProcessGroup 让子进程自成一个进程组（pgid == 子进程 pid），杀任务时把它派生的子进程一起杀掉。
func setNewProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killProcessGroup 对整组发 SIGKILL；进程已没（ESRCH）不算错，调用方以 Wait 结果为准。
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		return err
	}
	return nil
}
