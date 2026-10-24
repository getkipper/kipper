package controllers

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/platform"
)

// lhDiskNode is a Longhorn node with one filesystem disk of 100 GiB, of
// which available bytes are free.
func lhDiskNode(name string, available int64, tags ...interface{}) *unstructured.Unstructured {
	n := longhornNode(name, tags, "True", "True")
	_ = unstructured.SetNestedField(n.Object, map[string]interface{}{"disk-1": map[string]interface{}{
		"allowScheduling": true, "storageReserved": int64(0), "diskType": "filesystem",
	}}, "spec", "disks")
	_ = unstructured.SetNestedField(n.Object, map[string]interface{}{"disk-1": map[string]interface{}{
		"storageMaximum": 100 * gib, "storageScheduled": int64(0), "storageAvailable": available,
		"conditions": []interface{}{map[string]interface{}{"type": "Schedulable", "status": "True"}},
	}}, "status", "diskStatus")
	return n
}

func lhSetting(name, value string) *unstructured.Unstructured {
	return longhornObject("Setting", name, map[string]interface{}{"value": value})
}

// lhVolumeObj is an attached Longhorn volume of 10 GiB, 1 GiB used, for the
// claim ns/data-db-0.
func lhVolumeObj(name string, count int64, selector ...interface{}) *unstructured.Unstructured {
	v := longhornVolume(name, "ns", "data-db-0", count)
	spec := v.Object["spec"].(map[string]interface{})
	spec["nodeSelector"] = selector
	spec["dataLocality"] = "disabled"
	spec["replicaAutoBalance"] = "disabled"
	spec["size"] = "10737418240"
	spec["image"] = "engine:v1"
	status := v.Object["status"].(map[string]interface{})
	status["state"] = "attached"
	status["currentImage"] = "engine:v1"
	status["actualSize"] = 1 * gib
	return v
}

// lhEngine is pvc-1's active engine with the given replica modes.
func lhEngine(modes map[string]interface{}) *unstructured.Unstructured {
	e := longhornObject("Engine", "pvc-1-e-0", map[string]interface{}{
		"spec":   map[string]interface{}{"active": true, "volumeName": "pvc-1"},
		"status": map[string]interface{}{"replicaModeMap": modes},
	})
	e.SetLabels(map[string]string{"longhornvolume": "pvc-1"})
	return e
}

const healthyTime = "2026-10-24T10:00:00Z"

// convWorld is a server and one worker with room, on Longhorn 1.11.
func convWorld(objs ...crclient.Object) []crclient.Object {
	return append([]crclient.Object{
		lhDiskNode("server-1", 90*gib, platform.ServerStorageTag, platform.ReplicaStorageTag), kubeNode("server-1", corev1.ConditionTrue),
		lhDiskNode("worker-1", 90*gib, platform.ReplicaStorageTag), kubeNode("worker-1", corev1.ConditionTrue),
		lhSetting("current-longhorn-version", "v1.11.3"),
		lhSetting("storage-over-provisioning-percentage", "100"),
		lhSetting("storage-minimal-available-percentage", "25"),
	}, objs...)
}

func converterFor(objs ...crclient.Object) (storageConverter, crclient.Client) {
	c := longhornClient(objs...)
	return storageConverter{Reader: c, Writer: c, Now: func() time.Time { return convNow }}, c
}

func getLonghorn(t *testing.T, c crclient.Client, kind, name string) *unstructured.Unstructured {
	t.Helper()
	obj := longhornObject(kind, name, map[string]interface{}{})
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: longhornNamespace, Name: name}, obj))
	return obj
}

func conversionLease(t *testing.T, c crclient.Client) *coordinationv1.Lease {
	t.Helper()
	var lease coordinationv1.Lease
	err := c.Get(context.Background(), types.NamespacedName{Namespace: conversionLeaseNamespace, Name: conversionLeaseName}, &lease)
	if apierrors.IsNotFound(err) {
		return nil
	}
	require.NoError(t, err)
	return &lease
}

func heldLease(holder string, renewed time.Time) *coordinationv1.Lease {
	return &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Namespace: conversionLeaseNamespace, Name: conversionLeaseName},
		Spec: coordinationv1.LeaseSpec{HolderIdentity: ptr.To(holder), LeaseDurationSeconds: ptr.To(int32(conversionLeaseSeconds)),
			RenewTime: ptr.To(metav1.NewMicroTime(renewed))},
	}
}

func oneServerCopy() []crclient.Object {
	return []crclient.Object{
		lhVolumeObj("pvc-1", 1, platform.ServerStorageTag),
		longhornReplica("r-s", "pvc-1", "server-1", healthyTime, ""),
		lhEngine(map[string]interface{}{"r-s": "RW"}),
	}
}

func TestTheConverterRaisesTheCopiesAndTakesTheLease(t *testing.T) {
	sc, c := converterFor(convWorld(oneServerCopy()...)...)
	cond, err := sc.convert(context.Background(), boundClaim("ns", platform.LonghornReplicatedClass, "pvc-1"), ptr.To(int32(2)))
	require.NoError(t, err)
	require.NotNil(t, cond)
	assert.Equal(t, kipperv1.StorageCopiesConverting, cond.Reason)

	v := getLonghorn(t, c, "Volume", "pvc-1")
	count, _, _ := unstructured.NestedInt64(v.Object, "spec", "numberOfReplicas")
	selector, _, _ := unstructured.NestedStringSlice(v.Object, "spec", "nodeSelector")
	offline, _, _ := unstructured.NestedString(v.Object, "spec", "offlineRebuilding")
	assert.Equal(t, int64(2), count)
	assert.Equal(t, []string{platform.ReplicaStorageTag}, selector)
	assert.Equal(t, "enabled", offline)
	assert.Equal(t, "2026-10-24T12:00:00Z", v.GetAnnotations()[copiesRebuildAnnotation])
	lease := conversionLease(t, c)
	require.NotNil(t, lease)
	assert.Equal(t, "pvc-1", *lease.Spec.HolderIdentity)
}

func TestTheConverterLeavesOutOfflineRebuildingOnLonghornOneSeven(t *testing.T) {
	objs := convWorld(oneServerCopy()...)
	objs[4] = lhSetting("current-longhorn-version", "v1.7.3")
	sc, c := converterFor(objs...)
	_, err := sc.convert(context.Background(), boundClaim("ns", platform.LonghornReplicatedClass, "pvc-1"), ptr.To(int32(2)))
	require.NoError(t, err)
	_, found, _ := unstructured.NestedString(getLonghorn(t, c, "Volume", "pvc-1").Object, "spec", "offlineRebuilding")
	assert.False(t, found)
}

func TestAConversionWaitsForAnotherVolumesLease(t *testing.T) {
	sc, c := converterFor(convWorld(append(oneServerCopy(), heldLease("pvc-other", convNow.Add(-time.Minute)))...)...)
	cond, err := sc.convert(context.Background(), boundClaim("ns", platform.LonghornReplicatedClass, "pvc-1"), ptr.To(int32(2)))
	require.NoError(t, err)
	require.NotNil(t, cond)
	assert.Equal(t, kipperv1.StorageCopiesConversionPending, cond.Reason)
	count, _, _ := unstructured.NestedInt64(getLonghorn(t, c, "Volume", "pvc-1").Object, "spec", "numberOfReplicas")
	assert.Equal(t, int64(1), count, "nothing is asked of Longhorn without the lease")
}

// An expired lease is taken over only once the former holder's
// volume has no Kipper rebuild inside its budget.
func TestAnExpiredLeaseIsTakenOverOnlyWhenItsHolderIsIdle(t *testing.T) {
	expired := convNow.Add(-time.Hour)
	other := lhVolumeObj("pvc-other", 2, platform.ReplicaStorageTag)
	other.SetAnnotations(map[string]string{copiesRebuildAnnotation: convNow.Add(-15 * time.Minute).Format(time.RFC3339)})
	other.Object["status"].(map[string]interface{})["kubernetesStatus"] = map[string]interface{}{"namespace": "ns2", "pvcName": "data-db-0"}

	sc, _ := converterFor(convWorld(append(oneServerCopy(), heldLease("pvc-other", expired), other)...)...)
	cond, err := sc.convert(context.Background(), boundClaim("ns", platform.LonghornReplicatedClass, "pvc-1"), ptr.To(int32(2)))
	require.NoError(t, err)
	assert.Equal(t, kipperv1.StorageCopiesConversionPending, cond.Reason, "the holder's rebuild is still inside its budget")

	other.SetAnnotations(map[string]string{copiesRebuildAnnotation: convNow.Add(-2 * time.Hour).Format(time.RFC3339)})
	sc, c := converterFor(convWorld(append(oneServerCopy(), heldLease("pvc-other", expired), other)...)...)
	cond, err = sc.convert(context.Background(), boundClaim("ns", platform.LonghornReplicatedClass, "pvc-1"), ptr.To(int32(2)))
	require.NoError(t, err)
	assert.Equal(t, kipperv1.StorageCopiesConverting, cond.Reason)
	assert.Equal(t, "pvc-1", *conversionLease(t, c).Spec.HolderIdentity)

	sc, _ = converterFor(convWorld(append(oneServerCopy(), heldLease("pvc-gone", expired))...)...)
	cond, err = sc.convert(context.Background(), boundClaim("ns", platform.LonghornReplicatedClass, "pvc-1"), ptr.To(int32(2)))
	require.NoError(t, err)
	assert.Equal(t, kipperv1.StorageCopiesConverting, cond.Reason, "a deleted holder volume has no rebuild")
}

func TestAFinishedConversionReleasesTheLease(t *testing.T) {
	v := lhVolumeObj("pvc-1", 2, platform.ReplicaStorageTag)
	v.Object["spec"].(map[string]interface{})["offlineRebuilding"] = "enabled"
	v.SetAnnotations(map[string]string{copiesRebuildAnnotation: convNow.Add(-10 * time.Minute).Format(time.RFC3339)})
	sc, c := converterFor(convWorld(v,
		longhornReplica("r-s", "pvc-1", "server-1", healthyTime, ""), longhornReplica("r-w", "pvc-1", "worker-1", healthyTime, ""),
		lhEngine(map[string]interface{}{"r-s": "RW", "r-w": "RW"}), heldLease("pvc-1", convNow.Add(-time.Minute)))...)
	cond, err := sc.convert(context.Background(), boundClaim("ns", platform.LonghornReplicatedClass, "pvc-1"), ptr.To(int32(2)))
	require.NoError(t, err)
	assert.Nil(t, cond, "a finished volume reports its live copies")
	assert.NotContains(t, getLonghorn(t, c, "Volume", "pvc-1").GetAnnotations(), copiesRebuildAnnotation)
	assert.Nil(t, conversionLease(t, c).Spec.HolderIdentity)
}

func TestTheConverterDeletesTheWorkerCopyOnRollback(t *testing.T) {
	sc, c := converterFor(convWorld(lhVolumeObj("pvc-1", 1, platform.ReplicaStorageTag),
		longhornReplica("r-s", "pvc-1", "server-1", healthyTime, ""), longhornReplica("r-w", "pvc-1", "worker-1", healthyTime, ""),
		lhEngine(map[string]interface{}{"r-s": "RW", "r-w": "RW"}))...)
	cond, err := sc.convert(context.Background(), boundClaim("ns", platform.LonghornReplicatedClass, "pvc-1"), ptr.To(int32(1)))
	require.NoError(t, err)
	assert.Equal(t, kipperv1.StorageCopiesConverting, cond.Reason)
	err = c.Get(context.Background(), types.NamespacedName{Namespace: longhornNamespace, Name: "r-w"}, longhornObject("Replica", "r-w", map[string]interface{}{}))
	assert.True(t, apierrors.IsNotFound(err), "the worker copy is deleted")
	getLonghorn(t, c, "Replica", "r-s")
}

func TestTheConverterLeavesUnmanagedAndForeignVolumesAlone(t *testing.T) {
	sc, c := converterFor(convWorld(oneServerCopy()...)...)
	cond, err := sc.convert(context.Background(), boundClaim("ns", platform.LonghornReplicatedClass, "pvc-1"), nil)
	require.NoError(t, err)
	assert.Nil(t, cond)
	foreign := boundClaim("other-ns", platform.LonghornReplicatedClass, "pvc-1")
	cond, err = sc.convert(context.Background(), foreign, ptr.To(int32(2)))
	require.NoError(t, err)
	assert.Nil(t, cond)
	local := boundClaim("ns", platform.LocalPathClass, "pvc-1")
	cond, err = sc.convert(context.Background(), local, ptr.To(int32(2)))
	require.NoError(t, err)
	assert.Nil(t, cond)
	count, _, _ := unstructured.NestedInt64(getLonghorn(t, c, "Volume", "pvc-1").Object, "spec", "numberOfReplicas")
	assert.Equal(t, int64(1), count)
	assert.Nil(t, conversionLease(t, c))
}

func TestADumpClaimReservesRoomOnTheServer(t *testing.T) {
	workerOnly := []crclient.Object{
		lhVolumeObj("pvc-1", 1),
		longhornReplica("r-w", "pvc-1", "worker-1", healthyTime, ""),
		lhEngine(map[string]interface{}{"r-w": "RW"}),
	}
	objs := convWorld(workerOnly...)
	objs[0] = lhDiskNode("server-1", 35*gib, platform.ServerStorageTag, platform.ReplicaStorageTag)
	sc, _ := converterFor(objs...)
	cond, err := sc.convert(context.Background(), boundClaim("ns", platform.LonghornReplicatedClass, "pvc-1"), ptr.To(int32(1)))
	require.NoError(t, err)
	assert.Equal(t, kipperv1.StorageCopiesConverting, cond.Reason, "35 - 1 GiB is above the 25 GiB floor")

	dump := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "db-dump", Labels: map[string]string{dumpForLabel: "db"}},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "pvc-dump"},
	}
	dumpVolume := lhVolumeObj("pvc-dump", 1, platform.ServerStorageTag)
	dumpVolume.Object["status"].(map[string]interface{})["kubernetesStatus"] = map[string]interface{}{"namespace": "ns", "pvcName": "db-dump"}
	sc, c := converterFor(append(objs, dump, dumpVolume)...)
	cond, err = sc.convert(context.Background(), boundClaim("ns", platform.LonghornReplicatedClass, "pvc-1"), ptr.To(int32(1)))
	require.NoError(t, err)
	assert.Equal(t, kipperv1.StorageCopiesBlocked, cond.Reason, "the dump may grow by 9 GiB")
	assert.Nil(t, conversionLease(t, c))
}

// dbDataClaim is db's data claim as the StatefulSet controller creates it,
// labeled with the StatefulSet's selector.
func dbDataClaim() *corev1.PersistentVolumeClaim {
	pvc := boundClaim("ns", platform.LonghornReplicatedClass, "pvc-1")
	pvc.Labels = map[string]string{"app": "db"}
	return pvc
}

func TestAServiceReportsItsRunningConversion(t *testing.T) {
	svc := copiesService(1, false)
	r := copiesReconciler(convWorld(svc, dbDataClaim(),
		lhVolumeObj("pvc-1", 2, platform.ReplicaStorageTag),
		longhornReplica("r-s", "pvc-1", "server-1", healthyTime, ""), longhornReplica("r-w", "pvc-1", "worker-1", healthyTime, ""),
		lhEngine(map[string]interface{}{"r-s": "RW", "r-w": "RW"}))...)
	ctx := context.Background()
	require.NoError(t, r.reconcileStatefulSet(ctx, svc))
	require.NoError(t, r.updateStatus(ctx, svc))
	var got kipperv1.Service
	require.NoError(t, r.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "db"}, &got))
	c := meta.FindStatusCondition(got.Status.Conditions, kipperv1.ConditionStorageCopies)
	require.NotNil(t, c)
	assert.Equal(t, kipperv1.StorageCopiesConverting, c.Reason)
	assert.Contains(t, c.Message, "Lowering the copy count to 1")
	assert.Equal(t, storageRefreshConverting, storageRefresh(c))
	count, _, _ := unstructured.NestedInt64(getLonghorn(t, r.Client, "Volume", "pvc-1").Object, "spec", "numberOfReplicas")
	assert.Equal(t, int64(1), count)
}

func TestASharedVolumeIsNotConvertedToTwoCopies(t *testing.T) {
	sc, c := converterFor(convWorld(oneServerCopy()...)...)
	cond, err := convertShared(context.Background(), sc, boundClaim("ns", platform.LonghornSharedClass, "pvc-1"), ptr.To(int32(2)))
	require.NoError(t, err)
	assert.Nil(t, cond)
	count, _, _ := unstructured.NestedInt64(getLonghorn(t, c, "Volume", "pvc-1").Object, "spec", "numberOfReplicas")
	assert.Equal(t, int64(1), count)
}

// Copies are converted only on a claim the object owns. A claim
// that merely shares the name is reported and never written.
func TestAServiceNeverConvertsAClaimItDoesNotOwn(t *testing.T) {
	foreignController := dbDataClaim()
	foreignController.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "apps/v1", Kind: "StatefulSet", Name: "other", UID: "uid-other", Controller: ptr.To(true),
	}}
	for name, claim := range map[string]*corev1.PersistentVolumeClaim{
		"unlabeled":                   boundClaim("ns", platform.LonghornReplicatedClass, "pvc-1"),
		"controlled by another owner": foreignController,
	} {
		t.Run(name, func(t *testing.T) {
			svc := copiesService(1, false)
			r := copiesReconciler(convWorld(svc, claim,
				lhVolumeObj("pvc-1", 2, platform.ReplicaStorageTag),
				longhornReplica("r-s", "pvc-1", "server-1", healthyTime, ""), longhornReplica("r-w", "pvc-1", "worker-1", healthyTime, ""),
				lhEngine(map[string]interface{}{"r-s": "RW", "r-w": "RW"}))...)
			ctx := context.Background()
			require.NoError(t, r.reconcileStatefulSet(ctx, svc))
			require.NoError(t, r.updateStatus(ctx, svc))
			var got kipperv1.Service
			require.NoError(t, r.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "db"}, &got))
			c := meta.FindStatusCondition(got.Status.Conditions, kipperv1.ConditionStorageCopies)
			require.NotNil(t, c)
			assert.NotEqual(t, kipperv1.StorageCopiesConverting, c.Reason)
			count, _, _ := unstructured.NestedInt64(getLonghorn(t, r.Client, "Volume", "pvc-1").Object, "spec", "numberOfReplicas")
			assert.Equal(t, int64(2), count, "nothing is written to a volume the service does not own")
			getLonghorn(t, r.Client, "Replica", "r-w")
			assert.Nil(t, conversionLease(t, r.Client))
		})
	}
}

func TestDumpClaimsAndPlatformNamespacesAreNeverConverted(t *testing.T) {
	dump := boundClaim("ns", platform.LonghornReplicatedClass, "pvc-1")
	dump.Labels = map[string]string{dumpForLabel: "db"}
	drill := boundClaim("kipper-restore-drill", platform.LonghornReplicatedClass, "pvc-1")
	system := boundClaim("kipper-system", platform.LonghornReplicatedClass, "pvc-1")
	for _, claim := range []*corev1.PersistentVolumeClaim{dump, drill, system} {
		sc, c := converterFor(convWorld(oneServerCopy()...)...)
		cond, err := sc.convert(context.Background(), claim, ptr.To(int32(2)))
		require.NoError(t, err)
		assert.Nil(t, cond)
		count, _, _ := unstructured.NestedInt64(getLonghorn(t, c, "Volume", "pvc-1").Object, "spec", "numberOfReplicas")
		assert.Equal(t, int64(1), count, "claim %s/%s", claim.Namespace, claim.Name)
	}
}

func TestOnlyAProjectsOwnSharedClaimIsItsToConvert(t *testing.T) {
	claim := func(extra map[string]string) *corev1.PersistentVolumeClaim {
		l := map[string]string{kipperLabel: kipperValue, "kipper.run/resource-type": "shared-volume"}
		for k, v := range extra {
			l[k] = v
		}
		return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "shared-uploads", Namespace: "shop-test", Labels: l}}
	}
	assert.True(t, projectSharedClaim(claim(nil)))
	assert.False(t, projectSharedClaim(claim(map[string]string{"kipper.run/volume-name": "uploads"})),
		"a Volume named uploads owns its shared-uploads claim, the project does not")
	controlled := claim(nil)
	controlled.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: "x", UID: "u", Controller: ptr.To(true)}}
	assert.False(t, projectSharedClaim(controlled))
	assert.False(t, projectSharedClaim(&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "shared-uploads"}}))
}

// A running conversion in any environment makes the project's
// reason Converting, whatever the order, so admission refuses a change.
func TestAProjectShowsAConversionRunningInAnyEnvironment(t *testing.T) {
	converting := metav1.Condition{Status: metav1.ConditionFalse, Reason: kipperv1.StorageCopiesConverting, Message: "Building a copy on the server."}
	blocked := metav1.Condition{Status: metav1.ConditionFalse, Reason: kipperv1.StorageCopiesBlocked, Message: "The volume is not attached."}
	healthy := metav1.Condition{Status: metav1.ConditionTrue, Reason: kipperv1.StorageCopiesHealthy, Message: "1 of 1 copies are healthy."}
	for _, envs := range [][]envCopies{
		{{"test", blocked}, {"prod", converting}},
		{{"prod", converting}, {"test", blocked}},
		{{"test", healthy}, {"prod", converting}, {"acc", blocked}},
	} {
		got := projectCopiesCondition(envs)
		assert.Equal(t, kipperv1.StorageCopiesConverting, got.Reason)
		assert.Contains(t, got.Message, "The volume is not attached.")
	}
	assert.Equal(t, kipperv1.StorageCopiesBlocked, projectCopiesCondition([]envCopies{{"test", healthy}, {"prod", blocked}}).Reason)
}

func TestAVolumeConvertsOnlyItsOwnClaim(t *testing.T) {
	vol := &kipperv1.Volume{ObjectMeta: metav1.ObjectMeta{Name: "uploads", Namespace: "ns"}, Spec: kipperv1.VolumeSpec{Copies: ptr.To(int32(1))}}
	for name, labels := range map[string]map[string]string{
		"its own":    {kipperLabel: kipperValue, "kipper.run/resource-type": "shared-volume", "kipper.run/volume-name": "uploads"},
		"a stranger": {},
	} {
		t.Run(name, func(t *testing.T) {
			claim := boundClaim("ns", platform.LonghornSharedClass, "pvc-1")
			claim.Labels = labels
			objs := convWorld(lhVolumeObj("pvc-1", 2, platform.ReplicaStorageTag),
				longhornReplica("r-s", "pvc-1", "server-1", healthyTime, ""), longhornReplica("r-w", "pvc-1", "worker-1", healthyTime, ""),
				lhEngine(map[string]interface{}{"r-s": "RW", "r-w": "RW"}))
			c := longhornClient(objs...)
			r := &VolumeReconciler{Client: c, APIReader: c, Scheme: testScheme()}
			require.NoError(t, r.reportStorageCopies(context.Background(), vol, claim))
			count, _, _ := unstructured.NestedInt64(getLonghorn(t, c, "Volume", "pvc-1").Object, "spec", "numberOfReplicas")
			if name == "its own" {
				assert.Equal(t, int64(1), count)
			} else {
				assert.Equal(t, int64(2), count)
			}
		})
	}
}

// The delete re-reads the volume and plans again, so a keeper that failed
// since the first read stops the delete.
func TestADeleteIsDroppedWhenTheKeeperFailsBeforeIt(t *testing.T) {
	replicaLists := 0
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithRESTMapper(testMapper(true)).
		WithObjects(convWorld(lhVolumeObj("pvc-1", 1, platform.ReplicaStorageTag),
			longhornReplica("r-s", "pvc-1", "server-1", healthyTime, ""), longhornReplica("r-w", "pvc-1", "worker-1", healthyTime, ""),
			lhEngine(map[string]interface{}{"r-s": "RW", "r-w": "RW"}))...).
		WithInterceptorFuncs(interceptor.Funcs{List: func(ctx context.Context, cl crclient.WithWatch, list crclient.ObjectList, opts ...crclient.ListOption) error {
			if err := cl.List(ctx, list, opts...); err != nil {
				return err
			}
			u, ok := list.(*unstructured.UnstructuredList)
			if !ok || u.GetKind() != "ReplicaList" {
				return nil
			}
			if replicaLists++; replicaLists > 1 {
				for i := range u.Items {
					if u.Items[i].GetName() == "r-s" {
						_ = unstructured.SetNestedField(u.Items[i].Object, "2026-10-24T11:59:00Z", "spec", "failedAt")
					}
				}
			}
			return nil
		}}).Build()
	sc := storageConverter{Reader: c, Writer: c, Now: func() time.Time { return convNow }}
	_, err := sc.convert(context.Background(), boundClaim("ns", platform.LonghornReplicatedClass, "pvc-1"), ptr.To(int32(1)))
	require.NoError(t, err)
	assert.Equal(t, 2, replicaLists, "the delete read the replicas again")
	getLonghorn(t, c, "Replica", "r-w")
}

// rollbackService creates a reconciler for svc with a volume reduced to a
// requested count of one and both replicas still present. The fail interceptors
// control API failures.
func rollbackService(svc *kipperv1.Service, fail interceptor.Funcs) *ServiceReconciler {
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithRESTMapper(testMapper(true)).
		WithObjects(convWorld(svc, dbDataClaim(), lhVolumeObj("pvc-1", 1, platform.ReplicaStorageTag),
			longhornReplica("r-s", "pvc-1", "server-1", healthyTime, ""), longhornReplica("r-w", "pvc-1", "worker-1", healthyTime, ""),
			lhEngine(map[string]interface{}{"r-s": "RW", "r-w": "RW"}))...).
		WithStatusSubresource(&kipperv1.Service{}).WithInterceptorFuncs(fail).Build()
	return &ServiceReconciler{Client: c, APIReader: c, Scheme: testScheme()}
}

func failReplicaDeletes(err error) interceptor.Funcs {
	return interceptor.Funcs{Delete: func(ctx context.Context, cl crclient.WithWatch, obj crclient.Object, opts ...crclient.DeleteOption) error {
		if obj.GetObjectKind().GroupVersionKind().Kind == "Replica" {
			return err
		}
		return cl.Delete(ctx, obj, opts...)
	}}
}

func copiesConditionOf(t *testing.T, r *ServiceReconciler) *metav1.Condition {
	t.Helper()
	var got kipperv1.Service
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db"}, &got))
	return meta.FindStatusCondition(got.Status.Conditions, kipperv1.ConditionStorageCopies)
}

// Reporting Blocked lets users reset the copy count through kip and admission
// policy while the API keeps rejecting the conversion step.
func TestAConversionStepThatKeepsFailingReportsBlocked(t *testing.T) {
	forbidden := apierrors.NewForbidden(schema.GroupResource{Group: "longhorn.io", Resource: "replicas"}, "r-w", fmt.Errorf("denied by kipper-longhorn-server-replicas"))
	svc := copiesService(1, false)
	r := rollbackService(svc, failReplicaDeletes(forbidden))
	ctx := context.Background()
	require.NoError(t, r.reconcileStatefulSet(ctx, svc))

	require.NoError(t, r.updateStatus(ctx, svc))

	c := copiesConditionOf(t, r)
	require.NotNil(t, c)
	assert.Equal(t, kipperv1.StorageCopiesBlocked, c.Reason)
	assert.Contains(t, c.Message, "could not take the next step")
	assert.Contains(t, c.Message, "denied by kipper-longhorn-server-replicas")
	getLonghorn(t, r.Client, "Replica", "r-w")
}

// Conflicts are retriable, so they return an error and leave the condition unchanged.
func TestAConflictInAConversionStepIsRetried(t *testing.T) {
	conflict := apierrors.NewConflict(schema.GroupResource{Group: "longhorn.io", Resource: "volumes"}, "pvc-1", fmt.Errorf("the object has been modified"))
	svc := copiesService(1, false)
	r := rollbackService(svc, interceptor.Funcs{Patch: func(ctx context.Context, cl crclient.WithWatch, obj crclient.Object, patch crclient.Patch, opts ...crclient.PatchOption) error {
		if obj.GetObjectKind().GroupVersionKind().Kind == "Volume" {
			return conflict
		}
		return cl.Patch(ctx, obj, patch, opts...)
	}})
	lhVolume := getLonghorn(t, r.Client, "Volume", "pvc-1")
	_ = unstructured.SetNestedField(lhVolume.Object, "best-effort", "spec", "replicaAutoBalance")
	require.NoError(t, r.Update(context.Background(), lhVolume))
	ctx := context.Background()
	require.NoError(t, r.reconcileStatefulSet(ctx, svc))

	err := r.updateStatus(ctx, svc)

	require.Error(t, err)
	assert.True(t, apierrors.IsConflict(err))
	assert.Nil(t, copiesConditionOf(t, r))
}

// patchFailing is a converter for a one-copy volume whose patches go through fail.
func patchFailing(fail func(ctx context.Context, cl crclient.WithWatch, obj crclient.Object, patch crclient.Patch) error) storageConverter {
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithRESTMapper(testMapper(true)).
		WithObjects(convWorld(oneServerCopy()...)...).
		WithInterceptorFuncs(interceptor.Funcs{Patch: func(ctx context.Context, cl crclient.WithWatch, obj crclient.Object, patch crclient.Patch, opts ...crclient.PatchOption) error {
			return fail(ctx, cl, obj, patch)
		}}).Build()
	return storageConverter{Reader: c, Writer: c, Now: func() time.Time { return convNow }}
}

// A rebuild patch may succeed before its response is lost, or another pass's
// patch may succeed later. Keeping the lease until it expires covers both cases.
func TestAFailedRebuildPatchKeepsTheLease(t *testing.T) {
	cases := map[string]func(ctx context.Context, cl crclient.WithWatch, obj crclient.Object, patch crclient.Patch) error{
		"refused": func(context.Context, crclient.WithWatch, crclient.Object, crclient.Patch) error {
			return apierrors.NewForbidden(schema.GroupResource{Group: "longhorn.io", Resource: "volumes"}, "pvc-1", fmt.Errorf("webhook said no"))
		},
		"conflict": func(context.Context, crclient.WithWatch, crclient.Object, crclient.Patch) error {
			return apierrors.NewConflict(schema.GroupResource{Group: "longhorn.io", Resource: "volumes"}, "pvc-1", fmt.Errorf("modified"))
		},
		"landed, then timed out": func(ctx context.Context, cl crclient.WithWatch, obj crclient.Object, patch crclient.Patch) error {
			if err := cl.Patch(ctx, obj, patch); err != nil {
				return err
			}
			return apierrors.NewTimeoutError("the request did not finish", 1)
		},
	}
	for name, fail := range cases {
		t.Run(name, func(t *testing.T) {
			sc := patchFailing(fail)
			_, err := sc.convert(context.Background(), boundClaim("ns", platform.LonghornReplicatedClass, "pvc-1"), ptr.To(int32(2)))
			require.Error(t, err)
			lease := conversionLease(t, sc.Writer)
			require.NotNil(t, lease)
			require.NotNil(t, lease.Spec.HolderIdentity)
			assert.Equal(t, "pvc-1", *lease.Spec.HolderIdentity)
		})
	}
}

func failLonghornReads(err error) interceptor.Funcs {
	return interceptor.Funcs{
		Get: func(ctx context.Context, cl crclient.WithWatch, key crclient.ObjectKey, obj crclient.Object, opts ...crclient.GetOption) error {
			if obj.GetObjectKind().GroupVersionKind().Group == "longhorn.io" {
				return err
			}
			return cl.Get(ctx, key, obj, opts...)
		},
		List: func(ctx context.Context, cl crclient.WithWatch, list crclient.ObjectList, opts ...crclient.ListOption) error {
			if list.GetObjectKind().GroupVersionKind().Group == "longhorn.io" {
				return err
			}
			return cl.List(ctx, list, opts...)
		},
	}
}

// Persistent read failures also prevent live reporting. Saving Blocked lets
// users reset the copy count even when the previous condition was Converting.
func TestPersistentLonghornReadFailuresReportBlocked(t *testing.T) {
	forbidden := apierrors.NewForbidden(schema.GroupResource{Group: "longhorn.io", Resource: "volumes"}, "pvc-1", fmt.Errorf("RBAC denied"))
	svc := copiesService(1, false)
	svc.Status.Conditions = []metav1.Condition{{Type: kipperv1.ConditionStorageCopies, Status: metav1.ConditionFalse,
		Reason: kipperv1.StorageCopiesConverting, Message: "Lowering the copy count to 1.", LastTransitionTime: metav1.Now()}}
	r := rollbackService(svc, failLonghornReads(forbidden))
	ctx := context.Background()
	require.NoError(t, r.reconcileStatefulSet(ctx, svc))

	require.NoError(t, r.updateStatus(ctx, svc))

	c := copiesConditionOf(t, r)
	require.NotNil(t, c)
	assert.Equal(t, kipperv1.StorageCopiesBlocked, c.Reason)
	assert.Contains(t, c.Message, "RBAC denied")

	vol := &kipperv1.Volume{ObjectMeta: metav1.ObjectMeta{Name: "uploads", Namespace: "ns"}, Spec: kipperv1.VolumeSpec{Copies: ptr.To(int32(1))}}
	claim := boundClaim("ns", platform.LonghornSharedClass, "pvc-1")
	claim.Labels = map[string]string{kipperLabel: kipperValue, "kipper.run/resource-type": "shared-volume", "kipper.run/volume-name": "uploads"}
	c2 := crfake.NewClientBuilder().WithScheme(testScheme()).WithRESTMapper(testMapper(true)).WithObjects(convWorld()...).
		WithInterceptorFuncs(failLonghornReads(forbidden)).Build()
	vr := &VolumeReconciler{Client: c2, APIReader: c2, Scheme: testScheme()}
	require.NoError(t, vr.reportStorageCopies(ctx, vol, claim))
	vc := meta.FindStatusCondition(vol.Status.Conditions, kipperv1.ConditionStorageCopies)
	require.NotNil(t, vc)
	assert.Equal(t, kipperv1.StorageCopiesBlocked, vc.Reason)

	shared := boundClaim("ns", platform.LonghornSharedClass, "pvc-1")
	shared.Name = "shared-uploads"
	shared.Labels = map[string]string{kipperLabel: kipperValue, "kipper.run/resource-type": "shared-volume"}
	c3 := crfake.NewClientBuilder().WithScheme(testScheme()).WithRESTMapper(testMapper(true)).WithObjects(convWorld(shared)...).
		WithInterceptorFuncs(failLonghornReads(forbidden)).Build()
	pr := &ProjectReconciler{Client: c3, APIReader: c3, Scheme: testScheme()}
	project := &kipperv1.Project{ObjectMeta: metav1.ObjectMeta{Name: "shop"}, Spec: kipperv1.ProjectSpec{SharedStorage: &kipperv1.ProjectSharedStorage{Copies: ptr.To(int32(1))}}}
	pc, err := pr.reconcileSharedStorage(ctx, project, "ns", "longhorn")
	require.NoError(t, err)
	assert.Equal(t, kipperv1.StorageCopiesBlocked, pc.Reason)
}
