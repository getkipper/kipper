package cmd

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/controller/pkg/stranded"
	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/installer"
)

const newBootID = "2b3c4d5e-6f7a-4b8c-9d0e-1f2a3b4c5d6e"

type fakeRejoinCluster struct {
	*fakeFenceCluster
}

func (c fakeRejoinCluster) Cordon(string) error { c.add("cordon"); return nil }
func (c fakeRejoinCluster) RemoveTaint(string) error {
	c.add("untaint")
	c.node.OutOfService = false
	return nil
}

type fakeRejoinHost struct {
	*fakeFenceHost
	boot         *string
	cluster      *fakeFenceCluster
	rebootTo     string
	quiescentErr error
}

func (h *fakeRejoinHost) BootID() (string, error) { return *h.boot, nil }
func (h *fakeRejoinHost) VerifyInterlock() error  { *h.log = append(*h.log, "verify"); return nil }
func (h *fakeRejoinHost) Quiescent() error {
	*h.log = append(*h.log, "quiescent")
	return h.quiescentErr
}
func (h *fakeRejoinHost) Runner() installer.CommandRunner { return nil }
func (h *fakeRejoinHost) Reboot() error {
	*h.log = append(*h.log, "reboot")
	*h.boot = h.rebootTo
	return nil
}
func (h *fakeRejoinHost) RemoveInterlock() error {
	*h.log = append(*h.log, "remove-interlock")
	return nil
}
func (h *fakeRejoinHost) StartAgent() error {
	*h.log = append(*h.log, "start-agent")
	h.cluster.node.Ready = true
	h.cluster.node.BootID = *h.boot
	return nil
}

type rejoinTest struct {
	ft     *fenceTest
	boot   string
	rejoin *nodeRejoin
	host   *fakeRejoinHost
}

func newRejoinTest(fence *config.NodeFence, bootNow string) *rejoinTest {
	rec := worker(installer.NodeStateFenced)
	rec.Fence = fence
	rt := &rejoinTest{ft: newFenceTest(rec), boot: bootNow}
	rt.ft.cluster.node.OutOfService = true
	rt.host = &fakeRejoinHost{fakeFenceHost: rt.ft.host, boot: &rt.boot, cluster: rt.ft.cluster, rebootTo: newBootID}
	now := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	rt.rejoin = &nodeRejoin{
		Out:       &rt.ft.out,
		Cluster:   fakeRejoinCluster{rt.ft.cluster},
		Host:      rt.host,
		Reconnect: func() (rejoinHost, error) { return rt.host, nil },
		Checks: func(string, installer.CommandRunner) error {
			rt.ft.cluster.add("checks")
			return nil
		},
		Confirm: func(p string) (bool, error) {
			rt.ft.asked = append(rt.ft.asked, p)
			return rt.ft.answer, nil
		},
		Interactive: true,
		Now:         func() time.Time { return now },
		Sleep:       func(d time.Duration) { now = now.Add(d) },
		Poll:        10 * time.Second,
		BootWait:    10 * time.Minute,
		ReadyWait:   5 * time.Minute,
	}
	return rt
}

func (rt *rejoinTest) run(rec installer.NodeRecord) error {
	r := rt.rejoin
	return r.run(rec)
}

func killallFence() *config.NodeFence {
	return &config.NodeFence{Method: installer.FenceMethodKillall, Operator: "operator", StartedAt: healthy, FencedAt: healthy, BootID: fenceBootID}
}

func TestRejoinRebootsAHostThatHasNotBootedSinceTheFence(t *testing.T) {
	rt := newRejoinTest(killallFence(), fenceBootID)
	require.NoError(t, rt.run(rt.ft.record()))
	log := rt.ft.log
	interlock, reboot, verify, cordon, remove, start, untaint, checks := index(log, "interlock"), index(log, "reboot"), index(log, "verify"), index(log, "cordon"), index(log, "remove-interlock"), index(log, "start-agent"), index(log, "untaint"), index(log, "checks")
	assert.True(t, interlock < reboot && reboot < verify && verify < cordon && cordon < remove && remove < start && start < untaint && untaint < checks, "%q", log)
	assert.True(t, index(log, "save:fenced") < reboot, "the checkpoint is saved before the reboot: %q", log)
	rec := rt.ft.record()
	assert.Equal(t, installer.NodeStateReady, rec.State)
	assert.Nil(t, rec.Fence)
}

func TestRejoinSkipsTheRebootAfterANewBoot(t *testing.T) {
	rt := newRejoinTest(killallFence(), newBootID)
	require.NoError(t, rt.run(rt.ft.record()))
	assert.Equal(t, -1, index(rt.ft.log, "reboot"))
	log := rt.ft.log
	interlock, verify, quiescent, remove := index(log, "interlock"), index(log, "verify"), index(log, "quiescent"), index(log, "remove-interlock")
	assert.True(t, interlock >= 0 && interlock < verify && verify < quiescent && quiescent < remove,
		"a host that booted since the fence is checked like a rebooted one before the interlock is lifted: %q", log)
}

func TestRejoinStopsWhenAHostThatBootedSinceTheFenceIsNotQuiescent(t *testing.T) {
	rt := newRejoinTest(killallFence(), newBootID)
	rt.host.quiescentErr = errors.New("a container shim is still running")
	require.Error(t, rt.run(rt.ft.record()))
	assert.Equal(t, -1, index(rt.ft.log, "remove-interlock"), "the interlock stays: %q", rt.ft.log)
	assert.Equal(t, -1, index(rt.ft.log, "start-agent"), "%q", rt.ft.log)
}

func TestRejoinWithAnUnknownBootIDRebootsOnceAndResumesAfterIt(t *testing.T) {
	fence := killallFence()
	fence.BootID = installer.BootIDUnknown
	rt := newRejoinTest(fence, newBootID)
	rt.host.rebootTo = "3c4d5e6f-7a8b-4c9d-8e0f-1a2b3c4d5e6f"
	require.NoError(t, rt.run(rt.ft.record()))
	assert.True(t, index(rt.ft.log, "reboot") >= 0, "an unknown fence-time boot needs a reboot kip saw")

	// A rejoin interrupted after that reboot finds the checkpoint behind it.
	fence = killallFence()
	fence.BootID = installer.BootIDUnknown
	fence.RebootCheckpoint = newBootID
	rt = newRejoinTest(fence, "3c4d5e6f-7a8b-4c9d-8e0f-1a2b3c4d5e6f")
	require.NoError(t, rt.run(rt.ft.record()))
	assert.Equal(t, -1, index(rt.ft.log, "reboot"))
}

func TestRejoinStopsWhenTheHostDoesNotComeBack(t *testing.T) {
	rt := newRejoinTest(killallFence(), fenceBootID)
	rt.rejoin.Reconnect = func() (rejoinHost, error) { return nil, errors.New("connection refused") }
	err := rt.run(rt.ft.record())
	require.Error(t, err)
	assert.Equal(t, -1, index(rt.ft.log, "remove-interlock"))
	assert.Equal(t, fenceBootID, rt.ft.record().Fence.RebootCheckpoint, "the boot before the reboot is the checkpoint the next run compares with")
	assert.Equal(t, installer.NodeStateFenced, rt.ft.record().State)
}

func TestRejoinKeepsTheTaintUntilTheNodeIsReadyFromThisBoot(t *testing.T) {
	rt := newRejoinTest(killallFence(), newBootID)
	rt.host.fakeFenceHost = &fakeFenceHost{log: &rt.ft.log, id: fenceWorkerID}
	rt.rejoin.Host = &stuckAgentHost{rt.host}
	err := rt.run(rt.ft.record())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "did not report Ready")
	assert.Equal(t, -1, index(rt.ft.log, "untaint"))
}

type stuckAgentHost struct{ *fakeRejoinHost }

func (h *stuckAgentHost) StartAgent() error {
	*h.log = append(*h.log, "start-agent")
	return nil
}

func TestRejoinResumeAfterTheReleaseCordonsBeforeTheChecks(t *testing.T) {
	rt := newRejoinTest(killallFence(), newBootID)
	rt.ft.cluster.node.OutOfService = false
	require.NoError(t, rt.run(rt.ft.record()))
	assert.Equal(t, -1, index(rt.ft.log, "killall"), "a released worker is not stopped again")
	assert.True(t, index(rt.ft.log, "cordon") < index(rt.ft.log, "checks"))
	assert.Equal(t, installer.NodeStateReady, rt.ft.record().State)
}

func TestRejoinStartsStoppedServicesOnlyWithCurrentDataOrConsent(t *testing.T) {
	rt := newRejoinTest(killallFence(), newBootID)
	rt.ft.answer = false
	rt.ft.cluster.view = installer.FenceView{
		Services: []installer.FenceService{
			{Namespace: "shop", StatefulSet: "db", Volumes: []string{"pvc-1"}},
			{Namespace: "shop", StatefulSet: "cache", Volumes: []string{"pvc-2"}},
		},
		Replicas: []stranded.Replica{
			{Name: "r1", Volume: "pvc-1", Node: "worker-1", HealthyAt: healthy},
			{Name: "r2", Volume: "pvc-2", Node: "server-1", HealthyAt: healthy, LastFailedAt: "2026-10-24T08:30:00Z"},
		},
	}
	mark := installer.FenceStopMark{Node: "worker-1", Replicas: 1, Reason: installer.FenceStopStale}
	rt.ft.cluster.stops["shop/db"] = mark
	rt.ft.cluster.stops["shop/cache"] = mark
	require.NoError(t, rt.run(rt.ft.record()))
	assert.True(t, index(rt.ft.log, "start:shop/db") > index(rt.ft.log, "checks"), "a service whose copy is current anywhere starts")
	assert.Equal(t, -1, index(rt.ft.log, "start:shop/cache"), "a stale service waits for consent")
	assert.Len(t, rt.ft.asked, 1)
	assert.Contains(t, rt.ft.out.String(), "kip service start cache")

	rt = newRejoinTest(killallFence(), newBootID)
	rt.rejoin.AcceptStale = true
	rt.ft.cluster.view = installer.FenceView{
		Services: []installer.FenceService{{Namespace: "shop", StatefulSet: "cache", Volumes: []string{"pvc-2"}}},
		Replicas: []stranded.Replica{{Name: "r2", Volume: "pvc-2", Node: "server-1", HealthyAt: healthy, LastFailedAt: "2026-10-24T08:30:00Z"}},
	}
	rt.ft.cluster.stops["shop/cache"] = mark
	require.NoError(t, rt.run(rt.ft.record()))
	assert.True(t, index(rt.ft.log, "start:shop/cache") >= 0)
}

func TestFenceBlocksJoin(t *testing.T) {
	fencing := worker(installer.NodeStateFencing)
	fencing.Fence = killallFence()
	err := fenceBlocksJoin(fencing)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kip node fence worker-1")

	removing := worker(installer.NodeStateRemoving)
	removing.Fence = killallFence()
	err = fenceBlocksJoin(removing)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kip node remove worker-1 --force")

	assert.NoError(t, fenceBlocksJoin(worker(installer.NodeStateRemoving)))
	assert.NoError(t, fenceBlocksJoin(worker(installer.NodeStateReady)))
}

func TestFenceBlocksRebootAndPlainRemoval(t *testing.T) {
	fenced := worker(installer.NodeStateFenced)
	fenced.Fence = killallFence()
	fencing := worker(installer.NodeStateFencing)
	fencing.Fence = killallFence()
	assert.Error(t, fenceBlocksReboot(fenced))
	assert.Error(t, fenceBlocksReboot(fencing))
	assert.NoError(t, fenceBlocksReboot(worker(installer.NodeStateReady)))

	assert.Error(t, fenceAllowsRemoval(fenced, false), "a plain removal would try to drain a dead worker")
	assert.NoError(t, fenceAllowsRemoval(fenced, true))
	assert.Error(t, fenceAllowsRemoval(fencing, true), "an unfinished fence is finished first")
	removing := worker(installer.NodeStateRemoving)
	removing.Fence = killallFence()
	assert.Error(t, fenceAllowsRemoval(removing, false))
	assert.NoError(t, fenceAllowsRemoval(removing, true), "a forced removal of a fenced worker resumes with --force")
	assert.NoError(t, fenceAllowsRemoval(worker(installer.NodeStateReady), false))
}

type removalProbeHost struct {
	*fakeRejoinHost
	held, quiet error
}

func (h *removalProbeHost) VerifyInterlock() error { return h.held }
func (h *removalProbeHost) Quiescent() error       { return h.quiet }

func TestCheckFencedRemoval(t *testing.T) {
	rec := fencedWorker()
	var out bytes.Buffer
	host := func(held, quiet error) func() (rejoinHost, func(), error) {
		return func() (rejoinHost, func(), error) {
			return &removalProbeHost{fakeRejoinHost: &fakeRejoinHost{fakeFenceHost: &fakeFenceHost{log: &[]string{}, id: fenceWorkerID}}, held: held, quiet: quiet}, func() {}, nil
		}
	}
	gone := func() (rejoinHost, func(), error) { return nil, nil, errors.New("timed out") }

	assert.NoError(t, checkFencedRemoval(&out, rec, installer.FenceView{}, nil, gone))
	assert.NoError(t, checkFencedRemoval(&out, rec, installer.FenceView{}, nil, host(nil, nil)))
	err := checkFencedRemoval(&out, rec, installer.FenceView{}, nil, host(errors.New("the assert is gone"), nil))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kip node fence worker-1")
	assert.Error(t, checkFencedRemoval(&out, rec, installer.FenceView{}, nil, host(nil, errors.New("container shim 4242"))),
		"an inactive agent with a shim left is not held")

	stale := installer.FenceView{
		Services: []installer.FenceService{{Namespace: "shop", StatefulSet: "db", Volumes: []string{"pvc-1"}, Stop: &installer.FenceStopMark{Node: "worker-1"}}},
		Replicas: []stranded.Replica{{Name: "r1", Volume: "pvc-1", Node: "server-1", HealthyAt: healthy, LastFailedAt: "2026-10-24T08:30:00Z"}},
	}
	err = checkFencedRemoval(&out, rec, stale, []installer.NodeRecord{{Name: "server-1", Role: installer.NodeRoleServer, State: installer.NodeStateReady}, rec}, gone)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kip service start")

	only := installer.FenceView{Services: []installer.FenceService{{Namespace: "shop", StatefulSet: "db", Volumes: []string{"pvc-1"}, Stop: &installer.FenceStopMark{Node: "worker-1"}}}}
	out.Reset()
	require.NoError(t, checkFencedRemoval(&out, rec, only, nil, gone))
	assert.Contains(t, out.String(), "kip service delete db --delete-data")
	assert.Contains(t, out.String(), "kip backup restore <backup> --namespace shop")
}

func TestLeftoverInterlockIsClearedForEveryMembershipWithoutAFence(t *testing.T) {
	assert.True(t, clearsLeftoverInterlock(nil), "a fresh add")
	joining := worker(installer.NodeStateJoining)
	assert.True(t, clearsLeftoverInterlock(&joining), "a retry after the joining record was saved")
	ready := worker(installer.NodeStateReady)
	assert.True(t, clearsLeftoverInterlock(&ready))
	fenced := fencedWorker()
	assert.False(t, clearsLeftoverInterlock(&fenced))
}

func TestRejoinIgnoresCopiesOnAnotherFencedWorker(t *testing.T) {
	rt := newRejoinTest(killallFence(), newBootID)
	rt.rejoin.AcceptStale = true
	rt.ft.cluster.records = append(rt.ft.cluster.records, otherFencedWorker())
	rt.ft.cluster.view = installer.FenceView{
		Services: []installer.FenceService{{Namespace: "shop", StatefulSet: "db", Volumes: []string{"pvc-1"}}},
		Replicas: []stranded.Replica{{Name: "r1", Volume: "pvc-1", Node: "worker-2", HealthyAt: healthy}},
	}
	rt.ft.cluster.stops["shop/db"] = installer.FenceStopMark{Node: "worker-1", Replicas: 1, Reason: installer.FenceStopOnlyCopy}
	require.NoError(t, rt.run(rt.ft.record()))
	assert.False(t, logged(rt.ft.log, "start:"), "%q", rt.ft.log)
}

func TestCheckFencedRemovalRefusesAHostWhoseIdentityCannotBeRead(t *testing.T) {
	rec := fencedWorker()
	unreadable := func() (rejoinHost, func(), error) {
		h := &fakeFenceHost{log: &[]string{}, id: "", idErr: errors.New("session closed")}
		return &removalProbeHost{fakeRejoinHost: &fakeRejoinHost{fakeFenceHost: h}, held: errors.New("the assert is gone")}, func() {}, nil
	}
	err := checkFencedRemoval(&bytes.Buffer{}, rec, installer.FenceView{}, nil, unreadable)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "session closed")
}

func TestCheckFencedRemovalIgnoresCopiesOnAnotherFencedWorker(t *testing.T) {
	gone := func() (rejoinHost, func(), error) { return nil, nil, errors.New("timed out") }
	view := installer.FenceView{
		Services: []installer.FenceService{{Namespace: "shop", StatefulSet: "db", Volumes: []string{"pvc-1"}, Stop: &installer.FenceStopMark{Node: "worker-1"}}},
		Replicas: []stranded.Replica{{Name: "r1", Volume: "pvc-1", Node: "worker-2", HealthyAt: healthy}},
	}
	var out bytes.Buffer
	require.NoError(t, checkFencedRemoval(&out, fencedWorker(), view, []installer.NodeRecord{fencedWorker(), otherFencedWorker()}, gone))
	assert.Contains(t, out.String(), "the only copy of its data is on worker-1")
}

func TestAddRefusesAnotherMachineAtAFencedWorkersAddress(t *testing.T) {
	fenced := fencedWorker()
	records := []installer.NodeRecord{fenced}
	err := replacedFencedHost(records, fenced.Host, "dddddddddddddddddddddddddddddddd")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kip node remove worker-1 --force")

	assert.NoError(t, replacedFencedHost(records, fenced.Host, fenced.MachineID), "the fenced machine itself rejoins")
	assert.NoError(t, replacedFencedHost(records, "203.0.113.99", "dddddddddddddddddddddddddddddddd"))
	ready := worker(installer.NodeStateReady)
	assert.NoError(t, replacedFencedHost([]installer.NodeRecord{ready}, ready.Host, "dddddddddddddddddddddddddddddddd"))
}

func TestRejoinAtANewAddressIsRecordedInPrivateModeOnly(t *testing.T) {
	private := worker(installer.NodeStateFenced)
	private.Mode = installer.NetworkModePrivate

	moved, err := rejoinMoves(private, "203.0.113.11", "203.0.113.11")
	require.NoError(t, err)
	assert.False(t, moved)
	moved, err = rejoinMoves(private, "203.0.113.12", "203.0.113.11")
	require.NoError(t, err)
	assert.True(t, moved, "the other nodes reach a private-mode worker at its unchanged private address")

	retried := private
	retried.Host = "203.0.113.12"
	moved, err = rejoinMoves(retried, "203.0.113.12", "203.0.113.11")
	require.NoError(t, err)
	assert.True(t, moved, "a retry after the new Host was saved still publishes the new address")
	moved, err = rejoinMoves(private, "203.0.113.12", "")
	require.NoError(t, err)
	assert.False(t, moved, "without a published address there is nothing to move")

	public := private
	public.Mode = installer.NetworkModePublic
	public.PeerAddress = "203.0.113.11"
	_, err = rejoinMoves(public, "203.0.113.12", "203.0.113.11")
	require.Error(t, err, "in public mode the other nodes reach the worker at its published address")
	assert.Contains(t, err.Error(), "203.0.113.11")
	assert.Contains(t, err.Error(), "kip node remove worker-1 --force")
	_, err = rejoinMoves(public, "203.0.113.12", "")
	require.Error(t, err, "the recorded peer address stands in for an unknown published one")
	moved, err = rejoinMoves(public, "203.0.113.11", "203.0.113.11")
	require.NoError(t, err)
	assert.False(t, moved)
}

func TestRecordedWorkerRejoinsOverItsRecordedPrivatePath(t *testing.T) {
	server := installer.NodeRecord{Role: installer.NodeRoleServer, Mode: installer.NetworkModePrivate, PeerAddress: "172.31.5.10"}
	recorded := worker(installer.NodeStateReady)
	recorded.Mode, recorded.PeerAddress = installer.NetworkModePrivate, "172.31.40.20"

	s, j := recordedPeers(server, &recorded)
	assert.Equal(t, [2]string{"172.31.5.10", "172.31.40.20"}, [2]string{s, j})
	s, j = recordedPeers(server, nil)
	assert.Equal(t, [2]string{"", ""}, [2]string{s, j}, "a new host selects its network as before")
	public := server
	public.Mode = installer.NetworkModePublic
	s, j = recordedPeers(public, &recorded)
	assert.Equal(t, [2]string{"", ""}, [2]string{s, j})
}
