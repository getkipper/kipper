package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInstallProfileFlagRefusesUnknownNames(t *testing.T) {
	assert.NoError(t, checkInstallProfile(""))
	for _, p := range []string{"nano", "small", "medium", "large", "xlarge"} {
		assert.NoError(t, checkInstallProfile(p), p)
	}
	err := checkInstallProfile("huge")
	assert.ErrorContains(t, err, `unknown profile "huge"`)
	assert.ErrorContains(t, err, "nano, small, medium, large, xlarge")
}

func TestDrivenRequestCarriesTheProfile(t *testing.T) {
	cmd := installCmd
	require := assert.New(t)
	require.NoError(cmd.Flags().Set("profile", "small"))
	t.Cleanup(func() { _ = cmd.Flags().Set("profile", "") })
	req, err := drivenRequest(cmd, "203.0.113.10")
	require.NoError(err)
	require.Equal("small", req.Profile)
}

func TestInstallStorageFlagRefusesUnknownModes(t *testing.T) {
	assert.NoError(t, checkInstallStorage(""))
	assert.NoError(t, checkInstallStorage("local"))
	assert.NoError(t, checkInstallStorage("longhorn"))
	assert.ErrorContains(t, checkInstallStorage("nfs"), `unknown storage "nfs" (valid: local, longhorn)`)
}

func TestDrivenRequestCarriesTheStorage(t *testing.T) {
	cmd := installCmd
	require := assert.New(t)
	require.NoError(cmd.Flags().Set("storage", "longhorn"))
	t.Cleanup(func() { _ = cmd.Flags().Set("storage", "") })
	req, err := drivenRequest(cmd, "203.0.113.10")
	require.NoError(err)
	require.Equal("longhorn", req.Storage)
}

func TestDrivenRequestCarriesTheRepositoryKey(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", os.Getenv("HOME"))
	path := filepath.Join(t.TempDir(), "repository.key")
	require.NoError(t, os.WriteFile(path, []byte("k3y\n"), 0o600))
	cmd := installCmd
	require.NoError(t, cmd.Flags().Set("backup-repository-key-file", path))
	t.Cleanup(func() { _ = cmd.Flags().Set("backup-repository-key-file", "") })

	req, err := drivenRequest(cmd, "203.0.113.10")

	require.NoError(t, err)
	assert.Equal(t, "k3y", req.BackupRepositoryKey, "a recovery install from Windows keeps its key")
}
