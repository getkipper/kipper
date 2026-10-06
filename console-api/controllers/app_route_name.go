package controllers

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/routename"
)

// Reasons on RouteReady for the app's route name.
const (
	reasonRouteNameTaken       = "RouteNameTaken"
	reasonRouteNamePlatform    = "RouteNameReservedForPlatform"
	reasonRouteNameShared      = "RouteNameShared"
	reasonRouteNamePending     = "RouteNamePending"
	routeNameIndistinguishable = "This name conflicts with another app or service. Choose a different name to create the route."
	routeNamePlatformReserved  = "This name is reserved for a Kipper component. Choose a different name to create the route."
)

const (
	// routeNameTakenRetry is how often a refused route looks again, so it
	// recovers once its name is free.
	routeNameTakenRetry = 10 * time.Minute
	// routeNamePendingRetry is how soon a route waiting for its generation
	// looks again.
	routeNamePendingRetry = 5 * time.Second
)

// routeNameRetry returns how soon the app's route-name state should be looked
// at again, or 0 when nothing is waiting.
func routeNameRetry(app *kipperv1.App) time.Duration {
	cond := apimeta.FindStatusCondition(app.Status.Conditions, kipperv1.ConditionRouteReady)
	if cond == nil {
		return 0
	}
	switch cond.Reason {
	case reasonRouteNameTaken:
		return routeNameTakenRetry
	case reasonRouteNamePending:
		return routeNamePendingRetry
	}
	return 0
}

// claimRouteName decides whether the app's route may be written, given the
// name Traefik files its backend under. A route that already exists is never
// removed for a tenant collision, only marked; a route colliding with a live
// platform route is removed, because the platform route must win. A new route
// comes with a release func the caller holds until its Ingress is written; an
// existing route outside the gate is marked update-only.
func (r *AppReconciler) claimRouteName(ctx context.Context, app *kipperv1.App) (bool, func(), bool, error) {
	key := routename.Key(app.Namespace, app.Name)
	installedPort, existing, err := r.installedRoutePort(ctx, app)
	if err != nil {
		return false, nil, false, err
	}

	if platform, ok := routename.Platform(key); ok {
		publish, err := r.claimPlatformRouteName(ctx, app, platform, installedPort, existing)
		return publish, nil, false, err
	}

	if existing {
		return r.claimExistingRoute(ctx, app, key)
	}

	decision, release, err := admitNewRoute(ctx, r.RouteNames, r.hostReader(), r.Client, app.Namespace, app.Name)
	if err != nil {
		return false, nil, false, err
	}
	switch decision {
	case routeNamePending:
		r.setRouteNameCondition(app, metav1.ConditionFalse, reasonRouteNamePending,
			"The route is waiting for a name check. It will retry automatically.")
		return false, nil, false, nil
	case routeNameTaken:
		r.refuseRouteName(app, reasonRouteNameTaken, routeNameIndistinguishable)
		return false, nil, false, nil
	case routeNameReservedForPlatform:
		r.refuseRouteName(app, reasonRouteNamePlatform, routeNamePlatformReserved)
		return false, nil, false, nil
	}
	if err := r.markSharedRouteName(ctx, app, key); err != nil {
		if release != nil {
			release()
		}
		return false, nil, false, err
	}
	return true, release, false, nil
}

// claimExistingRoute lets an existing route keep its settings and decides
// whether it may also create Ingresses: a missing canonical route, a new guard
// Ingress, or a host change, which withdraws and recreates the route. That
// needs the gate, held until the writes finish, and a current right to the
// name, so nothing a bootstrap retired can come back on an earlier look.
func (r *AppReconciler) claimExistingRoute(ctx context.Context, app *kipperv1.App, key string) (bool, func(), bool, error) {
	if r.RouteNames == nil {
		return true, nil, false, r.markSharedRouteName(ctx, app, key)
	}
	release, admitted := r.RouteNames.Admit()
	if !admitted {
		return true, nil, true, nil
	}
	owned, err := reserveRouteName(ctx, r.hostReader(), r.Client, app.Namespace, key)
	if errors.Is(err, errRouteNameRace) {
		return true, release, true, nil
	}
	if err != nil {
		release()
		return false, nil, false, err
	}
	shared, err := sharesRouteName(ctx, r.hostReader(), app.Namespace, key)
	if err != nil {
		release()
		return false, nil, false, err
	}
	if !owned && !shared {
		return true, release, true, nil
	}
	if err := r.markSharedRouteName(ctx, app, key); err != nil {
		release()
		return false, nil, false, err
	}
	return true, release, false, nil
}

// claimPlatformRouteName reserves platform keys against new app routes.
// An existing app route is removed if its installed or requested port
// collides with an installed platform route.
func (r *AppReconciler) claimPlatformRouteName(ctx context.Context, app *kipperv1.App, platform routename.PlatformRoute, installedPort int32, existing bool) (bool, error) {
	if !existing {
		r.refuseRouteName(app, reasonRouteNamePlatform, routeNamePlatformReserved)
		return false, nil
	}
	live, err := platformRouteLive(ctx, r.hostReader(), platform)
	if err != nil {
		return false, err
	}
	switch {
	case live && installedPort == platform.Port:
		r.refuseRouteName(app, reasonRouteNamePlatform,
			"The route was removed because it conflicts with a Kipper component. Use a different app name to restore access.")
		return false, r.deleteOwnedRouteIngresses(ctx, app)
	case live && app.Spec.Port == platform.Port:
		// The Service has already moved to the new port, so the old route
		// would point at nothing; it is removed rather than left broken.
		r.refuseRouteName(app, reasonRouteNamePlatform,
			"The route was removed because this port causes a conflict with a Kipper component. Use a different app name to restore access.")
		return false, r.deleteOwnedRouteIngresses(ctx, app)
	}
	return true, nil
}

type routeUpdateOnlyKey struct{}

// withRouteUpdateOnly restricts applyOwnedIngress to existing Ingresses.
func withRouteUpdateOnly(ctx context.Context) context.Context {
	return context.WithValue(ctx, routeUpdateOnlyKey{}, true)
}

func routeUpdateOnly(ctx context.Context) bool {
	v, _ := ctx.Value(routeUpdateOnlyKey{}).(bool)
	return v
}

// withdrawForHostChange takes the route down while its host change waits for
// route names to be checked. Keeping the old host would skip the rest of the
// route's reconcile, and with it any protection the route has since asked for.
// Recreating the route on its new host requires admission through the gate.
func (r *AppReconciler) withdrawForHostChange(ctx context.Context, app *kipperv1.App) error {
	if err := r.deleteOwnedRouteIngresses(ctx, app); err != nil {
		return fmt.Errorf("withdrawing a route whose host change waits: %w", err)
	}
	r.setRouteNameCondition(app, metav1.ConditionFalse, reasonRouteNamePending,
		"The old route has been removed. The route on the new host is waiting for a name check and will retry automatically.")
	return errRouteCreateDeferred
}

// installedRoutePort returns the backend port of the app's own canonical
// route Ingress and whether that Ingress exists.
func (r *AppReconciler) installedRoutePort(ctx context.Context, app *kipperv1.App) (int32, bool, error) {
	var ing networkingv1.Ingress
	err := r.Get(ctx, types.NamespacedName{Name: app.Name, Namespace: app.Namespace}, &ing)
	if apierrors.IsNotFound(err) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if !ownedByWorkload(&ing, appOwner(app)) {
		return 0, false, nil
	}
	for _, b := range routename.Backends(ing) {
		if b.Service == app.Name {
			return b.Port, true, nil
		}
	}
	return 0, true, nil
}

// refuseRouteName records the refusal on RouteReady and emits a warning event.
func (r *AppReconciler) refuseRouteName(app *kipperv1.App, reason, message string) {
	r.setRouteNameCondition(app, metav1.ConditionFalse, reason, message)
	if r.Recorder != nil {
		r.Recorder.Event(app, corev1.EventTypeWarning, reason, message)
	}
}

// markSharedRouteName records on RouteReady that the app's route shares its
// traffic name with a route in another namespace.
func (r *AppReconciler) markSharedRouteName(ctx context.Context, app *kipperv1.App, key string) error {
	shared, err := sharesRouteName(ctx, r.hostReader(), app.Namespace, key)
	if err != nil || !shared {
		return err
	}
	r.setRouteNameCondition(app, metav1.ConditionTrue, reasonRouteNameShared,
		"The route is still published, but its name conflicts with another app or service. Request figures are unavailable. Use a different app name to avoid requests reaching the wrong destination.")
	return nil
}

// platformRouteLive reports whether an Ingress in the platform route's
// namespace routes to its backend on its port.
func platformRouteLive(ctx context.Context, reader client.Reader, p routename.PlatformRoute) (bool, error) {
	var list networkingv1.IngressList
	if err := reader.List(ctx, &list, client.InNamespace(p.Namespace)); err != nil {
		return false, fmt.Errorf("listing Ingresses in %s: %w", p.Namespace, err)
	}
	want := routename.Backend{Namespace: p.Namespace, Service: p.Service, Port: p.Port}
	for _, ing := range list.Items {
		if slices.Contains(routename.Backends(ing), want) {
			return true, nil
		}
	}
	return false, nil
}

func (r *AppReconciler) setRouteNameCondition(app *kipperv1.App, status metav1.ConditionStatus, reason, message string) {
	apimeta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
		Type:    kipperv1.ConditionRouteReady,
		Status:  status,
		Reason:  reason,
		Message: message,
	})
}
