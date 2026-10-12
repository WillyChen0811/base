//go:build !unix

package support

import (
	"os"
	"os/exec"
)

// setupProc 非 unix 平台没有进程组概念，退化为对进程本身操作。
func setupProc(cmd *exec.Cmd) {}

// interruptGroup 退化实现：向进程本身发 SIGINT（Windows 上会失败，靠宽限期后的 Kill 兜底）。
func interruptGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(os.Interrupt)
}

// killGroup 退化实现：直接杀进程本身。
func killGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}

// exitCodeOf 退化实现：拿不到信号号，直接用 ExitCode()。
func exitCodeOf(cmd *exec.Cmd) int {
	if cmd.ProcessState == nil {
		return -1
	}
	return cmd.ProcessState.ExitCode()
}
