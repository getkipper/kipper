package installer

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const fencePath = "/etc/systemd/system/k3s-agent.service.d/90-kipper-fence.conf"

func TestFenceDropInIsSelfReferentialAndNamesTheRejoin(t *testing.T) {
	assert.Equal(t, fencePath, FenceDropInPath())
	body := fenceDropIn()
	assert.Contains(t, body, "[Unit]\nAssertPathExists=!"+fencePath+"\n")
	assert.Contains(t, body, "kip node add")
}

func TestUnitObjectPathEscapesLikeSystemd(t *testing.T) {
	assert.Equal(t, "/org/freedesktop/systemd1/unit/k3s_2dagent_2eservice", unitObjectPath("k3s-agent.service"))
	assert.Equal(t, "/org/freedesktop/systemd1/unit/k3s_2eservice", unitObjectPath("k3s.service"))
}

func assertsJSON(entries string) string {
	return "loaded\n" + `{"type":"a(sbbsi)","data":[` + entries + `]}`
}

const kipAssert = `["AssertPathExists",false,true,"` + fencePath + `",0]`

func TestParseAssertsFindsKipsAssert(t *testing.T) {
	require.NoError(t, checkFenceAsserts(assertsJSON(kipAssert)))
	require.NoError(t, checkFenceAsserts(assertsJSON(`["AssertPathIsDirectory",false,false,"/run",0],`+kipAssert)))
}

func TestParseAssertsRefusesAResetOrChangedList(t *testing.T) {
	cases := map[string]string{
		// A later drop-in that assigns AssertPathExists= empty resets the list.
		"reset by a later drop-in": assertsJSON(``),
		"not negated":              assertsJSON(`["AssertPathExists",false,false,"` + fencePath + `",0]`),
		"a condition-like trigger": assertsJSON(`["AssertPathExists",true,true,"` + fencePath + `",0]`),
		"another path":             assertsJSON(`["AssertPathExists",false,true,"/etc/other",0]`),
		"unit not loaded":          "not-found\n" + `{"type":"a(sbbsi)","data":[` + kipAssert + `]}`,
		"no json":                  "loaded\nFailed to get property Asserts",
	}
	for name, out := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Error(t, checkFenceAsserts(out))
		})
	}
}

func TestInstallFenceInterlockWritesThenVerifies(t *testing.T) {
	host := &routedRunner{routes: []route{{contains: "busctl", out: assertsJSON(kipAssert)}, {contains: "test -e", out: "present\n"}}}
	require.NoError(t, InstallFenceInterlock(host))
	require.Len(t, host.stdins, 1)
	assert.Equal(t, fenceDropIn(), host.stdins[0])
	write, verify := -1, -1
	for i, c := range host.commands {
		if strings.Contains(c, fencePath) && strings.Contains(c, "daemon-reload") && write < 0 {
			write = i
		}
		if strings.Contains(c, "busctl") {
			verify = i
		}
	}
	assert.True(t, write >= 0 && verify > write, "the drop-in is written and reloaded before the verification: %q", host.commands)
}

func TestVerifyFenceInterlockNeedsTheFileAsWellAsTheLoadedAssert(t *testing.T) {
	held := &routedRunner{routes: []route{{contains: "busctl", out: assertsJSON(kipAssert)}, {contains: "test -e", out: "present\n"}}}
	require.NoError(t, VerifyFenceInterlock(held))

	// An interrupted rejoin can delete the drop-in and stop before the
	// reload: the loaded assert then blocks nothing.
	gone := &routedRunner{routes: []route{{contains: "busctl", out: assertsJSON(kipAssert)}, {contains: "test -e", out: "absent\n"}}}
	err := VerifyFenceInterlock(gone)
	require.Error(t, err)
	assert.Contains(t, err.Error(), fencePath)
}

func TestInstallFenceInterlockFailsWhenTheAssertIsMissing(t *testing.T) {
	host := &routedRunner{routes: []route{{contains: "busctl", out: assertsJSON(``)}, {contains: "test -e", out: "present\n"}}}
	err := InstallFenceInterlock(host)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "AssertPathExists")
}

func TestQuiescenceScriptDoesNotMatchItself(t *testing.T) {
	// pgrep -f would find the shell running the script if the script named
	// what it looks for verbatim.
	script := quiescenceScript()
	assert.NotContains(t, script, "longhorn")
	assert.NotContains(t, script, "containerd-shim")
}

func TestParseQuiescence(t *testing.T) {
	require.NoError(t, parseQuiescence("agent=inactive\n"))
	require.NoError(t, parseQuiescence("agent=failed\n"))
	cases := map[string]string{
		"agent active":      "agent=active\n",
		"agent activating":  "agent=activating\n",
		"no agent line":     "",
		"shim left":         "agent=inactive\nS 4242 /var/lib/rancher/k3s/data/current/bin/containerd-shim-runc-v2 -namespace k8s.io\n",
		"engine left":       "agent=inactive\nL 5151 /engine-binaries/longhorn replica /host/var/lib/longhorn/replicas/pvc-1\n",
		"device mounted":    "agent=inactive\nM /dev/longhorn/pvc-1 /var/lib/kubelet/plugins/kubernetes.io/csi/driver.longhorn.io/abc/globalmount\n",
		"unexpected output": "agent=inactive\nsomething else\n",
	}
	for name, out := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Error(t, parseQuiescence(out))
		})
	}
}

func TestStopAgentRunsKillallThenVerifies(t *testing.T) {
	host := &routedRunner{routes: []route{{contains: "agent=", out: "agent=inactive\n"}}}
	require.NoError(t, StopFencedAgent(host))
	require.GreaterOrEqual(t, len(host.commands), 2)
	assert.Contains(t, host.commands[0], "k3s-killall.sh")
	assert.Contains(t, host.commands[len(host.commands)-1], "is-active")
}

func TestStopAgentReportsSurvivors(t *testing.T) {
	host := &routedRunner{routes: []route{{contains: "agent=", out: "agent=inactive\nS 4242 containerd-shim-runc-v2\n"}}}
	err := StopFencedAgent(host)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "4242")
}

func TestRemoveFenceInterlockReloadsAndChecksItIsGone(t *testing.T) {
	host := &routedRunner{routes: []route{{contains: "busctl", out: assertsJSON(``)}}}
	require.NoError(t, RemoveFenceInterlock(host))
	assert.True(t, host.ran("rm -f "+fencePath))
	assert.True(t, host.ran("daemon-reload"))

	stuck := &routedRunner{routes: []route{{contains: "busctl", out: assertsJSON(kipAssert)}}}
	assert.Error(t, RemoveFenceInterlock(stuck))
}

func TestHasFenceInterlock(t *testing.T) {
	yes := &routedRunner{routes: []route{{contains: "test -e", out: "present\n"}}}
	got, err := HasFenceInterlock(yes)
	require.NoError(t, err)
	assert.True(t, got)
	no := &routedRunner{routes: []route{{contains: "test -e", out: "absent\n"}}}
	got, err = HasFenceInterlock(no)
	require.NoError(t, err)
	assert.False(t, got)
	broken := &routedRunner{routes: []route{{contains: "test -e", err: errors.New("ssh: lost")}}}
	_, err = HasFenceInterlock(broken)
	assert.Error(t, err)
}

func TestAgentStartAndUpgradePathsNeverTouchTheFenceDropIn(t *testing.T) {
	for name, cmd := range map[string]string{
		"agent install": agentInstallCommand("v1.36.2+k3s1", "192.0.2.10"),
		"agent upgrade": agentUpgradeCommand("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "v1.36.1+k3s1", "v1.36.2+k3s1"),
	} {
		t.Run(name, func(t *testing.T) {
			assert.NotContains(t, cmd, "service.d")
			assert.NotContains(t, cmd, FenceDropInName)
		})
	}
}

func TestUninstallHostRemovesTheFenceDropInAfterK3s(t *testing.T) {
	host := &routedRunner{}
	require.NoError(t, UninstallHost(host))
	uninstall, remove := -1, -1
	for i, c := range host.commands {
		if strings.Contains(c, "k3s-agent-uninstall.sh") && uninstall < 0 {
			uninstall = i
		}
		if strings.Contains(c, fencePath) {
			remove = i
		}
	}
	assert.True(t, uninstall >= 0 && remove > uninstall, "the drop-in goes after k3s is uninstalled: %q", host.commands)
}

func TestClearLeftoverInterlock(t *testing.T) {
	left := &routedRunner{routes: []route{{contains: "test -e", out: "present\n"}, {contains: "busctl", out: assertsJSON(``)}}}
	cleared, err := ClearLeftoverInterlock(left)
	require.NoError(t, err)
	assert.True(t, cleared)
	assert.True(t, left.ran("rm -f "+fencePath))

	clean := &routedRunner{routes: []route{{contains: "test -e", out: "absent\n"}}}
	cleared, err = ClearLeftoverInterlock(clean)
	require.NoError(t, err)
	assert.False(t, cleared)
	assert.False(t, clean.ran("rm -f"))
}

func TestFirewallRollbackRestartsOnlyARunningAgent(t *testing.T) {
	// A fenced worker's agent is stopped, so a firewall rollback on it must
	// leave the agent stopped and the drop-in in place.
	script := firewalldRun{id: "abc", role: RoleWorker}.rollbackScript(true, true, false)
	assert.NotContains(t, script, FenceDropInName)
	for _, line := range strings.Split(script, "\n") {
		if strings.Contains(line, "systemctl restart k3s-agent") || strings.Contains(line, "systemctl start k3s-agent") {
			assert.Contains(t, line, "if systemctl is-active --quiet k3s-agent; then", "%q", line)
		}
	}
	assert.Contains(t, script, "systemctl restart k3s-agent")
}

func TestAssertsReadNamesDBusWhenBusctlCannotConnect(t *testing.T) {
	assert.Contains(t, assertsCommand(), "|| echo "+busctlFailed)
	err := checkFenceAsserts("loaded\nFailed to connect to bus: No such file or directory\n" + busctlFailed + "\n")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "D-Bus")
	assert.Contains(t, err.Error(), "Failed to connect to bus")
}
