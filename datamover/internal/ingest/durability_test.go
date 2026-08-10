package ingest

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/getkipper/kipper/datamover/internal/wire"
)

// TestChunkDurabilityOrdering asserts the write-ahead discipline: every span
// data file of a chunk is fsynced before the bitmap claims the chunk, the
// bitmap file is fsynced before its rename lands, and the state directory is
// fsynced last. A crash between any of these steps must leave the bitmap at
// or behind the data, never ahead of it.
func TestChunkDurabilityOrdering(t *testing.T) {
	var (
		mu    sync.Mutex
		order []string
	)
	orig := syncFile
	syncFile = func(f *os.File) error {
		mu.Lock()
		order = append(order, f.Name())
		mu.Unlock()
		return orig(f)
	}
	t.Cleanup(func() { syncFile = orig })

	src := t.TempDir()
	writeFile(t, src, "data.bin", bytes.Repeat([]byte("d"), 700), 0o644)
	m, compressed := buildManifest(t, src)
	ts := startServer(t, t.TempDir())
	sendManifest(t, ts, compressed)

	mu.Lock()
	order = nil // InitState persists an empty bitmap; only the chunk matters.
	mu.Unlock()

	body, sum := chunkBody(t, src, m, 0)
	if resp := sendChunk(t, ts, 0, body, sum); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("chunk: status %d", resp.StatusCode)
	}

	mu.Lock()
	defer mu.Unlock()
	classify := func(name string) string {
		switch {
		case strings.Contains(name, string(filepath.Separator)+chunksDirName+string(filepath.Separator)):
			return "data"
		case filepath.Base(name) == chunksDirName:
			return "chunkdir"
		case strings.HasSuffix(name, bitmapFileName+".tmp"):
			return "bitmap"
		case filepath.Base(name) == wire.StateDirName:
			return "dir"
		default:
			return "other"
		}
	}
	var kinds []string
	for _, name := range order {
		kinds = append(kinds, classify(name))
	}
	lastData, chunkDirIdx, bitmapIdx, dirIdx := -1, -1, -1, -1
	for i, k := range kinds {
		switch k {
		case "data":
			lastData = i
		case "chunkdir":
			chunkDirIdx = i
		case "bitmap":
			bitmapIdx = i
		case "dir":
			dirIdx = i
		case "other":
			t.Errorf("unexpected sync of %s", order[i])
		}
	}
	if lastData == -1 || chunkDirIdx == -1 || bitmapIdx == -1 || dirIdx == -1 {
		t.Fatalf("missing syncs, got sequence %v", kinds)
	}
	// The chunk's data file and its directory entry must both be durable
	// before the completion bit, and the bitmap before its own dir sync.
	if lastData >= chunkDirIdx || chunkDirIdx >= bitmapIdx || bitmapIdx >= dirIdx {
		t.Errorf("wrong durability order %v: data then chunk-dir must sync before the bitmap, the bitmap before the dir", kinds)
	}
}

// TestChunkRejectionSkipsBitmapSync asserts a corrupt chunk never reaches the
// bitmap persist path.
func TestChunkRejectionSkipsBitmapSync(t *testing.T) {
	var (
		mu    sync.Mutex
		order []string
	)
	orig := syncFile
	syncFile = func(f *os.File) error {
		mu.Lock()
		order = append(order, f.Name())
		mu.Unlock()
		return orig(f)
	}
	t.Cleanup(func() { syncFile = orig })

	src := t.TempDir()
	writeFile(t, src, "data.bin", bytes.Repeat([]byte("e"), 300), 0o644)
	_, compressed := buildManifest(t, src)
	ts := startServer(t, t.TempDir())
	sendManifest(t, ts, compressed)

	mu.Lock()
	order = nil
	mu.Unlock()

	body, _ := chunkBody(t, src, mustManifest(t, src), 0)
	resp := sendChunk(t, ts, 0, body, strings.Repeat("ab", 32))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("corrupt chunk: status %d", resp.StatusCode)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, name := range order {
		if strings.HasSuffix(name, bitmapFileName+".tmp") {
			t.Error("rejected chunk must not persist the bitmap")
		}
	}
}
