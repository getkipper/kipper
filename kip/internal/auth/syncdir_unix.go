//go:build !windows

package auth

import "os"

// syncDir flushes a directory entry, so a rename survives a power cut.
func syncDir(path string) error {
	d, err := os.Open(path) //nolint:gosec // path is the auth store's own directory
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}
