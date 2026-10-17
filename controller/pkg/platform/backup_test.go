package platform

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBackupsConfigured(t *testing.T) {
	assert.False(t, BackupsConfigured(BackupNone))
	assert.True(t, BackupsConfigured(BackupInCluster))
	assert.True(t, BackupsConfigured(BackupExternal))
	assert.True(t, BackupsConfigured(""), "every cluster recorded before the field runs Velero")
	assert.Contains(t, BackupsNotConfigured, "--backup-storage-bucket")
}
