package controllers

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

const portRecheck = 5 * time.Minute

// portWatched selects routed or linked apps with no app readiness check.
// Stopped apps, build placeholders, and apps with an applied explicit opt-out are excluded.
func portWatched(app *kipperv1.App, check kipperv1.AppHealthCheckStatus, linkedTo bool) bool {
	if appStopped(app) || check.Type != "none" || check.Source == "declared" || check.Source == "building" {
		return false
	}
	return app.Spec.Route != nil || linkedTo
}

// observePort updates PortListening and returns the delay before the next check.
// Inconclusive results preserve the previous condition.
func (r *AppReconciler) observePort(ctx context.Context, app *kipperv1.App, deploy *appsv1.Deployment, check kipperv1.AppHealthCheckStatus, now time.Time) time.Duration {
	if r.PortDial == nil || !r.portWatchedNow(ctx, app, check) {
		apimeta.RemoveStatusCondition(&app.Status.Conditions, kipperv1.ConditionPortListening)
		r.portChecked.Delete(app.UID)
		return 0
	}
	if last, ok := r.portChecked.Load(app.UID); ok {
		if since := now.Sub(last.(time.Time)); since < portRecheck {
			return portRecheck - since
		}
	}
	r.portChecked.Store(app.UID, now)
	r.sweepPortChecked(now)
	accepted, refused := false, false
	for _, a := range r.probePort(ctx, r.PortDial, app, deploy, now) {
		if a.proxied {
			// The instance proxy can reach a localhost-only listener, so a
			// refusal on the pod IP is inconclusive.
			a.settledRefusal = false
		}
		accepted = accepted || a.accepted
		refused = refused || a.settledRefusal
	}
	cond := metav1.Condition{Type: kipperv1.ConditionPortListening, ObservedGeneration: app.Generation}
	switch {
	case accepted:
		cond.Status, cond.Reason = metav1.ConditionTrue, "Listening"
		cond.Message = fmt.Sprintf("the app accepts connections on port %d", app.Spec.Port)
	case refused:
		cond.Status, cond.Reason = metav1.ConditionFalse, "NothingListening"
		cond.Message = fmt.Sprintf("port %d refused a connection after the app container had been running for at least %s. "+
			"Configure the app to listen on 0.0.0.0:%d, or update its declared port. "+
			"For a worker that serves no traffic, disable this check: kip app update %s --health none",
			app.Spec.Port, inferenceSettledAge, app.Spec.Port, app.Name)
	default:
		return portRecheck
	}
	apimeta.SetStatusCondition(&app.Status.Conditions, cond)
	return portRecheck
}

// portWatchedNow looks up inbound links only when needed to determine eligibility.
func (r *AppReconciler) portWatchedNow(ctx context.Context, app *kipperv1.App, check kipperv1.AppHealthCheckStatus) bool {
	if !portWatched(app, check, true) {
		return false
	}
	return app.Spec.Route != nil || r.linkedTo(ctx, app)
}

// sweepPortChecked expires stale entries to bound tracking for deleted or unwatched apps.
func (r *AppReconciler) sweepPortChecked(now time.Time) {
	r.portChecked.Range(func(uid, last any) bool {
		if now.Sub(last.(time.Time)) > 2*portRecheck {
			r.portChecked.Delete(uid)
		}
		return true
	})
}

// linkedTo uses the link-target index to find inbound links.
// Lookup failures keep the app eligible for port checks.
func (r *AppReconciler) linkedTo(ctx context.Context, app *kipperv1.App) bool {
	var callers kipperv1.AppList
	if err := r.List(ctx, &callers, client.MatchingFields{linkTargetIndex: app.Namespace + "/" + app.Name}); err != nil {
		log.FromContext(ctx).Error(err, "finding apps that link to this one", "app", app.Name)
		return true
	}
	return len(callers.Items) > 0
}

// enqueueLinkTargets schedules targets for reconciliation so link changes
// trigger port checks even for otherwise idle apps.
func (r *AppReconciler) enqueueLinkTargets(_ context.Context, obj client.Object) []reconcile.Request {
	app, ok := obj.(*kipperv1.App)
	if !ok {
		return nil
	}
	requests := make([]reconcile.Request, 0, len(app.Spec.Links))
	for _, l := range app.Spec.Links {
		requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: l.Namespace, Name: l.App}})
	}
	return requests
}
