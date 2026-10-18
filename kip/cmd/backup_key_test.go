package cmd

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestKeyExportNeedsConsent(t *testing.T) {
	assert.NoError(t, confirmKeyExport(strings.NewReader(""), false, true))
	assert.Error(t, confirmKeyExport(strings.NewReader(""), false, false), "a script must pass --yes")
	assert.NoError(t, confirmKeyExport(strings.NewReader("y\n"), true, false))
	assert.Error(t, confirmKeyExport(strings.NewReader("n\n"), true, false))
}
