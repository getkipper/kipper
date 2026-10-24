package controllers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/meta/testrestmapper"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	kipperlabels "github.com/getkipper/kipper/controller/pkg/labels"
	"github.com/getkipper/kipper/controller/pkg/platform"
)

func reconcileSharedStorageProject(t *testing.T, extra ...crclient.Object) (crclient.Client, *kipperv1.Project) {
	t.Helper()
	scheme := testScheme()
	project := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "demo"},
		Spec: kipperv1.ProjectSpec{
			Environments:  []kipperv1.ProjectEnvironment{{Name: "prod"}},
			SharedStorage: &kipperv1.ProjectSharedStorage{Enabled: true, Size: "2Gi"},
		},
	}
	fakeClient := projectFakeBuilder().WithScheme(scheme).
		WithObjects(append([]crclient.Object{project, nodeWithIP("worker-1", "ExternalIP", "203.0.113.9")}, extra...)...).
		WithStatusSubresource(&kipperv1.Project{}).Build()
	r := &ProjectReconciler{Client: fakeClient, Scheme: scheme, APIReader: fakeClient}
	for range 2 {
		_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "demo"}})
		require.NoError(t, err)
	}
	got := &kipperv1.Project{}
	require.NoError(t, fakeClient.Get(context.Background(), crclient.ObjectKey{Name: "demo"}, got))
	return fakeClient, got
}

func TestProjectReconciler_SharedStorageOnLonghorn(t *testing.T) {
	c, project := reconcileSharedStorageProject(t)
	var pvc corev1.PersistentVolumeClaim
	require.NoError(t, c.Get(context.Background(), crclient.ObjectKey{Namespace: "demo-prod", Name: "shared-uploads"}, &pvc))
	assert.Equal(t, platform.LonghornSingleClass, *pvc.Spec.StorageClassName, "new shared storage without copies gets one copy")
	assert.Nil(t, apimeta.FindStatusCondition(project.Status.Conditions, conditionSharedStorageUnsupported))
}

func TestProjectReconciler_RefusesSharedStorageOnLocalStorage(t *testing.T) {
	c, project := reconcileSharedStorageProject(t, localPlatformConfig())
	err := c.Get(context.Background(), crclient.ObjectKey{Namespace: "demo-prod", Name: "shared-uploads"}, &corev1.PersistentVolumeClaim{})
	assert.True(t, apierrors.IsNotFound(err), "no ReadWriteMany claim is created on local storage")
	var ns corev1.Namespace
	assert.NoError(t, c.Get(context.Background(), crclient.ObjectKey{Name: "demo-prod"}, &ns), "the rest of the project is still set up")
	cond := apimeta.FindStatusCondition(project.Status.Conditions, conditionSharedStorageUnsupported)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, platform.SharedVolumesUnsupported, cond.Message)
}

// Shared storage keeps one copy in this release, so the hint for unset copies
// names one copy even when Longhorn keeps two.
func TestTheUnsetCopiesHintOfSharedStorageNamesACountKipAccepts(t *testing.T) {
	existing := sharedClaim(nil)
	existing.Namespace = "demo-prod"
	delete(existing.Labels, kipperlabels.VolumeName)
	existing.Spec.VolumeName = "pvc-9"
	_, project := reconcileSharedStorageProject(t, existing,
		longhornVolume("pvc-9", "demo-prod", "shared-uploads", 2),
		longhornReplica("r1", "pvc-9", "server-1", "2026-10-24T01:00:00Z", ""),
		longhornReplica("r2", "pvc-9", "worker-1", "2026-10-24T01:00:00Z", ""),
	)
	cond := apimeta.FindStatusCondition(project.Status.Conditions, kipperv1.ConditionStorageCopies)
	require.NotNil(t, cond)
	assert.Equal(t, kipperv1.StorageCopiesNotSet, cond.Reason)
	assert.Contains(t, cond.Message, "Longhorn keeps 2 copies")
	assert.Contains(t, cond.Message, "'kip project update demo --copies 1'")
	assert.NotContains(t, cond.Message, "--copies 2")
}

// retainedSharedWorld builds project demo with shared storage disabled and
// its prod shared claim retained. The claim is bound to the one-copy Longhorn
// volume pvc-1, whose copy r-s is on the server, and modes supplies the
// engine's replica modes.
func retainedSharedWorld(t *testing.T, modes map[string]interface{}) (crclient.Client, *ProjectReconciler) {
	t.Helper()
	scheme := testScheme()
	project := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "demo"},
		Spec: kipperv1.ProjectSpec{
			Environments:  []kipperv1.ProjectEnvironment{{Name: "prod"}},
			SharedStorage: &kipperv1.ProjectSharedStorage{Enabled: false, Size: "2Gi", Copies: ptr.To(int32(1))},
		},
	}
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "shared-uploads", Namespace: "demo-prod",
			Labels: map[string]string{kipperLabel: kipperValue, "kipper.run/resource-type": "shared-volume", "kipper.run/project": "demo"}},
		Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: ptr.To(platform.LonghornSingleClass), VolumeName: "pvc-1"},
	}
	v := longhornVolume("pvc-1", "demo-prod", "shared-uploads", 1)
	v.SetUID("uid-pvc-1")
	_ = unstructured.SetNestedField(v.Object, "healthy", "status", "robustness")
	_ = unstructured.SetNestedField(v.Object, "attached", "status", "state")
	keeper := longhornReplica("r-s", "pvc-1", "server-1", healthyTime, "")
	keeper.SetUID("uid-r-s")
	_ = unstructured.SetNestedField(keeper.Object, "2026-10-24T09:00:00Z", "spec", "lastHealthyAt")
	objs := convWorld(project, claim, v, keeper, lhEngine(modes))
	for _, o := range objs {
		if n, ok := o.(*corev1.Node); ok {
			// The egress policy needs each node's pod address family.
			n.Spec.PodCIDRs = []string{"10.42.0.0/24"}
		}
	}
	mapper := apimeta.MultiRESTMapper{testMapper(true), testrestmapper.TestOnlyStaticRESTMapper(scheme)}
	c := projectFakeBuilder().WithScheme(scheme).WithRESTMapper(mapper).WithObjects(objs...).
		WithStatusSubresource(&kipperv1.Project{}).Build()
	r := &ProjectReconciler{Client: c, Scheme: scheme, APIReader: c, Recorder: record.NewFakeRecorder(10)}
	return c, r
}

// Turning shared storage off while a reduction holds the claim's volume with
// its count lowered ends the hold, so the retained claim is not left attached
// on the server with its frontend disabled. The count goes back to the copies
// present and data locality off before the ticket goes.
func TestTurningSharedStorageOffReleasesAHeldVolume(t *testing.T) {
	c, r := retainedSharedWorld(t, map[string]interface{}{"r-s": "RW", "r-w": "RW"})
	ctx := context.Background()
	var pvc corev1.PersistentVolumeClaim
	require.NoError(t, c.Get(ctx, crclient.ObjectKey{Namespace: "demo-prod", Name: "shared-uploads"}, &pvc))
	pvc.Annotations = map[string]string{platform.CopyChangeStopAnnotation: `{"at":"2026-10-24T11:59:00Z"}`}
	require.NoError(t, c.Update(ctx, &pvc))
	require.NoError(t, c.Create(ctx, lhAttachmentObj("server-1")))
	require.NoError(t, c.Create(ctx, longhornReplica("r-w", "pvc-1", "worker-1", healthyTime, "")))
	setLonghornField(t, c, "Volume", "pvc-1", "best-effort", "spec", "dataLocality")
	setLonghornField(t, c, "Volume", "pvc-1", "server-1", "status", "currentNodeID")
	setLonghornField(t, c, "Volume", "pvc-1", true, "status", "frontendDisabled")
	var ticketDropped bool
	c = interceptor.NewClient(c.(crclient.WithWatch), interceptor.Funcs{Patch: func(ctx context.Context, cl crclient.WithWatch, obj crclient.Object, patch crclient.Patch, opts ...crclient.PatchOption) error {
		if obj.GetObjectKind().GroupVersionKind().Kind == "VolumeAttachment" {
			vol := getLonghorn(t, cl, "Volume", "pvc-1")
			count, _, _ := unstructured.NestedInt64(vol.Object, "spec", "numberOfReplicas")
			locality, _, _ := unstructured.NestedString(vol.Object, "spec", "dataLocality")
			assert.Equal(t, int64(2), count, "the count is raised before the ticket goes")
			assert.Equal(t, "disabled", locality, "data locality is off before the ticket goes")
			ticketDropped = true
		}
		return cl.Patch(ctx, obj, patch, opts...)
	}})
	r.Client, r.APIReader = c, c

	reconcileDemo(t, c, r)

	require.True(t, ticketDropped)
	require.NoError(t, c.Get(ctx, crclient.ObjectKey{Namespace: "demo-prod", Name: "shared-uploads"}, &pvc))
	assert.NotContains(t, pvc.Annotations, platform.CopyChangeStopAnnotation)
	assert.Nil(t, ticketOf(t, c))
	assert.True(t, replicaExists(t, c, "r-w"))
}

func reconcileDemo(t *testing.T, c crclient.Client, r *ProjectReconciler) *kipperv1.Project {
	t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "demo"}})
	require.NoError(t, err)
	got := &kipperv1.Project{}
	require.NoError(t, c.Get(context.Background(), crclient.ObjectKey{Name: "demo"}, got))
	return got
}

// TestTurningSharedStorageOffCreatesAndConvertsNothing checks that disabling
// shared storage preserves a retained claim's copy count and leaves a deleted
// claim absent.
func TestTurningSharedStorageOffCreatesAndConvertsNothing(t *testing.T) {
	c, r := retainedSharedWorld(t, map[string]interface{}{"r-s": "RW"})
	var pvc corev1.PersistentVolumeClaim
	require.NoError(t, c.Get(context.Background(), crclient.ObjectKey{Namespace: "demo-prod", Name: "shared-uploads"}, &pvc))
	setLonghornField(t, c, "Volume", "pvc-1", int64(2), "spec", "numberOfReplicas")

	project := reconcileDemo(t, c, r)
	count, _, _ := unstructured.NestedInt64(getLonghorn(t, c, "Volume", "pvc-1").Object, "spec", "numberOfReplicas")
	assert.Equal(t, int64(2), count, "the copies of a retained claim are left as they are")
	assert.Nil(t, apimeta.FindStatusCondition(project.Status.Conditions, kipperv1.ConditionStorageCopies))

	require.NoError(t, c.Delete(context.Background(), &pvc))
	reconcileDemo(t, c, r)
	err := c.Get(context.Background(), crclient.ObjectKey{Namespace: "demo-prod", Name: "shared-uploads"}, &corev1.PersistentVolumeClaim{})
	assert.True(t, apierrors.IsNotFound(err), "a deleted shared claim is not created again")
}
