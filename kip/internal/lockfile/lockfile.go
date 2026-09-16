// Package lockfile serializes writes to shared auth and configuration files.
// Lock a separate file: atomic replacement changes the protected file's inode,
// so a lock on that inode would no longer coordinate subsequent writers.
package lockfile

import (
	"fmt"
	"os"
	"path/filepath"
)

// Exclusive takes an exclusive advisory lock on path, creating it if needed, and
// returns the function that releases it. It blocks until the lock is available.
//
// The caller is expected to defer the release. A process that exits without
// calling it still releases the lock, since the operating system drops it when
// the descriptor closes.
func Exclusive(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("creating directory for %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // the caller names a path inside its own state directory
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	if err := lock(f); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("locking %s: %w", path, err)
	}
	return func() {
		_ = unlock(f)
		_ = f.Close()
	}, nil
}
