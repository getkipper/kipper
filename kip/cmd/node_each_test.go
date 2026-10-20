package cmd

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/kip/internal/installer"
)

const (
	eachServerID = "0123456789abcdef0123456789abcdef"
	eachWorkerA  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	eachWorkerB  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func eachRecords() []installer.NodeRecord {
	return []installer.NodeRecord{
		{Name: "server", Host: "203.0.113.10", MachineID: eachServerID, Role: installer.NodeRoleServer, State: installer.NodeStateReady},
		{Name: "worker-a", Host: "203.0.113.11", MachineID: eachWorkerA, Role: installer.NodeRoleWorker, State: installer.NodeStateReady},
		{Name: "worker-b", Host: "203.0.113.12", MachineID: eachWorkerB, Role: installer.NodeRoleWorker, State: installer.NodeStateReady},
	}
}

// eachHost answers the machine id probe with id.
type eachHost struct {
	firewallHost
	id string
}

func (h *eachHost) Run(command string) (string, error) {
	if command == "cat /etc/machine-id" {
		return h.id + "\n", nil
	}
	return h.firewallHost.Run(command)
}

// eachDialer records which hosts were dialed and closed, in order.
type eachDialer struct {
	ids     map[string]string
	refuse  map[string]bool
	events  []string
	openNow int
	maxOpen int
}

func (d *eachDialer) dial(rec installer.NodeRecord) (installer.FirewallRunner, func(), error) {
	d.events = append(d.events, "dial "+rec.Host)
	if d.refuse[rec.Host] {
		return nil, nil, errors.New("connection refused")
	}
	d.openNow++
	d.maxOpen = max(d.maxOpen, d.openNow)
	return &eachHost{id: d.ids[rec.Host]}, func() {
		d.openNow--
		d.events = append(d.events, "close "+rec.Host)
	}, nil
}

func newEachDialer() *eachDialer {
	return &eachDialer{ids: map[string]string{"203.0.113.11": eachWorkerA, "203.0.113.12": eachWorkerB}, refuse: map[string]bool{}}
}

func TestOnEachWorkerVisitsWorkersOneAtATimeInRecordOrder(t *testing.T) {
	d := newEachDialer()
	var visited []string

	err := onEachWorker(eachRecords(), d.dial, func(rec installer.NodeRecord, _ installer.FirewallRunner) error {
		visited = append(visited, rec.Name)
		return nil
	})

	require.NoError(t, err)
	assert.Equal(t, []string{"worker-a", "worker-b"}, visited, "the server is not a worker")
	assert.Equal(t, []string{"dial 203.0.113.11", "close 203.0.113.11", "dial 203.0.113.12", "close 203.0.113.12"}, d.events)
	assert.Equal(t, 1, d.maxOpen, "one connection per node, closed before the next node")
}

func TestOnEachWorkerStopsAtTheFirstFailureAndNamesTheNode(t *testing.T) {
	d := newEachDialer()
	var visited []string

	err := onEachWorker(eachRecords(), d.dial, func(rec installer.NodeRecord, _ installer.FirewallRunner) error {
		visited = append(visited, rec.Name)
		return errors.New("ufw failed")
	})

	require.Error(t, err)
	assert.Equal(t, []string{"worker-a"}, visited, "worker-b is left for the rerun")
	assert.Contains(t, err.Error(), "worker-a (203.0.113.11)")
	assert.Contains(t, err.Error(), "ufw failed")
	assert.Contains(t, err.Error(), "run the command again")
}

func TestOnEachWorkerStopsAtAnUnreachableWorker(t *testing.T) {
	d := newEachDialer()
	d.refuse["203.0.113.11"] = true
	ran := false

	err := onEachWorker(eachRecords(), d.dial, func(installer.NodeRecord, installer.FirewallRunner) error {
		ran = true
		return nil
	})

	require.Error(t, err)
	assert.False(t, ran, "an unreachable worker stops the run instead of being skipped")
	assert.Equal(t, []string{"dial 203.0.113.11"}, d.events, "no later worker is tried")
	assert.Contains(t, err.Error(), "worker-a (203.0.113.11)")
	assert.Contains(t, err.Error(), "connection refused")
}

func TestOnEachWorkerRefusesAHostThatAnswersAsAnotherMachine(t *testing.T) {
	d := newEachDialer()
	d.ids["203.0.113.11"] = "cccccccccccccccccccccccccccccccc"
	ran := false

	err := onEachWorker(eachRecords(), d.dial, func(installer.NodeRecord, installer.FirewallRunner) error {
		ran = true
		return nil
	})

	require.Error(t, err)
	assert.False(t, ran, "a host that took over a worker's address is left alone")
	assert.Contains(t, err.Error(), "no longer answers as node worker-a")
}

func TestNodesSettledRefusesAJoinOrRemovalInProgress(t *testing.T) {
	records := eachRecords()
	require.NoError(t, nodesSettled(records))

	joining := eachRecords()
	joining[1].State = installer.NodeStateJoining
	err := nodesSettled(joining)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "worker-a has not finished joining")
	assert.Contains(t, err.Error(), "kip node add 203.0.113.11")

	removing := eachRecords()
	removing[2].State = installer.NodeStateWiping
	err = nodesSettled(removing)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "worker-b is being removed")
	assert.Contains(t, err.Error(), "kip node remove worker-b")
}

func TestForEachNodeRunsTheServerFirstThenEachWorker(t *testing.T) {
	d := newEachDialer()
	server := &eachHost{id: eachServerID}
	var visited []string

	err := forEachNode(eachRecords(), server, d.dial, func(rec installer.NodeRecord, client installer.FirewallRunner) error {
		visited = append(visited, rec.Name)
		if rec.Role == installer.NodeRoleServer {
			assert.Same(t, server, client, "the server step runs on the server connection")
		}
		return nil
	})

	require.NoError(t, err)
	assert.Equal(t, []string{"server", "worker-a", "worker-b"}, visited)
}

func TestForEachNodeLeavesTheWorkersAloneWhenTheServerFails(t *testing.T) {
	d := newEachDialer()

	err := forEachNode(eachRecords(), &eachHost{id: eachServerID}, d.dial, func(installer.NodeRecord, installer.FirewallRunner) error {
		return errors.New("audit failed")
	})

	require.Error(t, err)
	assert.Empty(t, d.events, "no worker is reached after the server failed")
	assert.Contains(t, err.Error(), "server (203.0.113.10)")
	assert.Contains(t, err.Error(), "audit failed")
}

func TestHardenEachUsesEachNodesOwnRoleAndPeers(t *testing.T) {
	records := eachRecords()
	records[0].PeerAddress, records[1].PeerAddress, records[2].PeerAddress = "10.0.0.1", "10.0.0.2", "10.0.0.3"
	meshes := map[string]installer.FirewallMesh{}

	step := hardenEach(records, func(_ installer.FirewallRunner, host string, mesh installer.FirewallMesh) error {
		meshes[host] = mesh
		return nil
	})
	for _, rec := range records {
		require.NoError(t, step(rec, &eachHost{id: rec.MachineID}))
	}

	assert.Equal(t, installer.RoleServer, meshes["203.0.113.10"].Role)
	assert.Equal(t, installer.RoleWorker, meshes["203.0.113.11"].Role, "a worker gets the worker firewall: no public 80, 443 or 6443")
	assert.Equal(t, []installer.FirewallPeer{{Address: "10.0.0.1", Server: true}, {Address: "10.0.0.3"}}, meshes["203.0.113.11"].Peers,
		"a worker keeps its peer rules for the server and the other workers")
	assert.Equal(t, []installer.FirewallPeer{{Address: "10.0.0.2"}, {Address: "10.0.0.3"}}, meshes["203.0.113.10"].Peers)
}

func TestNodeMaintenanceCommandsDoNotPrintUsageOnRuntimeErrors(t *testing.T) {
	assert.True(t, nodeRepairHostCmd.SilenceUsage, "an unreachable node or a held lease is not a usage mistake")
	assert.True(t, clusterDNSRepairCmd.SilenceUsage, "an unreachable node or a held lease is not a usage mistake")
}
