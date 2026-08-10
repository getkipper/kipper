package controllers

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/domain"
)

func appWithRoute(name, namespace, host string) *kipperv1.App {
	return &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       kipperv1.AppSpec{Image: "nginx:1.25", Port: 80, Route: &kipperv1.AppRoute{Host: host, Path: "/"}},
	}
}

func routeClaim(host, owner string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      routeClaimName(host),
			Namespace: routeClaimNamespace,
			Labels:    map[string]string{routeClaimLabel: "true", routeOwnerNamespaceLabel: owner},
		},
		Data: map[string]string{"host": canonicalRouteHost(host), "owner": owner},
	}
}

func namespaceObj(name string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func TestReserveHost_OwnedByFirstNamespaceRefusedForOthers(t *testing.T) {
	scheme := testScheme()
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(namespaceObj("project-a")).Build()
	ctx := context.Background()

	ownedA, err := reserveHost(ctx, c, c, "project-a", "billing.example.com")
	require.NoError(t, err)
	assert.True(t, ownedA, "the first namespace to claim a host owns it")

	ownedB, err := reserveHost(ctx, c, c, "project-b", "billing.example.com")
	require.NoError(t, err)
	assert.False(t, ownedB, "another project must not claim a host A owns while A exists")

	// Same namespace re-reserving keeps ownership (path-based routing shares it).
	ownedAgain, err := reserveHost(ctx, c, c, "project-a", "billing.example.com")
	require.NoError(t, err)
	assert.True(t, ownedAgain, "the owning namespace shares the host across its workloads")
}

func TestReserveHost_CaseAndDotVariantsShareClaim(t *testing.T) {
	scheme := testScheme()
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(namespaceObj("project-a")).Build()
	ctx := context.Background()

	ownedA, err := reserveHost(ctx, c, c, "project-a", "Billing.Example.com")
	require.NoError(t, err)
	require.True(t, ownedA)

	ownedB, err := reserveHost(ctx, c, c, "project-b", "billing.example.com.")
	require.NoError(t, err)
	assert.False(t, ownedB, "case and trailing-dot variants collide with the canonical claim")
}

// A claim is sticky: deleting the only workload that routes to a host does not
// free it, so another project cannot take it while the owning project exists.
func TestReserveHost_ClaimIsStickyWhileOwnerExists(t *testing.T) {
	scheme := testScheme()
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(namespaceObj("project-a")).Build()
	ctx := context.Background()

	ownedA, err := reserveHost(ctx, c, c, "project-a", "keep.example.com")
	require.NoError(t, err)
	require.True(t, ownedA)

	// Project A's workload is gone (no release runs on workload deletion), but the
	// claim and A's namespace remain, so B is refused.
	ownedB, err := reserveHost(ctx, c, c, "project-b", "keep.example.com")
	require.NoError(t, err)
	assert.False(t, ownedB, "a host stays claimed by its live project after the workload is deleted")
}

// A claim outlives its project (claims are never explicitly released). The next
// project to want the host takes it over once the owner namespace is gone.
func TestReserveHost_TakesOverClaimFromDeletedProject(t *testing.T) {
	scheme := testScheme()
	// A leftover claim owned by "gone", whose namespace no longer exists.
	stale := routeClaim("shop.example.com", "gone")
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(stale).Build()
	ctx := context.Background()

	owned, err := reserveHost(ctx, c, c, "newcomer", "shop.example.com")
	require.NoError(t, err)
	assert.True(t, owned, "a claim whose owner project is gone is taken over")

	var claim corev1.ConfigMap
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: routeClaimName("shop.example.com"), Namespace: routeClaimNamespace}, &claim))
	assert.Equal(t, "newcomer", claim.Data["owner"], "the claim now records the new owner")
	assert.Equal(t, "newcomer", claim.Labels[routeOwnerNamespaceLabel])
}

// When two projects race to take over the same stale claim, the optimistic
// Update serializes them: the loser gets a Conflict and does not own the host,
// so no silent dual ownership is possible.
func TestReserveHost_TakeoverLosesOnConflict(t *testing.T) {
	scheme := testScheme()
	stale := routeClaim("shop.example.com", "gone")
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(stale).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, cl crclient.WithWatch, obj crclient.Object, opts ...crclient.UpdateOption) error {
				if cm, ok := obj.(*corev1.ConfigMap); ok && cm.Namespace == routeClaimNamespace {
					return errors.NewConflict(schema.GroupResource{Resource: "configmaps"}, cm.Name, fmt.Errorf("stale write"))
				}
				return cl.Update(ctx, obj, opts...)
			},
		}).Build()
	ctx := context.Background()

	owned, err := reserveHost(ctx, c, c, "newcomer", "shop.example.com")
	require.NoError(t, err, "losing the takeover race is not an error, it re-evaluates next reconcile")
	assert.False(t, owned, "the namespace that loses the optimistic update does not own the host")

	var claim corev1.ConfigMap
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: routeClaimName("shop.example.com"), Namespace: routeClaimNamespace}, &claim))
	assert.Equal(t, "gone", claim.Data["owner"], "a lost takeover leaves the claim unchanged")
}

// A terminating owner namespace still exists, so its host is not taken over
// until it is fully gone — that is what prevents a dual-serve during teardown.
func TestReserveHost_DoesNotTakeOverWhileOwnerTerminating(t *testing.T) {
	scheme := testScheme()
	terminating := namespaceObj("leaving")
	now := metav1.Now()
	terminating.DeletionTimestamp = &now
	terminating.Finalizers = []string{"kubernetes"}
	claim := routeClaim("shop.example.com", "leaving")
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(terminating, claim).Build()
	ctx := context.Background()

	owned, err := reserveHost(ctx, c, c, "newcomer", "shop.example.com")
	require.NoError(t, err)
	assert.False(t, owned, "a host is not taken over while its owner namespace is still terminating")
}

func TestHostReservedForPlatform_ConventionalSubdomains(t *testing.T) {
	scheme := testScheme()
	c := crfake.NewClientBuilder().WithScheme(scheme).Build()
	ctx := context.Background()
	const clusterDomain = "example.com"

	for _, h := range []string{"console.example.com", "console-api.example.com", "dex.example.com", "grafana.example.com", "example.com", "CONSOLE.example.com"} {
		reserved, err := hostReservedForPlatform(ctx, c, h, clusterDomain)
		require.NoError(t, err)
		assert.True(t, reserved, "%s must be reserved", h)
	}
	tenant, err := hostReservedForPlatform(ctx, c, "myapp.example.com", clusterDomain)
	require.NoError(t, err)
	assert.False(t, tenant, "a tenant host is allowed")

	none, err := hostReservedForPlatform(ctx, c, "console.example.com", "")
	require.NoError(t, err)
	assert.False(t, none, "with no cluster domain and no declaration or Ingress, nothing is reserved")
}

// A CONSOLE_DOMAIN/DEX_DOMAIN override or a domain-transition host does not match
// the conventional subdomains, so the guard must reject it by matching the live
// platform Ingress that actually serves it — one in kipper-system and one in the
// dex namespace.
func TestHostReservedForPlatform_LivePlatformIngressHosts(t *testing.T) {
	scheme := testScheme()
	consoleIngress := platformIngress("console", "kipper-system", "portal.corp.example")
	dexIngress := platformIngress("dex", "dex", "login.corp.example")
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(consoleIngress, dexIngress).Build()
	ctx := context.Background()

	for _, h := range []string{"portal.corp.example", "PORTAL.corp.example.", "login.corp.example"} {
		reserved, err := hostReservedForPlatform(ctx, c, h, "example.com")
		require.NoError(t, err)
		assert.True(t, reserved, "%s is served by a platform Ingress and must be reserved", h)
	}

	tenant, err := hostReservedForPlatform(ctx, c, "shop.corp.example", "example.com")
	require.NoError(t, err)
	assert.False(t, tenant, "a host no platform Ingress serves is claimable by a tenant")
}

// The ClusterIdentity declares override and transition hosts before any Ingress
// for them exists. Those hosts must be reserved from the declaration, so a
// tenant cannot claim a host the platform is about to serve.
func TestHostReservedForPlatform_ClusterIdentityDeclaredHosts(t *testing.T) {
	scheme := testScheme()
	ci := &kipperv1.ClusterIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: ClusterIdentityName},
		Spec: kipperv1.ClusterIdentitySpec{
			Domain: "example.com",
			Hosts:  &kipperv1.IdentityHosts{Console: "portal.corp.example"},
		},
		Status: kipperv1.ClusterIdentityStatus{
			ActiveHosts: &kipperv1.ResolvedHosts{ConsoleAPI: "api.active.example"},
			Transition: &kipperv1.TransitionStatus{
				Phase:        "CuttingOver",
				FromIdentity: &kipperv1.SteadyIdentity{Domain: "old.example.net"},
				From:         &kipperv1.ResolvedHosts{Dex: "login.old.example.net"},
				ToIdentity:   &kipperv1.SteadyIdentity{Domain: "new.example.io"},
				To:           &kipperv1.ResolvedHosts{Dex: "login.new.example.io"},
			},
		},
	}
	// No platform Ingress exists for any of these yet.
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(ci).Build()
	ctx := context.Background()

	cases := map[string]string{
		"a declared console override":    "portal.corp.example",
		"an active console-api host":     "api.active.example",
		"a transition source dex host":   "login.old.example.net",
		"a subdomain of the from-domain": "console.old.example.net",
		"a transition target dex host":   "login.new.example.io",
		"a subdomain of the to-domain":   "console.new.example.io",
	}
	for name, host := range cases {
		reserved, err := hostReservedForPlatform(ctx, c, host, "example.com")
		require.NoError(t, err)
		assert.True(t, reserved, "%s must be reserved even before its Ingress exists", name)
	}

	tenant, err := hostReservedForPlatform(ctx, c, "shop.corp.example", "example.com")
	require.NoError(t, err)
	assert.False(t, tenant, "an undeclared host stays claimable by a tenant")
}

func platformIngress(name, namespace, host string) *networkingv1.Ingress {
	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{{Host: host}},
		},
	}
}

// An App with no explicit route host derives its host from the namespace
// environment label; the derived host is reserved and stays claimed.
func TestAppReconcile_DerivedHostReservedAndKept(t *testing.T) {
	scheme := testScheme()
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "project-a", Labels: map[string]string{"kipper.run/environment": "prod"}},
	}
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "project-a"},
		Spec:       kipperv1.AppSpec{Image: "nginx:1.25", Port: 80, Route: &kipperv1.AppRoute{Path: "/"}},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(ns, app).WithStatusSubresource(app).Build()
	r := &AppReconciler{Client: c, Scheme: scheme, Domain: "example.com"}
	ctx := context.Background()

	require.NoError(t, r.reconcileIngress(ctx, app))

	wantHost := domain.SubdomainFor("web-prod", "example.com")
	var ing networkingv1.Ingress
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "web", Namespace: "project-a"}, &ing))
	require.NotEmpty(t, ing.Spec.Rules)
	assert.Equal(t, wantHost, ing.Spec.Rules[0].Host, "the derived per-environment host is served")

	var claims corev1.ConfigMapList
	require.NoError(t, c.List(ctx, &claims))
	require.Len(t, claims.Items, 1, "the derived host is reserved")
	assert.Equal(t, canonicalRouteHost(wantHost), claims.Items[0].Data["host"])
}

// A transient Namespace read failure while deriving an app's implicit host must
// surface, not silently derive and reserve the wrong (unqualified) host.
func TestAppReconcile_PropagatesNamespaceReadError(t *testing.T) {
	scheme := testScheme()
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "project-a"},
		Spec:       kipperv1.AppSpec{Image: "nginx:1.25", Port: 80, Route: &kipperv1.AppRoute{Path: "/"}},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app).WithStatusSubresource(app).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl crclient.WithWatch, key crclient.ObjectKey, obj crclient.Object, opts ...crclient.GetOption) error {
				if _, isNamespace := obj.(*corev1.Namespace); isNamespace {
					return context.DeadlineExceeded
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	r := &AppReconciler{Client: c, Scheme: scheme, Domain: "example.com"}

	err := r.reconcileIngress(context.Background(), app)
	require.Error(t, err, "a namespace read failure must surface, not derive a wrong host")

	var claims corev1.ConfigMapList
	require.NoError(t, c.List(context.Background(), &claims))
	assert.Empty(t, claims.Items, "no host is reserved when the environment could not be read")
}

func TestAppReconcile_RefusesHijackOfHostOwnedByAnotherProject(t *testing.T) {
	scheme := testScheme()
	claim := routeClaim("billing.example.com", "project-a")
	phish := appWithRoute("phish", "project-b", "billing.example.com")
	// A more specific path must not let B intercept a subset of A's traffic.
	phish.Spec.Route.Path = "/api"

	// project-a (the owner) is live, so its host cannot be taken over.
	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(phish, claim, namespaceObj("project-a"), namespaceObj("project-b")).
		WithStatusSubresource(phish).Build()
	r := &AppReconciler{Client: c, Scheme: scheme, Domain: "example.com"}

	require.NoError(t, r.reconcileIngress(context.Background(), phish))

	var ing networkingv1.Ingress
	err := c.Get(context.Background(), types.NamespacedName{Name: "phish", Namespace: "project-b"}, &ing)
	assert.True(t, errors.IsNotFound(err), "a hijack of another project's host must not create an Ingress, even on a different path")
}

func TestFunctionReconcile_RefusalDoesNotDeleteAnotherProjectsIngress(t *testing.T) {
	// Two functions named "api" in different projects share the keda Ingress
	// object fn-api. When project B is refused, project A's Ingress (labelled
	// with A's namespace) must survive.
	scheme := testScheme()

	victimIngress := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "fn-api",
			Namespace: "keda",
			Labels:    map[string]string{fnNamespaceLabel: "project-a"},
		},
	}
	r := &FunctionReconciler{Client: crfake.NewClientBuilder().WithScheme(scheme).WithObjects(victimIngress).Build(), Scheme: scheme}

	// Project B's function is refused; deleting its ingress must not touch A's.
	require.NoError(t, r.deleteFunctionIngress(context.Background(), "api", "project-b"))

	var ing networkingv1.Ingress
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "fn-api", Namespace: "keda"}, &ing),
		"a refused function must not delete a same-named function's Ingress in another project")
}

// Editing an HTTP function to cron-only stops it serving a route: its Ingress
// and HTTPScaledObject are torn down. The host claim stays (sticky), so a later
// edit back to HTTP reclaims the same host.
func TestFunctionCleanupHTTPServing_RemovesIngressKeepsClaim(t *testing.T) {
	scheme := testScheme()
	fnIngress := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "fn-report",
			Namespace: "keda",
			Labels:    map[string]string{fnNamespaceLabel: "project-a"},
		},
	}
	cronFn := &kipperv1.Function{
		ObjectMeta: metav1.ObjectMeta{Name: "report", Namespace: "project-a"},
		Spec:       kipperv1.FunctionSpec{Triggers: []kipperv1.FunctionTrigger{{Type: "cron", Schedule: "0 * * * *"}}},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(fnIngress, cronFn).Build()
	ctx := context.Background()

	_, err := reserveHost(ctx, c, c, "project-a", domain.SubdomainFor("fn-report", "example.com"))
	require.NoError(t, err)

	r := &FunctionReconciler{Client: c, Scheme: scheme, Domain: "example.com"}
	require.NoError(t, r.cleanupHTTPServing(ctx, cronFn))

	var ing networkingv1.Ingress
	assert.True(t, errors.IsNotFound(c.Get(ctx, types.NamespacedName{Name: "fn-report", Namespace: "keda"}, &ing)),
		"the HTTP-to-cron transition deletes the function's Ingress")

	var claims corev1.ConfigMapList
	require.NoError(t, c.List(ctx, &claims))
	require.Len(t, claims.Items, 1, "the host claim stays held by the project (sticky)")
}
