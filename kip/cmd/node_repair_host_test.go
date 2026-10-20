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
)

// The repair command is the one an operator runs against a node added by an
// older kip, so it has to reach that node the same way every other command
// does. Reaching for ~/.ssh/id_rsa specifically fails on a normal install,
// which has an ed25519 key, and leaves the node exposed to the restart this
// command exists to prevent.
func TestRepairHostResolvesTheSSHKeyLikeEverythingElse(t *testing.T) {
	t.Setenv("KIP_SSH_KEY", "")
	cluster := &config.Cluster{Host: "203.0.113.10", SSHKey: "/keys/cluster-key"}

	cfg := nodeSSH("203.0.113.11", "", cluster)
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

	assert.Equal(t, "/keys/named", nodeSSH("203.0.113.11", "/keys/named", cluster).KeyPath)
	assert.Equal(t, "/keys/from-env", nodeSSH("203.0.113.11", "", cluster).KeyPath,
		"the environment beats the cluster config, as it does everywhere else")
}

func TestRepairHostConnectsAsRoot(t *testing.T) {
	assert.Equal(t, "root", nodeSSH("203.0.113.11", "", nil).User,
		"writing host configuration needs root, as kip node add does")
}

// repairHost is an Ubuntu node that logs NFS client and needrestart writes and,
// on the server, node annotations.
type repairHost struct {
	eachHost
	name  string
	log   *[]string
	names map[string]string
}

func (h *repairHost) Run(command string) (string, error) {
	switch {
	case command == "cat /etc/os-release":
		return "ID=ubuntu\nVERSION_ID=\"24.04\"\n", nil
	case strings.Contains(command, "/etc/modprobe.d/kipper-nfs.conf"):
		*h.log = append(*h.log, "nfs "+h.name)
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
		"nfs server", "configure server", "server records 'server'",
		"nfs worker-a", "configure worker-a", "server records 'worker-a'",
		"nfs worker-b", "configure worker-b", "server records 'worker-b'",
	}, log)
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
