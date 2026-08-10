package export

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/getkipper/kipper/datamover/internal/chunk"
	"github.com/getkipper/kipper/datamover/internal/manifest"
	"github.com/getkipper/kipper/datamover/internal/wire"
)

func TestGetStateDigestMismatch(t *testing.T) {
	src := t.TempDir()
	writeFile(t, src, "a.txt", []byte("original"), 0o644)
	ts := startImport(t, t.TempDir())

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

	// The source changed after the manifest was sent: the client's encoding
	// no longer matches what the server holds.
	writeFile(t, src, "a.txt", []byte("changed!"), 0o644)
	c2 := newClient(t, ts.URL, src)
	c2.defaults()
	c2.raw, err = manifest.Encode(c2.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c2.getState(context.Background()); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Errorf("expected digest mismatch error, got: %v", err)
	}
}

func TestFinalizeBeforeAllChunks(t *testing.T) {
	src := t.TempDir()
	writeFile(t, src, "big.bin", make([]byte, 4096), 0o644)
	ts := startImport(t, t.TempDir())

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
	_, err = c.finalize(context.Background())
	var se *wire.StatusError
	if err == nil {
		t.Fatal("finalize with missing chunks must fail")
	}
	if !asStatus(err, &se) || se.Status != http.StatusConflict {
		t.Errorf("expected 409, got: %v", err)
	}
}

func asStatus(err error, target **wire.StatusError) bool {
	for err != nil {
		if se, ok := err.(*wire.StatusError); ok { //nolint:errorlint // manual unwrap loop
			*target = se
			return true
		}
		u, ok := err.(interface{ Unwrap() error }) //nolint:errorlint // manual unwrap loop
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func TestFSSourceOpenRangeMissingFile(t *testing.T) {
	src := &FSSource{Root: t.TempDir()}
	if _, err := src.OpenRange(context.Background(), "missing.txt", 0, 4); err == nil {
		t.Error("expected error for a missing file")
	}
}

func TestContextCancellationStopsUpload(t *testing.T) {
	src := t.TempDir()
	writeFile(t, src, "data.bin", make([]byte, 16*1024), 0o644)
	ts := startImport(t, t.TempDir())
	c := newClient(t, ts.URL, src)
	c.Concurrency = 1
	c.Backoff = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Run(ctx); err == nil {
		t.Error("cancelled context must abort the run")
	}
	if _, err := os.Stat(t.TempDir()); err != nil {
		t.Fatal(err)
	}
}
