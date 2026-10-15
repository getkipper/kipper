package installer

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sweptPaths returns the arguments of the uninstall's rm -rf command.
func sweptPaths(t *testing.T, host *hostScript) []string {
	t.Helper()
	for _, c := range host.commands {
		if _, args, ok := strings.Cut(c, "rm -rf "); ok {
			return strings.Fields(args)
		}
	}
	t.Fatal("the uninstall swept nothing")
	return nil
}

func TestUninstallHostRunsTheAgentUninstallScriptToo(t *testing.T) {
	host := &routedRunner{}

	require.NoError(t, UninstallHost(host))

	assert.True(t, host.ran("/usr/local/bin/k3s-agent-uninstall.sh"), "a worker's k3s is removed by its own script")
	assert.True(t, host.ran("/usr/local/bin/k3s-uninstall.sh"))
}

func TestUninstallHostSweepsTheNetworkChangeRecordAndTheLock(t *testing.T) {
	host := &hostScript{}

	require.NoError(t, UninstallHost(host))

	swept := sweptPaths(t, host)
	assert.Contains(t, swept, "/var/lib/kipper", "a network change record left behind makes the next install recover a cluster that no longer exists")
	assert.Contains(t, swept, apiServerLockPath, "a lock held by a dead run must not outlive the cluster")
	assert.Contains(t, swept, apiServerLockPath+".guard")
	assert.NotContains(t, swept, "/etc/kipper", "the firewall claim stays with the firewall rules the uninstall keeps")
}
