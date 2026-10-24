package controllers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/platform"
)

func copiesVolume(copies *int32, labels map[string]string) *kipperv1.Volume {
	return &kipperv1.Volume{
		ObjectMeta: metav1.ObjectMeta{Name: "uploads", Namespace: "shop", Labels: labels},
		Spec:       kipperv1.VolumeSpec{Size: "1Gi", Copies: copies},
	}
}

func reconcileVolume(t *testing.T, vol *kipperv1.Volume, extra ...crclient.Object) (crclient.Client, *kipperv1.Volume) {
	t.Helper()
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithRESTMapper(testMapper(true)).
		WithObjects(append(append(twoReplicaNodes(), vol), extra...)...).WithStatusSubresource(&kipperv1.Volume{}).Build()
	r := &VolumeReconciler{Client: c, APIReader: c, Scheme: testScheme()}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "shop", Name: "uploads"}})
	require.NoError(t, err)
	var got kipperv1.Volume
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "shop", Name: "uploads"}, &got))
	return c, &got
}

func sharedClaimClass(t *testing.T, c crclient.Client) (string, bool) {
	t.Helper()
	var pvc corev1.PersistentVolumeClaim
	err := c.Get(context.Background(), types.NamespacedName{Namespace: "shop", Name: "shared-uploads"}, &pvc)
	if apierrors.IsNotFound(err) {
		return "", false
	}
	require.NoError(t, err)
	return *pvc.Spec.StorageClassName, true
}

func TestANewSharedVolumeWithoutCopiesGetsOneCopy(t *testing.T) {
	c, _ := reconcileVolume(t, copiesVolume(nil, nil))
	class, ok := sharedClaimClass(t, c)
	require.True(t, ok)
	assert.Equal(t, platform.LonghornSingleClass, class)
}

func TestASharedVolumeCannotBeCreatedWithTwoCopies(t *testing.T) {
	c, got := reconcileVolume(t, copiesVolume(ptr.To(int32(2)), nil))
	_, ok := sharedClaimClass(t, c)
	assert.False(t, ok, "no claim while shared copies are not qualified")
	cond := apimeta.FindStatusCondition(got.Status.Conditions, kipperv1.ConditionStorageCopies)
	require.NotNil(t, cond)
	assert.Equal(t, kipperv1.StorageCopiesRWXNotQualified, cond.Reason)
	assert.Equal(t, "Pending", got.Status.Phase)
}

func TestARestoredTwoCopySharedVolumeGetsOneCopyAndKeepsItsIntent(t *testing.T) {
	c, got := reconcileVolume(t, copiesVolume(ptr.To(int32(2)), map[string]string{veleroRestoreNameLabel: "restore-1"}))
	class, ok := sharedClaimClass(t, c)
	require.True(t, ok, "a restore must bring the data back")
	assert.Equal(t, platform.LonghornSingleClass, class)
	assert.Equal(t, int32(2), *got.Spec.Copies)
	cond := apimeta.FindStatusCondition(got.Status.Conditions, kipperv1.ConditionStorageCopies)
	require.NotNil(t, cond)
	assert.Equal(t, kipperv1.StorageCopiesRWXNotQualified, cond.Reason)
}

func TestAnExistingSharedClaimIsLeftOnItsClass(t *testing.T) {
	existing := sharedClaim(nil)
	existing.Namespace = "shop"
	c, got := reconcileVolume(t, copiesVolume(ptr.To(int32(2)), nil), existing)
	class, ok := sharedClaimClass(t, c)
	require.True(t, ok)
	assert.Equal(t, platform.LonghornSharedClass, class)
	cond := apimeta.FindStatusCondition(got.Status.Conditions, kipperv1.ConditionStorageCopies)
	require.NotNil(t, cond)
	assert.Equal(t, kipperv1.StorageCopiesRWXNotQualified, cond.Reason)
	assert.Contains(t, cond.Message, "keeps its current copies", "an unbound claim has no live copies yet")
}

func TestSharedVolumeStatusReportsTheLiveCopies(t *testing.T) {
	existing := sharedClaim(nil)
	existing.Namespace = "shop"
	existing.Spec.VolumeName = "pvc-9"
	_, got := reconcileVolume(t, copiesVolume(nil, nil), existing,
		longhornVolume("pvc-9", "shop", "shared-uploads", 3),
		longhornReplica("r1", "pvc-9", "server-1", "2026-10-24T01:00:00Z", ""),
		longhornReplica("r2", "pvc-9", "worker-1", "2026-10-24T01:00:00Z", ""),
	)
	require.NotNil(t, got.Status.Storage)
	assert.Equal(t, int32(3), got.Status.Storage.Copies)
	assert.Equal(t, []string{"server-1", "worker-1"}, got.Status.Storage.Nodes)
	cond := apimeta.FindStatusCondition(got.Status.Conditions, kipperv1.ConditionStorageCopies)
	require.NotNil(t, cond)
	assert.Equal(t, kipperv1.StorageCopiesNotSet, cond.Reason)
	assert.Contains(t, cond.Message, "Longhorn keeps 3 copies (2 healthy), more than Kipper sets",
		"a shared volume on the stock class has three copies, which no copies value records")
}

func TestTheHolderAcceptsEveryLonghornClass(t *testing.T) {
	for _, class := range []string{platform.LonghornSharedClass, platform.LonghornSingleClass, platform.LonghornReplicatedClass} {
		pvc := sharedClaim(nil)
		pvc.Spec.StorageClassName = ptr.To(class)
		assert.Empty(t, claimUnsupported(pvc), class)
	}
	pvc := sharedClaim(nil)
	pvc.Spec.StorageClassName = ptr.To("local-path")
	assert.NotEmpty(t, claimUnsupported(pvc))
}

func TestProjectSharedStorageWithTwoCopiesIsHeldBack(t *testing.T) {
	scheme := testScheme()
	project := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "demo"},
		Spec: kipperv1.ProjectSpec{
			Environments:  []kipperv1.ProjectEnvironment{{Name: "prod"}},
			SharedStorage: &kipperv1.ProjectSharedStorage{Enabled: true, Size: "2Gi", Copies: ptr.To(int32(2))},
		},
	}
	c := projectFakeBuilder().WithScheme(scheme).WithObjects(project, nodeWithIP("worker-1", "ExternalIP", "203.0.113.9")).
		WithStatusSubresource(&kipperv1.Project{}).Build()
	r := &ProjectReconciler{Client: c, Scheme: scheme, APIReader: c}
	for range 2 {
		_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "demo"}})
		require.NoError(t, err)
	}
	err := c.Get(context.Background(), crclient.ObjectKey{Namespace: "demo-prod", Name: "shared-uploads"}, &corev1.PersistentVolumeClaim{})
	assert.True(t, apierrors.IsNotFound(err))
	var got kipperv1.Project
	require.NoError(t, c.Get(context.Background(), crclient.ObjectKey{Name: "demo"}, &got))
	cond := apimeta.FindStatusCondition(got.Status.Conditions, kipperv1.ConditionStorageCopies)
	require.NotNil(t, cond)
	assert.Equal(t, kipperv1.StorageCopiesRWXNotQualified, cond.Reason)
}
