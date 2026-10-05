package controllers

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/routename"
)

func gateFor(admitted bool) *RouteNameGate {
	g := NewRouteNameGate()
	if admitted {
		g.open()
	}
	return g
}

func namedRoutedApp(name, namespace string, port int32) *kipperv1.App {
	return &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: types.UID("uid-" + namespace + "-" + name)},
		Spec:       kipperv1.AppSpec{Image: "nginx:1.25", Port: port, Route: &kipperv1.AppRoute{Host: name + "." + namespace + ".example.com", Path: "/"}},
	}
}

// existingRoute is an app's route Ingress as an earlier Kipper version left it.
func existingRoute(app *kipperv1.App) *networkingv1.Ingress {
	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name: app.Name, Namespace: app.Namespace,
			Labels: map[string]string{kipperLabel: kipperValue, "app": app.Name},
		},
		Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{
			Host: app.Spec.Route.Host,
			IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{{
				Path: "/", Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
					Name: app.Name, Port: networkingv1.ServiceBackendPort{Number: app.Spec.Port},
				}},
			}}}},
		}}},
	}
}

func platformRouteIngress(r routename.PlatformRoute) *networkingv1.Ingress {
	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: r.Service, Namespace: r.Namespace},
		Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{
			Host: r.Service + ".example.com",
			IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{{
				Path: "/", Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
					Name: r.Service, Port: networkingv1.ServiceBackendPort{Number: r.Port},
				}},
			}}}},
		}}},
	}
}

func routeReady(app *kipperv1.App) *metav1.Condition {
	return apimeta.FindStatusCondition(app.Status.Conditions, kipperv1.ConditionRouteReady)
}

func ingressExists(t *testing.T, c crclient.Client, namespace, name string) bool {
	t.Helper()
	var ing networkingv1.Ingress
	err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: namespace}, &ing)
	if errors.IsNotFound(err) {
		return false
	}
	require.NoError(t, err)
	return true
}

func routeNameReconciler(t *testing.T, admitted bool, objs ...crclient.Object) (*AppReconciler, crclient.Client) {
	t.Helper()
	scheme := testScheme()
	b := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...)
	for _, o := range objs {
		if app, ok := o.(*kipperv1.App); ok {
			b = b.WithStatusSubresource(app)
		}
	}
	c := b.Build()
	return &AppReconciler{Client: c, Scheme: scheme, Domain: "example.com", RouteNames: gateFor(admitted)}, c
}

func TestAppRouteName_ANewRouteReservesItsName(t *testing.T) {
	app := namedRoutedApp("prod-web", "team", 8080)
	r, c := routeNameReconciler(t, true, app, namespaceWithUID("team", "uid-team"))

	require.NoError(t, r.reconcileIngress(context.Background(), app))

	assert.True(t, ingressExists(t, c, "team", "prod-web"))
	claim := readRouteNameClaim(t, c, routename.Key("team", "prod-web"))
	assert.Equal(t, "team", claim.Namespace)
}

func TestAppRouteName_ACollidingNameIsRefused(t *testing.T) {
	first := namedRoutedApp("prod-web", "team", 8080)
	second := namedRoutedApp("web", "team-prod", 8080)
	r, c := routeNameReconciler(t, true, first, second,
		namespaceWithUID("team", "uid-team"), namespaceWithUID("team-prod", "uid-team-prod"))
	ctx := context.Background()

	require.NoError(t, r.reconcileIngress(ctx, first))
	require.NoError(t, r.reconcileIngress(ctx, second))

	assert.False(t, ingressExists(t, c, "team-prod", "web"), "a colliding route must not be published")
	cond := routeReady(second)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, "RouteNameTaken", cond.Reason)
	assert.NotContains(t, cond.Message, "team", "the refusal names no other project")
}

func TestAppRouteName_APlatformNameIsRefusedForANewRoute(t *testing.T) {
	app := namedRoutedApp("system-console-api", "kipper", 9999)
	r, c := routeNameReconciler(t, true, app, namespaceWithUID("kipper", "uid-kipper"))

	require.NoError(t, r.reconcileIngress(context.Background(), app))

	assert.False(t, ingressExists(t, c, "kipper", "system-console-api"))
	cond := routeReady(app)
	require.NotNil(t, cond)
	assert.Equal(t, "RouteNameReservedForPlatform", cond.Reason)
}

func TestAppRouteName_AnExistingRouteOnALivePlatformNameIsRemoved(t *testing.T) {
	app := namedRoutedApp("system-console-api", "kipper", 8080)
	r, c := routeNameReconciler(t, true, app, existingRoute(app),
		platformRouteIngress(routename.PlatformRoute{Namespace: "kipper-system", Service: "console-api", Port: 8080}),
		namespaceWithUID("kipper", "uid-kipper"))

	require.NoError(t, r.reconcileIngress(context.Background(), app))

	assert.False(t, ingressExists(t, c, "kipper", "system-console-api"), "a route that collides with a live platform route is removed")
	assert.Equal(t, "RouteNameReservedForPlatform", routeReady(app).Reason)
}

func TestAppRouteName_AnExistingRouteOnAnAbsentOrOtherPortPlatformNameStays(t *testing.T) {
	cases := map[string]struct {
		port  int32
		extra []crclient.Object
	}{
		"platform route absent, same port": {port: 3080},
		"platform route on another port": {port: 8080, extra: []crclient.Object{
			platformRouteIngress(routename.PlatformRoute{Namespace: "kipper-ai", Service: "librechat-librechat", Port: 3080}),
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			app := namedRoutedApp("librechat", "kipper-ai-librechat", tc.port)
			objs := append([]crclient.Object{app, existingRoute(app), namespaceWithUID("kipper-ai-librechat", "uid-x")}, tc.extra...)
			r, c := routeNameReconciler(t, true, objs...)

			require.NoError(t, r.reconcileIngress(context.Background(), app))

			assert.True(t, ingressExists(t, c, "kipper-ai-librechat", "librechat"), "nothing collides today, so the route keeps working")
		})
	}
}

func TestAppRouteName_AGrandfatheredRouteIsNeverRemoved(t *testing.T) {
	app := namedRoutedApp("web", "team-prod", 8080)
	held := routeNameClaimConfigMap(routeNameClaim{
		Key: routename.Key("team-prod", "web"), Namespace: "team", UID: "uid-team", Shared: []string{"team-prod"}, SharedUIDs: map[string]string{"team-prod": "uid-team-prod"},
	})
	r, c := routeNameReconciler(t, true, app, existingRoute(app), held,
		namespaceWithUID("team", "uid-team"), namespaceWithUID("team-prod", "uid-team-prod"))

	require.NoError(t, r.reconcileIngress(context.Background(), app))

	assert.True(t, ingressExists(t, c, "team-prod", "web"), "an upgrade never removes a grandfathered route")
	cond := routeReady(app)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, "RouteNameShared", cond.Reason)
}

func TestAppRouteName_NoNewRouteBeforeThisGenerationAdmitsIt(t *testing.T) {
	app := namedRoutedApp("web", "shop", 8080)
	r, c := routeNameReconciler(t, false, app, namespaceWithUID("shop", "uid-shop"))

	require.NoError(t, r.reconcileIngress(context.Background(), app))

	assert.False(t, ingressExists(t, c, "shop", "web"))
	var claims corev1.ConfigMapList
	require.NoError(t, c.List(context.Background(), &claims, crclient.MatchingLabels{routeNameClaimLabel: "true"}))
	assert.Empty(t, claims.Items, "nothing is reserved before the generation admits new routes")
	assert.Equal(t, "RouteNamePending", routeReady(app).Reason)
}

func TestRouteNameRetry(t *testing.T) {
	cases := map[string]struct {
		reason string
		want   time.Duration
	}{
		"taken":   {reasonRouteNameTaken, routeNameTakenRetry},
		"pending": {reasonRouteNamePending, routeNamePendingRetry},
		"shared":  {reasonRouteNameShared, 0},
		"other":   {"HostUnavailable", 0},
	}
	for name, tc := range cases {
		app := namedRoutedApp("web", "shop", 8080)
		app.Status.Conditions = []metav1.Condition{{Type: kipperv1.ConditionRouteReady, Reason: tc.reason}}
		if got := routeNameRetry(app); got != tc.want {
			t.Errorf("%s: routeNameRetry = %v, want %v", name, got, tc.want)
		}
	}
	if got := routeNameRetry(namedRoutedApp("web", "shop", 8080)); got != 0 {
		t.Errorf("no condition: routeNameRetry = %v, want 0", got)
	}
}

func TestAppRouteName_AGrandfatheredSharerCanRestoreItsRoute(t *testing.T) {
	app := namedRoutedApp("web", "team-prod", 8080)
	held := routeNameClaimConfigMap(routeNameClaim{
		Key: routename.Key("team-prod", "web"), Namespace: "team", UID: "uid-team", Shared: []string{"team-prod"}, SharedUIDs: map[string]string{"team-prod": "uid-team-prod"},
	})
	r, c := routeNameReconciler(t, true, app, held,
		namespaceWithUID("team", "uid-team"), namespaceWithUID("team-prod", "uid-team-prod"))

	require.NoError(t, r.reconcileIngress(context.Background(), app))

	assert.True(t, ingressExists(t, c, "team-prod", "web"), "a sharer that lost its Ingress may publish it again")
	assert.Equal(t, reasonRouteNameShared, routeReady(app).Reason)
}

func TestAppRouteName_PlatformRemovalLooksAtTheInstalledPort(t *testing.T) {
	app := namedRoutedApp("librechat", "kipper-ai-librechat", 8080)
	installed := existingRoute(app)
	app.Spec.Port = 3080
	r, c := routeNameReconciler(t, true, app, installed,
		platformRouteIngress(routename.PlatformRoute{Namespace: "kipper-ai", Service: "librechat-librechat", Port: 3080}),
		namespaceWithUID("kipper-ai-librechat", "uid-x"))

	require.NoError(t, r.reconcileIngress(context.Background(), app))

	assert.False(t, ingressExists(t, c, "kipper-ai-librechat", "librechat"),
		"on the platform's port the route would collide, and the Service already moved to it, so the route is removed")
	cond := routeReady(app)
	require.NotNil(t, cond)
	assert.Equal(t, reasonRouteNamePlatform, cond.Reason)
}

func TestAppRouteName_ARefusalRaisesAnEvent(t *testing.T) {
	first := namedRoutedApp("prod-web", "team", 8080)
	second := namedRoutedApp("web", "team-prod", 8080)
	r, _ := routeNameReconciler(t, true, first, second,
		namespaceWithUID("team", "uid-team"), namespaceWithUID("team-prod", "uid-team-prod"))
	recorder := record.NewFakeRecorder(10)
	r.Recorder = recorder
	ctx := context.Background()

	require.NoError(t, r.reconcileIngress(ctx, first))
	require.NoError(t, r.reconcileIngress(ctx, second))

	select {
	case e := <-recorder.Events:
		assert.Contains(t, e, "Warning "+reasonRouteNameTaken)
	default:
		t.Fatal("a refused route raises a warning event")
	}
}

func TestAppRouteName_AnAdmittedWriteReleasesTheGate(t *testing.T) {
	app := namedRoutedApp("web", "shop", 8080)
	r, _ := routeNameReconciler(t, true, app, namespaceWithUID("shop", "uid-shop"))

	require.NoError(t, r.reconcileIngress(context.Background(), app))

	closed := make(chan struct{})
	go func() { r.RouteNames.close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("the reconcile kept its admission after writing the route")
	}
}

func TestAppRouteName_AnotherWorkloadInTheSameNamespaceWithTheSameKeyIsRefused(t *testing.T) {
	first := namedRoutedApp("prod-web", "team", 8080)
	second := namedRoutedApp("prod--web", "team", 8080)
	r, c := routeNameReconciler(t, true, first, second, namespaceWithUID("team", "uid-team"))
	ctx := context.Background()

	require.NoError(t, r.reconcileIngress(ctx, first))
	require.NoError(t, r.reconcileIngress(ctx, second))

	assert.False(t, ingressExists(t, c, "team", "prod--web"), "Traefik would file both under team-prod-web")
	assert.Equal(t, reasonRouteNameTaken, routeReady(second).Reason)
}

func TestAppRouteName_ALostTakeoverRaceWaitsInsteadOfRefusing(t *testing.T) {
	app := namedRoutedApp("web", "team-prod", 8080)
	stale := routeNameClaimConfigMap(routeNameClaim{Key: "team-prod-web", Namespace: "gone", UID: "uid-gone"})
	scheme := testScheme()
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, stale, namespaceWithUID("team-prod", "uid-team-prod")).
		WithStatusSubresource(app).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, cl crclient.WithWatch, obj crclient.Object, opts ...crclient.UpdateOption) error {
				if cm, ok := obj.(*corev1.ConfigMap); ok && cm.Namespace == routeClaimNamespace {
					return errors.NewConflict(schema.GroupResource{Resource: "configmaps"}, cm.Name, fmt.Errorf("changed"))
				}
				return cl.Update(ctx, obj, opts...)
			},
		}).Build()
	r := &AppReconciler{Client: c, Scheme: scheme, Domain: "example.com", RouteNames: gateFor(true)}

	require.NoError(t, r.reconcileIngress(context.Background(), app))

	assert.Equal(t, reasonRouteNamePending, routeReady(app).Reason, "a free name lost to a race is looked at again shortly")
}

func TestAppRouteName_ARecreatedHolderNamespaceIsNotTheSharedHolder(t *testing.T) {
	app := namedRoutedApp("prod-web", "team", 8080)
	c := routeNameClaim{Key: "team-prod-web", Namespace: "team", UID: "uid-old"}
	c.setShared([]string{"team-prod"}, map[string]types.UID{"team-prod": "uid-team-prod"})
	r, cl := routeNameReconciler(t, true, app, routeNameClaimConfigMap(c),
		namespaceWithUID("team", "uid-new"), namespaceWithUID("team-prod", "uid-team-prod"))

	require.NoError(t, r.reconcileIngress(context.Background(), app))

	assert.False(t, ingressExists(t, cl, "team", "prod-web"), "a namespace recreated under the holder's name does not inherit its right")
	assert.Equal(t, reasonRouteNameTaken, routeReady(app).Reason)
}

func TestAppRouteName_WhileTheGateIsClosedAnExistingRouteTakesSettingsButNotAHostChange(t *testing.T) {
	t.Run("a setting change applies at once", func(t *testing.T) {
		app := namedRoutedApp("web", "shop", 8080)
		installed := existingRoute(app)
		app.Spec.Route.Path = "/api"
		r, c := routeNameReconciler(t, false, app, installed, namespaceWithUID("shop", "uid-shop"))

		require.NoError(t, r.reconcileIngress(context.Background(), app))

		var ing networkingv1.Ingress
		require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "web", Namespace: "shop"}, &ing))
		assert.Equal(t, "/api", ing.Spec.Rules[0].HTTP.Paths[0].Path)
	})
	t.Run("a host change withdraws the route until it can move", func(t *testing.T) {
		app := namedRoutedApp("web", "shop", 8080)
		installed := existingRoute(app)
		installed.Spec.Rules[0].Host = "old.example.com"
		app.Spec.Route.RequireAPIKey = true
		r, c := routeNameReconciler(t, false, app, installed, namespaceWithUID("shop", "uid-shop"))

		err := r.reconcileIngress(context.Background(), app)

		assert.ErrorIs(t, err, errRouteCreateDeferred, "the reconcile is retried")
		assert.False(t, ingressExists(t, c, "shop", "web"), "the old host would serve without the protection the route now asks for")
		assert.Equal(t, reasonRouteNamePending, routeReady(app).Reason)
	})
}

func TestApplyOwnedIngress_UpdateOnlyNeverCreates(t *testing.T) {
	app := namedRoutedApp("web", "shop", 8080)
	r, c := routeNameReconciler(t, false, app, namespaceWithUID("shop", "uid-shop"))

	err := r.applyOwnedIngress(withRouteUpdateOnly(context.Background()), app, existingRoute(app))

	assert.ErrorIs(t, err, errRouteCreateDeferred, "a skipped create is never reported as written")
	assert.False(t, ingressExists(t, c, "shop", "web"), "a route that is gone is recreated only through the gate")
}

func TestAppRouteName_ARemovedRouteOfAPairInOneNamespaceStaysRefused(t *testing.T) {
	app := namedRoutedApp("prod--web", "team", 8080)
	other := namedRoutedApp("prod-web", "team", 8080)
	held := routeNameClaim{Key: "team-prod-web", Namespace: "team", UID: "uid-team"}
	r, c := routeNameReconciler(t, true, app, other, existingRoute(other), routeNameClaimConfigMap(held), namespaceWithUID("team", "uid-team"))

	require.NoError(t, r.reconcileIngress(context.Background(), app))

	assert.False(t, ingressExists(t, c, "team", "prod--web"), "two names Traefik reads alike cannot both route; rename one")
	assert.Equal(t, reasonRouteNameTaken, routeReady(app).Reason)
}

func TestAppRouteName_AnExistingRouteWithoutARightIsUpdateOnlyEvenWithTheGateOpen(t *testing.T) {
	app := namedRoutedApp("web", "team-prod", 8080)
	held := routeNameClaimConfigMap(routeNameClaim{Key: routename.Key("team-prod", "web"), Namespace: "team", UID: "uid-team"})
	r, _ := routeNameReconciler(t, true, app, existingRoute(app), held,
		namespaceWithUID("team", "uid-team"), namespaceWithUID("team-prod", "uid-team-prod"))

	publish, release, updateOnly, err := r.claimRouteName(context.Background(), app)
	if release != nil {
		release()
	}

	require.NoError(t, err)
	assert.True(t, publish, "the route keeps its settings")
	assert.True(t, updateOnly, "a namespace that no longer holds or shares the name cannot recreate its Ingress")
}

func TestAppRouteName_TheHolderOfAnExistingRouteMayCreateItsIngresses(t *testing.T) {
	app := namedRoutedApp("web", "shop", 8080)
	held := routeNameClaimConfigMap(routeNameClaim{Key: "shop-web", Namespace: "shop", UID: "uid-shop"})
	r, _ := routeNameReconciler(t, true, app, existingRoute(app), held, namespaceWithUID("shop", "uid-shop"))

	_, release, updateOnly, err := r.claimRouteName(context.Background(), app)
	if release != nil {
		release()
	}

	require.NoError(t, err)
	assert.False(t, updateOnly, "its guard Ingresses and a host change still need to be written")
}

func TestAppRouteName_AGuardThatCannotBeCreatedWithdrawsTheRoute(t *testing.T) {
	app := namedRoutedApp("web", "shop", 8080)
	installed := existingRoute(app)
	app.Spec.Route.InternalPaths = []string{"/admin"}
	r, c := routeNameReconciler(t, false, app, installed, namespaceWithUID("shop", "uid-shop"))

	err := r.reconcileIngress(context.Background(), app)

	assert.Error(t, err, "the reconcile fails so it is retried")
	assert.False(t, ingressExists(t, c, "shop", "web"), "a route whose refusals are not in place does not keep serving")
}

func TestAppRouteName_APendingHostChangeDoesNotKeepAnUnprotectedOldHost(t *testing.T) {
	app := namedRoutedApp("web", "shop", 8080)
	installed := existingRoute(app)
	installed.Spec.Rules[0].Host = "old.example.com"
	app.Spec.Route.InternalPaths = []string{"/admin"}
	r, c := routeNameReconciler(t, false, app, installed, namespaceWithUID("shop", "uid-shop"))

	err := r.reconcileIngress(context.Background(), app)

	assert.Error(t, err, "the reconcile fails so it is retried")
	assert.False(t, ingressExists(t, c, "shop", "web"), "the old host would serve /admin without the refusal")
}
