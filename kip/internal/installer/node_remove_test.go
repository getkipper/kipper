package installer

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEvictLonghornNodeSkipsAClusterWithoutLonghorn(t *testing.T) {
	h := &routedRunner{}
	require.NoError(t, EvictLonghornNode(h, "worker-1"))
	assert.False(t, h.ran("patch nodes.longhorn.io"), "no CRD, no Longhorn node to change")

	h = &routedRunner{routes: []route{
		{contains: "get crd nodes.longhorn.io", out: "customresourcedefinition.apiextensions.k8s.io/nodes.longhorn.io\n"},
		{contains: "get nodes.longhorn.io", out: "node.longhorn.io/worker-1\n"},
	}}
	require.NoError(t, EvictLonghornNode(h, "worker-1"))
	assert.True(t, h.ran(`"allowScheduling":false,"evictionRequested":true`))
}

func TestDeleteNodeObjectsDeletesTheNodeAndItsLonghornNode(t *testing.T) {
	h := &routedRunner{routes: []route{
		{contains: "get crd nodes.longhorn.io", out: "customresourcedefinition.apiextensions.k8s.io/nodes.longhorn.io\n"},
		{contains: "get nodes.longhorn.io", out: "node.longhorn.io/worker-1\n"},
	}}
	require.NoError(t, DeleteNodeObjects(h, "worker-1"))
	assert.True(t, h.ran("kubectl delete node 'worker-1' --ignore-not-found"))
	assert.True(t, h.ran("delete nodes.longhorn.io 'worker-1'"))
}

func TestDeleteNodeRecordRemovesOnlyThatKey(t *testing.T) {
	h := &routedRunner{}
	require.NoError(t, DeleteNodeRecord(h, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"))
	require.Len(t, h.stdins, 1)
	assert.JSONEq(t, `{"data":{"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb":null}}`, h.stdins[0])
	assert.Contains(t, h.commands[0], `|| exit 1`, "a failed read of the ConfigMap is an error, not a finished removal")

	still := &routedRunner{routes: []route{{contains: "jsonpath='{.data}'", out: `{"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb":"{\"name\":\"worker-1\",\"host\":\"203.0.113.11\",\"machineID\":\"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\",\"role\":\"worker\",\"state\":\"removing\"}"}`}}}
	assert.ErrorContains(t, DeleteNodeRecord(still, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"), "still there", "the removal is checked, not assumed")

	assert.Error(t, DeleteNodeRecord(&routedRunner{}, "x; rm -rf /"), "only a machine id reaches the command")
}

// A removal that stopped after asking Longhorn to evict leaves the request in
// place, and Longhorn refuses scheduling while it stands. Rejoining with
// kip node add clears it, so a started removal can be undone.
func TestEnablingLonghornSchedulingClearsAnEvictionRequest(t *testing.T) {
	h := &routedRunner{}
	require.NoError(t, setLonghornScheduling(h, "worker-1", true, fastJoin))
	assert.True(t, h.ran(`{"spec":{"allowScheduling":true,"evictionRequested":false}}`))

	h = &routedRunner{}
	require.NoError(t, setLonghornScheduling(h, "worker-1", false, fastJoin))
	assert.True(t, h.ran(`{"spec":{"allowScheduling":false}}`), "disabling leaves an eviction request alone")
}

func TestNodeRegisteredIgnoresKubectlWarnings(t *testing.T) {
	warning := "Warning: the cluster version is newer than this kubectl\n"
	gone := &routedRunner{routes: []route{{contains: "kubectl get node", out: warning}}}
	registered, err := NodeRegistered(gone, "worker-1")
	require.NoError(t, err)
	assert.False(t, registered, "a warning is not a Node")

	there := &routedRunner{routes: []route{{contains: "kubectl get node", out: warning + "node/worker-1\n"}}}
	registered, err = NodeRegistered(there, "worker-1")
	require.NoError(t, err)
	assert.True(t, registered)
}

func TestVolumesOnlyOnCountsFailedReplicasHere(t *testing.T) {
	replicas := "pvc-a\tworker-1\t2026-10-18T00:00:00Z\t2026-10-18T00:05:00Z\n" +
		"pvc-b\tworker-1\t2026-10-18T00:00:00Z\t\n" +
		"pvc-b\tserver-1\t2026-10-18T00:00:00Z\t\n" +
		"pvc-c\tserver-1\t2026-10-18T00:00:00Z\t\n"
	assert.Equal(t, []string{"pvc-a"}, volumesOnlyOn(replicas, "worker-1"))
}

func TestJoinInProgressExemptsTheNodeItself(t *testing.T) {
	records := []NodeRecord{{Name: "w1", MachineID: "a", State: NodeStateJoining}, {Name: "w2", MachineID: "b", State: NodeStateReady}}
	_, busy := JoinInProgress(records, "b")
	assert.True(t, busy)
	_, busy = JoinInProgress(records, "a")
	assert.False(t, busy)
}
