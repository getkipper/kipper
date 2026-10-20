package installer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func skewRecords(workers ...NodeRecord) []NodeRecord {
	records := []NodeRecord{{Name: "server-1", Host: "203.0.113.10", MachineID: serverMachine, Role: NodeRoleServer, State: NodeStateReady}}
	return append(records, workers...)
}

func workerRecord(name, host, state string) NodeRecord {
	return NodeRecord{Name: name, Host: host, MachineID: strings.Repeat("a", 31) + name[len(name)-1:], Role: NodeRoleWorker, State: state}
}

func TestCheckK3sSkew(t *testing.T) {
	const pin = k3sVersion
	worker := workerRecord("worker-1", "203.0.113.11", NodeStateReady)
	tests := []struct {
		name      string
		installed string
		agent     string
		wantErr   []string
	}{
		{name: "agent at the server's version before a one-minor upgrade", installed: "v1.35.6+k3s1", agent: "v1.35.6+k3s1"},
		{name: "agent three minors behind the planned server", installed: "v1.35.6+k3s1", agent: "v1.33.9+k3s1"},
		{name: "agent four minors behind the planned server", installed: "v1.35.6+k3s1", agent: "v1.32.9+k3s1",
			wantErr: []string{"worker-1", "v1.32.9+k3s1", pin, "three minor versions", "kip node add 203.0.113.11"}},
		{name: "agent newer than the planned server", installed: "v1.36.2+k3s1", agent: "v1.36.3+k3s1",
			wantErr: []string{"worker-1", "v1.36.3+k3s1", "newer", pin}},
		{name: "agent newer minor than the planned server", installed: "v1.35.6+k3s1", agent: "v1.37.0+k3s1",
			wantErr: []string{"worker-1", "newer"}},
		{name: "a lag the server step does not widen is left to the agent upgrade", installed: pin, agent: "v1.32.9+k3s1"},
		{name: "a skipped server step is checked against the installed version", installed: "v1.34.9+k3s1", agent: "v1.31.0+k3s1"},
		{name: "a skipped server step still refuses a newer agent", installed: "v1.34.9+k3s1", agent: "v1.35.0+k3s1",
			wantErr: []string{"newer", "v1.34.9+k3s1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			versions := map[string]nodeVersion{
				"server-1": {version: tt.installed, ready: true},
				"worker-1": {version: tt.agent, ready: true},
			}
			err := checkK3sSkew(tt.installed, skewRecords(worker), versions)
			if tt.wantErr == nil {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			for _, want := range tt.wantErr {
				assert.Contains(t, err.Error(), want)
			}
		})
	}
}

func TestCheckK3sSkewChecksEveryRecordedAgent(t *testing.T) {
	records := skewRecords(
		workerRecord("worker-1", "203.0.113.11", NodeStateReady),
		workerRecord("worker-2", "203.0.113.12", NodeStateJoining),
		workerRecord("worker-3", "203.0.113.13", NodeStateRemoving),
	)
	versions := map[string]nodeVersion{
		"server-1": {version: "v1.35.6+k3s1", ready: true},
		"worker-1": {version: "v1.35.6+k3s1", ready: true},
		"worker-2": {version: "v1.32.0+k3s1"},
	}
	err := checkK3sSkew("v1.35.6+k3s1", records, versions)
	require.Error(t, err, "a joining agent's kubelet still talks to the API server")
	assert.Contains(t, err.Error(), "worker-2")

	delete(versions, "worker-2")
	assert.NoError(t, checkK3sSkew("v1.35.6+k3s1", records, versions), "a record without a Node runs no kubelet")
}

func TestCheckK3sSkewRefusesAnUnreadableAgentVersion(t *testing.T) {
	versions := map[string]nodeVersion{"worker-1": {version: "v1.36"}}
	err := checkK3sSkew(k3sVersion, skewRecords(workerRecord("worker-1", "203.0.113.11", NodeStateReady)), versions)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "worker-1")
}

func TestParseNodeVersions(t *testing.T) {
	out := "server-1\tv1.36.2+k3s1\tTrue\nworker-1\tv1.35.6+k3s1\tFalse\nworker-2\tv1.35.6+k3s1\t\n\n"
	got, err := parseNodeVersions(out)
	require.NoError(t, err)
	assert.Equal(t, map[string]nodeVersion{
		"server-1": {version: "v1.36.2+k3s1", ready: true},
		"worker-1": {version: "v1.35.6+k3s1"},
		"worker-2": {version: "v1.35.6+k3s1"},
	}, got)

	_, err = parseNodeVersions("garbage line\n")
	assert.Error(t, err)
}

func TestCheckClusterK3sSkewSkipsAClusterWithoutWorkers(t *testing.T) {
	r := &routedRunner{}
	require.NoError(t, checkClusterK3sSkew(r, skewRecords()))
	assert.Empty(t, r.commands, "a single-node cluster needs no version reads")
}

func TestCheckClusterK3sSkewReadsTheServerAndNodes(t *testing.T) {
	r := &routedRunner{routes: []route{
		{contains: "k3s --version", out: "k3s version v1.35.6+k3s1 (abc)\n"},
		{contains: "kubeletVersion", out: "server-1\tv1.35.6+k3s1\tTrue\nworker-1\tv1.32.1+k3s1\tTrue\n"},
	}}
	err := checkClusterK3sSkew(r, skewRecords(workerRecord("worker-1", "203.0.113.11", NodeStateReady)))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "worker-1")
}

// agentCluster simulates node versions and worker upgrades. Successful upgrades
// report the target version on the next read unless the worker is marked stuck.
type agentCluster struct {
	records  []NodeRecord
	versions map[string]nodeVersion
	failOn   string
	stuck    string
	afterRun func(host string)
	ran      map[string][]string
	reads    int
}

func (c *agentCluster) Run(command string) (string, error) {
	switch {
	case strings.Contains(command, "configmap kipper-nodes"):
		data := map[string]string{}
		for _, r := range c.records {
			rec, _ := json.Marshal(r)
			data[r.MachineID] = string(rec)
		}
		body, _ := json.Marshal(data)
		return string(body), nil
	case strings.Contains(command, "kubeletVersion"):
		c.reads++
		var b strings.Builder
		for name, v := range c.versions {
			ready := "False"
			if v.ready {
				ready = "True"
			}
			fmt.Fprintf(&b, "%s\t%s\t%s\n", name, v.version, ready)
		}
		return b.String(), nil
	}
	return "", fmt.Errorf("unexpected server command %q", command)
}

func (c *agentCluster) RunStdin(command string, _ io.Reader) (string, error) { return c.Run(command) }

type agentHost struct {
	cluster *agentCluster
	host    string
}

func (h agentHost) Run(command string) (string, error) {
	c := h.cluster
	c.ran[h.host] = append(c.ran[h.host], command)
	if h.host == c.failOn {
		return "curl: (6) Could not resolve host", errors.New("exit status 1")
	}
	for _, r := range c.records {
		if r.Host == h.host && r.Host != c.stuck {
			c.versions[r.Name] = nodeVersion{version: c.versions["server-1"].version, ready: true}
		}
	}
	if c.afterRun != nil {
		c.afterRun(h.host)
	}
	return "", nil
}

func (h agentHost) RunStdin(command string, _ io.Reader) (string, error) { return h.Run(command) }

func (c *agentCluster) onWorker(host string, use func(CommandRunner) error) error {
	return use(agentHost{cluster: c, host: host})
}

func newAgentCluster(workers map[string]string, records ...NodeRecord) *agentCluster {
	c := &agentCluster{records: skewRecords(records...), versions: map[string]nodeVersion{"server-1": {version: "v1.36.2+k3s1", ready: true}}, ran: map[string][]string{}}
	for name, v := range workers {
		c.versions[name] = nodeVersion{version: v, ready: true}
	}
	return c
}

func TestUpgradeAgentsBringsEachWorkerToTheServersVersion(t *testing.T) {
	c := newAgentCluster(
		map[string]string{"worker-1": "v1.35.6+k3s1", "worker-2": "v1.36.2+k3s1", "worker-3": "v1.35.6+k3s1"},
		workerRecord("worker-1", "203.0.113.11", NodeStateReady),
		workerRecord("worker-2", "203.0.113.12", NodeStateReady),
		workerRecord("worker-3", "203.0.113.13", NodeStateReady),
	)
	var out bytes.Buffer
	require.NoError(t, upgradeAgents(&out, c, c.onWorker, time.Second, time.Millisecond))

	for _, host := range []string{"203.0.113.11", "203.0.113.13"} {
		require.Len(t, c.ran[host], 1, "one command per agent")
		cmd := c.ran[host][0]
		assert.Contains(t, cmd, "INSTALL_K3S_VERSION='v1.36.2+k3s1'")
		assert.Contains(t, cmd, "INSTALL_K3S_EXEC=agent")
		assert.Contains(t, cmd, "INSTALL_K3S_FORCE_RESTART=true", "a retry after the binary was written must still restart the agent")
		assert.Contains(t, cmd, "/etc/systemd/system/k3s-agent.service.env", "the agent keeps its server URL and token")
		assert.NotContains(t, cmd, "drain")
	}
	assert.Empty(t, c.ran["203.0.113.12"], "a worker already at the version is skipped")
	assert.Contains(t, out.String(), "worker-2 already runs k3s v1.36.2+k3s1")
	assert.Contains(t, out.String(), "Upgrading k3s on worker-1 from v1.35.6+k3s1 to v1.36.2+k3s1")
}

func TestUpgradeAgentsStopsAtTheFirstFailure(t *testing.T) {
	c := newAgentCluster(
		map[string]string{"worker-1": "v1.35.6+k3s1", "worker-2": "v1.35.6+k3s1"},
		workerRecord("worker-1", "203.0.113.11", NodeStateReady),
		workerRecord("worker-2", "203.0.113.12", NodeStateReady),
	)
	c.failOn = "203.0.113.11"
	err := upgradeAgents(io.Discard, c, c.onWorker, time.Second, time.Millisecond)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "worker-1")
	assert.Contains(t, err.Error(), "kip install again")
	assert.Empty(t, c.ran["203.0.113.12"], "the next worker waits for a re-run")
}

func TestUpgradeAgentsWaitsForTheNewVersion(t *testing.T) {
	c := newAgentCluster(
		map[string]string{"worker-1": "v1.35.6+k3s1", "worker-2": "v1.35.6+k3s1"},
		workerRecord("worker-1", "203.0.113.11", NodeStateReady),
		workerRecord("worker-2", "203.0.113.12", NodeStateReady),
	)
	c.stuck = "203.0.113.11"
	err := upgradeAgents(io.Discard, c, c.onWorker, 20*time.Millisecond, time.Millisecond)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "worker-1")
	assert.Contains(t, err.Error(), "v1.36.2+k3s1")
	assert.Empty(t, c.ran["203.0.113.12"])
}

func TestUpgradeAgentsLeavesWorkersInTransitionAlone(t *testing.T) {
	c := newAgentCluster(
		map[string]string{"worker-1": "v1.35.6+k3s1", "worker-2": "v1.35.6+k3s1"},
		workerRecord("worker-1", "203.0.113.11", NodeStateJoining),
		workerRecord("worker-2", "203.0.113.12", NodeStateRemoving),
	)
	var out bytes.Buffer
	require.NoError(t, upgradeAgents(&out, c, c.onWorker, time.Second, time.Millisecond))
	assert.Empty(t, c.ran)
	assert.Contains(t, out.String(), "worker-1")
	assert.Contains(t, out.String(), "joining")
}

func TestUpgradeAgentsRefusesANewerAgent(t *testing.T) {
	c := newAgentCluster(
		map[string]string{"worker-1": "v1.36.3+k3s1"},
		workerRecord("worker-1", "203.0.113.11", NodeStateReady),
	)
	err := upgradeAgents(io.Discard, c, c.onWorker, time.Second, time.Millisecond)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "newer")
	assert.Empty(t, c.ran)
}

func TestUpgradeAgentsRefusesAMissingNode(t *testing.T) {
	c := newAgentCluster(nil, workerRecord("worker-1", "203.0.113.11", NodeStateReady))
	err := upgradeAgents(io.Discard, c, c.onWorker, time.Second, time.Millisecond)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "worker-1")
	assert.Empty(t, c.ran)
}

func TestUpgradeAgentsWithoutWorkersReadsNoVersions(t *testing.T) {
	c := newAgentCluster(nil)
	require.NoError(t, upgradeAgents(io.Discard, c, c.onWorker, time.Second, time.Millisecond))
	assert.Zero(t, c.reads)
}

func TestHasWorkers(t *testing.T) {
	assert.False(t, HasWorkers(skewRecords()))
	assert.True(t, HasWorkers(skewRecords(workerRecord("worker-1", "203.0.113.11", NodeStateJoining))))
}

func TestUpgradeAgentsWaitsForAWorkerAlreadyOnTheVersionToBeReady(t *testing.T) {
	c := newAgentCluster(
		map[string]string{"worker-1": "v1.36.2+k3s1", "worker-2": "v1.35.6+k3s1"},
		workerRecord("worker-1", "203.0.113.11", NodeStateReady),
		workerRecord("worker-2", "203.0.113.12", NodeStateReady),
	)
	c.versions["worker-1"] = nodeVersion{version: "v1.36.2+k3s1"}
	err := upgradeAgents(io.Discard, c, c.onWorker, 20*time.Millisecond, time.Millisecond)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "worker-1")
	assert.Empty(t, c.ran, "no worker restarts while another is not Ready")
}

func TestUpgradeAgentsReportsTheHostsLastLine(t *testing.T) {
	c := newAgentCluster(map[string]string{"worker-1": "v1.35.6+k3s1"}, workerRecord("worker-1", "203.0.113.11", NodeStateReady))
	c.failOn = "203.0.113.11"
	err := upgradeAgents(io.Discard, c, c.onWorker, time.Second, time.Millisecond)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Could not resolve host")
	assert.NotContains(t, err.Error(), "INSTALL_K3S_VERSION", "the script itself stays out of the error")
}

// runAgentGuard runs the upgrade command in a local shell against a fake
// machine ID, a fake k3s binary and no agent environment file, so it stops
// at the environment check when the guards pass.
func runAgentGuard(t *testing.T, machineID, installed string) string {
	t.Helper()
	dir := t.TempDir()
	idFile := dir + "/machine-id"
	require.NoError(t, os.WriteFile(idFile, []byte(machineID+"\n"), 0o600))
	if installed != "" {
		k3s := fmt.Sprintf("#!/bin/sh\necho 'k3s version %s (abc123)'\necho 'go version go1.26'\n", installed)
		//nolint:gosec // G306: the stub stands in for k3s on PATH, so it has to be executable.
		require.NoError(t, os.WriteFile(dir+"/k3s", []byte(k3s), 0o700))
	}
	oldID, oldEnv := leaseMachineIDFile, k3sAgentEnvFile
	leaseMachineIDFile, k3sAgentEnvFile = idFile, dir+"/absent.env"
	t.Cleanup(func() { leaseMachineIDFile, k3sAgentEnvFile = oldID, oldEnv })
	cmd := exec.Command("sh", "-c", agentUpgradeCommand(workerMachine, "v1.35.6+k3s1", "v1.36.2+k3s1"))
	cmd.Env = append(os.Environ(), "PATH="+dir+":/usr/bin:/bin")
	out, err := cmd.CombinedOutput()
	require.Error(t, err, "every case here stops before the download")
	return string(out)
}

func TestAgentUpgradeCommandGuards(t *testing.T) {
	out := runAgentGuard(t, serverMachine, "v1.35.6+k3s1")
	assert.Contains(t, out, "machine "+serverMachine, "another machine at the recorded address is left alone")
	assert.NotContains(t, out, "absent.env")

	out = runAgentGuard(t, workerMachine, "v1.37.0+k3s1")
	assert.Contains(t, out, "v1.37.0+k3s1", "a binary newer than its node reports is never replaced")
	assert.NotContains(t, out, "absent.env")

	out = runAgentGuard(t, workerMachine, "")
	assert.Contains(t, out, "none", "a host without k3s is not an agent to upgrade")
	assert.NotContains(t, out, "absent.env")

	for _, installed := range []string{"v1.35.6+k3s1", "v1.36.2+k3s1"} {
		out = runAgentGuard(t, workerMachine, installed)
		assert.Contains(t, out, "absent.env is missing", "installed %s passes the guards", installed)
	}
}

func TestUpgradeAgentsRereadsReadinessForEachWorker(t *testing.T) {
	c := newAgentCluster(
		map[string]string{"worker-1": "v1.35.6+k3s1", "worker-2": "v1.36.2+k3s1", "worker-3": "v1.35.6+k3s1"},
		workerRecord("worker-1", "203.0.113.11", NodeStateReady),
		workerRecord("worker-2", "203.0.113.12", NodeStateReady),
		workerRecord("worker-3", "203.0.113.13", NodeStateReady),
	)
	c.afterRun = func(host string) {
		if host == "203.0.113.11" {
			c.versions["worker-2"] = nodeVersion{version: "v1.36.2+k3s1"}
		}
	}
	err := upgradeAgents(io.Discard, c, c.onWorker, 20*time.Millisecond, time.Millisecond)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "worker-2")
	assert.Empty(t, c.ran["203.0.113.13"], "worker-3 waits while worker-2 is not Ready")
}

func TestHostFailure(t *testing.T) {
	script := "a very long script"
	wrapped := fmt.Errorf("running %q on host: %w", script, errors.New("exit status 1"))
	assert.EqualError(t, hostFailure("noise\nlast words\n", wrapped), "last words")
	assert.EqualError(t, hostFailure("", wrapped), "exit status 1")
	assert.ErrorIs(t, hostFailure("", ErrNodeLeaseLost), ErrNodeLeaseLost)
}

func TestUpgradeAgentsKeepsALostLease(t *testing.T) {
	c := newAgentCluster(map[string]string{"worker-1": "v1.35.6+k3s1"}, workerRecord("worker-1", "203.0.113.11", NodeStateReady))
	onWorker := func(string, func(CommandRunner) error) error { return ErrNodeLeaseLost }
	err := upgradeAgents(io.Discard, c, onWorker, time.Second, time.Millisecond)
	assert.ErrorIs(t, err, ErrNodeLeaseLost)
}
