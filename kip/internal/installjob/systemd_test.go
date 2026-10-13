package installjob

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Write command output to the log file so the driver can replay it after reconnecting.
func TestSystemdRunArgs(t *testing.T) {
	got := systemdRunArgs("kipper-install-run-1", []string{"/usr/local/bin/kip", "install", "--local"}, "/root/.kip/jobs/run-1/install.log")

	assert.Equal(t, []string{
		"--unit", "kipper-install-run-1", "--collect", "--quiet",
		"-p", "User=root", "-p", "WorkingDirectory=/root", "--setenv=HOME=/root",
		"-p", "StandardOutput=append:/root/.kip/jobs/run-1/install.log",
		"-p", "StandardError=append:/root/.kip/jobs/run-1/install.log",
		"--", "/usr/local/bin/kip", "install", "--local",
	}, got)
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
