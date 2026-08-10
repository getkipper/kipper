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

// Update runs mutate against a freshly loaded config and saves the result, with
// an exclusive lock held across all three steps.
//
// Every command that changes ~/.kip/config.yaml used to load, modify and save as
// three separate acts, which loses an update whenever two runs overlap — and
// they do overlap, because an uninstall holds a credential across a wipe that
// takes minutes and an operator can be answering a prompt for longer than that.
// Two review rounds turned up the same failure from that: one run deleting the
// entry another had just written, and with it the only local copy of a live
// gateway credential.
//
// Reading inside the lock is the point. A caller that loads first and passes the
// result in has already lost the guarantee, so mutate is handed the config this
// function read, and whatever it decides is written back before the lock drops.
// Returning ErrNoChange skips the write.
//
// mutate must not call Update, Load or Save. The lock is not re-entrant, so a
// nested call blocks on a lock this goroutine already holds and the command
// hangs with nothing to show for it.
//
// The lock is advisory, so it orders exactly the callers that take it — which is
// every writer of this file. Save and SaveTo have no production callers outside
// this package (test fixtures seed configs with them under isolated HOMEs), and
// that is the property to keep: one more load-modify-save beside this function
// would put the guarantee back to being a description of most writers rather
// than all of them.
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
