package ingest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/getkipper/kipper/datamover/internal/manifest"
)

// TestCommitEntryErrors drives the dir and symlink commit paths into their
// failure branches with a write-protected root.
func TestCommitEntryErrors(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory write protection")
	}
	src := t.TempDir()
	if err := os.Mkdir(filepath.Join(src, "d"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("d", filepath.Join(src, "l")); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	st := buildState(t, src, root)

	if err := os.Chmod(root, 0o500); err != nil { //nolint:gosec // G302: read-only root drives the failure branch
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) }) //nolint:gosec // G302: restore traversal for TempDir cleanup

	var dirEntry, linkEntry manifest.Entry
	for _, e := range st.Manifest.Entries {
		switch e.Type {
		case manifest.TypeDir:
			dirEntry = e
		case manifest.TypeSymlink:
			linkEntry = e
		}
	}
	if res := commitDir(st, dirEntry); res.Match || res.Error == "" {
		t.Errorf("dir commit into a read-only root must fail: %+v", res)
	}
	if res := commitSymlink(st, linkEntry); res.Match || res.Error == "" {
		t.Errorf("symlink commit into a read-only root must fail: %+v", res)
	}
}

// TestCommitSymlinkReplacesStaleLink covers the swap over an existing link.
func TestCommitSymlinkReplacesStaleLink(t *testing.T) {
	src := t.TempDir()
	if err := os.Symlink("new-target", filepath.Join(src, "cur")); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.Symlink("old-target", filepath.Join(root, "cur")); err != nil {
		t.Fatal(err)
	}
	st := buildState(t, src, root)
	res := commitSymlink(st, st.Manifest.Entries[0])
	if !res.Match || res.Error != "" {
		t.Fatalf("swap over a stale link failed: %+v", res)
	}
	if target, err := os.Readlink(filepath.Join(root, "cur")); err != nil || target != "new-target" {
		t.Errorf("link not swapped: %q %v", target, err)
	}
}
