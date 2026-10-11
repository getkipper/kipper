package installer

import (
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stdinHost struct {
	commands []string
	stdin    []string
}

func (h *stdinHost) Run(command string) (string, error) {
	h.commands = append(h.commands, command)
	return "", nil
}

func (h *stdinHost) RunStdin(command string, r io.Reader) (string, error) {
	h.commands = append(h.commands, command)
	b, err := io.ReadAll(r)
	h.stdin = append(h.stdin, string(b))
	return "", err
}

func TestTheMinIOPasswordStaysOffTheCommandLine(t *testing.T) {
	host := &stdinHost{}
	const password = "synthetic-minio-value" //nolint:gosec // a test value, not a credential

	require.NoError(t, installInClusterMinIO(host, password))

	for _, c := range host.commands {
		assert.NotContains(t, c, password, "the password must stay out of process arguments")
	}
	assert.Contains(t, strings.Join(host.stdin, ""), password, "the manifest still carries it, on stdin")
}

func TestTheJoinTokenStaysOffTheCommandLine(t *testing.T) {
	cmd := agentInstallCommand("v1.35.1+k3s1", "203.0.113.10")

	assert.NotContains(t, cmd, "K3S_TOKEN=", "the token is read from stdin rather than assigned on the command line")
	assert.Contains(t, cmd, "read -r K3S_TOKEN")
	assert.Contains(t, cmd, "K3S_URL=https://203.0.113.10:6443")
	assert.Contains(t, cmd, "INSTALL_K3S_VERSION='v1.35.1+k3s1'")
}

func TestTheBucketJobPassesTheMinIOPasswordThroughTheEnvironment(t *testing.T) {
	host := &stdinHost{}

	require.NoError(t, createMinIOBucket(host))

	job := strings.Join(host.commands, "\n")
	assert.NotContains(t, job, "mc alias set", "mc alias set takes the password as an argument")
	assert.Contains(t, job, "MC_HOST_kipper=")
}

func TestTheBackupStoreDoesNotCheckForUpdates(t *testing.T) {
	host := &stdinHost{}
	require.NoError(t, installInClusterMinIO(host, "synthetic-minio-value"))

	assert.Contains(t, strings.Join(host.stdin, ""), "- name: MINIO_UPDATE\n              value: \"off\"")
}
