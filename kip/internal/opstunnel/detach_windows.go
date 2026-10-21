//go:build windows

package opstunnel

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// detach starts the process without a console and outside the starter's
// process group, so it survives the terminal and the ssh process that
// started it.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS,
		HideWindow:    true,
	}
}
