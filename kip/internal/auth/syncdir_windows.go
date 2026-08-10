//go:build windows

package auth

// syncDir does nothing on Windows. A directory cannot be opened for writing
// there, and Sync is FlushFileBuffers, which needs that right — so the call
// fails with access denied and turns a durability nicety into a save that never
// succeeds.
//
// Doing nothing is what SQLite and LevelDB do here, and it is a real trade
// rather than a free one: NTFS journals directory metadata and flushes the
// journal lazily, so a power cut in that window can still lose the rename. The
// alternative is a store that never saves at all.
func syncDir(string) error { return nil }
