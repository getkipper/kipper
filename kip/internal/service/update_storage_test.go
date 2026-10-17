package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/getkipper/kipper/kip/internal/manifest"
)

func storageClaim(class string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data-db-0", Namespace: "default", Labels: map[string]string{"app": "db"}},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: &class,
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceStorage: resource.MustParse("1Gi"),
			}},
		},
	}
}

func dbStatefulSet() *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "default"},
		Spec: appsv1.StatefulSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "postgres", Image: "postgres:16-alpine"}},
		}}},
	}
}

func storageServiceCR() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": manifest.ServiceGVR.GroupVersion().String(), "kind": "Service",
		"metadata": map[string]interface{}{"name": "db", "namespace": "default"},
		"spec":     map[string]interface{}{"type": "postgres", "storage": "1Gi"},
	}}
}

func declaredStorage(t *testing.T, m *Manager) string {
	t.Helper()
	svc, err := m.Dynamic.Resource(manifest.ServiceGVR).Namespace("default").Get(context.Background(), "db", metav1.GetOptions{})
	require.NoError(t, err)
	s, _, _ := unstructured.NestedString(svc.Object, "spec", "storage")
	return s
}

func claimRequest(t *testing.T, m *Manager) string {
	t.Helper()
	pvc, err := m.Client.CoreV1().PersistentVolumeClaims("default").Get(context.Background(), "data-db-0", metav1.GetOptions{})
	require.NoError(t, err)
	return pvc.Spec.Resources.Requests.Storage().String()
}

func TestUpdateStorageOnLonghornGrowsTheClaimAndTheDeclaredSize(t *testing.T) {
	m := managerWith([]runtime.Object{dbStatefulSet(), storageClaim("longhorn-single")}, storageServiceCR())
	result, err := m.Update(context.Background(), "default", "db", Options{Storage: "5Gi"})
	require.NoError(t, err)
	assert.True(t, result.StorageExpanded)
	assert.Equal(t, "5Gi", claimRequest(t, m))
	assert.Equal(t, "5Gi", declaredStorage(t, m))
}

func TestUpdateStorageOnLocalStorageRecordsTheDeclaredSizeOnly(t *testing.T) {
	m := managerWith([]runtime.Object{dbStatefulSet(), storageClaim("local-path")}, storageServiceCR())
	result, err := m.Update(context.Background(), "default", "db", Options{Storage: "5Gi"})
	require.NoError(t, err)
	assert.False(t, result.StorageExpanded)
	assert.True(t, result.StorageDeclared)
	assert.Equal(t, "1Gi", claimRequest(t, m), "a local-path claim is never patched")
	assert.Equal(t, "5Gi", declaredStorage(t, m))
}

func TestUpdateStorageRefusesASizeThatIsNoQuantity(t *testing.T) {
	for _, size := range []string{"banana", "0", "-1Gi"} {
		t.Run(size, func(t *testing.T) {
			m := managerWith([]runtime.Object{dbStatefulSet(), storageClaim("local-path")}, storageServiceCR())
			_, err := m.Update(context.Background(), "default", "db", Options{Storage: size})
			assert.ErrorContains(t, err, "--storage")
			assert.Equal(t, "1Gi", declaredStorage(t, m), "nothing is written for a size that is no quantity")
		})
	}
}

func TestUpdateStorageRefusesToShrinkALonghornVolume(t *testing.T) {
	m := managerWith([]runtime.Object{dbStatefulSet(), storageClaim("longhorn-single")}, storageServiceCR())
	_, err := m.Update(context.Background(), "default", "db", Options{Storage: "512Mi"})
	assert.ErrorContains(t, err, "1Gi")
	assert.Equal(t, "1Gi", declaredStorage(t, m))
	assert.Equal(t, "1Gi", claimRequest(t, m))
}

func TestUpdateStorageRefusalWritesNoOtherChange(t *testing.T) {
	m := managerWith([]runtime.Object{dbStatefulSet(), storageClaim("longhorn-single")}, storageServiceCR())
	_, err := m.Update(context.Background(), "default", "db", Options{Storage: "512Mi", MemoryLimit: "2Gi"})
	require.Error(t, err)
	svc, err := m.Dynamic.Resource(manifest.ServiceGVR).Namespace("default").Get(context.Background(), "db", metav1.GetOptions{})
	require.NoError(t, err)
	_, found, _ := unstructured.NestedMap(svc.Object, "spec", "resources")
	assert.False(t, found, "the memory change given with a refused storage change is not written")
}
