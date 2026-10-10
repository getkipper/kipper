//go:build !windows

package ssh

import (
	"os/exec"
	"syscall"
)

// ownProcessGroup makes cancellation kill the command and helpers that remain
// in its process group, including those holding output pipes open.
func ownProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
