package cmd

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSSECWithoutExternalStorageIsRefused(t *testing.T) {
	cmd := &cobra.Command{}
	for _, name := range []string{"backup-storage-bucket", "backup-storage-region", "backup-storage-endpoint", "backup-storage-credentials", "backup-storage-profile", "backup-storage-sse-c-key-file"} {
		cmd.Flags().String(name, "", "")
	}
	cmd.Flags().Bool("backup-storage-sse-c", false, "")
	require.NoError(t, cmd.Flags().Set("backup-storage-sse-c", "true"))

	_, err := resolveBackupStorage(cmd)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--backup-storage-bucket")
}
