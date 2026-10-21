//go:build !windows

package opstunnel

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSecureDirRefusesASymlinkWithoutTouchingItsTarget(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	require.NoError(t, os.Mkdir(target, 0o755)) //nolint:gosec // the test checks that this mode survives
	link := filepath.Join(base, "link")
	require.NoError(t, os.Symlink(target, link))

	err := secureDir(link)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is not a directory")
	info, err := os.Stat(target)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), info.Mode().Perm(), "the link's target keeps its mode")
}

func TestSecureDirTightensAnOwnDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run")
	require.NoError(t, os.Mkdir(dir, 0o755)) //nolint:gosec // secureDir must tighten this
	require.NoError(t, secureDir(dir))
	info, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
}
