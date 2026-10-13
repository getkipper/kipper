//go:build !windows

package installjob

import (
	"os"
	"syscall"
)

// lock takes an exclusive lock on path and returns the function that releases
// it.
func lock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // a fixed name under the root-only jobs directory
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil { //nolint:gosec // a file descriptor fits in an int
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:gosec // a file descriptor fits in an int
		_ = f.Close()
	}, nil
}
