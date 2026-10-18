package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBackupProtectionNamesBothLayers(t *testing.T) {
	sseC := backupProtection(map[string]string{"customerKeyEncryptionSecret": "kipper-backup-sse-key/customer-key"}, false)
	assert.Contains(t, sseC[0], "SSE-C")
	assert.Contains(t, sseC[1], "repository key")

	assert.Contains(t, backupProtection(map[string]string{"serverSideEncryption": "AES256"}, false)[0], "SSE-S3")
	assert.Contains(t, backupProtection(map[string]string{"s3Url": "https://s3.example.com"}, false)[0], "store's own settings")
	assert.Contains(t, backupProtection(nil, true)[1], "published default key", "an old cluster's volume data is not really protected by its key")
}
