package export

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getkipper/kipper/datamover/internal/chunk"
	"github.com/getkipper/kipper/datamover/internal/ingest"
	"github.com/getkipper/kipper/datamover/internal/manifest"
	"github.com/getkipper/kipper/datamover/internal/wire"
)

const testToken = "test-token-1234"

func writeFile(t *testing.T, root, rel string, content []byte, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, content, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

// startImport runs a real ingest server over httptest with an FS committer.
func startImport(t *testing.T, root string) *httptest.Server {
	t.Helper()
	srv, err := ingest.NewServer(root, "", testToken, ingest.FSCommitter{}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func newClient(t *testing.T, baseURL, srcRoot string) *Client {
	t.Helper()
	m, err := manifest.BuildDir(srcRoot, 1024)
	if err != nil {
		t.Fatal(err)
	}
	return &Client{
		BaseURL:    baseURL,
		TransferID: "t1",
		Token:      testToken,
		Source:     &FSSource{Root: srcRoot},
		Manifest:   m,
		Backoff:    time.Millisecond,
		Logf:       t.Logf,
	}
}

func TestRoundTripVolume(t *testing.T) {
	src := t.TempDir()
	big := bytes.Repeat([]byte("0123456789abcdef"), 300) // 4800 bytes, spans chunks at 1KiB
	writeFile(t, src, "uploads/big.bin", big, 0o644)
	writeFile(t, src, "uploads/nested/small.txt", []byte("small"), 0o600)
	writeFile(t, src, "app.conf", []byte("key=value\n"), 0o755)
	writeFile(t, src, "empty.dat", nil, 0o644)
	mtime := time.Date(2026, 6, 1, 8, 30, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(src, "app.conf"), mtime, mtime); err != nil {
		t.Fatal(err)
	}

	dst := t.TempDir()
	writeFile(t, dst, "stale/old.log", []byte("must be deleted"), 0o644)
	writeFile(t, dst, "app.conf", []byte("outdated"), 0o644)

	ts := startImport(t, dst)
	c := newClient(t, ts.URL, src)
	if err := c.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	for rel, want := range map[string][]byte{
		"uploads/big.bin":          big,
		"uploads/nested/small.txt": []byte("small"),
		"app.conf":                 []byte("key=value\n"),
		"empty.dat":                {},
	} {
		got, err := os.ReadFile(filepath.Join(dst, filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("%s: %v", rel, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: content mismatch", rel)
		}
	}
	info, err := os.Stat(filepath.Join(dst, "app.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("app.conf mode = %o, want 755", info.Mode().Perm())
	}
	if info.ModTime().UnixNano() != mtime.UnixNano() {
		t.Errorf("app.conf mtime = %v, want %v", info.ModTime(), mtime)
	}
	if _, err := os.Stat(filepath.Join(dst, "stale")); !os.IsNotExist(err) {
		t.Error("stale dir survived the deletion pass")
	}
	if _, err := os.Stat(filepath.Join(dst, wire.StateDirName)); !os.IsNotExist(err) {
		t.Error("state dir survived a clean finalize")
	}
}

func TestSingleFileMode(t *testing.T) {
	src := t.TempDir()
	dump := bytes.Repeat([]byte("INSERT INTO t VALUES (1);\n"), 200)
	p := writeFile(t, src, "dump.sql.zst", dump, 0o600)

	dst := t.TempDir()
	ts := startImport(t, dst)
	m, err := manifest.BuildFile(p, 1024)
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{
		BaseURL:    ts.URL,
		TransferID: "t1",
		Token:      testToken,
		Source:     &FSSource{Root: filepath.Dir(p)},
		Manifest:   m,
		Backoff:    time.Millisecond,
	}
	if err := c.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "dump.sql.zst"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, dump) {
		t.Error("single-file content mismatch")
	}
}

// countingTransport counts chunk PUTs and can simulate a kill after a number
// of successful uploads.
type countingTransport struct {
	base      http.RoundTripper
	mu        sync.Mutex
	chunkPuts int
	failAfter int // -1 disables the simulated kill
}

func (c *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodPut && strings.Contains(req.URL.Path, "/chunk/") {
		c.mu.Lock()
		if c.failAfter >= 0 && c.chunkPuts >= c.failAfter {
			c.mu.Unlock()
			if req.Body != nil {
				_ = req.Body.Close()
			}
			return nil, errors.New("simulated kill")
		}
		c.chunkPuts++
		c.mu.Unlock()
	}
	return c.base.RoundTrip(req)
}

func (c *countingTransport) puts() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.chunkPuts
}

func TestResumeAfterKill(t *testing.T) {
	src := t.TempDir()
	writeFile(t, src, "data.bin", bytes.Repeat([]byte("x"), 8*1024), 0o644)

	dst := t.TempDir()
	srv1, err := ingest.NewServer(dst, "", testToken, ingest.FSCommitter{}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	ts1 := httptest.NewServer(srv1.Handler())

	const killAfter = 3
	t1 := &countingTransport{base: http.DefaultTransport, failAfter: killAfter}
	c := newClient(t, ts1.URL, src)
	c.HTTP = &http.Client{Transport: t1}
	c.Concurrency = 1
	c.MaxAttempts = 1
	if err := c.Run(context.Background()); err == nil {
		t.Fatal("expected the killed run to fail")
	}
	ts1.Close()
	if t1.puts() != killAfter {
		t.Fatalf("first run uploaded %d chunks, want %d", t1.puts(), killAfter)
	}

	// Simulated pod restart: a fresh server on the same root must reload the
	// bitmap, and the second run must send only the missing chunks.
	srv2, err := ingest.NewServer(dst, "", testToken, ingest.FSCommitter{}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	ts2 := httptest.NewServer(srv2.Handler())
	defer ts2.Close()

	t2 := &countingTransport{base: http.DefaultTransport, failAfter: -1}
	c2 := newClient(t, ts2.URL, src)
	c2.HTTP = &http.Client{Transport: t2}
	c2.Concurrency = 1
	if err := c2.Run(context.Background()); err != nil {
		t.Fatalf("resumed run: %v", err)
	}
	totalChunks := 8
	if got, want := t2.puts(), totalChunks-killAfter; got != want {
		t.Errorf("resumed run uploaded %d chunks, want %d (already-sent bytes must not be re-sent)", got, want)
	}
	got, err := os.ReadFile(filepath.Join(dst, "data.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, bytes.Repeat([]byte("x"), 8*1024)) {
		t.Error("assembled content mismatch after resume")
	}
}

// flakyHandler injects one 500 into the first chunk PUT.
func flakyHandler(next http.Handler) http.Handler {
	var once sync.Once
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		failed := false
		if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/chunk/") {
			once.Do(func() {
				failed = true
				http.Error(w, "transient", http.StatusInternalServerError)
			})
		}
		if !failed {
			next.ServeHTTP(w, r)
		}
	})
}

func TestChunkRetryOn500(t *testing.T) {
	src := t.TempDir()
	writeFile(t, src, "data.bin", bytes.Repeat([]byte("y"), 3000), 0o644)
	dst := t.TempDir()
	srv, err := ingest.NewServer(dst, "", testToken, ingest.FSCommitter{}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(flakyHandler(srv.Handler()))
	defer ts.Close()

	c := newClient(t, ts.URL, src)
	c.Concurrency = 1
	if err := c.Run(context.Background()); err != nil {
		t.Fatalf("Run should survive one 500 per chunk budget: %v", err)
	}
}

func TestChunkRetriesExhausted(t *testing.T) {
	src := t.TempDir()
	writeFile(t, src, "data.bin", []byte("payload"), 0o644)
	dst := t.TempDir()
	srv, err := ingest.NewServer(dst, "", testToken, ingest.FSCommitter{}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/chunk/") {
			http.Error(w, "broken", http.StatusInternalServerError)
			return
		}
		srv.Handler().ServeHTTP(w, r)
	}))
	defer ts.Close()

	c := newClient(t, ts.URL, src)
	err = c.Run(context.Background())
	if err == nil {
		t.Fatal("expected failure after retries are exhausted")
	}
	if !strings.Contains(err.Error(), "after 3 attempts") {
		t.Errorf("error should mention the attempt budget, got: %v", err)
	}
}

func TestFinalizeCatchesFlippedByte(t *testing.T) {
	src := t.TempDir()
	writeFile(t, src, "critical.db", bytes.Repeat([]byte("z"), 2048), 0o644)
	dst := t.TempDir()
	ts := startImport(t, dst)

	c := newClient(t, ts.URL, src)
	c.defaults()
	var err error
	c.raw, err = manifest.Encode(c.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	c.plan = chunk.NewPlan(c.Manifest)
	if err := c.postManifest(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, err := c.getState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.uploadChunks(context.Background(), state.CompletedChunks); err != nil {
		t.Fatal(err)
	}

	// Flip one byte in the staged chunk data before finalize.
	partial := filepath.Join(dst, wire.StateDirName, "chunks", "0")
	data, err := os.ReadFile(partial)
	if err != nil {
		t.Fatal(err)
	}
	data[100] ^= 0xff
	if err := os.WriteFile(partial, data, 0o600); err != nil { //nolint:gosec // G703: test-controlled path
		t.Fatal(err)
	}

	report, err := c.finalize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.verifyReport(report); err == nil {
		t.Fatal("verifyReport must fail on a flipped byte")
	} else if !strings.Contains(err.Error(), "hash mismatch") {
		t.Errorf("unexpected verification error: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dst, "critical.db")); !os.IsNotExist(statErr) {
		t.Error("corrupt file must not be renamed into place")
	}
}

func TestAuthRejected(t *testing.T) {
	src := t.TempDir()
	writeFile(t, src, "a.txt", []byte("data"), 0o644)
	ts := startImport(t, t.TempDir())

	c := newClient(t, ts.URL, src)
	c.Token = "wrong-token"
	err := c.Run(context.Background())
	if err == nil {
		t.Fatal("expected auth rejection")
	}
	var se *wire.StatusError
	if !errors.As(err, &se) || se.Status != http.StatusUnauthorized {
		t.Errorf("expected 401 status error, got: %v", err)
	}
}

func TestVerifyReport(t *testing.T) {
	m := &manifest.Manifest{ChunkSize: 1024, Entries: []manifest.Entry{
		{Path: "a.txt", Type: manifest.TypeFile, Size: 1, SHA256: "aa11"},
		{Path: "b.txt", Type: manifest.TypeFile, Size: 1, SHA256: "bb22"},
	}}
	ok := func(path, sha string) wire.FileResult {
		return wire.FileResult{Path: path, SHA256: sha, Match: true}
	}
	tests := []struct {
		name    string
		report  wire.Report
		wantErr string
	}{
		{
			name:   "all match",
			report: wire.Report{Files: []wire.FileResult{ok("a.txt", "aa11"), ok("b.txt", "bb22")}},
		},
		{
			name:    "file missing from report",
			report:  wire.Report{Files: []wire.FileResult{ok("a.txt", "aa11")}},
			wantErr: "missing from report",
		},
		{
			name: "hash mismatch",
			report: wire.Report{Files: []wire.FileResult{
				ok("a.txt", "aa11"),
				{Path: "b.txt", SHA256: "dead", Match: false},
			}},
			wantErr: "hash mismatch",
		},
		{
			name: "apply error",
			report: wire.Report{Files: []wire.FileResult{
				ok("a.txt", "aa11"),
				{Path: "b.txt", SHA256: "bb22", Match: true, Error: "applying ownership: permission denied"},
			}},
			wantErr: "permission denied",
		},
		{
			name:    "extra files in report",
			report:  wire.Report{Files: []wire.FileResult{ok("a.txt", "aa11"), ok("b.txt", "bb22"), ok("c.txt", "cc33")}},
			wantErr: "report covers 3 entries",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Client{Manifest: m}
			err := c.verifyReport(&tt.report)
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("got error %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// fakeObjectSource is an in-memory objectLister standing in for a bucket.
type fakeObjectSource struct {
	objects map[string][]byte
}

func (f *fakeObjectSource) list(context.Context) ([]objectInfo, error) {
	var out []objectInfo
	// Deliberately unsorted iteration: map order exercises manifest sorting.
	for k, v := range f.objects {
		out = append(out, objectInfo{Key: k, Size: int64(len(v)), MtimeUnixNano: 42})
	}
	return out, nil
}

func (f *fakeObjectSource) OpenRange(_ context.Context, path string, offset, length int64) (io.ReadCloser, error) {
	data, ok := f.objects[path]
	if !ok {
		return nil, errors.New("no such object")
	}
	return io.NopCloser(bytes.NewReader(data[offset : offset+length])), nil
}

func TestS3ObjectManifestAndRoundTrip(t *testing.T) {
	src := &fakeObjectSource{objects: map[string][]byte{
		"invoices/2026/07.pdf": bytes.Repeat([]byte("p"), 1500),
		"avatars/anna.png":     []byte("png-bytes"),
		"index.json":           []byte(`{"ok":true}`),
	}}
	m, err := buildObjectManifest(context.Background(), src, 1024)
	if err != nil {
		t.Fatal(err)
	}
	wantOrder := []string{"avatars/anna.png", "index.json", "invoices/2026/07.pdf"}
	for i, want := range wantOrder {
		if m.Entries[i].Path != want {
			t.Fatalf("entry %d: got %q, want %q", i, m.Entries[i].Path, want)
		}
	}
	// Determinism across builds despite map iteration order.
	m2, err := buildObjectManifest(context.Background(), src, 1024)
	if err != nil {
		t.Fatal(err)
	}
	raw1, _ := manifest.Encode(m)  //nolint:errcheck // struct encoding cannot fail here
	raw2, _ := manifest.Encode(m2) //nolint:errcheck // struct encoding cannot fail here
	if manifest.Digest(raw1) != manifest.Digest(raw2) {
		t.Error("object manifest is not deterministic")
	}

	dst := t.TempDir()
	ts := startImport(t, dst)
	c := &Client{
		BaseURL:    ts.URL,
		TransferID: "t1",
		Token:      testToken,
		Source:     src,
		Manifest:   m,
		Backoff:    time.Millisecond,
	}
	if err := c.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for key, want := range src.objects {
		got, err := os.ReadFile(filepath.Join(dst, filepath.FromSlash(key)))
		if err != nil {
			t.Errorf("%s: %v", key, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: content mismatch", key)
		}
	}
}

func TestEmptyManifestSyncsDeletions(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	writeFile(t, dst, "leftover.txt", []byte("gone"), 0o644)
	ts := startImport(t, dst)
	c := newClient(t, ts.URL, src)
	if err := c.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "leftover.txt")); !os.IsNotExist(err) {
		t.Error("full-sync semantics must delete files absent from an empty manifest")
	}
}
