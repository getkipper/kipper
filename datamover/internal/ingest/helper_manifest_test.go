package ingest

import (
	"testing"

	"github.com/getkipper/kipper/datamover/internal/manifest"
)

// mustManifest rebuilds the manifest for a source tree.
func mustManifest(t *testing.T, src string) *manifest.Manifest {
	t.Helper()
	m, err := manifest.BuildDir(src, 1024)
	if err != nil {
		t.Fatal(err)
	}
	return m
}
