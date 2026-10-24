package controllers

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/platform"
)

func longhornObject(kind, name string, fields map[string]interface{}) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: fields}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: "longhorn.io", Version: "v1beta2", Kind: kind})
	u.SetNamespace(longhornNamespace)
	u.SetName(name)
	return u
}

func longhornNode(name string, tags []interface{}, ready, schedulable string) *unstructured.Unstructured {
	return longhornObject("Node", name, map[string]interface{}{
		"spec": map[string]interface{}{"tags": tags, "allowScheduling": true},
		"status": map[string]interface{}{"conditions": []interface{}{
			map[string]interface{}{"type": "Ready", "status": ready},
			map[string]interface{}{"type": "Schedulable", "status": schedulable},
		}},
	})
}

func longhornVolume(name, namespace, claim string, replicas int64) *unstructured.Unstructured {
	return longhornObject("Volume", name, map[string]interface{}{
		"spec":   map[string]interface{}{"numberOfReplicas": replicas},
		"status": map[string]interface{}{"kubernetesStatus": map[string]interface{}{"namespace": namespace, "pvcName": claim}},
	})
}

func longhornReplica(name, volume, node, healthyAt, failedAt string) *unstructured.Unstructured {
	u := longhornObject("Replica", name, map[string]interface{}{
		"spec":   map[string]interface{}{"nodeID": node, "healthyAt": healthyAt, "failedAt": failedAt, "volumeName": volume},
		"status": map[string]interface{}{"currentState": "running"},
	})
	u.SetLabels(map[string]string{"longhornvolume": volume})
	return u
}

// salvaged records a failure timestamp without changing healthyAt or failedAt
// to model salvage without a rebuild.
func salvaged(replica *unstructured.Unstructured, lastFailedAt string) *unstructured.Unstructured {
	_ = unstructured.SetNestedField(replica.Object, lastFailedAt, "spec", "lastFailedAt")
	return replica
}

func detached(volume *unstructured.Unstructured) *unstructured.Unstructured {
	_ = unstructured.SetNestedField(volume.Object, "detached", "status", "state")
	return volume
}

func kubeNode(name string, ready corev1.ConditionStatus, taints ...corev1.Taint) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       corev1.NodeSpec{Taints: taints},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: ready,
			LastTransitionTime: metav1.NewTime(convNow.Add(-time.Hour))}}},
	}
}

func boundClaim(namespace, class, volume string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "data-db-0"},
		Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: ptr.To(class), VolumeName: volume},
	}
}

// testMapper serves the core and Kipper kinds the controllers read, plus
// Longhorn's when withLonghorn is set.
func testMapper(withLonghorn bool) meta.RESTMapper {
	mapper := meta.NewDefaultRESTMapper(nil)
	for _, gvk := range []schema.GroupVersionKind{
		{Version: "v1", Kind: "PersistentVolumeClaim"}, {Version: "v1", Kind: "Pod"}, {Version: "v1", Kind: "Secret"},
		{Version: "v1", Kind: "ConfigMap"}, {Version: "v1", Kind: "Service"}, {Group: "apps", Version: "v1", Kind: "StatefulSet"},
		{Group: "kipper.run", Version: "v1alpha1", Kind: "Service"}, {Group: "kipper.run", Version: "v1alpha1", Kind: "Volume"},
		{Group: "kipper.run", Version: "v1alpha1", Kind: "App"}, {Group: "coordination.k8s.io", Version: "v1", Kind: "Lease"},
	} {
		mapper.Add(gvk, meta.RESTScopeNamespace)
	}
	for _, gvk := range []schema.GroupVersionKind{
		{Version: "v1", Kind: "Node"}, {Version: "v1", Kind: "Namespace"}, {Version: "v1", Kind: "PersistentVolume"}, {Group: "kipper.run", Version: "v1alpha1", Kind: "PlatformConfig"},
		{Group: "kipper.run", Version: "v1alpha1", Kind: "Project"},
	} {
		mapper.Add(gvk, meta.RESTScopeRoot)
	}
	if withLonghorn {
		for _, kind := range []string{"Node", "Volume", "Replica", "Engine", "Setting", "VolumeAttachment"} {
			mapper.Add(schema.GroupVersionKind{Group: "longhorn.io", Version: "v1beta2", Kind: kind}, meta.RESTScopeNamespace)
		}
	}
	return mapper
}

func emptyMapper() meta.RESTMapper { return testMapper(false) }

func longhornClient(objs ...crclient.Object) crclient.Client {
	return crfake.NewClientBuilder().WithScheme(testScheme()).WithRESTMapper(testMapper(true)).WithObjects(objs...).Build()
}

func TestUsableReplicaNodesCountsOnlyTaggedReadyUnfencedNodes(t *testing.T) {
	outOfService := corev1.Taint{Key: platform.OutOfServiceTaint, Value: "nodeshutdown", Effect: corev1.TaintEffectNoExecute}
	c := longhornClient(
		longhornNode("server-1", []interface{}{"kipper-server", "kipper-replica"}, "True", "True"), kubeNode("server-1", corev1.ConditionTrue),
		longhornNode("worker-1", []interface{}{"kipper-replica"}, "True", "True"), kubeNode("worker-1", corev1.ConditionTrue),
		longhornNode("worker-2", []interface{}{}, "True", "True"), kubeNode("worker-2", corev1.ConditionTrue),
		longhornNode("worker-3", []interface{}{"kipper-replica"}, "True", "True"), kubeNode("worker-3", corev1.ConditionFalse),
		longhornNode("worker-4", []interface{}{"kipper-replica"}, "True", "True"), kubeNode("worker-4", corev1.ConditionTrue, outOfService),
		longhornNode("worker-5", []interface{}{"kipper-replica"}, "True", "False"), kubeNode("worker-5", corev1.ConditionTrue),
		longhornNode("worker-6", []interface{}{"kipper-replica"}, "True", "True"),
		longhornNode("worker-7", []interface{}{"kipper-replica"}, "False", "True"), kubeNode("worker-7", corev1.ConditionTrue),
	)
	n, err := usableReplicaNodes(context.Background(), c)
	require.NoError(t, err)
	assert.Equal(t, 2, n, "untagged, NotReady in either system, fenced, unschedulable and node-less entries do not count")
}

func TestUsableReplicaNodesIsZeroWithoutLonghorn(t *testing.T) {
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithRESTMapper(emptyMapper()).Build()
	n, err := usableReplicaNodes(context.Background(), c)
	require.NoError(t, err, "a local-path cluster has no Longhorn kinds, which is no error")
	assert.Zero(t, n)
}

func TestCopiesCapacityReportsTheOfferAndUsableNodes(t *testing.T) {
	nodes := []crclient.Object{
		longhornNode("server-1", []interface{}{"kipper-replica"}, "True", "True"), kubeNode("server-1", corev1.ConditionTrue),
		longhornNode("worker-1", []interface{}{"kipper-replica"}, "True", "True"), kubeNode("worker-1", corev1.ConditionTrue),
	}
	offer := &kipperv1.PlatformConfig{ObjectMeta: metav1.ObjectMeta{Name: PlatformConfigName},
		Spec: kipperv1.PlatformConfigSpec{MaxCopies: ptr.To[int32](2)}}

	offered, usable, err := CopiesCapacity(context.Background(), longhornClient(append(nodes, offer)...))
	require.NoError(t, err)
	assert.Equal(t, int32(2), offered)
	assert.Equal(t, 2, usable)

	offered, _, err = CopiesCapacity(context.Background(), longhornClient(nodes...))
	require.NoError(t, err)
	assert.Equal(t, int32(1), offered, "a cluster without a PlatformConfig offers one copy")
}

func TestLiveCopiesReadsTheClaimsOwnVolumeAndHealthyReplicas(t *testing.T) {
	c := longhornClient(
		longhornVolume("pvc-1", "shop", "data-db-0", 2),
		longhornReplica("r1", "pvc-1", "worker-1", "2026-10-24T01:00:00Z", ""),
		longhornReplica("r2", "pvc-1", "server-1", "2026-10-24T01:00:00Z", ""),
		longhornReplica("r3", "pvc-1", "worker-2", "2026-10-24T01:00:00Z", "2026-10-24T02:00:00Z"),
		longhornReplica("r4", "pvc-1", "worker-3", "", ""),
		longhornReplica("r5", "pvc-other", "worker-3", "2026-10-24T01:00:00Z", ""),
		kubeNode("server-1", corev1.ConditionTrue), kubeNode("worker-1", corev1.ConditionTrue),
		kubeNode("worker-2", corev1.ConditionTrue), kubeNode("worker-3", corev1.ConditionTrue),
	)
	live, err := readLiveCopies(context.Background(), c, boundClaim("shop", "longhorn-replicated", "pvc-1"))
	require.NoError(t, err)
	require.NotNil(t, live)
	assert.Equal(t, liveCopies{Copies: 2, Healthy: 2, Nodes: []string{"server-1", "worker-1"}}, *live)
}

func TestLiveCopiesCountOnlyCurrentReplicasOnAvailableHolders(t *testing.T) {
	fence := corev1.Taint{Key: platform.OutOfServiceTaint, Value: "nodeshutdown", Effect: corev1.TaintEffectNoExecute}
	healthy := "2026-10-24T01:00:00Z"
	for name, tc := range map[string]struct {
		objs []crclient.Object
		want liveCopies
	}{
		"a fenced holder": {
			[]crclient.Object{kubeNode("worker-1", corev1.ConditionTrue, fence)},
			liveCopies{Copies: 2, Healthy: 1, Nodes: []string{"server-1"}},
		},
		"a NotReady holder": {
			[]crclient.Object{kubeNode("worker-1", corev1.ConditionFalse)},
			liveCopies{Copies: 2, Healthy: 1, Nodes: []string{"server-1"}},
		},
		"a holder whose node is gone": {
			nil,
			liveCopies{Copies: 2, Healthy: 1, Nodes: []string{"server-1"}},
		},
		"an untagged but available holder still holds its copy": {
			[]crclient.Object{kubeNode("worker-1", corev1.ConditionTrue)},
			liveCopies{Copies: 2, Healthy: 2, Nodes: []string{"server-1", "worker-1"}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			objs := append([]crclient.Object{
				detached(longhornVolume("pvc-1", "shop", "data-db-0", 2)),
				longhornReplica("r1", "pvc-1", "server-1", healthy, ""),
				longhornReplica("r2", "pvc-1", "worker-1", healthy, ""),
				kubeNode("server-1", corev1.ConditionTrue),
			}, tc.objs...)
			live, err := readLiveCopies(context.Background(), longhornClient(objs...), boundClaim("shop", "longhorn-replicated", "pvc-1"))
			require.NoError(t, err)
			require.NotNil(t, live)
			assert.Equal(t, tc.want, *live)
		})
	}
}

func TestASalvagedReplicaIsNotACurrentCopy(t *testing.T) {
	c := longhornClient(
		longhornVolume("pvc-1", "shop", "data-db-0", 2),
		longhornReplica("r1", "pvc-1", "server-1", "2026-10-24T01:00:00Z", ""),
		salvaged(longhornReplica("r2", "pvc-1", "worker-1", "2026-10-24T01:00:00Z", ""), "2026-10-24T02:00:00Z"),
		kubeNode("server-1", corev1.ConditionTrue), kubeNode("worker-1", corev1.ConditionTrue),
	)
	live, err := readLiveCopies(context.Background(), c, boundClaim("shop", "longhorn-replicated", "pvc-1"))
	require.NoError(t, err)
	require.NotNil(t, live)
	assert.Equal(t, liveCopies{Copies: 2, Healthy: 1, Nodes: []string{"server-1"}}, *live)
}

// Longhorn moves only lastHealthyAt when a salvaged copy serves writes again,
// so a copy that served writes after recovering counts as healthy whether
// attached or detached. Conversion also checks for copies that may hold newer writes.
func TestASalvagedCopyThatServedAgainIsHealthy(t *testing.T) {
	for _, state := range []string{"attached", "detached"} {
		t.Run(state, func(t *testing.T) {
			volume := longhornVolume("pvc-1", "shop", "data-db-0", 1)
			_ = unstructured.SetNestedField(volume.Object, state, "status", "state")
			keeper := salvaged(longhornReplica("r1", "pvc-1", "server-1", "2026-10-25T09:22:11Z", ""), "2026-10-25T09:54:36Z")
			_ = unstructured.SetNestedField(keeper.Object, "2026-10-25T09:54:46Z", "spec", "lastHealthyAt")
			c := longhornClient(volume, keeper, kubeNode("server-1", corev1.ConditionTrue))

			live, err := readLiveCopies(context.Background(), c, boundClaim("shop", "longhorn-single", "pvc-1"))

			require.NoError(t, err)
			require.NotNil(t, live)
			assert.Equal(t, liveCopies{Copies: 1, Healthy: 1, Nodes: []string{"server-1"}}, *live)
		})
	}
}

// A detached volume that no pod has mounted yet holds no data, so its copies
// are complete, and its copies condition settles.
func TestACopyOfAVolumeNeverAttachedIsHealthy(t *testing.T) {
	c := longhornClient(
		detached(longhornVolume("pvc-1", "shop", "data-db-0", 1)),
		longhornReplica("r1", "pvc-1", "server-1", "", ""),
		kubeNode("server-1", corev1.ConditionTrue),
	)

	live, err := readLiveCopies(context.Background(), c, boundClaim("shop", "longhorn-single", "pvc-1"))

	require.NoError(t, err)
	require.NotNil(t, live)
	assert.Equal(t, liveCopies{Copies: 1, Healthy: 1, Nodes: []string{"server-1"}}, *live)
	assert.Equal(t, metav1.ConditionTrue, copiesCondition(ptr.To(int32(1)), live, "kip volume update db --copies 1").Status)
}

// Only a volume created empty counts its unmarked copies. A replica Longhorn
// reuses for a rebuild keeps its failure history, and a volume restored from a
// backup has no data in its copies until the restore completes.
func TestAnUnmarkedCopyOfAVolumeThatHadOrAwaitsDataIsNotHealthy(t *testing.T) {
	reused := longhornReplica("r1", "pvc-1", "server-1", "", "")
	_ = unstructured.SetNestedField(reused.Object, "2026-10-24T02:00:00Z", "spec", "lastFailedAt")
	_ = unstructured.SetNestedField(reused.Object, "2026-10-24T01:00:00Z", "spec", "lastHealthyAt")
	restoring := detached(longhornVolume("pvc-1", "shop", "data-db-0", 1))
	_ = unstructured.SetNestedField(restoring.Object, "s3://backups@example/?backup=backup-1&volume=pvc-0", "spec", "fromBackup")
	_ = unstructured.SetNestedField(restoring.Object, true, "status", "restoreRequired")
	for name, objs := range map[string][]crclient.Object{
		"reused for a rebuild": {detached(longhornVolume("pvc-1", "shop", "data-db-0", 1)), reused},
		"restore pending":      {restoring, longhornReplica("r1", "pvc-1", "server-1", "", "")},
	} {
		t.Run(name, func(t *testing.T) {
			c := longhornClient(append(objs, kubeNode("server-1", corev1.ConditionTrue))...)
			live, err := readLiveCopies(context.Background(), c, boundClaim("shop", "longhorn-single", "pvc-1"))
			require.NoError(t, err)
			require.NotNil(t, live)
			assert.Equal(t, liveCopies{Copies: 1}, *live)
		})
	}
}

func TestTwoReplicasOnOneNodeAreOneCopy(t *testing.T) {
	c := longhornClient(
		longhornVolume("pvc-1", "shop", "data-db-0", 2),
		longhornReplica("r1", "pvc-1", "server-1", "2026-10-24T01:00:00Z", ""),
		longhornReplica("r2", "pvc-1", "server-1", "2026-10-24T01:00:00Z", ""),
		kubeNode("server-1", corev1.ConditionTrue),
	)
	live, err := readLiveCopies(context.Background(), c, boundClaim("shop", "longhorn-replicated", "pvc-1"))
	require.NoError(t, err)
	require.NotNil(t, live)
	assert.Equal(t, liveCopies{Copies: 2, Healthy: 1, Nodes: []string{"server-1"}}, *live, "losing that node loses both")
}

func TestAnUnreadableHolderIsUnknown(t *testing.T) {
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithRESTMapper(testMapper(true)).
		WithObjects(
			longhornVolume("pvc-1", "shop", "data-db-0", 2),
			longhornReplica("r1", "pvc-1", "server-1", "2026-10-24T01:00:00Z", ""),
			longhornReplica("r2", "pvc-1", "worker-1", "2026-10-24T01:00:00Z", ""),
			kubeNode("server-1", corev1.ConditionTrue), kubeNode("worker-1", corev1.ConditionTrue),
		).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c crclient.WithWatch, key crclient.ObjectKey, obj crclient.Object, opts ...crclient.GetOption) error {
				if _, node := obj.(*corev1.Node); node && key.Name == "worker-1" {
					return apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, key.Name, nil)
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).Build()
	live, err := readLiveCopies(context.Background(), c, boundClaim("shop", "longhorn-replicated", "pvc-1"))
	require.NoError(t, err)
	require.NotNil(t, live)
	assert.Equal(t, liveCopies{Copies: 2, Healthy: 1, Unknown: 1, Nodes: []string{"server-1"}}, *live)

	cond := copiesCondition(ptr.To(int32(2)), live, "kip service update db --copies 1|2")
	assert.Equal(t, metav1.ConditionUnknown, cond.Status)
	assert.Equal(t, kipperv1.StorageCopiesUnknown, cond.Reason)
	assert.Contains(t, cond.Message, "1 of 2 copies are healthy")
	assert.Contains(t, cond.Message, "could not read node information for 1 copy")
}

func TestLiveCopiesRefusesAVolumeThatBelongsToAnotherClaim(t *testing.T) {
	c := longhornClient(longhornVolume("pvc-1", "other", "data-db-0", 2))
	live, err := readLiveCopies(context.Background(), c, boundClaim("shop", "longhorn-single", "pvc-1"))
	require.NoError(t, err)
	assert.Nil(t, live, "a volume whose Kubernetes status names another claim is never reported")
}

func TestLiveCopiesIsUnknownForAnUnboundOrMissingVolume(t *testing.T) {
	c := longhornClient()
	live, err := readLiveCopies(context.Background(), c, boundClaim("shop", "longhorn-single", ""))
	require.NoError(t, err)
	assert.Nil(t, live)
	live, err = readLiveCopies(context.Background(), c, boundClaim("shop", "longhorn-single", "pvc-gone"))
	require.NoError(t, err)
	assert.Nil(t, live)
}

func TestLiveCopiesOfALocalPathClaimIsOneCopy(t *testing.T) {
	live, err := readLiveCopies(context.Background(), longhornClient(), boundClaim("shop", "local-path", "pvc-1"))
	require.NoError(t, err)
	require.NotNil(t, live)
	assert.Equal(t, liveCopies{Copies: 1, Healthy: 1}, *live)
}

func TestUnsetCopiesAboveWhatKipperSetsAreKeptUntilCopiesAreSet(t *testing.T) {
	c := copiesCondition(nil, &liveCopies{Copies: 3, Healthy: 3}, "kip volume update files --copies 1|2")
	assert.Equal(t, kipperv1.StorageCopiesNotSet, c.Reason)
	assert.Contains(t, c.Message, "Longhorn keeps 3 copies (3 healthy), more than Kipper sets")
	assert.Contains(t, c.Message, "'kip volume update files --copies 1|2' converts it", "setting copies converts a three-copy volume")
}

func TestCopiesCondition(t *testing.T) {
	two := ptr.To(int32(2))
	one := ptr.To(int32(1))
	for name, tc := range map[string]struct {
		wanted *int32
		live   *liveCopies
		status metav1.ConditionStatus
		reason string
		says   string
	}{
		"unset reports the live copies":     {nil, &liveCopies{Copies: 2, Healthy: 1}, metav1.ConditionUnknown, kipperv1.StorageCopiesNotSet, "Longhorn keeps 2 copies (1 healthy). Set them with 'kip service update db"},
		"unset and not provisioned":         {nil, nil, metav1.ConditionUnknown, kipperv1.StorageCopiesNotSet, "kip service update db --copies"},
		"not provisioned yet":               {two, nil, metav1.ConditionUnknown, kipperv1.StorageCopiesPending, "not provisioned"},
		"healthy":                           {two, &liveCopies{Copies: 2, Healthy: 2}, metav1.ConditionTrue, kipperv1.StorageCopiesHealthy, "2 of 2"},
		"degraded":                          {two, &liveCopies{Copies: 2, Healthy: 1}, metav1.ConditionFalse, kipperv1.StorageCopiesDegraded, "1 of 2"},
		"a changed value not converted yet": {two, &liveCopies{Copies: 1, Healthy: 1}, metav1.ConditionFalse, kipperv1.StorageCopiesConversionPending, "and the volume has 1"},
		"one copy":                          {one, &liveCopies{Copies: 1, Healthy: 1}, metav1.ConditionTrue, kipperv1.StorageCopiesHealthy, "1 of 1"},
	} {
		t.Run(name, func(t *testing.T) {
			c := copiesCondition(tc.wanted, tc.live, "kip service update db --copies 1|2")
			assert.Equal(t, kipperv1.ConditionStorageCopies, c.Type)
			assert.Equal(t, tc.status, c.Status)
			assert.Equal(t, tc.reason, c.Reason)
			assert.Contains(t, c.Message, tc.says)
		})
	}
}

func TestUnsetCopiesNameOneCopyInTheSingular(t *testing.T) {
	c := copiesCondition(nil, &liveCopies{Copies: 1, Healthy: 1}, "kip service update db --copies 1|2")
	assert.Contains(t, c.Message, "Longhorn keeps 1 copy (1 healthy).")
	c = copiesCondition(nil, &liveCopies{Copies: 2, Healthy: 1}, "kip service update db --copies 1|2")
	assert.Contains(t, c.Message, "Longhorn keeps 2 copies (1 healthy).")
}

// Extra healthy copies, such as a server copy kept after cancellation, count
// as healthy and appear in the status message.
func TestMoreHealthyCopiesThanSetAreHealthy(t *testing.T) {
	cond := copiesCondition(ptr.To(int32(2)), &liveCopies{Copies: 2, Healthy: 3}, "kip service update db --copies 1|2")
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, kipperv1.StorageCopiesHealthy, cond.Reason)
	assert.Equal(t, "3 copies are healthy, more than the 2 set.", cond.Message)
}
