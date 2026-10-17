package installer

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// k3sInstallHost simulates a fresh or existing Ubuntu installation for InstallK3s.
type k3sInstallHost struct {
	installed bool
	backend   string
	commands  []string
}

func (h *k3sInstallHost) Run(cmd string) (string, error) {
	h.commands = append(h.commands, cmd)
	switch cmd {
	case "cat /etc/os-release":
		return "ID=ubuntu\nID_LIKE=debian\nVERSION_ID=\"26.04\"\n", nil
	case k3sVersionProbeCmd:
		if h.installed {
			return "k3s version " + k3sVersion + " (abc)\n", nil
		}
		return "KIP_K3S_ABSENT\n", nil
	}
	if strings.Contains(cmd, "backend-type") {
		if h.backend == "" {
			return "wireguard\n", nil
		}
		return h.backend + "\n", nil
	}
	return "", nil
}

func (h *k3sInstallHost) RunStdin(cmd string, _ io.Reader) (string, error) { return h.Run(cmd) }

func (h *k3sInstallHost) index(fragment string) int {
	for i, c := range h.commands {
		if strings.Contains(c, fragment) {
			return i
		}
	}
	return -1
}

// Worker joins use temporary bootstrap tokens, so installation needs no static agent token.
func TestInstallK3sWritesNoAgentToken(t *testing.T) {
	for _, installed := range []bool{false, true} {
		h := &k3sInstallHost{installed: installed}

		require.NoError(t, InstallK3s(h, "203.0.113.10", nil, installed, false, StorageLonghorn))

		for _, c := range h.commands {
			assert.NotContains(t, c, "agent-token", "installed=%v", installed)
		}
		assert.Equal(t, -1, h.index(k3sNetworkConfigPath), "installed=%v", installed)
	}
}

func TestWithoutOutputDropsTheRunnersOutput(t *testing.T) {
	err := withoutOutput(fmt.Errorf("running cat: %w: agent-token: secret-test-value", errors.New("exit status 255")))

	assert.EqualError(t, err, "exit status 255")
}
