//go:build windows

package ssh

import "os/exec"

// ownProcessGroup retains CommandContext cancellation on Windows: it kills
// only SSH itself. WaitDelay bounds the wait for inherited output pipes.
func ownProcessGroup(*exec.Cmd) {}
