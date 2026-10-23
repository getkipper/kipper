package installer

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	engine182 = "longhornio/longhorn-engine:v1.8.2"
	engine173 = "longhornio/longhorn-engine:v1.7.3"
	im182     = "longhornio/longhorn-instance-manager:v1.8.2"
)

// settledSnapshot is a two-node cluster settled at v1.8.2 with one attached
// two-replica volume and one detached volume.
func settledSnapshot() longhornSnapshot {
	s := longhornSnapshot{
		Workloads: map[string]lhWorkload{},
		Settings: map[string]string{
			"current-longhorn-version":       "v1.8.2",
			"default-engine-image":           engine182,
			"default-instance-manager-image": im182,
		},
		EngineImages:  []lhEngineImage{{Image: engine182, State: "deployed", CLIAPI: 10, Controller: 5}},
		LonghornNodes: []string{"server-1", "worker-1"},
		InstanceManagers: []lhInstanceManager{
			{Node: "server-1", Image: im182, DataEngine: "v1", State: "running"},
			{Node: "worker-1", Image: im182, DataEngine: "v1", State: "running"},
		},
		Volumes: []lhVolume{
			{Name: "pvc-db", Claim: "shop/data-db-0", Replicas: 2, Image: engine182, CurrentImage: engine182, DataEngine: "v1", State: "attached", Robustness: "healthy"},
			{Name: "pvc-idle", Replicas: 1, Image: engine182, CurrentImage: engine182, DataEngine: "v1", State: "detached", Robustness: "unknown"},
		},
		Replicas: []lhReplica{
			{Name: "pvc-db-r-1", Volume: "pvc-db", Node: "server-1", HealthyAt: "t", State: "running"},
			{Name: "pvc-db-r-2", Volume: "pvc-db", Node: "worker-1", HealthyAt: "t", State: "running"},
		},
		Engines: []lhEngine{
			{Volume: "pvc-db", State: "running", Modes: map[string]string{"pvc-db-r-1": "RW", "pvc-db-r-2": "RW"}},
			{Volume: "pvc-idle"},
		},
	}
	for _, name := range longhornSettledWorkloads {
		w := lhWorkload{Kind: strings.Split(name, "/")[0], Ready: true}
		if name == "DaemonSet/longhorn-manager" {
			w.Manager = "longhornio/longhorn-manager:v1.8.2"
		}
		s.Workloads[name] = w
	}
	return s
}

func TestSettledSnapshotHasNoProblems(t *testing.T) {
	assert.Empty(t, settledSnapshot().settledProblems("v1.8.2"))
}

func TestComponentsMustHaveMovedToTheVersion(t *testing.T) {
	s := settledSnapshot()
	assert.NotEmpty(t, s.componentProblems("v1.9.2"), "everything still runs v1.8.2")

	s.Settings["current-longhorn-version"] = "v1.7.3"
	assert.Contains(t, strings.Join(s.componentProblems("v1.8.2"), "\n"), "has not finished upgrading to v1.8.2")

	s = settledSnapshot()
	delete(s.Workloads, "Deployment/csi-provisioner")
	assert.Contains(t, strings.Join(s.componentProblems("v1.8.2"), "\n"), "csi-provisioner does not exist yet")

	s = settledSnapshot()
	s.Workloads["DaemonSet/longhorn-csi-plugin"] = lhWorkload{Kind: "DaemonSet"}
	assert.Contains(t, strings.Join(s.componentProblems("v1.8.2"), "\n"), "longhorn-csi-plugin has not finished rolling out")

	s = settledSnapshot()
	s.EngineImages[0].State = "deploying"
	assert.Contains(t, strings.Join(s.componentProblems("v1.8.2"), "\n"), "not deployed")

	s = settledSnapshot()
	s.InstanceManagers[1].State = "starting"
	assert.Contains(t, strings.Join(s.componentProblems("v1.8.2"), "\n"), "node worker-1 has no running instance manager")
}

// A node out of service, as a fence leaves it, has no instance manager by
// design. The problem says why and how to bring it back or let it go.
func TestComponentProblemsNameANodeOutOfService(t *testing.T) {
	nodes, err := ParseNodeFacts([]byte(`{"items":[{"metadata":{"name":"server-1"}},{"metadata":{"name":"worker-1"},"spec":{"taints":[{"key":"node.kubernetes.io/out-of-service","effect":"NoExecute"}]}}]}`))
	require.NoError(t, err)
	s := settledSnapshot()
	s.Nodes = nodes
	s.InstanceManagers[1].State = "stopped"

	problems := strings.Join(s.componentProblems("v1.8.2"), "\n")
	assert.Contains(t, problems, "node worker-1 is out of service")
	assert.Contains(t, problems, "kip node add")
	assert.Contains(t, problems, "kip node remove --force")
}

func TestVolumeProblems(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(*longhornSnapshot)
		want   string
	}{
		"faulted":            {func(s *longhornSnapshot) { s.Volumes[1].Robustness = "faulted" }, "pvc-idle is faulted"},
		"attaching":          {func(s *longhornSnapshot) { s.Volumes[1].State = "attaching" }, "pvc-idle is attaching"},
		"attached unknown":   {func(s *longhornSnapshot) { s.Volumes[0].Robustness = "unknown" }, "shop/data-db-0 is attached with unknown health"},
		"v2":                 {func(s *longhornSnapshot) { s.Volumes[1].DataEngine = "v2" }, "v2 data engine"},
		"engine moving":      {func(s *longhornSnapshot) { s.Volumes[0].CurrentImage = engine173 }, "is moving from engine"},
		"rebuilding":         {func(s *longhornSnapshot) { s.Engines[0].Rebuilding = true }, "a replica of shop/data-db-0 is rebuilding"},
		"write-only":         {func(s *longhornSnapshot) { s.Engines[0].Modes["pvc-db-r-2"] = "WO" }, "is WO, not RW"},
		"degraded, 2 nodes":  {func(s *longhornSnapshot) { s.Volumes[0].Robustness = "degraded" }, "shop/data-db-0 is degraded"},
		"no engine":          {func(s *longhornSnapshot) { s.Engines = s.Engines[1:] }, "Longhorn lists no engine for it"},
		"no replica modes":   {func(s *longhornSnapshot) { s.Engines[0].Modes = nil }, "none of its replicas is running and read-write"},
		"one mode missing":   {func(s *longhornSnapshot) { delete(s.Engines[0].Modes, "pvc-db-r-2") }, "only 1 of its 2 replicas"},
		"replica missing":    {func(s *longhornSnapshot) { s.Replicas = s.Replicas[:1] }, "only 1 of its 2 replicas"},
		"replica stopped":    {func(s *longhornSnapshot) { s.Replicas[1].State = "stopped" }, "only 1 of its 2 replicas"},
		"replica failed":     {func(s *longhornSnapshot) { s.Replicas[1].FailedAt = "t" }, "only 1 of its 2 replicas"},
		"engine not running": {func(s *longhornSnapshot) { s.Engines[0].State = "starting" }, "its engine is starting"},
		"two engines": {func(s *longhornSnapshot) {
			s.Engines = append(s.Engines, lhEngine{Volume: "pvc-db", State: "running", Modes: map[string]string{"pvc-db-r-1": "RW", "pvc-db-r-2": "RW"}})
		}, "has 2 engines"},
	} {
		s := settledSnapshot()
		tc.change(&s)
		assert.Contains(t, strings.Join(s.volumeProblems(), "\n"), tc.want, name)
	}
}

func TestDetachedUnknownIsNormal(t *testing.T) {
	assert.Empty(t, settledSnapshot().volumeProblems())
}

// byDesignSnapshot represents a stock-class shared volume requesting three
// replicas on two nodes, degraded with a usable copy on each node.
func byDesignSnapshot() longhornSnapshot {
	s := settledSnapshot()
	s.Volumes[0].Replicas = 3
	s.Volumes[0].Robustness = "degraded"
	return s
}

func TestDegradedByDesignIsNotAProblem(t *testing.T) {
	assert.Empty(t, byDesignSnapshot().volumeProblems())
}

func TestDegradedByDesignNeedsAUsableCopyOnEveryNode(t *testing.T) {
	for name, change := range map[string]func(*longhornSnapshot){
		"failed copy":         func(s *longhornSnapshot) { s.Replicas[1].FailedAt = "t" },
		"copy not running":    func(s *longhornSnapshot) { s.Replicas[1].State = "stopped" },
		"copy not read-write": func(s *longhornSnapshot) { s.Engines[0].Modes["pvc-db-r-2"] = "ERR" },
		"copy missing":        func(s *longhornSnapshot) { s.Replicas = s.Replicas[:1] },
		"never healthy":       func(s *longhornSnapshot) { s.Replicas[1].HealthyAt = "" },
	} {
		s := byDesignSnapshot()
		change(&s)
		assert.NotEmpty(t, s.volumeProblems(), name)
	}
}

func TestEngineLagExemptsOnlyByDesignDegradedVolumes(t *testing.T) {
	s := byDesignSnapshot()
	s.Volumes[0].Image, s.Volumes[0].CurrentImage = engine173, engine173
	s.Volumes[1].Image, s.Volumes[1].CurrentImage = engine173, engine173
	lagging, exempt := s.engineLag()
	assert.Equal(t, []string{"pvc-idle"}, lagging)
	assert.Equal(t, []string{"shop/data-db-0"}, exempt)

	assert.Empty(t, s.readyProblems("v1.8.2"), "an older stable engine is ready for a catch-up")
	assert.Contains(t, strings.Join(s.settledProblems("v1.8.2"), "\n"), "pvc-idle does not run the default engine yet")
}

func TestParseLonghornObjects(t *testing.T) {
	data := `Warning: v1beta1 is deprecated
{"items":[
 {"kind":"Volume","metadata":{"name":"pvc-db"},"spec":{"numberOfReplicas":3,"image":"e:1","dataEngine":"v1"},
  "status":{"state":"attached","robustness":"degraded","currentImage":"e:1","kubernetesStatus":{"namespace":"shop","pvcName":"data"}}},
 {"kind":"Replica","metadata":{"name":"pvc-db-r-1"},"spec":{"volumeName":"pvc-db","nodeID":"n1","healthyAt":"t","failedAt":""},"status":{"currentState":"running"}},
 {"kind":"Engine","metadata":{"name":"pvc-db-e-0"},"spec":{"volumeName":"pvc-db"},
  "status":{"currentState":"running","replicaModeMap":{"pvc-db-r-1":"RW"},"rebuildStatus":{"tcp://10.0.0.1:10000":{"isRebuilding":true}}}},
 {"kind":"EngineImage","metadata":{"name":"ei-1"},"spec":{"image":"e:1"},"status":{"state":"deployed","cliAPIVersion":10,"controllerAPIVersion":5}},
 {"kind":"InstanceManager","metadata":{"name":"im-1"},"spec":{"image":"im:1","nodeID":"n1","dataEngine":"v1"},"status":{"currentState":"running"}},
 {"kind":"Node","metadata":{"name":"n1"}}
]}`
	var s longhornSnapshot
	require.NoError(t, parseLonghornObjects(jsonPayload(data), &s))
	assert.Equal(t, []lhVolume{{Name: "pvc-db", Claim: "shop/data", Replicas: 3, Image: "e:1", DataEngine: "v1", State: "attached", Robustness: "degraded", CurrentImage: "e:1"}}, s.Volumes)
	assert.Equal(t, []lhReplica{{Name: "pvc-db-r-1", Volume: "pvc-db", Node: "n1", HealthyAt: "t", State: "running"}}, s.Replicas)
	assert.Equal(t, []lhEngine{{Volume: "pvc-db", State: "running", Modes: map[string]string{"pvc-db-r-1": "RW"}, Rebuilding: true}}, s.Engines)
	assert.Equal(t, []lhEngineImage{{Name: "ei-1", Image: "e:1", State: "deployed", CLIAPI: 10, Controller: 5}}, s.EngineImages)
	assert.Equal(t, []lhInstanceManager{{Node: "n1", Image: "im:1", DataEngine: "v1", State: "running"}}, s.InstanceManagers)
	assert.Equal(t, []string{"n1"}, s.LonghornNodes)
}

func TestParseLonghornWorkloads(t *testing.T) {
	data := `{"items":[
 {"kind":"DaemonSet","metadata":{"name":"longhorn-manager","generation":3},
  "spec":{"template":{"spec":{"containers":[{"name":"longhorn-manager","image":"longhornio/longhorn-manager:v1.8.2"}]}}},
  "status":{"observedGeneration":3,"desiredNumberScheduled":2,"updatedNumberScheduled":2,"numberAvailable":2}},
 {"kind":"Deployment","metadata":{"name":"csi-attacher","generation":2},"spec":{"replicas":3},
  "status":{"observedGeneration":2,"replicas":4,"updatedReplicas":3,"availableReplicas":3}}
]}`
	var s longhornSnapshot
	require.NoError(t, parseLonghornWorkloads([]byte(data), &s))
	assert.Equal(t, lhWorkload{Kind: "DaemonSet", Ready: true, Manager: "longhornio/longhorn-manager:v1.8.2"}, s.Workloads["DaemonSet/longhorn-manager"])
	assert.False(t, s.Workloads["Deployment/csi-attacher"].Ready, "an old pod is still there")
}

func TestReadLonghornSnapshotFailsClosed(t *testing.T) {
	r := &routedRunner{routes: []route{{contains: "settings.longhorn.io", out: "error: the server doesn't have a resource type"}}}
	_, err := readLonghornSnapshot(r)
	assert.Error(t, err)
}
