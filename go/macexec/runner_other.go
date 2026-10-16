//go:build !unix

package macexec

import "os/exec"

// setNewProcessGroup 在非 unix 平台没有进程组语义，退化为不做任何设置。
func setNewProcessGroup(cmd *exec.Cmd) {}

// killProcessGroup 在非 unix 平台退化为只杀主进程。
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
