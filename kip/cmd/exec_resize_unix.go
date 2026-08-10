//go:build !windows

package cmd

import (
	"os"
	"os/signal"
	"syscall"
)

// notifyOnResize calls onResize whenever the local terminal changes size.
func notifyOnResize(onResize func()) {
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	go func() {
		for range winch {
			onResize()
		}
	}()
}
