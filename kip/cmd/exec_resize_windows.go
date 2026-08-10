//go:build windows

package cmd

// notifyOnResize does nothing on Windows, which reports console resizes as input
// events rather than as a signal. The size sent when the session opens is still
// correct, so the remote shell draws its prompt at the right width; resizing the
// window mid-session leaves it drawing to the old one until the next command.
func notifyOnResize(func()) {}
