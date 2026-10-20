package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/installer"
)

const (
	serverID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	workerID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func removalRecords() []installer.NodeRecord {
	return []installer.NodeRecord{
		{Name: "server-1", Host: "203.0.113.10", MachineID: serverID, Role: installer.NodeRoleServer, State: installer.NodeStateReady},
		{Name: "worker-1", Host: "203.0.113.11", MachineID: workerID, Role: installer.NodeRoleWorker, State: installer.NodeStateReady},
	}
}

// removalHost is the server's view during a removal. The worker holds the
// only copy of pvc-1 until lastCopyPolls replica listings have passed.
type removalHost struct {
	commands           []string
	lastCopyPolls      int
	nodeDeleted        bool
	longhornDeleteErrs int
	recordDeleted      bool
	workerState        string
	// strandedReplica lists a failed worker replica of pvc-2, which then has
	// no healthy copy anywhere.
	strandedReplica bool
}

// records renders the node records as the ConfigMap's data, without the
// worker once its key was set to null.
func (h *removalHost) records() string {
	data := map[string]string{}
	for _, r := range removalRecords() {
		if r.MachineID == workerID {
			if h.recordDeleted {
				continue
			}
			if h.workerState != "" {
				r.State = h.workerState
			}
		}
		b, _ := json.Marshal(r)
		data[r.MachineID] = string(b)
	}
	b, _ := json.Marshal(data)
	return string(b)
}

func (h *removalHost) Run(cmd string) (string, error) {
	h.commands = append(h.commands, cmd)
	switch {
	case strings.HasPrefix(cmd, "kubectl get node 'worker-1'"):
		if h.nodeDeleted {
			return "", nil
		}
		return "node/worker-1\n", nil
	case strings.HasPrefix(cmd, "kubectl delete node 'worker-1'"):
		h.nodeDeleted = true
		return "", nil
	case strings.Contains(cmd, "delete nodes.longhorn.io"):
		if h.longhornDeleteErrs > 0 {
			h.longhornDeleteErrs--
			return "", errors.New("the server is currently unable to handle the request")
		}
		return "", nil
	case strings.Contains(cmd, `"`+workerID+`":null`):
		h.recordDeleted = true
		return "", nil
	case strings.Contains(cmd, `\"state\":\"removing\"`):
		h.workerState = installer.NodeStateRemoving
		return "", nil
	case strings.Contains(cmd, `\"state\":\"wiping\"`):
		h.workerState = installer.NodeStateWiping
		return "", nil
	case strings.Contains(cmd, "get configmap kipper-nodes --ignore-not-found -o name"):
		return "configmap/kipper-nodes\n", nil
	case strings.Contains(cmd, "jsonpath='{.data}'"):
		return h.records(), nil
	case strings.Contains(cmd, "get crd nodes.longhorn.io"):
		return "customresourcedefinition.apiextensions.k8s.io/nodes.longhorn.io\n", nil
	case strings.Contains(cmd, "get nodes.longhorn.io"):
		return "node.longhorn.io/worker-1\n", nil
	case strings.Contains(cmd, "get replicas.longhorn.io"):
		out := "pvc-1\tworker-1\t2026-10-18T00:00:00Z\t\n"
		if h.strandedReplica {
			out += "pvc-2\tworker-1\t2026-10-18T00:00:00Z\t2026-10-18T00:05:00Z\n"
		}
		if h.lastCopyPolls > 0 {
			h.lastCopyPolls--
			return out, nil
		}
		return out + "pvc-1\tserver-1\t2026-10-18T00:01:00Z\t\n", nil
	case strings.Contains(cmd, "get volumes.longhorn.io"):
		return "pvc-1\tshop\tdata-db-0\n", nil
	}
	return "", nil
}

func (h *removalHost) RunStdin(cmd string, in io.Reader) (string, error) {
	stdin, _ := io.ReadAll(in)
	return h.Run(cmd + " <<< " + string(stdin))
}

func (h *removalHost) index(fragment string) int {
	for i, c := range h.commands {
		if strings.Contains(c, fragment) {
			return i
		}
	}
	return -1
}

func newRemoval(h *removalHost, events *[]string) nodeRemoval {
	records := removalRecords()
	return nodeRemoval{
		Out: io.Discard, Master: h, Worker: records[1],
		HostHoldsReplicas: func() (bool, error) { return true, nil },
		HostRunsK3s:       func() (bool, error) { return true, nil },
		Timeout:           time.Minute, Poll: time.Millisecond, Sleep: func(time.Duration) {},
		WipeWorker: func(claim func() error) error {
			if err := claim(); err != nil {
				return err
			}
			*events = append(*events, "wipe@"+strconv.Itoa(len(h.commands)))
			return nil
		},
		UpdatePeers: func(remaining []installer.NodeRecord) ([]string, error) {
			*events = append(*events, "peers@"+strconv.Itoa(len(h.commands)))
			if len(remaining) != 1 || remaining[0].MachineID != serverID {
				return nil, errors.New("the removed worker is still among the peers")
			}
			return nil, nil
		},
		SaveRecords: func([]installer.NodeRecord) { *events = append(*events, "saved") },
	}
}

func TestNodeRemovalRunsItsStepsInOrder(t *testing.T) {
	h := &removalHost{}
	var events []string
	require.NoError(t, newRemoval(h, &events).run())

	removing := h.index(`\"state\":\"removing\"`)
	evict := h.index(`"evictionRequested":true`)
	drain := h.index("kubectl drain worker-1")
	deleteNode := h.index("kubectl delete node 'worker-1'")
	deleteRecord := h.index(`"` + workerID + `":null`)
	for name, i := range map[string]int{"removing": removing, "evict": evict, "drain": drain, "delete node": deleteNode, "delete record": deleteRecord} {
		require.GreaterOrEqual(t, i, 0, "%s never ran: %v", name, h.commands)
	}
	assert.Less(t, removing, evict, "the record says removing before anything changes")
	assert.Less(t, evict, drain, "Longhorn copies the volumes before the pods leave")
	require.Len(t, events, 3)
	wipeAt, peersAt := events[0], events[1]
	assert.True(t, strings.HasPrefix(wipeAt, "wipe@") && strings.HasPrefix(peersAt, "peers@"), "%v", events)
	assert.Greater(t, atoi(t, wipeAt), drain, "k3s is uninstalled after the drain")
	assert.LessOrEqual(t, atoi(t, wipeAt), deleteNode, "k3s is uninstalled before the Node is deleted, or its kubelet registers it again")
	assert.Greater(t, atoi(t, peersAt), deleteNode)
	assert.GreaterOrEqual(t, deleteRecord, atoi(t, peersAt), "the record goes last, so a failed step can be resumed")
	assert.Equal(t, "saved", events[2])
}

func atoi(t *testing.T, event string) int {
	t.Helper()
	_, n, _ := strings.Cut(event, "@")
	v, err := strconv.Atoi(n)
	require.NoError(t, err)
	return v
}

func TestNodeRemovalWaitsUntilAnotherNodeHasACopy(t *testing.T) {
	h := &removalHost{lastCopyPolls: 3}
	var events []string
	require.NoError(t, newRemoval(h, &events).run())
	assert.Greater(t, h.index("kubectl drain"), 0)
}

func TestNodeRemovalStopsWhileTheWorkerHoldsALastCopy(t *testing.T) {
	h := &removalHost{lastCopyPolls: 1 << 30}
	var events []string
	r := newRemoval(h, &events)
	r.Timeout = time.Nanosecond

	err := r.run()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "shop/data-db-0")
	assert.Contains(t, err.Error(), "run the command again")
	assert.Equal(t, -1, h.index("kubectl drain"), "nothing is drained while the worker holds a last copy")
	assert.Empty(t, events, "the host is left alone")
}

func TestNodeRemovalKeepsTheRecordWhenTheWipeFails(t *testing.T) {
	h := &removalHost{}
	var events []string
	r := newRemoval(h, &events)
	r.WipeWorker = func(func() error) error { return errors.New("connecting to 203.0.113.11: no route to host") }

	require.Error(t, r.run())
	assert.Equal(t, -1, h.index("kubectl delete node"), "a Node whose host still runs k3s would come back")
	assert.Equal(t, -1, h.index(`":null`))
}

func TestNodeRemovalKeepsTheRecordWhenThePeerRulesFail(t *testing.T) {
	h := &removalHost{}
	var events []string
	r := newRemoval(h, &events)
	r.UpdatePeers = func([]installer.NodeRecord) ([]string, error) { return nil, errors.New("ufw failed") }

	err := r.run()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "still recorded as removing")
	assert.Equal(t, -1, h.index(`":null`), "the old address keeps access until its rules are gone, so the record stays for a re-run")
}

func TestNodeRemovalResumesWithoutRewritingTheRecord(t *testing.T) {
	h := &removalHost{}
	var events []string
	h.workerState = installer.NodeStateRemoving
	r := newRemoval(h, &events)
	r.Worker.State = installer.NodeStateRemoving

	require.NoError(t, r.run())
	assert.Equal(t, -1, h.index(`\"state\":\"removing\"`))
}

func TestNodeRemovalResumesAfterTheNodeWasDeleted(t *testing.T) {
	h := &removalHost{longhornDeleteErrs: 1}
	var events []string
	r := newRemoval(h, &events)

	require.Error(t, r.run(), "the first run stops when Longhorn's node cannot be deleted")
	require.True(t, h.nodeDeleted)
	assert.Equal(t, -1, h.index(`":null`), "the record stays for the re-run")

	h.commands = nil
	r.Worker.State = installer.NodeStateRemoving
	require.NoError(t, r.run(), "a re-run finishes once the Node is gone")
	assert.Equal(t, -1, h.index("kubectl cordon"), "there is no Node left to cordon or drain")
	assert.Greater(t, h.index(`":null`), 0)
	assert.Equal(t, 2, strings.Count(strings.Join(events, " "), "wipe@"), "the identity-checked wipe runs again, because a missing Node does not prove the host is clean")
}

func TestRemovableWorkerRefusesTheServer(t *testing.T) {
	_, err := removableWorker(removalRecords(), "server-1")
	assert.ErrorContains(t, err, "only workers can be removed")

	rec, err := removableWorker(removalRecords(), "203.0.113.11")
	require.NoError(t, err)
	assert.Equal(t, "worker-1", rec.Name)

	_, err = removableWorker(removalRecords(), "worker-9")
	assert.ErrorContains(t, err, "no node")
}

func TestWipeRemovedWorkerChecksTheHost(t *testing.T) {
	worker := config.NodeRecord{Name: "worker-1", Host: "203.0.113.11", MachineID: workerID}
	elsewhere := func() (string, error) { return serverID, nil }

	other := &removalIdentityHost{id: "cccccccccccccccccccccccccccccccc"}
	assert.ErrorContains(t, wipeRemovedWorker(other, worker, elsewhere, noClaim), "left alone")
	assert.False(t, other.uninstalled)

	self := &removalIdentityHost{id: workerID}
	here := func() (string, error) { return workerID, nil }
	assert.ErrorContains(t, wipeRemovedWorker(self, worker, here, noClaim), "machine running kip")
	assert.False(t, self.uninstalled)

	same := &removalIdentityHost{id: workerID}
	require.NoError(t, wipeRemovedWorker(same, worker, elsewhere, noClaim))
	assert.True(t, same.uninstalled)

	stays := &removalIdentityHost{id: workerID, k3sStays: true}
	assert.ErrorContains(t, wipeRemovedWorker(stays, worker, elsewhere, noClaim), "still installed", "the Node is only deleted once k3s is gone")
}

func noClaim() error { return nil }

func TestWipeRemovedWorkerClaimsTheWipeAfterTheIdentityChecks(t *testing.T) {
	worker := config.NodeRecord{Name: "worker-1", Host: "203.0.113.11", MachineID: workerID}
	elsewhere := func() (string, error) { return serverID, nil }

	claimed := false
	other := &removalIdentityHost{id: "cccccccccccccccccccccccccccccccc"}
	_ = wipeRemovedWorker(other, worker, elsewhere, func() error { claimed = true; return nil })
	assert.False(t, claimed, "a replaced host leaves the record keepable")

	refused := &removalIdentityHost{id: workerID}
	err := wipeRemovedWorker(refused, worker, elsewhere, func() error { return errors.New("the claim was refused") })
	assert.ErrorContains(t, err, "the claim was refused")
	assert.False(t, refused.uninstalled, "nothing on the host changes without the claim")
}

type removalIdentityHost struct {
	id          string
	uninstalled bool
	k3sStays    bool
}

func (h *removalIdentityHost) Run(cmd string) (string, error) {
	if cmd == "cat /etc/machine-id" {
		return h.id + "\n", nil
	}
	if strings.Contains(cmd, "k3s-agent-uninstall.sh") {
		h.uninstalled = true
	}
	if strings.Contains(cmd, "systemctl is-active --quiet k3s-agent") && h.k3sStays {
		return "present\n", nil
	}
	return "", nil
}

func (h *removalIdentityHost) RunStdin(cmd string, _ io.Reader) (string, error) { return h.Run(cmd) }

func TestRevokePeerExplainsAFirewallKipDoesNotManage(t *testing.T) {
	h := &firewallHost{firewalld: "inactive", ufw: "inactive", osRelease: "ID=ubuntu\nVERSION_ID=\"24.04\"\n"}
	removed := installer.NodeRecord{Name: "worker-1", Host: "203.0.113.11", PeerAddress: "10.0.0.4"}
	note, err := revokePeer(io.Discard, h, "worker-2", installer.FirewallMesh{}, removed)
	require.NoError(t, err)
	assert.Contains(t, note, "kip does not manage the firewall on worker-2")
	assert.Contains(t, note, "10.0.0.4", "the note names the address to revoke")
	for _, c := range h.commands {
		assert.NotContains(t, c, "ufw allow", "an unmanaged firewall is left alone")
	}
}

func TestARemovalInProgressBlocksOtherNodeChanges(t *testing.T) {
	records := removalRecords()
	records[1].State = installer.NodeStateRemoving

	assert.ErrorContains(t, removalBlocksJoin(records, "cccccccccccccccccccccccccccccccc"), "worker-1 is being removed")
	assert.NoError(t, removalBlocksJoin(records, workerID), "joining the node being removed keeps it")
	assert.ErrorContains(t, removalBlocksReboot(records[1]), "being removed")
	assert.NoError(t, removalBlocksReboot(records[0]))

	_, busy := installer.RemovalInProgress(records, workerID)
	assert.False(t, busy, "the node itself does not block its own removal")
}

// Once the Node is gone, Longhorn marks the worker's replicas failed, so the
// healthy-copy check passes, yet the disk may hold a volume's only data.
func TestNodeRemovalLeavesAHostHoldingTheOnlyDataOfAVolume(t *testing.T) {
	h := &removalHost{strandedReplica: true}
	var events []string
	r := newRemoval(h, &events)

	err := r.run()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pvc-2")
	assert.Empty(t, events, "nothing is wiped while the host may hold the only data")

	h = &removalHost{strandedReplica: true}
	events = nil
	r = newRemoval(h, &events)
	r.HostHoldsReplicas = func() (bool, error) { return false, nil }
	require.NoError(t, r.run(), "a host already wiped holds nothing to protect, so a re-run finishes")
}

func TestNodeRemovalStopsWhenAnotherRunKeptTheWorker(t *testing.T) {
	h := &removalHost{}
	var events []string
	r := newRemoval(h, &events)
	r.Worker.State = installer.NodeStateRemoving
	h.workerState = installer.NodeStateJoining

	err := r.run()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no longer recorded as being removed")
	assert.Empty(t, events)
}

func TestNodeRemovalSkipsTheDrainOnceTheHostIsWiped(t *testing.T) {
	h := &removalHost{workerState: installer.NodeStateRemoving}
	var events []string
	r := newRemoval(h, &events)
	r.Worker.State = installer.NodeStateRemoving
	r.HostRunsK3s = func() (bool, error) { return false, nil }
	r.HostHoldsReplicas = func() (bool, error) { return false, nil }

	require.NoError(t, r.run())
	assert.Equal(t, -1, h.index("kubectl drain"), "a host without k3s runs no pods, and draining its dead Node would wait for the timeout")
	assert.Greater(t, h.index("kubectl delete node"), 0)
}

func TestKeepingARemovingNodeNeedsItsHostUnwiped(t *testing.T) {
	rec := removalRecords()[1]
	rec.State = installer.NodeStateRemoving
	assert.ErrorContains(t, keepRemovingNode(&removalIdentityHost{id: workerID}, rec), "already wiped its host")
	assert.NoError(t, keepRemovingNode(&k3sHost{}, rec))
	rec.State = installer.NodeStateReady
	assert.NoError(t, keepRemovingNode(&removalIdentityHost{id: workerID}, rec), "only a removal in progress is checked")
}

func TestKeepingRefusesAWorkerWhoseWipeWasClaimed(t *testing.T) {
	rec := removalRecords()[1]
	rec.State = installer.NodeStateWiping
	err := keepRemovingNode(&k3sHost{}, rec)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "can no longer be kept")
	assert.Contains(t, err.Error(), "kip node remove worker-1")
}

func TestAWipingRecordBlocksOtherNodeChanges(t *testing.T) {
	records := removalRecords()
	records[1].State = installer.NodeStateWiping

	assert.ErrorContains(t, removalBlocksJoin(records, "cccccccccccccccccccccccccccccccc"), "worker-1 is being removed")
	assert.ErrorContains(t, removalBlocksReboot(records[1]), "being removed")
}

func TestTheWipeClaimComesRightBeforeTheUninstall(t *testing.T) {
	h := &removalHost{}
	var events []string
	var out bytes.Buffer
	r := newRemoval(h, &events)
	r.Out = &out

	require.NoError(t, r.run())
	claim := h.index(`\"state\":\"wiping\"`)
	require.GreaterOrEqual(t, claim, 0, "the wipe is claimed: %v", h.commands)
	assert.Greater(t, claim, h.index("kubectl drain"))
	assert.Greater(t, claim, h.index("get replicas.longhorn.io"), "the data checks run first")
	require.NotEmpty(t, events)
	assert.Equal(t, claim+1, atoi(t, events[0]), "the claim is the last server command before the wipe")
	text := out.String()
	assert.Less(t, strings.Index(text, "Draining"), strings.Index(text, "Uninstalling k3s"))
}

func TestAClaimedWipeResumesWithoutDrainingAgain(t *testing.T) {
	h := &removalHost{workerState: installer.NodeStateWiping}
	var events []string
	r := newRemoval(h, &events)
	r.Worker.State = installer.NodeStateWiping
	r.HostHoldsReplicas = func() (bool, error) { return true, nil }

	require.NoError(t, r.run())
	assert.Equal(t, -1, h.index(`\"state\":\"removing\"`), "a claimed wipe is never turned back into a keepable removal")
	assert.Equal(t, -1, h.index("kubectl drain"))
	assert.Contains(t, strings.Join(events, " "), "wipe@", "the wipe runs again, since an interrupted one may not have finished")
	assert.Greater(t, h.index(`":null`), 0)
}

func TestForcedRemovalClaimsTheWipeBeforeDeletingAnything(t *testing.T) {
	h := &removalHost{}
	var events []string
	r := forcedRemoval(h, &events)

	require.NoError(t, r.run())
	claim := h.index(`\"state\":\"wiping\"`)
	require.GreaterOrEqual(t, claim, 0, "a forced removal claims the wipe too")
	assert.Less(t, claim, h.index("kubectl delete node"))
	assert.Less(t, claim, atoi(t, events[0]), "before the peer rules change")
}

func TestAResumedWipeChecksTheDataAgain(t *testing.T) {
	h := &removalHost{workerState: installer.NodeStateWiping, strandedReplica: true}
	var events []string
	r := newRemoval(h, &events)
	r.Worker.State = installer.NodeStateWiping

	err := r.run()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pvc-2")
	assert.Empty(t, events, "a copy that became the only one after the claim is not wiped")
}

func TestAForcedRemovalResumesAClaimedWipe(t *testing.T) {
	h := &removalHost{workerState: installer.NodeStateWiping}
	var events []string
	r := forcedRemoval(h, &events)
	r.Worker.State = installer.NodeStateWiping

	require.NoError(t, r.run())
	assert.Equal(t, -1, h.index(`\"state\":\"removing\"`))
	assert.Greater(t, h.index(`":null`), 0)
}

type k3sHost struct{}

func (k3sHost) Run(string) (string, error)                 { return "present\n", nil }
func (k3sHost) RunStdin(string, io.Reader) (string, error) { return "present\n", nil }

func TestHostHoldsReplicasNeverReadsAFailureAsEmpty(t *testing.T) {
	_, err := hostHoldsReplicas(answerHost{err: errors.New("ls: reading directory: Input/output error")})
	assert.Error(t, err, "an unreadable directory cannot authorize the wipe")
	_, err = hostHoldsReplicas(answerHost{out: "Warning: something\n"})
	assert.Error(t, err)
	holds, err := hostHoldsReplicas(answerHost{out: "present\n"})
	require.NoError(t, err)
	assert.True(t, holds)
	holds, err = hostHoldsReplicas(answerHost{out: "empty\n"})
	require.NoError(t, err)
	assert.False(t, holds)
}

type answerHost struct {
	out string
	err error
}

func (h answerHost) Run(string) (string, error)                 { return h.out, h.err }
func (h answerHost) RunStdin(string, io.Reader) (string, error) { return h.out, h.err }

func TestRemovalStartsOnlyWhileTheWorkerAnswers(t *testing.T) {
	worker := removalRecords()[1]
	down := func() (installer.CommandRunner, func(), error) { return nil, nil, errors.New("no route to host") }
	assert.ErrorContains(t, checkWorkerIdentity(worker, down), "nothing was changed")

	other := func() (installer.CommandRunner, func(), error) {
		return &removalIdentityHost{id: "cccccccccccccccccccccccccccccccc"}, func() {}, nil
	}
	assert.ErrorContains(t, checkWorkerIdentity(worker, other), "not the recorded")

	same := func() (installer.CommandRunner, func(), error) {
		return &removalIdentityHost{id: workerID}, func() {}, nil
	}
	assert.NoError(t, checkWorkerIdentity(worker, same))
}

// TestServerIsTheClusterChecksTheLocalIDFirst covers a different cluster at the
// same address whose records match its own ID, making the local ID check necessary.
func TestServerIsTheClusterChecksTheLocalIDFirst(t *testing.T) {
	records := removalRecords()
	other := "cccccccccccccccccccccccccccccccc"
	otherRecords := []installer.NodeRecord{{Name: "server-1", MachineID: other, Role: installer.NodeRoleServer}}

	assert.ErrorContains(t, serverIsTheCluster(other, serverID, otherRecords), "recorded as "+serverID)
	assert.NoError(t, serverIsTheCluster(serverID, serverID, records))
	assert.ErrorContains(t, serverIsTheCluster(serverID, "", otherRecords), "records its server")
	assert.NoError(t, serverIsTheCluster(other, "", otherRecords), "an entry without a local ID falls back to the server's own record")
	assert.ErrorContains(t, serverIsTheCluster(serverID, serverID, records[1:]), "hold no server", "records without a server fail closed")
}

func forcedRemoval(h *removalHost, events *[]string) nodeRemoval {
	r := newRemoval(h, events)
	r.Force = true
	fail := func() (bool, error) { return false, errors.New("the host must not be contacted") }
	r.HostRunsK3s, r.HostHoldsReplicas = fail, fail
	r.WipeWorker = func(func() error) error {
		*events = append(*events, "wipe")
		return errors.New("the host must not be wiped")
	}
	return r
}

func TestForcedRemovalDeregistersWithoutTouchingTheHost(t *testing.T) {
	h := &removalHost{}
	var events []string
	var out bytes.Buffer
	r := forcedRemoval(h, &events)
	r.Out = &out

	require.NoError(t, r.run())
	assert.NotContains(t, strings.Join(events, " "), "wipe")
	assert.Equal(t, -1, h.index("kubectl drain"), "a dead node cannot be drained")
	assert.Greater(t, h.index("kubectl delete node 'worker-1'"), 0)
	assert.Greater(t, h.index(`":null`), 0, "the record is dropped")
	assert.Contains(t, strings.Join(events, " "), "peers@", "the address loses its firewall access")
	assert.Contains(t, out.String(), "still holds", "the operator learns the host keeps valid credentials")
}

func TestForcedRemovalRefusesWhileTheWorkerHoldsTheOnlyData(t *testing.T) {
	h := &removalHost{strandedReplica: true}
	var events []string
	r := forcedRemoval(h, &events)

	err := r.run()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pvc-2")
	assert.Contains(t, err.Error(), "--accept-data-loss")
	assert.Equal(t, -1, h.index("kubectl delete node"), "nothing is deregistered without consent")

	h = &removalHost{strandedReplica: true}
	events = nil
	r = forcedRemoval(h, &events)
	r.AcceptDataLoss = true
	require.NoError(t, r.run())
	assert.Greater(t, h.index("kubectl delete node"), 0)
}

func TestForceIsRefusedForAHostThatStillAnswers(t *testing.T) {
	worker := removalRecords()[1]
	alive := func() (installer.CommandRunner, func(), error) {
		return &removalIdentityHost{id: workerID}, func() {}, nil
	}
	assert.ErrorContains(t, refuseForceForLiveHost(worker, alive), "still answers")

	down := func() (installer.CommandRunner, func(), error) { return nil, nil, errors.New("no route to host") }
	assert.NoError(t, refuseForceForLiveHost(worker, down))

	replaced := func() (installer.CommandRunner, func(), error) {
		return &removalIdentityHost{id: "cccccccccccccccccccccccccccccccc"}, func() {}, nil
	}
	assert.NoError(t, refuseForceForLiveHost(worker, replaced), "another machine at the address is not the worker")
}

func TestNodeRemoveIsListedInHelp(t *testing.T) {
	assert.False(t, nodeRemoveCmd.Hidden)
}
