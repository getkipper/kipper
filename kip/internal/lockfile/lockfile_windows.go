//go:build windows

package lockfile

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// Windows has no flock. LockFileEx over the first byte is the equivalent: an
// exclusive byte-range lock the operating system releases when the handle
// closes. Locking one byte rather than the whole file keeps it independent of
// the file's length, which for a lock file is always zero.
const lockedBytes = 1

func lock(f *os.File) error {
	return windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK,
		0, lockedBytes, 0,
		new(windows.Overlapped),
	)
}

func tryLock(f *os.File) error {
	err := windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, lockedBytes, 0,
		new(windows.Overlapped),
	)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return ErrHeld
	}
	return err
}

func unlock(f *os.File) error {
	return windows.UnlockFileEx(
		windows.Handle(f.Fd()),
		0, lockedBytes, 0,
		new(windows.Overlapped),
	)
}
