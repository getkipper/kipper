package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/kip/internal/installer"
)

func TestSSECWithoutExternalStorageIsRefused(t *testing.T) {
	// Reusing recorded storage carries no SSE-C setting, so accepting either
	// flag value here would silently discard the choice.
	for _, value := range []string{"true", "false"} {
		t.Run(value, func(t *testing.T) {
			cmd := &cobra.Command{}
			for _, name := range []string{"backup-storage-bucket", "backup-storage-region", "backup-storage-endpoint", "backup-storage-credentials", "backup-storage-profile", "backup-storage-sse-c-key-file"} {
				cmd.Flags().String(name, "", "")
			}
			cmd.Flags().Bool("backup-storage-sse-c", false, "")
			require.NoError(t, cmd.Flags().Set("backup-storage-sse-c", value))

			_, err := resolveBackupStorage(cmd)

			require.Error(t, err)
			assert.Contains(t, err.Error(), "--backup-storage-bucket")
		})
	}
}

func externalStorageFlags(t *testing.T) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{}
	for _, name := range []string{"backup-storage-bucket", "backup-storage-region", "backup-storage-endpoint", "backup-storage-credentials", "backup-storage-profile", "backup-storage-sse-c-key-file"} {
		cmd.Flags().String(name, "", "")
	}
	cmd.Flags().Bool("backup-storage-sse-c", false, "")
	creds := filepath.Join(t.TempDir(), "credentials")
	require.NoError(t, os.WriteFile(creds, []byte("[default]\naws_access_key_id = AKIAEXAMPLE\naws_secret_access_key = example\n"), 0o600))
	require.NoError(t, cmd.Flags().Set("backup-storage-bucket", "backups"))
	require.NoError(t, cmd.Flags().Set("backup-storage-region", "hel1"))
	require.NoError(t, cmd.Flags().Set("backup-storage-endpoint", "https://hel1.example.com"))
	require.NoError(t, cmd.Flags().Set("backup-storage-credentials", creds))
	return cmd
}

func TestSSECIsOnlyOffWhenTurnedOff(t *testing.T) {
	cmd := externalStorageFlags(t)
	cfg, err := resolveBackupStorage(cmd)
	require.NoError(t, err)
	assert.False(t, cfg.SSECOff, "without the flag, the install turns SSE-C on where the store supports it")
	assert.False(t, cfg.SSEC, "without the flag, an unsupported store is not an error")

	require.NoError(t, cmd.Flags().Set("backup-storage-sse-c", "false"))
	cfg, err = resolveBackupStorage(cmd)
	require.NoError(t, err)
	assert.True(t, cfg.SSECOff)
}

func TestSSECTurnedOffWithAKeyFileIsRefused(t *testing.T) {
	cmd := externalStorageFlags(t)
	keyFile := filepath.Join(t.TempDir(), "sse.key")
	require.NoError(t, os.WriteFile(keyFile, []byte(installer.EncodeSSECKey(make([]byte, 32))), 0o600))
	require.NoError(t, cmd.Flags().Set("backup-storage-sse-c-key-file", keyFile))
	require.NoError(t, cmd.Flags().Set("backup-storage-sse-c", "false"))

	_, err := resolveBackupStorage(cmd)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "contradict")
}

func TestAPlainHTTPBackupEndpointOnThePublicInternetIsRefused(t *testing.T) {
	cmd := &cobra.Command{}
	for _, name := range []string{"backup-storage-bucket", "backup-storage-region", "backup-storage-endpoint", "backup-storage-credentials", "backup-storage-profile", "backup-storage-sse-c-key-file"} {
		cmd.Flags().String(name, "", "")
	}
	cmd.Flags().Bool("backup-storage-sse-c", false, "")
	require.NoError(t, cmd.Flags().Set("backup-storage-bucket", "backups"))
	require.NoError(t, cmd.Flags().Set("backup-storage-region", "eu-central"))
	require.NoError(t, cmd.Flags().Set("backup-storage-endpoint", "http://objects.example.com"))

	_, err := resolveBackupStorage(cmd)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "https://")
}

func readOnlyCommand(t *testing.T, set map[string]string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{}
	for _, name := range []string{"backup-storage-bucket", "backup-storage-region", "backup-storage-endpoint", "backup-storage-credentials", "backup-storage-profile", "backup-storage-sse-c-key-file"} {
		cmd.Flags().String(name, "", "")
	}
	cmd.Flags().Bool("backup-storage-sse-c", false, "")
	cmd.Flags().Bool("backup-storage-read-only", false, "")
	for name, value := range set {
		require.NoError(t, cmd.Flags().Set(name, value))
	}
	return cmd
}

func TestReadOnlyBackupStorageNeedsAnExternalBucket(t *testing.T) {
	_, err := resolveBackupStorage(readOnlyCommand(t, map[string]string{"backup-storage-read-only": "true"}))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--backup-storage-bucket")
}

func TestReadOnlyBackupStorageRefusesANewSSECKey(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, err := resolveBackupStorage(readOnlyCommand(t, map[string]string{
		"backup-storage-read-only": "true", "backup-storage-sse-c": "true",
		"backup-storage-bucket": "old-backups", "backup-storage-region": "eu-west-1",
	}))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--backup-storage-sse-c-key-file")
}

func TestReadOnlyBackupStorageIsKeptInTheConfig(t *testing.T) {
	creds := filepath.Join(t.TempDir(), "credentials")
	require.NoError(t, os.WriteFile(creds, []byte("[default]\naws_access_key_id = AKIAEXAMPLE\naws_secret_access_key = example-secret\n"), 0o600))

	cfg, err := resolveBackupStorage(readOnlyCommand(t, map[string]string{
		"backup-storage-read-only": "true", "backup-storage-bucket": "old-backups",
		"backup-storage-region": "eu-west-1", "backup-storage-credentials": creds,
	}))

	require.NoError(t, err)
	assert.True(t, cfg.ReadOnly)
}

func TestReadOnlyBackupStorageNeedsTheRepositoryKey(t *testing.T) {
	readOnly := &installer.BackupStorageConfig{Bucket: "old-backups", Region: "eu-west-1", ReadOnly: true}

	err := requireRecoveryKey(readOnly, false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--backup-repository-key-file")
	assert.NoError(t, requireRecoveryKey(readOnly, true))
	assert.NoError(t, requireRecoveryKey(&installer.BackupStorageConfig{Bucket: "backups", Region: "eu-west-1"}, false))
	assert.NoError(t, requireRecoveryKey(nil, false))
}
