package controllers

import (
	"context"
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8slabels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/labels"
	"github.com/getkipper/kipper/controller/pkg/platform"
)

// A volume holder keeps a shared claim mounted so Velero can back up its files
// even when no app mounts it. Without a running pod mounting the claim, backups
// include only its Kubernetes objects. ReadWriteMany lets the holder run
// permanently alongside apps that mount the same claim.
const (
	conditionBackupHolderReady = "BackupHolderReady"

	volumeHolderPrefix = "volume-holder-"

	// volumeHolderImage uses the same digest-pinned BusyBox image as the node-join check.
	volumeHolderImage = "docker.io/library/busybox:1.37.0@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e"

	// volumeHolderUser is BusyBox's nobody user and group.
	volumeHolderUser int64 = 65534

	volumeHolderMountName = "data"

	// veleroRestoreContainer is the init container Velero adds to a restored
	// pod that carries volume data. It waits until the data is written.
	veleroRestoreContainer = "restore-wait"

	// holderRetryInterval covers changes that produce no watched event,
	// such as quota becoming available.
	holderRetryInterval = time.Minute

	// holderStartGrace limits how long a holder may start or terminate before
	// the Volume reports a problem. An active restore has no start deadline.
	holderStartGrace = 10 * time.Minute
)

// volumeHolderCommand sleeps until the pod is stopped and exits at once on
// SIGTERM, which a shell running as PID 1 would otherwise ignore.
var volumeHolderCommand = []string{"sh", "-c", "trap 'exit 0' TERM; while true; do sleep 3600 & wait $!; done"}

func sharedClaimName(volume string) string { return "shared-" + volume }

func volumeHolderName(volume string) string { return volumeHolderPrefix + volume }

func volumeHolderLabels(volume string) map[string]string {
	return map[string]string{
		kipperLabel:         kipperValue,
		labels.ResourceType: labels.ResourceTypeVolumeHolder,
		labels.VolumeName:   volume,
	}
}

// PodCacheNamespaces scopes the controller manager's Pod cache to every pod in
// the monitoring namespace, which the OOM watcher reads, and to volume holders
// in every other namespace. Controllers must use an API reader for other pods.
func PodCacheNamespaces() map[string]cache.Config {
	holders := k8slabels.SelectorFromSet(k8slabels.Set{
		kipperLabel:         kipperValue,
		labels.ResourceType: labels.ResourceTypeVolumeHolder,
	})
	return map[string]cache.Config{
		monitoringNamespace: {},
		cache.AllNamespaces: {LabelSelector: holders},
	}
}

// buildVolumeHolder returns the holder pod for vol, without an owner reference.
//
// The holder container mounts the claim read-only. The volume source stays
// writable so Velero's restore container can write to the same volume. Omitting
// fsGroup avoids recursive changes to file ownership on the volume.
func buildVolumeHolder(vol *kipperv1.Volume) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      volumeHolderName(vol.Name),
			Namespace: vol.Namespace,
			Labels:    volumeHolderLabels(vol.Name),
		},
		Spec: corev1.PodSpec{
			AutomountServiceAccountToken:  ptr.To(false),
			EnableServiceLinks:            ptr.To(false),
			TerminationGracePeriodSeconds: ptr.To(int64(5)),
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   ptr.To(true),
				RunAsUser:      ptr.To(volumeHolderUser),
				RunAsGroup:     ptr.To(volumeHolderUser),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:    "holder",
				Image:   volumeHolderImage,
				Command: volumeHolderCommand,
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: ptr.To(false),
					ReadOnlyRootFilesystem:   ptr.To(true),
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("1m"),
						corev1.ResourceMemory: resource.MustParse("8Mi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("10m"),
						corev1.ResourceMemory: resource.MustParse("16Mi"),
					},
				},
				VolumeMounts: []corev1.VolumeMount{{Name: volumeHolderMountName, MountPath: "/data", ReadOnly: true}},
			}},
			Volumes: []corev1.Volume{{
				Name: volumeHolderMountName,
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: sharedClaimName(vol.Name)},
				},
			}},
		},
	}
}

// volumeForHolder maps Kipper holder labels to a Volume in the same namespace.
// It returns nil when the required labels are missing or do not match.
func volumeForHolder(_ context.Context, obj client.Object) []reconcile.Request {
	l := obj.GetLabels()
	if l[kipperLabel] != kipperValue || l[labels.ResourceType] != labels.ResourceTypeVolumeHolder || l[labels.VolumeName] == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: l[labels.VolumeName]}}}
}

// claimProvenance checks ownership of pvc and returns a reason on failure.
// Ownerless claims, including restored claims, require Kipper's labels for vol.
func claimProvenance(pvc *corev1.PersistentVolumeClaim, vol *kipperv1.Volume) (bool, string) {
	if ref := metav1.GetControllerOf(pvc); ref != nil {
		return controllerRefNames(ref, vol, "Volume")
	}
	l := pvc.Labels
	if l[kipperLabel] != kipperValue || l[labels.ResourceType] != labels.ResourceTypeSharedVolume || l[labels.VolumeName] != vol.Name {
		return false, "was not created by Kipper for this volume"
	}
	return true, ""
}

// claimUnsupported names what makes pvc unfit for a permanent holder, or
// returns "" when it is a ReadWriteMany filesystem claim on the shared class.
func claimUnsupported(pvc *corev1.PersistentVolumeClaim) string {
	if !slices.Contains(pvc.Spec.AccessModes, corev1.ReadWriteMany) {
		return "is not ReadWriteMany"
	}
	if pvc.Spec.VolumeMode != nil && *pvc.Spec.VolumeMode != corev1.PersistentVolumeFilesystem {
		return "is a block volume"
	}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != platform.LonghornSharedClass {
		return fmt.Sprintf("does not use the %q storage class", platform.LonghornSharedClass)
	}
	return ""
}

// holderProvenance checks ownership of pod and returns a reason on failure.
// An ownerless pod requires Kipper's holder labels for vol and a reference to
// vol's claim. The pod's name alone is insufficient evidence of ownership.
func holderProvenance(pod *corev1.Pod, vol *kipperv1.Volume) (bool, string) {
	if ref := metav1.GetControllerOf(pod); ref != nil {
		return controllerRefNames(ref, vol, "Volume")
	}
	l := pod.Labels
	if l[kipperLabel] != kipperValue || l[labels.ResourceType] != labels.ResourceTypeVolumeHolder || l[labels.VolumeName] != vol.Name {
		return false, "was not created by Kipper as this volume's holder"
	}
	claim := sharedClaimName(vol.Name)
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == claim {
			return true, ""
		}
	}
	return false, fmt.Sprintf("does not mount the claim %s", claim)
}

// reconcileHolder maintains vol's holder and sets its BackupHolderReady
// condition for the caller to persist. It returns a retry delay, or zero to
// wait for a watched event, and any API error that prevents reconciliation.
//
// Pending holders are preserved so Velero's init container can finish restoring
// their data. Replacing one would interrupt the restore. Velero restores pods
// before custom resources, so a restored holder can precede its Volume. An
// ownerless holder is adopted once its labels and claim establish ownership.
func (r *VolumeReconciler) reconcileHolder(ctx context.Context, vol *kipperv1.Volume, pvc *corev1.PersistentVolumeClaim) (time.Duration, error) {
	if ok, why := claimProvenance(pvc, vol); !ok {
		r.setHolderCondition(vol, "ForeignClaim", fmt.Sprintf(
			"The claim %s %s, so no backup holder runs for it and its files are backed up only while an app mounts it.", pvc.Name, why))
		return 0, nil
	}
	if why := claimUnsupported(pvc); why != "" {
		r.setHolderCondition(vol, "UnsupportedClaim", fmt.Sprintf(
			"The claim %s %s, so no backup holder runs for it and its files are only backed up while an app mounts it.", pvc.Name, why))
		return 0, nil
	}

	key := types.NamespacedName{Namespace: vol.Namespace, Name: volumeHolderName(vol.Name)}
	var pod corev1.Pod
	err := r.Get(ctx, key, &pod)
	if errors.IsNotFound(err) {
		desired := buildVolumeHolder(vol)
		if err := controllerutil.SetControllerReference(vol, desired, r.Scheme); err != nil {
			return 0, err
		}
		err = r.Create(ctx, desired)
		switch {
		case err == nil:
			r.setHolderCondition(vol, "Starting", "The backup holder was created and is starting.")
			return 0, nil
		case errors.IsAlreadyExists(err):
			// Outside monitoring, the cache holds only labelled holders.
			// Read directly to check for a name collision with an uncached pod.
			if err := r.apiReader().Get(ctx, key, &pod); err != nil {
				return 0, err
			}
		case errors.IsForbidden(err) || errors.IsInvalid(err):
			r.setHolderCondition(vol, "CreateRejected", fmt.Sprintf(
				"The cluster refused the backup holder, so the volume's files are backed up only while an app mounts it: %v", err))
			return holderRetryInterval, nil
		default:
			return 0, fmt.Errorf("creating backup holder: %w", err)
		}
	} else if err != nil {
		return 0, err
	}

	if ok, why := holderProvenance(&pod, vol); !ok {
		r.setHolderCondition(vol, "NameTaken", fmt.Sprintf(
			"The pod %s %s. Remove or rename that pod so the volume's backup holder can run.", pod.Name, why))
		return holderRetryInterval, nil
	}
	if metav1.GetControllerOf(&pod) == nil {
		if err := controllerutil.SetControllerReference(vol, &pod, r.Scheme); err != nil {
			return 0, err
		}
		if err := r.Update(ctx, &pod); err != nil {
			return 0, fmt.Errorf("adopting backup holder: %w", err)
		}
	}

	ended := pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded
	if (ended || pod.DeletionTimestamp != nil) && restoreUnfinished(&pod) {
		r.setHolderCondition(vol, "RestoreInterrupted",
			"The restored backup holder stopped before its restore finished, so the volume's files may be incomplete. kip backup restore reports the restore failure. Restore again if it failed.")
	}

	switch {
	case pod.DeletionTimestamp != nil:
		// A pod on a node that is down stays terminating until the node
		// returns or is removed, and the holder's name cannot be reused
		// meanwhile.
		pending := time.Since(pod.DeletionTimestamp.Time)
		if pending >= holderStartGrace {
			r.setHolderCondition(vol, "StuckTerminating", fmt.Sprintf(
				"The backup holder has been terminating for more than %s, usually because its node is unreachable. It is replaced once the node is back or removed with kip node remove.", holderStartGrace))
			return 0, nil
		}
		if !restoreUnfinished(&pod) {
			r.setHolderCondition(vol, "Starting", "The backup holder is being replaced.")
		}
		return holderStartGrace - pending, nil
	case ended:
		// A terminal pod cannot restart or support backups or restores.
		// Deletion triggers reconciliation to create its replacement.
		uid := pod.UID
		if err := r.Delete(ctx, &pod, client.Preconditions{UID: &uid}); err != nil && !errors.IsNotFound(err) {
			return 0, fmt.Errorf("removing ended backup holder: %w", err)
		}
		r.setHolderCondition(vol, "Starting", "The backup holder ended and is being replaced.")
		return 0, nil
	case podReady(&pod):
		r.setHolderReady(vol)
		return 0, nil
	}
	return r.reportHolderNotReady(vol, &pod), nil
}

// reportHolderNotReady records why a holder that has not ended is not ready
// and returns when to look again if no pod event will arrive first.
func (r *VolumeReconciler) reportHolderNotReady(vol *kipperv1.Volume, pod *corev1.Pod) time.Duration {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Reason == corev1.PodReasonUnschedulable {
			r.setHolderCondition(vol, "Unschedulable", "The backup holder cannot be scheduled: "+c.Message)
			return 0
		}
	}
	if name, waiting := failingContainer(pod); waiting != nil {
		r.setHolderCondition(vol, "ContainerFailing", fmt.Sprintf(
			"The backup holder's container %s is failing: %s %s", name, waiting.Reason, waiting.Message))
		return 0
	}
	if restoreUnfinished(pod) {
		r.setHolderCondition(vol, "Restoring", "The backup holder is waiting for a restore to write the volume's files.")
		return 0
	}
	var age time.Duration
	if !pod.CreationTimestamp.IsZero() {
		age = time.Since(pod.CreationTimestamp.Time)
	}
	if age >= holderStartGrace {
		r.setHolderCondition(vol, "NotStarting", fmt.Sprintf(
			"The backup holder has not started within %s. Its pod is %s.", holderStartGrace, pod.Status.Phase))
		return 0
	}
	r.setHolderCondition(vol, "Starting", "The backup holder is starting.")
	return holderStartGrace - age
}

// failingContainer returns the first container waiting for a non-empty reason
// other than ContainerCreating or PodInitializing, checking init containers
// first. It returns an empty name and nil when none matches.
func failingContainer(pod *corev1.Pod) (string, *corev1.ContainerStateWaiting) {
	for _, statuses := range [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses} {
		for _, cs := range statuses {
			w := cs.State.Waiting
			if w != nil && w.Reason != "" && w.Reason != "ContainerCreating" && w.Reason != "PodInitializing" {
				return cs.Name, w
			}
		}
	}
	return "", nil
}

// restoreUnfinished reports whether pod carries Velero's restore container and
// that container has not finished successfully.
func restoreUnfinished(pod *corev1.Pod) bool {
	if !hasInitContainer(pod, veleroRestoreContainer) {
		return false
	}
	for _, cs := range pod.Status.InitContainerStatuses {
		if cs.Name == veleroRestoreContainer && cs.State.Terminated != nil && cs.State.Terminated.ExitCode == 0 {
			return false
		}
	}
	return true
}

func (r *VolumeReconciler) apiReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *VolumeReconciler) setHolderReady(vol *kipperv1.Volume) {
	apimeta.SetStatusCondition(&vol.Status.Conditions, metav1.Condition{
		Type:               conditionBackupHolderReady,
		Status:             metav1.ConditionTrue,
		Reason:             "Running",
		Message:            "The backup holder is running, so backups copy the volume's files.",
		ObservedGeneration: vol.Generation,
	})
}

// setHolderCondition sets BackupHolderReady to false. When the condition
// changes and a recorder is available, reasons other than Starting or Restoring
// also emit a warning event to draw the operator's attention.
func (r *VolumeReconciler) setHolderCondition(vol *kipperv1.Volume, reason, message string) {
	changed := apimeta.SetStatusCondition(&vol.Status.Conditions, metav1.Condition{
		Type:               conditionBackupHolderReady,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: vol.Generation,
	})
	if changed && r.Recorder != nil && reason != "Starting" && reason != "Restoring" {
		r.Recorder.Event(vol, corev1.EventTypeWarning, "BackupHolder"+reason, message)
	}
}

func hasInitContainer(pod *corev1.Pod, name string) bool {
	for _, c := range pod.Spec.InitContainers {
		if c.Name == name {
			return true
		}
	}
	return false
}
