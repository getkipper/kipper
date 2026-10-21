package cmd

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/getkipper/kipper/kip/internal/doctor"
	"github.com/getkipper/kipper/kip/internal/k8s"
)

func TestStatusPrintsNoUsageOnARuntimeError(t *testing.T) {
	assert.True(t, statusCmd.SilenceUsage, "an unreachable API is not a usage mistake")
}

func TestNodeMarkFlagsACordonedNode(t *testing.T) {
	assert.Equal(t, "✔", nodeMark(k8s.NodeInfo{Status: "Ready"}))
	assert.Equal(t, "⚠", nodeMark(k8s.NodeInfo{Status: "Ready", Cordoned: true}))
	assert.Equal(t, "✗", nodeMark(k8s.NodeInfo{Status: "NotReady", Cordoned: true}))
}

func TestWriteBackupHealth(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	state := doctor.BackupState{Configured: true,
		Schedules: []doctor.BackupSchedule{{Name: "velero-daily-apps", Cron: "0 3 * * *", Created: now.AddDate(0, -1, 0), LastBackup: now.Add(-9 * time.Hour)}},
		Runs:      []doctor.BackupRun{{Name: "velero-daily-apps-20261021030000", Schedule: "velero-daily-apps", Phase: "PartiallyFailed", Created: now.Add(-9 * time.Hour), Errors: 1}},
	}
	var out bytes.Buffer

	writeBackupHealth(&out, state, nil, now)

	assert.Contains(t, out.String(), "Backups:")
	assert.Contains(t, out.String(), "⚠  the last velero-daily-apps backup velero-daily-apps-20261021030000 partially failed with 1 error")

	out.Reset()
	writeBackupHealth(&out, doctor.BackupState{}, errors.New("forbidden"), now)
	assert.Contains(t, out.String(), "not checked (could not read the backups): forbidden")
}
