package installer

import (
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// catRunner includes command output in errors, matching the SSH runner.
type catRunner struct {
	out string
	err error
}

func (c catRunner) Run(cmd string) (string, error) {
	if c.err != nil {
		return c.out, fmt.Errorf("running %q on 203.0.113.10: %w: %s", cmd, c.err, c.out)
	}
	return c.out, nil
}

func (c catRunner) RunStdin(cmd string, _ io.Reader) (string, error) { return c.Run(cmd) }

func TestFetchKubeconfigKeepsTheFileOutOfItsErrors(t *testing.T) {
	_, err := FetchKubeconfig(catRunner{out: "client-key-data: not-a-real-key", err: errors.New("exit status 255")}, "203.0.113.10")

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "not-a-real-key")
	assert.Contains(t, err.Error(), "exit status 255")
}

func TestFetchKubeconfigPointsAtTheHost(t *testing.T) {
	kc, err := FetchKubeconfig(catRunner{out: "server: https://127.0.0.1:6443\n"}, "203.0.113.10")

	require.NoError(t, err)
	assert.Equal(t, "server: https://203.0.113.10:6443\n", kc)
}
