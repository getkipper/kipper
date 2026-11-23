package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

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

func TestAResourceUpdateLeavesTheCopiesAlone(t *testing.T) {
	for _, declared := range []interface{}{nil, int64(2)} {
		cr := storageServiceCR()
		if declared != nil {
			require.NoError(t, unstructured.SetNestedField(cr.Object, declared, "spec", "copies"))
		}
		m := managerWith([]runtime.Object{dbStatefulSet(), storageClaim("longhorn-single")}, cr)
		_, err := m.Update(context.Background(), "default", "db", Options{MemoryLimit: "1Gi"})
		require.NoError(t, err)
		svc, err := m.Dynamic.Resource(manifest.ServiceGVR).Namespace("default").Get(context.Background(), "db", metav1.GetOptions{})
		require.NoError(t, err)
		copies, found, _ := unstructured.NestedFieldNoCopy(svc.Object, "spec", "copies")
		if declared == nil {
			assert.False(t, found, "a memory change never adds copies")
		} else {
			assert.Equal(t, declared, copies, "a memory change keeps the declared copies")
		}
		memory, _, _ := unstructured.NestedString(svc.Object, "spec", "resources", "memoryLimit")
		assert.Equal(t, "1Gi", memory)
	}
}

// namedStorageClaim creates a Longhorn claim with 1Gi capacity and the given
// request. Requests above 1Gi represent unfinished expansions.
func namedStorageClaim(name, request string) *corev1.PersistentVolumeClaim {
	pvc := storageClaim("longhorn-single")
	pvc.Name = name
	pvc.Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse(request)
	pvc.Status.Capacity = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}
	return pvc
}

// longhornWebhook simulates Longhorn admission by denying updates to the claim
// named refused, including dry runs. Dry runs leave claims unchanged. The returned
// slice tracks claim names passed to the fake client's default update handler.
func longhornWebhook(t *testing.T, m *Manager, refused string) *[]string {
	t.Helper()
	var written []string
	m.Client.(*fake.Clientset).PrependReactor("update", "persistentvolumeclaims", func(a k8stesting.Action) (bool, runtime.Object, error) {
		update := a.(k8stesting.UpdateActionImpl)
		pvc := update.GetObject().(*corev1.PersistentVolumeClaim)
		if pvc.Name == refused {
			return true, nil, apierrors.NewForbidden(corev1.Resource("persistentvolumeclaims"), pvc.Name,
				errors.New(`admission webhook "validator.longhorn.io" denied the request: cannot schedule 19 GiB more storage on node server-1 disk default`))
		}
		if len(update.UpdateOptions.DryRun) > 0 {
			return true, pvc, nil
		}
		written = append(written, pvc.Name)
		return false, nil, nil
	})
	return &written
}

func TestUpdateStorageWritesNoClaimWhenLonghornRefusesOne(t *testing.T) {
	m := managerWith([]runtime.Object{dbStatefulSet(), namedStorageClaim("data-db-0", "1Gi"), namedStorageClaim("dump-db-0", "1Gi")}, storageServiceCR())
	written := longhornWebhook(t, m, "dump-db-0")

	_, err := m.Update(context.Background(), "default", "db", Options{Storage: "20Gi", MemoryLimit: "2Gi"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "dump-db-0")
	assert.Contains(t, err.Error(), "cannot schedule")
	assert.Contains(t, err.Error(), "kip cluster storage --over-provisioning")
	assert.Empty(t, *written, "no claim grows when Longhorn refuses one of them")
	assert.Equal(t, "1Gi", claimRequest(t, m))
	assert.Equal(t, "1Gi", declaredStorage(t, m))
}

func TestUpdateStorageAcceptsASmallerSizeAfterAnUnfinishedExpansion(t *testing.T) {
	m := managerWith([]runtime.Object{dbStatefulSet(), namedStorageClaim("data-db-0", "20Gi")}, storageServiceCR())
	longhornWebhook(t, m, "")

	_, err := m.Update(context.Background(), "default", "db", Options{Storage: "5Gi"})

	require.NoError(t, err)
	assert.Equal(t, "5Gi", claimRequest(t, m))
}

func TestUpdateStorageRefusesASizeAtOrBelowTheCapacityOfAnUnfinishedExpansion(t *testing.T) {
	for _, size := range []string{"1Gi", "512Mi"} {
		t.Run(size, func(t *testing.T) {
			m := managerWith([]runtime.Object{dbStatefulSet(), namedStorageClaim("data-db-0", "20Gi")}, storageServiceCR())

			_, err := m.Update(context.Background(), "default", "db", Options{Storage: size})

			require.Error(t, err)
			assert.Contains(t, err.Error(), "more than 1Gi")
			assert.Equal(t, "20Gi", claimRequest(t, m))
		})
	}
}

func TestUpdateStorageGrowsWhenAWebhookCannotDryRun(t *testing.T) {
	m := managerWith([]runtime.Object{dbStatefulSet(), storageClaim("longhorn-single")}, storageServiceCR())
	m.Client.(*fake.Clientset).PrependReactor("update", "persistentvolumeclaims", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if len(a.(k8stesting.UpdateActionImpl).UpdateOptions.DryRun) > 0 {
			return true, nil, apierrors.NewBadRequest(`admission webhook "validator.example.com" does not support dry run`)
		}
		return false, nil, nil
	})

	_, err := m.Update(context.Background(), "default", "db", Options{Storage: "5Gi"})

	require.NoError(t, err)
	assert.Equal(t, "5Gi", claimRequest(t, m))
}

func TestUpdateStorageNamesTheClaimsItRaisedBeforeARefusal(t *testing.T) {
	m := managerWith([]runtime.Object{dbStatefulSet(), namedStorageClaim("data-db-0", "1Gi"), namedStorageClaim("dump-db-0", "1Gi")}, storageServiceCR())
	m.Client.(*fake.Clientset).PrependReactor("update", "persistentvolumeclaims", func(a k8stesting.Action) (bool, runtime.Object, error) {
		update := a.(k8stesting.UpdateActionImpl)
		pvc := update.GetObject().(*corev1.PersistentVolumeClaim)
		if len(update.UpdateOptions.DryRun) > 0 {
			return true, pvc, nil
		}
		if pvc.Name == "dump-db-0" {
			return true, nil, apierrors.NewForbidden(corev1.Resource("persistentvolumeclaims"), pvc.Name,
				errors.New(`admission webhook "validator.longhorn.io" denied the request: cannot schedule 19 GiB more storage`))
		}
		return false, nil, nil
	})

	_, err := m.Update(context.Background(), "default", "db", Options{Storage: "20Gi"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "dump-db-0")
	assert.Contains(t, err.Error(), "data-db-0 already requests 20Gi")
	assert.Contains(t, err.Error(), "the service's declared size is unchanged")
	assert.Equal(t, "20Gi", claimRequest(t, m))
	assert.Equal(t, "1Gi", declaredStorage(t, m))
}

func TestUpdateStorageGivesNoLonghornHintForAnotherRefusal(t *testing.T) {
	m := managerWith([]runtime.Object{dbStatefulSet(), storageClaim("longhorn-single")}, storageServiceCR())
	m.Client.(*fake.Clientset).PrependReactor("update", "persistentvolumeclaims", func(a k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(corev1.Resource("persistentvolumeclaims"), "data-db-0",
			errors.New(`User "dev" cannot update resource "persistentvolumeclaims"`))
	})

	_, err := m.Update(context.Background(), "default", "db", Options{Storage: "5Gi"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot update resource")
	assert.NotContains(t, err.Error(), "over-provisioning")
}

func TestUpdateStorageGivesNoLonghornHintForAQuotaOnTheLonghornClass(t *testing.T) {
	m := managerWith([]runtime.Object{dbStatefulSet(), storageClaim("longhorn-single")}, storageServiceCR())
	m.Client.(*fake.Clientset).PrependReactor("update", "persistentvolumeclaims", func(a k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(corev1.Resource("persistentvolumeclaims"), "data-db-0",
			errors.New(`exceeded quota: storage, requested: longhorn-single.storageclass.storage.k8s.io/requests.storage=4Gi`))
	})

	_, err := m.Update(context.Background(), "default", "db", Options{Storage: "5Gi"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeded quota")
	assert.NotContains(t, err.Error(), "over-provisioning")
}
