package controllers

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8slabels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
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
	// the owning resource reports a problem. A restore has its own deadline.
	holderStartGrace = 10 * time.Minute

	// holderRestoreDeadline is the maximum pod age before a holder with an
	// unfinished restore is replaced.
	holderRestoreDeadline = 24 * time.Hour
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

// holderTarget names a holder pod, the shared claim it keeps mounted and the
// Kipper object that controls the pod. claimOwned checks that the claim is
// the one Kipper created for owner and returns a reason when it is not.
type holderTarget struct {
	owner      client.Object
	kind       string
	namespace  string
	pod        string
	claim      string
	labels     map[string]string
	claimOwned func(*corev1.PersistentVolumeClaim) (bool, string)
}

func volumeHolderTarget(vol *kipperv1.Volume) holderTarget {
	return holderTarget{
		owner: vol, kind: "Volume", namespace: vol.Namespace,
		pod: volumeHolderName(vol.Name), claim: sharedClaimName(vol.Name), labels: volumeHolderLabels(vol.Name),
		claimOwned: func(pvc *corev1.PersistentVolumeClaim) (bool, string) { return claimProvenance(pvc, vol) },
	}
}

// PodCacheNamespaces scopes the controller manager's Pod cache to every pod in
// the monitoring namespace, which the OOM watcher reads, and to shared-volume
// and project shared-storage holders in every other namespace. Controllers
// must use an API reader for other pods.
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
func buildVolumeHolder(vol *kipperv1.Volume) *corev1.Pod {
	return buildHolder(volumeHolderTarget(vol))
}

// buildHolder returns the holder pod for t, without an owner reference.
//
// The holder container mounts the claim read-only. The volume source stays
// writable so Velero's restore container can write to the same volume. Omitting
// fsGroup avoids recursive changes to file ownership on the volume.
func buildHolder(t holderTarget) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      t.pod,
			Namespace: t.namespace,
			Labels:    maps.Clone(t.labels),
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
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: t.claim},
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
// returns "" when it is a ReadWriteMany filesystem claim on a Longhorn class.
func claimUnsupported(pvc *corev1.PersistentVolumeClaim) string {
	if !slices.Contains(pvc.Spec.AccessModes, corev1.ReadWriteMany) {
		return "is not ReadWriteMany"
	}
	if pvc.Spec.VolumeMode != nil && *pvc.Spec.VolumeMode != corev1.PersistentVolumeFilesystem {
		return "is a block volume"
	}
	if pvc.Spec.StorageClassName == nil || !platform.IsLonghornClass(*pvc.Spec.StorageClassName) {
		return "does not use a Longhorn storage class"
	}
	return ""
}

// holderProvenance checks ownership of pod and returns a reason on failure.
// An ownerless pod requires every holder label of t and a reference to t's
// claim. The pod's name alone is insufficient evidence of ownership.
func holderProvenance(pod *corev1.Pod, t holderTarget) (bool, string) {
	if ref := metav1.GetControllerOf(pod); ref != nil {
		return controllerRefNames(ref, t.owner, t.kind)
	}
	for k, v := range t.labels {
		if pod.Labels[k] != v {
			return false, fmt.Sprintf("was not created by Kipper as this %s's holder", strings.ToLower(t.kind))
		}
	}
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == t.claim {
			return true, ""
		}
	}
	return false, fmt.Sprintf("does not mount the claim %s", t.claim)
}

// holderReport is where one holder pass records the BackupHolderReady
// condition. event, when set, receives the reason and message of a changed
// condition that needs attention. save, when set, persists the condition
// before a holder is deleted for replacement.
type holderReport struct {
	conditions *[]metav1.Condition
	generation int64
	event      func(reason, message string)
	save       func(context.Context) error
}

// holderOps maintains holder pods with the controller's client and scheme.
// reader reads pods outside the controller's pod cache.
type holderOps struct {
	client client.Client
	reader client.Reader
	scheme *runtime.Scheme
}

func (r *VolumeReconciler) holders() holderOps {
	return holderOps{client: r.Client, reader: r.apiReader(), scheme: r.Scheme}
}

// reconcileHolder maintains vol's holder and sets its BackupHolderReady
// condition for the caller to persist. It returns a retry delay, or zero to
// wait for a watched event, and any API error that prevents reconciliation.
func (r *VolumeReconciler) reconcileHolder(ctx context.Context, vol *kipperv1.Volume, pvc *corev1.PersistentVolumeClaim) (time.Duration, error) {
	st := holderReport{
		conditions: &vol.Status.Conditions,
		generation: vol.Generation,
		save: func(ctx context.Context) error {
			if err := r.Status().Update(ctx, vol); err != nil {
				return fmt.Errorf("saving volume status: %w", err)
			}
			return nil
		},
	}
	if r.Recorder != nil {
		st.event = func(reason, message string) {
			r.Recorder.Event(vol, corev1.EventTypeWarning, "BackupHolder"+reason, message)
		}
	}
	return r.holders().reconcile(ctx, volumeHolderTarget(vol), pvc, st)
}

// reconcile maintains t's holder for pvc and records its state in st. It
// returns a retry delay, or zero to wait for a watched event, and any API
// error that prevents reconciliation.
//
// Pending holders are preserved until the restore deadline so Velero's init
// container can finish restoring their data. Replacing one interrupts the
// restore. Velero restores pods before custom resources, so a restored holder
// can precede its owner. An ownerless holder is adopted once its labels and
// claim establish ownership.
func (h holderOps) reconcile(ctx context.Context, t holderTarget, pvc *corev1.PersistentVolumeClaim, st holderReport) (time.Duration, error) {
	if ok, why := t.claimOwned(pvc); !ok {
		st.setFalse("ForeignClaim", fmt.Sprintf(
			"The claim %s %s, so no backup holder runs for it and its files are backed up only while an app mounts it.", pvc.Name, why))
		return 0, nil
	}
	if why := claimUnsupported(pvc); why != "" {
		st.setFalse("UnsupportedClaim", fmt.Sprintf(
			"The claim %s %s, so no backup holder runs for it and its files are only backed up while an app mounts it.", pvc.Name, why))
		return 0, nil
	}

	key := types.NamespacedName{Namespace: t.namespace, Name: t.pod}
	var pod corev1.Pod
	err := h.client.Get(ctx, key, &pod)
	if errors.IsNotFound(err) {
		desired := buildHolder(t)
		if err := controllerutil.SetControllerReference(t.owner, desired, h.scheme); err != nil {
			return 0, err
		}
		err = h.client.Create(ctx, desired)
		switch {
		case err == nil:
			st.setFalse("Starting", "The backup holder was created and is starting.")
			return 0, nil
		case errors.IsAlreadyExists(err):
			// Outside monitoring, the cache holds only labelled holders.
			// Read directly to check for a name collision with an uncached pod.
			if err := h.reader.Get(ctx, key, &pod); err != nil {
				return 0, err
			}
		case errors.IsForbidden(err) || errors.IsInvalid(err):
			st.setFalse("CreateRejected", fmt.Sprintf(
				"The cluster refused the backup holder, so the volume's files are backed up only while an app mounts it: %v", err))
			return holderRetryInterval, nil
		default:
			return 0, fmt.Errorf("creating backup holder: %w", err)
		}
	} else if err != nil {
		return 0, err
	}

	if ok, why := holderProvenance(&pod, t); !ok {
		st.setFalse("NameTaken", fmt.Sprintf(
			"The pod %s %s. Remove or rename that pod so the volume's backup holder can run.", pod.Name, why))
		return holderRetryInterval, nil
	}
	if metav1.GetControllerOf(&pod) == nil {
		if err := controllerutil.SetControllerReference(t.owner, &pod, h.scheme); err != nil {
			return 0, err
		}
		if err := h.client.Update(ctx, &pod); err != nil {
			return 0, fmt.Errorf("adopting backup holder: %w", err)
		}
	}

	ended := pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded
	if (ended || pod.DeletionTimestamp != nil) && restoreUnfinished(&pod) {
		st.setFalse("RestoreInterrupted",
			"The restored backup holder stopped before its restore finished, so the volume's files may be incomplete. kip backup restore reports the restore failure. Restore again if it failed.")
	}

	switch {
	case pod.DeletionTimestamp != nil:
		// A pod on a node that is down stays terminating until the node
		// returns or is removed, and the holder's name cannot be reused
		// meanwhile.
		pending := time.Since(pod.DeletionTimestamp.Time)
		if pending >= holderStartGrace {
			st.setFalse("StuckTerminating", fmt.Sprintf(
				"The backup holder has been terminating for more than %s, usually because its node is unreachable. It is replaced once the node is back or removed with kip node remove.", holderStartGrace))
			return 0, nil
		}
		if !restoreUnfinished(&pod) {
			st.setFalse("Starting", "The backup holder is being replaced.")
		}
		return holderStartGrace - pending, nil
	case ended:
		// A terminal pod cannot restart or support backups or restores.
		// Deletion triggers reconciliation to create its replacement.
		if err := h.replace(ctx, &pod, st); err != nil {
			return 0, fmt.Errorf("removing ended backup holder: %w", err)
		}
		st.setFalse("Starting", "The backup holder ended and is being replaced.")
		return 0, nil
	case podReady(&pod):
		st.setReady()
		return 0, nil
	case restoreUnfinished(&pod) && holderAge(&pod) >= holderRestoreDeadline:
		// Velero's restore container waits for a done file that a failed
		// volume restore never writes, so the pod would stay in its init
		// container and keep the volume out of later backups.
		st.setFalse("RestoreStuck", fmt.Sprintf(
			"The volume restore is still unfinished %s after the backup holder was created. The backup holder is being replaced so backups include the volume again. Restore again if the files are incomplete.", holderRestoreDeadline))
		if err := h.replace(ctx, &pod, st); err != nil {
			return 0, fmt.Errorf("removing stuck backup holder: %w", err)
		}
		return 0, nil
	}
	return reportHolderNotReady(&pod, st), nil
}

// remove deletes t's holder after verifying ownership. A missing, foreign or
// already deleting pod needs no action.
func (h holderOps) remove(ctx context.Context, t holderTarget) error {
	var pod corev1.Pod
	if err := h.client.Get(ctx, types.NamespacedName{Namespace: t.namespace, Name: t.pod}, &pod); err != nil {
		return client.IgnoreNotFound(err)
	}
	if ok, _ := holderProvenance(&pod, t); !ok || pod.DeletionTimestamp != nil {
		return nil
	}
	uid := pod.UID
	return client.IgnoreNotFound(h.client.Delete(ctx, &pod, client.Preconditions{UID: &uid}))
}

// replace calls st.save, when provided, before deleting pod to trigger
// replacement. Saving first preserves restore warnings before their evidence
// is deleted. If saving fails, the pod remains for the next reconciliation.
func (h holderOps) replace(ctx context.Context, pod *corev1.Pod, st holderReport) error {
	if st.save != nil {
		if err := st.save(ctx); err != nil {
			return err
		}
	}
	uid := pod.UID
	if err := h.client.Delete(ctx, pod, client.Preconditions{UID: &uid}); err != nil && !errors.IsNotFound(err) {
		return err
	}
	return nil
}

// reportHolderNotReady records why a holder that has not ended is not ready
// and returns when to look again if no pod event will arrive first.
func reportHolderNotReady(pod *corev1.Pod, st holderReport) time.Duration {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Reason == corev1.PodReasonUnschedulable {
			st.setFalse("Unschedulable", "The backup holder cannot be scheduled: "+c.Message)
			return 0
		}
	}
	if name, waiting := failingContainer(pod); waiting != nil {
		st.setFalse("ContainerFailing", fmt.Sprintf(
			"The backup holder's container %s is failing: %s %s", name, waiting.Reason, waiting.Message))
		return 0
	}
	age := holderAge(pod)
	if restoreUnfinished(pod) {
		st.setFalse("Restoring", "The backup holder is waiting for a restore to write the volume's files.")
		return holderRestoreDeadline - age
	}
	if age >= holderStartGrace {
		st.setFalse("NotStarting", fmt.Sprintf(
			"The backup holder has not started within %s. Its pod is %s.", holderStartGrace, pod.Status.Phase))
		return 0
	}
	st.setFalse("Starting", "The backup holder is starting.")
	return holderStartGrace - age
}

// holderAge returns the time since pod was created, or zero if its creation
// time is unset.
func holderAge(pod *corev1.Pod) time.Duration {
	if pod.CreationTimestamp.IsZero() {
		return 0
	}
	return time.Since(pod.CreationTimestamp.Time)
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

func (st holderReport) setReady() {
	apimeta.SetStatusCondition(st.conditions, metav1.Condition{
		Type:               conditionBackupHolderReady,
		Status:             metav1.ConditionTrue,
		Reason:             "Running",
		Message:            "The backup holder is running, so backups copy the volume's files.",
		ObservedGeneration: st.generation,
	})
}

// setFalse sets BackupHolderReady to false, preserving RestoreStuck and
// RestoreInterrupted warnings when the new reason is Starting. When the
// condition changes, reasons other than Starting or Restoring also go to
// st.event, when provided, to draw the operator's attention.
func (st holderReport) setFalse(reason, message string) {
	if reason == "Starting" {
		if c := apimeta.FindStatusCondition(*st.conditions, conditionBackupHolderReady); c != nil &&
			(c.Reason == "RestoreStuck" || c.Reason == "RestoreInterrupted") {
			return
		}
	}
	changed := apimeta.SetStatusCondition(st.conditions, metav1.Condition{
		Type:               conditionBackupHolderReady,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: st.generation,
	})
	if changed && st.event != nil && reason != "Starting" && reason != "Restoring" {
		st.event(reason, message)
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
