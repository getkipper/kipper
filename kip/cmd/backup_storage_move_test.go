package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func moveCommand(t *testing.T, set map[string]string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{}
	addMoveFlags(cmd.Flags())
	for name, value := range set {
		require.NoError(t, cmd.Flags().Set(name, value))
	}
	return cmd
}

func moveFlags(t *testing.T) map[string]string {
	t.Helper()
	creds := filepath.Join(t.TempDir(), "credentials")
	require.NoError(t, os.WriteFile(creds, []byte("[default]\naws_access_key_id = AKIAEXAMPLE\naws_secret_access_key = example-secret\n"), 0o600))
	return map[string]string{
		"bucket": "new-backups", "region": "hel1", "endpoint": "https://objects.example.com", "credentials": creds,
		"sse-c": "true", "export-new-key-to": "new.key", "export-previous-key-to": "previous.key", "export-sse-c-key-to": "sse.key",
	}
}

func TestMoveOptionsCarryTheNewBucketAndTheKeyFiles(t *testing.T) {
	opts, err := moveOptionsFromFlags(moveCommand(t, moveFlags(t)))

	require.NoError(t, err)
	assert.Equal(t, "new-backups", opts.Target.Bucket)
	assert.Equal(t, "hel1", opts.Target.Region)
	assert.Equal(t, "https://objects.example.com", opts.Target.Endpoint)
	assert.Equal(t, "AKIAEXAMPLE", opts.Target.AccessKeyID)
	assert.True(t, opts.Target.SSEC)
	assert.Equal(t, "new.key", opts.NewKeyFile)
	assert.Equal(t, "previous.key", opts.PreviousKeyFile)
	assert.Equal(t, "sse.key", opts.SSECKeyFile)
}

func TestMoveOptionsRequireTheBucketAndBothKeyFiles(t *testing.T) {
	for _, name := range []string{"bucket", "region", "export-new-key-to", "export-previous-key-to"} {
		t.Run(name, func(t *testing.T) {
			flags := moveFlags(t)
			delete(flags, name)

			_, err := moveOptionsFromFlags(moveCommand(t, flags))

			require.Error(t, err)
			assert.Contains(t, err.Error(), "--"+name)
		})
	}
}

func TestConfirmSummary(t *testing.T) {
	summary := []string{"From: bucket old-backups", "To:   bucket new-backups"}
	for _, tc := range []struct {
		name        string
		interactive bool
		yes         bool
		answer      string
		wantErr     string
	}{
		{"yes flag", false, true, "", ""},
		{"answered yes", true, false, "y\n", ""},
		{"answered no", true, false, "n\n", "canceled"},
		{"no terminal", false, false, "y\n", "--yes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer

			err := confirmSummary(strings.NewReader(tc.answer), &out, tc.interactive, tc.yes, summary, "Move the backups?", "moving the backups")

			assert.Contains(t, out.String(), "bucket new-backups", "the summary is shown before any answer")
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}
