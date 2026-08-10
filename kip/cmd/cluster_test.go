package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/getkipper/kipper/kip/internal/config"
)

func TestResolveSSHKeyPrefersFlag(t *testing.T) {
	t.Setenv("KIP_SSH_KEY", "/env/key")
	cluster := &config.Cluster{SSHKey: "/cluster/key"}

	explicit, fallback := resolveSSHKey("/flag/key", cluster)

	assert.Equal(t, "/flag/key", explicit, "flag must win over env and config")
	assert.NotEmpty(t, fallback)
}

func TestResolveSSHKeyFallsToEnvWhenFlagEmpty(t *testing.T) {
	t.Setenv("KIP_SSH_KEY", "/env/key")
	cluster := &config.Cluster{SSHKey: "/cluster/key"}

	explicit, _ := resolveSSHKey("", cluster)

	assert.Equal(t, "/env/key", explicit, "env must win over config when flag is empty")
}

func TestResolveSSHKeyFallsToClusterConfigWhenFlagAndEnvEmpty(t *testing.T) {
	t.Setenv("KIP_SSH_KEY", "")
	cluster := &config.Cluster{SSHKey: "/cluster/key"}

	explicit, _ := resolveSSHKey("", cluster)

	assert.Equal(t, "/cluster/key", explicit)
}

func TestResolveSSHKeyEmptyExplicitWhenNothingSet(t *testing.T) {
	t.Setenv("KIP_SSH_KEY", "")
	cluster := &config.Cluster{}

	explicit, _ := resolveSSHKey("", cluster)

	// Empty explicit means: do not pass `-i + IdentitiesOnly`. Let ssh
	// consult the agent and the rest of its normal lookup chain.
	assert.Empty(t, explicit)
}

func TestResolveSSHKeyTolerantOfNilCluster(t *testing.T) {
	// kip install runs before any cluster entry exists, so the resolver
	// must accept nil and fall back to flag/env only.
	t.Setenv("KIP_SSH_KEY", "")
	explicit, fallback := resolveSSHKey("/flag/key", nil)

	assert.Equal(t, "/flag/key", explicit)
	assert.NotEmpty(t, fallback)
}

func TestResolveSSHKeyFallbackPointsAtDefaultEd25519Path(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("UserHomeDir unavailable")
	}
	_, fallback := resolveSSHKey("", nil)
	assert.Equal(t, filepath.Join(home, ".ssh", "id_ed25519"), fallback)
}
