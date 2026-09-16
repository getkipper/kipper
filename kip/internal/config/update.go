package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/getkipper/kipper/kip/internal/lockfile"
)

// ErrNoChange tells Update to leave the file alone. It is not a failure: a
// mutation that finds nothing to do should not rewrite the config, because a
// rewrite is what makes a concurrent reader see a different file.
var ErrNoChange = errors.New("nothing to change")

// Update locks, reloads, mutates, and saves the config as one advisory-locked
// operation. All production writers must use this path to preserve concurrent
// updates. ErrNoChange skips the save.
// mutate must use the supplied config and avoid nested config operations,
// especially Update: the lock is not reentrant.
func Update(mutate func(*Config) error) error {
	dir, err := Dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}

	// The lock is a file of its own: the config is replaced by rename, so a lock
	// held on the old inode says nothing to whoever opens the new one.
	unlock, err := lockfile.Exclusive(filepath.Join(dir, "config.lock"))
	if err != nil {
		return err
	}
	defer unlock()

	cfg, err := Load()
	if err != nil {
		return err
	}
	if err := mutate(cfg); err != nil {
		if errors.Is(err, ErrNoChange) {
			return nil
		}
		return err
	}
	return SaveTo(cfg, filepath.Join(dir, "config.yaml"))
}
