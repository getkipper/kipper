package main

import (
	"strings"
	"testing"
)

// The datamover reference console-api falls back to when the Deployment does not
// override it. An organisation rename does not redirect container-registry
// package paths, so a wrong organisation here fails every volume transfer on a
// cluster whose console-api Deployment predates the env var.
func TestDatamoverFallbackIsPublishedUnderTheKipperOrg(t *testing.T) {
	const prefix = "ghcr.io/getkipper/"

	t.Setenv("DATAMOVER_IMAGE", "")
	if got := datamoverImage(); !strings.HasPrefix(got, prefix) {
		t.Errorf("datamover fallback is %q, which is not published under %s", got, prefix)
	}

	t.Setenv("DATAMOVER_IMAGE", "registry.example.com/operator-chosen:v3")
	if got := datamoverImage(); got != "registry.example.com/operator-chosen:v3" {
		t.Errorf("an operator override must win, got %q", got)
	}
}
