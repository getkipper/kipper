package installer

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// k3sInstallHost simulates a fresh or existing Ubuntu installation for InstallK3s.
type k3sInstallHost struct {
	installed bool
	backend   string
	addrs     string
	external  string
	configErr bool
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
	case "ip -o -4 addr show":
		return h.addrs, nil
	}
	if strings.HasPrefix(cmd, "test ! -f /etc/rancher/k3s/config.yaml") {
		if h.configErr {
			return "Permission denied", errors.New("exit status 1")
		}
		return h.external, nil
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

func (h *k3sInstallHost) k3sConfig(t *testing.T) string {
	t.Helper()
	i := h.index("cat > /etc/rancher/k3s/config.yaml <<")
	require.NotEqual(t, -1, i, "the k3s config was never written")
	return h.commands[i]
}

const (
	privateAddrs = "1: lo    inet 127.0.0.1/8 scope host lo\n2: ens5    inet 172.31.28.180/20 brd 172.31.31.255 scope global dynamic ens5\n"
	publicAddrs  = "1: lo    inet 127.0.0.1/8 scope host lo\n2: eth0    inet 203.0.113.10/32 scope global dynamic eth0\n"
)

// Behind NAT, advertising the external address lets DNS checks use it instead
// of the private address on the node's interface.
func TestInstallK3sAdvertisesANATAddressAsExternal(t *testing.T) {
	h := &k3sInstallHost{addrs: privateAddrs}

	require.NoError(t, InstallK3s(h, "203.0.113.10", nil, false, false, StorageLonghorn))

	assert.Contains(t, h.k3sConfig(t), "node-external-ip: 203.0.113.10\n")
}

func TestInstallK3sLeavesALocalAddressAlone(t *testing.T) {
	h := &k3sInstallHost{addrs: publicAddrs}

	require.NoError(t, InstallK3s(h, "203.0.113.10", nil, false, false, StorageLonghorn))

	assert.NotContains(t, h.k3sConfig(t), "node-external-ip")
}

func TestInstallK3sResolvesAHostName(t *testing.T) {
	lookup := lookupHostIPs
	t.Cleanup(func() { lookupHostIPs = lookup })
	lookupHostIPs = func(string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("2001:db8::1"), net.ParseIP("203.0.113.10")}, nil
	}
	h := &k3sInstallHost{addrs: privateAddrs}

	require.NoError(t, InstallK3s(h, "server.example.com", nil, false, false, StorageLonghorn))

	assert.Contains(t, h.k3sConfig(t), "node-external-ip: 203.0.113.10\n")
}

// An empty address list leaves NAT detection inconclusive, so kip avoids
// advertising an unverified external address.
func TestInstallK3sSkipsTheExternalAddressWhenTheListIsUnreadable(t *testing.T) {
	h := &k3sInstallHost{}

	require.NoError(t, InstallK3s(h, "203.0.113.10", nil, false, false, StorageLonghorn))

	assert.NotContains(t, h.k3sConfig(t), "node-external-ip")
}

// Inconclusive detection preserves the last install's external address because
// rewriting the config without that setting would remove it.
func TestInstallK3sKeepsTheExternalAddressWhenTheListIsUnreadable(t *testing.T) {
	for _, host := range []string{"203.0.113.10", "2001:db8::10"} {
		h := &k3sInstallHost{installed: true, external: "disable:\n  - traefik\nnode-external-ip: \"203.0.113.10\" # public NAT address\n"}

		require.NoError(t, InstallK3s(h, host, nil, true, false, StorageLonghorn))

		assert.Contains(t, h.k3sConfig(t), "node-external-ip: 203.0.113.10\n", "host %s", host)
	}
}

// k3s accepts lists and dual-stack values, so kip preserves the existing form.
func TestInstallK3sKeepsAnyExistingExternalAddressForm(t *testing.T) {
	for existing, want := range map[string]string{
		"node-external-ip:\n  - 203.0.113.10\n":         "node-external-ip:\n- 203.0.113.10\n",
		"node-external-ip: 2001:db8::20\n":              "node-external-ip: 2001:db8::20\n",
		"node-external-ip: 203.0.113.10,2001:db8::20\n": "node-external-ip: 203.0.113.10,2001:db8::20\n",
	} {
		h := &k3sInstallHost{installed: true, external: existing}

		require.NoError(t, InstallK3s(h, "2001:db8::20", nil, true, false, StorageLonghorn))

		assert.Contains(t, h.k3sConfig(t), want, "existing %q", existing)
	}
}

func TestInstallK3sStopsWhenTheExistingConfigIsUnreadable(t *testing.T) {
	h := &k3sInstallHost{installed: true, configErr: true}

	err := InstallK3s(h, "203.0.113.10", nil, true, false, StorageLonghorn)

	require.Error(t, err)
	assert.Equal(t, -1, h.index("cat > /etc/rancher/k3s/config.yaml <<"), "an unreadable config must not be overwritten")
}

// With several IPv4 addresses, kip cannot tell which one SSH reached.
func TestInstallK3sSkipsAnAmbiguousHostName(t *testing.T) {
	lookup := lookupHostIPs
	t.Cleanup(func() { lookupHostIPs = lookup })
	lookupHostIPs = func(string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("203.0.113.10"), net.ParseIP("203.0.113.11")}, nil
	}
	h := &k3sInstallHost{addrs: privateAddrs}

	require.NoError(t, InstallK3s(h, "server.example.com", nil, false, false, StorageLonghorn))

	assert.NotContains(t, h.k3sConfig(t), "node-external-ip")
}
