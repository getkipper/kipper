package controllers

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/platform"
	"github.com/getkipper/kipper/controller/pkg/servicecatalog"
	"github.com/getkipper/kipper/controller/pkg/stranded"
)

const dumpVolumeFaulted = "Faulted"

// reportDumpVolume sets DumpVolumeReady to false while the service's dump
// claim is controlled by the service and uses a faulted Longhorn volume. It
// removes the condition otherwise and leaves claim deletion to the user.
// For an owned claim with consistent backups enabled, it returns pendingPodRetry
// during deletion so reconciliation can create a replacement promptly, or
// storageRefreshUnsettled for a non-local claim. Polling detects faults while
// the pod is running because the controller has no Longhorn watch. Otherwise,
// it returns zero. Read and status update errors are returned to the caller.
func (r *ServiceReconciler) reportDumpVolume(ctx context.Context, svc *kipperv1.Service) (time.Duration, error) {
	before := svc.Status.DeepCopy()
	faulted, retry, err := r.dumpVolumeFaulted(ctx, svc)
	if err != nil {
		return 0, err
	}
	if faulted {
		apimeta.SetStatusCondition(&svc.Status.Conditions, metav1.Condition{
			Type: servicecatalog.ConditionDumpVolumeReady, Status: metav1.ConditionFalse, Reason: dumpVolumeFaulted,
			Message: fmt.Sprintf("The dump volume is faulted, so consistent dumps fail and the database pod cannot start until the dump volume is replaced. "+
				"A pod that is running keeps running until it restarts. "+
				"Recreating the dump volume leaves the data volume unchanged: kip service recreate-dump %s", svc.Name),
			ObservedGeneration: svc.Generation,
		})
	} else {
		apimeta.RemoveStatusCondition(&svc.Status.Conditions, servicecatalog.ConditionDumpVolumeReady)
	}
	return retry, r.writeStatusIfChanged(ctx, svc, before)
}

func (r *ServiceReconciler) dumpVolumeFaulted(ctx context.Context, svc *kipperv1.Service) (faulted bool, retry time.Duration, err error) {
	if !wantsConsistentBackups(svc) {
		return false, 0, nil
	}
	var pvc corev1.PersistentVolumeClaim
	if err := r.Get(ctx, types.NamespacedName{Namespace: svc.Namespace, Name: dumpClaimName(svc.Name)}, &pvc); err != nil {
		return false, 0, client.IgnoreNotFound(err)
	}
	if owner := metav1.GetControllerOf(&pvc); owner == nil || owner.UID != svc.UID {
		return false, 0, nil
	}
	if !pvc.DeletionTimestamp.IsZero() {
		return false, pendingPodRetry, nil
	}
	if pvc.Spec.StorageClassName != nil && *pvc.Spec.StorageClassName == platform.LocalPathClass {
		return false, 0, nil
	}
	robustness, err := longhornRobustness(ctx, r.longhornReader(), &pvc)
	if err != nil {
		return false, 0, err
	}
	return robustness == "faulted", storageRefreshUnsettled, nil
}

// dumpVolumeRefusal explains why a faulted dump volume blocks a copy change:
// the database could not restart afterwards. It returns "" otherwise and
// propagates read errors.
func (r *ServiceReconciler) dumpVolumeRefusal(ctx context.Context, svc *kipperv1.Service) (string, error) {
	faulted, _, err := r.dumpVolumeFaulted(ctx, svc)
	if err != nil || !faulted {
		return "", err
	}
	return fmt.Sprintf("The dump volume is faulted, so %s could not start again after the change. "+
		"Replace it with 'kip service recreate-dump %s'. The change continues once the new dump volume is ready.", svc.Name, svc.Name), nil
}

// longhornRobustness reads status.robustness of the Longhorn volume behind
// pvc, found through the bound PV's CSI volume handle. It returns "" for a
// local-path or unbound claim, a missing or non-Longhorn PV, a PV bound to
// another claim or to an earlier claim of the same name, a missing Longhorn
// volume or kind, and a Longhorn volume whose claim reference names another
// claim.
func longhornRobustness(ctx context.Context, reader client.Reader, pvc *corev1.PersistentVolumeClaim) (string, error) {
	if pvc.Spec.VolumeName == "" || (pvc.Spec.StorageClassName != nil && *pvc.Spec.StorageClassName == platform.LocalPathClass) {
		return "", nil
	}
	var pv corev1.PersistentVolume
	if err := reader.Get(ctx, types.NamespacedName{Name: pvc.Spec.VolumeName}, &pv); err != nil {
		if absentKind(err) {
			return "", nil
		}
		return "", fmt.Errorf("reading PV %s: %w", pvc.Spec.VolumeName, err)
	}
	ref := pv.Spec.ClaimRef
	if ref == nil || ref.Namespace != pvc.Namespace || ref.Name != pvc.Name || ref.UID != pvc.UID || pv.Spec.CSI == nil || pv.Spec.CSI.Driver != stranded.LonghornDriver {
		return "", nil
	}
	handle := pv.Spec.CSI.VolumeHandle
	volume := &unstructured.Unstructured{}
	volume.SetGroupVersionKind(longhornVolumeKind)
	if err := reader.Get(ctx, types.NamespacedName{Namespace: longhornNamespace, Name: handle}, volume); err != nil {
		if absentKind(err) {
			return "", nil
		}
		return "", fmt.Errorf("reading Longhorn volume %s: %w", handle, err)
	}
	ns, _, _ := unstructured.NestedString(volume.Object, "status", "kubernetesStatus", "namespace")
	claim, _, _ := unstructured.NestedString(volume.Object, "status", "kubernetesStatus", "pvcName")
	if ns != pvc.Namespace || claim != pvc.Name {
		return "", nil
	}
	robustness, _, _ := unstructured.NestedString(volume.Object, "status", "robustness")
	return robustness, nil
}
