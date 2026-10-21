package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/kip/internal/installer"
	"github.com/getkipper/kipper/kip/internal/localrun"
)

const (
	accessServerID = "0123456789abcdef0123456789abcdef"
	accessWorkerID = "fedcba9876543210fedcba9876543210"
)

// accessHost is a node for the access verbs. On the server it also keeps
// the kipper-access ConfigMap in memory.
type accessHost struct {
	observed map[string]string
	record   string
	writes   []installer.AccessRecord
	deleted  bool
}

func (h *accessHost) Run(command string) (string, error) {
	switch {
	case strings.Contains(command, "get configmap kipper-access"):
		if h.record == "" {
			return "", nil
		}
		data, _ := json.Marshal(map[string]string{"record": h.record})
		return string(data), nil
	case strings.Contains(command, "delete configmap kipper-access"):
		h.record, h.deleted = "", true
		return "", nil
	}
	return "", errors.New("unexpected command: " + command)
}

func (h *accessHost) RunStdin(command string, stdin io.Reader) (string, error) {
	if !strings.Contains(command, "patch configmap kipper-access") {
		return "", errors.New("unexpected command: " + command)
	}
	var patch struct {
		Data map[string]string `json:"data"`
	}
	if err := json.NewDecoder(stdin).Decode(&patch); err != nil {
		return "", err
	}
	h.record = patch.Data["record"]
	var rec installer.AccessRecord
	if err := json.Unmarshal([]byte(h.record), &rec); err != nil {
		return "", err
	}
	h.writes = append(h.writes, rec)
	return "", nil
}

func (h *accessHost) SurvivesSSHRateLimit() bool    { return true }
func (h *accessHost) ConfirmFreshConnection() error { return nil }
func (h *accessHost) ObservedAddress(family string) (string, error) {
	if a, ok := h.observed[family]; ok {
		return a, nil
	}
	return "", errors.New("ssh: connect to host: Network is unreachable")
}

func (h *accessHost) stored(t *testing.T) installer.AccessRecord {
	t.Helper()
	var rec installer.AccessRecord
	require.NoError(t, json.Unmarshal([]byte(h.record), &rec))
	return rec
}

func (h *accessHost) store(t *testing.T, rec installer.AccessRecord) {
	t.Helper()
	b, err := json.Marshal(rec)
	require.NoError(t, err)
	h.record = string(b)
}

// accessFixture is a one-node cluster whose host state and apply calls are
// recorded in memory.
type accessFixture struct {
	out        *bytes.Buffer
	server     *accessHost
	c          *accessCluster
	headers    map[installer.FirewallRunner]string
	applied    []installer.AccessApply
	result     map[string]error
	refused    map[string]error
	removed    []installer.FirewallRunner
	opsRemoved []installer.FirewallRunner
}

func newAccessFixture(t *testing.T, observed map[string]string) *accessFixture {
	t.Helper()
	f := &accessFixture{out: &bytes.Buffer{}, server: &accessHost{observed: observed}, headers: map[installer.FirewallRunner]string{}, result: map[string]error{}, refused: map[string]error{}}
	server := installer.NodeRecord{Name: "server", Host: "192.0.2.10", PeerAddress: "10.0.0.10", MachineID: accessServerID, Role: installer.NodeRoleServer}
	f.c = &accessCluster{
		out:     f.out,
		server:  f.server,
		records: []installer.NodeRecord{server},
		nodes:   []accessNode{{rec: server, node: f.server, base: f.server}},
		families: func(string) []string {
			return []string{"4", "6"}
		},
		who:         "operator",
		readyz:      func(string) error { return nil },
		apiFamilies: []string{"4", "6"},
		now:         func() time.Time { return time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC) },
		pod:         func() (netip.Prefix, error) { return netip.MustParsePrefix("10.42.0.0/16"), nil },
		opsState:    func(installer.CommandRunner) (installer.OpsHostState, error) { return installer.OpsHostState{}, nil },
		ports: func(installer.CommandRunner) ([]int, error) {
			return []int{22}, nil
		},
		state: func(r installer.CommandRunner) (installer.AccessHostState, error) {
			table := f.headers[r.(installer.FirewallRunner)]
			return installer.AccessHostState{Table: table, Operators: table, Live: table != ""}, nil
		},
		live: func(installer.CommandRunner) (installer.AccessLive, error) {
			return installer.AccessLive{Sets: map[string][]netip.Prefix{"admin4": {netip.MustParsePrefix("198.51.100.0/24")}}}, nil
		},
		apply: func(a installer.AccessApply) (string, error) {
			f.applied = append(f.applied, a)
			if err := f.refused[a.Label]; err != nil {
				return "", err
			}
			if err := f.result[a.Label]; err != nil {
				return installer.AccessRestored, err
			}
			return installer.AccessCommitted, nil
		},
		remove: func(r installer.CommandRunner) error {
			if _, closed := r.(closedRunner); closed {
				return errors.New("session closed")
			}
			f.removed = append(f.removed, r.(installer.FirewallRunner))
			return nil
		},
		removeOps: func(r installer.CommandRunner) error {
			if _, closed := r.(closedRunner); closed {
				return errors.New("session closed")
			}
			f.opsRemoved = append(f.opsRemoved, r.(installer.FirewallRunner))
			return nil
		},
	}
	return f
}

// committed makes a host report committed files at generation g.
func (f *accessFixture) committed(r installer.FirewallRunner, g int) {
	f.headers[r] = "# kipper-access generation " + strconv.Itoa(g) + "\nadd element inet kipper_access admin4 { 198.51.100.0/24 }\n"
}

func (f *accessFixture) addWorker() *accessHost {
	w := &accessHost{observed: map[string]string{"4": "198.51.100.7"}}
	rec := installer.NodeRecord{Name: "worker-1", Host: "192.0.2.11", PeerAddress: "10.0.0.11", MachineID: accessWorkerID, Role: installer.NodeRoleWorker}
	f.c.records = append(f.c.records, rec)
	f.c.nodes = append(f.c.nodes, accessNode{rec: rec, node: w, base: w})
	return w
}

func sourcePrefixes(rec installer.AccessRecord) []string {
	var out []string
	for _, s := range rec.Sources {
		out = append(out, s.Prefix)
	}
	return out
}

func TestAccessAllowHereStartsTheListWithBothFamilies(t *testing.T) {
	f := newAccessFixture(t, map[string]string{"4": "198.51.100.7", "6": "2001:db8:1:2:aaaa::9"})

	require.NoError(t, f.c.change("allow", f.c.allowEdit(nil, true, "laptop"), false))

	rec := f.server.stored(t)
	assert.Equal(t, []string{"198.51.100.7/32", "2001:db8:1:2::/64"}, sourcePrefixes(rec))
	assert.Equal(t, 1, rec.Generation)
	assert.Equal(t, "laptop", rec.Sources[0].Note)
	assert.Equal(t, "operator@198.51.100.7", rec.Sources[0].AddedBy)
	require.Len(t, rec.History, 1)
	assert.Equal(t, "allow", rec.History[0].Action)
	assert.Equal(t, map[string]string{"server": installer.AccessCommitted}, rec.History[0].Nodes)

	require.Len(t, f.applied, 1)
	a := f.applied[0]
	assert.Equal(t, []string{"4", "6"}, a.Families)
	assert.Equal(t, 1, a.Intent.Generation)
	assert.Equal(t, []string{"198.51.100.7/32", "2001:db8:1:2::/64"}, prefixStrings(a.Intent.Admin))
	assert.Contains(t, f.out.String(), "restricts every issued kubeconfig")
	assert.NotContains(t, f.out.String(), "one entry")
}

func prefixStrings(ps []netip.Prefix) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.String())
	}
	return out
}

func TestAccessAllowWarnsAboutASingleEntry(t *testing.T) {
	f := newAccessFixture(t, map[string]string{"4": "198.51.100.7"})

	require.NoError(t, f.c.change("allow", f.c.allowEdit(nil, true, ""), false))

	assert.Equal(t, []string{"4"}, f.applied[0].Families)
	assert.Contains(t, f.out.String(), "no IPv6 session")
	assert.Contains(t, f.out.String(), "The list has one entry")
}

func TestAccessAllowRefusesAListThatLocksOutThisWorkstation(t *testing.T) {
	f := newAccessFixture(t, map[string]string{"4": "198.51.100.7"})

	err := f.c.change("allow", f.c.allowEdit([]string{"203.0.113.0/24"}, false, ""), false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "lock you out")
	assert.Empty(t, f.server.writes, "nothing is recorded")
	assert.Empty(t, f.applied, "nothing is loaded")
}

func TestAccessAllowChecksEveryFamilyThatConnected(t *testing.T) {
	f := newAccessFixture(t, map[string]string{"4": "198.51.100.7", "6": "2001:db8::9"})

	err := f.c.change("allow", f.c.allowEdit([]string{"198.51.100.7"}, false, ""), false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "2001:db8::9")
	assert.Empty(t, f.applied)
}

func TestAccessAllowRefusesWhenNoFamilyConnects(t *testing.T) {
	f := newAccessFixture(t, map[string]string{})

	err := f.c.change("allow", f.c.allowEdit(nil, true, ""), false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "changed nothing")
	assert.Empty(t, f.server.writes)
}

func TestAccessRefusesToRunOnTheClusterHost(t *testing.T) {
	f := newAccessFixture(t, nil)
	f.c.nodes[0].base = &localHostRunner{}

	err := f.c.change("allow", f.c.allowEdit(nil, true, ""), false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "from your workstation")
}

type localHostRunner struct{ accessHost }

func (*localHostRunner) ObservedAddress(string) (string, error) { return "", localrun.ErrNoConnection }

func TestAccessAllowAddsToTheRecordAndKeepsExistingSources(t *testing.T) {
	f := newAccessFixture(t, map[string]string{"4": "198.51.100.7"})
	f.server.store(t, installer.AccessRecord{Version: 1, Generation: 3, Sources: []installer.AccessSource{{Prefix: "198.51.100.0/24", Note: "office"}}})
	f.committed(f.server, 3)

	require.NoError(t, f.c.change("allow", f.c.allowEdit([]string{"2001:db8::/48", "198.51.100.0/24"}, false, ""), false))

	rec := f.server.stored(t)
	assert.Equal(t, []string{"198.51.100.0/24", "2001:db8::/48"}, sourcePrefixes(rec))
	assert.Equal(t, 4, rec.Generation)
	assert.Equal(t, []string{"2001:db8::/48"}, rec.History[0].Prefixes)
	assert.NotContains(t, f.out.String(), "restricts every issued kubeconfig", "the first-apply warning is for the first apply")
}

func TestAccessAllowRejectsAnInvalidPrefix(t *testing.T) {
	f := newAccessFixture(t, map[string]string{"4": "198.51.100.7"})

	err := f.c.change("allow", f.c.allowEdit([]string{"0.0.0.0/0"}, false, ""), false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "kip cluster access reset")
	assert.Empty(t, f.server.writes)
}

func TestAccessRevokeRefusesTheLastEntry(t *testing.T) {
	f := newAccessFixture(t, map[string]string{"4": "198.51.100.7"})
	f.server.store(t, installer.AccessRecord{Version: 1, Generation: 2, Sources: []installer.AccessSource{{Prefix: "198.51.100.0/24"}}})
	f.committed(f.server, 2)

	err := f.c.change("revoke", revokeEdit([]string{"198.51.100.0/24"}), false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "reset --yes")
	assert.Empty(t, f.server.writes)
}

func TestAccessRevokeRefusesAPrefixThatCoversThisSession(t *testing.T) {
	f := newAccessFixture(t, map[string]string{"4": "198.51.100.7"})
	f.server.store(t, installer.AccessRecord{Version: 1, Generation: 2, Sources: []installer.AccessSource{{Prefix: "198.51.100.0/24"}, {Prefix: "203.0.113.0/24"}}})
	f.committed(f.server, 2)

	err := f.c.change("revoke", revokeEdit([]string{"198.51.100.0/24"}), false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "lock you out")
	assert.Empty(t, f.applied)
}

func TestAccessRevokeRemovesThePrefix(t *testing.T) {
	f := newAccessFixture(t, map[string]string{"4": "198.51.100.7"})
	f.server.store(t, installer.AccessRecord{Version: 1, Generation: 2, Sources: []installer.AccessSource{{Prefix: "198.51.100.0/24"}, {Prefix: "203.0.113.0/24"}}})
	f.committed(f.server, 2)

	require.NoError(t, f.c.change("revoke", revokeEdit([]string{"203.0.113.0/24"}), false))

	assert.Equal(t, []string{"198.51.100.0/24"}, sourcePrefixes(f.server.stored(t)))
	assert.Equal(t, 3, f.applied[0].Intent.Generation)
	assert.Contains(t, f.out.String(), "Sessions already open stay open")
}

func TestAccessRevokeRefusesAPrefixThatIsNotListed(t *testing.T) {
	f := newAccessFixture(t, map[string]string{"4": "198.51.100.7"})
	f.server.store(t, installer.AccessRecord{Version: 1, Generation: 2, Sources: []installer.AccessSource{{Prefix: "198.51.100.0/24"}, {Prefix: "203.0.113.0/24"}}})
	f.committed(f.server, 2)

	err := f.c.change("revoke", revokeEdit([]string{"192.0.2.0/24"}), false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "not in the access list")
}

func TestAccessApplyWithoutARecordRefuses(t *testing.T) {
	f := newAccessFixture(t, map[string]string{"4": "198.51.100.7"})

	err := f.c.change("apply", nil, false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "allow --here")
}

func TestAccessRefusesAnOrphanedTable(t *testing.T) {
	f := newAccessFixture(t, map[string]string{"4": "198.51.100.7"})
	f.committed(f.server, 5)

	err := f.c.change("allow", f.c.allowEdit(nil, true, ""), false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "kip cluster access reset")
	assert.Empty(t, f.server.writes)
}

func TestAccessRefusesAStaleRecord(t *testing.T) {
	f := newAccessFixture(t, map[string]string{"4": "198.51.100.7"})
	f.server.store(t, installer.AccessRecord{Version: 1, Generation: 2, Sources: []installer.AccessSource{{Prefix: "198.51.100.0/24"}, {Prefix: "203.0.113.0/24"}}})
	f.committed(f.server, 5)

	err := f.c.change("apply", nil, false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--accept-older-record")
	assert.Empty(t, f.applied)
}

func TestAccessApplyAcceptingAnOlderRecordReportsWhatItAdmitsAgain(t *testing.T) {
	f := newAccessFixture(t, map[string]string{"4": "198.51.100.7"})
	f.server.store(t, installer.AccessRecord{Version: 1, Generation: 2, Sources: []installer.AccessSource{{Prefix: "198.51.100.0/24"}, {Prefix: "203.0.113.0/24"}}})
	f.committed(f.server, 5)

	require.NoError(t, f.c.change("apply", nil, true))

	assert.Contains(t, f.out.String(), "admits again")
	assert.Contains(t, f.out.String(), "server: 203.0.113.0/24")
	assert.Equal(t, 6, f.applied[0].Intent.Generation, "the next generation is newer than every node's")
	assert.Equal(t, 6, f.server.stored(t).Generation)
}

func TestAccessApplyRecordsAFailedOutcome(t *testing.T) {
	f := newAccessFixture(t, map[string]string{"4": "198.51.100.7"})
	f.server.store(t, installer.AccessRecord{Version: 1, Generation: 2, Sources: []installer.AccessSource{{Prefix: "198.51.100.0/24"}}})
	f.committed(f.server, 2)
	f.result["server"] = errors.New("a new IPv4 SSH connection failed after the change")

	err := f.c.change("apply", nil, false)

	require.Error(t, err)
	rec := f.server.stored(t)
	assert.Equal(t, "restored: a new IPv4 SSH connection failed after the change", rec.History[0].Nodes["server"])
}

func TestAccessChangesLoadWorkersFirstAndTheServerLast(t *testing.T) {
	f := newAccessFixture(t, map[string]string{"4": "198.51.100.7"})
	f.addWorker()

	require.NoError(t, f.c.change("allow", f.c.allowEdit(nil, true, ""), false))

	require.Len(t, f.applied, 2)
	assert.Equal(t, "worker-1", f.applied[0].Label)
	assert.Equal(t, "10.0.0.10", f.applied[0].Intent.Server4.String(), "the worker admits the server on SSH")
	assert.Equal(t, "server", f.applied[1].Label)
	assert.Contains(t, prefixStrings(f.applied[1].Intent.Cluster4), "10.0.0.11/32")
}

func TestAccessChangeStopsAtTheFirstFailedNode(t *testing.T) {
	f := newAccessFixture(t, map[string]string{"4": "198.51.100.7"})
	f.addWorker()
	f.result["worker-1"] = errors.New("worker-1: loading the new access table failed")

	err := f.c.change("allow", f.c.allowEdit(nil, true, ""), false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "server still holds the previous list")
	require.Len(t, f.applied, 1)
	assert.Equal(t, "not changed", f.server.stored(t).History[0].Nodes["server"])
}

func TestAccessChangeRefusesAWorkerThatSeesThisWorkstationOutsideTheList(t *testing.T) {
	f := newAccessFixture(t, map[string]string{"4": "198.51.100.7"})
	w := f.addWorker()
	w.observed["4"] = "203.0.113.50"

	err := f.c.change("allow", f.c.allowEdit(nil, true, ""), false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "203.0.113.50")
	assert.Contains(t, err.Error(), "worker-1")
	assert.Empty(t, f.applied)
}

func TestAccessResetDeletesTheRecordOnlyWhenEveryNodeWasReset(t *testing.T) {
	f := newAccessFixture(t, map[string]string{"4": "198.51.100.7"})
	f.server.store(t, installer.AccessRecord{Version: 1, Generation: 2, Sources: []installer.AccessSource{{Prefix: "198.51.100.0/24"}}})

	err := f.c.resetAll([]installer.NodeRecord{{Name: "worker-1"}})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "worker-1 is unreachable")
	assert.False(t, f.server.deleted)
	assert.Len(t, f.removed, 1)

	require.NoError(t, f.c.resetAll(nil))
	assert.True(t, f.server.deleted)
}

func TestAccessReportMatchesWhenEveryNodeEnforcesTheRecord(t *testing.T) {
	f := newAccessFixture(t, map[string]string{"4": "198.51.100.7"})
	f.server.store(t, installer.AccessRecord{Version: 1, Generation: 2, Sources: []installer.AccessSource{{Prefix: "198.51.100.0/24"}}})
	f.committed(f.server, 2)
	f.c.families = func(string) []string { return []string{"4"} }
	f.c.live = func(installer.CommandRunner) (installer.AccessLive, error) { return installer.AccessLive{}, nil }

	r := f.c.report([]showNode{{rec: f.c.nodes[0].rec, client: f.server}})

	assert.True(t, r.Present)
	require.Len(t, r.Nodes, 1)
	assert.Equal(t, 2, r.Nodes[0].Generation)
	assert.Contains(t, r.Nodes[0].Admitted, "IPv4 SSH from 198.51.100.7")
}

// An operator without an IPv6 route cannot open IPv6 connections, so these
// failures do not indicate a problem with a list containing only IPv4 prefixes.
func TestAccessReportCountsAFailedFamilyOnlyWhenTheListHoldsIt(t *testing.T) {
	f := newAccessFixture(t, map[string]string{"4": "198.51.100.7"})
	f.server.store(t, installer.AccessRecord{Version: 1, Generation: 2, Sources: []installer.AccessSource{{Prefix: "198.51.100.0/24"}}})
	f.committed(f.server, 2)
	f.c.state = func(installer.CommandRunner) (installer.AccessHostState, error) {
		return installer.AccessHostState{Table: f.headers[f.server], Operators: f.headers[f.server], Live: true, UnitEnabled: "enabled"}, nil
	}
	f.c.live = func(installer.CommandRunner) (installer.AccessLive, error) {
		live := installer.AccessLive{Sets: map[string][]netip.Prefix{"admin4": {netip.MustParsePrefix("198.51.100.0/24")}}}
		live.Chain.Hook, live.Chain.Priority, live.Chain.Policy, live.Chain.Rules = "input", -10, "accept", 8
		return live, nil
	}
	f.c.readyz = func(family string) error {
		if family == "6" {
			return errors.New("dial tcp [2001:db8::10]:6443: connect: network is unreachable")
		}
		return nil
	}

	r := f.c.report([]showNode{{rec: f.c.nodes[0].rec, client: f.server}})

	assert.True(t, r.Match, r.Nodes[0].Problems)
	assert.Empty(t, r.Nodes[0].Problems)
	notes := strings.Join(r.Nodes[0].Notes, "\n")
	assert.Contains(t, notes, "a new IPv6 SSH connection failed")
	assert.Contains(t, notes, "the Kubernetes API did not answer over IPv6")
	var out bytes.Buffer
	printAccessReport(&out, r)
	assert.Contains(t, out.String(), "server: matches")
	assert.Contains(t, out.String(), "a new IPv6 SSH connection failed")

	f.server.observed = map[string]string{}
	r = f.c.report([]showNode{{rec: f.c.nodes[0].rec, client: f.server}})
	assert.False(t, r.Match)
	assert.Contains(t, strings.Join(r.Nodes[0].Problems, "\n"), "a new IPv4 SSH connection failed")

	f.server.observed = map[string]string{"4": "198.51.100.7"}
	f.server.store(t, installer.AccessRecord{Version: 1, Generation: 2, Sources: []installer.AccessSource{{Prefix: "198.51.100.0/24"}, {Prefix: "2001:db8::/32"}}})
	r = f.c.report([]showNode{{rec: f.c.nodes[0].rec, client: f.server}})
	assert.False(t, r.Match)
	problems := strings.Join(r.Nodes[0].Problems, "\n")
	assert.Contains(t, problems, "a new IPv6 SSH connection failed")
	assert.Contains(t, problems, "the Kubernetes API did not answer over IPv6")
}

func TestAccessReportCountsEveryFailedFamilyWithoutAList(t *testing.T) {
	f := newAccessFixture(t, map[string]string{})
	f.c.readyz = func(string) error { return errors.New("dial tcp 192.0.2.10:6443: i/o timeout") }

	r := f.c.report([]showNode{{rec: f.c.nodes[0].rec, client: f.server}})

	assert.False(t, r.Match)
	problems := strings.Join(r.Nodes[0].Problems, "\n")
	assert.Contains(t, problems, "a new IPv4 SSH connection failed")
	assert.Contains(t, problems, "the Kubernetes API did not answer over IPv4")
	assert.Empty(t, r.Nodes[0].Notes)
}

func TestAccessReportFlagsDifferencesAndUnreachableNodes(t *testing.T) {
	f := newAccessFixture(t, map[string]string{"4": "198.51.100.7"})
	f.server.store(t, installer.AccessRecord{Version: 1, Generation: 3, Sources: []installer.AccessSource{{Prefix: "198.51.100.0/24"}, {Prefix: "203.0.113.0/24"}}})
	f.committed(f.server, 2)
	f.c.families = func(string) []string { return []string{"4"} }
	worker := installer.NodeRecord{Name: "worker-1", Host: "192.0.2.11"}

	r := f.c.report([]showNode{{rec: f.c.nodes[0].rec, client: f.server}, {rec: worker, err: errors.New("connection timed out")}})

	assert.False(t, r.Match)
	assert.Contains(t, strings.Join(r.Nodes[0].Problems, "\n"), "holds generation 2 while the record has generation 3")
	assert.Contains(t, strings.Join(r.Nodes[0].Problems, "\n"), "differs from the record")
	assert.Equal(t, []string{"not verified: connection timed out"}, r.Nodes[1].Problems)
}

func TestAccessReportFlagsATableWithoutARecord(t *testing.T) {
	f := newAccessFixture(t, map[string]string{"4": "198.51.100.7"})
	f.committed(f.server, 2)
	f.c.families = func(string) []string { return []string{"4"} }
	f.c.live = func(installer.CommandRunner) (installer.AccessLive, error) { return installer.AccessLive{}, nil }

	r := f.c.report([]showNode{{rec: f.c.nodes[0].rec, client: f.server}})

	assert.False(t, r.Match)
	assert.Contains(t, r.Nodes[0].Problems[0], "no access record")
}

func TestAccessHostFamiliesFollowsALiteralAddress(t *testing.T) {
	assert.Equal(t, []string{"4"}, hostFamilies("192.0.2.10"))
	assert.Equal(t, []string{"6"}, hostFamilies("2001:db8::10"))
}

func TestAccessChangeRefusesWithoutAnAPIBaseline(t *testing.T) {
	f := newAccessFixture(t, map[string]string{"4": "198.51.100.7"})
	f.c.readyz = func(string) error { return errors.New("connection refused") }

	err := f.c.change("allow", f.c.allowEdit(nil, true, ""), false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "did not answer")
	assert.Empty(t, f.server.writes, "nothing is recorded")
	assert.Empty(t, f.applied)
}

func TestAccessChangeRefusesWhenTheAPICannotBeChecked(t *testing.T) {
	f := newAccessFixture(t, map[string]string{"4": "198.51.100.7"})
	f.c.readyz, f.c.apiErr = nil, "loading kubeconfig: no such file"

	err := f.c.change("allow", f.c.allowEdit(nil, true, ""), false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no such file")
	assert.Empty(t, f.server.writes)
}

func TestAccessChangeVerifiesTheAPIOnTheServerInTheFamiliesThatAnswered(t *testing.T) {
	f := newAccessFixture(t, map[string]string{"4": "198.51.100.7", "6": "2001:db8::9"})
	f.c.readyz = func(family string) error {
		if family == "6" {
			return errors.New("no route")
		}
		return nil
	}

	require.NoError(t, f.c.change("allow", f.c.allowEdit(nil, true, ""), false))

	assert.Equal(t, []string{"4"}, f.applied[0].ReadyzOn)
	assert.Contains(t, f.out.String(), "IPv6 API access is not verified")
}

func TestAccessReportMatchesOverlappingGrants(t *testing.T) {
	f := newAccessFixture(t, map[string]string{"4": "198.51.100.7"})
	rec := installer.AccessRecord{Version: 1, Generation: 2, Sources: []installer.AccessSource{{Prefix: "198.51.100.7/32"}, {Prefix: "198.51.100.0/24"}}}
	f.server.store(t, rec)
	f.committed(f.server, 2)
	f.c.families = func(string) []string { return []string{"4"} }

	r := f.c.report([]showNode{{rec: f.c.nodes[0].rec, client: f.server}})

	assert.NotContains(t, strings.Join(r.Nodes[0].Problems, "\n"), "differs from the record")
}

func TestAccessReportFlagsAnUnfinishedRemoval(t *testing.T) {
	f := newAccessFixture(t, map[string]string{"4": "198.51.100.7"})
	f.c.families = func(string) []string { return []string{"4"} }
	worker := installer.NodeRecord{Name: "worker-1", Host: "192.0.2.11", State: installer.NodeStateWiping}

	r := f.c.report([]showNode{{rec: f.c.nodes[0].rec, client: f.server}, {rec: worker, err: errors.New("connection refused")}})

	assert.False(t, r.Match)
	assert.Contains(t, r.Nodes[1].Problems[0], "kip node remove worker-1")
}

func TestAccessChangeRefusesWhileARemovalIsUnfinished(t *testing.T) {
	f := newAccessFixture(t, map[string]string{"4": "198.51.100.7"})
	f.c.records = append(f.c.records, installer.NodeRecord{Name: "worker-1", Host: "192.0.2.11", MachineID: accessWorkerID, Role: installer.NodeRoleWorker, State: installer.NodeStateWiping})

	err := f.c.change("allow", f.c.allowEdit(nil, true, ""), false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "kip node remove worker-1")
	assert.Empty(t, f.server.writes)
}

func TestAccessRefusalBeforeAChangeIsShownOncePerNode(t *testing.T) {
	f := newAccessFixture(t, map[string]string{"4": "198.51.100.7"})
	f.server.store(t, listedRecord())
	f.committed(f.server, 4)
	conflict := "nftables.service is enabled or running on this host"
	f.refused["server"] = errors.New("server: " + conflict)

	require.Error(t, f.c.change("apply", nil, false))

	rec := f.server.stored(t)
	assert.Equal(t, "not changed: "+conflict, rec.History[0].Nodes["server"], "The node name belongs in the key only, so omit it from the outcome.")

	f.c.families = func(string) []string { return []string{"4"} }
	f.c.state = func(installer.CommandRunner) (installer.AccessHostState, error) {
		return installer.AccessHostState{Table: f.headers[f.server], Operators: f.headers[f.server], Live: true, UnitEnabled: "enabled", NftablesService: true}, nil
	}
	f.c.live = func(installer.CommandRunner) (installer.AccessLive, error) { return installer.AccessLive{}, nil }
	var out bytes.Buffer
	printAccessReport(&out, f.c.report([]showNode{{rec: f.c.nodes[0].rec, client: f.server}}))
	assert.Equal(t, 1, strings.Count(out.String(), conflict), out.String())
	assert.Contains(t, out.String(), "server: not changed")
}

func TestAccessReportShowsAnEarlyEntryWithoutAnOutcomeAsNotChanged(t *testing.T) {
	var out bytes.Buffer
	printAccessReport(&out, accessReport{Present: true, Generation: 2, History: []installer.AccessChange{{Action: "apply", Generation: 2, Nodes: map[string]string{"server": ": server: refused"}}}})
	assert.Contains(t, out.String(), "server: not changed\n")
}
