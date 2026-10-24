package installer

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/controller/pkg/stranded"
	"github.com/getkipper/kipper/kip/internal/config"
)

const notReadyNode = `{"metadata":{"name":"worker-1"},
 "spec":{"taints":[{"key":"node.kubernetes.io/unreachable","effect":"NoExecute"}]},
 "status":{"nodeInfo":{"bootID":"0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0"},
  "conditions":[{"type":"Ready","status":"Unknown","lastTransitionTime":"2026-10-24T08:00:00Z"}]}}
`

func TestReadFenceNodeMeasuresNotReadyOnTheServerClock(t *testing.T) {
	now := time.Date(2026, 10, 24, 8, 1, 30, 0, time.UTC).Unix()
	master := &routedRunner{routes: []route{{contains: "kubectl get node", out: notReadyNode + "kip-server-now=" + strconv.FormatInt(now, 10) + "\n"}}}
	n, err := ReadFenceNode(master, "worker-1")
	require.NoError(t, err)
	assert.True(t, n.Exists)
	assert.False(t, n.Ready)
	assert.False(t, n.OutOfService)
	assert.Equal(t, 90*time.Second, n.NotReadyFor)
	assert.Equal(t, "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0", n.BootID)
}

func TestReadFenceNodeSeesTheTaintAndAMissingNode(t *testing.T) {
	tainted := strings.Replace(notReadyNode, `"node.kubernetes.io/unreachable"`, `"node.kubernetes.io/out-of-service","value":"nodeshutdown"`, 1)
	master := &routedRunner{routes: []route{{contains: "kubectl get node", out: tainted + "kip-server-now=1793174400\n"}}}
	n, err := ReadFenceNode(master, "worker-1")
	require.NoError(t, err)
	assert.True(t, n.OutOfService)

	gone := &routedRunner{routes: []route{{contains: "kubectl get node", out: "kip-server-now=1793174400\n"}}}
	n, err = ReadFenceNode(gone, "worker-1")
	require.NoError(t, err)
	assert.False(t, n.Exists)

	broken := &routedRunner{routes: []route{{contains: "kubectl get node", out: notReadyNode}}}
	_, err = ReadFenceNode(broken, "worker-1")
	assert.Error(t, err, "without the server's clock the NotReady age is unknown")
}

func TestTaintOutOfServiceAppliesTheNoExecuteTaint(t *testing.T) {
	master := &routedRunner{}
	require.NoError(t, TaintOutOfService(master, "worker-1"))
	assert.True(t, master.ran("kubectl taint node 'worker-1' node.kubernetes.io/out-of-service=nodeshutdown:NoExecute --overwrite"))
}

func TestKubeletAnswersProbesFromTheServer(t *testing.T) {
	open := &routedRunner{routes: []route{{contains: "/dev/tcp/198.51.100.7/10250", out: "kip-kubelet=open\n"}}}
	up, err := KubeletAnswers(open, "198.51.100.7")
	require.NoError(t, err)
	assert.True(t, up)
	closed := &routedRunner{routes: []route{{contains: "/dev/tcp/", out: "kip-kubelet=closed\n"}}}
	up, err = KubeletAnswers(closed, "198.51.100.7")
	require.NoError(t, err)
	assert.False(t, up)
	// A probe that did not run proves nothing about the kubelet.
	broken := &routedRunner{routes: []route{{contains: "/dev/tcp/", err: errors.New("ssh: channel open failed")}}}
	_, err = KubeletAnswers(broken, "198.51.100.7")
	assert.Error(t, err)
	silent := &routedRunner{routes: []route{{contains: "/dev/tcp/", out: "bash: timeout: command not found\n"}}}
	_, err = KubeletAnswers(silent, "198.51.100.7")
	assert.Error(t, err)
	odd := &routedRunner{}
	_, err = KubeletAnswers(odd, "worker-1; reboot")
	require.Error(t, err, "a name that does not resolve is not probed")
	assert.True(t, odd.ran("getent ahostsv4 'worker-1; reboot'"), "the name is quoted: %q", odd.commands)
}

const fencePods = `{"items":[
 {"metadata":{"name":"db-0","namespace":"shop","labels":{"app.kubernetes.io/managed-by":"kipper"},
   "ownerReferences":[{"kind":"StatefulSet","name":"db","apiVersion":"apps/v1"}]},
  "spec":{"nodeName":"worker-1","volumes":[{"name":"data","persistentVolumeClaim":{"claimName":"data-db-0"}}]}},
 {"metadata":{"name":"blob-0","namespace":"shop","labels":{"app.kubernetes.io/managed-by":"kipper"}},
  "spec":{"nodeName":"worker-1","volumes":[{"name":"data","persistentVolumeClaim":{"claimName":"blob"}}]}}
]}`

const fenceClaims = `{"items":[
 {"metadata":{"name":"data-db-0","namespace":"shop"},"spec":{"volumeName":"pvc-1","accessModes":["ReadWriteOnce"]}},
 {"metadata":{"name":"blob","namespace":"shop"},"spec":{"volumeName":"pvc-2","accessModes":["ReadWriteOnce"]}}
]}`

const fencePVs = `{"items":[
 {"metadata":{"name":"pvc-1"},"spec":{"csi":{"driver":"driver.longhorn.io","volumeHandle":"pvc-1"}}},
 {"metadata":{"name":"pvc-2"},"spec":{"csi":{"driver":"driver.longhorn.io","volumeHandle":"pvc-2"}}}
]}`

const fenceSets = `{"items":[
 {"metadata":{"name":"db","namespace":"shop"},"spec":{"replicas":1}},
 {"metadata":{"name":"queue","namespace":"jobs","annotations":{"kipper.run/fence-stop":"{\"node\":\"worker-1\",\"replicas\":1,\"reason\":\"stale\",\"volumes\":[\"pvc-9\"]}"}},"spec":{"replicas":0}},
 {"metadata":{"name":"other","namespace":"jobs","annotations":{"kipper.run/fence-stop":"{\"node\":\"worker-2\",\"replicas\":1,\"reason\":\"stale\",\"volumes\":[\"pvc-8\"]}"}},"spec":{"replicas":0}}
]}`

const fenceReplicas = `{"items":[
 {"metadata":{"name":"pvc-1-r-a"},"spec":{"volumeName":"pvc-1","nodeID":"server-1","healthyAt":"2026-10-24T08:00:00Z"}}
]}`

func fenceViewRunner() *routedRunner {
	return &routedRunner{routes: []route{
		{contains: "get pods", out: fencePods},
		{contains: "get pvc", out: fenceClaims},
		{contains: "get pv ", out: fencePVs},
		{contains: "get statefulsets", out: fenceSets},
		{contains: "replicas.longhorn.io", out: fenceReplicas},
	}}
}

func TestReadFenceViewGroupsByStatefulSetAndAddsEarlierStops(t *testing.T) {
	view, err := ReadFenceView(fenceViewRunner(), "worker-1")
	require.NoError(t, err)
	require.Len(t, view.Services, 2)
	assert.Equal(t, FenceService{Namespace: "jobs", StatefulSet: "queue", Volumes: []string{"pvc-9"},
		Stop: &FenceStopMark{Node: "worker-1", Replicas: 1, Reason: "stale", Volumes: []string{"pvc-9"}}}, view.Services[0])
	assert.Equal(t, FenceService{Namespace: "shop", StatefulSet: "db", Pods: []string{"db-0"}, Volumes: []string{"pvc-1"}}, view.Services[1])
	assert.Equal(t, []PodHolder{{Namespace: "shop", Pod: "blob-0"}}, view.Holders, "an RWO pod no StatefulSet owns is listed with what starts it")
	assert.Len(t, view.Replicas, 1)
}

func TestReadFenceViewFailsOnAnUnreadableStopMark(t *testing.T) {
	r := fenceViewRunner()
	r.routes[3] = route{contains: "get statefulsets", out: `{"items":[{"metadata":{"name":"q","namespace":"jobs","annotations":{"kipper.run/fence-stop":"{"}}}]}`}
	_, err := ReadFenceView(r, "worker-1")
	assert.Error(t, err)
}

func TestJudgeFreshness(t *testing.T) {
	healthy := "2026-10-24T08:00:00Z"
	replicas := []stranded.Replica{
		{Name: "a-local", Volume: "a", Node: "worker-1", HealthyAt: healthy},
		{Name: "a-current", Volume: "a", Node: "server-1", HealthyAt: healthy},
		{Name: "b-stale", Volume: "b", Node: "server-1", HealthyAt: healthy, LastFailedAt: "2026-10-24T09:00:00Z"},
		{Name: "c-local", Volume: "c", Node: "worker-1", HealthyAt: healthy},
	}
	f := JudgeFreshness("worker-1", []string{"a"}, replicas)
	assert.True(t, f.Current)
	assert.Empty(t, f.Reason)

	f = JudgeFreshness("worker-1", []string{"a", "b"}, replicas)
	assert.False(t, f.Current)
	assert.Equal(t, FenceStopStale, f.Reason)
	assert.Equal(t, []config.ReplicaStamp{
		{Volume: "a", Name: "a-current", Node: "server-1", HealthyAt: healthy},
		{Volume: "b", Name: "b-stale", Node: "server-1", HealthyAt: healthy, LastFailedAt: "2026-10-24T09:00:00Z"},
	}, f.Stamps)

	f = JudgeFreshness("worker-1", []string{"b", "c"}, replicas)
	assert.False(t, f.Current)
	assert.Equal(t, FenceStopOnlyCopy, f.Reason, "a volume with no copy elsewhere outranks a stale one")

	f = JudgeFreshness("", []string{"c"}, replicas)
	assert.True(t, f.Current, "with no node excluded, the returned worker's own replica counts")
}

func TestSameStamps(t *testing.T) {
	a := []config.ReplicaStamp{{Volume: "v", Name: "r", Node: "server-1", HealthyAt: "2026-10-24T08:00:00Z"}}
	b := []config.ReplicaStamp{{Volume: "v", Name: "r", Node: "server-1", HealthyAt: "2026-10-24T08:05:00Z"}}
	assert.True(t, SameStamps(a, a))
	assert.False(t, SameStamps(a, b), "a replica that changed since the consent voids it")
	assert.False(t, SameStamps(nil, nil), "a consent with no replicas consents to nothing")
}

func TestStopForFenceRecordsTheCountInTheSamePatch(t *testing.T) {
	master := &routedRunner{routes: []route{{contains: "get statefulset 'db'", out: `{"metadata":{"resourceVersion":"41"},"spec":{"replicas":2}}`}}}
	require.NoError(t, StopForFence(master, "shop", "db", FenceStopMark{Node: "worker-1", Reason: FenceStopStale, Volumes: []string{"pvc-1"}}))
	require.Len(t, master.stdins, 1)
	var patch struct {
		Metadata struct {
			ResourceVersion string            `json:"resourceVersion"`
			Annotations     map[string]string `json:"annotations"`
		} `json:"metadata"`
		Spec struct {
			Replicas int `json:"replicas"`
		} `json:"spec"`
	}
	require.NoError(t, json.Unmarshal([]byte(master.stdins[0]), &patch))
	assert.Equal(t, "41", patch.Metadata.ResourceVersion)
	assert.Equal(t, 0, patch.Spec.Replicas)
	var mark FenceStopMark
	require.NoError(t, json.Unmarshal([]byte(patch.Metadata.Annotations[FenceStopAnnotation]), &mark))
	assert.Equal(t, FenceStopMark{Node: "worker-1", Replicas: 2, Reason: FenceStopStale, Volumes: []string{"pvc-1"}}, mark)
}

func TestStopForFenceNeverOverwritesAnEarlierStop(t *testing.T) {
	master := &routedRunner{routes: []route{{contains: "get statefulset 'db'", out: `{"metadata":{"resourceVersion":"42","annotations":{"kipper.run/fence-stop":"{\"node\":\"worker-1\",\"replicas\":2}"}},"spec":{"replicas":0}}`}}}
	require.NoError(t, StopForFence(master, "shop", "db", FenceStopMark{Node: "worker-1", Reason: FenceStopStale}))
	assert.Empty(t, master.stdins, "a retry must not record 0 as the count to restore")
}

func TestStartAfterFenceRestoresTheCountAndDropsTheMark(t *testing.T) {
	master := &routedRunner{routes: []route{{contains: "get statefulset 'db'", out: `{"metadata":{"resourceVersion":"43","annotations":{"kipper.run/fence-stop":"{\"node\":\"worker-1\",\"replicas\":2,\"reason\":\"stale\"}"}},"spec":{"replicas":0}}`}}}
	require.NoError(t, StartAfterFence(master, "shop", "db"))
	require.Len(t, master.stdins, 1)
	assert.JSONEq(t, `{"metadata":{"resourceVersion":"43","annotations":{"kipper.run/fence-stop":null}},"spec":{"replicas":2}}`, master.stdins[0])

	untouched := &routedRunner{routes: []route{{contains: "get statefulset 'db'", out: `{"metadata":{"resourceVersion":"44"},"spec":{"replicas":1}}`}}}
	require.NoError(t, StartAfterFence(untouched, "shop", "db"))
	assert.Empty(t, untouched.stdins)
}

func TestAttachmentsOnListsLonghornAttachmentsOfTheNode(t *testing.T) {
	master := &routedRunner{routes: []route{{contains: "get volumeattachments", out: `{"items":[
	 {"metadata":{"name":"csi-a"},"spec":{"attacher":"driver.longhorn.io","nodeName":"worker-1","source":{"persistentVolumeName":"pvc-1"}}},
	 {"metadata":{"name":"csi-b"},"spec":{"attacher":"driver.longhorn.io","nodeName":"server-1","source":{"persistentVolumeName":"pvc-2"}}},
	 {"metadata":{"name":"csi-c"},"spec":{"attacher":"other.csi","nodeName":"worker-1","source":{"persistentVolumeName":"pvc-3"}}}
	]}`}}}
	got, err := AttachmentsOn(master, "worker-1")
	require.NoError(t, err)
	assert.Equal(t, []Attachment{{Name: "csi-a", PersistentVolume: "pvc-1"}}, got)
}

func TestDeletesNeverStripFinalizers(t *testing.T) {
	master := &routedRunner{}
	require.NoError(t, DeleteAttachment(master, "csi-a"))
	require.NoError(t, ForceDeletePod(master, "shop", "db-0", "uid-1"))
	for _, c := range master.commands {
		assert.NotContains(t, c, "finalizers")
		assert.NotContains(t, c, "patch")
	}
}

func TestForceDeletePodDeletesOnlyThePodItScanned(t *testing.T) {
	master := &routedRunner{}
	require.NoError(t, ForceDeletePod(master, "shop", "db-0", "uid-1"))
	assert.True(t, master.ran("kubectl delete --raw '/api/v1/namespaces/shop/pods/db-0' -f -"), "%q", master.commands)
	require.Len(t, master.stdins, 1)
	assert.JSONEq(t, `{"kind":"DeleteOptions","apiVersion":"v1","gracePeriodSeconds":0,"preconditions":{"uid":"uid-1"}}`, master.stdins[0])

	// A refused delete is fine once the scanned pod is gone or replaced,
	// and an error while the scanned pod is still there.
	for uid, wantErr := range map[string]bool{"": false, "uid-2": false, "uid-1": true} {
		refused := &routedRunner{routes: []route{
			{contains: "delete --raw", err: errors.New("Error from server (Conflict)")},
			{contains: "get pod 'db-0'", out: uid},
		}}
		err := ForceDeletePod(refused, "shop", "db-0", "uid-1")
		assert.Equal(t, wantErr, err != nil, "pod now has uid %q: %v", uid, err)
	}

	none := &routedRunner{}
	assert.Error(t, ForceDeletePod(none, "shop", "db-0", ""))
	assert.Empty(t, none.commands, "a pod without a known UID is never deleted")
}

func TestPodsWithVolumesOnCarriesTheUIDAndThePersistentVolumeName(t *testing.T) {
	master := &routedRunner{routes: []route{
		{contains: "get pods -A", out: `{"items":[{"metadata":{"name":"db-0","namespace":"shop","uid":"uid-1","labels":{"app.kubernetes.io/managed-by":"kipper"},
		  "ownerReferences":[{"kind":"StatefulSet","name":"db","apiVersion":"apps/v1"}]},
		  "spec":{"nodeName":"worker-1","volumes":[{"name":"data","persistentVolumeClaim":{"claimName":"data-db-0"}}]}}]}`},
		{contains: "get pvc -A", out: `{"items":[{"metadata":{"name":"data-db-0","namespace":"shop"},"spec":{"volumeName":"restored-db","accessModes":["ReadWriteOnce"]}}]}`},
		{contains: "get pv ", out: `{"items":[{"metadata":{"name":"restored-db"},"spec":{"csi":{"driver":"driver.longhorn.io","volumeHandle":"pvc-1"}}}]}`},
	}}
	got, err := PodsWithVolumesOn(master, "worker-1")
	require.NoError(t, err)
	assert.Equal(t, []FencedPod{{Namespace: "shop", Name: "db-0", UID: "uid-1", PersistentVolumes: []string{"restored-db"}}}, got)
}

func TestPodPlacement(t *testing.T) {
	master := &routedRunner{routes: []route{{contains: "get pod 'db-0'", out: `{"metadata":{"name":"db-0"},"spec":{"nodeName":"server-1"},"status":{"conditions":[{"type":"Ready","status":"True"}]}}`}}}
	node, ready, err := PodPlacement(master, "shop", "db-0")
	require.NoError(t, err)
	assert.Equal(t, "server-1", node)
	assert.True(t, ready)

	terminating := &routedRunner{routes: []route{{contains: "get pod", out: `{"metadata":{"name":"db-0","deletionTimestamp":"2026-10-24T08:00:00Z"},"spec":{"nodeName":"worker-1"},"status":{"conditions":[{"type":"Ready","status":"True"}]}}`}}}
	_, ready, err = PodPlacement(terminating, "shop", "db-0")
	require.NoError(t, err)
	assert.False(t, ready, "a pod being deleted is not serving")

	node, _, err = PodPlacement(&routedRunner{}, "shop", "db-0")
	require.NoError(t, err)
	assert.Empty(t, node)
}

func TestStartAgentClearsTheFailedStateFirst(t *testing.T) {
	host := &routedRunner{}
	require.NoError(t, StartAgent(host))
	require.Len(t, host.commands, 1)
	assert.Equal(t, "systemctl reset-failed k3s-agent 2>/dev/null; systemctl start k3s-agent", host.commands[0])
}

func TestAvailableReplicasCountsOnlyReadyNodesWithoutAFence(t *testing.T) {
	replicas := []stranded.Replica{
		{Name: "r1", Volume: "pvc-1", Node: "server-1"},
		{Name: "r2", Volume: "pvc-1", Node: "worker-1"},
		{Name: "r3", Volume: "pvc-1", Node: "worker-2"},
		{Name: "r4", Volume: "pvc-1", Node: "worker-3"},
		// A worker whose record a forced removal deleted can leave replicas.
		{Name: "r5", Volume: "pvc-1", Node: "worker-4"},
		{Name: "r6", Volume: "pvc-1", Node: "worker-5"},
		{Name: "r7", Volume: "pvc-1", Node: "worker-6"},
		{Name: "r8", Volume: "pvc-1", Node: "worker-7"},
	}
	records := []NodeRecord{
		{Name: "server-1", State: NodeStateReady},
		{Name: "worker-1", State: NodeStateFenced, Fence: &config.NodeFence{}},
		{Name: "worker-2", State: NodeStateRemoving, Fence: &config.NodeFence{}},
		{Name: "worker-3", State: NodeStateReady},
		// An unfinished removal or join is no member to start a service on.
		{Name: "worker-5", State: NodeStateRemoving},
		{Name: "worker-6", State: NodeStateWiping},
		{Name: "worker-7", State: NodeStateJoining},
	}
	names := func(rs []stranded.Replica) []string {
		var out []string
		for _, r := range rs {
			out = append(out, r.Name)
		}
		return out
	}
	assert.Equal(t, []string{"r1", "r4"}, names(AvailableReplicas(replicas, records, "")))
	assert.Equal(t, []string{"r1", "r2", "r4"}, names(AvailableReplicas(replicas, records, "worker-1")))
}

func TestFenceReadsSkipKubectlWarnings(t *testing.T) {
	// The SSH runner merges stderr into stdout, so a kubectl warning lands
	// in front of the JSON.
	const warning = "Warning: v1 Something is deprecated in v1.99+ [see docs]\n"
	master := &routedRunner{routes: []route{
		{contains: "get node", out: warning + `{"metadata":{"name":"worker-1"}}` + "\nkip-server-now=1792800000\n"},
		{contains: "get volumeattachments", out: warning + `{"items":[{"metadata":{"name":"csi-a"},"spec":{"attacher":"driver.longhorn.io","nodeName":"worker-1","source":{"persistentVolumeName":"pvc-1"}}}]}`},
		{contains: "get replicas.longhorn.io", out: warning + `{"items":[]}`},
		{contains: "get pod 'db-0'", out: warning + `{"metadata":{"name":"db-0"},"spec":{"nodeName":"server-1"}}`},
	}}
	n, err := ReadFenceNode(master, "worker-1")
	require.NoError(t, err)
	assert.True(t, n.Exists)
	vas, err := AttachmentsOn(master, "worker-1")
	require.NoError(t, err)
	assert.Len(t, vas, 1)
	_, err = ReadReplicas(master)
	require.NoError(t, err)
	node, _, err := PodPlacement(master, "shop", "db-0")
	require.NoError(t, err)
	assert.Equal(t, "server-1", node)

	gone := &routedRunner{routes: []route{{contains: "get node", out: warning + "kip-server-now=1792800000\n"}}}
	n, err = ReadFenceNode(gone, "worker-1")
	require.NoError(t, err)
	assert.False(t, n.Exists, "a warning alone is no node")
}

func TestFenceValueReadsSkipKubectlWarnings(t *testing.T) {
	const warning = "Warning: something is deprecated\n"
	master := &routedRunner{routes: []route{{contains: "node-down-pod-deletion-policy", out: warning + "do-nothing"}}}
	policy, err := NodeDownPolicy(master)
	require.NoError(t, err)
	assert.Equal(t, "do-nothing", policy)

	still := &routedRunner{routes: []route{
		{contains: "delete --raw", err: errors.New("Error from server (InternalError)")},
		{contains: "get pod 'db-0'", out: warning + "uid-1"},
	}}
	assert.Error(t, ForceDeletePod(still, "shop", "db-0", "uid-1"), "the scanned pod is still there behind the warning")
}

func TestKubeletAnswersResolvesAHostnameOnTheServer(t *testing.T) {
	master := &routedRunner{routes: []route{
		{contains: "getent ahostsv4 'worker-1.example.com'", out: "192.0.2.21      STREAM worker-1.example.com\n192.0.2.21      DGRAM\n"},
		{contains: "/dev/tcp/192.0.2.21/10250", out: "kip-kubelet=open\n"},
	}}
	up, err := KubeletAnswers(master, "worker-1.example.com")
	require.NoError(t, err)
	assert.True(t, up)

	for _, address := range []string{"worker-9.example.com", "2001:db8::21"} {
		unresolved := &routedRunner{routes: []route{{contains: "getent", err: errors.New("exit status 2")}}}
		_, err := KubeletAnswers(unresolved, address)
		require.Error(t, err, address)
		assert.Contains(t, err.Error(), address)
	}

	down := &routedRunner{routes: []route{{contains: "/dev/tcp/192.0.2.21/10250", out: "kip-kubelet=closed\n"}}}
	up, err = KubeletAnswers(down, "192.0.2.21")
	require.NoError(t, err)
	assert.False(t, up)
}

func TestClearFenceStopDropsTheMarkAndKeepsTheServiceStopped(t *testing.T) {
	master := &routedRunner{routes: []route{{contains: "get statefulset 'db'", out: `{"metadata":{"resourceVersion":"43","annotations":{"kipper.run/fence-stop":"{}"}},"spec":{"replicas":0}}`}}}
	require.NoError(t, ClearFenceStop(master, "shop", "db"))
	require.Len(t, master.stdins, 1)
	assert.JSONEq(t, `{"metadata":{"resourceVersion":"43","annotations":{"kipper.run/fence-stop":null}}}`, master.stdins[0])
}

func TestReadLonghornVolumes(t *testing.T) {
	master := &routedRunner{routes: []route{{contains: "get volumes.longhorn.io", out: "Warning: x\n" + `{"items":[{"metadata":{"name":"pvc-1"}},{"metadata":{"name":"pvc-2"}}]}`}}}
	got, err := ReadLonghornVolumes(master)
	require.NoError(t, err)
	assert.Equal(t, []string{"pvc-1", "pvc-2"}, got)
}

func TestReadFenceViewNamesWhatStartsEachUnownedPod(t *testing.T) {
	r := fenceViewRunner()
	r.routes[0] = route{contains: "get pods", out: `{"items":[
 {"metadata":{"name":"dt-move1-export","namespace":"shop","labels":{"app.kubernetes.io/managed-by":"kipper","kipper.run/resource-type":"datatransfer-mover"}},
  "spec":{"nodeName":"worker-1","volumes":[{"name":"data","persistentVolumeClaim":{"claimName":"blob"}}]}},
 {"metadata":{"name":"report-abc","namespace":"shop","labels":{"app.kubernetes.io/managed-by":"kipper"},
   "ownerReferences":[{"kind":"Job","name":"report","apiVersion":"batch/v1","controller":true}]},
  "spec":{"nodeName":"worker-1","volumes":[{"name":"data","persistentVolumeClaim":{"claimName":"blob"}}]}},
 {"metadata":{"name":"web-7f-x","namespace":"shop","labels":{"app.kubernetes.io/managed-by":"kipper"},
   "ownerReferences":[{"kind":"ReplicaSet","name":"web-7f","apiVersion":"apps/v1","controller":true}]},
  "spec":{"nodeName":"worker-1","volumes":[{"name":"data","persistentVolumeClaim":{"claimName":"blob"}}]}}
]}`}
	r.routes = append(r.routes, route{contains: "get datatransfers.kipper.run -A", out: `{"items":[]}`})
	view, err := ReadFenceView(r, "worker-1")
	require.NoError(t, err)
	assert.Equal(t, []PodHolder{
		{Namespace: "shop", Pod: "dt-move1-export", Kind: HolderDataTransfer, Name: "move1"},
		{Namespace: "shop", Pod: "report-abc", Kind: HolderJob, Name: "report", APIVersion: "batch/v1"},
		{Namespace: "shop", Pod: "web-7f-x", Kind: "ReplicaSet", Name: "web-7f", APIVersion: "apps/v1"},
	}, view.Holders)
}

func TestFailDataTransferStopsAnUnfinishedTransferOnly(t *testing.T) {
	master := &routedRunner{routes: []route{{contains: "get datatransfers.kipper.run 'move1'", out: `{"metadata":{"resourceVersion":"7"},"status":{"phase":"Transferring"}}`}}}
	require.NoError(t, FailDataTransfer(master, "shop", "move1", "worker-1 was fenced"))
	assert.True(t, master.ran("kubectl -n 'shop' patch datatransfers.kipper.run 'move1' --subresource=status --type merge --patch-file /dev/stdin"), "%q", master.commands)
	require.Len(t, master.stdins, 1)
	assert.JSONEq(t, `{"metadata":{"resourceVersion":"7"},"status":{"phase":"Failed","lastError":"worker-1 was fenced"}}`, master.stdins[0])

	for _, phase := range []string{"Completed", "Failed"} {
		done := &routedRunner{routes: []route{{contains: "get datatransfers.kipper.run", out: `{"metadata":{"resourceVersion":"7"},"status":{"phase":"` + phase + `"}}`}}}
		require.NoError(t, FailDataTransfer(done, "shop", "move1", "x"))
		assert.Empty(t, done.stdins, phase)
	}
	gone := &routedRunner{}
	require.NoError(t, FailDataTransfer(gone, "shop", "move1", "x"), "a deleted transfer starts nothing")
	assert.Empty(t, gone.stdins)
}

func TestSuspendJob(t *testing.T) {
	master := &routedRunner{}
	require.NoError(t, SuspendJob(master, "shop", "report"))
	assert.True(t, master.ran("kubectl -n 'shop' patch job 'report' --type merge --patch-file /dev/stdin"), "%q", master.commands)
	require.Len(t, master.stdins, 1)
	assert.JSONEq(t, `{"spec":{"suspend":true}}`, master.stdins[0])
}

func TestOwnerExists(t *testing.T) {
	there := &routedRunner{routes: []route{{contains: "get 'replicaset.v1.apps' 'web-7f' --ignore-not-found -o name", out: "replicaset.apps/web-7f\n"}}}
	ok, err := OwnerExists(there, "shop", "apps/v1", "ReplicaSet", "web-7f")
	require.NoError(t, err)
	assert.True(t, ok)
	ok, err = OwnerExists(&routedRunner{}, "shop", "apps/v1", "ReplicaSet", "web-7f")
	require.NoError(t, err)
	assert.False(t, ok)
	core := &routedRunner{}
	_, _ = OwnerExists(core, "shop", "v1", "ReplicationController", "rc")
	assert.True(t, core.ran("get 'replicationcontroller' 'rc'"), "%q", core.commands)
}

func TestReadFenceViewAddsTheServiceAMigrationMoverPaused(t *testing.T) {
	r := fenceViewRunner()
	r.routes[0] = route{contains: "get pods", out: `{"items":[
 {"metadata":{"name":"dt-move1-export","namespace":"shop","labels":{"app.kubernetes.io/managed-by":"kipper","kipper.run/resource-type":"datatransfer-mover"}},
  "spec":{"nodeName":"worker-1","volumes":[{"name":"data","persistentVolumeClaim":{"claimName":"data-db-0","readOnly":true}}]}}
]}`}
	r.routes[3] = route{contains: "get statefulsets", out: `{"items":[
 {"metadata":{"name":"db","namespace":"shop"},"spec":{"replicas":0,"volumeClaimTemplates":[{"metadata":{"name":"data"}}]}},
 {"metadata":{"name":"db","namespace":"other"},"spec":{"replicas":0,"volumeClaimTemplates":[{"metadata":{"name":"data"}}]}}
]}`}
	r.routes = append(r.routes, route{contains: "get datatransfers.kipper.run -A", out: `{"items":[
 {"metadata":{"name":"move1","namespace":"shop"},"spec":{"kind":"servicePVC","source":{"service":"db"},"sourceReplicas":2}},
 {"metadata":{"name":"move2","namespace":"shop"},"spec":{"kind":"servicePVC","source":{"service":"db"},"sourceReplicas":5}}
]}`})
	view, err := ReadFenceView(r, "worker-1")
	require.NoError(t, err)
	assert.Equal(t, []FenceService{{Namespace: "shop", StatefulSet: "db", Volumes: []string{"pvc-1"}, Replicas: 2}}, view.Services,
		"the paused source service is judged like a stranded one, with the count to restore")
	require.Len(t, view.Holders, 1)
}

func TestStopForFenceRecordsTheCountToRestoreForAPausedService(t *testing.T) {
	paused := &routedRunner{routes: []route{{contains: "get statefulset 'db'", out: `{"metadata":{"resourceVersion":"42"},"spec":{"replicas":0}}`}}}
	require.NoError(t, StopForFence(paused, "shop", "db", FenceStopMark{Node: "worker-1", Reason: FenceStopStale, Replicas: 2}))
	require.Len(t, paused.stdins, 1)
	assert.Contains(t, paused.stdins[0], `\"replicas\":2`)

	unknown := &routedRunner{routes: []route{{contains: "get statefulset 'db'", out: `{"metadata":{"resourceVersion":"42"},"spec":{"replicas":0}}`}}}
	require.NoError(t, StopForFence(unknown, "shop", "db", FenceStopMark{Node: "worker-1", Reason: FenceStopStale}))
	assert.Contains(t, unknown.stdins[0], `\"replicas\":1`, "every Kipper service is created with one replica")
}

// fakeTimeoutShell runs Bash commands with a stub timeout command.
// newFakeTimeoutShell sets its exit codes for local API and peer probes.
type fakeTimeoutShell struct{ path string }

func newFakeTimeoutShell(t *testing.T, self, peer int) fakeTimeoutShell {
	dir := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\ncase \"$*\" in *127.0.0.1/6443*) exit %d ;; *) exit %d ;; esac\n", self, peer)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "timeout"), []byte(script), 0o700)) //nolint:gosec // the fake command must be executable
	return fakeTimeoutShell{path: dir + string(os.PathListSeparator) + os.Getenv("PATH")}
}

func (f fakeTimeoutShell) Run(command string) (string, error) {
	cmd := exec.Command("bash", "-c", command)
	cmd.Env = append(os.Environ(), "PATH="+f.path)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (f fakeTimeoutShell) RunStdin(command string, _ io.Reader) (string, error) {
	return f.Run(command)
}

func TestKubeletProbeTellsAClosedPortFromAProbeThatDidNotRun(t *testing.T) {
	for _, c := range []struct {
		name       string
		self, peer int
		up         bool
		fails      bool
	}{
		{name: "open", peer: 0, up: true},
		{name: "refused", peer: 1},
		{name: "timed out", peer: 124},
		{name: "bash missing", peer: 127, fails: true},
		{name: "bash not executable", peer: 126, fails: true},
		{name: "no /dev/tcp on the server", self: 1, peer: 1, fails: true},
		{name: "timeout missing", self: 127, peer: 127, fails: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			up, err := KubeletAnswers(newFakeTimeoutShell(t, c.self, c.peer), "198.51.100.7")
			if c.fails {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, c.up, up)
		})
	}
}

func TestReadFenceViewListsUnmanagedPodsWithSingleNodeVolumes(t *testing.T) {
	r := fenceViewRunner()
	r.routes[0] = route{contains: "get pods", out: `{"items":[
 {"metadata":{"name":"cache-0","namespace":"tools"},
  "spec":{"nodeName":"worker-1","volumes":[{"name":"data","persistentVolumeClaim":{"claimName":"data-db-0"}},{"name":"more","persistentVolumeClaim":{"claimName":"blob"}}]}},
 {"metadata":{"name":"elsewhere","namespace":"tools"},
  "spec":{"nodeName":"worker-2","volumes":[{"name":"data","persistentVolumeClaim":{"claimName":"blob"}}]}},
 {"metadata":{"name":"plain","namespace":"tools"},"spec":{"nodeName":"worker-1"}}
]}`}
	r.routes[1] = route{contains: "get pvc", out: `{"items":[
 {"metadata":{"name":"data-db-0","namespace":"tools"},"spec":{"volumeName":"pvc-1","accessModes":["ReadWriteOnce"]}},
 {"metadata":{"name":"blob","namespace":"tools"},"spec":{"volumeName":"pvc-2","accessModes":["ReadWriteOnce"]}}
]}`}
	view, err := ReadFenceView(r, "worker-1")
	require.NoError(t, err)
	assert.Equal(t, []string{"tools/cache-0"}, view.Unmanaged)
	assert.Empty(t, view.Holders, "an unmanaged pod is warned about, never stopped")
}

func TestNodeExternalIP(t *testing.T) {
	master := &routedRunner{routes: []route{{contains: "get node 'worker-1'", out: "Warning: x\n198.51.100.20\n"}}}
	got, err := NodeExternalIP(master, "worker-1")
	require.NoError(t, err)
	assert.Equal(t, "198.51.100.20", got)
	assert.True(t, master.ran(`ExternalIP`))

	none := &routedRunner{routes: []route{{contains: "get node", out: ""}}}
	got, err = NodeExternalIP(none, "worker-1")
	require.NoError(t, err)
	assert.Empty(t, got)
}
