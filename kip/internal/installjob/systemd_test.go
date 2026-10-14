package installjob

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Write command output to the log file so the driver can replay it after reconnecting.
func TestSystemdRunArgs(t *testing.T) {
	got := systemdRunArgs("kipper-install-run-1", []string{"/usr/local/bin/kip", "install", "--local"}, "/root/.kip/jobs/run-1/install.log")

	assert.Equal(t, []string{
		"--unit", "kipper-install-run-1", "--collect", "--quiet",
		"-p", "User=root", "-p", "WorkingDirectory=/root", "--setenv=HOME=/root",
		"--", "/bin/sh", "-c", `exec "$@" >>"$0" 2>&1`, "/root/.kip/jobs/run-1/install.log",
		"/usr/local/bin/kip", "install", "--local",
	}, got)
}

func TestSystemdRunArgsAppendBothStreamsToTheLog(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "install log")
	require.NoError(t, os.WriteFile(logPath, []byte("earlier\n"), 0o600))

	args := systemdRunArgs("unit", []string{"/bin/sh", "-c", `echo "out $1"; echo "err $2" >&2`, "sh", "a b", "c'd"}, logPath)
	command := args[slices.Index(args, "--")+1:]
	require.NoError(t, exec.Command(command[0], command[1:]...).Run()) //nolint:gosec // a fixed test command

	got, err := os.ReadFile(logPath)
	require.NoError(t, err)
	assert.Equal(t, "earlier\nout a b\nerr c'd\n", string(got))
}

// Treat transitional and unknown states as busy to avoid concurrent installs.
func TestUnitBusy(t *testing.T) {
	for state, busy := range map[string]bool{
		"active": true, "activating": true, "deactivating": true, "reloading": true, "refreshing": true,
		"inactive": false, "failed": false,
	} {
		got, err := unitBusy(state + "\n")
		assert.NoError(t, err, state)
		assert.Equal(t, busy, got, state)
	}
	_, err := unitBusy("")
	assert.Error(t, err, "no answer from systemd is not proof the unit is gone")
}
