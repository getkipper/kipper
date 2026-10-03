package controllers

import (
	"context"
	stderrors "errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func routeMiddlewares(t *testing.T, c client.Client, name string) []string {
	t.Helper()
	var ing networkingv1.Ingress
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "project-test"}, &ing))
	return strings.Split(ing.Annotations["traefik.ingress.kubernetes.io/router.middlewares"], ",")
}

func stoppableRoutedApp() *kipperv1.App {
	app := newTestApp()
	app.UID = types.UID("uid-my-app")
	app.Spec.Route = &kipperv1.AppRoute{Host: "shop.example.com", BasicAuth: true, RequireAPIKey: true}
	return app
}

func TestStoppedPage_LastInTheChainOnlyWhileStopped(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	app := stoppableRoutedApp()
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}

	require.NoError(t, r.reconcileIngress(ctx, app))
	assert.NotContains(t, routeMiddlewares(t, c, app.Name), stoppedPageRef, "a running app's own 503s must reach its visitors")

	stop(app)
	require.NoError(t, r.reconcileIngress(ctx, app))
	chain := routeMiddlewares(t, c, app.Name)
	assert.Equal(t, stoppedPageRef, chain[len(chain)-1], "auth and the API-key gate must run before the stopped page")
	assert.Contains(t, chain, "project-test-my-app-basic-auth@kubernetescrd")

	mw := middleware(t, c, "kipper-system", "app-stopped")
	errorsSpec, _, _ := unstructured.NestedMap(mw.Object, "spec", "errors")
	assert.Equal(t, []interface{}{"503"}, errorsSpec["status"])
	assert.Equal(t, "/stopped", errorsSpec["query"])
	svc, _, _ := unstructured.NestedMap(errorsSpec, "service")
	assert.Equal(t, "kipper-authz", svc["name"])
	assert.EqualValues(t, 8080, svc["port"])

	start(app)
	require.NoError(t, r.reconcileIngress(ctx, app))
	assert.NotContains(t, routeMiddlewares(t, c, app.Name), stoppedPageRef)
}

// A middleware write failure must allow route reconciliation and schedule a retry.
func TestStoppedPage_FailedApplyLeavesItOutAndRetries(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	app := stoppableRoutedApp()
	stop(app)
	fail := true
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if fail && obj.GetName() == "app-stopped" {
					return stderrors.New("refused")
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}

	require.NoError(t, r.reconcileIngress(ctx, app))
	assert.NotContains(t, routeMiddlewares(t, c, app.Name), stoppedPageRef)
	assert.Equal(t, rolloutRequeue, r.stoppedPageRetry(app), "a failed apply must schedule its own retry")

	fail = false
	require.NoError(t, r.reconcileIngress(ctx, app))
	chain := routeMiddlewares(t, c, app.Name)
	assert.Equal(t, stoppedPageRef, chain[len(chain)-1])
	assert.Zero(t, r.stoppedPageRetry(app))
}

// A path reopened under the route guard serves through the app's full chain,
// so it shows the stopped page too; a refused internal path keeps answering
// 404 and never reaches it.
func TestStoppedPage_GuardIngresses(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	app := guardedApp()
	app.Spec.Route.PublicPaths = []string{"/actuator/health"}
	stop(app)
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, routeGuardOn()).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}

	require.NoError(t, r.reconcileIngress(ctx, app))

	public := routeMiddlewares(t, c, PublicPathsIngressName(app.Name))
	assert.Equal(t, stoppedPageRef, public[len(public)-1])
	assert.NotContains(t, routeMiddlewares(t, c, InternalPathsIngressName(app.Name)), stoppedPageRef)
}

// The shared middleware should converge after a failed write or deletion,
// then require no further writes while its spec matches.
func TestStoppedPage_WrittenOnlyWhenMissingOrChanged(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	app := stoppableRoutedApp()
	stop(app)
	patches, fail := 0, true
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if obj.GetName() == "app-stopped" {
					patches++
					if fail {
						return stderrors.New("refused")
					}
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}

	require.NoError(t, r.reconcileIngress(ctx, app))
	fail = false
	require.NoError(t, r.reconcileIngress(ctx, app))
	require.NoError(t, r.reconcileIngress(ctx, app))
	require.NoError(t, r.reconcileIngress(ctx, app))

	assert.Equal(t, 2, patches, "one failed write, then one that succeeded and holds")
	chain := routeMiddlewares(t, c, app.Name)
	assert.Equal(t, stoppedPageRef, chain[len(chain)-1])

	require.NoError(t, c.Delete(ctx, middleware(t, c, stoppedPageNamespace, stoppedPageMiddleware)))
	require.NoError(t, r.reconcileIngress(ctx, app))
	assert.Equal(t, 3, patches, "a middleware deleted by hand is written again at once")
}
