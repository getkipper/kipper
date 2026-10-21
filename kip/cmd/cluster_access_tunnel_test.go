package cmd

import (
	"bytes"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/transport"
)

const testDeviceKey = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="

func deviceCluster() *config.Cluster {
	return &config.Cluster{Name: "prod", Ops: &config.Ops{
		Device: "laptop", State: config.OpsActive, Address: "10.99.0.129",
		Nodes: []config.OpsNode{{
			MachineID: "m-1", Name: "server", Endpoint: "198.51.100.7:443", PublicKey: testDeviceKey,
			Address: "10.99.0.1", SSHPorts: []int{22}, Server: true,
		}},
	}}
}

func writeDeviceKey(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	path, err := config.OpsKeyPath("prod")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(testDeviceKey+"\n"), 0o600))
}

func TestOpsManifestComesFromTheLocalDevice(t *testing.T) {
	writeDeviceKey(t)
	m, err := opsManifest(deviceCluster())
	require.NoError(t, err)
	assert.Equal(t, testDeviceKey, m.PrivateKey, "the key file's trailing newline is dropped")
	assert.Equal(t, netip.MustParseAddr("10.99.0.129"), m.Address)
	require.Len(t, m.Nodes, 1)
	assert.Equal(t, "m-1", m.Nodes[0].MachineID)
	assert.Equal(t, netip.MustParseAddr("10.99.0.1"), m.Nodes[0].Address)
	assert.True(t, m.Nodes[0].Server)
}

func TestOpsManifestRefusesAMissingOrStaleDevice(t *testing.T) {
	writeDeviceKey(t)
	none := deviceCluster()
	none.Ops = nil
	_, err := opsManifest(none)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kip cluster access device add")

	stale := deviceCluster()
	stale.Ops.State = config.OpsStale
	_, err = opsManifest(stale)
	assert.Error(t, err, "a stale key is never used")

	pending := deviceCluster()
	pending.Ops.State = config.OpsPending
	_, err = opsManifest(pending)
	assert.NoError(t, err, "a pending key carries its own verification")

	noKey := deviceCluster()
	noKey.Name = "other"
	_, err = opsManifest(noKey)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reading the device key")
}

func TestPipeStdioKeepsTheReplyAfterStdinEnds(t *testing.T) {
	a, b := net.Pipe()
	go func() {
		got, _ := io.ReadAll(io.LimitReader(b, 4))
		_, _ = b.Write([]byte(strings.ToUpper(string(got))))
		_ = b.Close()
	}()
	var out bytes.Buffer
	require.NoError(t, pipeStdio(a, strings.NewReader("ping"), &out))
	assert.Equal(t, "PING", out.String())
}

func TestNodeSSHWithoutADeviceHasNoProxyCommand(t *testing.T) {
	cfg, err := nodeSSH("198.51.100.7", "", &config.Cluster{Name: "plain", Host: "198.51.100.7"})
	require.NoError(t, err)
	for _, o := range cfg.Options {
		assert.NotContains(t, o, "ProxyCommand")
	}
}

func TestViaStoresTheDefault(t *testing.T) {
	writeDeviceKey(t)
	t.Setenv("KIP_CLUSTER", "")
	require.NoError(t, config.Save(&config.Config{CurrentCluster: "prod", Clusters: []config.Cluster{*deviceCluster()}}))
	var out bytes.Buffer
	clusterAccessViaCmd.SetOut(&out)
	require.NoError(t, runClusterAccessVia(clusterAccessViaCmd, []string{"direct"}))
	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, "direct", cfg.GetCluster("prod").Ops.Via)
	assert.Contains(t, out.String(), "directly")

	require.NoError(t, runClusterAccessVia(clusterAccessViaCmd, []string{"auto"}))
	cfg, err = config.Load()
	require.NoError(t, err)
	assert.Empty(t, cfg.GetCluster("prod").Ops.Via, "auto is the default, so nothing is stored")

	assert.Error(t, runClusterAccessVia(clusterAccessViaCmd, []string{"vpn"}))
}

func TestRetargetSSHChoosesTheTargetsOwnTunnel(t *testing.T) {
	old := transport.Override
	transport.Override = transport.Tunnel
	t.Cleanup(func() { transport.Override = old })
	c := deviceCluster()
	c.Ops.Nodes[0].Host = "198.51.100.7"
	c.Ops.Nodes = append(c.Ops.Nodes, config.OpsNode{MachineID: "m-2", Name: "worker", Host: "198.51.100.8", Address: "10.99.0.2", SSHPorts: []int{2222}})
	server, err := nodeSSH("198.51.100.7", "", c)
	require.NoError(t, err)
	server.Options = append([]string{"BatchMode=yes"}, server.Options...)

	worker, err := retargetSSH(server, c, "198.51.100.8")
	require.NoError(t, err)
	assert.Equal(t, "198.51.100.8", worker.Host)
	joined := strings.Join(worker.Options, "\n")
	assert.Contains(t, joined, "'m-2'")
	assert.NotContains(t, joined, "'m-1'", "the server's ProxyCommand is gone")
	assert.Contains(t, worker.Options, "Port=2222")
	assert.NotContains(t, worker.Options, "Port=22")
	assert.Contains(t, worker.Options, "BatchMode=yes", "other options stay")

	transport.Override = transport.Direct
	direct, err := retargetSSH(server, c, "198.51.100.8")
	require.NoError(t, err)
	assert.Equal(t, []string{"BatchMode=yes"}, direct.Options, "a direct target carries no tunnel options")
}
