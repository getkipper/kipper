package controllers

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func TestResolveRedirectFromHosts_ValidationAndClaims(t *testing.T) {
	scheme := testScheme()
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(
		namespaceObj("other-project"),
		routeClaim("taken.example.com", "other-project"),
	).Build()
	ctx := context.Background()

	accepted, refused, err := resolveRedirectFromHosts(ctx, c, c, "project-test", "my-app", "test.example.io", "example.com", []string{
		"WWW.Example.com.",  // canonicalised to www.example.com
		"www.example.com",   // duplicate after canonicalisation, dropped silently
		"example.com",       // the canonical host itself, dropped silently
		"not a hostname",    // malformed
		"*.example.com",     // wildcard
		"promo.kipper.run",  // gateway domain
		"taken.example.com", // owned by another live project
		"old.example.net",
	})
	require.NoError(t, err)

	assert.Equal(t, []string{"www.example.com", "old.example.net"}, accepted)

	require.Len(t, refused, 4)
	reasons := map[string]string{}
	for _, ref := range refused {
		reasons[ref.host] = ref.reason
	}
	assert.Contains(t, reasons["not a hostname"], "not a valid hostname")
	assert.Contains(t, reasons["*.example.com"], "not a valid hostname")
	assert.Contains(t, reasons["promo.kipper.run"], "kipper.run")
	assert.Contains(t, reasons["taken.example.com"], "claimed by another project")

	// Accepted hosts hold a claim for the calling namespace.
	var claim corev1.ConfigMap
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: routeClaimName("www.example.com"), Namespace: routeClaimNamespace}, &claim))
	assert.Equal(t, "project-test", claim.Data["owner"])
}

func TestResolveRedirectFromHosts_RefusesOverlongHostname(t *testing.T) {
	scheme := testScheme()
	c := crfake.NewClientBuilder().WithScheme(scheme).Build()

	// Every label is valid on its own; only the combined length breaks the
	// API server's 253-character limit for an Ingress rule host.
	label := strings.Repeat("a", 63)
	overlong := strings.Join([]string{label, label, label, label}, ".") + ".example.com"
	require.Greater(t, len(overlong), 253)

	accepted, refused, err := resolveRedirectFromHosts(context.Background(), c, c, "project-test", "my-app", "test.example.io", "example.com", []string{overlong})
	require.NoError(t, err)
	assert.Empty(t, accepted)
	require.Len(t, refused, 1)
	assert.Contains(t, refused[0].reason, "not a valid hostname")
}

func TestResolveRedirectFromHosts_RefusesSiblingHosts(t *testing.T) {
	scheme := testScheme()

	sibling := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: "project-test"},
		Spec: kipperv1.AppSpec{
			Image: "shop:1", Port: 80,
			Route: &kipperv1.AppRoute{Host: "api.example.com", RedirectFrom: []string{"old.example.com"}},
		},
	}
	fnIngress := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout-fn", Namespace: "project-test"},
		Spec:       networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{Host: "fn.example.com"}}},
	}
	// The app's own Ingress from the previous reconcile must not veto its
	// own alias on the next one.
	ownIngress := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "my-app", Namespace: "project-test"},
		Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{
			{Host: "example.com"}, {Host: "www.example.com"},
		}},
	}

	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(sibling, fnIngress, ownIngress).Build()

	accepted, refused, err := resolveRedirectFromHosts(context.Background(), c, c, "project-test", "my-app", "test.example.io", "example.com", []string{
		"api.example.com", // the sibling app's canonical host
		"old.example.com", // the sibling app's redirect alias
		"fn.example.com",  // served by an Ingress the app does not own
		"www.example.com", // the app's own alias, still free
	})
	require.NoError(t, err)

	assert.Equal(t, []string{"www.example.com"}, accepted)
	require.Len(t, refused, 3)
	for _, ref := range refused {
		assert.Contains(t, ref.reason, "another app in this project",
			"host %q must be refused as a sibling conflict", ref.host)
	}
}

func TestResolveRedirectFromHosts_RefusesSiblingDerivedHost(t *testing.T) {
	scheme := testScheme()

	// The sibling's route has no explicit host, so it serves the derived
	// <app>.<clusterDomain> hostname — and its Ingress does not exist yet,
	// so only spec-level resolution can catch the conflict.
	sibling := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: "project-test"},
		Spec: kipperv1.AppSpec{
			Image: "shop:1", Port: 80,
			Route: &kipperv1.AppRoute{},
		},
	}

	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(sibling).Build()

	accepted, refused, err := resolveRedirectFromHosts(context.Background(), c, c, "project-test", "my-app", "test.example.io", "example.com", []string{
		"shop.test.example.io",
	})
	require.NoError(t, err)

	assert.Empty(t, accepted)
	require.Len(t, refused, 1)
	assert.Contains(t, refused[0].reason, "another app in this project")
}

func TestReconcileIngress_RedirectFromServesAliasHost(t *testing.T) {
	scheme := testScheme()

	app := newTestApp()
	app.Spec.Route = &kipperv1.AppRoute{
		Host:         "example.com",
		RedirectFrom: []string{"www.example.com"},
	}

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(app).
		WithStatusSubresource(app).
		Build()

	reconciler := &AppReconciler{Client: fakeClient, Scheme: scheme, Domain: "test.kipper.run"}
	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-app", Namespace: "project-test"},
	})
	require.NoError(t, err)

	var ingress networkingv1.Ingress
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &ingress))

	require.Len(t, ingress.Spec.Rules, 2)
	assert.Equal(t, "example.com", ingress.Spec.Rules[0].Host)
	assert.Equal(t, "www.example.com", ingress.Spec.Rules[1].Host)
	assert.Equal(t, "/", ingress.Spec.Rules[1].HTTP.Paths[0].Path)
	assert.Equal(t, "my-app", ingress.Spec.Rules[1].HTTP.Paths[0].Backend.Service.Name)

	require.Len(t, ingress.Spec.TLS, 2)
	assert.Equal(t, []string{"example.com"}, ingress.Spec.TLS[0].Hosts)
	assert.Equal(t, "my-app-tls", ingress.Spec.TLS[0].SecretName)
	assert.Equal(t, []string{"www.example.com"}, ingress.Spec.TLS[1].Hosts)
	assert.Equal(t, aliasTLSSecretName("my-app", "www.example.com"), ingress.Spec.TLS[1].SecretName,
		"each alias gets its own cert so a missing DNS record cannot block the canonical host's issuance")
	assert.NotEqual(t, ingress.Spec.TLS[0].SecretName, ingress.Spec.TLS[1].SecretName)

	mwAnn := ingress.Annotations["traefik.ingress.kubernetes.io/router.middlewares"]
	assert.Contains(t, mwAnn, "project-test-my-app-redirect-from-0@kubernetescrd")
	assert.Greater(t, strings.Index(mwAnn, "redirect-from-0"), strings.Index(mwAnn, "rate-limit"),
		"the redirect must stay behind the rate limit so alias traffic is metered")

	mw := getMiddleware(t, fakeClient, "my-app-redirect-from-0")
	regex, _, err := unstructured.NestedString(mw.Object, "spec", "redirectRegex", "regex")
	require.NoError(t, err)
	assert.Equal(t, `^https?://www\.example\.com/(.*)`, regex)
	replacement, _, err := unstructured.NestedString(mw.Object, "spec", "redirectRegex", "replacement")
	require.NoError(t, err)
	assert.Equal(t, "https://example.com/${1}", replacement)
	permanent, _, err := unstructured.NestedBool(mw.Object, "spec", "redirectRegex", "permanent")
	require.NoError(t, err)
	assert.True(t, permanent)

	var claim corev1.ConfigMap
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: routeClaimName("www.example.com"), Namespace: routeClaimNamespace}, &claim))
	assert.Equal(t, "project-test", claim.Data["owner"])
}

func TestReconcileIngress_RefusedRedirectFromDegradesRoute(t *testing.T) {
	scheme := testScheme()

	app := newTestApp()
	app.Spec.Route = &kipperv1.AppRoute{
		Host:         "example.com",
		RedirectFrom: []string{"www.example.com", "promo.kipper.run"},
	}

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(app, namespaceObj("other-project"), routeClaim("www.example.com", "other-project")).
		WithStatusSubresource(app).
		Build()

	reconciler := &AppReconciler{Client: fakeClient, Scheme: scheme, Domain: "test.kipper.run"}
	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-app", Namespace: "project-test"},
	})
	require.NoError(t, err)

	// The canonical host keeps serving; the refused aliases get no rule.
	var ingress networkingv1.Ingress
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &ingress))
	require.Len(t, ingress.Spec.Rules, 1)
	assert.Equal(t, "example.com", ingress.Spec.Rules[0].Host)
	assert.NotContains(t, ingress.Annotations["traefik.ingress.kubernetes.io/router.middlewares"], "redirect-from")

	mw := middlewareStub("my-app-redirect-from-0")
	err = fakeClient.Get(context.Background(), types.NamespacedName{Name: "my-app-redirect-from-0", Namespace: "project-test"}, mw)
	assert.True(t, errors.IsNotFound(err), "a refused alias must not get a redirect middleware")

	var updated kipperv1.App
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &updated))
	cond := apimeta.FindStatusCondition(updated.Status.Conditions, kipperv1.ConditionRouteReady)
	require.NotNil(t, cond, "refused aliases must be surfaced on the route condition")
	assert.Equal(t, "RedirectHostUnavailable", cond.Reason)
	assert.Contains(t, cond.Message, "www.example.com")
	assert.Contains(t, cond.Message, "promo.kipper.run")
}

func TestReconcileIngress_RedirectFromWithGatewayPrimaryHost(t *testing.T) {
	scheme := testScheme()

	app := newTestApp()
	// No explicit host: the primary host derives to a *.kipper.run
	// subdomain fronted by the gateway.
	app.Spec.Route = &kipperv1.AppRoute{RedirectFrom: []string{"example.org"}}

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(app).
		WithStatusSubresource(app).
		Build()

	reconciler := &AppReconciler{Client: fakeClient, Scheme: scheme, Domain: "test.kipper.run"}
	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-app", Namespace: "project-test"},
	})
	require.NoError(t, err)

	var ingress networkingv1.Ingress
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &ingress))

	assert.Equal(t, "letsencrypt-prod", ingress.Annotations["cert-manager.io/cluster-issuer"],
		"the alias cert needs cert-manager even though the primary host is gateway-fronted")

	require.Len(t, ingress.Spec.TLS, 2)
	assert.Empty(t, ingress.Spec.TLS[0].SecretName, "no cert is requested for the gateway-fronted host")
	assert.Equal(t, aliasTLSSecretName("my-app", "example.org"), ingress.Spec.TLS[1].SecretName)

	require.Len(t, ingress.Spec.Rules, 2)
	assert.Equal(t, "example.org", ingress.Spec.Rules[1].Host)
}

func TestReconcileIngress_RedirectFromRemovalCleansUp(t *testing.T) {
	scheme := testScheme()

	app := newTestApp()
	app.Spec.Route = &kipperv1.AppRoute{
		Host:         "example.com",
		RedirectFrom: []string{"www.example.com"},
	}

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(app).
		WithStatusSubresource(app).
		Build()

	reconciler := &AppReconciler{Client: fakeClient, Scheme: scheme, Domain: "test.kipper.run"}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "my-app", Namespace: "project-test"}}

	_, err := reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)
	getMiddleware(t, fakeClient, "my-app-redirect-from-0")

	var current kipperv1.App
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &current))
	current.Spec.Route.RedirectFrom = nil
	require.NoError(t, fakeClient.Update(context.Background(), &current))

	_, err = reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)

	mw := middlewareStub("my-app-redirect-from-0")
	err = fakeClient.Get(context.Background(), types.NamespacedName{Name: "my-app-redirect-from-0", Namespace: "project-test"}, mw)
	assert.True(t, errors.IsNotFound(err), "removing the alias must delete its middleware")

	var ingress networkingv1.Ingress
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &ingress))
	require.Len(t, ingress.Spec.Rules, 1)
	require.Len(t, ingress.Spec.TLS, 1)
	assert.NotContains(t, ingress.Annotations["traefik.ingress.kubernetes.io/router.middlewares"], "redirect-from")
}
