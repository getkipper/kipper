package controllers

import (
	"context"
	stderrors "errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/platform"
)

func twoReplicaNodes() []crclient.Object {
	return []crclient.Object{
		longhornNode("server-1", []interface{}{"kipper-server", "kipper-replica"}, "True", "True"), kubeNode("server-1", corev1.ConditionTrue),
		longhornNode("worker-1", []interface{}{"kipper-replica"}, "True", "True"), kubeNode("worker-1", corev1.ConditionTrue),
	}
}

func oneReplicaNode() []crclient.Object { return twoReplicaNodes()[:2] }

func copiesService(copies int32, consistentBackups bool) *kipperv1.Service {
	svc := bareService("postgres")
	svc.Spec.Copies = ptr.To(copies)
	svc.Spec.ConsistentBackups = consistentBackups
	return svc
}

func copiesReconciler(objs ...crclient.Object) *ServiceReconciler {
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithRESTMapper(testMapper(true)).WithObjects(objs...).
		WithStatusSubresource(&kipperv1.Service{}).Build()
	r := &ServiceReconciler{Client: c, APIReader: c, Scheme: testScheme()}
	return r
}

func claimClasses(t *testing.T, r *ServiceReconciler) (data, dump string) {
	t.Helper()
	ctx := context.Background()
	var sts appsv1.StatefulSet
	require.NoError(t, r.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "db"}, &sts))
	require.Len(t, sts.Spec.VolumeClaimTemplates, 1)
	data = *sts.Spec.VolumeClaimTemplates[0].Spec.StorageClassName
	var claim corev1.PersistentVolumeClaim
	if err := r.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "db-dump"}, &claim); err == nil {
		dump = *claim.Spec.StorageClassName
	}
	return data, dump
}

func TestTwoCopiesGetTheReplicatedClassAndASingleCopyDump(t *testing.T) {
	svc := copiesService(2, true)
	r := copiesReconciler(append(twoReplicaNodes(), svc)...)
	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))
	data, dump := claimClasses(t, r)
	assert.Equal(t, platform.LonghornReplicatedClass, data)
	assert.Equal(t, platform.LonghornSingleClass, dump, "the dump is transient and always one copy")
}

func TestALocalStorageDumpStaysLocalPath(t *testing.T) {
	svc := copiesService(1, true)
	r := copiesReconciler(localPlatformConfig(), svc)
	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))
	data, dump := claimClasses(t, r)
	assert.Equal(t, platform.LocalPathClass, data)
	assert.Equal(t, platform.LocalPathClass, dump)
}

func TestUnsetCopiesCreateASingleCopy(t *testing.T) {
	svc := bareService("postgres")
	r := copiesReconciler(svc)
	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))
	data, _ := claimClasses(t, r)
	assert.Equal(t, platform.LonghornSingleClass, data)
}

func requireGate(t *testing.T, err error, reason string) {
	t.Helper()
	var gate *storageGateError
	require.True(t, stderrors.As(err, &gate), "want a storage gate, got %v", err)
	assert.Equal(t, reason, gate.reason)
}

func TestTwoCopiesWaitForASecondReplicaNode(t *testing.T) {
	svc := copiesService(2, true)
	r := copiesReconciler(append(oneReplicaNode(), svc)...)
	err := r.reconcileStatefulSet(context.Background(), svc)
	requireGate(t, err, kipperv1.StorageCopiesNotEnoughReplicaNodes)
	var gate *storageGateError
	require.True(t, stderrors.As(err, &gate))
	assert.Contains(t, gate.message, "the cluster has 1")
	assert.NotContains(t, gate.message, "server-1", "a tenant sees counts, never node names")

	var sts appsv1.StatefulSet
	assert.True(t, apierrors.IsNotFound(r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db"}, &sts)), "no single-copy fallback")
	var dump corev1.PersistentVolumeClaim
	assert.True(t, apierrors.IsNotFound(r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db-dump"}, &dump)))
}

func TestTwoCopiesOnLocalStorageAreRefused(t *testing.T) {
	svc := copiesService(2, false)
	r := copiesReconciler(localPlatformConfig(), svc)
	requireGate(t, r.reconcileStatefulSet(context.Background(), svc), kipperv1.StorageCopiesLocalStorage)
}

func TestARestoredServiceIsCreatedWithTooFewReplicaNodes(t *testing.T) {
	svc := copiesService(2, false)
	svc.Labels = map[string]string{veleroRestoreNameLabel: "restore-1"}
	r := copiesReconciler(append(oneReplicaNode(), svc)...)
	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))
	data, _ := claimClasses(t, r)
	assert.Equal(t, platform.LonghornReplicatedClass, data, "Longhorn creates it degraded and adds the copy later")
}

func TestAServiceWhoseDataClaimExistsIsReconciledWhateverCopiesSays(t *testing.T) {
	svc := copiesService(2, false)
	claim := boundClaim("ns", platform.LonghornSingleClass, "pvc-1")
	r := copiesReconciler(append(oneReplicaNode(), svc, claim)...)
	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc), "a restored claim must not wait for the copies gate")
	var sts appsv1.StatefulSet
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db"}, &sts))
}

func TestServiceStatusReportsTheLiveCopies(t *testing.T) {
	svc := copiesService(2, false)
	r := copiesReconciler(append(twoReplicaNodes(), svc,
		boundClaim("ns", platform.LonghornReplicatedClass, "pvc-1"),
		longhornVolume("pvc-1", "ns", "data-db-0", 2),
		longhornReplica("r1", "pvc-1", "server-1", "2026-10-24T01:00:00Z", ""),
		longhornReplica("r2", "pvc-1", "worker-1", "2026-10-24T01:00:00Z", ""),
	)...)
	ctx := context.Background()
	require.NoError(t, r.reconcileStatefulSet(ctx, svc))
	require.NoError(t, r.updateStatus(ctx, svc))
	var got kipperv1.Service
	require.NoError(t, r.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "db"}, &got))
	require.NotNil(t, got.Status.Storage)
	assert.Equal(t, kipperv1.StorageCopiesStatus{Class: platform.LonghornReplicatedClass, Copies: 2, HealthyCopies: 2, Nodes: []string{"server-1", "worker-1"}}, *got.Status.Storage)
	c := apimeta.FindStatusCondition(got.Status.Conditions, kipperv1.ConditionStorageCopies)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionTrue, c.Status)
	assert.Equal(t, kipperv1.StorageCopiesHealthy, c.Reason)
}

func TestAServiceWithAFencedCopyHolderIsDegraded(t *testing.T) {
	svc := copiesService(2, false)
	fence := corev1.Taint{Key: platform.OutOfServiceTaint, Value: "nodeshutdown", Effect: corev1.TaintEffectNoExecute}
	r := copiesReconciler(svc,
		longhornNode("server-1", []interface{}{"kipper-server", "kipper-replica"}, "True", "True"), kubeNode("server-1", corev1.ConditionTrue),
		longhornNode("worker-1", []interface{}{"kipper-replica"}, "True", "True"), kubeNode("worker-1", corev1.ConditionTrue, fence),
		boundClaim("ns", platform.LonghornReplicatedClass, "pvc-1"),
		longhornVolume("pvc-1", "ns", "data-db-0", 2),
		longhornReplica("r1", "pvc-1", "server-1", "2026-10-24T01:00:00Z", ""),
		longhornReplica("r2", "pvc-1", "worker-1", "2026-10-24T01:00:00Z", ""),
	)
	ctx := context.Background()
	require.NoError(t, r.reconcileStatefulSet(ctx, svc))
	require.NoError(t, r.updateStatus(ctx, svc))
	var got kipperv1.Service
	require.NoError(t, r.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "db"}, &got))
	c := apimeta.FindStatusCondition(got.Status.Conditions, kipperv1.ConditionStorageCopies)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionFalse, c.Status, "kip status lists a False condition as needing attention")
	assert.Equal(t, kipperv1.StorageCopiesDegraded, c.Reason)
	assert.Equal(t, []string{"server-1"}, got.Status.Storage.Nodes)
}

func TestAnUnsetServiceIsReportedAsUnmanaged(t *testing.T) {
	svc := bareService("postgres")
	r := copiesReconciler(svc,
		boundClaim("ns", platform.LonghornSharedClass, "pvc-1"),
		longhornVolume("pvc-1", "ns", "data-db-0", 2),
		longhornReplica("r1", "pvc-1", "server-1", "2026-10-24T01:00:00Z", ""),
		kubeNode("server-1", corev1.ConditionTrue),
	)
	ctx := context.Background()
	require.NoError(t, r.reconcileStatefulSet(ctx, svc))
	require.NoError(t, r.updateStatus(ctx, svc))
	var got kipperv1.Service
	require.NoError(t, r.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "db"}, &got))
	c := apimeta.FindStatusCondition(got.Status.Conditions, kipperv1.ConditionStorageCopies)
	require.NotNil(t, c)
	assert.Equal(t, kipperv1.StorageCopiesNotSet, c.Reason)
	assert.Contains(t, c.Message, "Longhorn keeps 2 copies (1 healthy)")
	assert.Contains(t, c.Message, "kip service update db --copies 1|2")
}

func TestAProjectWithSharedStorageCopiesIsReadAgainSooner(t *testing.T) {
	assert.Equal(t, egressRefreshInterval, projectRequeue(nil), "a project without shared storage copies keeps the egress interval")
	settled := []metav1.Condition{{Type: kipperv1.ConditionStorageCopies, Status: metav1.ConditionTrue}}
	assert.Equal(t, storageRefreshSettled, projectRequeue(settled))
	degraded := []metav1.Condition{{Type: kipperv1.ConditionStorageCopies, Status: metav1.ConditionFalse}}
	assert.Equal(t, storageRefreshUnsettled, projectRequeue(degraded))
}

func TestUnsetCopiesAreRefreshedLikeSettledOnes(t *testing.T) {
	unset := &metav1.Condition{Type: kipperv1.ConditionStorageCopies, Status: metav1.ConditionUnknown, Reason: kipperv1.StorageCopiesNotSet}
	assert.Equal(t, storageRefreshSettled, storageRefresh(unset), "unset copies change only when someone sets them")
	pending := &metav1.Condition{Type: kipperv1.ConditionStorageCopies, Status: metav1.ConditionUnknown, Reason: kipperv1.StorageCopiesPending}
	assert.Equal(t, storageRefreshUnsettled, storageRefresh(pending))
	unknown := &metav1.Condition{Type: kipperv1.ConditionStorageCopies, Status: metav1.ConditionUnknown, Reason: kipperv1.StorageCopiesUnknown}
	assert.Equal(t, storageRefreshUnsettled, storageRefresh(unknown))
}

// A blocked conversion uses the running interval while waiting for pods to
// release the volume or keeping a workload stopped because these controllers
// do not watch the pods or Longhorn. Other blocks use the slower interval.
func TestABlockedConversionIsReadAgainSoonOnlyWhileItWaitsOrStops(t *testing.T) {
	blocked := func(message string) []metav1.Condition {
		return []metav1.Condition{{Type: kipperv1.ConditionStorageCopies, Status: metav1.ConditionFalse, Reason: kipperv1.StorageCopiesBlocked, Message: message}}
	}
	sc := storageConverter{StatefulSet: "db"}
	assert.Equal(t, storageRefreshConverting, storageRequeue(blocked("db keeps running. "+inUse([]string{"debug"}))))
	assert.Equal(t, storageRefreshConverting, storageRequeue(blocked("prod: "+inUse([]string{"web-1"}))))
	assert.Equal(t, storageRefreshConverting, storageRequeue(blocked(sc.stoppedFor()+copyNodeDown("worker-1"))))
	assert.Equal(t, storageRefreshUnsettled, storageRequeue(blocked(copyNodeDown("worker-1"))))

	holderStopped := append(blocked(copyNodeDown("worker-1")),
		metav1.Condition{Type: conditionBackupHolderReady, Status: metav1.ConditionFalse, Reason: "StoppedForCopyChange"})
	assert.Equal(t, storageRefreshConverting, storageRequeue(holderStopped), "a shared volume's stop shows on its holder")
}

func TestTheProjectCopiesCommandIsKip(t *testing.T) {
	assert.Equal(t, "kip project update shop --copies 2", projectCopiesCommand("shop", 2))
	assert.Equal(t, "kip project update shop --copies", projectStorageCommands("shop").Copies)
}

// A cluster that grew onto Longhorn keeps a service's data on local storage
// until 'kip service storage longhorn' moves it, so two copies wait for that.
func TestTwoCopiesOfAServiceStillOnLocalStorageWaitForTheMove(t *testing.T) {
	svc := copiesService(2, false)
	r := copiesReconciler(append(twoReplicaNodes(), svc, boundClaim("ns", platform.LocalPathClass, "pvc-1"))...)
	ctx := context.Background()
	require.NoError(t, r.reconcileStatefulSet(ctx, svc))
	require.NoError(t, r.updateStatus(ctx, svc))
	var got kipperv1.Service
	require.NoError(t, r.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "db"}, &got))
	c := apimeta.FindStatusCondition(got.Status.Conditions, kipperv1.ConditionStorageCopies)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionFalse, c.Status)
	assert.Equal(t, kipperv1.StorageCopiesLocalStorage, c.Reason)
	assert.Contains(t, c.Message, "'kip service storage longhorn db'")
}
