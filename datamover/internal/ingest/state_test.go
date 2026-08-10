package ingest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/getkipper/kipper/datamover/internal/manifest"
	"github.com/getkipper/kipper/datamover/internal/wire"
)

func TestLoadStateAbsent(t *testing.T) {
	root := t.TempDir()
	st, err := LoadState(root, DefaultStateDir(root))
	if err != nil || st != nil {
		t.Errorf("fresh root: got state %v, err %v; want nil, nil", st, err)
	}
}

func TestLoadStateRejectsCorruption(t *testing.T) {
	src := t.TempDir()
	writeFile(t, src, "data.bin", []byte("abcdef"), 0o644)
	m, err := manifest.BuildDir(src, 4)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := manifest.Encode(m)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		corrupt func(t *testing.T, root string)
	}{
		{"truncated bitmap", func(t *testing.T, root string) {
			if err := os.WriteFile(filepath.Join(root, wire.StateDirName, "bitmap"), []byte{}, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"garbage manifest", func(t *testing.T, root string) {
			if err := os.WriteFile(filepath.Join(root, wire.StateDirName, "manifest.json"), []byte("{oops"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"missing id file", func(t *testing.T, root string) {
			if err := os.Remove(filepath.Join(root, wire.StateDirName, "transfer-id")); err != nil {
				t.Fatal(err)
			}
		}},
		{"missing bitmap", func(t *testing.T, root string) {
			if err := os.Remove(filepath.Join(root, wire.StateDirName, "bitmap")); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			if _, err := InitState(root, DefaultStateDir(root), "t1", raw, m); err != nil {
				t.Fatal(err)
			}
			tt.corrupt(t, root)
			if _, err := LoadState(root, DefaultStateDir(root)); err == nil {
				t.Error("corrupted state must fail to load")
			}
		})
	}
}

func TestStateChunkTracking(t *testing.T) {
	src := t.TempDir()
	writeFile(t, src, "data.bin", []byte("abcdefgh"), 0o644)
	m, err := manifest.BuildDir(src, 4)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := manifest.Encode(m)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	st, err := InitState(root, DefaultStateDir(root), "t1", raw, m)
	if err != nil {
		t.Fatal(err)
	}
	if st.AllChunksDone() {
		t.Error("fresh state must not report all chunks done")
	}
	if st.ChunkDone(0) {
		t.Error("chunk 0 must start incomplete")
	}
	if err := st.MarkChunk(0); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkChunk(1); err != nil {
		t.Fatal(err)
	}
	if !st.AllChunksDone() {
		t.Error("all chunks marked, AllChunksDone must be true")
	}
	if err := st.Destroy(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(st.StateDir()); !os.IsNotExist(err) {
		t.Error("Destroy must remove all transfer state")
	}
}
