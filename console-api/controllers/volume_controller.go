package controllers

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/platform"
)

const volumeFinalizer = "kipper.run/volume-cleanup"

// VolumeReconciler reconciles a Volume CR.
type VolumeReconciler struct {
	client.Client
	APIReader client.Reader
	Scheme    *runtime.Scheme
	Recorder  record.EventRecorder
}

func (r *VolumeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var vol kipperv1.Volume
	if err := r.Get(ctx, req.NamespacedName, &vol); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !vol.DeletionTimestamp.IsZero() {
		logger.Info("cleaning up volume resources", "volume", vol.Name)
		// Detach the volume from every app before releasing the finalizer.
		// A stale App.spec.volumes entry keeps rendering pods that use the
		// PVC, and pvc-protection then blocks the owner-reference cascade —
		// the claim would sit in Terminating forever.
		vol.Spec.Mounts = nil
		if err := r.syncAppVolumes(ctx, &vol); err != nil {
			return ctrl.Result{}, fmt.Errorf("detaching volume from apps: %w", err)
		}
		controllerutil.RemoveFinalizer(&vol, volumeFinalizer)
		return ctrl.Result{}, r.Update(ctx, &vol)
	}

	if !controllerutil.ContainsFinalizer(&vol, volumeFinalizer) {
		controllerutil.AddFinalizer(&vol, volumeFinalizer)
		if err := r.Update(ctx, &vol); err != nil {
			return ctrl.Result{}, err
		}
	}

	mode, err := clusterStorageMode(ctx, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !platform.SupportsSharedVolumes(mode) {
		return ctrl.Result{}, r.reportSharedStorageUnsupported(ctx, &vol)
	}

	pvc, err := r.reconcilePVC(ctx, &vol, mode)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling pvc: %w", err)
	}

	backups, err := clusterBackupsConfigured(ctx, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}
	var retry time.Duration
	switch {
	case pvc == nil:
	case backups:
		if retry, err = r.reconcileHolder(ctx, &vol, pvc); err != nil {
			return ctrl.Result{}, fmt.Errorf("reconciling backup holder: %w", err)
		}
	default:
		if err := r.holders().remove(ctx, volumeHolderTarget(&vol)); err != nil {
			return ctrl.Result{}, fmt.Errorf("removing backup holder: %w", err)
		}
		apimeta.RemoveStatusCondition(&vol.Status.Conditions, conditionBackupHolderReady)
	}

	if err := r.syncAppVolumes(ctx, &vol); err != nil {
		return ctrl.Result{}, fmt.Errorf("syncing app volumes: %w", err)
	}

	if err := r.updateStatus(ctx, &vol); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status: %w", err)
	}

	// Polling keeps copy status current because Longhorn replicas and nodes are not watched.
	retry = earliest(retry, storageRequeue(vol.Status.Conditions))
	return ctrl.Result{RequeueAfter: retry}, nil
}

// syncAppVolumes ensures each App CR referenced in Volume.Spec.Mounts has a
// matching entry in App.Spec.Volumes. This keeps the App CR in sync regardless
// of whether the mount was created via the API handler or another path.
func (r *VolumeReconciler) syncAppVolumes(ctx context.Context, vol *kipperv1.Volume) error {
	// Build set of apps that should have this volume
	wantMounted := make(map[string]string, len(vol.Spec.Mounts))
	for _, m := range vol.Spec.Mounts {
		wantMounted[m.App] = m.MountPath
	}

	// List all apps in the namespace and reconcile their Spec.Volumes
	var appList kipperv1.AppList
	if err := r.List(ctx, &appList, client.InNamespace(vol.Namespace)); err != nil {
		return fmt.Errorf("listing apps: %w", err)
	}

	for i := range appList.Items {
		app := &appList.Items[i]
		mountPath, shouldMount := wantMounted[app.Name]

		hasMount := false
		pathCurrent := false
		for _, vm := range app.Spec.Volumes {
			if vm.Name == vol.Name {
				hasMount = true
				pathCurrent = vm.MountPath == mountPath
				break
			}
		}

		switch {
		case shouldMount && hasMount && !pathCurrent:
			for j := range app.Spec.Volumes {
				if app.Spec.Volumes[j].Name == vol.Name {
					app.Spec.Volumes[j].MountPath = mountPath
				}
			}
			if err := r.Update(ctx, app); err != nil {
				return fmt.Errorf("updating volume path on app %s: %w", app.Name, err)
			}
		case shouldMount && !hasMount:
			app.Spec.Volumes = append(app.Spec.Volumes, kipperv1.AppVolumeMount{
				Name:      vol.Name,
				MountPath: mountPath,
			})
			if err := r.Update(ctx, app); err != nil {
				return fmt.Errorf("adding volume to app %s: %w", app.Name, err)
			}
		case !shouldMount && hasMount:
			var filtered []kipperv1.AppVolumeMount
			for _, vm := range app.Spec.Volumes {
				if vm.Name != vol.Name {
					filtered = append(filtered, vm)
				}
			}
			app.Spec.Volumes = filtered
			if err := r.Update(ctx, app); err != nil {
				return fmt.Errorf("removing volume from app %s: %w", app.Name, err)
			}
		}
	}

	return nil
}

// reportSharedStorageUnsupported persists a Pending phase and the reason shared
// storage is unavailable, returning any status update error.
func (r *VolumeReconciler) reportSharedStorageUnsupported(ctx context.Context, vol *kipperv1.Volume) error {
	vol.Status.Phase = "Pending"
	apimeta.SetStatusCondition(&vol.Status.Conditions, metav1.Condition{
		Type:               conditionSharedStorageUnsupported,
		Status:             metav1.ConditionTrue,
		Reason:             "LocalStorage",
		Message:            platform.SharedVolumesUnsupported,
		ObservedGeneration: vol.Generation,
	})
	return r.Status().Update(ctx, vol)
}

// reconcilePVC returns vol's claim, creating it if missing. It adopts ownerless
// claims, including restored claims, when Kipper's labels establish ownership.
// Existing claims otherwise remain unchanged.
//
// A new claim follows the copies creation gate: it returns nil without a claim
// while the gate holds creation back, with the reason in the volume's
// StorageCopies condition. A restored volume asking for two copies gets a
// single-copy claim while shared copies are not qualified, so its data comes
// back, and keeps its copies as intent.
func (r *VolumeReconciler) reconcilePVC(ctx context.Context, vol *kipperv1.Volume, mode string) (*corev1.PersistentVolumeClaim, error) {
	pvcName := sharedClaimName(vol.Name)
	storageClass := platform.ClaimClassForCopies(mode, vol.Spec.Copies)

	desired := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName,
			Namespace: vol.Namespace,
			Labels: map[string]string{
				kipperLabel:                kipperValue,
				"kipper.run/resource-type": "shared-volume",
				"kipper.run/volume-name":   vol.Name,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			StorageClassName: &storageClass,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse(vol.Spec.Size),
				},
			},
		},
	}

	if err := controllerutil.SetControllerReference(vol, desired, r.Scheme); err != nil {
		return nil, err
	}

	var existing corev1.PersistentVolumeClaim
	err := r.Get(ctx, types.NamespacedName{Name: pvcName, Namespace: vol.Namespace}, &existing)
	if errors.IsNotFound(err) {
		restored := vol.Labels[veleroRestoreNameLabel] != ""
		reason, message, err := creationGate(ctx, r.longhornReader(), mode, vol.Spec.Copies, true, restored)
		if err != nil {
			return nil, err
		}
		if reason != "" {
			if !restored || reason != kipperv1.StorageCopiesRWXNotQualified {
				setCopiesCondition(&vol.Status.Conditions, metav1.Condition{
					Type: kipperv1.ConditionStorageCopies, Status: metav1.ConditionFalse, Reason: reason, Message: message,
				}, vol.Generation)
				return nil, nil
			}
			single := platform.SingleWriterClass(mode)
			desired.Spec.StorageClassName = &single
		}
		if err := r.Create(ctx, desired); err != nil {
			return nil, err
		}
		return desired, nil
	}
	if err != nil {
		return nil, err
	}
	if metav1.GetControllerOf(&existing) == nil {
		if ok, _ := claimProvenance(&existing, vol); ok {
			if err := controllerutil.SetControllerReference(vol, &existing, r.Scheme); err != nil {
				return nil, err
			}
			if err := r.Update(ctx, &existing); err != nil {
				return nil, fmt.Errorf("adopting claim: %w", err)
			}
		}
	}
	return &existing, nil
}

func (r *VolumeReconciler) updateStatus(ctx context.Context, vol *kipperv1.Volume) error {
	pvcName := sharedClaimName(vol.Name)

	var pvc corev1.PersistentVolumeClaim
	err := r.Get(ctx, types.NamespacedName{Name: pvcName, Namespace: vol.Namespace}, &pvc)
	if errors.IsNotFound(err) {
		vol.Status.Phase = "Pending"
		return r.Status().Update(ctx, vol)
	}
	if err != nil {
		return err
	}
	if err := r.reportStorageCopies(ctx, vol, &pvc); err != nil {
		return err
	}

	switch pvc.Status.Phase {
	case corev1.ClaimBound:
		vol.Status.Phase = "Bound"
	case corev1.ClaimPending:
		vol.Status.Phase = "Pending"
	default:
		vol.Status.Phase = "Pending"
	}

	if qty, ok := pvc.Status.Capacity[corev1.ResourceStorage]; ok {
		vol.Status.ActualSize = qty.String()
	}

	// Track which apps have this mounted
	var mountedApps []string
	for _, m := range vol.Spec.Mounts {
		mountedApps = append(mountedApps, m.App)
	}
	vol.Status.MountedApps = mountedApps

	return r.Status().Update(ctx, vol)
}

// SetupWithManager watches Volumes, their claims, and holder pods. It also watches
// Longhorn volumes when the kind and permissions are available at startup.
// Holders are mapped by label so restored, ownerless holders also trigger
// reconciliation of their Volume. The manager's Pod cache must include holder
// pods, as configured by PodCacheNamespaces.
func (r *VolumeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b, _ := watchLonghornVolumes(mgr, ctrl.NewControllerManagedBy(mgr).
		For(&kipperv1.Volume{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(volumeForHolder)), volumeForLonghornVolume)
	return b.Complete(r)
}
