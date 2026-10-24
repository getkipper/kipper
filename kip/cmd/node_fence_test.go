package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/controller/pkg/stranded"
	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/installer"
)

const (
	fenceServerID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	fenceWorkerID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	fenceBootID   = "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0"
	nodeBootID    = "1a2b3c4d-5e6f-4a1b-8c2d-3e4f5a6b7c8d"
	healthy       = "2026-10-24T08:00:00Z"
)

type fakeFenceCluster struct {
	log        *[]string
	records    []installer.NodeRecord
	node       installer.FenceNode
	kubelet    bool
	kubeletErr error
	view       installer.FenceView
	policy     string
	pods       []installer.FencedPod
	vas        []installer.Attachment
	running    map[string]string
	onDelVA    func()
	stops      map[string]installer.FenceStopMark
	owners     map[string]bool
	// Fail before the matching write to simulate interruption after failSkip matches.
	failOn   string
	failSkip int
}

func (c *fakeFenceCluster) add(e string) { *c.log = append(*c.log, e) }

func (c *fakeFenceCluster) interrupted(e string) error {
	if c.failOn == "" || c.failOn != e {
		return nil
	}
	if c.failSkip > 0 {
		c.failSkip--
		return nil
	}
	c.failOn = ""
	return errors.New("interrupted at " + e)
}

func (c *fakeFenceCluster) Records() ([]installer.NodeRecord, error) { return c.records, nil }
func (c *fakeFenceCluster) SaveRecord(rec installer.NodeRecord) error {
	if err := installer.SaveNodeRecord(discardRunner{}, rec); err != nil {
		return err
	}
	// The fence keeps changing its copy, so the fake keeps what was saved.
	if err := c.interrupted("save:" + rec.State); err != nil {
		return err
	}
	b, _ := json.Marshal(rec)
	rec = installer.NodeRecord{}
	_ = json.Unmarshal(b, &rec)
	for i := range c.records {
		if c.records[i].MachineID == rec.MachineID {
			c.records[i] = rec
		}
	}
	c.add("save:" + rec.State)
	return nil
}
func (c *fakeFenceCluster) Node(string) (installer.FenceNode, error) { return c.node, nil }
func (c *fakeFenceCluster) KubeletAnswers(string) (bool, error)      { return c.kubelet, c.kubeletErr }
func (c *fakeFenceCluster) View(string) (installer.FenceView, error) {
	v := c.view
	v.Services = nil
	for _, s := range c.view.Services {
		if m, ok := c.stops[s.Namespace+"/"+s.StatefulSet]; ok {
			s.Stop = &m
		}
		v.Services = append(v.Services, s)
	}
	return v, nil
}
func (c *fakeFenceCluster) Stop(ns, name string, mark installer.FenceStopMark) error {
	if err := c.interrupted("stop:" + ns + "/" + name); err != nil {
		return err
	}
	c.add("stop:" + ns + "/" + name)
	mark.Replicas = max(mark.Replicas, 1)
	c.stops[ns+"/"+name] = mark
	return nil
}
func (c *fakeFenceCluster) Start(ns, name string) error {
	if err := c.interrupted("start:" + ns + "/" + name); err != nil {
		return err
	}
	c.add("start:" + ns + "/" + name)
	delete(c.stops, ns+"/"+name)
	return nil
}
func (c *fakeFenceCluster) FailTransfer(ns, name, _ string) error {
	c.add("fail-transfer:" + ns + "/" + name)
	return nil
}
func (c *fakeFenceCluster) SuspendJob(ns, name string) error {
	c.add("suspend-job:" + ns + "/" + name)
	return nil
}
func (c *fakeFenceCluster) OwnerExists(ns, _, kind, name string) (bool, error) {
	return c.owners[ns+"/"+kind+"/"+name], nil
}
func (c *fakeFenceCluster) Taint(string) error {
	if err := c.interrupted("taint"); err != nil {
		return err
	}
	c.add("taint")
	c.node.OutOfService = true
	return nil
}
func (c *fakeFenceCluster) DeletionPolicy() (string, error) { return c.policy, nil }
func (c *fakeFenceCluster) PodsWithVolumes(string) ([]installer.FencedPod, error) {
	return c.pods, nil
}
func (c *fakeFenceCluster) Attachments(string) ([]installer.Attachment, error) { return c.vas, nil }
func (c *fakeFenceCluster) DeleteAttachment(name string) error {
	c.add("delete-va:" + name)
	var kept []installer.Attachment
	for _, a := range c.vas {
		if a.Name != name {
			kept = append(kept, a)
		}
	}
	c.vas = kept
	if c.onDelVA != nil {
		c.onDelVA()
	}
	return nil
}
func (c *fakeFenceCluster) DeletePod(ns, name, uid string) error {
	c.add("delete-pod:" + ns + "/" + name + "@" + uid)
	return nil
}
func (c *fakeFenceCluster) PodPlacement(ns, name string) (string, bool, error) {
	n := c.running[ns+"/"+name]
	return n, n != "", nil
}

type discardRunner struct{}

func (discardRunner) Run(string) (string, error) { return "", nil }
func (discardRunner) RunStdin(string, io.Reader) (string, error) {
	return "", nil
}

type fakeFenceHost struct {
	log       *[]string
	id        string
	idErr     error
	stopErr   error
	afterStop func()
}

func (h *fakeFenceHost) MachineID() (string, error) { return h.id, h.idErr }
func (h *fakeFenceHost) BootID() (string, error)    { return fenceBootID, nil }
func (h *fakeFenceHost) InstallInterlock() error {
	*h.log = append(*h.log, "interlock")
	return nil
}
func (h *fakeFenceHost) StopAgent() error {
	*h.log = append(*h.log, "killall")
	if h.afterStop != nil {
		h.afterStop()
	}
	return h.stopErr
}

type fenceTest struct {
	log     []string
	out     bytes.Buffer
	cluster *fakeFenceCluster
	host    *fakeFenceHost
	asked   []string
	answer  bool
	fence   *nodeFence
}

func worker(state string) installer.NodeRecord {
	return installer.NodeRecord{Name: "worker-1", Host: "203.0.113.11", Role: installer.NodeRoleWorker, State: state, MachineID: fenceWorkerID, PeerAddress: "10.0.0.11"}
}

func newFenceTest(rec installer.NodeRecord) *fenceTest {
	t := &fenceTest{answer: true}
	server := installer.NodeRecord{Name: "server-1", Host: "203.0.113.10", Role: installer.NodeRoleServer, State: installer.NodeStateReady, MachineID: fenceServerID}
	t.cluster = &fakeFenceCluster{
		log:     &t.log,
		records: []installer.NodeRecord{server, rec},
		node:    installer.FenceNode{Exists: true, NotReadyFor: 5 * time.Minute, BootID: nodeBootID},
		policy:  "do-nothing",
		running: map[string]string{},
		stops:   map[string]installer.FenceStopMark{},
	}
	t.host = &fakeFenceHost{log: &t.log, id: fenceWorkerID}
	now := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	t.fence = &nodeFence{
		Out:     &t.out,
		Cluster: t.cluster,
		Dial: func(installer.NodeRecord) (fenceHost, func(), error) {
			if t.host == nil {
				return nil, nil, errors.New("connection timed out")
			}
			return t.host, func() {}, nil
		},
		Confirm: func(p string) (bool, error) {
			t.asked = append(t.asked, p)
			return t.answer, nil
		},
		Interactive:  true,
		Operator:     "operator",
		Now:          func() time.Time { return now },
		Sleep:        func(d time.Duration) { now = now.Add(d) },
		Poll:         5 * time.Second,
		MinNotReady:  time.Minute,
		NotReadyWait: 3 * time.Minute,
		PodGCGrace:   time.Minute,
		AttachWait:   2 * time.Minute,
		Timeout:      10 * time.Minute,
	}
	return t
}

func (t *fenceTest) record() installer.NodeRecord { return t.cluster.records[1] }

func (t *fenceTest) run(node string) error {
	f := t.fence
	return f.run(node)
}

// identify makes the worker's host answer with another machine ID.
func (t *fenceTest) identify(id string) {
	h := t.host
	h.id = id
}

func index(log []string, event string) int {
	for i, e := range log {
		if e == event {
			return i
		}
	}
	return -1
}

// logged reports whether any event in log starts with prefix.
func logged(log []string, prefix string) bool {
	for _, e := range log {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}

func dbService() installer.FenceService {
	return installer.FenceService{Namespace: "shop", StatefulSet: "db", Pods: []string{"db-0"}, Volumes: []string{"pvc-1"}}
}

func TestFenceOverSSHInterlocksBeforeItStopsAndTaintsLast(t *testing.T) {
	ft := newFenceTest(worker(installer.NodeStateReady))
	ft.cluster.view = installer.FenceView{Services: []installer.FenceService{dbService()},
		Replicas: []stranded.Replica{{Name: "r1", Volume: "pvc-1", Node: "server-1", HealthyAt: healthy}}}
	ft.cluster.running["shop/db-0"] = "server-1"

	require.NoError(t, ft.run("worker-1"))
	fencing, interlock, killall, taint := index(ft.log, "save:fencing"), index(ft.log, "interlock"), index(ft.log, "killall"), index(ft.log, "taint")
	assert.True(t, fencing >= 0 && fencing < interlock && interlock < killall && killall < taint, "the record, then the interlock, then killall, then the taint: %q", ft.log)
	assert.Equal(t, "save:fenced", ft.log[len(ft.log)-1])
	assert.Equal(t, -1, index(ft.log, "stop:shop/db"), "a service with a current copy elsewhere is not stopped")
	rec := ft.record()
	assert.Equal(t, installer.NodeStateFenced, rec.State)
	assert.Equal(t, installer.FenceMethodKillall, rec.Fence.Method)
	assert.Equal(t, fenceBootID, rec.Fence.BootID)
	assert.Equal(t, "operator", rec.Fence.Operator)
	assert.NotEmpty(t, rec.Fence.VerifiedAt)
	assert.NotEmpty(t, rec.Fence.FencedAt)
	assert.Len(t, ft.asked, 1)
	assert.Contains(t, ft.out.String(), "db (shop) runs again on server-1")
}

func TestFenceRefusesTooEarly(t *testing.T) {
	ft := newFenceTest(worker(installer.NodeStateReady))
	ft.cluster.node.NotReadyFor = 20 * time.Second
	err := ft.run("worker-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least 1m0s")
	assert.Empty(t, ft.log)

	ft = newFenceTest(worker(installer.NodeStateReady))
	ft.cluster.node.Ready = true
	require.Error(t, ft.run("worker-1"))
	assert.Empty(t, ft.log)
}

func TestFenceRefusesRecordsItMustNotTouch(t *testing.T) {
	for _, state := range []string{installer.NodeStateJoining, installer.NodeStateRemoving, installer.NodeStateWiping} {
		t.Run(state, func(t *testing.T) {
			ft := newFenceTest(worker(state))
			require.Error(t, ft.run("worker-1"))
			assert.Empty(t, ft.log)
		})
	}
	ft := newFenceTest(worker(installer.NodeStateReady))
	err := ft.run("server-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "only workers")

	ft = newFenceTest(worker(installer.NodeStateReady))
	ft.cluster.node.Exists = false
	err = ft.run("worker-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kip node remove worker-1 --force")
}

func fencedWorker() installer.NodeRecord {
	rec := worker(installer.NodeStateFenced)
	rec.Fence = &config.NodeFence{Method: installer.FenceMethodPoweredOff, Operator: "operator", StartedAt: healthy, FencedAt: healthy, BootID: installer.BootIDUnknown}
	return rec
}

func TestFenceRerunRefusesAWorkerARejoinAlreadyReleased(t *testing.T) {
	ft := newFenceTest(fencedWorker())
	ft.cluster.node.OutOfService = false
	err := ft.run("worker-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kip node add 203.0.113.11")
	assert.Empty(t, ft.log, "nothing on the host is stopped once a rejoin released it")
}

func TestFenceRerunOfAPoweredOffFenceStopsAHostThatCameBack(t *testing.T) {
	ft := newFenceTest(fencedWorker())
	ft.cluster.node.OutOfService = true
	require.NoError(t, ft.run("worker-1"))
	assert.True(t, index(ft.log, "interlock") >= 0 && index(ft.log, "interlock") < index(ft.log, "killall"))
	assert.Equal(t, -1, index(ft.log, "taint"))
	assert.Equal(t, installer.FenceMethodKillall, ft.record().Fence.Method)
	assert.NotEmpty(t, ft.record().Fence.VerifiedAt)
	assert.Empty(t, ft.asked, "the fence was confirmed when it was first run")
}

func TestFenceRefusesAnotherMachineAtTheAddress(t *testing.T) {
	ft := newFenceTest(worker(installer.NodeStateReady))
	ft.identify("cccccccccccccccccccccccccccccccc")
	require.Error(t, ft.run("worker-1"))
	assert.Empty(t, ft.log)
}

func TestFencePoweredOffRefusedWhileSSHAnswers(t *testing.T) {
	ft := newFenceTest(worker(installer.NodeStateReady))
	ft.fence.PoweredOff = true
	err := ft.run("worker-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "answers over SSH")
	assert.Empty(t, ft.log)
}

func TestFencePoweredOffRefusedWhileTheKubeletAnswers(t *testing.T) {
	ft := newFenceTest(worker(installer.NodeStateReady))
	ft.host = nil
	ft.cluster.kubelet = true
	ft.fence.PoweredOff = true
	err := ft.run("worker-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kubelet still answers")
	assert.Empty(t, ft.log)
}

func TestFencePoweredOffNeedsTheAssertion(t *testing.T) {
	ft := newFenceTest(worker(installer.NodeStateReady))
	ft.host = nil
	ft.fence.Interactive = false
	err := ft.run("worker-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--powered-off")
	assert.Empty(t, ft.log)

	ft = newFenceTest(worker(installer.NodeStateReady))
	ft.host = nil
	ft.answer = false
	require.Error(t, ft.run("worker-1"))
	assert.Empty(t, ft.log)
	require.Len(t, ft.asked, 1)
	assert.Contains(t, ft.asked[0], "Is worker-1 powered off, and will it stay off until you run kip node add?")
}

func TestFencePoweredOffRecordsTheNodesBootID(t *testing.T) {
	ft := newFenceTest(worker(installer.NodeStateReady))
	ft.host = nil
	ft.fence.PoweredOff = true
	require.NoError(t, ft.run("worker-1"))
	rec := ft.record()
	assert.Equal(t, installer.FenceMethodPoweredOff, rec.Fence.Method)
	assert.Equal(t, nodeBootID, rec.Fence.BootID)
	assert.Empty(t, rec.Fence.VerifiedAt)
	assert.Equal(t, -1, index(ft.log, "interlock"))
	assert.Empty(t, ft.asked)
}

func TestFenceResumeKeepsStartAndBootID(t *testing.T) {
	rec := worker(installer.NodeStateFencing)
	rec.Fence = &config.NodeFence{Method: installer.FenceMethodPoweredOff, Operator: "earlier", StartedAt: healthy, BootID: installer.BootIDUnknown}
	ft := newFenceTest(rec)
	ft.cluster.node.NotReadyFor = 0
	ft.fence.Yes = true
	require.NoError(t, ft.run("worker-1"))
	got := ft.record()
	assert.Equal(t, healthy, got.Fence.StartedAt)
	assert.Equal(t, installer.BootIDUnknown, got.Fence.BootID)
	assert.Equal(t, "earlier", got.Fence.Operator)
	assert.Equal(t, installer.FenceMethodKillall, got.Fence.Method)
	assert.Empty(t, ft.asked, "--yes answers the killall question")
}

func TestFenceStopsWhenKillallLeavesSomethingRunning(t *testing.T) {
	ft := newFenceTest(worker(installer.NodeStateReady))
	ft.host.stopErr = errors.New("the host is still running parts of the node: container shim 4242")
	require.Error(t, ft.run("worker-1"))
	assert.Equal(t, -1, index(ft.log, "taint"))
	assert.Equal(t, installer.NodeStateFencing, ft.record().State)
}

func TestFenceWaitsForNotReadyAfterKillall(t *testing.T) {
	ft := newFenceTest(worker(installer.NodeStateFencing))
	ft.cluster.records[1].Fence = &config.NodeFence{Method: installer.FenceMethodKillall, StartedAt: healthy, BootID: fenceBootID}
	ft.cluster.node.Ready = true
	ft.fence.Yes = true
	err := ft.run("worker-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "still reports Ready")
	assert.Equal(t, -1, index(ft.log, "taint"))
}

func TestFenceStopsWhatStartsAnUnownedPodBeforeTheTaint(t *testing.T) {
	ft := newFenceTest(worker(installer.NodeStateReady))
	ft.fence.Yes = true
	ft.cluster.view = installer.FenceView{Holders: []installer.PodHolder{
		{Namespace: "shop", Pod: "dt-move1-export", Kind: installer.HolderDataTransfer, Name: "move1"},
		{Namespace: "shop", Pod: "report-abc", Kind: installer.HolderJob, Name: "report"},
		{Namespace: "shop", Pod: "import-0"},
	}}
	require.NoError(t, ft.run("worker-1"))
	taint := index(ft.log, "taint")
	assert.True(t, index(ft.log, "fail-transfer:shop/move1") >= 0 && index(ft.log, "fail-transfer:shop/move1") < taint, "%q", ft.log)
	assert.True(t, index(ft.log, "suspend-job:shop/report") >= 0 && index(ft.log, "suspend-job:shop/report") < taint, "%q", ft.log)
	assert.Contains(t, ft.out.String(), "shop/import-0")
	assert.NotContains(t, ft.out.String(), "Delete it")
}

func TestFenceRefusesAPodWhoseOtherOwnerStillExists(t *testing.T) {
	ft := newFenceTest(worker(installer.NodeStateReady))
	ft.fence.Yes = true
	ft.cluster.view = installer.FenceView{Holders: []installer.PodHolder{{Namespace: "shop", Pod: "web-7f-x", Kind: "ReplicaSet", Name: "web-7f", APIVersion: "apps/v1"}}}
	ft.cluster.owners = map[string]bool{"shop/ReplicaSet/web-7f": true}
	err := ft.run("worker-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ReplicaSet web-7f")
	assert.Contains(t, err.Error(), "shop/web-7f-x")
	assert.NotContains(t, err.Error(), "Delete it")
	assert.Equal(t, -1, index(ft.log, "taint"))

	ft.cluster.owners = nil
	require.NoError(t, ft.run("worker-1"), "once the owner is gone the pod is left to the taint")
	assert.True(t, index(ft.log, "taint") >= 0)
}

func staleView() installer.FenceView {
	return installer.FenceView{Services: []installer.FenceService{dbService()},
		Replicas: []stranded.Replica{{Name: "r1", Volume: "pvc-1", Node: "server-1", HealthyAt: healthy, LastFailedAt: "2026-10-24T08:30:00Z"}}}
}

func TestFenceStopsAStaleServiceBeforeTheTaintAndKeepsItStoppedWithoutConsent(t *testing.T) {
	ft := newFenceTest(worker(installer.NodeStateReady))
	ft.fence.Yes = true
	ft.fence.Interactive = false
	ft.cluster.view = staleView()
	require.NoError(t, ft.run("worker-1"))
	assert.True(t, index(ft.log, "stop:shop/db") >= 0 && index(ft.log, "stop:shop/db") < index(ft.log, "taint"), "%q", ft.log)
	assert.Equal(t, -1, index(ft.log, "start:shop/db"))
	rec := ft.record()
	require.Len(t, rec.Fence.Stopped, 1)
	assert.Equal(t, installer.FenceStopStale, rec.Fence.Stopped[0].Reason)
	assert.Empty(t, rec.Fence.Stopped[0].Consent)
	assert.Contains(t, ft.out.String(), "kip node fence worker-1 --accept-stale-data")
}

func TestFenceStartsAStaleServiceOnlyAfterConsentIsRecorded(t *testing.T) {
	ft := newFenceTest(worker(installer.NodeStateReady))
	ft.fence.Yes = true
	ft.fence.AcceptStale = true
	ft.cluster.view = staleView()
	ft.cluster.running["shop/db-0"] = "server-1"
	require.NoError(t, ft.run("worker-1"))
	start := index(ft.log, "start:shop/db")
	require.True(t, start > index(ft.log, "taint"), "%q", ft.log)
	assert.Empty(t, ft.record().Fence.Stopped, "the entry leaves the record once the service started")
	assert.Contains(t, ft.out.String(), "db (shop) runs again on server-1, on older copies of its data")
	assert.NotContains(t, ft.out.String(), "data current up to")
}

func TestFenceReusesConsentOnlyWhileTheCopiesAreUnchanged(t *testing.T) {
	stamps := []config.ReplicaStamp{{Volume: "pvc-1", Name: "r1", Node: "server-1", HealthyAt: healthy, LastFailedAt: "2026-10-24T08:30:00Z"}}
	rec := fencedWorker()
	rec.Fence.Stopped = []config.FenceStop{{Namespace: "shop", StatefulSet: "db", Reason: installer.FenceStopStale, Consent: stamps}}
	ft := newFenceTest(rec)
	ft.host = nil
	ft.cluster.node.OutOfService = true
	ft.cluster.view = staleView()
	ft.cluster.view.Services[0].Pods = nil
	ft.cluster.stops["shop/db"] = installer.FenceStopMark{Node: "worker-1", Replicas: 1, Reason: installer.FenceStopStale, Volumes: []string{"pvc-1"}}
	ft.cluster.running["shop/db-0"] = "server-1"
	require.NoError(t, ft.run("worker-1"))
	assert.Empty(t, ft.asked)
	assert.True(t, index(ft.log, "start:shop/db") >= 0)

	rec = fencedWorker()
	changed := append([]config.ReplicaStamp(nil), stamps...)
	changed[0].HealthyAt = "2026-10-24T07:00:00Z"
	rec.Fence.Stopped = []config.FenceStop{{Namespace: "shop", StatefulSet: "db", Reason: installer.FenceStopStale, Consent: changed}}
	ft = newFenceTest(rec)
	ft.host = nil
	ft.answer = false
	ft.cluster.node.OutOfService = true
	ft.cluster.view = staleView()
	ft.cluster.stops["shop/db"] = installer.FenceStopMark{Node: "worker-1", Replicas: 1, Reason: installer.FenceStopStale, Volumes: []string{"pvc-1"}}
	require.NoError(t, ft.run("worker-1"))
	require.Len(t, ft.asked, 1, "a changed copy voids the earlier consent")
	assert.Equal(t, -1, index(ft.log, "start:shop/db"))
	assert.Equal(t, -1, index(ft.log, "stop:shop/db"), "a service already stopped is not patched again")
}

func TestFenceNeverStartsAServiceWhoseOnlyCopyIsOnTheWorker(t *testing.T) {
	ft := newFenceTest(worker(installer.NodeStateReady))
	ft.fence.Yes = true
	ft.fence.AcceptStale = true
	ft.cluster.view = installer.FenceView{Services: []installer.FenceService{dbService()}}
	require.NoError(t, ft.run("worker-1"))
	assert.True(t, index(ft.log, "stop:shop/db") >= 0)
	assert.Equal(t, -1, index(ft.log, "start:shop/db"))
	assert.Equal(t, installer.FenceStopOnlyCopy, ft.record().Fence.Stopped[0].Reason)
	assert.Contains(t, ft.out.String(), "kip service delete db --delete-data")
	assert.Contains(t, ft.out.String(), "kip backup restore <backup> --namespace shop")
	assert.Contains(t, ft.out.String(), "from a backup taken before the fence, while db ran. A later backup holds none of its files")
}

func TestFenceNudgeDetachesBeforeItDeletesThePod(t *testing.T) {
	ft := newFenceTest(worker(installer.NodeStateReady))
	ft.fence.Yes = true
	ft.cluster.pods = []installer.FencedPod{{Namespace: "shop", Name: "db-0", UID: "uid-1", PersistentVolumes: []string{"pvc-1"}}}
	ft.cluster.vas = []installer.Attachment{{Name: "csi-a", PersistentVolume: "pvc-1"}, {Name: "csi-other", PersistentVolume: "pvc-9"}}
	require.NoError(t, ft.run("worker-1"))
	va, pod := index(ft.log, "delete-va:csi-a"), index(ft.log, "delete-pod:shop/db-0@uid-1")
	assert.True(t, index(ft.log, "taint") < va && va < pod, "%q", ft.log)
	assert.Equal(t, -1, index(ft.log, "delete-va:csi-other"), "only the pod's own attachments are deleted")
	assert.Contains(t, ft.out.String(), "has not deleted pod shop/db-0")
}

func TestFenceNudgeStopsWhenTheNodeComesBack(t *testing.T) {
	ft := newFenceTest(worker(installer.NodeStateReady))
	ft.fence.Yes = true
	ft.cluster.pods = []installer.FencedPod{{Namespace: "shop", Name: "db-0", UID: "uid-1", PersistentVolumes: []string{"pvc-1"}}}
	ft.cluster.vas = []installer.Attachment{{Name: "csi-a", PersistentVolume: "pvc-1"}}
	ft.cluster.onDelVA = func() { ft.cluster.node.Ready = true }
	err := ft.run("worker-1")
	require.Error(t, err)
	assert.False(t, logged(ft.log, "delete-pod:"), "the pod is not deleted once the node reports Ready")
}

func TestFenceNudgeStopsWhenAnAttachmentStays(t *testing.T) {
	ft := newFenceTest(worker(installer.NodeStateReady))
	ft.fence.Yes = true
	ft.cluster.pods = []installer.FencedPod{{Namespace: "shop", Name: "db-0", UID: "uid-1", PersistentVolumes: []string{"pvc-1"}}}
	ft.cluster.vas = []installer.Attachment{{Name: "csi-a", PersistentVolume: "pvc-1"}}
	ft.cluster.onDelVA = func() { ft.cluster.vas = []installer.Attachment{{Name: "csi-a", PersistentVolume: "pvc-1"}} }
	err := ft.run("worker-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "csi-a")
	assert.Contains(t, err.Error(), "do not delete the pod with kubectl --force")
	assert.False(t, logged(ft.log, "delete-pod:"))
}

func TestFenceWarnsAboutLonghornsDeletionPolicy(t *testing.T) {
	ft := newFenceTest(worker(installer.NodeStateReady))
	ft.fence.Yes = true
	ft.cluster.policy = "delete-both-statefulset-and-deployment-pod"
	require.NoError(t, ft.run("worker-1"))
	assert.Contains(t, ft.out.String(), "node-down-pod-deletion-policy")
}

func TestFenceReportsAServiceThatDidNotStart(t *testing.T) {
	ft := newFenceTest(worker(installer.NodeStateReady))
	ft.fence.Yes = true
	ft.cluster.view = installer.FenceView{Services: []installer.FenceService{dbService()},
		Replicas: []stranded.Replica{{Name: "r1", Volume: "pvc-1", Node: "server-1", HealthyAt: healthy}}}
	err := ft.run("worker-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), fmt.Sprintf("within %s", 10*time.Minute))
	assert.True(t, strings.Contains(ft.out.String(), "not running on another node yet"))
	assert.Equal(t, installer.NodeStateFenced, ft.record().State, "the fence holds even when a service is slow to start")
}

func TestFenceInterruptedAtEveryWriteResumesSafely(t *testing.T) {
	for _, consent := range []bool{false, true} {
		clean := newFenceTest(worker(installer.NodeStateReady))
		clean.fence.Yes, clean.fence.Interactive, clean.fence.AcceptStale = true, false, consent
		clean.cluster.view = staleView()
		clean.cluster.running["shop/db-0"] = "server-1"
		require.NoError(t, clean.run("worker-1"))
		seen := map[string]int{}
		for _, e := range clean.log {
			write := strings.HasPrefix(e, "save:") || strings.HasPrefix(e, "stop:") || strings.HasPrefix(e, "start:") || e == "taint"
			if !write {
				continue
			}
			skip := seen[e]
			seen[e]++
			t.Run(fmt.Sprintf("consent=%v/%s#%d", consent, e, skip+1), func(t *testing.T) {
				ft := newFenceTest(worker(installer.NodeStateReady))
				ft.fence.Yes, ft.fence.Interactive, ft.fence.AcceptStale = true, false, consent
				ft.cluster.view = staleView()
				ft.cluster.running["shop/db-0"] = "server-1"
				ft.cluster.failOn, ft.cluster.failSkip = e, skip
				require.Error(t, ft.run("worker-1"))
				require.NoError(t, ft.run("worker-1"), "%q", ft.log)

				stop, taint := index(ft.log, "stop:shop/db"), index(ft.log, "taint")
				assert.True(t, stop >= 0 && stop < taint, "the stale service is stopped before the first taint: %q", ft.log)
				rec := ft.record()
				assert.Equal(t, installer.NodeStateFenced, rec.State)
				if consent {
					assert.True(t, index(ft.log, "start:shop/db") > taint, "%q", ft.log)
					assert.Empty(t, rec.Fence.Stopped)
				} else {
					assert.False(t, logged(ft.log, "start:"), "no stale start without consent: %q", ft.log)
					require.Len(t, rec.Fence.Stopped, 1)
				}
			})
		}
		assert.NotEmpty(t, seen)
	}
}

// otherFencedWorker is a second worker whose own fence is not finished.
func otherFencedWorker() installer.NodeRecord {
	rec := worker(installer.NodeStateFenced)
	rec.Name, rec.Host, rec.MachineID, rec.PeerAddress = "worker-2", "203.0.113.12", "cccccccccccccccccccccccccccccccc", "10.0.0.12"
	rec.Fence = killallFence()
	return rec
}

func TestFenceIgnoresCopiesOnAnotherFencedWorker(t *testing.T) {
	ft := newFenceTest(worker(installer.NodeStateReady))
	ft.fence.Yes, ft.fence.AcceptStale = true, true
	ft.cluster.records = append(ft.cluster.records, otherFencedWorker())
	ft.cluster.view = installer.FenceView{Services: []installer.FenceService{dbService()},
		Replicas: []stranded.Replica{{Name: "r1", Volume: "pvc-1", Node: "worker-2", HealthyAt: healthy}}}
	require.NoError(t, ft.run("worker-1"))
	assert.True(t, index(ft.log, "stop:shop/db") >= 0 && index(ft.log, "stop:shop/db") < index(ft.log, "taint"), "%q", ft.log)
	assert.False(t, logged(ft.log, "start:"), "%q", ft.log)
	require.Len(t, ft.record().Fence.Stopped, 1)
	assert.Equal(t, installer.FenceStopOnlyCopy, ft.record().Fence.Stopped[0].Reason)
}

func TestFencePoweredOffRefusedWhenTheKubeletCannotBeProbed(t *testing.T) {
	ft := newFenceTest(worker(installer.NodeStateReady))
	ft.host = nil
	ft.fence.PoweredOff = true
	ft.cluster.kubeletErr = errors.New("worker-1.example.com does not resolve to an IPv4 address on the server")
	err := ft.run("worker-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot probe")
	assert.Contains(t, err.Error(), "does not resolve")
	assert.Empty(t, ft.log, "nothing is recorded or changed")
}

func TestFenceStopsAMigrationsPausedServiceBeforeItFailsTheTransfer(t *testing.T) {
	ft := newFenceTest(worker(installer.NodeStateReady))
	ft.fence.Yes = true
	ft.fence.Interactive = false
	ft.cluster.view = installer.FenceView{
		Services: []installer.FenceService{{Namespace: "shop", StatefulSet: "db", Volumes: []string{"pvc-1"}, Replicas: 2}},
		Holders:  []installer.PodHolder{{Namespace: "shop", Pod: "dt-move1-export", Kind: installer.HolderDataTransfer, Name: "move1"}},
		Replicas: staleView().Replicas,
	}
	require.NoError(t, ft.run("worker-1"))
	stop, fail := index(ft.log, "stop:shop/db"), index(ft.log, "fail-transfer:shop/move1")
	assert.True(t, stop >= 0 && stop < fail, "the stop mark is in place before the migration can restore the service: %q", ft.log)
	assert.Equal(t, int32(2), ft.cluster.stops["shop/db"].Replicas)
	assert.False(t, logged(ft.log, "start:"), "%q", ft.log)
}

func TestFenceReportsAServiceWithoutPodsWithoutWaiting(t *testing.T) {
	ft := newFenceTest(worker(installer.NodeStateReady))
	ft.fence.Yes = true
	ft.cluster.view = installer.FenceView{
		Services: []installer.FenceService{{Namespace: "shop", StatefulSet: "db", Volumes: []string{"pvc-1"}}},
		Replicas: []stranded.Replica{{Name: "r1", Volume: "pvc-1", Node: "server-1", HealthyAt: healthy}},
	}
	require.NoError(t, ft.run("worker-1"))
	assert.Contains(t, ft.out.String(), "db (shop) had no pod on worker-1, so there is none to move")
}

func TestFenceWaitsForAPausedServiceItStartedOnOlderData(t *testing.T) {
	ft := newFenceTest(worker(installer.NodeStateReady))
	ft.fence.Yes = true
	ft.fence.AcceptStale = true
	ft.cluster.view = installer.FenceView{
		Services: []installer.FenceService{{Namespace: "shop", StatefulSet: "db", Volumes: []string{"pvc-1"}, Replicas: 2}},
		Holders:  []installer.PodHolder{{Namespace: "shop", Pod: "dt-move1-export", Kind: installer.HolderDataTransfer, Name: "move1"}},
		Replicas: staleView().Replicas,
	}
	ft.cluster.running["shop/db-0"] = "server-1"
	err := ft.run("worker-1")
	require.Error(t, err, "db-1 never runs, so the fence waits for it and fails")
	assert.True(t, logged(ft.log, "start:shop/db"), "%q", ft.log)
	assert.NotContains(t, ft.out.String(), "none to move")

	ft.cluster.running["shop/db-1"] = "server-1"
	require.NoError(t, ft.run("worker-1"))
	assert.Contains(t, ft.out.String(), "db (shop) runs again on server-1, on older copies of its data")
}

func TestFenceWarnsAboutUnmanagedPodsBeforeTheTaint(t *testing.T) {
	ft := newFenceTest(worker(installer.NodeStateReady))
	ft.fence.Yes = true
	ft.cluster.view = installer.FenceView{Unmanaged: []string{"tools/backfill-x2", "tools/cache-0"}}
	require.NoError(t, ft.run("worker-1"), "an unmanaged pod never stops the fence")
	out := ft.out.String()
	assert.Contains(t, out, "tools/backfill-x2, tools/cache-0")
	assert.Contains(t, out, "kip does not check")
	assert.True(t, strings.Index(out, "tools/cache-0") < strings.Index(out, "is fenced: tainted out of service"), "the warning comes before the taint")
}
