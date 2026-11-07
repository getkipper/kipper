package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/installer"
	"github.com/getkipper/kipper/kip/internal/ssh"
)

// The repair command is the one an operator runs against a node added by an
// older kip, so it has to reach that node the same way every other command
// does. Reaching for ~/.ssh/id_rsa specifically fails on a normal install,
// which has an ed25519 key, and leaves the node exposed to the restart this
// command exists to prevent.
func TestRepairHostResolvesTheSSHKeyLikeEverythingElse(t *testing.T) {
	t.Setenv("KIP_SSH_KEY", "")
	cluster := &config.Cluster{Host: "203.0.113.10", SSHKey: "/keys/cluster-key"}

	cfg := mustNodeSSH(t, "", cluster)
	assert.Equal(t, "/keys/cluster-key", cfg.KeyPath,
		"the cluster's own key is what installed it and what should repair it")

	home, err := os.UserHomeDir()
	if err == nil {
		assert.Equal(t, filepath.Join(home, ".ssh", "id_ed25519"), cfg.FallbackKeyPath,
			"and the fallback is the one kip has always used, not id_rsa")
	}
}

func TestRepairHostPrefersTheFlag(t *testing.T) {
	t.Setenv("KIP_SSH_KEY", "/keys/from-env")
	cluster := &config.Cluster{SSHKey: "/keys/cluster-key"}

	assert.Equal(t, "/keys/named", mustNodeSSH(t, "/keys/named", cluster).KeyPath)
	assert.Equal(t, "/keys/from-env", mustNodeSSH(t, "", cluster).KeyPath,
		"the environment beats the cluster config, as it does everywhere else")
}

func TestRepairHostConnectsAsRoot(t *testing.T) {
	assert.Equal(t, "root", mustNodeSSH(t, "", nil).User,
		"writing host configuration needs root, as kip node add does")
}

// repairHost is an Ubuntu node that logs NFS client, image GC, k3s restart
// and needrestart writes and, on the server, node annotations. stale names the
// k3s unit whose kubelet started before the image GC drop-in.
type repairHost struct {
	eachHost
	name       string
	log        *[]string
	names      map[string]string
	stale      string
	restartErr error
}

func (h *repairHost) Run(command string) (string, error) {
	switch {
	case command == "cat /etc/os-release":
		return "ID=ubuntu\nVERSION_ID=\"24.04\"\n", nil
	case strings.Contains(command, "/etc/modprobe.d/kipper-nfs.conf"):
		*h.log = append(*h.log, "nfs "+h.name)
	case strings.Contains(command, "ExecMainStartTimestamp"):
		return h.stale, nil
	case strings.Contains(command, "kipper-image-gc.yaml"):
		*h.log = append(*h.log, "image-gc "+h.name)
	case strings.Contains(command, "systemctl restart"):
		*h.log = append(*h.log, "restart "+h.name)
		return "", h.restartErr
	case strings.HasPrefix(command, "mkdir -p /etc/needrestart"):
		*h.log = append(*h.log, "configure "+h.name)
	case strings.HasPrefix(command, "kubectl get nodes"):
		for id, name := range h.names {
			if strings.Contains(command, id) {
				return name + "\n", nil
			}
		}
	case strings.HasPrefix(command, "kubectl annotate node"):
		*h.log = append(*h.log, h.name+" records "+strings.Fields(command)[3])
	}
	return h.eachHost.Run(command)
}

func TestRepairHostOnEveryNodeConfiguresEachAndRecordsItFromTheServer(t *testing.T) {
	var log []string
	names := map[string]string{eachServerID: "server", eachWorkerA: "worker-a", eachWorkerB: "worker-b"}
	server := &repairHost{eachHost: eachHost{id: eachServerID}, name: "server", log: &log, names: names}
	ids := map[string]string{"203.0.113.11": eachWorkerA, "203.0.113.12": eachWorkerB}
	dial := func(rec installer.NodeRecord) (installer.FirewallRunner, func(), error) {
		return &repairHost{eachHost: eachHost{id: ids[rec.Host]}, name: rec.Name, log: &log}, func() {}, nil
	}

	require.NoError(t, repairHostOnNodes(eachRecords(), server, dial))

	assert.Equal(t, []string{
		"nfs server", "image-gc server", "configure server", "server records 'server'",
		"nfs worker-a", "image-gc worker-a", "configure worker-a", "server records 'worker-a'",
		"nfs worker-b", "image-gc worker-b", "configure worker-b", "server records 'worker-b'",
	}, log)
}

// The repair must restart a stale kubelet before recording the host as configured.
func TestRepairHostRestartsK3sOnlyWhereTheKubeletPredatesTheDropIn(t *testing.T) {
	var log []string
	names := map[string]string{eachServerID: "server", eachWorkerA: "worker-a", eachWorkerB: "worker-b"}
	server := &repairHost{eachHost: eachHost{id: eachServerID}, name: "server", log: &log, names: names}
	ids := map[string]string{"203.0.113.11": eachWorkerA, "203.0.113.12": eachWorkerB}
	stale := map[string]string{"203.0.113.11": "k3s-agent\n"}
	var dial nodeDialer = func(rec installer.NodeRecord) (installer.FirewallRunner, func(), error) {
		return &repairHost{eachHost: eachHost{id: ids[rec.Host]}, name: rec.Name, log: &log, stale: stale[rec.Host]}, func() {}, nil
	}

	out := captureStdout(t, func() { require.NoError(t, repairHostOnNodes(eachRecords(), server, dial)) })

	assert.Equal(t, []string{
		"nfs server", "image-gc server", "configure server", "server records 'server'",
		"nfs worker-a", "image-gc worker-a", "restart worker-a", "configure worker-a", "server records 'worker-a'",
		"nfs worker-b", "image-gc worker-b", "configure worker-b", "server records 'worker-b'",
	}, log)
	assert.Equal(t, 1, strings.Count(out, "Restarting k3s-agent"), out)
}

func TestRepairHostStopsWhenTheRestartFails(t *testing.T) {
	var log []string
	server := &repairHost{eachHost: eachHost{id: eachServerID}, name: "server", log: &log,
		names: map[string]string{eachServerID: "server"}, stale: "k3s\n", restartErr: errors.New("exit status 1")}

	err := repairNodeHost(server, server, "203.0.113.10", eachServerID)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "restarting k3s")
	assert.NotContains(t, log, "server records 'server'", "a node whose kubelet missed the drop-in is not recorded as configured")
}

// onceID answers the machine id probe once, then fails it.
type onceID struct {
	repairHost
	reads int
}

func (h *onceID) Run(command string) (string, error) {
	if command == "cat /etc/machine-id" {
		h.reads++
		if h.reads > 1 {
			return "", errors.New("connection reset")
		}
	}
	return h.repairHost.Run(command)
}

func TestRepairHostOnEveryNodeRecordsTheCheckedMachineID(t *testing.T) {
	var log []string
	names := map[string]string{eachServerID: "server", eachWorkerA: "worker-a", eachWorkerB: "worker-b"}
	server := &repairHost{eachHost: eachHost{id: eachServerID}, name: "server", log: &log, names: names}
	ids := map[string]string{"203.0.113.11": eachWorkerA, "203.0.113.12": eachWorkerB}
	dial := func(rec installer.NodeRecord) (installer.FirewallRunner, func(), error) {
		return &onceID{repairHost: repairHost{eachHost: eachHost{id: ids[rec.Host]}, name: rec.Name, log: &log}}, func() {}, nil
	}

	require.NoError(t, repairHostOnNodes(eachRecords(), server, dial))

	assert.Contains(t, log, "server records 'worker-a'",
		"a worker whose identity was checked is recorded, even if a later read of it would fail")
	assert.Contains(t, log, "server records 'worker-b'")
}

func mustNodeSSH(t *testing.T, flagValue string, cluster *config.Cluster) ssh.Config {
	t.Helper()
	cfg, err := nodeSSH("203.0.113.11", flagValue, cluster)
	require.NoError(t, err)
	return cfg
}
