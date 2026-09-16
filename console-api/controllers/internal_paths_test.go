package controllers

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

// servedBy models router selection by priority, using rule length as the default.
// Prefix matching uses strings.HasPrefix to exercise the guard's segment boundaries.
func servedBy(t *testing.T, c crclient.Client, path string) string {
	t.Helper()

	var list networkingv1.IngressList
	require.NoError(t, c.List(context.Background(), &list, crclient.InNamespace("project-test")))

	best, bestPriority := "", -1
	for i := range list.Items {
		ing := &list.Items[i]
		priority := 0
		if p, ok := ing.Annotations["traefik.ingress.kubernetes.io/router.priority"]; ok {
			n, err := strconv.Atoi(p)
			require.NoError(t, err, "router.priority must be a number")
			priority = n
		}
		for _, rule := range ing.Spec.Rules {
			if rule.Host != guardedHost || rule.HTTP == nil {
				continue
			}
			for _, p := range rule.HTTP.Paths {
				matched := path == p.Path
				if !matched && p.PathType != nil && *p.PathType != networkingv1.PathTypeExact {
					matched = strings.HasPrefix(path, p.Path)
				}
				if !matched {
					continue
				}
				rank := priority
				if rank == 0 {
					rank = len(rule.Host) + len(p.Path)
				}
				if rank > bestPriority {
					best, bestPriority = ing.Name, rank
				}
			}
		}
	}
	return best
}

const guardedHost = "webapp.example.com"

func guardedApp() *kipperv1.App {
	app := newTestApp()
	app.UID = types.UID("uid-my-app")
	app.Spec.Route = &kipperv1.AppRoute{Host: guardedHost, Path: "/domains-api"}
	return app
}

func routeGuardOn() *kipperv1.PlatformConfig {
	return &kipperv1.PlatformConfig{
		ObjectMeta: metav1.ObjectMeta{Name: PlatformConfigName},
		Spec: kipperv1.PlatformConfigSpec{
			Profile:    "small",
			RouteGuard: &kipperv1.RouteGuardSpec{BlockInternalPaths: true},
		},
	}
}

func middleware(t *testing.T, c crclient.Client, namespace, name string) *unstructured.Unstructured {
	t.Helper()
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(schema.GroupVersionKind{Group: "traefik.io", Version: "v1alpha1", Kind: "Middleware"})
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: namespace}, got))
	return got
}

func TestRouteGuard_RefusesInternalPathsAndStillServesTheApp(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	app := guardedApp()
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, routeGuardOn()).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}

	require.NoError(t, r.reconcileIngress(ctx, app))

	for _, path := range []string{
		"/domains-api/actuator",
		"/domains-api/actuator/",
		"/domains-api/actuator/metrics",
		"/domains-api/internal/jobs",
		"/domains-api/.git/config",
		"/domains-api/.env",
		// Servlet backends strip these path parameters.
		"/domains-api/actuator;jsessionid=1/metrics",
		"/domains-api/actuator;x=1",
		"/domains-api/.env;x=1",
		// Cover backends that decode encoded separators.
		"/domains-api/.git%2Fconfig",
		"/domains-api/.git%2fconfig",
	} {
		assert.Equal(t, "my-app-internal-paths", servedBy(t, c, path),
			"%s must be refused, not served", path)
	}
	for _, path := range []string{
		"/domains-api",
		"/domains-api/api/v1/domains",
		"/domains-api/actuators",
		"/domains-api/internally",
	} {
		assert.Equal(t, "my-app", servedBy(t, c, path),
			"%s is the app's own path and must still be served", path)
	}

	deny := middleware(t, c, "project-test", "my-app-internal-paths")
	address, found, err := unstructured.NestedString(deny.Object, "spec", "forwardAuth", "address")
	require.NoError(t, err)
	require.True(t, found, "the refusal must be a forwardAuth")
	assert.Equal(t, denyAuthAddress, address)

	guard := &networkingv1.Ingress{}
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "my-app-internal-paths", Namespace: "project-test"}, guard))
	assert.Equal(t, "project-test-my-app-internal-paths@kubernetescrd",
		lastMiddleware(guard), "the refusal must be the last thing in the chain")
	require.Len(t, guard.Spec.TLS, 1, "the guard must reach the entrypoint the traffic arrives on")
	assert.Equal(t, []string{guardedHost}, guard.Spec.TLS[0].Hosts)
	assert.Equal(t, "my-app-tls", guard.Spec.TLS[0].SecretName, "the same certificate as the route it guards")
	assert.NotContains(t, guard.Annotations, "cert-manager.io/cluster-issuer",
		"only the route's own Ingress may ask for the certificate")
}

func lastMiddleware(ing *networkingv1.Ingress) string {
	parts := strings.Split(ing.Annotations["traefik.ingress.kubernetes.io/router.middlewares"], ",")
	return parts[len(parts)-1]
}

func TestRouteGuard_DeclaredPathsAreRefusedWithTheClusterDefaultOff(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	app := guardedApp()
	app.Spec.Route.InternalPaths = []string{"/admin"}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}

	require.NoError(t, r.reconcileIngress(ctx, app))

	assert.Equal(t, "my-app-internal-paths", servedBy(t, c, "/domains-api/admin/users"))
	assert.Equal(t, "my-app", servedBy(t, c, "/domains-api/actuator/metrics"),
		"the default list is off on this cluster, so nothing else changes")
	assert.Equal(t, "my-app", servedBy(t, c, "/domains-api/administration"),
		"a declared prefix must stop at a segment boundary")
}

func TestRouteGuard_APublicPathIsServedAgain(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	app := guardedApp()
	app.Spec.Route.PublicPaths = []string{"/actuator/prometheus"}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, routeGuardOn()).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}

	require.NoError(t, r.reconcileIngress(ctx, app))

	assert.Equal(t, "my-app-public-paths", servedBy(t, c, "/domains-api/actuator/prometheus"))
	assert.Equal(t, "my-app-internal-paths", servedBy(t, c, "/domains-api/actuator/metrics"),
		"the rest of the prefix stays refused")
	assert.Equal(t, "my-app-internal-paths", servedBy(t, c, "/domains-api/actuator/prometheusX"),
		"the opt-out names one path, not a prefix of it")
	assert.Equal(t, "my-app-internal-paths", servedBy(t, c, "/domains-api/actuator/prometheus/all"),
		"and not the subtree below it either")

	assert.Equal(t, "my-app-internal-paths", servedBy(t, c, "/domains-api/actuator/prometheus/../env"),
		"a reopened path must not become a way back into the prefix above it")
	assert.Equal(t, "my-app-public-paths", servedBy(t, c, "/domains-api/actuator/prometheus/"),
		"the trailing-slash form of the same path is the same resource")

	allow := &networkingv1.Ingress{}
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "my-app-public-paths", Namespace: "project-test"}, allow))
	assert.Contains(t, allow.Annotations["traefik.ingress.kubernetes.io/router.middlewares"],
		"my-app-strip-prefix@kubernetescrd",
		"a reopened path is served by the route, so it carries the route's own chain")
}

func TestRouteGuard_APublicPathNothingRefusesBuildsNoRouter(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	app := guardedApp()
	app.Spec.Route.PublicPaths = []string{"/api/v1"}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, routeGuardOn()).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}

	require.NoError(t, r.reconcileIngress(ctx, app))

	err := c.Get(ctx, types.NamespacedName{Name: "my-app-public-paths", Namespace: "project-test"}, &networkingv1.Ingress{})
	assert.True(t, errors.IsNotFound(err), "nothing to reopen, so nothing to build")
}

func TestRouteGuard_TurningItOffRemovesTheRefusal(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	app := guardedApp()
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, routeGuardOn()).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}
	require.NoError(t, r.reconcileIngress(ctx, app))
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "my-app-internal-paths", Namespace: "project-test"}, &networkingv1.Ingress{}))

	var pc kipperv1.PlatformConfig
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: PlatformConfigName}, &pc))
	pc.Spec.RouteGuard.BlockInternalPaths = false
	require.NoError(t, c.Update(ctx, &pc))

	require.NoError(t, r.reconcileIngress(ctx, app))

	err := c.Get(ctx, types.NamespacedName{Name: "my-app-internal-paths", Namespace: "project-test"}, &networkingv1.Ingress{})
	assert.True(t, errors.IsNotFound(err), "the refusing Ingress must go with the setting")
	mw := &unstructured.Unstructured{}
	mw.SetGroupVersionKind(schema.GroupVersionKind{Group: "traefik.io", Version: "v1alpha1", Kind: "Middleware"})
	err = c.Get(ctx, types.NamespacedName{Name: "my-app-internal-paths", Namespace: "project-test"}, mw)
	assert.True(t, errors.IsNotFound(err), "and so must the middleware behind it")
	assert.Equal(t, "my-app", servedBy(t, c, "/domains-api/actuator/metrics"))
}

func TestRouteGuard_RemovingTheRouteRemovesTheGuard(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	app := guardedApp()
	app.Spec.Route.PublicPaths = []string{"/actuator/prometheus"}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, routeGuardOn()).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}
	require.NoError(t, r.reconcileIngress(ctx, app))

	app.Spec.Route = nil
	require.NoError(t, r.reconcileIngress(ctx, app))

	for _, name := range []string{"my-app", "my-app-internal-paths", "my-app-public-paths"} {
		err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: "project-test"}, &networkingv1.Ingress{})
		assert.True(t, errors.IsNotFound(err), "%s must go with the route", name)
	}
}

func TestRouteGuard_ARefusalItCannotInstallStopsTheRoute(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	app := guardedApp()
	foreign := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "my-app-internal-paths", Namespace: "project-test"},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, routeGuardOn(), foreign).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}

	err := r.reconcileIngress(ctx, app)

	require.Error(t, err, "a refusal that cannot be installed is not a successful reconcile")
	notFound := c.Get(ctx, types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &networkingv1.Ingress{})
	assert.True(t, errors.IsNotFound(notFound),
		"the route must not be published while its refusals are missing")
}

func TestRouteGuard_AnUnusableRoutePathStopsTheRoute(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	app := guardedApp()
	app.Spec.Route.Path = "/domains-api/../admin"
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, routeGuardOn()).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}

	err := r.reconcileIngress(ctx, app)

	require.Error(t, err)
	notFound := c.Get(ctx, types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &networkingv1.Ingress{})
	assert.True(t, errors.IsNotFound(notFound), "the route must not be published unguarded")
}

// Simulate an invalid declaration accepted by an older CRD schema.
func TestRouteGuard_ReportsAPathItCannotRefuse(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	app := guardedApp()
	app.Spec.Route.InternalPaths = []string{"/ops", "/admin/../private"}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, routeGuardOn()).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}

	require.NoError(t, r.reconcileIngress(ctx, app))

	assert.Equal(t, "my-app-internal-paths", servedBy(t, c, "/domains-api/ops/x"),
		"the entries that do work must still be refused")
	cond := apimeta.FindStatusCondition(app.Status.Conditions, kipperv1.ConditionRouteReady)
	require.NotNil(t, cond, "the route must say what it could not refuse")
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Contains(t, cond.Message, "/admin/../private")
}

func TestRouteGuard_LeavesAForeignIngressAlone(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	app := guardedApp()
	foreign := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "my-app-internal-paths", Namespace: "project-test"},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, foreign).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}

	require.NoError(t, r.reconcileIngress(ctx, app))

	assert.NoError(t, c.Get(ctx, types.NamespacedName{Name: "my-app-internal-paths", Namespace: "project-test"},
		&networkingv1.Ingress{}), "an Ingress this app does not own must survive")
}

func TestRouteGuard_OnlyAGuardChangeReachesEveryApp(t *testing.T) {
	off := &kipperv1.PlatformConfig{
		ObjectMeta: metav1.ObjectMeta{Name: PlatformConfigName},
		Spec:       kipperv1.PlatformConfigSpec{Profile: "small"},
	}
	resized := off.DeepCopy()
	resized.Spec.Components = []kipperv1.ComponentOverride{{Name: "loki", MemoryLimit: "2Gi"}}
	on := off.DeepCopy()
	on.Spec.RouteGuard = &kipperv1.RouteGuardSpec{BlockInternalPaths: true}

	p := routeGuardChanged()
	assert.False(t, p.Update(event.UpdateEvent{ObjectOld: off, ObjectNew: resized}),
		"a component resize changes nothing about what a route refuses")
	assert.True(t, p.Update(event.UpdateEvent{ObjectOld: off, ObjectNew: on}),
		"turning the guard on has to reach every routed app")
	assert.True(t, p.Update(event.UpdateEvent{ObjectOld: on, ObjectNew: off}),
		"and so does turning it off")
}

// Exercise guard installation on an already published route.
func TestRouteGuard_WithdrawsAPublishedRouteItCannotGuard(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	app := guardedApp()
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}

	// Publish with cluster defaults disabled.
	require.NoError(t, r.reconcileIngress(ctx, app))
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &networkingv1.Ingress{}))

	// Enable defaults with a guard name already owned by another app.
	require.NoError(t, c.Create(ctx, &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "my-app-internal-paths", Namespace: "project-test"},
	}))
	require.NoError(t, c.Create(ctx, routeGuardOn()))

	err := r.reconcileIngress(ctx, app)

	require.Error(t, err)
	notFound := c.Get(ctx, types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &networkingv1.Ingress{})
	assert.True(t, errors.IsNotFound(notFound),
		"a route that cannot be guarded must stop publishing, not keep serving what it claims to refuse")
	cond := apimeta.FindStatusCondition(app.Status.Conditions, kipperv1.ConditionRouteReady)
	require.NotNil(t, cond)
	assert.Contains(t, cond.Message, "withdrawn")
	assert.NoError(t, c.Get(ctx, types.NamespacedName{Name: "my-app-internal-paths", Namespace: "project-test"},
		&networkingv1.Ingress{}), "and the object that is not ours is still not ours to delete")
}

func TestRouteGuard_KeepsAWorkingRouteThroughATransientFailure(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	app := guardedApp()
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, routeGuardOn()).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}
	require.NoError(t, r.reconcileIngress(ctx, app))

	// Inject a guard write failure while retaining the installed objects.
	blocked := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(app, routeGuardOn(), ingressNamed(t, c, "my-app"), ingressNamed(t, c, "my-app-internal-paths")).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, cl crclient.WithWatch, obj crclient.Object, opts ...crclient.UpdateOption) error {
				if _, isIngress := obj.(*networkingv1.Ingress); isIngress && obj.GetName() == "my-app-internal-paths" {
					return errors.NewConflict(schema.GroupResource{Resource: "ingresses"}, obj.GetName(), assert.AnError)
				}
				return cl.Update(ctx, obj, opts...)
			},
		}).Build()
	failing := &AppReconciler{Client: blocked, Scheme: scheme}

	err := failing.reconcileIngress(ctx, app)

	require.Error(t, err)
	assert.NoError(t, blocked.Get(ctx, types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &networkingv1.Ingress{}),
		"a route whose refusals are already installed must survive a failed write")
}

func ingressNamed(t *testing.T, c crclient.Client, name string) *networkingv1.Ingress {
	t.Helper()
	var ing networkingv1.Ingress
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: name, Namespace: "project-test"}, &ing))
	ing.ResourceVersion = ""
	return &ing
}

func TestRouteGuard_TheObjectItBuildsIsTheObjectTheConsoleChecksFor(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	app := guardedApp()
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, routeGuardOn()).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}
	require.NoError(t, r.reconcileIngress(ctx, app))

	var guard networkingv1.Ingress
	require.NoError(t, c.Get(ctx, types.NamespacedName{
		Name: InternalPathsIngressName("my-app"), Namespace: "project-test"}, &guard))

	assert.True(t, RefusalInstalled(&guard, nil, app, guardedHost, true),
		"the guard the reconciler just built must read as installed")

	widened := app.DeepCopy()
	widened.Spec.Route.InternalPaths = []string{"/ops"}
	assert.False(t, RefusalInstalled(&guard, nil, widened, guardedHost, true),
		"a guard built before the route was widened does not yet refuse what it now names")

	assert.False(t, RefusalInstalled(&guard, nil, app, "elsewhere.example.com", true),
		"nor does one serving a host this route no longer uses")

	assert.False(t, RefusalInstalled(&networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: InternalPathsIngressName("my-app"), Namespace: "project-test"},
	}, nil, app, guardedHost, true),
		"and an object under the name that refuses nothing is not a refusal")

	// A stale public exception outranks the deny rules.
	stale := guard.DeepCopy()
	stale.Name = PublicPathsIngressName("my-app")
	assert.False(t, RefusalInstalled(&guard, stale, app, guardedHost, true),
		"a public exception left behind by an edit still serves what this reports as refused")

	chainless := guard.DeepCopy()
	chainless.Annotations[routerMiddlewaresAnnotation] = "traefik-rate-limit@kubernetescrd"
	assert.False(t, RefusalInstalled(chainless, nil, app, guardedHost, true),
		"a refusing router with nothing refusing in its chain is not a refusal")

	adopted := guard.DeepCopy()
	adopted.OwnerReferences = nil
	assert.False(t, RefusalInstalled(adopted, nil, app, guardedHost, true),
		"nor is an object this app does not control")
}

func TestRouteGuard_KeepsTheRouteOnlyWhileTheClusterRefuses(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	app := guardedApp()
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, routeGuardOn()).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}
	require.NoError(t, r.reconcileIngress(ctx, app))

	assert.True(t, r.guardEnforcing(ctx, app, guardedHost, true))

	mw := &unstructured.Unstructured{}
	mw.SetGroupVersionKind(middlewareGVK)
	mw.SetName(InternalPathsIngressName("my-app"))
	mw.SetNamespace("project-test")
	require.NoError(t, c.Delete(ctx, mw))

	assert.False(t, r.guardEnforcing(ctx, app, guardedHost, true),
		"a chain naming a Middleware that is gone refuses nothing")
}

func TestRouteGuard_WithdrawsWhenTheMiddlewareNameIsTaken(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	app := guardedApp()
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}
	require.NoError(t, r.reconcileIngress(ctx, app))
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &networkingv1.Ingress{}))

	foreign := &unstructured.Unstructured{}
	foreign.SetGroupVersionKind(middlewareGVK)
	foreign.SetName(InternalPathsIngressName("my-app"))
	foreign.SetNamespace("project-test")
	require.NoError(t, c.Create(ctx, foreign))
	require.NoError(t, c.Create(ctx, routeGuardOn()))

	err := r.reconcileIngress(ctx, app)

	require.Error(t, err)
	notFound := c.Get(ctx, types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &networkingv1.Ingress{})
	assert.True(t, errors.IsNotFound(notFound),
		"a route whose refusal has nowhere to live must stop publishing")
}

func TestRouteGuard_WithdrawsWhenTheFirstGuardWriteFails(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	app := guardedApp()
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}
	require.NoError(t, r.reconcileIngress(ctx, app))

	published := ingressNamed(t, c, "my-app")
	blocked := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(app, routeGuardOn(), published).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl crclient.WithWatch, obj crclient.Object, opts ...crclient.CreateOption) error {
				if _, isIngress := obj.(*networkingv1.Ingress); isIngress && obj.GetName() == InternalPathsIngressName("my-app") {
					return errors.NewForbidden(schema.GroupResource{Resource: "ingresses"}, obj.GetName(), assert.AnError)
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).Build()
	failing := &AppReconciler{Client: blocked, Scheme: scheme}

	err := failing.reconcileIngress(ctx, app)

	require.Error(t, err)
	notFound := blocked.Get(ctx, types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &networkingv1.Ingress{})
	assert.True(t, errors.IsNotFound(notFound),
		"nothing is refusing these paths, so the route must not publish them")
}

func TestRouteGuard_WhatDoesNotCountAsARefusal(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	app := guardedApp()
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, routeGuardOn()).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}
	require.NoError(t, r.reconcileIngress(ctx, app))

	var guard networkingv1.Ingress
	require.NoError(t, c.Get(ctx, types.NamespacedName{
		Name: InternalPathsIngressName("my-app"), Namespace: "project-test"}, &guard))

	// This different middleware reference contains the expected reference as a substring.
	impostor := guard.DeepCopy()
	impostor.Annotations[routerMiddlewaresAnnotation] = "project-test-xproject-test-my-app-internal-paths@kubernetescrd"
	require.Contains(t, impostor.Annotations[routerMiddlewaresAnnotation],
		"project-test-my-app-internal-paths@kubernetescrd",
		"the point of this case is that searching the chain finds it")
	assert.False(t, RefusalInstalled(impostor, nil, app, guardedHost, true),
		"a chain naming something else that merely ends the same way refuses nothing")
	assert.False(t, refusalEnforced(impostor, nil, app, guardedHost, true))

	otherClass := guard.DeepCopy()
	otherClass.Spec.IngressClassName = strPtr("nginx")
	assert.False(t, RefusalInstalled(otherClass, nil, app, guardedHost, true),
		"a guard assigned to another controller is not refusing anything")
	assert.False(t, refusalEnforced(otherClass, nil, app, guardedHost, true))

	classless := guard.DeepCopy()
	classless.Spec.IngressClassName = nil
	assert.False(t, RefusalInstalled(classless, nil, app, guardedHost, true))
}

func TestRouteGuard_ABroaderRefusalIsStillARefusal(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	app := guardedApp()
	app.Spec.Route.InternalPaths = []string{"/ops"}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, routeGuardOn()).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}
	require.NoError(t, r.reconcileIngress(ctx, app))

	var guard networkingv1.Ingress
	require.NoError(t, c.Get(ctx, types.NamespacedName{
		Name: InternalPathsIngressName("my-app"), Namespace: "project-test"}, &guard))

	// Remove /ops from the policy while its deny rule remains installed.
	narrowed := app.DeepCopy()
	narrowed.Spec.Route.InternalPaths = nil

	assert.True(t, refusalEnforced(&guard, nil, narrowed, guardedHost, true),
		"everything this route asks to refuse is refused, so it must not be taken offline")
	assert.False(t, RefusalInstalled(&guard, nil, narrowed, guardedHost, true),
		"the console still reports it as unconfirmed, because it is not what the route asks for")

	// A missing public exception also keeps enforcement stricter than requested.
	withException := app.DeepCopy()
	withException.Spec.Route.PublicPaths = []string{"/ops/metrics"}
	assert.True(t, refusalEnforced(&guard, nil, withException, guardedHost, true))
	assert.False(t, RefusalInstalled(&guard, nil, withException, guardedHost, true))
}

func TestRouteGuard_AStaleExceptionIsNotEnforcement(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	app := guardedApp()
	app.Spec.Route.PublicPaths = []string{"/actuator/prometheus"}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, routeGuardOn()).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}
	require.NoError(t, r.reconcileIngress(ctx, app))

	var deny, allow networkingv1.Ingress
	require.NoError(t, c.Get(ctx, types.NamespacedName{
		Name: InternalPathsIngressName("my-app"), Namespace: "project-test"}, &deny))
	require.NoError(t, c.Get(ctx, types.NamespacedName{
		Name: PublicPathsIngressName("my-app"), Namespace: "project-test"}, &allow))

	withdrawn := app.DeepCopy()
	withdrawn.Spec.Route.PublicPaths = nil
	assert.False(t, refusalEnforced(&deny, &allow, withdrawn, guardedHost, true),
		"the exception is still being served, so the refusal is not in force")
	assert.False(t, RefusalInstalled(&deny, &allow, withdrawn, guardedHost, true))
}

func TestRouteGuard_AnUnreadableGuardIsNotAnAbsentOne(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	app := guardedApp()
	app.Spec.Route.PublicPaths = []string{"/actuator/prometheus"}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, routeGuardOn()).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}
	require.NoError(t, r.reconcileIngress(ctx, app))

	// Remove the exception from the policy and fail the read of its stale Ingress.
	narrowed := app.DeepCopy()
	narrowed.Spec.Route.PublicPaths = nil
	unreadable := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(narrowed, routeGuardOn(),
			ingressNamed(t, c, "my-app"),
			ingressNamed(t, c, InternalPathsIngressName("my-app")),
			ingressNamed(t, c, PublicPathsIngressName("my-app")),
			denyMiddlewareFrom(t, c)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl crclient.WithWatch, key crclient.ObjectKey, obj crclient.Object, opts ...crclient.GetOption) error {
				if _, isIngress := obj.(*networkingv1.Ingress); isIngress && key.Name == PublicPathsIngressName("my-app") {
					return errors.NewForbidden(schema.GroupResource{Resource: "ingresses"}, key.Name, assert.AnError)
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	blind := &AppReconciler{Client: unreadable, Scheme: scheme}

	// The remaining objects satisfy enforcement checks.
	readable := &AppReconciler{Client: crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(narrowed, routeGuardOn(),
			ingressNamed(t, c, "my-app"),
			ingressNamed(t, c, InternalPathsIngressName("my-app")),
			denyMiddlewareFrom(t, c)).Build(), Scheme: scheme}
	require.True(t, readable.guardEnforcing(ctx, narrowed, guardedHost, true),
		"with the exception genuinely gone, this cluster is enforcing")

	assert.False(t, blind.guardEnforcing(ctx, narrowed, guardedHost, true),
		"an exception that may still be serving cannot be assumed away")
}

func denyMiddlewareFrom(t *testing.T, c crclient.Client) *unstructured.Unstructured {
	t.Helper()
	mw := &unstructured.Unstructured{}
	mw.SetGroupVersionKind(middlewareGVK)
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{
		Name: InternalPathsIngressName("my-app"), Namespace: "project-test"}, mw))
	mw.SetResourceVersion("")
	return mw
}

// A failed host migration must leave the old host protected.
func TestRouteGuard_AHostChangeDoesNotLeaveTheOldNamePublished(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	app := guardedApp()
	app.Spec.Route.Host = "old.example.com"
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, routeGuardOn()).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}
	require.NoError(t, r.reconcileIngress(ctx, app))

	// Move to a new hostname and fail the publication write.
	moved := app.DeepCopy()
	moved.Spec.Route.Host = "new.example.com"
	blocked := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(moved, routeGuardOn(),
			ingressNamed(t, c, "my-app"),
			ingressNamed(t, c, InternalPathsIngressName("my-app")),
			denyMiddlewareFrom(t, c)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl crclient.WithWatch, obj crclient.Object, opts ...crclient.CreateOption) error {
				if isAppIngress(obj) {
					return errors.NewForbidden(schema.GroupResource{Resource: "ingresses"}, obj.GetName(), assert.AnError)
				}
				return cl.Create(ctx, obj, opts...)
			},
			Update: func(ctx context.Context, cl crclient.WithWatch, obj crclient.Object, opts ...crclient.UpdateOption) error {
				if isAppIngress(obj) {
					return errors.NewForbidden(schema.GroupResource{Resource: "ingresses"}, obj.GetName(), assert.AnError)
				}
				return cl.Update(ctx, obj, opts...)
			},
		}).Build()
	failing := &AppReconciler{Client: blocked, Scheme: scheme}

	err := failing.reconcileIngress(ctx, moved)
	require.Error(t, err, "the route could not be republished")

	var published networkingv1.Ingress
	readErr := blocked.Get(ctx, types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &published)
	if readErr == nil {
		for _, rule := range published.Spec.Rules {
			assert.NotEqual(t, "old.example.com", rule.Host,
				"the hostname the route left behind must not still be served")
		}
	} else {
		assert.True(t, errors.IsNotFound(readErr))
	}
}

func isAppIngress(obj crclient.Object) bool {
	_, isIngress := obj.(*networkingv1.Ingress)
	return isIngress && obj.GetName() == "my-app"
}

func TestRouteGuard_AnUnchangedHostIsNotWithdrawn(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	app := guardedApp()
	app.Spec.Route.RedirectFrom = []string{"www.example.com"}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, routeGuardOn()).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}
	require.NoError(t, r.reconcileIngress(ctx, app))
	require.NoError(t, r.reconcileIngress(ctx, app))

	var published networkingv1.Ingress
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &published))
	hosts := map[string]bool{}
	for _, rule := range published.Spec.Rules {
		hosts[rule.Host] = true
	}
	assert.True(t, hosts[guardedHost], "the route's own hostname keeps serving")
	assert.True(t, hosts["www.example.com"], "and so does an alias it redirects from")
}

// Test withdrawal directly: a full reconcile can recreate the Ingress and
// conceal an unnecessary deletion during an alias edit.
func TestRouteGuard_WithdrawsOnlyWhenTheCanonicalHostMoved(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	app := guardedApp()

	published := func(t *testing.T, hosts ...string) *AppReconciler {
		t.Helper()
		ing := &networkingv1.Ingress{
			ObjectMeta: metav1.ObjectMeta{
				Name: "my-app", Namespace: "project-test",
				Labels: map[string]string{"app": "my-app", kipperLabel: kipperValue},
			},
		}
		for _, h := range hosts {
			ing.Spec.Rules = append(ing.Spec.Rules, networkingv1.IngressRule{Host: h})
		}
		require.NoError(t, controllerutil.SetControllerReference(app, ing, scheme))
		return &AppReconciler{
			Client: crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, ing).Build(),
			Scheme: scheme,
		}
	}
	stillPublished := func(r *AppReconciler) bool {
		return r.Get(ctx, types.NamespacedName{Name: "my-app", Namespace: "project-test"},
			&networkingv1.Ingress{}) == nil
	}

	// The old canonical host becomes an alias, but still serves the backend
	// until reconciliation. Withdraw it before moving the guard.
	r := published(t, "old.example.com")
	require.NoError(t, r.withdrawSupersededPublication(ctx, app, "new.example.com"))
	assert.False(t, stillPublished(r),
		"the name being redirected from must not keep routing to the backend while the guard moves")

	r = published(t, "old.example.com")
	require.NoError(t, r.withdrawSupersededPublication(ctx, app, "new.example.com"))
	assert.False(t, stillPublished(r))

	// Alias removal preserves the canonical host.
	r = published(t, "app.example.com", "www.example.com")
	require.NoError(t, r.withdrawSupersededPublication(ctx, app, "app.example.com"))
	assert.True(t, stillPublished(r),
		"the canonical host did not move, so the route is not withdrawn")

	// Alias addition also preserves the canonical host.
	r = published(t, "app.example.com")
	require.NoError(t, r.withdrawSupersededPublication(ctx, app, "app.example.com"))
	assert.True(t, stillPublished(r))

	foreign := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "my-app", Namespace: "project-test"},
		Spec:       networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{Host: "old.example.com"}}},
	}
	stranger := &AppReconciler{
		Client: crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, foreign).Build(),
		Scheme: scheme,
	}
	require.NoError(t, stranger.withdrawSupersededPublication(ctx, app, "new.example.com"))
	assert.True(t, stillPublished(stranger), "not ours to withdraw")
}
