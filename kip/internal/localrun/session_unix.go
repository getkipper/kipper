//go:build !windows

package localrun

import (
	"os/exec"
	"syscall"
)

// ownSession isolates the command from interrupts sent to kip's process group.
// Commands that exit on EOF can then shut down when their input closes.
func ownSession(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
