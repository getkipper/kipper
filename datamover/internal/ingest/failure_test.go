package ingest

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/getkipper/kipper/datamover/internal/wire"
)

// TestBitmapSyncFailureFailsChunk asserts a failed fsync surfaces as a 500
// instead of silently acknowledging an undurable chunk.
func TestBitmapSyncFailureFailsChunk(t *testing.T) {
	var armed bool
	orig := syncFile
	syncFile = func(f *os.File) error {
		if armed && strings.HasSuffix(f.Name(), bitmapFileName+".tmp") {
			return errors.New("disk on fire")
		}
		return orig(f)
	}
	t.Cleanup(func() { syncFile = orig })

	src := t.TempDir()
	writeFile(t, src, "data.bin", bytes.Repeat([]byte("f"), 200), 0o644)
	m, compressed := buildManifest(t, src)

	root := t.TempDir()
	srv, err := NewServer(root, "", testToken, FSCommitter{}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	ts := newTestTS(t, srv)
	sendManifest(t, ts, compressed)
	armed = true
	body, sum := chunkBody(t, src, m, 0)
	if resp := sendChunk(t, ts, 0, body, sum); resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("failed bitmap sync: got status %d, want 500", resp.StatusCode)
	}
	if getState(t, ts).CompletedChunks.Count(1) != 0 {
		t.Error("chunk must not count as complete when the bitmap could not be persisted")
	}
}

type failCommitter struct{}

func (failCommitter) Commit(context.Context, *State) (*wire.Report, error) {
	return nil, errors.New("target volume vanished")
}

func TestFinalizeCommitErrorFailsTransfer(t *testing.T) {
	src := t.TempDir()
	writeFile(t, src, "data.bin", []byte("abc"), 0o644)
	m, compressed := buildManifest(t, src)

	srv, err := NewServer(t.TempDir(), "", testToken, failCommitter{}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	ts := newTestTS(t, srv)
	sendManifest(t, ts, compressed)
	sendAllChunks(t, ts, src, m)
	resp, _ := finalize(t, ts)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("commit error: got status %d, want 500", resp.StatusCode)
	}
	var p wire.Progress
	presp := request(t, ts, http.MethodGet, "/kipper-transfer/t1/progress", testToken, nil, nil)
	if err := decodeJSON(presp, &p); err != nil {
		t.Fatal(err)
	}
	if p.Phase != wire.PhaseFailed {
		t.Errorf("after commit error: phase %q, want failed", p.Phase)
	}
}
