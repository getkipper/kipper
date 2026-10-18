package cmd

import (
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestKeyExportNeedsConsent(t *testing.T) {
	assert.NoError(t, confirmKeyExport(strings.NewReader(""), io.Discard, false, true, "key", "risk"))
	assert.Error(t, confirmKeyExport(strings.NewReader(""), io.Discard, false, false, "key", "risk"), "a script must pass --yes")
	assert.NoError(t, confirmKeyExport(strings.NewReader("y\n"), io.Discard, true, false, "key", "risk"))
	assert.Error(t, confirmKeyExport(strings.NewReader("n\n"), io.Discard, true, false, "key", "risk"))
}

func TestKeyExportPromptShowsTheCallersRisk(t *testing.T) {
	var out strings.Builder
	_ = confirmKeyExport(strings.NewReader("n\n"), &out, true, false, "key", "Anyone with this key can read every backup.")
	assert.Equal(t, "\n  Anyone with this key can read every backup. Export it? [y/N]: ", out.String())
}
