package ingest

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/getkipper/kipper/datamover/internal/chunk"
	"github.com/getkipper/kipper/datamover/internal/wire"
)

// TestStateDirOutsideRootKeepsRootClean covers the NFS-root scenario: with
// --state-dir on separate scratch, the data volume sees no transient state at
// any point, staging is flat chunk files (no source-tree mirroring), and the
// bitmap resumes from the scratch dir across a server restart.
func TestStateDirOutsideRootKeepsRootClean(t *testing.T) {
	src := t.TempDir()
	writeFile(t, src, "deep/nested/tree/data.bin", bytes.Repeat([]byte("n"), 2500), 0o644)
	writeFile(t, src, "deep/other.txt", []byte("more"), 0o644)
	m, compressed := buildManifest(t, src)

	root := t.TempDir()
	stateDir := filepath.Join(t.TempDir(), "scratch")

	srv, err := NewServer(root, stateDir, testToken, FSCommitter{}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	sendManifest(t, ts, compressed)
	plan := chunk.NewPlan(m)
	for n := 0; n < plan.NumChunks(); n++ {
		body, sum := chunkBody(t, src, m, n)
		if resp := sendChunk(t, ts, n, body, sum); resp.StatusCode != http.StatusNoContent {
			t.Fatalf("chunk %d: status %d", n, resp.StatusCode)
		}
	}
	ts.Close()

	// Mid-transfer, the root holds nothing at all.
	rootEntries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(rootEntries) != 0 {
		t.Errorf("root must stay clean during the transfer, found %v", rootEntries)
	}
	if _, err := os.Stat(filepath.Join(root, wire.StateDirName)); !os.IsNotExist(err) {
		t.Error("no .kipper-transfer-state may appear under root")
	}

	// Staging is flat: chunks/<n> files only, no mirrored source dirs.
	chunkEntries, err := os.ReadDir(filepath.Join(stateDir, "chunks"))
	if err != nil {
		t.Fatal(err)
	}
	if len(chunkEntries) != plan.NumChunks() {
		t.Errorf("staged %d chunk files, want %d", len(chunkEntries), plan.NumChunks())
	}
	for _, e := range chunkEntries {
		if e.IsDir() {
			t.Errorf("chunk staging must be flat, found dir %q", e.Name())
		}
	}
	if _, err := os.Stat(filepath.Join(stateDir, "deep")); !os.IsNotExist(err) {
		t.Error("state dir must not mirror the source tree")
	}

	// Restart on the same scratch dir resumes the bitmap.
	srv2, err := NewServer(root, stateDir, testToken, FSCommitter{}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	ts2 := httptest.NewServer(srv2.Handler())
	defer ts2.Close()
	if got := getState(t, ts2).CompletedChunks.Count(plan.NumChunks()); got != plan.NumChunks() {
		t.Errorf("restart lost resume state: %d of %d chunks", got, plan.NumChunks())
	}

	resp, report := finalize(t, ts2)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("finalize: status %d", resp.StatusCode)
	}
	for _, res := range report.Files {
		if !res.Match || res.Error != "" {
			t.Errorf("entry %s failed: %+v", res.Path, res)
		}
	}
	got, err := os.ReadFile(filepath.Join(root, "deep", "nested", "tree", "data.bin"))
	if err != nil || !bytes.Equal(got, bytes.Repeat([]byte("n"), 2500)) {
		t.Errorf("committed content mismatch: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, wire.StateDirName)); !os.IsNotExist(err) {
		t.Error("root must have no state dir after finalize")
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Error("scratch state dir must be removed after a clean finalize")
	}
}
