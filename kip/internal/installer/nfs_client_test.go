package installer

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func runNFSClientCommand(t *testing.T, conf, param string) {
	t.Helper()
	out, err := exec.Command("sh", "-c", nfsClientCommand(conf, param)).CombinedOutput()
	require.NoError(t, err, string(out))
}

func TestNFSClientCommandTurnsDirectoryDelegationsOff(t *testing.T) {
	dir := t.TempDir()
	conf, param := filepath.Join(dir, "kipper-nfs.conf"), filepath.Join(dir, "directory_delegations")
	require.NoError(t, os.WriteFile(param, []byte("Y\n"), 0o600))

	runNFSClientCommand(t, conf, param)
	runNFSClientCommand(t, conf, param)

	saved, err := os.ReadFile(conf)
	require.NoError(t, err)
	assert.Equal(t, "options nfsv4 directory_delegations=0\n", string(saved), "the setting must survive a reboot, written once")
	live, err := os.ReadFile(param)
	require.NoError(t, err)
	assert.Equal(t, "N", strings.TrimSpace(string(live)), "mounts already in use must stop asking at once")
}

func TestNFSClientCommandWorksOnKernelsWithoutDirectoryDelegations(t *testing.T) {
	dir := t.TempDir()
	conf, param := filepath.Join(dir, "kipper-nfs.conf"), filepath.Join(dir, "directory_delegations")

	runNFSClientCommand(t, conf, param)

	assert.FileExists(t, conf, "a later kernel upgrade must find the setting in place")
	assert.NoFileExists(t, param)
}

func TestPrepareNodeOSSetsTheNFSClientOnlyForLonghorn(t *testing.T) {
	debian := []route{{contains: "cat /etc/os-release", out: "ID=debian\nVERSION_ID=\"13\"\n"}}

	longhorn := &routedRunner{routes: debian}
	require.NoError(t, prepareNodeOS(longhorn, false, true))
	assert.True(t, longhorn.ran(nfsClientConfPath), "Longhorn serves shared volumes over NFS\n%s", strings.Join(longhorn.commands, "\n"))

	local := &routedRunner{routes: debian}
	require.NoError(t, prepareNodeOS(local, false, false))
	assert.False(t, local.ran(nfsClientConfPath), "local storage has no shared volumes")
}
