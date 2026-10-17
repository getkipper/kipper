package cmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/getkipper/kipper/kip/internal/installer"
)

// TestBackupStorageResizeRefusesAClusterWithoutBackups checks that a cluster
// without backups gets a specific error. It has no MinIO volume, and advice
// about external storage would imply that it has a backup destination.
func TestBackupStorageResizeRefusesAClusterWithoutBackups(t *testing.T) {
	assert.False(t, backupsConfiguredOn(context.Background(), platformConfigWithBackup(installer.BackupNone)))
	assert.ErrorIs(t, backupResizeGate(context.Background(), platformConfigWithBackup(installer.BackupNone)), errBackupsNotConfigured)
	assert.NoError(t, backupResizeGate(context.Background(), platformConfigWithBackup(installer.BackupInCluster)))
}
