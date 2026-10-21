//go:build !windows

package opstunnel

import (
	"os/exec"
	"syscall"
)

// detach puts the process in a session of its own, so it survives the
// terminal and the ssh process that started it.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
