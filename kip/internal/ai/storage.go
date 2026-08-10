package ai

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// veleroNamespace and minioPVCName name the in-cluster MinIO PVC that
// Velero uses as its BackupStorageLocation. The kip installer creates
// both at install time.
const (
	veleroNamespace = "velero"
	minioPVCName    = "minio-storage"
	// backupSizeMultiplier accounts for one full snapshot at parity with
	// the source plus headroom for incremental layers and Kopia working
	// overhead. Matches the rule of thumb already documented in
	// docs/en/ai.md.
	backupSizeMultiplier = 3
)

// ErrMinIONotFound is returned by CheckBackupStorage when the MinIO PVC
// does not exist. Callers can treat this as a non-error skip when the
// AI bundle is being installed before backup support has been wired in.
var ErrMinIONotFound = errors.New("velero/minio-storage PVC not found")

// StorageReport summarises whether MinIO has enough capacity to hold a
// projected backup of the AI bundle. All sizes are in bytes.
type StorageReport struct {
	MinIOCapacity int64
	Projected     int64
	Sufficient    bool
}

// CheckBackupStorage compares the velero/minio-storage PVC capacity
// against the projected AI bundle backup size. The projection is
// `backupSizeMultiplier * modelCacheSize`; the model cache dominates the
// bundle's footprint so other bundle PVCs are not added separately.
//
// Returns ErrMinIONotFound when the PVC is missing so the caller can
// distinguish "Velero not installed" from "Velero installed but
// undersized".
func CheckBackupStorage(ctx context.Context, clientset kubernetes.Interface, modelCacheSize string) (StorageReport, error) {
	pvc, err := clientset.CoreV1().PersistentVolumeClaims(veleroNamespace).Get(ctx, minioPVCName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return StorageReport{}, ErrMinIONotFound
	}
	if err != nil {
		return StorageReport{}, fmt.Errorf("reading minio PVC: %w", err)
	}

	// Status.Capacity reflects what the storage class actually
	// provisioned once the volume is Bound. Fall back to the
	// requested size if the PVC is still pending so the check is
	// useful before Longhorn finishes the bind.
	capacity, ok := pvc.Status.Capacity[corev1.ResourceStorage]
	if !ok || capacity.IsZero() {
		capacity = pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	}

	cache, err := resource.ParseQuantity(modelCacheSize)
	if err != nil {
		return StorageReport{}, fmt.Errorf("parsing model cache size %q: %w", modelCacheSize, err)
	}

	projected := cache.Value() * backupSizeMultiplier
	return StorageReport{
		MinIOCapacity: capacity.Value(),
		Projected:     projected,
		Sufficient:    capacity.Value() >= projected,
	}, nil
}
