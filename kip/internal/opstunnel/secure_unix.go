//go:build !windows

package opstunnel

import (
	"fmt"
	"os"
	"syscall"
)

// secureDir creates dir with owner-only permissions. Before changing an
// existing directory's permissions, it rejects symlinks and other owners.
func secureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("checking %s: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory, so kip will not put the tunnel's socket there", dir)
	}
	if st, ok := info.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s belongs to another user, so kip will not put the tunnel's socket there", dir)
	}
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // a directory needs its search bit for its owner
		return fmt.Errorf("securing %s: %w", dir, err)
	}
	return nil
}

// SecureFile sets path to mode 0600 and returns any chmod error.
func SecureFile(path string) error {
	return os.Chmod(path, 0o600)
}
