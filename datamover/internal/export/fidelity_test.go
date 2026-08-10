package export

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRoundTripFilesystemFidelity covers the full layout contract: empty
// directories with their modes, symlinks reproduced verbatim (never
// followed), stale type conflicts converged, and deletion that touches
// nothing outside the transfer root.
func TestRoundTripFilesystemFidelity(t *testing.T) {
	src := t.TempDir()
	// Empty directory with a distinctive mode: load-bearing for MinIO.
	if err := os.MkdirAll(filepath.Join(src, "buckets", "empty-bucket"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(src, "buckets", "empty-bucket"), 0o500); err != nil { //nolint:gosec // G302: directories need the traversal bit
		t.Fatal(err)
	}
	writeFile(t, src, "releases/v2/app.bin", []byte("binary"), 0o755)
	if err := os.Symlink("releases/v2", filepath.Join(src, "current")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/var/run/outside.sock", filepath.Join(src, "abs.lnk")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, src, "sub/inner.txt", []byte("inner"), 0o644)

	// An unrelated directory next to the destination: nothing in it may be
	// created or deleted, even when symlinks under the root point at it.
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "precious.txt")
	if err := os.WriteFile(outsideFile, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}

	dst := t.TempDir()
	// Stale entries with type conflicts and escape attempts:
	writeFile(t, dst, "current", []byte("a file where a symlink belongs"), 0o644)
	if err := os.MkdirAll(filepath.Join(dst, "abs.lnk"), 0o750); err != nil {
		t.Fatal(err) // a dir where a symlink belongs
	}
	if err := os.Symlink(outside, filepath.Join(dst, "sub")); err != nil {
		t.Fatal(err) // a symlink where a dir belongs, pointing outside the root
	}
	if err := os.Symlink(outsideFile, filepath.Join(dst, "stale.lnk")); err != nil {
		t.Fatal(err) // stale symlink, absent from the manifest
	}
	if err := os.MkdirAll(filepath.Join(dst, "stale-empty"), 0o750); err != nil {
		t.Fatal(err) // stale empty dir, absent from the manifest
	}

	ts := startImport(t, dst)
	c := newClient(t, ts.URL, src)
	if err := c.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	info, err := os.Stat(filepath.Join(dst, "buckets", "empty-bucket"))
	if err != nil || !info.IsDir() {
		t.Fatalf("empty bucket dir missing: %v", err)
	}
	if info.Mode().Perm() != 0o500 {
		t.Errorf("empty dir mode = %o, want 500", info.Mode().Perm())
	}
	for link, wantTarget := range map[string]string{
		"current": "releases/v2",
		"abs.lnk": "/var/run/outside.sock",
	} {
		got, err := os.Readlink(filepath.Join(dst, link))
		if err != nil {
			t.Errorf("%s: %v", link, err)
			continue
		}
		if got != wantTarget {
			t.Errorf("%s points at %q, want %q", link, got, wantTarget)
		}
	}
	gotInner, err := os.ReadFile(filepath.Join(dst, "sub", "inner.txt"))
	if err != nil || string(gotInner) != "inner" {
		t.Errorf("sub/inner.txt: %v %q", err, gotInner)
	}
	if fi, err := os.Lstat(filepath.Join(dst, "sub")); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Error("sub must be a real directory, the stale symlink followed nothing")
	}
	if _, err := os.Lstat(filepath.Join(dst, "stale.lnk")); !os.IsNotExist(err) {
		t.Error("stale symlink must be deleted")
	}
	if _, err := os.Lstat(filepath.Join(dst, "stale-empty")); !os.IsNotExist(err) {
		t.Error("stale empty dir must be pruned")
	}
	// The escape targets survived untouched.
	if got, err := os.ReadFile(outsideFile); err != nil || string(got) != "keep me" {
		t.Errorf("file outside the root was touched: %v %q", err, got)
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 1 {
		t.Errorf("dir outside the root gained or lost entries: %v", err)
	}
}

func TestManifestDigestCoversLinkTarget(t *testing.T) {
	src := t.TempDir()
	if err := os.Symlink("v1", filepath.Join(src, "current")); err != nil {
		t.Fatal(err)
	}
	m1, err := manifestBuild(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(src, "current")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("v2", filepath.Join(src, "current")); err != nil {
		t.Fatal(err)
	}
	m2, err := manifestBuild(src)
	if err != nil {
		t.Fatal(err)
	}
	if m1 == m2 {
		t.Error("changing a link target must change the manifest digest")
	}
}

func TestRedirectRefused(t *testing.T) {
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://192.0.2.10/elsewhere", http.StatusTemporaryRedirect)
	}))
	defer redirecting.Close()

	src := t.TempDir()
	writeFile(t, src, "a.txt", []byte("data"), 0o644)
	c := newClient(t, redirecting.URL, src)
	c.HTTP = NewHTTPClient()
	c.Backoff = time.Millisecond
	err := c.Run(context.Background())
	if err == nil {
		t.Fatal("a redirecting target must fail the transfer")
	}
	if !strings.Contains(err.Error(), "refusing redirect") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestNewHTTPClientPosture(t *testing.T) {
	c := NewHTTPClient()
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatal("transport is not *http.Transport")
	}
	if tr.TLSClientConfig == nil || tr.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Error("TLS minimum version must be 1.2")
	}
	if c.CheckRedirect == nil {
		t.Fatal("CheckRedirect must be set")
	}
	req, err := http.NewRequest(http.MethodGet, "https://198.51.100.7/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CheckRedirect(req, nil); err == nil {
		t.Error("CheckRedirect must refuse every redirect")
	}
}
