package controllers

import (
	"context"
	"fmt"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/internal/resourcebounds"
	"github.com/getkipper/kipper/controller/pkg/capacity"
)

func appPolicy(app *kipperv1.App) *capacity.Policy {
	return app.Spec.Autoscale.Policy()
}

// replicasOutsideBounds reports whether a valid policy's bounds exclude the
// stored replica count. A nil count is the default of 1.
func replicasOutsideBounds(app *kipperv1.App) bool {
	policy := appPolicy(app)
	if capacity.Validate(policy, nil) != nil {
		return false
	}
	lo, hi, ok := policy.Bounds()
	n := replicasOf(app.Spec.Replicas)
	return ok && (n < lo || n > hi)
}

// trackedMetrics uses the policy's targets when valid, or the retained HPA's
// metrics when invalid. A missing HPA tracks nothing; other read failures
// conservatively treat both metrics as tracked.
func (r *AppReconciler) trackedMetrics(ctx context.Context, app *kipperv1.App) resourcebounds.Tracked {
	if !appAutoscaled(app) || appPolicy(app).Usable() {
		return resourcebounds.TrackedMetrics(app, nil)
	}
	var hpa autoscalingv2.HorizontalPodAutoscaler
	err := r.Get(ctx, types.NamespacedName{Name: app.Name, Namespace: app.Namespace}, &hpa)
	switch {
	case err == nil:
		return resourcebounds.TrackedMetrics(app, &hpa)
	case errors.IsNotFound(err):
		return resourcebounds.TrackedMetrics(app, nil)
	default:
		log.FromContext(ctx).Error(err, "reading the autoscaler, keeping the live size of both metrics", "app", app.Name)
		return resourcebounds.Tracked{CPU: true, Memory: true}
	}
}

func setAutoscalingCondition(app *kipperv1.App, status metav1.ConditionStatus, reason, message string) {
	apimeta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
		Type:               kipperv1.ConditionAutoscalingReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: app.Generation,
	})
}

// clampToBounds returns the nearest in-bounds count. Callers must first confirm
// that the policy has valid bounds.
func clampToBounds(app *kipperv1.App) int32 {
	lo, hi, _ := appPolicy(app).Bounds()
	return min(max(replicasOf(app.Spec.Replicas), lo), hi)
}

func reportReplicasOutsideBounds(app *kipperv1.App) {
	lo, hi, _ := appPolicy(app).Bounds()
	setAutoscalingCondition(app, metav1.ConditionFalse, "ReplicasOutsideBounds",
		fmt.Sprintf("replicas (%d) lies outside the bounds %d to %d, so Kipper does not apply it; set a count within the bounds or change them", replicasOf(app.Spec.Replicas), lo, hi))
}

// reportDisabledPolicy reports invalid policy fields before replica-bound
// violations, clearing the condition when neither applies.
func reportDisabledPolicy(app *kipperv1.App) {
	if err := capacity.Validate(appPolicy(app), nil); err != nil {
		setAutoscalingCondition(app, metav1.ConditionFalse, "InvalidPolicy",
			fmt.Sprintf("the autoscaling block is invalid: %v", err))
		return
	}
	if replicasOutsideBounds(app) {
		reportReplicasOutsideBounds(app)
		return
	}
	apimeta.RemoveStatusCondition(&app.Status.Conditions, kipperv1.ConditionAutoscalingReady)
}

// deleteOwnedHPA deletes this app's own autoscaler, if it has one. An object
// that merely shares the name is left alone.
func (r *AppReconciler) deleteOwnedHPA(ctx context.Context, app *kipperv1.App) error {
	var existing autoscalingv2.HorizontalPodAutoscaler
	if err := r.Get(ctx, types.NamespacedName{Name: app.Name, Namespace: app.Namespace}, &existing); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if !ownedByWorkload(&existing, appOwner(app)) {
		return nil
	}
	if err := r.Delete(ctx, &existing); err != nil && !errors.IsNotFound(err) {
		return err
	}
	return nil
}

// removeDisabledAutoscaler removes a disabled HPA immediately after reconciling
// the Deployment, so a later child failure cannot leave it overriding the count.
// Deletion failures are recorded without stopping the pass; the final HPA step
// retries if reached.
func (r *AppReconciler) removeDisabledAutoscaler(ctx context.Context, app *kipperv1.App) error {
	if appAutoscaled(app) {
		return nil
	}
	if err := r.deleteOwnedHPA(ctx, app); err != nil {
		setAutoscalingCondition(app, metav1.ConditionFalse, "AutoscalerDeleteFailed",
			fmt.Sprintf("the autoscaler could not be deleted: %v", err))
		return nil
	}
	reportDisabledPolicy(app)
	return nil
}
