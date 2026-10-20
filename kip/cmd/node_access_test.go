package cmd

import (
	"bytes"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
}

func (h *heldHost) Run(string) (string, error) {
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
