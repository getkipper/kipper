package cmd

import (
	"bytes"
	"errors"
	"net/netip"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/installer"
)

func listedRecord() installer.AccessRecord {
	return installer.AccessRecord{Version: 1, Generation: 4, Sources: []installer.AccessSource{{Prefix: "198.51.100.0/24"}, {Prefix: "203.0.113.0/24"}}}
}

func TestNodeAccessIsOffWithoutARecord(t *testing.T) {
	f := newAccessFixture(t, map[string]string{"4": "198.51.100.7"})

	a, notes, err := prepareNodeAccess(f.c, f.c.nodes, nil, "node add", true)

	require.NoError(t, err)
	assert.Empty(t, notes)
	require.NoError(t, a.load(f.c.nodes[0], f.c.records, ""))
	assert.Empty(t, f.applied, "a cluster without a list sees no change")
	assert.Empty(t, f.server.writes)
}

func TestNodeAccessRefusesALeftoverTableWithoutARecord(t *testing.T) {
	f := newAccessFixture(t, map[string]string{"4": "198.51.100.7"})
	f.committed(f.server, 3)

	_, _, err := prepareNodeAccess(f.c, f.c.nodes, nil, "node add", true)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "kip cluster access reset")
}

func TestNodeAddRefusesANodeWithoutATable(t *testing.T) {
	f := newAccessFixture(t, map[string]string{"4": "198.51.100.7"})
	f.server.store(t, listedRecord())
	f.committed(f.server, 4)
	f.addWorker()

	_, _, err := prepareNodeAccess(f.c, f.c.nodes, nil, "node add", true)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "worker-1 has no access table")
	assert.Contains(t, err.Error(), "kip cluster access apply")
}

// Adding a node adds the joining host's addresses to the server's live table,
// so the committed table must be loaded first.
func TestNodeAddRefusesATableMissingFromTheKernel(t *testing.T) {
	f := newAccessFixture(t, map[string]string{"4": "198.51.100.7"})
	f.server.store(t, listedRecord())
	f.committed(f.server, 4)
	f.c.state = func(installer.CommandRunner) (installer.AccessHostState, error) {
		return installer.AccessHostState{Table: f.headers[f.server], Operators: f.headers[f.server], UnitEnabled: "enabled", UnitActive: "active"}, nil
	}

	_, _, err := prepareNodeAccess(f.c, f.c.nodes, nil, "node add 192.0.2.12", true)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "server: access table missing from the kernel")
	assert.Contains(t, err.Error(), "kip cluster access apply")

	_, _, err = prepareNodeAccess(f.c, f.c.nodes, nil, "node remove worker-1", false)
	require.NoError(t, err, "removal loads the table again, so it goes ahead")
}

func TestNodeAddPointsAnUnfinishedJoinAtItsOwnRetry(t *testing.T) {
	f := newAccessFixture(t, map[string]string{"4": "198.51.100.7"})
	f.server.store(t, listedRecord())
	f.committed(f.server, 4)
	f.addWorker()
	f.c.nodes[1].rec.State = installer.NodeStateJoining

	_, _, err := prepareNodeAccess(f.c, f.c.nodes, nil, "node add", true)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "Run 'kip node add 192.0.2.11' again")
}

func TestNodeRemoveSkipsANodeWithoutATableWithANote(t *testing.T) {
	f := newAccessFixture(t, map[string]string{"4": "198.51.100.7"})
	f.server.store(t, listedRecord())
	f.committed(f.server, 4)
	f.addWorker()

	a, notes, err := prepareNodeAccess(f.c, f.c.nodes, nil, "node remove worker-2", false)

	require.NoError(t, err)
	assert.Equal(t, []string{"worker-1 has no access table. Run 'kip cluster access apply'"}, notes)
	require.NoError(t, a.load(f.c.nodes[1], f.c.records, ""))
	assert.Empty(t, f.applied)
	require.NoError(t, a.load(f.c.nodes[0], f.c.records, ""))
	require.Len(t, f.applied, 1)
	assert.Equal(t, 5, f.applied[0].Intent.Generation)
}

func TestNodeAccessRefusesAStaleRecord(t *testing.T) {
	f := newAccessFixture(t, map[string]string{"4": "198.51.100.7"})
	f.server.store(t, listedRecord())
	f.committed(f.server, 9)

	_, _, err := prepareNodeAccess(f.c, f.c.nodes, nil, "node add", true)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--accept-older-record")
}

func TestNodeAccessRefusesAWorkstationOutsideTheList(t *testing.T) {
	f := newAccessFixture(t, map[string]string{"4": "192.0.2.99"})
	f.server.store(t, listedRecord())
	f.committed(f.server, 4)

	_, _, err := prepareNodeAccess(f.c, f.c.nodes, nil, "node add", true)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "allow --here")
	assert.Empty(t, f.server.writes)
}

func TestNodeAccessRecordsTheGenerationOnTheFirstLoad(t *testing.T) {
	f := newAccessFixture(t, map[string]string{"4": "198.51.100.7"})
	f.server.store(t, listedRecord())
	f.committed(f.server, 4)
	worker := f.addWorker()
	f.committed(worker, 4)

	a, _, err := prepareNodeAccess(f.c, f.c.nodes, nil, "node add 192.0.2.12", true)
	require.NoError(t, err)
	assert.Empty(t, f.server.writes, "nothing is recorded before a node changes")

	require.NoError(t, a.load(f.c.nodes[0], f.c.records, ""))
	require.NoError(t, a.load(f.c.nodes[1], f.c.records, ""))

	assert.Equal(t, 5, f.server.writes[0].Generation, "the generation is recorded before the first load")
	assert.Empty(t, f.server.writes[0].History[0].Nodes)
	rec := f.server.stored(t)
	assert.Equal(t, 5, rec.Generation)
	assert.Equal(t, "node add 192.0.2.12", rec.History[0].Action)
	assert.Equal(t, map[string]string{"server": installer.AccessCommitted, "worker-1": installer.AccessCommitted}, rec.History[0].Nodes)
	require.Len(t, f.applied, 2)
	assert.Equal(t, 5, f.applied[1].Intent.Generation)
	assert.Equal(t, []string{"198.51.100.0/24", "203.0.113.0/24"}, prefixStrings(f.applied[1].Intent.Admin))
}

func TestNodeAccessChecksAJoinerBeforeLoadingIt(t *testing.T) {
	f := newAccessFixture(t, map[string]string{"4": "198.51.100.7"})
	f.server.store(t, listedRecord())
	f.committed(f.server, 4)
	a, _, err := prepareNodeAccess(f.c, f.c.nodes, nil, "node add", true)
	require.NoError(t, err)
	joiner := &accessHost{observed: map[string]string{"4": "192.0.2.99"}}
	rec := installer.NodeRecord{Name: "worker-2", Host: "192.0.2.12", PeerAddress: "10.0.0.12", MachineID: accessWorkerID, Role: installer.NodeRoleWorker}

	err = a.load(accessNode{rec: rec, node: joiner, base: joiner}, append(f.c.records, rec), "")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "allow --here")
	assert.Empty(t, f.applied)
}

func TestNodeAccessExcludesTheRemovedNode(t *testing.T) {
	f := newAccessFixture(t, map[string]string{"4": "198.51.100.7"})
	f.server.store(t, listedRecord())
	f.committed(f.server, 4)
	a, _, err := prepareNodeAccess(f.c, f.c.nodes, nil, "node remove", false)
	require.NoError(t, err)
	gone := installer.NodeRecord{Name: "worker-9", Host: "192.0.2.19", PeerAddress: "10.0.0.19", MachineID: accessWorkerID, Role: installer.NodeRoleWorker}

	require.NoError(t, a.load(f.c.nodes[0], append(f.c.records, gone), gone.MachineID))

	assert.NotContains(t, prefixStrings(f.applied[0].Intent.Cluster4), "10.0.0.19/32")
}

func TestNodeAccessAddsTheAllowHintWhenTheHostSawAnotherAddress(t *testing.T) {
	f := newAccessFixture(t, map[string]string{"4": "198.51.100.7"})
	f.server.store(t, listedRecord())
	f.committed(f.server, 4)
	f.result["server"] = errors.New("server: the host saw this workstation as 192.0.2.99 over IPv4, which the new access table does not admit. kip restored the previous access table")
	a, _, err := prepareNodeAccess(f.c, f.c.nodes, nil, "node add", true)
	require.NoError(t, err)

	err = a.load(f.c.nodes[0], f.c.records, "")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "allow --here")
}

func TestReportAccessGuardSaysOpenWithoutAListOrTable(t *testing.T) {
	var out bytes.Buffer
	reportAccessGuard(&out, &accessHost{}, &stateHost{})
	assert.Contains(t, out.String(), "open to every address")
}

func TestReportAccessGuardNamesAProblem(t *testing.T) {
	server := &accessHost{}
	server.store(t, listedRecord())
	var out bytes.Buffer

	reportAccessGuard(&out, server, &stateHost{})

	assert.Contains(t, out.String(), "access table missing. Run kip cluster access apply")
}

// Each node relies on its own host firewall when its access table is missing,
// so kip status reports every node's access guard.
func TestStatusReportsTheAccessGuardOfEveryNode(t *testing.T) {
	server := &accessHost{}
	server.store(t, listedRecord())
	targets := []hostTarget{
		{name: "server-1", prefix: "server-1: ", runner: server},
		{name: "worker-1", prefix: "worker-1: ", runner: &stateHost{}},
		{name: "worker-2", prefix: "worker-2: ", unreached: "could not reach 192.0.2.12 over SSH"},
	}
	var out bytes.Buffer

	reportAccessGuards(&out, server, targets)

	got := out.String()
	assert.Equal(t, 1, strings.Count(got, "Operator access:"))
	assert.Contains(t, got, "worker-1: access table missing. Run kip cluster access apply")
	assert.Contains(t, got, "worker-2: not checked (could not reach 192.0.2.12 over SSH)")
}

// heldHost simulates a healthy, loaded access table at gen that admits operators.
type heldHost struct {
	accessHost
	gen       int
	operators string
	ops       string
}

func (h *heldHost) Run(command string) (string, error) {
	switch {
	case strings.Contains(command, "kipper_ops"):
		return h.ops, nil
	case strings.Contains(command, "machine-id"):
		return accessServerID + "\n", nil
	}
	header := "# kipper-access generation " + strconv.Itoa(h.gen) + "\n"
	return header + "table inet kipper_access {}\n----kip-access----\n" +
		header + "add element inet kipper_access admin4 { " + h.operators + " }\n----kip-access----\n" +
		"enabled enabled\nactive active\nlive\nnftables  \n", nil
}

func TestReportAccessGuardConfirmsAHostThatMatchesTheRecord(t *testing.T) {
	server := &accessHost{}
	server.store(t, listedRecord())
	var out bytes.Buffer

	reportAccessGuard(&out, server, &heldHost{gen: 4, operators: "198.51.100.0/24, 203.0.113.0/24"})

	assert.Contains(t, out.String(), "✔  restricted to 2 sources (generation 4)")
}

// TestReportAccessGuardWarnsAboutAnOlderGeneration covers a rolled-back revoke.
// The record is written before hosts are updated, so restoring the previous table
// still admits the revoked prefix.
func TestReportAccessGuardWarnsAboutAnOlderGeneration(t *testing.T) {
	server := &accessHost{}
	server.store(t, listedRecord())
	var out bytes.Buffer

	reportAccessGuard(&out, server, &heldHost{gen: 3, operators: "198.51.100.0/24, 203.0.113.0/24, 192.0.2.0/24"})

	assert.Contains(t, out.String(), "holds generation 3 while the record has generation 4. Run 'kip cluster access show'")
	assert.NotContains(t, out.String(), "✔")
}

func TestReportAccessGuardWarnsAboutADifferentOperatorList(t *testing.T) {
	server := &accessHost{}
	server.store(t, listedRecord())
	var out bytes.Buffer

	reportAccessGuard(&out, server, &heldHost{gen: 4, operators: "198.51.100.0/24, 192.0.2.0/24"})

	assert.Contains(t, out.String(), "its committed operator list differs from the record. Run 'kip cluster access show'")
	assert.NotContains(t, out.String(), "✔")
}

// stateHost answers the access state command with an empty host.
type stateHost struct{ accessHost }

func (h *stateHost) Run(command string) (string, error) {
	return "----kip-access----\n----kip-access----\nenabled \nactive \nnftables  \n", nil
}

func TestUpdateRemainingPeersReadsTheAccessListBeforeChangingAFirewall(t *testing.T) {
	f := newAccessFixture(t, map[string]string{"4": "198.51.100.7"})
	f.addWorker()
	worker := &machineHost{id: accessWorkerID}
	dial := func(string) (installer.FirewallRunner, installer.FirewallRunner, func(), error) {
		return worker, worker, func() {}, nil
	}
	var seen []string
	access := func(nodes []accessNode) (*nodeAccess, []string, error) {
		for _, n := range nodes {
			seen = append(seen, n.rec.Name)
		}
		return nil, nil, errors.New("stale record")
	}
	removed := installer.NodeRecord{Name: "worker-9", MachineID: "99999999999999999999999999999999"}

	_, err := updateRemainingPeers(&bytes.Buffer{}, accessNode{node: f.server, base: f.server}, f.c.records, removed, dial, access)

	require.EqualError(t, err, "stale record")
	assert.Equal(t, []string{"server", "worker-1"}, seen)
	for _, c := range worker.commands {
		assert.NotContains(t, c, "ufw", "no firewall changed before the access list was read")
	}
}

// machineHost answers only the machine ID and records every command.
type machineHost struct {
	accessHost
	id       string
	commands []string
}

func (h *machineHost) Run(command string) (string, error) {
	h.commands = append(h.commands, command)
	return h.id + "\n", nil
}

func TestNodeAddRefusesWhenTheRecordedJoinerHoldsANewerList(t *testing.T) {
	f := newAccessFixture(t, map[string]string{"4": "198.51.100.7"})
	f.server.store(t, listedRecord())
	f.committed(f.server, 4)
	joiner := []installer.AccessNodeHeader{{Name: "worker-2", Generation: 5, Present: true, Sources: 1}}

	_, _, err := prepareNodeAccess(f.c, f.c.nodes, joiner, "node add", true)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "worker-2 (generation 5)")
}

func TestNodeRemoveRefusesATargetThatHoldsANewerList(t *testing.T) {
	server := &accessHost{}
	server.store(t, listedRecord())
	target := &stateHost{}

	require.NoError(t, checkRemovalTargetAccess(server, target, "worker-1"))

	newer := &newerHost{}
	err := checkRemovalTargetAccess(server, newer, "worker-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "worker-1 (generation 9)")
	assert.NoError(t, checkRemovalTargetAccess(&accessHost{}, target, "worker-1"), "a cluster without a list removes as before")
	err = checkRemovalTargetAccess(&accessHost{}, newer, "worker-1")
	require.Error(t, err, "a table without a record is evidence of a lost record")
	assert.Contains(t, err.Error(), "kip cluster access reset")
}

// newerHost reports committed access files at generation 9.
type newerHost struct{ accessHost }

func (h *newerHost) Run(command string) (string, error) {
	g := "# kipper-access generation 9\nadd element inet kipper_access admin4 { 198.51.100.0/24 }\n"
	return g + "----kip-access----\n" + g + "----kip-access----\nlive\n", nil
}

// testWorkerKey is a second node's WireGuard public key.
var testWorkerKey = wordKey("workerworkerworkerworkerworker12")

func TestNodeAccessGivesAJoinerItsOperatorInterface(t *testing.T) {
	f := newDeviceFixture(t, config.OpsActive, map[string]string{"4": "198.51.100.7"})
	f.deviceRecord(t, "198.51.100.7/32")
	f.c.opsKey = func(installer.CommandRunner) (string, error) { return testWorkerKey, nil }
	a, _, err := prepareNodeAccess(f.c, f.c.nodes, nil, "node add 192.0.2.11", true)
	require.NoError(t, err)
	f.addWorker()
	records := f.c.records
	f.c.records = records[:1]

	require.NoError(t, a.load(f.c.nodes[0], records, ""))
	require.NoError(t, a.load(f.c.nodes[1], records, ""))

	on, ok := f.server.stored(t).Ops.Node(accessWorkerID)
	require.True(t, ok, "the joiner's operator address is recorded")
	assert.Equal(t, "10.99.0.2", on.Address)
	assert.Equal(t, testWorkerKey, on.PublicKey)
	require.Len(t, f.opsCalls, 2)
	assert.Equal(t, netip.MustParseAddr("10.99.0.2"), f.opsCalls[1].Address)
	cfg, err := config.Load()
	require.NoError(t, err)
	var names []string
	for _, n := range cfg.GetCluster("prod").Ops.Nodes {
		names = append(names, n.Name)
	}
	assert.Equal(t, []string{"server", "worker-1"}, names, "the tunnel learns the new node")
}

func TestNodeAccessRefusesAJoinerWithARouteIntoTheOperatorRange(t *testing.T) {
	f := newDeviceFixture(t, config.OpsActive, map[string]string{"4": "198.51.100.7"})
	f.deviceRecord(t, "198.51.100.7/32")
	a, _, err := prepareNodeAccess(f.c, f.c.nodes, nil, "node add 192.0.2.11", true)
	require.NoError(t, err)
	f.addWorker()
	f.routes = []netip.Prefix{netip.MustParsePrefix("10.99.0.128/25")}

	require.NoError(t, a.load(f.c.nodes[0], f.c.records, ""))
	err = a.load(f.c.nodes[1], f.c.records, "")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "10.99.0.128/25")
	_, ok := f.server.stored(t).Ops.Node(accessWorkerID)
	assert.False(t, ok)
	assert.Len(t, f.opsCalls, 1, "the joiner's interface is never loaded")
}

func TestNodeAccessDropsTheRemovedNodesOperatorAddress(t *testing.T) {
	f := newDeviceFixture(t, config.OpsActive, map[string]string{"4": "198.51.100.7"})
	rec := f.deviceRecord(t, "198.51.100.7/32")
	rec.Ops.Nodes = append(rec.Ops.Nodes, installer.AccessOpsNode{MachineID: accessWorkerID, Address: "10.99.0.2", PublicKey: testWorkerKey})
	rec.Ops.NextNode = 3
	f.server.store(t, rec)
	a, _, err := prepareNodeAccess(f.c, f.c.nodes, nil, "node remove worker-9", false)
	require.NoError(t, err)

	require.NoError(t, a.load(f.c.nodes[0], f.c.records, accessWorkerID))

	_, ok := f.server.stored(t).Ops.Node(accessWorkerID)
	assert.False(t, ok)
}

func TestReportAccessGuardCountsRegisteredDevices(t *testing.T) {
	server := &accessHost{}
	server.store(t, installer.AccessRecord{
		Version: 2, Generation: 4,
		Sources: []installer.AccessSource{{Prefix: "198.51.100.0/24"}},
		Ops: &installer.AccessOps{Range: installer.DefaultOpsRange, Port: 443, NextNode: 2, NextDevice: 130,
			Nodes: []installer.AccessOpsNode{{MachineID: accessServerID, Address: "10.99.0.1", PublicKey: testNodeKey}}},
		Devices: []installer.AccessDevice{{Name: "laptop", PublicKey: testWorkerKey, Address: "10.99.0.129", State: installer.DeviceActive}},
	})
	var out bytes.Buffer

	host := &heldHost{gen: 4, operators: "198.51.100.0/24",
		ops: "dir=1\nenabled=1\ntable=1\ntablesum=abc\nloadedsum=abc\n" + strings.Repeat("rule=input\n", 6) + "hook=input input\n" + strings.Repeat("rule=forward\n", 2) + "hook=forward forward\nrule=raw\nhook=raw prerouting\n" +
			"up=1\nmtu=1380\nroute=10.99.0.128/25\nkey=" + testNodeKey + "\nport=443\naddress=10.99.0.1/32\npeer=" + testWorkerKey + "\t10.99.0.129/32\n"}

	reportAccessGuard(&out, server, host)

	assert.Contains(t, out.String(), "✔  restricted to one source and one device (generation 4)")
	assert.NotContains(t, out.String(), "the list has one entry", "a registered device is a second way in")
	assert.NotContains(t, out.String(), "⚠")

	out.Reset()
	host.ops = "dir=1\n"
	reportAccessGuard(&out, server, host)
	assert.Contains(t, out.String(), "⚠  the operator interface kipops is down. Run 'kip cluster access show'")
}

var testJoinerKey = wordKey("joinjoinjoinjoinjoinjoinjoinjoin")

// joinerFixture is a device-only cluster with this workstation's device
// active and a joining worker whose tunnel answers only once its operator
// interface is loaded. events records the order of loads and probes.
type joinerFixture struct {
	*deviceFixture
	joiner     accessNode
	records    []installer.NodeRecord
	events     []string
	joinerUp   bool
	joinerOps  error
	joinerDown bool
}

func newJoinerFixture(t *testing.T) *joinerFixture {
	t.Helper()
	f := &joinerFixture{deviceFixture: newDeviceFixture(t, config.OpsActive, map[string]string{"4": "198.51.100.7"})}
	f.deviceRecord(t)
	host := &accessHost{observed: map[string]string{"4": "198.51.100.7"}}
	rec := installer.NodeRecord{Name: "worker-2", Host: "192.0.2.12", PeerAddress: "10.0.0.12", MachineID: accessWorkerID, Role: installer.NodeRoleWorker, State: installer.NodeStateJoining}
	f.joiner = accessNode{rec: rec, node: host, base: host}
	f.records = append(append([]installer.NodeRecord(nil), f.c.records...), rec)
	f.c.opsKey = func(r installer.CommandRunner) (string, error) {
		if r == host {
			return testJoinerKey, nil
		}
		return testNodeKey, nil
	}
	f.c.applyOps = func(r installer.CommandRunner, in installer.OpsNodeIntent, _ string) error {
		if r == host {
			f.events = append(f.events, "ops worker-2")
			if f.joinerOps != nil {
				return f.joinerOps
			}
			f.joinerUp = true
		}
		f.opsCalls = append(f.opsCalls, in)
		return nil
	}
	f.c.tunnel = func(n accessNode) (installer.AddressObserver, error) {
		return observeFunc(func(string) (string, error) {
			if n.rec.MachineID == accessWorkerID {
				f.events = append(f.events, "tunnel worker-2")
				if !f.joinerUp || f.joinerDown {
					return "", errors.New("dial tcp 10.99.0.2:22: i/o timeout")
				}
			}
			return "10.99.0.129", nil
		}), nil
	}
	return f
}

func TestNodeAddReachesAJoinerOnADeviceOnlyClusterThroughTheTunnel(t *testing.T) {
	f := newJoinerFixture(t)
	a, _, err := prepareNodeAccess(f.c, f.c.nodes, nil, "node add 192.0.2.12", true)
	require.NoError(t, err)

	require.NoError(t, a.prepareJoiner(f.joiner, f.records))

	assert.Equal(t, []string{"ops worker-2", "tunnel worker-2"}, f.events, "the interface comes up before the baseline")
	assert.Empty(t, f.applied, "the joiner's access table waits for its join")
	stored := f.server.stored(t)
	on, ok := stored.Ops.Node(accessWorkerID)
	require.True(t, ok, "the record gives the joiner an operator address")
	assert.Equal(t, testJoinerKey, on.PublicKey)
	assert.Equal(t, "node add 192.0.2.12", stored.History[len(stored.History)-1].Action)
	cfg, err := config.Load()
	require.NoError(t, err)
	var names []string
	for _, n := range cfg.Clusters[0].Ops.Nodes {
		names = append(names, n.Name)
	}
	assert.Contains(t, names, "worker-2", "this workstation's node list has the joiner")

	assert.Equal(t, "operator@198.51.100.7", stored.History[len(stored.History)-1].By)

	require.NoError(t, a.load(f.c.nodes[0], f.records, ""))
	require.NoError(t, a.load(f.joiner, f.records, ""))
	assert.Equal(t, []string{"ops worker-2", "tunnel worker-2", "ops worker-2"}, f.events, "the load reuses the baseline")
	require.Len(t, f.applied, 2)
	assert.Equal(t, "worker-2", f.applied[1].Label)
	assert.NotNil(t, f.applied[1].Tunnel, "the joiner's table is checked through the tunnel")
	assert.Empty(t, f.applied[1].Intent.Admin)
	assert.Equal(t, map[string]string{"server": installer.AccessCommitted, "worker-2": installer.AccessCommitted}, f.server.stored(t).History[len(stored.History)-1].Nodes)
}

func TestNodeAddRefusesAJoinerTheTunnelCannotReach(t *testing.T) {
	f := newJoinerFixture(t)
	f.joinerDown = true
	a, _, err := prepareNodeAccess(f.c, f.c.nodes, nil, "node add 192.0.2.12", true)
	require.NoError(t, err)

	err = a.prepareJoiner(f.joiner, f.records)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "UDP port 443")
	assert.Contains(t, err.Error(), "kip cluster access allow")
	assert.Empty(t, f.applied)
}

func TestNodeAddStopsBeforeTheJoinWhenTheJoinerCannotTakeDevices(t *testing.T) {
	f := newJoinerFixture(t)
	f.joinerOps = errors.New("firewalld runs on this node")
	a, _, err := prepareNodeAccess(f.c, f.c.nodes, nil, "node add 192.0.2.12", true)
	require.NoError(t, err)

	err = a.prepareJoiner(f.joiner, f.records)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "firewalld")
	assert.Equal(t, []string{"ops worker-2"}, f.events)
}

func TestNodeAddChecksAJoinerWithoutDevicesBeforeItJoins(t *testing.T) {
	f := newAccessFixture(t, map[string]string{"4": "198.51.100.7"})
	f.server.store(t, listedRecord())
	f.committed(f.server, 4)
	a, _, err := prepareNodeAccess(f.c, f.c.nodes, nil, "node add", true)
	require.NoError(t, err)
	joiner := &accessHost{observed: map[string]string{"4": "192.0.2.99"}}
	rec := installer.NodeRecord{Name: "worker-2", Host: "192.0.2.12", PeerAddress: "10.0.0.12", MachineID: accessWorkerID, Role: installer.NodeRoleWorker}

	err = a.prepareJoiner(accessNode{rec: rec, node: joiner, base: joiner}, append(f.c.records, rec))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "allow --here")
	assert.Empty(t, f.server.writes, "nothing is recorded for a refused joiner")
}

func TestNodeAddOverTheTunnelOnlyNamesThePortWhenTheJoinerIsUnreachable(t *testing.T) {
	f := newJoinerFixture(t)
	f.c.noDirect = true
	f.joinerDown = true
	a, _, err := prepareNodeAccess(f.c, f.c.nodes, nil, "node add 192.0.2.12", true)
	require.NoError(t, err)

	err = a.prepareJoiner(f.joiner, f.records)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "UDP port 443")
	assert.NotContains(t, err.Error(), "changed nothing", "the joiner's record and interface have changed by now")
}

func TestNodeAddWithoutADeviceLoadsTheJoinersInterfaceBeforeTheCheck(t *testing.T) {
	f := newJoinerFixture(t)
	f.deviceRecord(t, "198.51.100.0/24")
	f.c.device, f.c.useTunnel = nil, false
	a, _, err := prepareNodeAccess(f.c, f.c.nodes, nil, "node add 192.0.2.12", true)
	require.NoError(t, err)

	require.NoError(t, a.prepareJoiner(f.joiner, f.records))

	assert.Equal(t, []string{"ops worker-2"}, f.events, "a workstation without a device checks only the direct path")
	_, ok := f.server.stored(t).Ops.Node(accessWorkerID)
	assert.True(t, ok)
	assert.False(t, a.tunnelled(accessWorkerID))
}

func TestNodeAddOverTheDirectPathPointsAtTheTunnel(t *testing.T) {
	f := newJoinerFixture(t)
	f.deviceRecord(t, "198.51.100.0/24")
	f.c.useTunnel = false
	f.joiner.base.(*accessHost).observed["4"] = "192.0.2.99"
	a, _, err := prepareNodeAccess(f.c, f.c.nodes, nil, "node add 192.0.2.12", true)
	require.NoError(t, err)

	err = a.prepareJoiner(f.joiner, f.records)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--via auto")
}

func TestNodeAddAgainKeepsTheJoinersOperatorAddress(t *testing.T) {
	f := newJoinerFixture(t)
	rec := f.server.stored(t)
	rec.Ops.Nodes = append(rec.Ops.Nodes, installer.AccessOpsNode{MachineID: accessWorkerID, Address: "10.99.0.2", PublicKey: testJoinerKey})
	rec.Ops.NextNode = 3
	f.server.store(t, rec)
	a, _, err := prepareNodeAccess(f.c, f.c.nodes, nil, "node add 192.0.2.12", true)
	require.NoError(t, err)

	require.NoError(t, a.prepareJoiner(f.joiner, f.records))

	stored := f.server.stored(t)
	on, ok := stored.Ops.Node(accessWorkerID)
	require.True(t, ok)
	assert.Equal(t, "10.99.0.2", on.Address)
	assert.Equal(t, 3, stored.Ops.NextNode, "no new address is handed out")
	assert.True(t, a.tunnelled(accessWorkerID))
}
