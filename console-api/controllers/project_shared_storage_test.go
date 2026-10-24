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
	ctrl "sigs.k8s.io/controller-runtime"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
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
