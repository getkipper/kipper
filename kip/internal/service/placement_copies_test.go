package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func placementReplica(name, node, lastFailedAt string) *unstructured.Unstructured {
	r := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "longhorn.io/v1beta2", "kind": "Replica",
		"metadata": map[string]interface{}{"name": name, "namespace": "longhorn-system", "labels": map[string]interface{}{"longhornvolume": "pvc-1"}},
		"spec":     map[string]interface{}{"nodeID": node, "healthyAt": "2026-10-24T01:00:00Z", "failedAt": "", "lastFailedAt": lastFailedAt},
	}}
	return r
}

func placementNode(name string, ready corev1.ConditionStatus, taints ...corev1.Taint) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       corev1.NodeSpec{Taints: taints},
		Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: ready}}},
	}
}

func longhornPlacementManager(nodes []runtime.Object, replicas ...runtime.Object) *Manager {
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "data-db-0"},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "pv-1"},
	}
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-1"},
		Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
			CSI: &corev1.CSIPersistentVolumeSource{Driver: "driver.longhorn.io", VolumeHandle: "pvc-1"},
		}},
	}
	client := fake.NewSimpleClientset(append([]runtime.Object{claim, pv}, nodes...)...) //nolint:staticcheck
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{LonghornReplicaGVR: "ReplicaList"}, replicas...)
	return &Manager{Client: client, Dynamic: dyn}
}

func TestPlacementCountsCurrentCopiesOnAvailableNodesOnce(t *testing.T) {
	fence := corev1.Taint{Key: "node.kubernetes.io/out-of-service", Effect: corev1.TaintEffectNoExecute}
	for name, tc := range map[string]struct {
		nodes       []runtime.Object
		extra       []runtime.Object
		want        []string
		unavailable []string
	}{
		"two ready holders": {[]runtime.Object{placementNode("worker-1", corev1.ConditionTrue)}, nil, []string{"server-1", "worker-1"}, nil},
		"a fenced holder":   {[]runtime.Object{placementNode("worker-1", corev1.ConditionTrue, fence)}, nil, []string{"server-1"}, []string{"worker-1"}},
		"a NotReady holder": {[]runtime.Object{placementNode("worker-1", corev1.ConditionFalse)}, nil, []string{"server-1"}, []string{"worker-1"}},
		"a missing holder":  {nil, nil, []string{"server-1"}, nil},
		"a second replica on the same node": {[]runtime.Object{placementNode("worker-1", corev1.ConditionTrue)},
			[]runtime.Object{placementReplica("r3", "server-1", "")}, []string{"server-1", "worker-1"}, nil},
	} {
		replicas := append([]runtime.Object{placementReplica("r1", "server-1", ""), placementReplica("r2", "worker-1", "")}, tc.extra...)
		mgr := longhornPlacementManager(append([]runtime.Object{placementNode("server-1", corev1.ConditionTrue)}, tc.nodes...), replicas...)
		p, err := mgr.Placement(context.Background(), "shop", "db")
		require.NoError(t, err, name)
		assert.Equal(t, tc.want, p.DataNodes, name)
		assert.Equal(t, tc.unavailable, p.UnavailableNodes, name)
		assert.Empty(t, p.UncheckedNodes, name)
	}
}

func TestPlacementLeavesASalvagedReplicaOut(t *testing.T) {
	mgr := longhornPlacementManager(
		[]runtime.Object{placementNode("server-1", corev1.ConditionTrue), placementNode("worker-1", corev1.ConditionTrue)},
		placementReplica("r1", "server-1", ""),
		placementReplica("r2", "worker-1", "2026-10-24T02:00:00Z"),
	)
	p, err := mgr.Placement(context.Background(), "shop", "db")
	require.NoError(t, err)
	assert.Equal(t, []string{"server-1"}, p.DataNodes)
}

func TestPlacementNamesHoldersWhoseNodeCannotBeRead(t *testing.T) {
	mgr := longhornPlacementManager(
		[]runtime.Object{placementNode("server-1", corev1.ConditionTrue), placementNode("worker-1", corev1.ConditionTrue)},
		placementReplica("r1", "server-1", ""),
		placementReplica("r2", "worker-1", ""),
	)
	mgr.Client.(*fake.Clientset).PrependReactor("get", "nodes", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.(k8stesting.GetAction).GetName() == "worker-1" {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "worker-1", nil)
		}
		return false, nil, nil
	})
	p, err := mgr.Placement(context.Background(), "shop", "db")
	require.NoError(t, err)
	assert.Equal(t, []string{"server-1"}, p.DataNodes)
	assert.Equal(t, []string{"worker-1"}, p.UncheckedNodes)
}

func placementVolume(state string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "longhorn.io/v1beta2", "kind": "Volume",
		"metadata": map[string]interface{}{"name": "pvc-1", "namespace": "longhorn-system"},
		"status":   map[string]interface{}{"state": state},
	}}
}

// Data placement uses the same definition of a healthy copy as the service's
// copies status: a copy that served writes again after a salvage holds the
// data, and so does the copy of a volume no pod has mounted yet.
func TestPlacementCountsTheCopiesTheStatusCounts(t *testing.T) {
	served := placementReplica("r1", "server-1", "2026-10-24T02:00:00Z")
	_ = unstructured.SetNestedField(served.Object, "2026-10-24T02:00:10Z", "spec", "lastHealthyAt")
	unused := placementReplica("r1", "server-1", "")
	_ = unstructured.SetNestedField(unused.Object, "", "spec", "healthyAt")
	for name, objs := range map[string][]runtime.Object{
		"salvaged and served again": {placementVolume("attached"), served},
		"never attached":            {placementVolume("detached"), unused},
	} {
		mgr := longhornPlacementManager([]runtime.Object{placementNode("server-1", corev1.ConditionTrue)}, objs...)
		p, err := mgr.Placement(context.Background(), "shop", "db")
		require.NoError(t, err, name)
		assert.Equal(t, []string{"server-1"}, p.DataNodes, name)
	}
}

// A volume restored from a backup has no data in its unmarked copies until
// the restore completes, and a reused replica keeps its failure history.
func TestPlacementLeavesOutUnmarkedCopiesOfAVolumeThatHadOrAwaitsData(t *testing.T) {
	restoring := placementVolume("detached")
	_ = unstructured.SetNestedField(restoring.Object, "s3://backups@example/?backup=backup-1&volume=pvc-0", "spec", "fromBackup")
	unused := placementReplica("r1", "server-1", "")
	_ = unstructured.SetNestedField(unused.Object, "", "spec", "healthyAt")
	reused := placementReplica("r1", "server-1", "2026-10-24T02:00:00Z")
	_ = unstructured.SetNestedField(reused.Object, "", "spec", "healthyAt")
	for name, objs := range map[string][]runtime.Object{
		"restore pending":      {restoring, unused},
		"reused for a rebuild": {placementVolume("detached"), reused},
	} {
		mgr := longhornPlacementManager([]runtime.Object{placementNode("server-1", corev1.ConditionTrue)}, objs...)
		p, err := mgr.Placement(context.Background(), "shop", "db")
		require.NoError(t, err, name)
		assert.Empty(t, p.DataNodes, name)
	}
}
