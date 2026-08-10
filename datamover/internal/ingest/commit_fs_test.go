package ingest

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/getkipper/kipper/datamover/internal/manifest"
)

// buildState assembles a ready-to-commit State: manifest from src, chunk
// staging filled with the source content.
func buildState(t *testing.T, src, root string) *State {
	t.Helper()
	m := mustManifest(t, src)
	raw, err := manifest.Encode(m)
	if err != nil {
		t.Fatal(err)
	}
	st, err := InitState(root, DefaultStateDir(root), "t1", raw, m)
	if err != nil {
		t.Fatal(err)
	}
	stageChunks(t, st, src)
	return st
}

// stageChunks fills the flat chunk staging from the source tree.
func stageChunks(t *testing.T, st *State, src string) {
	t.Helper()
	for n := 0; n < st.Plan.NumChunks(); n++ {
		var payload bytes.Buffer
		for _, sp := range st.Plan.Spans(n) {
			f, err := os.Open(filepath.Join(src, filepath.FromSlash(sp.Path)))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.Copy(&payload, io.NewSectionReader(f, sp.FileOffset, sp.Length)); err != nil {
				t.Fatal(err)
			}
			_ = f.Close()
		}
		if err := os.WriteFile(st.ChunkPath(n), payload.Bytes(), 0o600); err != nil { //nolint:gosec // G703: test-controlled path
			t.Fatal(err)
		}
	}
}

func TestFSCommitterFidelity(t *testing.T) {
	src := t.TempDir()
	if err := os.Mkdir(filepath.Join(src, "empty-bucket"), 0o750); err != nil {
		t.Fatal(err)
	}
	dirMtime := time.Date(2026, 5, 5, 5, 5, 5, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(src, "empty-bucket"), dirMtime, dirMtime); err != nil {
		t.Fatal(err)
	}
	writeFile(t, src, "docs/readme.md", []byte("# hi"), 0o644)
	if err := os.Symlink("docs/readme.md", filepath.Join(src, "README")); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	// Type conflicts the committer must converge:
	if err := os.Mkdir(filepath.Join(root, "README"), 0o750); err != nil {
		t.Fatal(err) // dir where the symlink belongs
	}
	writeFile(t, root, "docs", []byte("file where a dir belongs"), 0o644)

	st := buildState(t, src, root)
	report, err := FSCommitter{}.Commit(t.Context(), st)
	if err != nil {
		t.Fatal(err)
	}
	for _, res := range report.Files {
		if !res.Match || res.Error != "" {
			t.Errorf("entry %s failed: %+v", res.Path, res)
		}
	}
	if target, err := os.Readlink(filepath.Join(root, "README")); err != nil || target != "docs/readme.md" {
		t.Errorf("symlink not converged: %q %v", target, err)
	}
	info, err := os.Stat(filepath.Join(root, "empty-bucket"))
	if err != nil || !info.IsDir() {
		t.Fatalf("empty dir missing: %v", err)
	}
	if info.ModTime().UnixNano() != dirMtime.UnixNano() {
		t.Errorf("dir mtime = %v, want %v", info.ModTime(), dirMtime)
	}
	got, err := os.ReadFile(filepath.Join(root, "docs", "readme.md"))
	if err != nil || string(got) != "# hi" {
		t.Errorf("docs/readme.md not converged over the stale file: %v %q", err, got)
	}
}

func TestEnsureRealDirReplacesSymlinkComponent(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "data")); err != nil {
		t.Fatal(err)
	}
	if err := ensureRealDir(root, "data/nested"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(filepath.Join(root, "data"))
	if err != nil || fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		t.Error("symlink path component must be replaced by a real directory")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Error("nothing may be created through the symlink")
	}
}

func TestReplaceIfDir(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "was-dir")
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := replaceIfDir(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Error("stale directory tree must be removed")
	}
	file := filepath.Join(root, "plain.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := replaceIfDir(file); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(file); err != nil {
		t.Error("a non-directory must be left in place for the atomic rename")
	}
	if err := replaceIfDir(filepath.Join(root, "absent")); err != nil {
		t.Errorf("missing path must be fine: %v", err)
	}
}
