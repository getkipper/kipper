package controllers

import (
	"context"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/internal/resourcebounds"
	quotapkg "github.com/getkipper/kipper/console-api/quota"
	"github.com/getkipper/kipper/controller/pkg/rollout"
)

// tunedWorkload supplies the desired resources and the state needed to resolve
// them against user bounds and an optional recommendation.
type tunedWorkload struct {
	Namespace, Kind, Name string
	UID                   types.UID
	Spec                  resourcebounds.Spec
	// Desired arrives holding what the reconciler built from the spec and
	// profile; Live is the running container's resources, nil before the
	// first rollout.
	Desired, Live   *corev1.ResourceRequirements
	LiveAnnotations map[string]string
	Replicas        int32
	SurgePods       int32
	PodSpec         *corev1.PodSpec
	// Rollout classifies the live rollout when a recommendation is available.
	// It may read pods; nil treats the workload as settled.
	Rollout func() rolloutPhase
	// Tracked filters recommendations that would interfere with autoscaling.
	Tracked resourcebounds.Tracked
}

// rolloutPhase determines whether resource recommendations may apply.
type rolloutPhase int

const (
	// phaseSettled allows recommendations.
	phaseSettled rolloutPhase = iota
	// phaseInFlight postpones recommendations to avoid another rollout.
	phaseInFlight
	// phaseUnschedulable allows recommendations only when neither request grows.
	phaseUnschedulable
	// phaseFailed allows recommendations that may help a failed rollout recover.
	phaseFailed
)

// rolloutPhaseFor maps the reason a rollout has not finished to its phase.
func rolloutPhaseFor(reason rollout.Reason) rolloutPhase {
	switch reason {
	case rollout.Complete:
		return phaseSettled
	case rollout.Unschedulable, rollout.QuotaExceeded:
		return phaseUnschedulable
	case rollout.PodsRefused, rollout.DeadlineExceeded:
		return phaseFailed
	}
	return phaseInFlight
}

// applyTunedResources resolves CPU and memory from the spec, live values and
// recommendation. Paused tuning, expert mode or a quota rejection falls back
// to live values within the user's bounds. It reports a user request above its
// limit; that resource is fixed at the limit.
func applyTunedResources(ctx context.Context, reader client.Reader, w tunedWorkload) (requestAboveLimit bool) {
	rec := recommendationFor(ctx, reader, w)
	phase := phaseSettled
	if rec != nil && w.Rollout != nil {
		phase = w.Rollout()
	}
	if phase == phaseInFlight {
		rec = nil
	}
	trial := w.Desired.DeepCopy()
	resourcebounds.Apply(trial, w.Spec, rec, w.Live)
	if rec != nil && phase == phaseUnschedulable && w.Live != nil && requestsGrow(*w.Live, *trial) {
		trial = w.Desired.DeepCopy()
		resourcebounds.Apply(trial, w.Spec, nil, w.Live)
	}
	if rec != nil && w.Live != nil && !quotaAllows(ctx, reader, w.Namespace, *w.Live, *trial, w.Replicas, w.SurgePods,
		quotapkg.WithContainerResources(w.PodSpec, *trial)) {
		logf.FromContext(ctx).Info("the resource recommendation does not fit the project quota, keeping the live size",
			"kind", w.Kind, "name", w.Name)
		trial = w.Desired.DeepCopy()
		resourcebounds.Apply(trial, w.Spec, nil, w.Live)
	}
	*w.Desired = *trial
	return requestAboveLimitIn(w.Spec.CPURequest, w.Spec.CPULimit) || requestAboveLimitIn(w.Spec.MemoryRequest, w.Spec.MemoryLimit)
}

func requestsGrow(live, desired corev1.ResourceRequirements) bool {
	for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		want, ok := desired.Requests[name]
		if !ok {
			continue
		}
		have, ok := live.Requests[name]
		if !ok || want.Cmp(have) > 0 {
			return true
		}
	}
	return false
}

func recommendationFor(ctx context.Context, reader client.Reader, w tunedWorkload) *kipperv1.TunedResources {
	if resourcebounds.TuningPaused(w.LiveAnnotations) || expertMode(ctx, reader) {
		return nil
	}
	var rt kipperv1.ResourceTuning
	err := reader.Get(ctx, types.NamespacedName{Namespace: w.Namespace, Name: resourcebounds.TuningName(w.Kind, w.Name)}, &rt)
	switch {
	case err == nil && !resourcebounds.TuningBelongsTo(&rt, w.UID):
		return nil
	case err == nil:
		rec := w.Tracked.Recommendation(rt.Status)
		return &rec
	case errors.IsNotFound(err), meta.IsNoMatchError(err), errors.IsForbidden(err):
		// No record yet, the CRD is not installed, or console-api may not
		// read it: all mean there is no recommendation to apply.
		return nil
	default:
		logf.FromContext(ctx).Error(err, "reading the resource recommendation, keeping the live size")
		return nil
	}
}

func requestAboveLimitIn(request, limit resourcebounds.Quantity) bool {
	return request.Source == resourcebounds.User && limit.Source == resourcebounds.User && request.Value.Cmp(limit.Value) > 0
}

// quotaAllows reports whether changing the workload from live to desired fits
// the project quota. An unreadable quota counts as not fitting, so an
// increase waits rather than wedging the rollout at admission.
func quotaAllows(ctx context.Context, reader client.Reader, namespace string, live, desired corev1.ResourceRequirements, replicas, surgePods int32, podSpec *corev1.PodSpec) bool {
	var quota corev1.ResourceQuota
	err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: kipperv1.ProjectQuotaName}, &quota)
	if errors.IsNotFound(err) {
		return true
	}
	if err != nil {
		return false
	}
	_, _, _, fits := quotapkg.Fits(&quota, live, desired, replicas, surgePods, podSpec)
	return fits
}

func expertMode(ctx context.Context, c client.Reader) bool {
	var cm corev1.ConfigMap
	key := types.NamespacedName{Namespace: resourcebounds.ModeConfigMapNamespace, Name: resourcebounds.ModeConfigMapName}
	if err := c.Get(ctx, key, &cm); err != nil {
		return false
	}
	return cm.Data["mode"] == resourcebounds.ModeExpert
}

// ownTuningRecords watches tuning records when the CRD and permissions are
// available at startup. Starting an unsyncable watch would block the manager.
// Without a watch, recommendations apply on the owner's next reconcile;
// adding the watch requires a restart.
func ownTuningRecords(mgr ctrl.Manager, b *builder.Builder) *builder.Builder {
	gk := kipperv1.GroupVersion.WithKind("ResourceTuning").GroupKind()
	if _, err := mgr.GetRESTMapper().RESTMapping(gk, kipperv1.GroupVersion.Version); err != nil {
		ctrl.Log.Info("ResourceTuning is not installed, resource recommendations apply on the next reconcile only", "reason", err.Error())
		return b
	}
	if !mayWatchTuningRecords(mgr) {
		ctrl.Log.Info("console-api may not list and watch ResourceTuning, resource recommendations apply on the next reconcile only")
		return b
	}
	return b.Owns(&kipperv1.ResourceTuning{})
}

func mayWatchTuningRecords(mgr ctrl.Manager) bool {
	clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		return false
	}
	for _, verb := range []string{"list", "watch"} {
		review := &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authorizationv1.ResourceAttributes{Group: kipperv1.GroupVersion.Group, Resource: "resourcetunings", Verb: verb},
		}}
		got, err := clientset.AuthorizationV1().SelfSubjectAccessReviews().Create(context.Background(), review, metav1.CreateOptions{})
		if err != nil || !got.Status.Allowed {
			return false
		}
	}
	return true
}

// rolloutReplicas uses the higher of the desired and live replica counts
// so quota checks account for replicas added by an autoscaler.
func rolloutReplicas(desired, live *int32) int32 {
	n := replicasOf(desired)
	if live != nil && *live > n {
		n = *live
	}
	return n
}

func replicasOf(r *int32) int32 {
	if r == nil {
		return 1
	}
	return *r
}
