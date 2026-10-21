package installer

import (
	"os"
	"os/exec"
	"path/filepath"
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
	assert.NotContains(t, swept, "/etc/kipper", "the firewall claim stays with the firewall the uninstall keeps")
}

// stateScene is a host root with a populated /etc/kipper and stand-ins for
// flock and systemctl, so the cleanup script runs against real files.
type stateScene struct {
	t          *testing.T
	root, bin  string
	activeUnit string
}

func newStateScene(t *testing.T, files ...string) stateScene {
	s := stateScene{t: t, root: t.TempDir(), bin: t.TempDir()}
	for _, name := range files {
		path := filepath.Join(s.root, firewalldStateDir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
		require.NoError(t, os.WriteFile(path, nil, 0o600))
	}
	return s
}

func (s stateScene) run() string {
	s.t.Helper()
	for name, body := range map[string]string{
		"flock":     "exit 0",
		"systemctl": "echo '" + s.activeUnit + "'",
	} {
		//nolint:gosec // The command stand-ins must be executable.
		require.NoError(s.t, os.WriteFile(filepath.Join(s.bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755))
	}
	cmd := exec.Command("sh", "-c", clearKipperStateCommand(s.root)) //nolint:gosec // The test runs a generated cleanup script against temporary files.
	cmd.Env = []string{"PATH=" + s.bin + ":/usr/bin:/bin"}
	out, err := cmd.CombinedOutput()
	require.NoError(s.t, err, string(out))
	return string(out)
}

func (s stateScene) left() []string {
	entries, err := os.ReadDir(filepath.Join(s.root, firewalldStateDir))
	require.NoError(s.t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

var firewallRecords = []string{"firewall-managed", "firewalld-managed", "ufw.lock", "firewalld.lock", ufwFailOpenFile}

// TestClearKipperStateKeepsTheFirewallRecords checks that cleanup retains firewall
// state and lock files needed by pending waiters.
func TestClearKipperStateKeepsTheFirewallRecords(t *testing.T) {
	s := newStateScene(t, append([]string{"network-mode", "join/worker-1.json"}, firewallRecords...)...)

	out := s.run()

	assert.ElementsMatch(t, firewallRecords, s.left())
	assert.NotContains(t, out, firewallChangePendingMarker)
}

func TestClearKipperStateLeavesAPendingFirewallChangeAlone(t *testing.T) {
	for name, s := range map[string]stateScene{
		"a ufw reservation":       newStateScene(t, "network-mode", "ufw-run", "ufw.lock", "firewalld.lock"),
		"a firewalld reservation": newStateScene(t, "network-mode", "firewalld-run", "ufw.lock", "firewalld.lock"),
		"an active rollback unit": func() stateScene {
			s := newStateScene(t, "network-mode", "ufw.lock", "firewalld.lock")
			s.activeUnit = ufwRollbackUnit + "-abc123.timer loaded active waiting"
			return s
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			before := s.left()
			assert.Contains(t, s.run(), firewallChangePendingMarker)
			assert.ElementsMatch(t, before, s.left())
		})
	}
}

func TestClearKipperStateToleratesAHostWithoutKipperState(t *testing.T) {
	s := stateScene{t: t, root: t.TempDir(), bin: t.TempDir()}
	assert.Empty(t, s.run())
}

func TestUninstallHostReportsAPendingFirewallChange(t *testing.T) {
	host := &hostScript{reply: func(command string) (string, error) {
		if strings.Contains(command, "find /etc/kipper") {
			return firewallChangePendingMarker + "\n", nil
		}
		return "", nil
	}}
	require.NoError(t, UninstallHost(host), "A pending firewall change must allow uninstall to succeed.")
	assert.Contains(t, strings.Join(host.commands, "\n"), clearKipperStateCommand(""))
}

func TestUninstallHostRemovesTheAccessTableAfterK3sAndBeforeTheStateSweep(t *testing.T) {
	host := &routedRunner{}
	require.NoError(t, UninstallHost(host))
	k3s, removal, ops, clear := -1, -1, -1, -1
	for i, c := range host.commands {
		switch {
		case strings.Contains(c, "k3s-agent-uninstall.sh"):
			k3s = i
		case c == removeAccessCommand(""):
			removal = i
		case strings.Contains(c, "kipper-ops-remove.sh"):
			ops = i
		case c == clearKipperStateCommand(""):
			clear = i
		}
	}
	require.GreaterOrEqual(t, removal, 0, "the access table is removed so a reused host boots open")
	assert.Less(t, k3s, removal, "no workload is exposed while the guard still runs")
	assert.Equal(t, removal+1, ops, "the operator interface goes after the access table")
	assert.Equal(t, ops+1, clear, "while /etc/kipper/ops still says which UFW port to close")
}
