package ingest

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/getkipper/kipper/datamover/internal/wire"
)

// withFastIORetry disables retry sleeps so tests exercising the backoff loop
// stay fast, and restores the seams afterwards.
func withFastIORetry(t *testing.T) {
	t.Helper()
	origSleep := ioSleep
	ioSleep = func(_ time.Duration) {}
	origLstat, origReadDir := lstatFn, readDirFn
	t.Cleanup(func() {
		ioSleep = origSleep
		lstatFn = origLstat
		readDirFn = origReadDir
	})
}

func TestIsTransientIO(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"eio", syscall.EIO, true},
		{"eremoteio numeric", syscall.Errno(121), true},
		{"wrapped eio in patherror", &os.PathError{Op: "lstat", Path: "/x", Err: syscall.EIO}, true},
		{"wrapped eremoteio", &os.PathError{Op: "readdirent", Path: "/x", Err: syscall.Errno(121)}, true},
		{"double wrapped eio", errors.New("outer: " + (&os.PathError{Err: syscall.EIO}).Error()), false},
		{"enoent", syscall.ENOENT, false},
		{"not-exist patherror", &os.PathError{Op: "open", Path: "/x", Err: syscall.ENOENT}, false},
		{"plain error", errors.New("boom"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTransientIO(tt.err); got != tt.want {
				t.Errorf("isTransientIO(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestRetryIOSucceedsAfterTransient(t *testing.T) {
	withFastIORetry(t)
	calls := 0
	err := retryIO(func() error {
		calls++
		if calls < 3 {
			return &os.PathError{Op: "lstat", Path: "/x", Err: syscall.EIO}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if calls != 3 {
		t.Errorf("expected 3 attempts, got %d", calls)
	}
}

func TestRetryIOGivesUpOnPersistent(t *testing.T) {
	withFastIORetry(t)
	calls := 0
	err := retryIO(func() error {
		calls++
		return syscall.EIO
	})
	if !errors.Is(err, syscall.EIO) {
		t.Errorf("expected the EIO to surface, got %v", err)
	}
	if calls != ioRetryAttempts {
		t.Errorf("expected %d attempts, got %d", ioRetryAttempts, calls)
	}
}

func TestRetryIONonTransientReturnsImmediately(t *testing.T) {
	withFastIORetry(t)
	calls := 0
	sentinel := errors.New("permission denied")
	err := retryIO(func() error {
		calls++
		return sentinel
	})
	if !errors.Is(err, sentinel) || calls != 1 {
		t.Errorf("non-transient error must not retry: err=%v calls=%d", err, calls)
	}
}

// TestDeletionPassRetriesTransientReaddir asserts a readdir seam that returns
// EIO twice then succeeds still completes the transfer and the deletion.
func TestDeletionPassRetriesTransientReaddir(t *testing.T) {
	withFastIORetry(t)
	src := t.TempDir()
	writeFile(t, src, "keep.txt", []byte("keep"), 0o644)
	m, compressed := buildManifest(t, src)

	root := t.TempDir()
	writeFile(t, root, "stray.txt", []byte("delete me"), 0o644)

	var mu sync.Mutex
	failsLeft := 2
	readDirFn = func(name string) ([]os.DirEntry, error) {
		mu.Lock()
		fail := name == root && failsLeft > 0
		if fail {
			failsLeft--
		}
		mu.Unlock()
		if fail {
			return nil, &os.PathError{Op: "readdirent", Path: name, Err: syscall.Errno(121)}
		}
		return os.ReadDir(name)
	}

	srv, err := NewServer(root, "", testToken, FSCommitter{}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	sendManifest(t, ts, compressed)
	sendAllChunks(t, ts, src, m)
	resp, report := finalize(t, ts)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("finalize: status %d", resp.StatusCode)
	}
	if failsLeft != 0 {
		t.Errorf("readdir seam should have been retried through both failures, %d left", failsLeft)
	}
	if len(report.Deleted) != 1 || report.Deleted[0] != "stray.txt" {
		t.Errorf("stray file must be deleted after the retry: %v", report.Deleted)
	}
	if _, err := os.Stat(filepath.Join(root, "stray.txt")); !os.IsNotExist(err) {
		t.Error("stray file must be gone")
	}
	if got, err := os.ReadFile(filepath.Join(root, "keep.txt")); err != nil || string(got) != "keep" {
		t.Errorf("committed file must be intact: %v %q", err, got)
	}
}

// TestDeletionPassPersistentIOFailsTransfer asserts that a permanently failing
// deletion walk fails finalize rather than reporting a clean sync it did not
// achieve. Full-sync is a correctness contract: a stray file that cannot be
// removed means the target is not the source, so the transfer must not
// complete and resume state must survive for a retry.
func TestDeletionPassPersistentIOFailsTransfer(t *testing.T) {
	withFastIORetry(t)
	src := t.TempDir()
	writeFile(t, src, "verified.bin", bytes.Repeat([]byte("v"), 2048), 0o644)
	m, compressed := buildManifest(t, src)

	root := t.TempDir()
	writeFile(t, root, "stray.txt", []byte("would be deleted"), 0o644)

	readDirFn = func(name string) ([]os.DirEntry, error) {
		if name == root {
			return nil, &os.PathError{Op: "readdirent", Path: name, Err: syscall.EIO}
		}
		return os.ReadDir(name)
	}

	stateDir := t.TempDir()
	srv, err := NewServer(root, stateDir, testToken, FSCommitter{}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	sendManifest(t, ts, compressed)
	sendAllChunks(t, ts, src, m)
	resp, _ := finalize(t, ts)
	if resp.StatusCode == http.StatusOK {
		t.Fatal("finalize must fail when the deletion pass cannot complete")
	}
	// Resume state must survive so the transfer can retry rather than
	// re-uploading from scratch.
	if _, err := os.Stat(filepath.Join(stateDir, "bitmap")); err != nil {
		t.Errorf("resume bitmap must survive a failed finalize: %v", err)
	}
}

// TestFinalizeStillFailsOnRealMismatch guards that IO resilience never masks a
// genuine content mismatch.
func TestFinalizeStillFailsOnRealMismatch(t *testing.T) {
	src := t.TempDir()
	writeFile(t, src, "db.sqlite", bytes.Repeat([]byte("n"), 1500), 0o644)
	m, compressed := buildManifest(t, src)
	root := t.TempDir()
	srv, err := NewServer(root, "", testToken, FSCommitter{}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	sendManifest(t, ts, compressed)
	sendAllChunks(t, ts, src, m)

	// Flip a byte in the staged chunk so assembly produces a real mismatch.
	chunkPath := filepath.Join(root, wire.StateDirName, "chunks", "0")
	data, err := os.ReadFile(chunkPath)
	if err != nil {
		t.Fatal(err)
	}
	data[10] ^= 0xff
	if err := os.WriteFile(chunkPath, data, 0o600); err != nil { //nolint:gosec // G703: test-controlled path
		t.Fatal(err)
	}
	resp, report := finalize(t, ts)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("finalize: status %d", resp.StatusCode)
	}
	if report.Files[0].Match || report.Files[0].Error == "" {
		t.Error("a real content mismatch must hard-fail verification, never be retried away")
	}
	if _, err := os.Stat(filepath.Join(root, "db.sqlite")); !os.IsNotExist(err) {
		t.Error("mismatched file must not be committed")
	}
}
