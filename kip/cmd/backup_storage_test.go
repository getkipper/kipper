package cmd

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func minioClaim(request, capacity string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "minio-storage", Namespace: "velero"},
		Spec: corev1.PersistentVolumeClaimSpec{Resources: corev1.VolumeResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(request)},
		}},
		Status: corev1.PersistentVolumeClaimStatus{
			Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(capacity)},
		},
	}
}

// growingClientset simulates a storage driver that completes expansion on update.
func growingClientset(objects ...runtime.Object) *fake.Clientset {
	cs := fake.NewClientset(objects...)
	cs.PrependReactor("update", "persistentvolumeclaims", func(action k8stesting.Action) (bool, runtime.Object, error) {
		pvc := action.(k8stesting.UpdateAction).GetObject().(*corev1.PersistentVolumeClaim)
		pvc.Status.Capacity = corev1.ResourceList{corev1.ResourceStorage: pvc.Spec.Resources.Requests[corev1.ResourceStorage]}
		return false, nil, nil
	})
	return cs
}

func resizeForTest(cs *fake.Clientset, size string) (string, error) {
	var out bytes.Buffer
	err := resizeBackupStorage(context.Background(), cs, size, time.Millisecond, 50*time.Millisecond, &out)
	return out.String(), err
}

func TestResizeBackupStorageGrowsTheMinIOVolume(t *testing.T) {
	cs := growingClientset(minioClaim("10Gi", "10Gi"))

	out, err := resizeForTest(cs, "30Gi")
	require.NoError(t, err)
	assert.Contains(t, out, "10Gi to 30Gi")

	pvc, err := cs.CoreV1().PersistentVolumeClaims("velero").Get(context.Background(), "minio-storage", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "30Gi", pvc.Spec.Resources.Requests.Storage().String())
}

func TestResizeBackupStorageNeverShrinks(t *testing.T) {
	cs := growingClientset(minioClaim("30Gi", "30Gi"))

	_, err := resizeForTest(cs, "10Gi")
	assert.ErrorContains(t, err, "can only grow")

	pvc, err := cs.CoreV1().PersistentVolumeClaims("velero").Get(context.Background(), "minio-storage", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "30Gi", pvc.Spec.Resources.Requests.Storage().String())
}

func TestResizeBackupStorageAtTheCurrentSizeChangesNothing(t *testing.T) {
	cs := growingClientset(minioClaim("30Gi", "30Gi"))

	out, err := resizeForTest(cs, "30Gi")
	require.NoError(t, err)
	assert.Contains(t, out, "already 30Gi")
	for _, a := range cs.Actions() {
		assert.NotEqual(t, "update", a.GetVerb())
	}
}

// TestResizeBackupStorageWaitsForAnExpansionStillInProgress verifies that retries
// wait for the pending request without updating it again.
func TestResizeBackupStorageWaitsForAnExpansionStillInProgress(t *testing.T) {
	cs := fake.NewClientset(minioClaim("30Gi", "10Gi"))

	_, err := resizeForTest(cs, "30Gi")
	assert.ErrorContains(t, err, "does not report that size yet")
	for _, a := range cs.Actions() {
		assert.NotEqual(t, "update", a.GetVerb())
	}
}

func TestResizeBackupStorageReportsAVolumeThatDoesNotGrow(t *testing.T) {
	cs := fake.NewClientset(minioClaim("10Gi", "10Gi"))

	_, err := resizeForTest(cs, "30Gi")
	assert.ErrorContains(t, err, "does not report that size yet")
}

func TestResizeBackupStorageWithoutInClusterMinIO(t *testing.T) {
	_, err := resizeForTest(fake.NewClientset(), "30Gi")
	assert.ErrorContains(t, err, "no in-cluster MinIO volume")
}

func TestResizeBackupStorageRejectsAnInvalidSize(t *testing.T) {
	_, err := resizeForTest(growingClientset(minioClaim("10Gi", "10Gi")), "thirty")
	assert.ErrorContains(t, err, "invalid --size")
}
