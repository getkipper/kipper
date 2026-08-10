package ai

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake" //nolint:staticcheck // fake clientset is the test seam
)

func mustParseValue(q string) int64 {
	parsed := resource.MustParse(q)
	return parsed.Value()
}

func minioPVC(capacity, request string) *corev1.PersistentVolumeClaim {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: minioPVCName, Namespace: veleroNamespace},
		Spec: corev1.PersistentVolumeClaimSpec{
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{},
			},
		},
		Status: corev1.PersistentVolumeClaimStatus{
			Capacity: corev1.ResourceList{},
		},
	}
	if request != "" {
		pvc.Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse(request)
	}
	if capacity != "" {
		pvc.Status.Capacity[corev1.ResourceStorage] = resource.MustParse(capacity)
	}
	return pvc
}

func TestCheckBackupStorageSufficient(t *testing.T) {
	clientset := fake.NewSimpleClientset(minioPVC("30Gi", "30Gi")) //nolint:staticcheck

	report, err := CheckBackupStorage(context.Background(), clientset, "10Gi")
	require.NoError(t, err)

	assert.True(t, report.Sufficient, "30Gi capacity should be enough for 3*10Gi projected")
	assert.Equal(t, mustParseValue("30Gi"), report.MinIOCapacity)
	assert.Equal(t, mustParseValue("30Gi"), report.Projected)
}

func TestCheckBackupStorageInsufficient(t *testing.T) {
	clientset := fake.NewSimpleClientset(minioPVC("5Gi", "5Gi")) //nolint:staticcheck

	report, err := CheckBackupStorage(context.Background(), clientset, "10Gi")
	require.NoError(t, err)

	assert.False(t, report.Sufficient)
	assert.Equal(t, mustParseValue("5Gi"), report.MinIOCapacity)
	assert.Equal(t, mustParseValue("30Gi"), report.Projected)
}

func TestCheckBackupStorageNotFound(t *testing.T) {
	clientset := fake.NewSimpleClientset() //nolint:staticcheck

	_, err := CheckBackupStorage(context.Background(), clientset, "10Gi")
	assert.True(t, errors.Is(err, ErrMinIONotFound))
}

func TestCheckBackupStorageStatusFallback(t *testing.T) {
	// Pending PVC: Status.Capacity empty, but Spec.Resources.Requests is
	// set. The check must use the request as a stand-in.
	clientset := fake.NewSimpleClientset(minioPVC("", "30Gi")) //nolint:staticcheck

	report, err := CheckBackupStorage(context.Background(), clientset, "10Gi")
	require.NoError(t, err)

	assert.True(t, report.Sufficient)
	assert.Equal(t, mustParseValue("30Gi"), report.MinIOCapacity)
}

func TestCheckBackupStorageBadQuantity(t *testing.T) {
	clientset := fake.NewSimpleClientset(minioPVC("30Gi", "30Gi")) //nolint:staticcheck

	_, err := CheckBackupStorage(context.Background(), clientset, "not-a-quantity")
	require.Error(t, err)
}
