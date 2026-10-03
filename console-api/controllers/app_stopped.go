package controllers

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/internal/resourcebounds"
	"github.com/getkipper/kipper/controller/pkg/labels"
)

// stoppedAnnotation marks a Deployment scaled to zero for an app stop.
// For autoscaled apps, it authorizes the one-time scale-up on start.
const stoppedAnnotation = labels.AnnoStopped

// lastStopAnnotation records when the app was last stopped and stays after
// the start.
const lastStopAnnotation = labels.AnnoLastStop

func appStopped(app *kipperv1.App) bool {
	return app.Spec.Stopped != nil
}

func appAutoscaled(app *kipperv1.App) bool {
	return app.Spec.Autoscale != nil && app.Spec.Autoscale.Enabled
}

// startReplicas restores the configured count, or at least one replica at the
// autoscaling minimum so the HPA can resume scaling.
func startReplicas(app *kipperv1.App) int32 {
	if appAutoscaled(app) {
		return max(app.Spec.Autoscale.MinReplicas, 1)
	}
	if app.Spec.Replicas != nil {
		return *app.Spec.Replicas
	}
	return 1
}

// setStoppedMarker adds or removes the stop marker and reports whether it
// changed.
func setStoppedMarker(d *appsv1.Deployment, stopped bool) bool {
	_, has := d.Annotations[stoppedAnnotation]
	switch {
	case stopped && !has:
		if d.Annotations == nil {
			d.Annotations = map[string]string{}
		}
		d.Annotations[stoppedAnnotation] = "true"
		return true
	case !stopped && has:
		delete(d.Annotations, stoppedAnnotation)
		return true
	}
	return false
}

const (
	stoppedPageMiddleware = "app-stopped"
	stoppedPageNamespace  = "kipper-system"
	stoppedPageRef        = stoppedPageNamespace + "-" + stoppedPageMiddleware + "@kubernetescrd"
)

// stoppedPage is the Traefik errors middleware that turns the 503 of a route
// without endpoints into kipper-authz's stopped page. It lives next to the
// service it queries, because Traefik resolves that service in the
// middleware's own namespace.
func stoppedPage() *unstructured.Unstructured {
	mw := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{
			"name":      stoppedPageMiddleware,
			"namespace": stoppedPageNamespace,
			"labels":    map[string]interface{}{kipperLabel: kipperValue},
		},
		"spec": map[string]interface{}{
			"errors": map[string]interface{}{
				"status":  []interface{}{"503"},
				"query":   "/stopped",
				"service": map[string]interface{}{"name": "kipper-authz", "port": int64(8080)},
			},
		},
	}}
	mw.SetGroupVersionKind(middlewareGVK)
	return mw
}

// ensureStoppedPage ensures the shared middleware matches the desired spec and
// reports whether the route may reference it. Failed writes schedule a retry;
// the route can still be reconciled without the custom page.
func (r *AppReconciler) ensureStoppedPage(ctx context.Context, app *kipperv1.App) bool {
	if !appStopped(app) {
		r.stoppedPageFailed.Delete(app.UID)
		return false
	}
	desired := stoppedPage()
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(middlewareGVK)
	err := r.Get(ctx, types.NamespacedName{Namespace: stoppedPageNamespace, Name: stoppedPageMiddleware}, live)
	if err == nil && equality.Semantic.DeepEqual(live.Object["spec"], desired.Object["spec"]) {
		r.stoppedPageFailed.Delete(app.UID)
		return true
	}
	//nolint:staticcheck // SSA via client.Apply; the typed Apply-configuration API is not adopted in this codebase yet
	err = r.Patch(ctx, desired, client.Apply, client.FieldOwner(servingFieldManager), client.ForceOwnership)
	if err != nil {
		log.FromContext(ctx).Error(err, "applying the stopped-page middleware; the route answers without it", "app", app.Name, "namespace", app.Namespace)
		r.stoppedPageFailed.Store(app.UID, true)
		return false
	}
	r.stoppedPageFailed.Delete(app.UID)
	return true
}

// stoppedPageRetry returns the retry delay after a middleware write failure, or zero.
func (r *AppReconciler) stoppedPageRetry(app *kipperv1.App) time.Duration {
	if _, failed := r.stoppedPageFailed.Load(app.UID); failed {
		return rolloutRequeue
	}
	return 0
}

// beginStop clears stale sizing recommendations before writing a stopped
// Deployment. Its timestamp lets the resource controller detect a stop/start
// cycle between passes. Retrying clears the recommendation and refreshes the stamp.
func (r *AppReconciler) beginStop(ctx context.Context, app *kipperv1.App, d *appsv1.Deployment) error {
	var rt kipperv1.ResourceTuning
	err := r.Get(ctx, types.NamespacedName{Namespace: app.Namespace, Name: resourcebounds.TuningName("App", app.Name)}, &rt)
	switch {
	case apierrors.IsNotFound(err), apimeta.IsNoMatchError(err):
	case err != nil:
		return fmt.Errorf("reading the resource recommendation: %w", err)
	case resourcebounds.TuningBelongsTo(&rt, app.UID) &&
		(rt.Status.Recommendation != (kipperv1.TunedResources{}) || rt.Status.RecommendedAt != nil):
		rt.Status.Recommendation = kipperv1.TunedResources{}
		rt.Status.RecommendedAt = nil
		if err := r.Status().Update(ctx, &rt); err != nil {
			return fmt.Errorf("clearing the resource recommendation: %w", err)
		}
	}
	if d.Annotations == nil {
		d.Annotations = map[string]string{}
	}
	d.Annotations[lastStopAnnotation] = time.Now().UTC().Format(time.RFC3339Nano)
	return nil
}
