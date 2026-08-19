package cmd

import (
	"testing"

	"github.com/spf13/cobra"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/kip/internal/config"
)

func stageClusterForHosts(t *testing.T, domain string) {
	t.Helper()
	withFakeHome(t)
	require.NoError(t, config.Save(&config.Config{
		CurrentCluster: "shop",
		Clusters: []config.Cluster{
			{Name: "shop", Host: "203.0.113.10", Domain: domain, Kubeconfig: "/dev/null"},
		},
	}))
}

// A fresh command per call. Copying the real one shares its flag pointers, so
// a value set by one invocation is still set on the next, and --reset then
// arrives looking like it was combined with a pin.
func hostsCommand(t *testing.T, args ...string) error {
	t.Helper()
	cmd := &cobra.Command{RunE: runClusterHosts}
	cmd.Flags().String("cluster", "", "")
	cmd.Flags().String("console", "", "")
	cmd.Flags().String("console-api", "", "")
	cmd.Flags().String("dex", "", "")
	cmd.Flags().Bool("reset", false, "")
	require.NoError(t, cmd.Flags().Parse(args))
	return runClusterHosts(cmd, nil)
}

func hostsOf(t *testing.T) *config.Cluster {
	t.Helper()
	cfg, err := config.Load()
	require.NoError(t, err)
	entry := cfg.GetCluster("shop")
	require.NotNil(t, entry)
	return entry
}

// The catch-22 this exists for: the stored login host is wrong, so the operator
// cannot sign in, so every command that could read the right one from the
// cluster is unreachable. This one never contacts the cluster.
func TestClusterHostsPinsTheLoginHostWithoutTheCluster(t *testing.T) {
	stageClusterForHosts(t, "console.example.com")
	require.Equal(t, "dex.console.example.com", hostsOf(t).DexHost(),
		"the derived host is the wrong one an operator is stuck on")

	require.NoError(t, hostsCommand(t, "--dex", "dex.example.com"))

	assert.Equal(t, "dex.example.com", hostsOf(t).DexHost())
}

// Fixing one host must not unpin the two that already work, or an operator
// repairing the login loses the console and API on the way.
func TestClusterHostsLeavesTheOthersAlone(t *testing.T) {
	stageClusterForHosts(t, "console.example.com")
	require.NoError(t, hostsCommand(t, "--console", "console.example.com", "--console-api", "console-api.example.com"))

	require.NoError(t, hostsCommand(t, "--dex", "dex.example.com"))

	entry := hostsOf(t)
	assert.Equal(t, "console.example.com", entry.ConsoleHost())
	assert.Equal(t, "console-api.example.com", entry.ConsoleAPIHost())
	assert.Equal(t, "dex.example.com", entry.DexHost())
}

// A cluster that has moved back onto conventional names needs the pins gone,
// or the entry keeps pointing at hosts that no longer serve.
func TestClusterHostsResetFollowsTheDomainAgain(t *testing.T) {
	stageClusterForHosts(t, "example.com")
	require.NoError(t, hostsCommand(t, "--dex", "dex.old.example.com"))
	require.Equal(t, "dex.old.example.com", hostsOf(t).DexHost())

	require.NoError(t, hostsCommand(t, "--reset"))

	entry := hostsOf(t)
	assert.Equal(t, "dex.example.com", entry.DexHost())
	assert.Empty(t, entry.DexDomain)
}

// Clearing everything and pinning one thing are opposite instructions, and
// guessing which was meant would silently discard the other.
func TestClusterHostsRefusesResetTogetherWithAPin(t *testing.T) {
	stageClusterForHosts(t, "example.com")

	err := hostsCommand(t, "--reset", "--dex", "dex.example.com")

	require.Error(t, err)
}

// Reading is the default, so an operator can find out what kip believes before
// changing it.
func TestClusterHostsWithNoFlagsChangesNothing(t *testing.T) {
	stageClusterForHosts(t, "console.example.com")
	require.NoError(t, hostsCommand(t, "--dex", "dex.example.com"))

	require.NoError(t, hostsCommand(t))

	assert.Equal(t, "dex.example.com", hostsOf(t).DexHost())
}

// This is the command an operator reaches for when a stored hostname has
// already broken their login. Accepting another unusable one and reporting
// success leaves them exactly where they started, with no sign of why.
func TestClusterHostsRefusesValuesThatAreNotHostnames(t *testing.T) {
	for _, bad := range []string{
		"https://dex.example.com",
		"dex.example.com/auth",
		"dex.example.com:5556",
		"dex example.com",
		"operator@dex.example.com",
		"localhost",
		".dex.example.com",
	} {
		stageClusterForHosts(t, "example.com")

		err := hostsCommand(t, "--dex", bad)

		require.Error(t, err, "%q was accepted as a hostname", bad)
		assert.Equal(t, "dex.example.com", hostsOf(t).DexHost(), "nothing is stored when the value is refused")
	}
}

func TestClusterHostsAcceptsAnOrdinaryHostname(t *testing.T) {
	stageClusterForHosts(t, "example.com")

	require.NoError(t, hostsCommand(t, "--dex", "login.corp.example.com"))

	assert.Equal(t, "login.corp.example.com", hostsOf(t).DexHost())
}
