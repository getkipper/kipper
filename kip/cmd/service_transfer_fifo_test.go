//go:build !windows

package cmd

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A failed export preserves a pipe created by the operator because cleanup
// only removes files created by kip.
func TestAFailedExportKeepsAPipeItDidNotCreate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dump.pipe")
	require.NoError(t, syscall.Mkfifo(path, 0o600))
	reader, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = reader.Close() })

	out, err := createExportFile(path)
	require.NoError(t, err)
	assert.Equal(t, os.Stderr, out.progress(), "messages stay out of a dump that goes to a stream")
	out.discard()

	_, err = os.Stat(path)
	assert.NoError(t, err, "the pipe is still there")
}
