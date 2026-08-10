package controllers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// The policy is deliberately permissive. It is attached to workloads Kipper
// knows nothing about, and removing 'unsafe-inline' would break every app with
// an inline script or handler, on upgrade, with a console error as the only
// clue. Tightening it is an opt-in feature rather than a change of default, so
// this pins the default against being tightened by accident.
func TestBuildWorkloadCSP_DefaultPolicy(t *testing.T) {
	assert.Equal(t,
		"default-src 'self'; script-src 'self' 'unsafe-inline' 'unsafe-eval'; "+
			"style-src 'self' 'unsafe-inline'; img-src 'self' data: https:; "+
			"font-src 'self' data: https:; connect-src 'self' wss: https:;",
		buildWorkloadCSP(nil))

	assert.Equal(t, buildWorkloadCSP(nil), buildWorkloadCSP([]string{}),
		"an empty allowlist is the same as none")
}

func TestBuildWorkloadCSP_AllowlistReachesTheFetchDirectives(t *testing.T) {
	csp := buildWorkloadCSP([]string{"cdn.example.com", "fonts.example.com"})

	assert.Contains(t, csp, "script-src 'self' 'unsafe-inline' 'unsafe-eval' cdn.example.com fonts.example.com;")
	assert.Contains(t, csp, "style-src 'self' 'unsafe-inline' cdn.example.com fonts.example.com;")
	assert.Contains(t, csp, "font-src 'self' data: https: cdn.example.com fonts.example.com;")

	// connect-src already carries the https: scheme-source, which permits any
	// HTTPS origin, so the allowlist would add nothing there.
	assert.Contains(t, csp, "connect-src 'self' wss: https:;")
}

// The regression this exists for: the App builder took the CR's own slice and
// rewrote its entries in place, so reconciling an app quietly rewrote
// app.Spec.Route.CSPAllowlist on the live object. The rewritten values were
// never read, which is the only reason it did not also change the header.
func TestBuildWorkloadCSP_DoesNotMutateTheCallersSlice(t *testing.T) {
	allowlist := []string{"cdn.example.com", "https://fonts.example.com"}
	before := append([]string(nil), allowlist...)

	buildWorkloadCSP(allowlist)

	assert.Equal(t, before, allowlist, "a builder must not rewrite the caller's slice")
}

// Apps and functions serve the same kind of traffic and had two copies of this
// string. They had already drifted apart, so one function is what keeps them
// honest.
func TestWorkloadCSP_IsTheSameForAppsAndFunctions(t *testing.T) {
	app := &AppReconciler{}
	fn := &FunctionReconciler{}

	for _, allowlist := range [][]string{nil, {"cdn.example.com"}} {
		require.Equal(t, app.buildCSP(allowlist), fn.buildFunctionCSP(allowlist),
			"an app and a function with the same allowlist must get the same policy")
		require.Equal(t, buildWorkloadCSP(allowlist), app.buildCSP(allowlist))
	}
}

// The helper tests above prove the string. This drives the reconciler that
// production actually runs, so the assertion covers the wiring: the policy that
// reaches the Traefik Middleware, and the CR slice that must come back
// untouched. Asserting only on the builder would stay green if the mutation
// moved into the caller.
func TestReconcileSecurityMiddleware_EmitsThePolicyAndLeavesTheCRAlone(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := routedApp()
	app.Spec.Route.CSPAllowlist = []string{"cdn.example.com", "fonts.example.com"}
	original := append([]string(nil), app.Spec.Route.CSPAllowlist...)

	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}

	require.NoError(t, r.reconcileSecurityMiddleware(ctx, app))

	mw := &unstructured.Unstructured{}
	mw.SetGroupVersionKind(schema.GroupVersionKind{Group: "traefik.io", Version: "v1alpha1", Kind: "Middleware"})
	require.NoError(t, c.Get(ctx, crclient.ObjectKey{Name: app.Name + "-security", Namespace: app.Namespace}, mw))

	headers, found, err := unstructured.NestedMap(mw.Object, "spec", "headers")
	require.NoError(t, err)
	require.True(t, found, "the security middleware must carry headers")

	assert.Equal(t,
		"default-src 'self'; script-src 'self' 'unsafe-inline' 'unsafe-eval' cdn.example.com fonts.example.com; "+
			"style-src 'self' 'unsafe-inline' cdn.example.com fonts.example.com; img-src 'self' data: https:; "+
			"font-src 'self' data: https: cdn.example.com fonts.example.com; connect-src 'self' wss: https:;",
		headers["contentSecurityPolicy"],
		"the deployed header must not change for an app that already has one")

	assert.Equal(t, original, app.Spec.Route.CSPAllowlist,
		"reconciling must not rewrite the allowlist on the live CR")
}
