// Package lockfile serialises kip invocations that would otherwise overwrite
// each other's changes to a shared file.
//
// Two of them need it. The auth store holds a refresh token that Dex rotates on
// use, so two racing refreshes leave one process with a revoked one. The local
// config holds gateway credentials, and an uninstall keeps one across a wipe
// that takes minutes, long enough for another command to replace the entry it
// is about to delete.
//
// Both follow the same rule, which is the reason this is one package rather than
// two copies: the lock goes on a file of its own, never on the file being
// guarded. Both of those are replaced by writing a temp file and renaming over
// them, and a lock held on the replaced inode tells the next process nothing.
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
