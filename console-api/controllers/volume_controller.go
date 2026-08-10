package controllers

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

const volumeFinalizer = "kipper.run/volume-cleanup"

// VolumeReconciler reconciles a Volume CR.
type VolumeReconciler struct {
	client.Client
	Scheme *runtime.Scheme
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

	if err := r.reconcilePVC(ctx, &vol); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling pvc: %w", err)
	}

	if err := r.syncAppVolumes(ctx, &vol); err != nil {
		return ctrl.Result{}, fmt.Errorf("syncing app volumes: %w", err)
	}

	if err := r.updateStatus(ctx, &vol); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status: %w", err)
	}

	return ctrl.Result{}, nil
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

func (r *VolumeReconciler) reconcilePVC(ctx context.Context, vol *kipperv1.Volume) error {
	pvcName := "shared-" + vol.Name
	storageClass := "longhorn"

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
		return err
	}

	var existing corev1.PersistentVolumeClaim
	err := r.Get(ctx, types.NamespacedName{Name: pvcName, Namespace: vol.Namespace}, &existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	return err // PVCs cannot be updated (immutable spec)
}

func (r *VolumeReconciler) updateStatus(ctx context.Context, vol *kipperv1.Volume) error {
	pvcName := "shared-" + vol.Name

	var pvc corev1.PersistentVolumeClaim
	err := r.Get(ctx, types.NamespacedName{Name: pvcName, Namespace: vol.Namespace}, &pvc)
	if errors.IsNotFound(err) {
		vol.Status.Phase = "Pending"
		return r.Status().Update(ctx, vol)
	}
	if err != nil {
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

func (r *VolumeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kipperv1.Volume{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Complete(r)
}
