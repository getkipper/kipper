package controllers

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/platform"
)

// monitoringNamespace is where kube-prometheus-stack and loki are installed
// (see kip/internal/installer/observability.go). Pods watched by the OOM
// reconciler must live here; matching on label alone would also catch user
// apps that happen to call themselves "prometheus".
const monitoringNamespace = "monitoring"

// bumpCooldown is the minimum interval between consecutive auto-bumps of the
// same component. Long enough for helm-controller to roll the new memory
// limit and for the pod to actually try the new value before we decide to
// bump again. Without it, a single ongoing OOM event would burn through the
// entire ceiling in a few seconds.
const bumpCooldown = 10 * time.Minute

// componentCeilings caps the auto-bump per component. If the doubled value
// would exceed the ceiling, the bump stops at the ceiling and AtCeiling is
// set in status so the user knows we can't help further automatically.
var componentCeilings = map[string]string{
	componentPrometheus: "4Gi",
	componentLoki:       "2Gi",
}

// PodOOMReconciler watches pods in the monitoring namespace. When a known
// system component (Prometheus, Loki) is OOMKilled, the reconciler bumps the
// corresponding PlatformConfig override so the existing PlatformConfigReconciler
// re-rolls the HelmChart with more memory.
//
// Auto-bump is the failsafe: profile defaults are sized for the typical
// cluster, but if a particular workload pushes Prometheus or Loki past those
// defaults, the OOM watcher reacts without the user having to learn
// kubectl.
type PodOOMReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

func (r *PodOOMReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var pod corev1.Pod
	if err := r.Get(ctx, req.NamespacedName, &pod); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	component, ok := componentForPod(&pod)
	if !ok {
		return ctrl.Result{}, nil
	}
	eventID, hasOOM := oomEventIdentity(&pod)
	if !hasOOM {
		return ctrl.Result{}, nil
	}

	// LastTerminationState.Terminated stays set as OOMKilled long after
	// the actual death — through every routine pod-status update for the
	// rest of the pod's lifetime. A container that previously OOMed but
	// is now running successfully has already been addressed (the pod
	// restarted, or the limit was raised by a profile/manual change in
	// the meantime). Bumping in that state would over-react to a
	// historical event and can push the limit past what the workload
	// actually needs, all the way to the ceiling for nothing.
	if oomedContainerStabilized(&pod) {
		return ctrl.Result{}, nil
	}

	var pc kipperv1.PlatformConfig
	if err := r.Get(ctx, types.NamespacedName{Name: PlatformConfigName}, &pc); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("PlatformConfig not found; cannot record auto-bump",
				"component", component, "pod", pod.Name)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Skip if this exact OOM event has already been processed. A pod's
	// LastTerminationState stays set to OOMKilled long after the actual
	// termination, so every routine status update would otherwise look
	// like a fresh OOM. Using the Terminated.FinishedAt timestamp as the
	// event identity makes dedup correct across reconciler retries and
	// the (longer) cooldown window.
	//
	// Before exiting, repair status audit if a previous status patch
	// failed: the dedup marker landed atomically on spec, but the
	// follow-up Status().Patch may have missed, leaving the user-facing
	// audit fields blank. Trying once more here is cheap and lets the
	// console eventually catch up.
	if alreadyHandled(&pc, component, eventID) {
		if statusAuditMissing(&pc, component) {
			// Bubble the error so controller-runtime requeues us until
			// the audit catches up. Silent failure here means the user
			// loses visibility into an at-ceiling state on the very
			// pod event that triggered it.
			if err := r.repairStatusAudit(ctx, component); err != nil {
				return ctrl.Result{}, fmt.Errorf("repairing status audit for %s: %w", component, err)
			}
		}
		return ctrl.Result{}, nil
	}

	if recentBump(&pc, component) {
		return ctrl.Result{}, nil
	}

	currentLimit := effectiveLimit(&pc, component)
	bumped, atCeiling, err := computeBumpedLimit(currentLimit, componentCeilings[component])
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("computing bumped limit: %w", err)
	}
	if bumped == currentLimit {
		// Already at ceiling; record both the event and the cooldown
		// so a flood of OOM updates doesn't pin the queue and so a
		// different OOM event arriving within the cooldown still
		// debounces correctly. Status update is best-effort audit.
		setLastHandledOOM(&pc, component, eventID)
		setLastBumpAt(&pc, component, time.Now())
		if err := r.Update(ctx, &pc); err != nil {
			return ctrl.Result{}, fmt.Errorf("recording ceiling-hit event: %w", err)
		}
		return ctrl.Result{}, r.recordCeilingHit(ctx, component, currentLimit)
	}

	// Atomic: write the new override, event-identity marker, and
	// bump-at cooldown timestamp in one Update call. If the subsequent
	// status write fails, a retry sees both markers — same event is
	// deduped, and a *different* event within the cooldown window is
	// suppressed too. The status update is then pure user-facing audit.
	now := time.Now()
	setOverride(&pc, component, bumped)
	setLastHandledOOM(&pc, component, eventID)
	setLastBumpAt(&pc, component, now)
	if err := r.Update(ctx, &pc); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating PlatformConfig spec: %w", err)
	}

	nowMeta := metav1.NewTime(now)
	reason := fmt.Sprintf("OOMKilled detected on pod %s", pod.Name)
	if err := r.recordBump(ctx, component, currentLimit, bumped, atCeiling, nowMeta, reason); err != nil {
		// Status carries the audit fields (LastBumpAt etc.) but the
		// dedup marker now lives on spec/metadata, so a status-write
		// failure can't cause a re-bump. Surface the error anyway so
		// controller-runtime requeues and the audit eventually catches up.
		return ctrl.Result{}, fmt.Errorf("recording bump status for %s: %w", component, err)
	}

	logger.Info("auto-bumped memory limit",
		"component", component,
		"from", currentLimit, "to", bumped,
		"atCeiling", atCeiling,
		"pod", pod.Name)

	return ctrl.Result{}, nil
}

// statusAuditMissing returns true when the dedup annotation says we've
// handled an OOM event for this component but the status.components entry
// doesn't reflect the latest bump yet. Indicates a previous reconcile's
// Status().Patch failed after the spec/annotation Update succeeded.
//
// Three things mean "stale": no status entry at all, status LastBumpAt
// older than the annotation timestamp (a newer bump didn't make it into
// status), or AtCeiling disagreeing with the live limit (the audit thinks
// auto-bump still has room when in fact it has run out, hiding the
// incident signal the Platform page surfaces).
func statusAuditMissing(pc *kipperv1.PlatformConfig, component string) bool {
	annoTS := pc.Annotations[lastBumpAtAnnotation(component)]
	if annoTS == "" {
		return false
	}
	annoTime, err := time.Parse(time.RFC3339, annoTS)
	if err != nil {
		return false
	}

	currentLimit := effectiveLimit(pc, component)
	shouldBeAtCeiling := isAtCeiling(component, currentLimit)

	for _, cs := range pc.Status.Components {
		if cs.Name == component {
			if cs.LastBumpAt == nil {
				return true
			}
			if cs.LastBumpAt.Time.Before(annoTime) {
				return true
			}
			if shouldBeAtCeiling && !cs.AtCeiling {
				return true
			}
			return false
		}
	}
	return true
}

// repairStatusAudit re-writes the user-facing audit fields for a component
// using the canonical bump-at timestamp from the annotation. The bump
// from/to limits are not preserved on the annotation, so we record the
// audit-pending entry with the currently-applied limit.
//
// AtCeiling is recomputed from the live state: if the current effective
// limit is at or above the component ceiling, we mark it. This matters on
// the audit-recovery path because losing the AtCeiling flag would hide
// from the operator that auto-bump can't help further — exactly when they
// need to know.
func (r *PodOOMReconciler) repairStatusAudit(ctx context.Context, component string) error {
	var pc kipperv1.PlatformConfig
	if err := r.Get(ctx, types.NamespacedName{Name: PlatformConfigName}, &pc); err != nil {
		return err
	}
	ts := pc.Annotations[lastBumpAtAnnotation(component)]
	if ts == "" {
		return nil
	}
	at, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return fmt.Errorf("parsing %s annotation: %w", lastBumpAtAnnotation(component), err)
	}
	base := pc.DeepCopy()
	limit := effectiveLimit(&pc, component)
	atCeiling := isAtCeiling(component, limit)
	reason := "audit recovered from annotation after a missed status update"
	if atCeiling {
		reason = "at ceiling; auto-bump cannot help further (audit recovered)"
	}
	setBumpRecord(&pc, component, limit, limit, atCeiling, metav1.NewTime(at), reason)
	return r.Status().Patch(ctx, &pc, client.MergeFrom(base))
}

// isAtCeiling returns true when the given memory limit is at or above the
// component's auto-bump ceiling. Used by the audit-repair path to recover
// the AtCeiling state that would otherwise be lost across a failed
// recordCeilingHit / status patch.
func isAtCeiling(component, limit string) bool {
	ceiling := componentCeilings[component]
	if ceiling == "" || limit == "" {
		return false
	}
	cur, err := resource.ParseQuantity(limit)
	if err != nil {
		return false
	}
	ceil, err := resource.ParseQuantity(ceiling)
	if err != nil {
		return false
	}
	return cur.Cmp(ceil) >= 0
}

// recordBump writes the bump audit to PlatformConfig.status using a JSON
// merge patch. The merge patch targets only the status fields we changed
// and doesn't carry a resourceVersion, so the racing PlatformConfigReconciler
// (writing CurrentMemoryLimit on the same status) can't conflict with us.
//
// Even if this call fails, the spec write that preceded it included the
// per-component oom-event-handled annotation, so no re-bump can result.
// The status fields here are audit metadata for the user, not load-bearing.
func (r *PodOOMReconciler) recordBump(ctx context.Context, component, from, to string, atCeiling bool, at metav1.Time, reason string) error {
	var pc kipperv1.PlatformConfig
	if err := r.Get(ctx, types.NamespacedName{Name: PlatformConfigName}, &pc); err != nil {
		return fmt.Errorf("refetching PlatformConfig: %w", err)
	}
	base := pc.DeepCopy()
	setBumpRecord(&pc, component, from, to, atCeiling, at, reason)
	return r.Status().Patch(ctx, &pc, client.MergeFrom(base))
}

// oomEventIdentity returns a stable identifier for the *newest* OOM
// termination across all containers on the pod, or ("", false) if no OOM is
// recorded. On a multi-container pod, an older container's already-handled
// OOMKilled stays sticky in LastTerminationState; we must look past it to
// catch a fresher OOM on a different container.
//
// FinishedAt advances exactly once per real OOM event and stays frozen
// across the routine pod-status updates that follow the restart, so it is
// the right comparison key. Use RFC3339 (not Nano) because metav1.Time
// round-trips through the K8s API at second precision.
func oomEventIdentity(pod *corev1.Pod) (string, bool) {
	var (
		bestContainer string
		bestFinished  time.Time
		bestRestart   int32
		found         bool
	)
	for _, cs := range pod.Status.ContainerStatuses {
		t := cs.LastTerminationState.Terminated
		if t == nil || t.Reason != "OOMKilled" {
			continue
		}
		when := t.FinishedAt.Time
		if when.IsZero() {
			// Synthesize a deterministic order from restart count when
			// the test/older pod doesn't carry FinishedAt. Newer
			// restarts beat older ones for the same container.
			when = time.Unix(int64(cs.RestartCount), 0)
		}
		if !found || when.After(bestFinished) {
			bestContainer = cs.Name
			bestFinished = when
			bestRestart = cs.RestartCount
			found = true
		}
	}
	if !found {
		return "", false
	}
	if !bestFinished.IsZero() && bestFinished.Unix() > 0 {
		return fmt.Sprintf("%s/%s", bestContainer, bestFinished.UTC().Format(time.RFC3339)), true
	}
	return fmt.Sprintf("%s/%s/%d", pod.UID, bestContainer, bestRestart), true
}

// Annotation keys on PlatformConfig used by the OOM watcher. Both live in
// metadata.annotations so they get written atomically with spec.components
// in the single Update call that records a bump — no split-write window
// where status loss could lead to a re-bump.
//
// lastOOMAnnotation:    most recent OOM event identity handled per component.
//
//	Drives event-level dedup so the same OOMKilled
//	container status can't trigger two bumps.
//
// lastBumpAtAnnotation: timestamp of the most recent bump per component.
//
//	Drives the 10-minute cooldown that prevents
//	*different* OOM events from cascading bumps.
//	Status.components[i].LastBumpAt mirrors this for
//	user-facing audit; the annotation is authoritative
//	because it survives a failed Status write.
func lastOOMAnnotation(component string) string {
	return "platform.kipper.run/last-oom-" + component
}
func lastBumpAtAnnotation(component string) string {
	return "platform.kipper.run/last-bump-at-" + component
}

func alreadyHandled(pc *kipperv1.PlatformConfig, component, eventID string) bool {
	if pc.Annotations == nil {
		return false
	}
	return pc.Annotations[lastOOMAnnotation(component)] == eventID
}

func setLastHandledOOM(pc *kipperv1.PlatformConfig, component, eventID string) {
	if pc.Annotations == nil {
		pc.Annotations = map[string]string{}
	}
	pc.Annotations[lastOOMAnnotation(component)] = eventID
}

func setLastBumpAt(pc *kipperv1.PlatformConfig, component string, at time.Time) {
	if pc.Annotations == nil {
		pc.Annotations = map[string]string{}
	}
	pc.Annotations[lastBumpAtAnnotation(component)] = at.UTC().Format(time.RFC3339)
}

// componentForPod maps a pod to its PlatformConfig component name, gated by
// namespace so a user app labelled `app.kubernetes.io/name=prometheus` does
// not accidentally drive cluster sizing.
func componentForPod(pod *corev1.Pod) (string, bool) {
	if pod.Namespace != monitoringNamespace {
		return "", false
	}
	name := pod.Labels["app.kubernetes.io/name"]
	switch name {
	case "prometheus":
		return componentPrometheus, true
	case "loki":
		return componentLoki, true
	}
	return "", false
}

// oomedContainerStabilized reports whether any container with an OOMKilled
// LastTerminationState is currently in state.Running. That combination means
// the pod restarted after the OOM and is now back up, so the OOM no longer
// reflects an active failure. CrashLoopBackOff containers have state.Waiting
// rather than state.Running, so a real ongoing OOM does NOT pass this check.
func oomedContainerStabilized(pod *corev1.Pod) bool {
	for _, cs := range pod.Status.ContainerStatuses {
		term := cs.LastTerminationState.Terminated
		if term == nil || term.Reason != "OOMKilled" {
			continue
		}
		if cs.State.Running != nil {
			return true
		}
	}
	return false
}

func hasOOMKilledContainer(pod *corev1.Pod) bool {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.LastTerminationState.Terminated != nil &&
			cs.LastTerminationState.Terminated.Reason == "OOMKilled" {
			return true
		}
	}
	return false
}

// recentBump returns true when a bump has been recorded for this component
// within the cooldown window. The metadata annotation is authoritative
// because it lands atomically with the spec write that triggered the bump;
// the status field is the user-facing mirror and may legitimately lag.
func recentBump(pc *kipperv1.PlatformConfig, component string) bool {
	if pc.Annotations != nil {
		if s := pc.Annotations[lastBumpAtAnnotation(component)]; s != "" {
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				return time.Since(t) < bumpCooldown
			}
		}
	}
	for _, cs := range pc.Status.Components {
		if cs.Name == component && cs.LastBumpAt != nil {
			return time.Since(cs.LastBumpAt.Time) < bumpCooldown
		}
	}
	return false
}

// effectiveLimit resolves the memory limit currently in effect for a
// component: an explicit override wins, otherwise the profile default.
func effectiveLimit(pc *kipperv1.PlatformConfig, component string) string {
	for _, c := range pc.Spec.Components {
		if c.Name == component && c.MemoryLimit != "" {
			return c.MemoryLimit
		}
	}
	res := platform.ResourcesForProfile(pc.Spec.Profile)
	switch component {
	case componentPrometheus:
		return res.PrometheusMemoryLimit
	case componentLoki:
		return res.LokiMemoryLimit
	}
	return ""
}

// computeBumpedLimit doubles the current limit, capping at the ceiling.
// Returns the formatted new limit and whether the cap was reached.
//
// A current limit already at or above the ceiling returns the original value
// unchanged with atCeiling=true. Lowering a user-set override (e.g. 6Gi above
// a 4Gi auto-bump ceiling) would clobber a manual decision and make an
// incident worse, never better; the auto-bump only grows, never shrinks.
func computeBumpedLimit(current, ceiling string) (string, bool, error) {
	cur, err := resource.ParseQuantity(current)
	if err != nil {
		return "", false, fmt.Errorf("parsing current limit %q: %w", current, err)
	}
	ceil, err := resource.ParseQuantity(ceiling)
	if err != nil {
		return "", false, fmt.Errorf("parsing ceiling %q: %w", ceiling, err)
	}

	if cur.Cmp(ceil) >= 0 {
		return current, true, nil
	}

	doubled := resource.NewQuantity(cur.Value()*2, resource.BinarySI)
	if doubled.Cmp(ceil) >= 0 {
		return ceil.String(), true, nil
	}
	return doubled.String(), false, nil
}

func setOverride(pc *kipperv1.PlatformConfig, component, memoryLimit string) {
	for i, c := range pc.Spec.Components {
		if c.Name == component {
			pc.Spec.Components[i].MemoryLimit = memoryLimit
			return
		}
	}
	pc.Spec.Components = append(pc.Spec.Components, kipperv1.ComponentOverride{
		Name:        component,
		MemoryLimit: memoryLimit,
	})
}

func setBumpRecord(pc *kipperv1.PlatformConfig, component, from, to string, atCeiling bool, at metav1.Time, reason string) {
	for i, cs := range pc.Status.Components {
		if cs.Name == component {
			pc.Status.Components[i].LastBumpAt = &at
			pc.Status.Components[i].LastBumpFrom = from
			pc.Status.Components[i].LastBumpTo = to
			pc.Status.Components[i].LastBumpReason = reason
			pc.Status.Components[i].AtCeiling = atCeiling
			return
		}
	}
	pc.Status.Components = append(pc.Status.Components, kipperv1.ComponentStatus{
		Name:               component,
		CurrentMemoryLimit: to,
		LastBumpAt:         &at,
		LastBumpFrom:       from,
		LastBumpTo:         to,
		LastBumpReason:     reason,
		AtCeiling:          atCeiling,
	})
}

// recordCeilingHit updates status to flag that a component reached its
// ceiling and the operator needs to make a manual decision (raise the
// ceiling, optimise the workload, or move to a bigger node). Patch-based
// for the same reason as recordBump: avoid a stale-cache conflict loop
// with the PlatformConfigReconciler.
func (r *PodOOMReconciler) recordCeilingHit(ctx context.Context, component, limit string) error {
	var pc kipperv1.PlatformConfig
	if err := r.Get(ctx, types.NamespacedName{Name: PlatformConfigName}, &pc); err != nil {
		return fmt.Errorf("refetching PlatformConfig: %w", err)
	}
	for _, cs := range pc.Status.Components {
		if cs.Name == component && cs.AtCeiling {
			return nil
		}
	}
	base := pc.DeepCopy()
	now := metav1.NewTime(time.Now())
	setBumpRecord(&pc, component, limit, limit, true, now,
		fmt.Sprintf("at ceiling %s; manual intervention needed", limit))
	if err := r.Status().Patch(ctx, &pc, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("updating status at ceiling: %w", err)
	}
	return nil
}

func (r *PodOOMReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Pod{}, builder.WithPredicates(oomPodPredicate)).
		Complete(r)
}

// oomPodPredicate gates the controller's reconcile queue. Without it, every
// pod event in the cluster would enter the queue and the reconciler would
// have to discard most of them. We discard at the predicate level instead.
var oomPodPredicate = predicate.Funcs{
	CreateFunc: func(e event.CreateEvent) bool {
		return podMatters(e.Object)
	},
	UpdateFunc: func(e event.UpdateEvent) bool {
		return podMatters(e.ObjectNew)
	},
	DeleteFunc: func(_ event.DeleteEvent) bool { return false },
	GenericFunc: func(_ event.GenericEvent) bool {
		return false
	},
}

func podMatters(obj client.Object) bool {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return false
	}
	if _, watched := componentForPod(pod); !watched {
		return false
	}
	return hasOOMKilledContainer(pod)
}
