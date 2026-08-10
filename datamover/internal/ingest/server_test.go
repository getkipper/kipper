package ingest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/getkipper/kipper/datamover/internal/chunk"
	"github.com/getkipper/kipper/datamover/internal/manifest"
	"github.com/getkipper/kipper/datamover/internal/wire"
)

const testToken = "ingest-test-token"

func writeFile(t *testing.T, root, rel string, content []byte, mode os.FileMode) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, content, mode); err != nil {
		t.Fatal(err)
	}
}

func startServer(t *testing.T, root string) *httptest.Server {
	t.Helper()
	srv, err := NewServer(root, "", testToken, FSCommitter{}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func request(t *testing.T, ts *httptest.Server, method, path, token string, body []byte, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// buildManifest walks a source tree and returns the manifest with its
// compressed wire encoding.
func buildManifest(t *testing.T, srcRoot string) (*manifest.Manifest, []byte) {
	t.Helper()
	m, err := manifest.BuildDir(srcRoot, 1024)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := manifest.Encode(m)
	if err != nil {
		t.Fatal(err)
	}
	compressed, err := manifest.Compress(raw)
	if err != nil {
		t.Fatal(err)
	}
	return m, compressed
}

// chunkBody reads chunk n from the source tree and returns its compressed
// body and payload hash.
func chunkBody(t *testing.T, srcRoot string, m *manifest.Manifest, n int) ([]byte, string) {
	t.Helper()
	plan := chunk.NewPlan(m)
	var payload bytes.Buffer
	for _, sp := range plan.Spans(n) {
		f, err := os.Open(filepath.Join(srcRoot, filepath.FromSlash(sp.Path)))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(&payload, io.NewSectionReader(f, sp.FileOffset, sp.Length)); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
	}
	sum := sha256.Sum256(payload.Bytes())
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = enc.Close() }()
	return enc.EncodeAll(payload.Bytes(), nil), hex.EncodeToString(sum[:])
}

func sendManifest(t *testing.T, ts *httptest.Server, compressed []byte) {
	t.Helper()
	resp := request(t, ts, http.MethodPost, "/kipper-transfer/t1/manifest", testToken, compressed, nil)
	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body) //nolint:errcheck // test diagnostics
		t.Fatalf("manifest: status %d: %s", resp.StatusCode, body)
	}
}

func sendChunk(t *testing.T, ts *httptest.Server, n int, body []byte, sum string) *http.Response {
	t.Helper()
	return request(t, ts, http.MethodPut, fmt.Sprintf("/kipper-transfer/t1/chunk/%d", n), testToken, body,
		map[string]string{wire.HeaderChunkSHA256: sum, "Content-Encoding": "zstd"})
}

func getState(t *testing.T, ts *httptest.Server) wire.StateResponse {
	t.Helper()
	resp := request(t, ts, http.MethodGet, "/kipper-transfer/t1/state", testToken, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("state: status %d", resp.StatusCode)
	}
	var state wire.StateResponse
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestAuthRequiredOnEveryEndpoint(t *testing.T) {
	ts := startServer(t, t.TempDir())
	endpoints := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/kipper-transfer/t1/manifest"},
		{http.MethodGet, "/kipper-transfer/t1/state"},
		{http.MethodPut, "/kipper-transfer/t1/chunk/0"},
		{http.MethodPost, "/kipper-transfer/t1/finalize"},
		{http.MethodPost, "/kipper-transfer/t1/abort"},
		{http.MethodGet, "/kipper-transfer/t1/progress"},
	}
	for _, tok := range []string{"", "wrong-token"} {
		for _, ep := range endpoints {
			t.Run(fmt.Sprintf("%s %s token=%q", ep.method, ep.path, tok), func(t *testing.T) {
				resp := request(t, ts, ep.method, ep.path, tok, nil, nil)
				if resp.StatusCode != http.StatusUnauthorized {
					t.Errorf("got status %d, want 401", resp.StatusCode)
				}
			})
		}
	}
}

func TestCorruptChunkRejected(t *testing.T) {
	src := t.TempDir()
	writeFile(t, src, "data.bin", bytes.Repeat([]byte("q"), 500), 0o644)
	m, compressed := buildManifest(t, src)
	ts := startServer(t, t.TempDir())
	sendManifest(t, ts, compressed)

	body, _ := chunkBody(t, src, m, 0)
	wrongSum := sha256.Sum256([]byte("something else"))
	resp := sendChunk(t, ts, 0, body, hex.EncodeToString(wrongSum[:]))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("corrupt chunk: got status %d, want 422", resp.StatusCode)
	}
	if got := getState(t, ts).CompletedChunks.Count(1); got != 0 {
		t.Errorf("corrupt chunk must not be marked complete, bitmap has %d", got)
	}
}

func TestChunkPayloadLengthMismatch(t *testing.T) {
	src := t.TempDir()
	writeFile(t, src, "data.bin", bytes.Repeat([]byte("q"), 500), 0o644)
	m, compressed := buildManifest(t, src)
	ts := startServer(t, t.TempDir())
	sendManifest(t, ts, compressed)

	// Truncated payload with a hash that matches the truncation: length is
	// checked independently of the hash.
	short := bytes.Repeat([]byte("q"), 100)
	sum := sha256.Sum256(short)
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = enc.Close() }()
	resp := sendChunk(t, ts, 0, enc.EncodeAll(short, nil), hex.EncodeToString(sum[:]))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("short chunk: got status %d, want 422", resp.StatusCode)
	}
	long := bytes.Repeat([]byte("q"), 600)
	longSum := sha256.Sum256(long)
	resp = sendChunk(t, ts, 0, enc.EncodeAll(long, nil), hex.EncodeToString(longSum[:]))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("oversized chunk: got status %d, want 422", resp.StatusCode)
	}
	_ = m
}

func TestChunkIdempotentReplay(t *testing.T) {
	src := t.TempDir()
	writeFile(t, src, "data.bin", bytes.Repeat([]byte("r"), 300), 0o644)
	m, compressed := buildManifest(t, src)
	ts := startServer(t, t.TempDir())
	sendManifest(t, ts, compressed)

	body, sum := chunkBody(t, src, m, 0)
	if resp := sendChunk(t, ts, 0, body, sum); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("first chunk: status %d", resp.StatusCode)
	}
	if resp := sendChunk(t, ts, 0, body, sum); resp.StatusCode != http.StatusOK {
		t.Errorf("replayed chunk: got status %d, want 200", resp.StatusCode)
	}
}

func TestFinalizeRejectedWithMissingChunks(t *testing.T) {
	src := t.TempDir()
	writeFile(t, src, "data.bin", bytes.Repeat([]byte("s"), 3000), 0o644)
	_, compressed := buildManifest(t, src)
	ts := startServer(t, t.TempDir())
	sendManifest(t, ts, compressed)

	resp := request(t, ts, http.MethodPost, "/kipper-transfer/t1/finalize", testToken, nil, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("finalize with missing chunks: got status %d, want 409", resp.StatusCode)
	}
}

func TestProgressAndAbort(t *testing.T) {
	src := t.TempDir()
	writeFile(t, src, "data.bin", bytes.Repeat([]byte("u"), 2500), 0o644)
	m, compressed := buildManifest(t, src)
	root := t.TempDir()
	ts := startServer(t, root)

	progress := func() wire.Progress {
		resp := request(t, ts, http.MethodGet, "/kipper-transfer/t1/progress", testToken, nil, nil)
		var p wire.Progress
		if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
			t.Fatal(err)
		}
		return p
	}

	if p := progress(); p.Phase != wire.PhaseWaiting || p.TotalChunks != 0 {
		t.Errorf("before manifest: got %+v", p)
	}
	sendManifest(t, ts, compressed)
	body, sum := chunkBody(t, src, m, 1)
	if resp := sendChunk(t, ts, 1, body, sum); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("chunk: status %d", resp.StatusCode)
	}
	p := progress()
	if p.Phase != wire.PhaseReceiving || p.TotalChunks != 3 || p.ChunksDone != 1 || p.BytesDone != 1024 || p.TotalBytes != 2500 {
		t.Errorf("mid-transfer progress: got %+v", p)
	}

	resp := request(t, ts, http.MethodPost, "/kipper-transfer/t1/abort", testToken, nil, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("abort: status %d", resp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(root, wire.StateDirName)); !os.IsNotExist(err) {
		t.Error("abort must remove the state dir")
	}
	if p := progress(); p.Phase != wire.PhaseWaiting {
		t.Errorf("after abort: got phase %q", p.Phase)
	}
}

func TestManifestResendKeepsBitmapOnSameDigest(t *testing.T) {
	src := t.TempDir()
	writeFile(t, src, "data.bin", bytes.Repeat([]byte("v"), 2048), 0o644)
	m, compressed := buildManifest(t, src)
	ts := startServer(t, t.TempDir())
	sendManifest(t, ts, compressed)
	body, sum := chunkBody(t, src, m, 0)
	if resp := sendChunk(t, ts, 0, body, sum); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("chunk: status %d", resp.StatusCode)
	}

	sendManifest(t, ts, compressed)
	if got := getState(t, ts).CompletedChunks.Count(2); got != 1 {
		t.Errorf("same-digest manifest resend must keep the bitmap, got %d completed", got)
	}

	// A different manifest resets everything.
	writeFile(t, src, "extra.bin", []byte("new"), 0o644)
	_, compressed2 := buildManifest(t, src)
	sendManifest(t, ts, compressed2)
	if got := getState(t, ts).CompletedChunks.Count(3); got != 0 {
		t.Errorf("changed manifest must reset the bitmap, got %d completed", got)
	}
}

func TestManifestPathTraversalRejected(t *testing.T) {
	m := &manifest.Manifest{ChunkSize: 1024, Entries: []manifest.Entry{{
		Path:   "../../../etc/cron.d/evil",
		Type:   manifest.TypeFile,
		Size:   4,
		SHA256: "40b642cdc4c2a5623c78ae2c6e3fd8f0339102a2b0adb5992adafa4c07b431ff",
	}}}
	raw, err := manifest.Encode(m)
	if err != nil {
		t.Fatal(err)
	}
	compressed, err := manifest.Compress(raw)
	if err != nil {
		t.Fatal(err)
	}
	ts := startServer(t, t.TempDir())
	resp := request(t, ts, http.MethodPost, "/kipper-transfer/t1/manifest", testToken, compressed, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("traversal manifest: got status %d, want 400", resp.StatusCode)
	}
}

func TestTransferIDMismatchRejected(t *testing.T) {
	src := t.TempDir()
	writeFile(t, src, "data.bin", []byte("abcd"), 0o644)
	_, compressed := buildManifest(t, src)
	ts := startServer(t, t.TempDir())
	sendManifest(t, ts, compressed)

	resp := request(t, ts, http.MethodGet, "/kipper-transfer/other/state", testToken, nil, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("other transfer id: got status %d, want 409", resp.StatusCode)
	}
}

func TestStateSurvivesRestart(t *testing.T) {
	src := t.TempDir()
	writeFile(t, src, "data.bin", bytes.Repeat([]byte("w"), 2048), 0o644)
	m, compressed := buildManifest(t, src)
	root := t.TempDir()

	srv1, err := NewServer(root, "", testToken, FSCommitter{}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	ts1 := httptest.NewServer(srv1.Handler())
	sendManifest(t, ts1, compressed)
	body, sum := chunkBody(t, src, m, 0)
	if resp := sendChunk(t, ts1, 0, body, sum); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("chunk: status %d", resp.StatusCode)
	}
	ts1.Close()

	srv2, err := NewServer(root, "", testToken, FSCommitter{}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	ts2 := httptest.NewServer(srv2.Handler())
	defer ts2.Close()
	state := getState(t, ts2)
	if state.TotalChunks != 2 || state.CompletedChunks.Count(2) != 1 {
		t.Errorf("restarted server lost state: %+v", state)
	}
}

// fakeStore is an in-memory ObjectStore.
type fakeStore struct {
	bucketExists bool
	objects      map[string][]byte
}

func (f *fakeStore) EnsureBucket(context.Context) error {
	f.bucketExists = true
	return nil
}

func (f *fakeStore) Put(_ context.Context, key string, r io.Reader, size int64) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if int64(len(data)) != size {
		return errors.New("size mismatch")
	}
	f.objects[key] = data
	return nil
}

func (f *fakeStore) List(context.Context) ([]string, error) {
	keys := make([]string, 0, len(f.objects))
	for k := range f.objects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys, nil
}

func (f *fakeStore) Remove(_ context.Context, key string) error {
	delete(f.objects, key)
	return nil
}

func TestObjectCommitter(t *testing.T) {
	src := t.TempDir()
	writeFile(t, src, "reports/q2.csv", bytes.Repeat([]byte("1,2,3\n"), 100), 0o644)
	writeFile(t, src, "logo.svg", []byte("<svg/>"), 0o644)
	st := buildState(t, src, t.TempDir())

	store := &fakeStore{objects: map[string][]byte{"stale/old.bak": []byte("remove me")}}
	report, err := ObjectCommitter{Store: store}.Commit(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	if !store.bucketExists {
		t.Error("commit must ensure the target bucket exists")
	}
	for _, res := range report.Files {
		if !res.Match || res.Error != "" {
			t.Errorf("file %s: %+v", res.Path, res)
		}
	}
	if len(store.objects) != 2 {
		t.Errorf("bucket has %d objects, want 2 (stale object deleted)", len(store.objects))
	}
	if _, ok := store.objects["stale/old.bak"]; ok {
		t.Error("deletion pass must remove objects absent from the manifest")
	}
	if len(report.Deleted) != 1 || report.Deleted[0] != "stale/old.bak" {
		t.Errorf("unexpected deletion report: %v", report.Deleted)
	}
	if !bytes.Equal(store.objects["logo.svg"], []byte("<svg/>")) {
		t.Error("uploaded object content mismatch")
	}
}

func TestObjectCommitterDetectsCorruption(t *testing.T) {
	src := t.TempDir()
	writeFile(t, src, "asset.bin", bytes.Repeat([]byte("k"), 256), 0o644)
	st := buildState(t, src, t.TempDir())
	corrupt := bytes.Repeat([]byte("k"), 256)
	corrupt[10] ^= 0xff
	if err := os.WriteFile(st.ChunkPath(0), corrupt, 0o600); err != nil { //nolint:gosec // G703: test-controlled path
		t.Fatal(err)
	}

	store := &fakeStore{objects: map[string][]byte{}}
	report, err := ObjectCommitter{Store: store}.Commit(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	if report.Files[0].Match {
		t.Error("corrupt object must not verify")
	}
	if len(store.objects) != 0 {
		t.Error("corrupt object must not be uploaded")
	}
}

func TestNewServerRequiresToken(t *testing.T) {
	if _, err := NewServer(t.TempDir(), "", "", FSCommitter{}, nil); err == nil {
		t.Error("empty token must be rejected")
	}
}

func sendAllChunks(t *testing.T, ts *httptest.Server, src string, m *manifest.Manifest) {
	t.Helper()
	plan := chunk.NewPlan(m)
	for n := 0; n < plan.NumChunks(); n++ {
		body, sum := chunkBody(t, src, m, n)
		if resp := sendChunk(t, ts, n, body, sum); resp.StatusCode != http.StatusNoContent {
			t.Fatalf("chunk %d: status %d", n, resp.StatusCode)
		}
	}
}

func finalize(t *testing.T, ts *httptest.Server) (*http.Response, wire.Report) {
	t.Helper()
	resp := request(t, ts, http.MethodPost, "/kipper-transfer/t1/finalize", testToken, nil, nil)
	var report wire.Report
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&report); err != nil {
			t.Fatal(err)
		}
	}
	return resp, report
}

func TestFinalizeCommitsAndReports(t *testing.T) {
	src := t.TempDir()
	content := bytes.Repeat([]byte("m"), 1800)
	writeFile(t, src, "media/video.mp4", content, 0o644)
	writeFile(t, src, "notes.txt", []byte("keep"), 0o600)
	m, compressed := buildManifest(t, src)

	root := t.TempDir()
	writeFile(t, root, "obsolete/cache.tmp", []byte("stale"), 0o644)
	ts := startServer(t, root)
	sendManifest(t, ts, compressed)
	sendAllChunks(t, ts, src, m)

	resp, report := finalize(t, ts)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("finalize: status %d", resp.StatusCode)
	}
	// media (dir), media/video.mp4, notes.txt
	if len(report.Files) != 3 {
		t.Fatalf("report covers %d entries, want 3", len(report.Files))
	}
	for _, res := range report.Files {
		if !res.Match || res.Error != "" {
			t.Errorf("file %s failed: %+v", res.Path, res)
		}
	}
	if len(report.Deleted) != 1 || report.Deleted[0] != "obsolete/cache.tmp" {
		t.Errorf("unexpected deletion report: %v", report.Deleted)
	}
	got, err := os.ReadFile(filepath.Join(root, "media", "video.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Error("committed content mismatch")
	}
	if _, err := os.Stat(filepath.Join(root, wire.StateDirName)); !os.IsNotExist(err) {
		t.Error("state dir must be removed after a clean finalize")
	}
	if _, err := os.Stat(filepath.Join(root, "obsolete")); !os.IsNotExist(err) {
		t.Error("emptied stale dir must be pruned")
	}

	// A retried finalize replays the report instead of failing.
	resp2, report2 := finalize(t, ts)
	if resp2.StatusCode != http.StatusOK || len(report2.Files) != 3 {
		t.Errorf("finalize replay: status %d, %d entries", resp2.StatusCode, len(report2.Files))
	}

	var p wire.Progress
	presp := request(t, ts, http.MethodGet, "/kipper-transfer/t1/progress", testToken, nil, nil)
	if err := json.NewDecoder(presp.Body).Decode(&p); err != nil {
		t.Fatal(err)
	}
	if p.Phase != wire.PhaseCompleted {
		t.Errorf("after finalize: phase %q, want %q", p.Phase, wire.PhaseCompleted)
	}
}

func TestFinalizeVerificationFailureKeepsState(t *testing.T) {
	src := t.TempDir()
	writeFile(t, src, "db.sqlite", bytes.Repeat([]byte("n"), 900), 0o644)
	m, compressed := buildManifest(t, src)
	root := t.TempDir()
	ts := startServer(t, root)
	sendManifest(t, ts, compressed)
	sendAllChunks(t, ts, src, m)

	partial := filepath.Join(root, wire.StateDirName, "chunks", "0")
	data, err := os.ReadFile(partial)
	if err != nil {
		t.Fatal(err)
	}
	data[0] ^= 0xff
	if err := os.WriteFile(partial, data, 0o600); err != nil { //nolint:gosec // G703: test-controlled path
		t.Fatal(err)
	}

	resp, report := finalize(t, ts)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("finalize: status %d", resp.StatusCode)
	}
	if report.Files[0].Match {
		t.Error("flipped byte must fail verification")
	}
	if _, err := os.Stat(filepath.Join(root, "db.sqlite")); !os.IsNotExist(err) {
		t.Error("corrupt file must not be committed")
	}
	if _, err := os.Stat(filepath.Join(root, wire.StateDirName)); err != nil {
		t.Error("state must be kept after a failed finalize so the exporter can retry")
	}

	var p wire.Progress
	presp := request(t, ts, http.MethodGet, "/kipper-transfer/t1/progress", testToken, nil, nil)
	if err := json.NewDecoder(presp.Body).Decode(&p); err != nil {
		t.Fatal(err)
	}
	if p.Phase != wire.PhaseFailed {
		t.Errorf("after failed finalize: phase %q, want %q", p.Phase, wire.PhaseFailed)
	}
}

func TestNewMinioStoreEndpoint(t *testing.T) {
	if _, err := NewMinioStore("://bad", "b", "ak", "sk"); err == nil {
		t.Error("invalid endpoint must be rejected")
	}
	if _, err := NewMinioStore("no-scheme-host", "b", "ak", "sk"); err == nil {
		t.Error("endpoint without host must be rejected")
	}
	if _, err := NewMinioStore("http://minio.kipper-system.svc:9000", "b", "ak", "sk"); err != nil {
		t.Errorf("valid endpoint rejected: %v", err)
	}
}

func TestChunkRequestValidation(t *testing.T) {
	src := t.TempDir()
	writeFile(t, src, "data.bin", bytes.Repeat([]byte("h"), 500), 0o644)
	m, compressed := buildManifest(t, src)
	body, sum := chunkBody(t, src, m, 0)

	t.Run("chunk before manifest", func(t *testing.T) {
		ts := startServer(t, t.TempDir())
		if resp := sendChunk(t, ts, 0, body, sum); resp.StatusCode != http.StatusConflict {
			t.Errorf("got status %d, want 409", resp.StatusCode)
		}
	})
	t.Run("state before manifest", func(t *testing.T) {
		ts := startServer(t, t.TempDir())
		resp := request(t, ts, http.MethodGet, "/kipper-transfer/t1/state", testToken, nil, nil)
		if resp.StatusCode != http.StatusConflict {
			t.Errorf("got status %d, want 409", resp.StatusCode)
		}
	})

	ts := startServer(t, t.TempDir())
	sendManifest(t, ts, compressed)
	tests := []struct {
		name       string
		path       string
		body       []byte
		headers    map[string]string
		wantStatus int
	}{
		{"chunk number out of range", "/kipper-transfer/t1/chunk/99", body,
			map[string]string{wire.HeaderChunkSHA256: sum}, http.StatusBadRequest},
		{"chunk number not numeric", "/kipper-transfer/t1/chunk/abc", body,
			map[string]string{wire.HeaderChunkSHA256: sum}, http.StatusBadRequest},
		{"missing hash header", "/kipper-transfer/t1/chunk/0", body, nil, http.StatusBadRequest},
		{"invalid zstd body", "/kipper-transfer/t1/chunk/0", []byte("not zstd at all"),
			map[string]string{wire.HeaderChunkSHA256: sum}, http.StatusUnprocessableEntity},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := request(t, ts, http.MethodPut, tt.path, testToken, tt.body, tt.headers)
			if resp.StatusCode != tt.wantStatus {
				t.Errorf("got status %d, want %d", resp.StatusCode, tt.wantStatus)
			}
		})
	}
}

func TestManifestRejectsGarbageBody(t *testing.T) {
	ts := startServer(t, t.TempDir())
	resp := request(t, ts, http.MethodPost, "/kipper-transfer/t1/manifest", testToken, []byte("not zstd"), nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("got status %d, want 400", resp.StatusCode)
	}
}
