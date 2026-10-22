package installer

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPlanServerK3sUpgrade(t *testing.T) {
	const pin = "v1.36.2+k3s1"
	tests := []struct {
		name      string
		installed string
		want      bool
		wantErr   []string
	}{
		{name: "one minor behind", installed: "v1.35.4+k3s1", want: true},
		{name: "an older patch of the same minor", installed: "v1.36.1+k3s1", want: true},
		{name: "already on the pin", installed: pin},
		{name: "newer than the pin", installed: "v1.36.3+k3s1", wantErr: []string{"v1.36.3+k3s1", "newer", pin, "Nothing was changed"}},
		{name: "two minors behind", installed: "v1.34.9+k3s1", wantErr: []string{"v1.34.9+k3s1", "one Kubernetes minor", "Nothing was changed"}},
		{name: "a different major", installed: "v0.36.2+k3s1", wantErr: []string{"v0.36.2+k3s1"}},
		{name: "no k3s installed", installed: "", wantErr: []string{"not installed", "kip install"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := planServerK3sUpgrade(tt.installed, pin)
			if tt.wantErr == nil {
				require.NoError(t, err)
				assert.Equal(t, tt.want, got)
				return
			}
			require.Error(t, err)
			for _, want := range tt.wantErr {
				assert.Contains(t, err.Error(), want)
			}
		})
	}
}

// k3sServer simulates the server host during a k3s upgrade. The install script
// moves the binary to k3sVersion at once, while the Node keeps reporting the
// old version as Ready for staleReads more reads, as a kubelet does until it
// posts its status after the restart.
type k3sServer struct {
	installed  string
	node       nodeVersion
	workers    map[string]nodeVersion
	records    []NodeRecord
	staleReads int
	stuck      bool
	restarted  bool
	commands   []string
}

func (s *k3sServer) Run(command string) (string, error) {
	s.commands = append(s.commands, command)
	switch {
	case strings.Contains(command, "k3s --version"):
		return fmt.Sprintf("k3s version %s (abc)\ngo version go1.26\n", s.installed), nil
	case strings.Contains(command, "get.k3s.io"):
		s.installed = k3sVersion
		s.restarted = true
		return "[INFO]  systemd: Starting k3s\n", nil
	case strings.Contains(command, "kubeletVersion"):
		if s.restarted && !s.stuck {
			if s.staleReads > 0 {
				s.staleReads--
			} else {
				s.node = nodeVersion{version: k3sVersion, ready: true}
			}
		}
		var b strings.Builder
		fmt.Fprintf(&b, "server-1\t%s\t%s\n", s.node.version, readyText(s.node.ready))
		for name, v := range s.workers {
			fmt.Fprintf(&b, "%s\t%s\t%s\n", name, v.version, readyText(v.ready))
		}
		return b.String(), nil
	case strings.Contains(command, "kubectl -n kube-system patch"):
		return "deployment.apps/coredns patched\n", nil
	}
	return "", fmt.Errorf("unexpected server command %q", command)
}

func (s *k3sServer) RunStdin(command string, _ io.Reader) (string, error) { return s.Run(command) }

func readyText(ready bool) string {
	if ready {
		return "True"
	}
	return "False"
}

func (s *k3sServer) ran(substr string) int {
	n := 0
	for _, c := range s.commands {
		if strings.Contains(c, substr) {
			n++
		}
	}
	return n
}

func serverOn(version string) *k3sServer {
	return &k3sServer{installed: version, node: nodeVersion{version: version, ready: true}, records: skewRecords()}
}

func TestUpgradeServerK3sRunsThePinnedInstallScript(t *testing.T) {
	s := serverOn("v1.35.4+k3s1")
	var out bytes.Buffer

	require.NoError(t, upgradeServerK3s(&out, s, s.records, "kip upgrade --k3s", time.Second, time.Millisecond))

	require.Equal(t, 1, s.ran("get.k3s.io"), "one install script run")
	for _, c := range s.commands {
		if strings.Contains(c, "get.k3s.io") {
			assert.Contains(t, c, "INSTALL_K3S_VERSION='"+k3sVersion+"'")
			assert.Contains(t, c, "INSTALL_K3S_EXEC='server'")
			assert.Contains(t, c, "INSTALL_K3S_FORCE_RESTART=true", "a binary replaced by an interrupted attempt still has to restart")
		}
	}
	assert.Zero(t, s.ran("config.yaml"), "the upgrade leaves the k3s config as install wrote it")
	assert.Positive(t, s.ran("kubectl -n kube-system patch"), "addon placement is restored after k3s reapplies its manifests")
	assert.Contains(t, out.String(), "from v1.35.4+k3s1 to "+k3sVersion)
	assert.Contains(t, out.String(), "Server runs k3s "+k3sVersion)
}

func TestUpgradeServerK3sWaitsPastAReadyNodeOnTheOldVersion(t *testing.T) {
	s := serverOn("v1.35.4+k3s1")
	s.staleReads = 3

	require.NoError(t, upgradeServerK3s(io.Discard, s, s.records, "kip upgrade --k3s", time.Second, time.Millisecond))

	assert.GreaterOrEqual(t, s.ran("kubeletVersion"), 4, "the old version's Ready status is not the upgraded server")
}

func TestUpgradeServerK3sNamesTheRerunWhenTheServerDoesNotComeBack(t *testing.T) {
	s := serverOn("v1.35.4+k3s1")
	s.stuck = true

	err := upgradeServerK3s(io.Discard, s, s.records, "kip upgrade --k3s", 20*time.Millisecond, time.Millisecond)

	require.Error(t, err)
	assert.Contains(t, err.Error(), k3sVersion)
	assert.Contains(t, err.Error(), "kip upgrade --k3s")
}

func TestUpgradeServerK3sLeavesAServerOnThePinRunning(t *testing.T) {
	s := serverOn(k3sVersion)
	var out bytes.Buffer

	require.NoError(t, upgradeServerK3s(&out, s, s.records, "kip upgrade --k3s", time.Second, time.Millisecond))

	assert.Zero(t, s.ran("get.k3s.io"))
	assert.Positive(t, s.ran("kubectl -n kube-system patch"), "a rerun repairs placement an interrupted attempt left out")
	assert.Contains(t, out.String(), "Server already runs k3s "+k3sVersion)
}

func TestUpgradeServerK3sRestartsAServerWhoseBinaryMovedAlone(t *testing.T) {
	s := serverOn("v1.35.4+k3s1")
	s.installed = k3sVersion

	require.NoError(t, upgradeServerK3s(io.Discard, s, s.records, "kip upgrade --k3s", time.Second, time.Millisecond))

	assert.Equal(t, 1, s.ran("INSTALL_K3S_FORCE_RESTART=true"), "the Node still runs the old release, so the server restarts")
}

func TestUpgradeServerK3sRefusesBeforeChangingAnything(t *testing.T) {
	t.Run("a newer server", func(t *testing.T) {
		s := serverOn("v1.37.0+k3s1")
		err := upgradeServerK3s(io.Discard, s, s.records, "kip upgrade --k3s", time.Second, time.Millisecond)
		require.Error(t, err)
		assert.Zero(t, s.ran("get.k3s.io"))
	})
	t.Run("a newer binary under an older running server", func(t *testing.T) {
		s := serverOn("v1.35.4+k3s1")
		s.installed = "v1.36.3+k3s1"
		err := upgradeServerK3s(io.Discard, s, s.records, "kip upgrade --k3s", time.Second, time.Millisecond)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "v1.36.3+k3s1")
		assert.Zero(t, s.ran("get.k3s.io"), "the install script would downgrade the binary")
	})
	t.Run("a newer worker while the server is already on the pin", func(t *testing.T) {
		s := serverOn(k3sVersion)
		s.records = skewRecords(
			workerRecord("worker-1", "203.0.113.11", NodeStateReady),
			workerRecord("worker-2", "203.0.113.12", NodeStateRemoving),
		)
		s.workers = map[string]nodeVersion{
			"worker-1": {version: "v1.35.4+k3s1", ready: true},
			"worker-2": {version: "v1.36.3+k3s1", ready: true},
		}

		err := upgradeServerK3s(io.Discard, s, s.records, "kip upgrade --k3s", time.Second, time.Millisecond)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "worker-2")
		assert.Zero(t, s.ran("get.k3s.io"))
		assert.Zero(t, s.ran("patch"))
	})
	t.Run("a worker the server would leave too far behind", func(t *testing.T) {
		s := serverOn("v1.35.4+k3s1")
		s.records = skewRecords(workerRecord("worker-1", "203.0.113.11", NodeStateReady))
		s.workers = map[string]nodeVersion{"worker-1": {version: "v1.32.9+k3s1", ready: true}}

		err := upgradeServerK3s(io.Discard, s, s.records, "kip upgrade --k3s", time.Second, time.Millisecond)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "worker-1")
		assert.Contains(t, err.Error(), "then run kip upgrade --k3s again")
		assert.Zero(t, s.ran("get.k3s.io"))
	})
}

func TestUpgradeAgentsNamesTheRerunCommand(t *testing.T) {
	c := newAgentCluster(
		map[string]string{"worker-1": "v1.35.6+k3s1"},
		workerRecord("worker-1", "203.0.113.11", NodeStateReady),
	)
	c.failOn = "203.0.113.11"

	err := upgradeAgents(io.Discard, c, c.onWorker, "kip upgrade --k3s", time.Second, time.Millisecond)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "Run kip upgrade --k3s again to resume")
}

func TestPlanK3sUpgrade(t *testing.T) {
	records := skewRecords(
		workerRecord("worker-1", "203.0.113.11", NodeStateReady),
		workerRecord("worker-2", "203.0.113.12", NodeStateReady),
		workerRecord("worker-3", "203.0.113.13", NodeStateRemoving),
	)

	plan, err := PlanK3sUpgrade(records, map[string]string{
		"server-1": "v1.35.4+k3s1", "worker-1": "v1.35.4+k3s1", "worker-2": k3sVersion, "worker-3": "v1.35.4+k3s1",
	}, "", "kip upgrade --k3s")

	require.NoError(t, err)
	assert.Equal(t, K3sUpgradePlan{Server: "v1.35.4+k3s1", MoveServer: true, Workers: []string{"worker-1"}}, plan,
		"only ready workers off the pin move; a worker being removed is left alone")
}

func TestPlanK3sUpgradeRefusesWorkersItCannotMove(t *testing.T) {
	_, err := PlanK3sUpgrade(skewRecords(workerRecord("worker-1", "203.0.113.11", NodeStateReady)),
		map[string]string{"server-1": "v1.35.4+k3s1", "worker-1": "v1.32.9+k3s1"}, "", "kip upgrade --k3s")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "worker-1")

	_, err = PlanK3sUpgrade(skewRecords(workerRecord("worker-1", "203.0.113.11", NodeStateReady)),
		map[string]string{"server-1": k3sVersion, "worker-1": "v1.36.3+k3s1"}, "", "kip upgrade --k3s")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "newer")
}

func TestPlanK3sUpgradeUsesTheFallbackWithoutAServerRecord(t *testing.T) {
	plan, err := PlanK3sUpgrade(nil, map[string]string{"old-server": "v1.35.4+k3s1"}, "v1.35.4+k3s1", "kip upgrade --k3s")

	require.NoError(t, err)
	assert.True(t, plan.MoveServer)
	assert.Equal(t, "v1.35.4+k3s1", plan.Server)
}

func TestPlanK3sUpgradeNeedsTheServersNode(t *testing.T) {
	_, err := PlanK3sUpgrade(skewRecords(), map[string]string{}, "", "kip upgrade --k3s")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "server-1")
}

func TestUpgradeServerK3sWaitsForAServerOnThePinToBeReady(t *testing.T) {
	s := serverOn(k3sVersion)
	s.node.ready = false

	err := upgradeServerK3s(io.Discard, s, s.records, "kip upgrade --k3s", 20*time.Millisecond, time.Millisecond)

	require.Error(t, err, "a retry must not hand an unready server on to the workers")
	assert.Contains(t, err.Error(), "kip upgrade --k3s")
	assert.Zero(t, s.ran("get.k3s.io"))
}

func TestPlanK3sUpgradeRefusesAReadyWorkerWithoutANode(t *testing.T) {
	_, err := PlanK3sUpgrade(skewRecords(workerRecord("worker-1", "203.0.113.11", NodeStateReady)),
		map[string]string{"server-1": k3sVersion}, "", "kip upgrade --k3s")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "worker-1")
	assert.Contains(t, err.Error(), "kip node add 203.0.113.11")
}

func TestCheckK3sUpgradeUsesASingleNodesVersionWithoutAServerRecord(t *testing.T) {
	s := serverOn("v1.34.9+k3s1")
	s.installed = "v1.35.4+k3s1"

	_, err := CheckK3sUpgrade(s, nil, "kip upgrade --k3s")

	require.Error(t, err, "the running server is two minors behind even though its binary is one")
	assert.Contains(t, err.Error(), "v1.34.9+k3s1")
}

func TestPlanK3sUpgradeRefusesWhileAWorkerIsJoining(t *testing.T) {
	_, err := PlanK3sUpgrade(skewRecords(workerRecord("worker-1", "203.0.113.11", NodeStateJoining)),
		map[string]string{"server-1": "v1.35.4+k3s1"}, "", "kip upgrade --k3s")

	require.Error(t, err, "a joining worker's Node name is provisional, so its version cannot be checked")
	assert.Contains(t, err.Error(), "worker-1")
	assert.Contains(t, err.Error(), "joining")
}

func TestUnrecordedIdentitiesFollowsTheUpgradeRules(t *testing.T) {
	records := skewRecords(workerRecord("worker-1", "203.0.113.11", NodeStateReady))
	worker := records[1]

	got, err := UnrecordedIdentities([]NodeIdentity{{Name: "worker-1", MachineID: worker.MachineID}}, records)
	require.NoError(t, err)
	assert.Empty(t, got)

	got, err = UnrecordedIdentities([]NodeIdentity{{Name: "worker-1", MachineID: strings.Repeat("f", 32)}}, records)
	require.NoError(t, err)
	assert.Equal(t, []string{"worker-1"}, got, "a matching name on another machine is not the recorded worker")
}
