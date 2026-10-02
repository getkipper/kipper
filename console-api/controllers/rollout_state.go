package controllers

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/rollout"
)

// Pod scheduling changes may not trigger the Deployment watch, so poll
// unfinished rollouts as well.
const rolloutRequeue = 30 * time.Second

// observeRollout sets the RolloutComplete condition from deploy and reports how
// soon to look again. Pods are read only while the rollout is unfinished.
func (r *AppReconciler) observeRollout(ctx context.Context, app *kipperv1.App, deploy *appsv1.Deployment) (time.Duration, error) {
	complete := func() (time.Duration, error) {
		apimeta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
			Type:               kipperv1.ConditionRolloutComplete,
			Status:             metav1.ConditionTrue,
			Reason:             "Complete",
			Message:            "every pod runs the current version",
			ObservedGeneration: app.Generation,
		})
		return 0, nil
	}
	if rollout.Settled(deploy) {
		return complete()
	}

	newPods, err := newestPods(ctx, r.hostReader(), deploy)
	if err != nil {
		return 0, err
	}
	reason, message := rollout.Explain(deploy, newPods, appStartupTimeout(app), time.Now())
	if reason == rollout.Complete {
		// A completed Deployment can still have a Pending pod after scaling.
		// Keep polling until the scheduler has handled it.
		if _, err := complete(); err != nil {
			return 0, err
		}
		for i := range newPods {
			if newPods[i].Status.Phase == corev1.PodPending {
				return rolloutRequeue, nil
			}
		}
		return 0, nil
	}
	if reason == rollout.NotBecomingReady {
		message += " " + checkHint(app, &deploy.Spec.Template)
	}
	previous := apimeta.FindStatusCondition(app.Status.Conditions, kipperv1.ConditionRolloutComplete)
	if reason == rollout.Unschedulable && (previous == nil || previous.Reason != string(rollout.Unschedulable)) && r.Recorder != nil {
		r.Recorder.Event(app, corev1.EventTypeWarning, "RolloutWaitingForCapacity", message)
	}
	apimeta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
		Type:               kipperv1.ConditionRolloutComplete,
		Status:             metav1.ConditionFalse,
		Reason:             string(reason),
		Message:            message,
		ObservedGeneration: app.Generation,
	})
	return rolloutRequeue, nil
}

// newestPods lists pods owned by the Deployment's highest-revision ReplicaSet.
func newestPods(ctx context.Context, reader client.Reader, deploy *appsv1.Deployment) ([]corev1.Pod, error) {
	if deploy.Spec.Selector == nil {
		return nil, nil
	}
	var sets appsv1.ReplicaSetList
	if err := reader.List(ctx, &sets, client.InNamespace(deploy.Namespace),
		client.MatchingLabels(deploy.Spec.Selector.MatchLabels)); err != nil {
		return nil, err
	}
	newest := rollout.NewestReplicaSet(deploy, sets.Items)
	if newest == nil {
		return nil, nil
	}
	var pods corev1.PodList
	if err := reader.List(ctx, &pods, client.InNamespace(deploy.Namespace),
		client.MatchingLabels(deploy.Spec.Selector.MatchLabels)); err != nil {
		return nil, err
	}
	return rollout.PodsOf(newest, pods.Items), nil
}

// checkHint describes the declared or inferred check and suggests next steps.
func checkHint(app *kipperv1.App, live *corev1.PodTemplateSpec) string {
	h := app.Spec.Health
	switch {
	case h == nil && storedInference(app, live) != inferredTCP, h != nil && h.Type == "none":
		return fmt.Sprintf("The app container has no check. Inspect the pod and its container logs with `kip app logs %s`.", app.Name)
	case h == nil:
		return fmt.Sprintf("Kipper checks that port %d accepts connections, because the app's earlier pods did. If this version needs longer to start, declare the check with more time: `kip app update %s --health tcp --health-startup-timeout 600`.", app.Spec.Port, app.Name)
	case h.Type == "http" && !isPlaceholder(app):
		port := app.Spec.Port
		if h.Port != nil {
			port = *h.Port
		}
		return fmt.Sprintf("Kipper runs an HTTP check of %s on port %d. It must return 200-399 when the app is ready. Allow more startup time with `kip app update %s --health-startup-timeout 600`.", h.Path, port, app.Name)
	}
	port := app.Spec.Port
	if h.Port != nil && !isPlaceholder(app) {
		port = *h.Port
	}
	return fmt.Sprintf("Kipper checks that port %d accepts connections. If the app listens only on localhost, bind it to 0.0.0.0. Allow more startup time with `kip app update %s --health-startup-timeout 600`.", port, app.Name)
}

// healthCheckStatus describes the desired check and whether the live template
// has it. Automatic checks are read from the template's inference annotation.
func healthCheckStatus(app *kipperv1.App, live *corev1.PodTemplateSpec) kipperv1.AppHealthCheckStatus {
	h := app.Spec.Health
	switch {
	case isPlaceholder(app):
		if h == nil || h.Type == "none" {
			return kipperv1.AppHealthCheckStatus{Type: "none", Source: "building"}
		}
		return kipperv1.AppHealthCheckStatus{Type: "tcp", Port: app.Spec.Port, Source: "building"}
	case h != nil:
		st := kipperv1.AppHealthCheckStatus{Type: h.Type, Path: h.Path, Source: "declared"}
		if h.Type != "none" {
			st.Port = app.Spec.Port
			if h.Port != nil {
				st.Port = *h.Port
			}
		}
		// The declared probe has not reached the Deployment template yet.
		if !equality.Semantic.DeepEqual(live.Spec.Containers[0].ReadinessProbe, declaredReadiness(app)) {
			st.Source = "applying"
		}
		return st
	}
	switch storedInference(app, live) {
	case inferredTCP:
		return kipperv1.AppHealthCheckStatus{Type: "tcp", Port: app.Spec.Port, Source: "inferred"}
	case inferredNone:
		return kipperv1.AppHealthCheckStatus{Type: "none", Source: "inferred"}
	}
	return kipperv1.AppHealthCheckStatus{Type: "none", Source: "pending"}
}

// liveRolloutPhase classifies the rollout for resource tuning. A pod read
// failure postpones recommendations until the rollout state is known.
func (r *AppReconciler) liveRolloutPhase(ctx context.Context, app *kipperv1.App, deploy *appsv1.Deployment) rolloutPhase {
	if rollout.Settled(deploy) {
		return phaseSettled
	}
	pods, err := newestPods(ctx, r.hostReader(), deploy)
	if err != nil {
		log.FromContext(ctx).Error(err, "could not read rollout pods; postponing the resource recommendation", "app", app.Name)
		return phaseInFlight
	}
	reason, _ := rollout.Explain(deploy, pods, appStartupTimeout(app), time.Now())
	phase := rolloutPhaseFor(reason)
	// Resume recommendations after the progress deadline, even when the
	// explanation names a more specific readiness problem.
	if phase == phaseInFlight && rollout.Failed(deploy) {
		return phaseFailed
	}
	return phase
}
