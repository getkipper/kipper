package installer

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// routedRunner answers each command with the reply of the first route whose
// substring it contains, and records commands and stdin.
type routedRunner struct {
	routes   []route
	commands []string
	stdins   []string
}

type route struct {
	contains string
	out      string
	err      error
}

func (r *routedRunner) SurvivesSSHRateLimit() bool    { return true }
func (r *routedRunner) ConfirmFreshConnection() error { return nil }

func (r *routedRunner) Run(command string) (string, error) {
	r.commands = append(r.commands, command)
	for _, rt := range r.routes {
		if strings.Contains(command, rt.contains) {
			return rt.out, rt.err
		}
	}
	return "", nil
}

func (r *routedRunner) RunStdin(command string, stdin io.Reader) (string, error) {
	b, _ := io.ReadAll(stdin)
	r.stdins = append(r.stdins, string(b))
	return r.Run(command)
}

func (r *routedRunner) ran(substring string) bool {
	for _, c := range r.commands {
		if strings.Contains(c, substring) {
			return true
		}
	}
	return false
}

const (
	serverMachine = "0123456789abcdef0123456789abcdef"
	workerMachine = "fedcba9876543210fedcba9876543210"
)

func recordsJSON(t *testing.T, records ...NodeRecord) string {
	t.Helper()
	data := map[string]string{}
	for _, r := range records {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		data[r.MachineID] = string(b)
	}
	b, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestReadNodeRecordsTreatsAMissingConfigMapAsEmpty(t *testing.T) {
	records, err := ReadNodeRecords(&routedRunner{})
	if err != nil || len(records) != 0 {
		t.Fatalf("records = %v, err = %v; want none", records, err)
	}
}

func TestReadNodeRecordsFailsWhenTheAPIDoesNotAnswer(t *testing.T) {
	_, err := ReadNodeRecords(&routedRunner{routes: []route{{contains: "kipper-nodes", err: errors.New("connection refused")}}})
	if err == nil {
		t.Fatal("an unreadable ConfigMap must not read as no records")
	}
}

func TestReadNodeRecordsListsTheServerFirst(t *testing.T) {
	out := recordsJSON(t,
		NodeRecord{Name: "a-worker", Host: "203.0.113.11", MachineID: workerMachine, Role: NodeRoleWorker, State: NodeStateReady},
		NodeRecord{Name: "z-server", Host: "203.0.113.10", MachineID: serverMachine, Role: NodeRoleServer, State: NodeStateReady},
	)
	records, err := ReadNodeRecords(&routedRunner{routes: []route{{contains: "kipper-nodes", out: out}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[0].Name != "z-server" || records[1].Name != "a-worker" {
		t.Fatalf("records = %+v, want the server first", records)
	}
}

func TestReadNodeRecordsRefusesUnsafeValues(t *testing.T) {
	good := NodeRecord{Name: "worker-1", Host: "203.0.113.11", MachineID: workerMachine, Role: NodeRoleWorker, State: NodeStateReady}
	for name, mutate := range map[string]func(*NodeRecord){
		"peer address": func(r *NodeRecord) { r.PeerAddress = "192.0.2.1; reboot" },
		"ipv6 peer":    func(r *NodeRecord) { r.PeerAddress = "2001:db8::1" },
		"interface":    func(r *NodeRecord) { r.Interface = "eth0 $(id)" },
		"host":         func(r *NodeRecord) { r.Host = "203.0.113.11 -oProxyCommand=x" },
		"role":         func(r *NodeRecord) { r.Role = "admin" },
		"state":        func(r *NodeRecord) { r.State = "" },
		"mode":         func(r *NodeRecord) { r.Mode = "mesh" },
		"name":         func(r *NodeRecord) { r.Name = "Worker_1" },
	} {
		t.Run(name, func(t *testing.T) {
			r := good
			mutate(&r)
			_, err := ReadNodeRecords(&routedRunner{routes: []route{{contains: "kipper-nodes", out: recordsJSON(t, r)}}})
			if err == nil {
				t.Fatalf("record with bad %s was accepted", name)
			}
		})
	}
}

func TestReadNodeRecordsRefusesAnEntryUnderAnotherMachine(t *testing.T) {
	b, _ := json.Marshal(NodeRecord{Name: "worker-1", Host: "203.0.113.11", MachineID: workerMachine, Role: NodeRoleWorker, State: NodeStateReady})
	out, _ := json.Marshal(map[string]string{serverMachine: string(b)})
	if _, err := ReadNodeRecords(&routedRunner{routes: []route{{contains: "kipper-nodes", out: string(out)}}}); err == nil {
		t.Fatal("an entry keyed by another machine id was accepted")
	}
}

func TestSaveNodeRecordPatchesTheEntryOnStdin(t *testing.T) {
	runner := &routedRunner{}
	r := NodeRecord{Name: "worker-1", Host: "203.0.113.11", MachineID: workerMachine, Role: NodeRoleWorker, Mode: NetworkModePrivate, PeerAddress: "192.0.2.11", Interface: "enp7s0.4000", State: NodeStateJoining}
	if err := SaveNodeRecord(runner, r); err != nil {
		t.Fatal(err)
	}
	if len(runner.commands) != 1 || !strings.Contains(runner.commands[0], "patch configmap kipper-nodes --type merge --patch-file /dev/stdin") {
		t.Fatalf("commands = %q", runner.commands)
	}
	var patch struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal([]byte(runner.stdins[0]), &patch); err != nil {
		t.Fatal(err)
	}
	var got NodeRecord
	if err := json.Unmarshal([]byte(patch.Data[workerMachine]), &got); err != nil || got != r {
		t.Fatalf("patched record = %+v (%v), want %+v", got, err, r)
	}
}

func TestSaveNodeRecordRefusesAnInvalidRecord(t *testing.T) {
	runner := &routedRunner{}
	if err := SaveNodeRecord(runner, NodeRecord{Name: "worker-1", MachineID: workerMachine, Role: NodeRoleWorker, State: NodeStateReady}); err == nil {
		t.Fatal("a record without an SSH address was saved")
	}
	if len(runner.commands) != 0 {
		t.Fatalf("commands ran for an invalid record: %q", runner.commands)
	}
}

func TestEnsureServerRecordBackfillsTheServer(t *testing.T) {
	runner := &routedRunner{routes: []route{
		{contains: "patch configmap", out: ""},
		{contains: "get configmap kipper-nodes", out: ""},
		{contains: "cat /etc/machine-id", out: serverMachine + "\n"},
		{contains: "nodeInfo.machineID==", out: "test-server\n"},
	}}
	records, err := EnsureServerRecord(runner, "203.0.113.10")
	if err != nil {
		t.Fatal(err)
	}
	want := NodeRecord{Name: "test-server", Host: "203.0.113.10", MachineID: serverMachine, Role: NodeRoleServer, State: NodeStateReady}
	if len(records) != 1 || records[0] != want {
		t.Fatalf("records = %+v, want %+v", records, want)
	}
	if !runner.ran("patch configmap kipper-nodes") {
		t.Fatal("the backfilled server record was not saved")
	}
}

func TestEnsureServerRecordKeepsAnExistingServer(t *testing.T) {
	server := NodeRecord{Name: "test-server", Host: "203.0.113.10", MachineID: serverMachine, Role: NodeRoleServer, State: NodeStateReady}
	runner := &routedRunner{routes: []route{
		{contains: "get configmap kipper-nodes", out: recordsJSON(t, server)},
		{contains: "cat /etc/machine-id", out: serverMachine + "\n"},
	}}
	if _, err := EnsureServerRecord(runner, "198.51.100.1"); err != nil {
		t.Fatal(err)
	}
	if runner.ran("patch configmap") {
		t.Fatal("an existing server record was rewritten")
	}
}

func TestRequireRecordedNodesRefusesAHandJoinedNode(t *testing.T) {
	server := NodeRecord{Name: "test-server", Host: "203.0.113.10", MachineID: serverMachine, Role: NodeRoleServer, State: NodeStateReady}
	runner := &routedRunner{routes: []route{
		{contains: "get configmap kipper-nodes", out: recordsJSON(t, server)},
		{contains: "cat /etc/machine-id", out: serverMachine + "\n"},
		{contains: "nodeInfo.machineID}", out: "test-server|" + serverMachine + "\nhand-joined|" + workerMachine + "\n"},
	}}
	_, err := RequireRecordedNodes(runner, "203.0.113.10")
	if err == nil || !strings.Contains(err.Error(), "hand-joined") || !strings.Contains(err.Error(), "kip node add") {
		t.Fatalf("err = %v, want a refusal naming hand-joined and kip node add", err)
	}
}

func TestRequireRecordedNodesAcceptsRecordedNodes(t *testing.T) {
	server := NodeRecord{Name: "test-server", Host: "203.0.113.10", MachineID: serverMachine, Role: NodeRoleServer, State: NodeStateReady}
	worker := NodeRecord{Name: "worker-1", Host: "203.0.113.11", MachineID: workerMachine, Role: NodeRoleWorker, State: NodeStateJoining}
	runner := &routedRunner{routes: []route{
		{contains: "get configmap kipper-nodes", out: recordsJSON(t, server, worker)},
		{contains: "cat /etc/machine-id", out: serverMachine + "\n"},
		{contains: "nodeInfo.machineID}", out: "test-server|" + serverMachine + "\nworker-1|" + workerMachine + "\n"},
	}}
	records, err := RequireRecordedNodes(runner, "203.0.113.10")
	if err != nil || len(records) != 2 {
		t.Fatalf("records = %+v, err = %v", records, err)
	}
}

func TestRefuseUnrecordedNodesCountsTheServerAsRecorded(t *testing.T) {
	runner := &routedRunner{routes: []route{
		{contains: "get configmap kipper-nodes", out: ""},
		{contains: "nodeInfo.machineID}", out: "test-server|" + serverMachine + "\n"},
	}}
	if _, err := refuseUnrecordedNodes(runner, serverMachine); err != nil {
		t.Fatalf("a single-node cluster without records was refused: %v", err)
	}
}

func TestRefuseUnrecordedNodesStopsAReRunWithAHandJoinedNode(t *testing.T) {
	runner := &routedRunner{routes: []route{
		{contains: "get configmap kipper-nodes", out: ""},
		{contains: "nodeInfo.machineID}", out: "test-server|" + serverMachine + "\nhand-joined|" + workerMachine + "\n"},
	}}
	if _, err := refuseUnrecordedNodes(runner, serverMachine); err == nil || !strings.Contains(err.Error(), "hand-joined") {
		t.Fatalf("err = %v, want a refusal naming hand-joined", err)
	}
}

func TestRefuseUnrecordedNodesFailsClosedOnCorruptRecords(t *testing.T) {
	runner := &routedRunner{routes: []route{{contains: "get configmap kipper-nodes", out: `{"x":"not json"}`}}}
	if _, err := refuseUnrecordedNodes(runner, serverMachine); err == nil {
		t.Fatal("corrupt records let the re-run through")
	}
}

func TestRefuseUnrecordedNodesNeedsTheServersMachineID(t *testing.T) {
	if _, err := refuseUnrecordedNodes(&routedRunner{}, ""); err == nil {
		t.Fatal("an unknown server identity let the re-run through")
	}
}

func TestRefuseUnrecordedNodesRefusesAnotherMachineAsTheServer(t *testing.T) {
	server := NodeRecord{Name: "test-server", Host: "203.0.113.10", MachineID: serverMachine, Role: NodeRoleServer, State: NodeStateReady}
	runner := &routedRunner{routes: []route{
		{contains: "get configmap kipper-nodes", out: recordsJSON(t, server)},
		{contains: "nodeInfo.machineID}", out: "test-server|" + serverMachine + "\n"},
	}}
	if _, err := refuseUnrecordedNodes(runner, workerMachine); err == nil || !strings.Contains(err.Error(), "test-server") {
		t.Fatalf("err = %v, want a refusal naming the recorded server", err)
	}
}

// API access alone does not identify the server: a worker could hold an admin
// kubeconfig. Compare the host's machine ID with the recorded server's.
func TestEnsureServerRecordRefusesAnotherMachine(t *testing.T) {
	server := NodeRecord{Name: "test-server", Host: "203.0.113.10", MachineID: serverMachine, Role: NodeRoleServer, State: NodeStateReady}
	runner := &routedRunner{routes: []route{
		{contains: "get configmap kipper-nodes", out: recordsJSON(t, server)},
		{contains: "cat /etc/machine-id", out: workerMachine + "\n"},
	}}
	if _, err := EnsureServerRecord(runner, "203.0.113.10"); err == nil {
		t.Fatal("a host with another machine id was accepted as the server")
	}
}

func TestUnrecordedNodesRefusesASharedMachineID(t *testing.T) {
	server := NodeRecord{Name: "test-server", Host: "203.0.113.10", MachineID: serverMachine, Role: NodeRoleServer, State: NodeStateReady}
	runner := &routedRunner{routes: []route{{contains: "nodeInfo.machineID}", out: "test-server|" + serverMachine + "\nclone|" + serverMachine + "\n"}}}
	if _, err := UnrecordedNodes(runner, []NodeRecord{server}); !errors.Is(err, errSharedMachineID) {
		t.Fatalf("err = %v, want errSharedMachineID", err)
	}
}

func TestUnrecordedNodesRefusesANodeUnderAnotherName(t *testing.T) {
	worker := NodeRecord{Name: "worker-1", Host: "203.0.113.11", MachineID: workerMachine, Role: NodeRoleWorker, State: NodeStateReady}
	runner := &routedRunner{routes: []route{{contains: "nodeInfo.machineID}", out: "impostor|" + workerMachine + "\n"}}}
	if _, err := UnrecordedNodes(runner, []NodeRecord{worker}); err == nil {
		t.Fatal("a node running a recorded machine under another name was accepted")
	}
}

func TestValidNodeHost(t *testing.T) {
	for host, want := range map[string]bool{
		"203.0.113.10":       true,
		"2001:db8::1":        true,
		"fe80::1%$(id)":      false,
		"fe80::1%eth0":       false,
		"node-1.example.com": true,
		"":                   false,
		"-oProxyCommand=x":   false,
		"$(id)":              false,
		"host;reboot":        false,
		"`id`":               false,
		"host name":          false,
		"a'b":                false,
	} {
		if got := ValidNodeHost(host); got != want {
			t.Errorf("ValidNodeHost(%q) = %v, want %v", host, got, want)
		}
	}
}

// Preserve existing names so the placeholder cannot hide a renamed node.
func TestWithUnrecordedHostKeepsAnExistingRecordsNameCheck(t *testing.T) {
	server := NodeRecord{Name: "old-server", Host: "203.0.113.10", MachineID: serverMachine, Role: NodeRoleServer, State: NodeStateReady}
	runner := &routedRunner{routes: []route{{contains: "nodeInfo.machineID}", out: "new-server|" + serverMachine + "\n"}}}
	if _, err := UnrecordedNodes(runner, WithUnrecordedHost([]NodeRecord{server}, serverMachine)); err == nil {
		t.Fatal("a renamed server passed through the stand-in")
	}
	got := WithUnrecordedHost([]NodeRecord{server}, workerMachine)
	if len(got) != 2 || got[1].MachineID != workerMachine || got[1].Name != "" {
		t.Fatalf("records = %+v, want a nameless stand-in for the new machine", got)
	}
}

// A joining record holds the name kip expected before registration; the
// node may register under another one, and the retry must still match it.
func TestUnrecordedNodesMatchesAJoiningRecordByMachineID(t *testing.T) {
	joining := NodeRecord{Name: "guessed", Host: "203.0.113.11", MachineID: workerMachine, Role: NodeRoleWorker, State: NodeStateJoining}
	runner := &routedRunner{routes: []route{{contains: "nodeInfo.machineID}", out: "actual-name|" + workerMachine + "\n"}}}
	unrecorded, err := UnrecordedNodes(runner, []NodeRecord{joining})
	require.NoError(t, err)
	assert.Empty(t, unrecorded)

	joining.State = NodeStateReady
	_, err = UnrecordedNodes(runner, []NodeRecord{joining})
	assert.Error(t, err, "a ready record keeps its name check")
}

func TestRequirePeersRecordedForJoin(t *testing.T) {
	const newMachine = "11111111111111111111111111111111"
	server := NodeRecord{Name: "test-server", Host: "203.0.113.10", MachineID: serverMachine, Role: NodeRoleServer, State: NodeStateReady}
	records := []NodeRecord{server}
	nodes := func(listing string) *routedRunner {
		return &routedRunner{routes: []route{{contains: "nodeInfo.machineID}", out: listing}}}
	}
	handJoined := "test-server|" + serverMachine + "\nhand-joined|" + workerMachine + "\n"

	err := RequirePeersRecordedForJoin(nodes(handJoined), records, newMachine)
	if err == nil || !strings.Contains(err.Error(), "hand-joined") {
		t.Fatalf("err = %v, want a fresh join refused while hand-joined has no record", err)
	}
	adopting := nodes(handJoined + "other|" + newMachine + "\n")
	if err := RequirePeersRecordedForJoin(adopting, records, newMachine); err != nil {
		t.Fatalf("adopting one unrecorded node while another waits was refused: %v", err)
	}
	if len(adopting.commands) != 1 {
		t.Fatalf("listed nodes %d times; both checks must read one listing, or a node registering in between changes the answer", len(adopting.commands))
	}
	resuming := append(append([]NodeRecord(nil), records...), NodeRecord{Name: "other", MachineID: newMachine, Role: NodeRoleWorker, State: NodeStateJoining})
	if err := RequirePeersRecordedForJoin(nodes(handJoined+"other|"+newMachine+"\n"), resuming, newMachine); err != nil {
		t.Fatalf("resuming an interrupted adoption while another node waits was refused: %v", err)
	}
	if err := RequirePeersRecordedForJoin(nodes(handJoined), resuming, newMachine); err == nil {
		t.Fatal("a joining record whose host has not registered yet must not excuse an unrecorded peer")
	}
	if err := RequirePeersRecordedForJoin(nodes("test-server|"+serverMachine+"\n"), records, newMachine); err != nil {
		t.Fatalf("a fresh join on a fully recorded cluster was refused: %v", err)
	}
}

func TestRefuseUnrecordedNodesStopsAReRunDuringARemoval(t *testing.T) {
	data := `{"` + serverMachine + `":"{\"name\":\"test-server\",\"host\":\"203.0.113.10\",\"machineID\":\"` + serverMachine + `\",\"role\":\"server\",\"state\":\"ready\"}","` +
		workerMachine + `":"{\"name\":\"worker-1\",\"host\":\"203.0.113.11\",\"machineID\":\"` + workerMachine + `\",\"role\":\"worker\",\"state\":\"removing\"}"}`
	runner := &routedRunner{routes: []route{
		{contains: "get configmap kipper-nodes", out: data},
		{contains: "nodeInfo.machineID}", out: "test-server|" + serverMachine + "\nworker-1|" + workerMachine + "\n"},
	}}
	if _, err := refuseUnrecordedNodes(runner, serverMachine); err == nil || !strings.Contains(err.Error(), "worker-1 is being removed") {
		t.Fatalf("err = %v, want a refusal while worker-1 is being removed", err)
	}
}
