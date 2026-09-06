package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/getkipper/kipper/kip/internal/config"
)

// The repair command is the one an operator runs against a node added by an
// older kip, so it has to reach that node the same way every other command
// does. Reaching for ~/.ssh/id_rsa specifically fails on a normal install,
// which has an ed25519 key, and leaves the node exposed to the restart this
// command exists to prevent.
func TestRepairHostResolvesTheSSHKeyLikeEverythingElse(t *testing.T) {
	t.Setenv("KIP_SSH_KEY", "")
	cluster := &config.Cluster{Host: "203.0.113.10", SSHKey: "/keys/cluster-key"}

	cfg := repairHostSSH("203.0.113.11", "", cluster)
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

	assert.Equal(t, "/keys/named", repairHostSSH("203.0.113.11", "/keys/named", cluster).KeyPath)
	assert.Equal(t, "/keys/from-env", repairHostSSH("203.0.113.11", "", cluster).KeyPath,
		"the environment beats the cluster config, as it does everywhere else")
}

func TestRepairHostConnectsAsRoot(t *testing.T) {
	assert.Equal(t, "root", repairHostSSH("203.0.113.11", "", nil).User,
		"writing host configuration needs root, as kip node add does")
}
