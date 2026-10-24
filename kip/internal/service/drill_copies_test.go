package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

// copiesClasses adds the replicated class and a class without a replica count
// to the classes every Longhorn cluster has.
func copiesClasses() *fake.Clientset {
	return fake.NewSimpleClientset( //nolint:staticcheck
		storageClass("longhorn-single", "driver.longhorn.io", map[string]string{"numberOfReplicas": "1", "nodeSelector": "kipper-server"}),
		storageClass("longhorn-replicated", "driver.longhorn.io", map[string]string{"numberOfReplicas": "2", "nodeSelector": "kipper-replica"}),
		storageClass("longhorn", "driver.longhorn.io", map[string]string{"numberOfReplicas": "3"}),
		storageClass("longhorn-defaults", "driver.longhorn.io", nil),
		storageClass("local-path", "rancher.io/local-path", nil),
	)
}

func drillCluster(nodes ...testNode) *Manager {
	objects := defaultSettings()
	for _, n := range nodes {
		objects = append(objects, longhornNodeObject(n))
	}
	return storageManager(copiesClasses(), objects...)
}

func TestEffectiveCopiesIsTheClassCountCappedByTheNodesThatCanTakeIt(t *testing.T) {
	disk := roomyDisk("default", 100<<30, 0, 0)
	server := healthyNode("server-1", []string{"kipper-server", "kipper-replica"}, disk)
	worker := healthyNode("worker-1", []string{"kipper-replica"}, disk)
	computeOnly := healthyNode("worker-2", nil, disk)
	down := healthyNode("worker-3", []string{"kipper-replica"}, disk)
	down.ready = "False"

	for name, tc := range map[string]struct {
		mgr   *Manager
		class string
		want  int
	}{
		"single copy":                        {drillCluster(server, worker), "longhorn-single", 1},
		"two copies on two replica nodes":    {drillCluster(server, worker), "longhorn-replicated", 2},
		"two copies on one replica node":     {drillCluster(server, computeOnly), "longhorn-replicated", 1},
		"a node that is down takes no copy":  {drillCluster(server, down), "longhorn-replicated", 1},
		"three copies on three nodes":        {drillCluster(server, worker, computeOnly), "longhorn", 3},
		"three copies on two nodes":          {drillCluster(server, worker), "longhorn", 2},
		"Longhorn's default count is three":  {drillCluster(server, worker, computeOnly), "longhorn-defaults", 3},
		"local storage keeps one copy":       {drillCluster(server), "local-path", 1},
		"no node takes the class: still one": {drillCluster(computeOnly), "longhorn-single", 1},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := tc.mgr.EffectiveCopies(context.Background(), tc.class)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestEffectiveCopiesOfAMissingClassIsAnError(t *testing.T) {
	_, err := drillCluster().EffectiveCopies(context.Background(), "gone")
	assert.Error(t, err)
}

func drillClaimManager(robustness string, wanted int64, replicas ...runtime.Object) *Manager {
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "kipper-restore-drill", Name: "data-db-0"},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "pv-1"},
	}
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-1"},
		Spec: corev1.PersistentVolumeSpec{
			ClaimRef: &corev1.ObjectReference{Namespace: "kipper-restore-drill", Name: "data-db-0", UID: "claim-uid"},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{Driver: "driver.longhorn.io", VolumeHandle: "pvc-1"},
			},
		},
	}
	volume := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "longhorn.io/v1beta2", "kind": "Volume",
		"metadata": map[string]interface{}{"name": "pvc-1", "namespace": "longhorn-system"},
		"spec":     map[string]interface{}{"numberOfReplicas": wanted},
		"status": map[string]interface{}{"robustness": robustness, "kubernetesStatus": map[string]interface{}{
			"namespace": "kipper-restore-drill", "pvcName": "data-db-0",
		}},
	}}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{LonghornReplicaGVR: "ReplicaList", LonghornVolumeGVR: "VolumeList"},
		append([]runtime.Object{volume}, replicas...)...)
	return &Manager{Client: fake.NewSimpleClientset(claim, pv), Dynamic: dyn} //nolint:staticcheck
}

func TestClaimCopiesReadsTheVolumesRobustnessAndCurrentCopies(t *testing.T) {
	failed := placementReplica("r2", "worker-1", "")
	require.NoError(t, unstructured.SetNestedField(failed.Object, "2026-10-24T02:00:00Z", "spec", "failedAt"))
	mgr := drillClaimManager("degraded", 2, placementReplica("r1", "server-1", ""), failed)

	claim := boundDrillClaim()
	got, err := mgr.ClaimCopies(context.Background(), &claim)
	require.NoError(t, err)
	assert.Equal(t, ClaimCopies{Robustness: "degraded", Wanted: 2, Current: 1}, got)
}

// The drill counts copies as the service's copies status does: a copy that
// served writes again after a salvage holds the data.
func TestClaimCopiesCountsTheCopiesTheStatusCounts(t *testing.T) {
	served := placementReplica("r1", "server-1", "2026-10-24T02:00:00Z")
	require.NoError(t, unstructured.SetNestedField(served.Object, "2026-10-24T02:00:10Z", "spec", "lastHealthyAt"))
	mgr := drillClaimManager("healthy", 1, served)

	claim := boundDrillClaim()
	got, err := mgr.ClaimCopies(context.Background(), &claim)
	require.NoError(t, err)
	assert.Equal(t, 1, got.Current)
}

func boundDrillClaim() corev1.PersistentVolumeClaim {
	var claim corev1.PersistentVolumeClaim
	claim.Namespace, claim.Name, claim.UID, claim.Spec.VolumeName = "kipper-restore-drill", "data-db-0", "claim-uid", "pv-1"
	return claim
}

func TestClaimCopiesIgnoresAVolumeBoundToAnotherClaim(t *testing.T) {
	cases := map[string]func(*corev1.PersistentVolumeClaim, *Manager){
		"a claim of another name": func(c *corev1.PersistentVolumeClaim, _ *Manager) { c.Name = "data-other-0" },
		"an earlier claim of the same name": func(c *corev1.PersistentVolumeClaim, _ *Manager) {
			c.UID = "later-claim-uid"
		},
		"Longhorn naming another claim": func(_ *corev1.PersistentVolumeClaim, m *Manager) {
			gvr := LonghornVolumeGVR
			v, err := m.Dynamic.Resource(gvr).Namespace("longhorn-system").Get(context.Background(), "pvc-1", metav1.GetOptions{})
			if err == nil {
				_ = unstructured.SetNestedField(v.Object, "data-other-0", "status", "kubernetesStatus", "pvcName")
				_, _ = m.Dynamic.Resource(gvr).Namespace("longhorn-system").Update(context.Background(), v, metav1.UpdateOptions{})
			}
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			mgr := drillClaimManager("faulted", 1)
			claim := boundDrillClaim()
			change(&claim, mgr)
			got, err := mgr.ClaimCopies(context.Background(), &claim)
			require.NoError(t, err)
			assert.Equal(t, ClaimCopies{}, got, "a volume not proven to be this claim's says nothing about it")
		})
	}
}

func TestClaimCopiesOfAClaimOutsideLonghornIsEmpty(t *testing.T) {
	mgr := drillClaimManager("healthy", 1)
	var claim corev1.PersistentVolumeClaim
	claim.Namespace, claim.Name = "kipper-restore-drill", "unbound"
	got, err := mgr.ClaimCopies(context.Background(), &claim)
	require.NoError(t, err)
	assert.Equal(t, ClaimCopies{}, got, "an unbound claim has no Longhorn volume yet")
}
