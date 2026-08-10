package controllers

import (
	"context"
	"fmt"
	"testing"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	kipperlabels "github.com/getkipper/kipper/controller/pkg/labels"
)

func linkTestApp(name, ns string, port int32, links ...kipperv1.AppLink) *kipperv1.App {
	return &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       kipperv1.AppSpec{Image: "example:1", Port: port, Links: links},
	}
}

// projectNS is a namespace as the project reconciler labels it, which is how
// both ends of a link are resolved back to their projects.
func projectNS(name, project string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   name,
		Labels: map[string]string{kipperlabels.Project: project},
	}}
}

// consentingProject is a target project that has agreed to be linked to.
func consentingProject(name string, from ...string) *kipperv1.Project {
	return &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       kipperv1.ProjectSpec{AllowLinksFrom: from},
	}
}

// linkWorld is the objects a consented cross-project link needs on both sides.
func linkWorld() []crclient.Object {
	return []crclient.Object{
		projectNS("hrportal-test", "hrportal"),
		projectNS("docuseal-test", "docuseal"),
		projectNS("billing-test", "billing"),
		projectNS("elsewhere-test", "elsewhere"),
		consentingProject("docuseal", "hrportal"),
		consentingProject("billing", "hrportal"),
		consentingProject("elsewhere", "hrportal"),
	}
}

func withWorld(objs ...crclient.Object) []crclient.Object {
	return append(linkWorld(), objs...)
}

func linkPolicy(t *testing.T, r *AppReconciler, app *kipperv1.App) (*networkingv1.NetworkPolicy, bool) {
	t.Helper()
	var np networkingv1.NetworkPolicy
	err := r.Get(context.Background(), types.NamespacedName{
		Name: linkPolicyName(app.Name), Namespace: app.Namespace,
	}, &np)
	if errors.IsNotFound(err) {
		return nil, false
	}
	require.NoError(t, err)
	return &np, true
}

// Without an allowance there is no path between two projects at all: the
// workload policy excepts the RFC1918 ranges, which covers both the service and
// the pod address, and the node addresses, which covers the public route. The
// link is what opens one, and it must open no more than it names.
func TestACrossProjectLinkOpensExactlyItsTarget(t *testing.T) {
	scheme := testScheme()
	caller := linkTestApp("hrportal-backend", "hrportal-test", 8080,
		kipperv1.AppLink{App: "docuseal", Namespace: "docuseal-test"})
	target := linkTestApp("docuseal", "docuseal-test", 3000)

	r := &AppReconciler{Client: crfake.NewClientBuilder().WithScheme(scheme).WithObjects(withWorld(caller, target)...).Build(), Scheme: scheme}
	_, err := r.reconcileLinkPolicy(context.Background(), caller)
	require.NoError(t, err)

	np, found := linkPolicy(t, r, caller)
	require.True(t, found, "a cross-project link must open egress")

	assert.Equal(t, map[string]string{"app": "hrportal-backend"}, np.Spec.PodSelector.MatchLabels,
		"the allowance must apply to the calling app's pods only")
	assert.Equal(t, []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}, np.Spec.PolicyTypes,
		"a link opens egress; the target's ingress is not this policy's business")

	require.Len(t, np.Spec.Egress, 1)
	require.Len(t, np.Spec.Egress[0].To, 1)
	peer := np.Spec.Egress[0].To[0]
	assert.Equal(t, map[string]string{"kubernetes.io/metadata.name": "docuseal-test"}, peer.NamespaceSelector.MatchLabels)
	assert.Equal(t, map[string]string{"app": "docuseal"}, peer.PodSelector.MatchLabels,
		"the peer must name the target app, not its whole namespace")

	require.Len(t, np.Spec.Egress[0].Ports, 1)
	assert.Equal(t, int32(3000), np.Spec.Egress[0].Ports[0].Port.IntVal,
		"the allowance must be scoped to the port the target actually serves")

	require.Len(t, np.OwnerReferences, 1, "the policy must be owned by the app so it goes when the app does")
	assert.Equal(t, "hrportal-backend", np.OwnerReferences[0].Name)
}

// The port is read from the target rather than recorded on the link, so a target
// that moves stays reachable and a link cannot hold open a port the target has
// stopped serving.
func TestTheAllowanceFollowsTheTargetsPort(t *testing.T) {
	scheme := testScheme()
	caller := linkTestApp("hrportal-backend", "hrportal-test", 8080,
		kipperv1.AppLink{App: "docuseal", Namespace: "docuseal-test"})
	target := linkTestApp("docuseal", "docuseal-test", 3000)

	r := &AppReconciler{Client: crfake.NewClientBuilder().WithScheme(scheme).WithObjects(withWorld(caller, target)...).Build(), Scheme: scheme}
	_, err := r.reconcileLinkPolicy(context.Background(), caller)
	require.NoError(t, err)

	target.Spec.Port = 4000
	require.NoError(t, r.Update(context.Background(), target))
	_, err = r.reconcileLinkPolicy(context.Background(), caller)
	require.NoError(t, err)

	np, found := linkPolicy(t, r, caller)
	require.True(t, found)
	assert.Equal(t, int32(4000), np.Spec.Egress[0].Ports[0].Port.IntVal,
		"the allowance still names the old port, so the link is broken while looking open")
}

// A peer here is a pod selector, so the rule only ever matches traffic already
// addressed to a pod. A target serving a public route carries the instance-id
// sidecar, whose Service sends the app's port to a pod listening 10000 above it,
// and an allowance naming the app's port matches nothing that arrives.
func TestTheAllowanceNamesThePortTheTargetsPodsListenOn(t *testing.T) {
	scheme := testScheme()
	caller := linkTestApp("hrportal-backend", "hrportal-test", 8080,
		kipperv1.AppLink{App: "docuseal", Namespace: "docuseal-test"})
	target := linkTestApp("docuseal", "docuseal-test", 3000)
	target.Spec.Route = &kipperv1.AppRoute{Host: "sign.example.com"}

	r := &AppReconciler{
		Client:       crfake.NewClientBuilder().WithScheme(scheme).WithObjects(withWorld(caller, target)...).Build(),
		Scheme:       scheme,
		SidecarImage: "ghcr.io/example/kipper-sidecar:1",
	}
	_, err := r.reconcileLinkPolicy(context.Background(), caller)
	require.NoError(t, err)

	np, found := linkPolicy(t, r, caller)
	require.True(t, found)
	require.Len(t, np.Spec.Egress[0].Ports, 1)
	assert.Equal(t, r.serviceTargetPort(target), np.Spec.Egress[0].Ports[0].Port.IntVal,
		"the allowance must name the port the target's Service routes to, which is where the packet lands")
	assert.Equal(t, int32(13000), np.Spec.Egress[0].Ports[0].Port.IntVal,
		"a routed target runs the sidecar, so its pods listen 10000 above the app's port")
}

// The offset belongs to the sidecar, not to links. A target nobody routes to
// serves on its own port, and the allowance has to name that.
func TestAnUnroutedTargetKeepsItsOwnPort(t *testing.T) {
	scheme := testScheme()
	caller := linkTestApp("hrportal-backend", "hrportal-test", 8080,
		kipperv1.AppLink{App: "docuseal", Namespace: "docuseal-test"})
	target := linkTestApp("docuseal", "docuseal-test", 3000)

	r := &AppReconciler{
		Client:       crfake.NewClientBuilder().WithScheme(scheme).WithObjects(withWorld(caller, target)...).Build(),
		Scheme:       scheme,
		SidecarImage: "ghcr.io/example/kipper-sidecar:1",
	}
	_, err := r.reconcileLinkPolicy(context.Background(), caller)
	require.NoError(t, err)

	np, found := linkPolicy(t, r, caller)
	require.True(t, found)
	require.Len(t, np.Spec.Egress[0].Ports, 1)
	assert.Equal(t, int32(3000), np.Spec.Egress[0].Ports[0].Port.IntVal,
		"an unrouted target gets no sidecar, so its pods listen on the app's own port")
}

// A link inside the app's own namespace needs nothing: the workload policy
// already allows the whole namespace. Writing a policy for it would be an
// allowance nobody needs and nobody would think to remove.
func TestASameProjectLinkOpensNothing(t *testing.T) {
	scheme := testScheme()
	caller := linkTestApp("api-gateway", "hrportal-test", 8080,
		kipperv1.AppLink{App: "domain-service", Namespace: "hrportal-test"})
	target := linkTestApp("domain-service", "hrportal-test", 9000)

	r := &AppReconciler{Client: crfake.NewClientBuilder().WithScheme(scheme).WithObjects(withWorld(caller, target)...).Build(), Scheme: scheme}
	_, err := r.reconcileLinkPolicy(context.Background(), caller)
	require.NoError(t, err)

	_, found := linkPolicy(t, r, caller)
	assert.False(t, found, "a same-namespace link must not create a policy")
}

// A link naming an app that is not there opens nothing. A standing allowance
// into another project for an absent target is one nobody can account for.
func TestALinkToAnAbsentTargetOpensNothing(t *testing.T) {
	scheme := testScheme()
	caller := linkTestApp("hrportal-backend", "hrportal-test", 8080,
		kipperv1.AppLink{App: "docuseal", Namespace: "docuseal-test"})

	r := &AppReconciler{Client: crfake.NewClientBuilder().WithScheme(scheme).WithObjects(withWorld(caller)...).Build(), Scheme: scheme}
	_, err := r.reconcileLinkPolicy(context.Background(), caller)
	require.NoError(t, err)

	_, found := linkPolicy(t, r, caller)
	assert.False(t, found, "no target, no allowance")
}

// Withdrawing the link withdraws the egress. Leaving the policy behind would
// keep a path open into another project after the reason for it was removed.
func TestRemovingTheLinkRemovesTheAllowance(t *testing.T) {
	scheme := testScheme()
	caller := linkTestApp("hrportal-backend", "hrportal-test", 8080,
		kipperv1.AppLink{App: "docuseal", Namespace: "docuseal-test"})
	target := linkTestApp("docuseal", "docuseal-test", 3000)

	r := &AppReconciler{Client: crfake.NewClientBuilder().WithScheme(scheme).WithObjects(withWorld(caller, target)...).Build(), Scheme: scheme}
	_, err := r.reconcileLinkPolicy(context.Background(), caller)
	require.NoError(t, err)
	_, found := linkPolicy(t, r, caller)
	require.True(t, found)

	caller.Spec.Links = nil
	_, err = r.reconcileLinkPolicy(context.Background(), caller)
	require.NoError(t, err)

	_, found = linkPolicy(t, r, caller)
	assert.False(t, found, "the allowance outlived the link that justified it")
}

// Several links produce several peers, each scoped to its own target. One link
// going stale must not take the others with it.
func TestEachLinkGetsItsOwnPeer(t *testing.T) {
	scheme := testScheme()
	caller := linkTestApp("hrportal-backend", "hrportal-test", 8080,
		kipperv1.AppLink{App: "docuseal", Namespace: "docuseal-test"},
		kipperv1.AppLink{App: "gone", Namespace: "elsewhere-test"},
		kipperv1.AppLink{App: "billing", Namespace: "billing-test"})
	docuseal := linkTestApp("docuseal", "docuseal-test", 3000)
	billing := linkTestApp("billing", "billing-test", 7000)

	r := &AppReconciler{Client: crfake.NewClientBuilder().WithScheme(scheme).WithObjects(withWorld(caller, docuseal, billing)...).Build(), Scheme: scheme}
	_, err := r.reconcileLinkPolicy(context.Background(), caller)
	require.NoError(t, err)

	np, found := linkPolicy(t, r, caller)
	require.True(t, found)
	require.Len(t, np.Spec.Egress, 2, "the absent target must be skipped and the others kept")

	ports := []int32{np.Spec.Egress[0].Ports[0].Port.IntVal, np.Spec.Egress[1].Ports[0].Port.IntVal}
	assert.ElementsMatch(t, []int32{3000, 7000}, ports)
}

// A target that changes or disappears has to reach the apps that depend on it.
// Without the index the caller is only reconciled by its own events, so an
// allowance keeps naming a port the target stopped serving — and anything that
// later takes that name and port inherits the access.
func TestAChangedTargetReachesItsCallers(t *testing.T) {
	scheme := testScheme()
	caller := linkTestApp("hrportal-backend", "hrportal-test", 8080,
		kipperv1.AppLink{App: "docuseal", Namespace: "docuseal-test"})
	other := linkTestApp("unrelated", "hrportal-test", 9090)
	target := linkTestApp("docuseal", "docuseal-test", 3000)

	client := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(withWorld(caller, other, target)...).
		WithIndex(&kipperv1.App{}, linkTargetIndex, LinkTargetKeys).Build()
	r := &AppReconciler{Client: client, Scheme: scheme}

	reqs := r.enqueueCallersOfLinkTarget(context.Background(), target)
	require.Len(t, reqs, 1, "the app that links to this target must be reconciled")
	assert.Equal(t, "hrportal-backend", reqs[0].Name)
	assert.Equal(t, "hrportal-test", reqs[0].Namespace)

	// An app nobody links to reconciles nobody.
	assert.Empty(t, r.enqueueCallersOfLinkTarget(context.Background(), other))
}

// The spec is writable directly, so a list repeating a target must render one
// rule rather than one per entry — otherwise a hand-written CR can inflate the
// policy until the API server or the network plugin refuses it, and a refused
// update leaves the previous allowances standing.
func TestRepeatedLinksRenderOnce(t *testing.T) {
	scheme := testScheme()
	links := make([]kipperv1.AppLink, 0, 50)
	for i := 0; i < 50; i++ {
		links = append(links, kipperv1.AppLink{App: "docuseal", Namespace: "docuseal-test"})
	}
	caller := linkTestApp("hrportal-backend", "hrportal-test", 8080, links...)
	target := linkTestApp("docuseal", "docuseal-test", 3000)

	r := &AppReconciler{Client: crfake.NewClientBuilder().WithScheme(scheme).WithObjects(withWorld(caller, target)...).Build(), Scheme: scheme}
	_, err := r.reconcileLinkPolicy(context.Background(), caller)
	require.NoError(t, err)

	np, found := linkPolicy(t, r, caller)
	require.True(t, found)
	assert.Len(t, np.Spec.Egress, 1, "fifty copies of one target must be one rule")
}

// The calling side declaring a link is not enough. A link opens a direct route
// to a backend, past the ingress and past every control attached to a public
// route, and an app's own project cannot grant access to somebody else's. The
// target project has to have agreed.
func TestALinkWithoutTheTargetProjectsConsentOpensNothing(t *testing.T) {
	scheme := testScheme()
	caller := linkTestApp("hrportal-backend", "hrportal-test", 8080,
		kipperv1.AppLink{App: "docuseal", Namespace: "docuseal-test"})
	target := linkTestApp("docuseal", "docuseal-test", 3000)

	tests := []struct {
		name    string
		project *kipperv1.Project
		opens   bool
	}{
		{name: "the target project allows this caller", project: consentingProject("docuseal", "hrportal"), opens: true},
		{name: "the target project allows a different one", project: consentingProject("docuseal", "someone-else"), opens: false},
		{name: "the target project allows nobody", project: consentingProject("docuseal"), opens: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := []crclient.Object{
				projectNS("hrportal-test", "hrportal"),
				projectNS("docuseal-test", "docuseal"),
				tt.project, caller, target,
			}
			r := &AppReconciler{Client: crfake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build(), Scheme: scheme}
			_, err := r.reconcileLinkPolicy(context.Background(), caller)
			require.NoError(t, err)

			_, found := linkPolicy(t, r, caller)
			assert.Equal(t, tt.opens, found)
		})
	}
}

// Consent is read from the namespaces' own labels, not from anything the caller
// wrote. A caller naming a namespace it has no relationship with cannot have
// that believed, and a namespace outside Kipper's projects is not a link target.
func TestConsentIsResolvedFromTheNamespacesNotTheLink(t *testing.T) {
	scheme := testScheme()
	caller := linkTestApp("hrportal-backend", "hrportal-test", 8080,
		kipperv1.AppLink{App: "secret-thing", Namespace: "some-other-ns"})
	target := linkTestApp("secret-thing", "some-other-ns", 9000)

	// The target namespace carries no project label, so it is not a project
	// namespace and no amount of declaring reaches it.
	objs := []crclient.Object{
		projectNS("hrportal-test", "hrportal"),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "some-other-ns"}},
		consentingProject("hrportal", "hrportal"),
		caller, target,
	}
	r := &AppReconciler{Client: crfake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build(), Scheme: scheme}
	_, err := r.reconcileLinkPolicy(context.Background(), caller)
	require.NoError(t, err)

	_, found := linkPolicy(t, r, caller)
	assert.False(t, found, "a namespace outside Kipper's projects is not a link target")
}

// Two environments of one project are both that project's, so there is nobody
// else to ask. Requiring a project to consent to itself would make a test
// environment unable to reach its own staging one.
func TestAnotherEnvironmentOfTheSameProjectNeedsNoConsent(t *testing.T) {
	scheme := testScheme()
	caller := linkTestApp("api", "hrportal-test", 8080,
		kipperv1.AppLink{App: "worker", Namespace: "hrportal-staging"})
	target := linkTestApp("worker", "hrportal-staging", 9000)

	objs := []crclient.Object{
		projectNS("hrportal-test", "hrportal"),
		projectNS("hrportal-staging", "hrportal"),
		consentingProject("hrportal"), // allows nobody, and needs to allow nobody
		caller, target,
	}
	r := &AppReconciler{Client: crfake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build(), Scheme: scheme}
	_, err := r.reconcileLinkPolicy(context.Background(), caller)
	require.NoError(t, err)

	np, found := linkPolicy(t, r, caller)
	require.True(t, found, "one project's own environments must reach each other")
	assert.Equal(t, map[string]string{"kubernetes.io/metadata.name": "hrportal-staging"},
		np.Spec.Egress[0].To[0].NamespaceSelector.MatchLabels)
}

// Withdrawing consent is a decision to close a path, and it has to close it.
// Without a Project watch the caller's policy is only rebuilt when the caller
// itself is reconciled, which for a stable deployment may be days — so a
// revoked grant would leave every path it authorised standing.
func TestAConsentChangeReachesTheCallersItAuthorised(t *testing.T) {
	scheme := testScheme()
	caller := linkTestApp("hrportal-backend", "hrportal-test", 8080,
		kipperv1.AppLink{App: "docuseal", Namespace: "docuseal-test"})
	unrelated := linkTestApp("other", "hrportal-test", 9090,
		kipperv1.AppLink{App: "billing", Namespace: "billing-test"})

	client := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(projectNS("docuseal-test", "docuseal"), projectNS("billing-test", "billing"),
			projectNS("hrportal-test", "hrportal"), caller, unrelated).
		WithIndex(&kipperv1.App{}, linkTargetNamespaceIndex, LinkTargetNamespaceKeys).
		Build()
	r := &AppReconciler{Client: client, Scheme: scheme}

	// docuseal's consent changed: only the app that links into it is affected.
	reqs := r.enqueueCallersOfProject(context.Background(), consentingProject("docuseal"))
	require.Len(t, reqs, 1, "the app linking into this project must be reconciled")
	assert.Equal(t, "hrportal-backend", reqs[0].Name)

	// A project nobody links into reconciles nobody.
	assert.Empty(t, r.enqueueCallersOfProject(context.Background(), consentingProject("nobody-links-here")))
}

// The extractors the manager registers are the ones a caller lookup depends on.
// A test that indexes its own copy proves the lookup and not the wiring.
func TestTheIndexExtractorsKeyOnWhatTheLookupsUse(t *testing.T) {
	app := &kipperv1.App{Spec: kipperv1.AppSpec{Links: []kipperv1.AppLink{
		{App: "docuseal", Namespace: "docuseal-test"},
		{App: "reports", Namespace: "docuseal-test"},
		{App: "", Namespace: "ignored"},
		{App: "nope", Namespace: ""},
	}}}

	assert.ElementsMatch(t, []string{"docuseal-test/docuseal", "docuseal-test/reports"}, LinkTargetKeys(app))
	assert.Equal(t, []string{"docuseal-test"}, LinkTargetNamespaceKeys(app),
		"one key per namespace, however many links point into it")
	assert.Nil(t, LinkTargetKeys(&corev1.Namespace{}), "a non-App object indexes nothing")
}

// The watches that notice a changed target or a withdrawn consent map events
// through the cache, and a map function cannot report a failure or ask to be
// retried. The periodic sweep is what stops a dropped event leaving a revoked
// consent's egress open until something unrelated happens to touch the app, so
// it is the mechanism that bounds the exposure and it has to be asserted.
func TestALinkedAppIsSweptPeriodically(t *testing.T) {
	scheme := testScheme()

	linked := linkTestApp("hrportal-backend", "hrportal-test", 8080,
		kipperv1.AppLink{App: "docuseal", Namespace: "docuseal-test"})
	unlinked := linkTestApp("plain", "hrportal-test", 8080)
	target := linkTestApp("docuseal", "docuseal-test", 3000)

	client := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(withWorld(linked, unlinked, target)...).
		WithStatusSubresource(linked, unlinked).
		Build()
	r := &AppReconciler{Client: client, Scheme: scheme}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "hrportal-backend", Namespace: "hrportal-test"},
	})
	require.NoError(t, err)
	assert.Equal(t, linkRefreshInterval, res.RequeueAfter,
		"an app holding links must be swept, or a dropped revocation event lasts forever")

	res, err = r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "plain", Namespace: "hrportal-test"},
	})
	require.NoError(t, err)
	assert.Zero(t, res.RequeueAfter,
		"an app with no links has no allowance to keep fresh and must not be swept")
}

// One namespace's lookup failing must not discard the callers already found for
// the others. Reconciling some callers of a withdrawn consent beats reconciling
// none, and the mapper has no way to ask for a retry.
func TestAPartialMappingFailureKeepsWhatItFound(t *testing.T) {
	scheme := testScheme()
	first := linkTestApp("first", "hrportal-test", 8080,
		kipperv1.AppLink{App: "a", Namespace: "docuseal-test"})
	second := linkTestApp("second", "hrportal-test", 8080,
		kipperv1.AppLink{App: "b", Namespace: "docuseal-prod"})

	// Two namespaces belong to the project whose consent changed; the lookup
	// for the second one fails.
	failing := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(
			projectNS("docuseal-test", "docuseal"),
			projectNS("docuseal-prod", "docuseal"),
			projectNS("hrportal-test", "hrportal"),
			first, second).
		WithIndex(&kipperv1.App{}, linkTargetNamespaceIndex, LinkTargetNamespaceKeys).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c crclient.WithWatch, list crclient.ObjectList, opts ...crclient.ListOption) error {
				for _, o := range opts {
					if m, ok := o.(crclient.MatchingFields); ok && m[linkTargetNamespaceIndex] == "docuseal-prod" {
						return fmt.Errorf("simulated cache failure")
					}
				}
				return c.List(ctx, list, opts...)
			},
		}).Build()
	r := &AppReconciler{Client: failing, Scheme: scheme}

	reqs := r.enqueueCallersOfProject(context.Background(), consentingProject("docuseal"))
	require.Len(t, reqs, 1, "the caller found before the failure must still be reconciled")
	assert.Equal(t, "first", reqs[0].Name)
}

// Revoking an allowance has to converge whether or not the workload does. The
// link policy is reconciled before the Deployment, the Service and the rest, so
// an app whose spec the cluster cannot roll still loses the egress its links no
// longer justify. Behind them it would not: the same pass would fail at the same
// place every time, and a consent taken back would stand for as long as the app
// stayed broken.
func TestRevokedEgressGoesEvenWhenTheWorkloadCannotReconcile(t *testing.T) {
	scheme := testScheme()
	caller := linkTestApp("hrportal-backend", "hrportal-test", 8080,
		kipperv1.AppLink{App: "docuseal", Namespace: "docuseal-test"})
	target := linkTestApp("docuseal", "docuseal-test", 3000)

	// docuseal has withdrawn its consent, and this app's Deployment will not
	// reconcile — a spec the API server rejects, a quota it cannot satisfy.
	// Every pass from here on fails at the same place.
	client := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(
		projectNS("hrportal-test", "hrportal"),
		projectNS("docuseal-test", "docuseal"),
		consentingProject("docuseal"),
		caller, target,
	).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.CreateOption) error {
			if _, isDeployment := obj.(*appsv1.Deployment); isDeployment {
				return fmt.Errorf("the cluster will not accept this deployment")
			}
			return c.Create(ctx, obj, opts...)
		},
	}).Build()
	r := &AppReconciler{Client: client, Scheme: scheme}

	// The allowance consent authorised before it was taken back.
	standing := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{
		Name: linkPolicyName("hrportal-backend"), Namespace: "hrportal-test"}}
	require.NoError(t, client.Create(context.Background(), standing))

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "hrportal-backend", Namespace: "hrportal-test"},
	})
	require.Error(t, err, "the reconcile must still fail on the workload it cannot roll")

	var np networkingv1.NetworkPolicy
	getErr := client.Get(context.Background(), types.NamespacedName{
		Name: linkPolicyName("hrportal-backend"), Namespace: "hrportal-test",
	}, &np)
	assert.True(t, errors.IsNotFound(getErr),
		"the withdrawn consent's allowance must be gone even though the pass failed after it")
}

// A link that opens nothing is recorded on both surfaces and carries no traffic,
// so the operator debugging a refused connection needs the app itself to say
// why. Until this, the only account was a line in the controller log.
func TestALinkThatOpensNothingSaysSoOnTheApp(t *testing.T) {
	scheme := testScheme()
	caller := linkTestApp("hrportal-backend", "hrportal-test", 8080,
		kipperv1.AppLink{App: "docuseal", Namespace: "docuseal-test"},
		kipperv1.AppLink{App: "ghost", Namespace: "billing-test"})
	target := linkTestApp("docuseal", "docuseal-test", 3000)

	// docuseal consents; billing does not, and "ghost" does not exist anyway.
	r := &AppReconciler{
		Client: crfake.NewClientBuilder().WithScheme(scheme).WithObjects(
			projectNS("hrportal-test", "hrportal"),
			projectNS("docuseal-test", "docuseal"),
			projectNS("billing-test", "billing"),
			consentingProject("docuseal", "hrportal"),
			consentingProject("billing"),
			caller, target,
		).WithStatusSubresource(&kipperv1.App{}).Build(),
		Scheme: scheme,
	}
	_, err := r.reconcileLinkPolicy(context.Background(), caller)
	require.NoError(t, err)

	cond := apimeta.FindStatusCondition(caller.Status.Conditions, kipperv1.ConditionLinksOpen)
	require.NotNil(t, cond, "a link that opens nothing must be reported on the app")
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Contains(t, cond.Message, "billing-test/ghost")
	assert.NotContains(t, cond.Message, "docuseal-test/docuseal",
		"the link that does carry traffic is not a complaint")

	// Consent granted: the complaint goes with it.
	var billing kipperv1.Project
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "billing"}, &billing))
	billing.Spec.AllowLinksFrom = []string{"hrportal"}
	require.NoError(t, r.Update(context.Background(), &billing))
	require.NoError(t, r.Create(context.Background(), linkTestApp("ghost", "billing-test", 9000)))

	_, err = r.reconcileLinkPolicy(context.Background(), caller)
	require.NoError(t, err)
	cond = apimeta.FindStatusCondition(caller.Status.Conditions, kipperv1.ConditionLinksOpen)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status,
		"once every link carries traffic the app says so, rather than falling silent")
}

// The condition has to reach the API server from where it is decided. Link
// policy is reconciled first precisely because the steps after it can fail, so a
// condition held in memory until the end of the pass goes missing on exactly the
// app that could not finish reconciling — the one whose operator most needs to
// know why its link is dead.
func TestWhyALinkIsDeadSurvivesAFailedReconcile(t *testing.T) {
	scheme := testScheme()
	caller := linkTestApp("hrportal-backend", "hrportal-test", 8080,
		kipperv1.AppLink{App: "docuseal", Namespace: "docuseal-test"})

	client := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(
		projectNS("hrportal-test", "hrportal"),
		projectNS("docuseal-test", "docuseal"),
		consentingProject("docuseal"), // no consent: the link opens nothing
		caller,
	).WithStatusSubresource(&kipperv1.App{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.CreateOption) error {
				if _, isDeployment := obj.(*appsv1.Deployment); isDeployment {
					return fmt.Errorf("the cluster will not accept this deployment")
				}
				return c.Create(ctx, obj, opts...)
			},
		}).Build()
	r := &AppReconciler{Client: client, Scheme: scheme}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "hrportal-backend", Namespace: "hrportal-test"},
	})
	require.Error(t, err, "the reconcile still fails on the workload it cannot roll")

	var stored kipperv1.App
	require.NoError(t, client.Get(context.Background(), types.NamespacedName{
		Name: "hrportal-backend", Namespace: "hrportal-test"}, &stored))
	cond := apimeta.FindStatusCondition(stored.Status.Conditions, kipperv1.ConditionLinksOpen)
	require.NotNil(t, cond, "the reason the link is dead must reach the API server, not just memory")
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Contains(t, cond.Message, "docuseal-test/docuseal")
}

// An app whose links all carry traffic says so, rather than saying nothing —
// otherwise "no complaint" and "never evaluated" read identically.
func TestAnAppWhoseLinksAllOpenSaysSo(t *testing.T) {
	scheme := testScheme()
	caller := linkTestApp("hrportal-backend", "hrportal-test", 8080,
		kipperv1.AppLink{App: "docuseal", Namespace: "docuseal-test"})
	target := linkTestApp("docuseal", "docuseal-test", 3000)

	r := &AppReconciler{
		Client: crfake.NewClientBuilder().WithScheme(scheme).
			WithObjects(withWorld(caller, target)...).WithStatusSubresource(&kipperv1.App{}).Build(),
		Scheme: scheme,
	}
	_, err := r.reconcileLinkPolicy(context.Background(), caller)
	require.NoError(t, err)

	cond := apimeta.FindStatusCondition(caller.Status.Conditions, kipperv1.ConditionLinksOpen)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)

	// An app declaring no links has nothing to report either way.
	plain := linkTestApp("standalone", "hrportal-test", 8080)
	_, perr := r.reconcileLinkPolicy(context.Background(), plain)
	require.NoError(t, perr)
	assert.Nil(t, apimeta.FindStatusCondition(plain.Status.Conditions, kipperv1.ConditionLinksOpen))
}

// The defect this shape exists to remove: an address stored when the link was
// made says whatever was true then. The allowance already follows the target's
// port, so a stored copy going stale leaves the caller dialling a number the
// policy no longer carries — reachable and refused, for reasons neither surface
// can show. Derived, there is no second copy to disagree.
func TestTheLinkAddressFollowsTheTargetsPort(t *testing.T) {
	scheme := testScheme()
	caller := linkTestApp("hrportal-backend", "hrportal-test", 8080,
		kipperv1.AppLink{App: "docuseal", Namespace: "docuseal-test"})
	target := linkTestApp("docuseal", "docuseal-test", 3000)

	r := &AppReconciler{
		Client: crfake.NewClientBuilder().WithScheme(scheme).WithObjects(withWorld(caller, target)...).Build(),
		Scheme: scheme,
	}

	live, _, err := ResolveLinks(context.Background(), r.Client, caller)
	require.NoError(t, err)
	vars := linkEnvVars(live)
	require.Len(t, vars, 1)
	assert.Equal(t, "DOCUSEAL_URL", vars[0].Name)
	assert.Equal(t, "http://docuseal.docuseal-test.svc.cluster.local:3000", vars[0].Value)

	target.Spec.Port = 9090
	require.NoError(t, r.Update(context.Background(), target))

	live, _, err = ResolveLinks(context.Background(), r.Client, caller)
	require.NoError(t, err)
	vars = linkEnvVars(live)
	require.Len(t, vars, 1)
	assert.Equal(t, "http://docuseal.docuseal-test.svc.cluster.local:9090", vars[0].Value,
		"the address the caller is given must be the one the target serves on now")
}

// The address is the Service's port, not the port the target's pods listen on.
// Those differ whenever the target runs the instance-id sidecar, and the caller
// dials the Service — the egress allowance names the other number for the same
// reason, and mixing them up breaks one end or the other.
func TestTheLinkAddressNamesTheServicePortNotThePodPort(t *testing.T) {
	scheme := testScheme()
	caller := linkTestApp("hrportal-backend", "hrportal-test", 8080,
		kipperv1.AppLink{App: "docuseal", Namespace: "docuseal-test"})
	target := linkTestApp("docuseal", "docuseal-test", 3000)
	target.Spec.Route = &kipperv1.AppRoute{Host: "sign.example.com"}

	r := &AppReconciler{
		Client:       crfake.NewClientBuilder().WithScheme(scheme).WithObjects(withWorld(caller, target)...).Build(),
		Scheme:       scheme,
		SidecarImage: "ghcr.io/example/kipper-sidecar:1",
	}

	live, _, err := ResolveLinks(context.Background(), r.Client, caller)
	require.NoError(t, err)
	vars := linkEnvVars(live)
	require.Len(t, vars, 1)
	assert.Equal(t, "http://docuseal.docuseal-test.svc.cluster.local:3000", vars[0].Value)
	assert.Equal(t, int32(13000), r.serviceTargetPort(target),
		"the pods are on 13000, so this test is exercising the case where the two differ")
}

// A link that opens nothing gets no address. Handing the caller one the policy
// refuses to carry it to fails further from the cause than a missing variable,
// and the LinksOpen condition already says why.
func TestAnUnconsentedLinkGivesTheCallerNoAddress(t *testing.T) {
	scheme := testScheme()
	caller := linkTestApp("hrportal-backend", "hrportal-test", 8080,
		kipperv1.AppLink{App: "docuseal", Namespace: "docuseal-test"},
		kipperv1.AppLink{App: "worker", Namespace: "hrportal-test"})
	target := linkTestApp("docuseal", "docuseal-test", 3000)
	worker := linkTestApp("worker", "hrportal-test", 9000)

	r := &AppReconciler{
		Client: crfake.NewClientBuilder().WithScheme(scheme).WithObjects(
			projectNS("hrportal-test", "hrportal"),
			projectNS("docuseal-test", "docuseal"),
			consentingProject("docuseal"), // withheld
			caller, target, worker,
		).Build(),
		Scheme: scheme,
	}

	live, _, err := ResolveLinks(context.Background(), r.Client, caller)
	require.NoError(t, err)
	vars := linkEnvVars(live)
	require.Len(t, vars, 1, "only the link that carries traffic gets an address")
	assert.Equal(t, "WORKER_URL", vars[0].Name,
		"a link inside the app's own namespace needs no consent and still gets its address")
}

// The policy and the address are rendered from one resolution, so they cannot
// disagree about which links are live. They used to: the policy loop skipped a
// same-namespace link before looking for its target, so a dependency inside the
// app's own namespace that did not exist counted as carrying traffic while no
// address was produced for it — the app reported every link open with a dead
// one among them.
func TestADeadLinkInTheAppsOwnNamespaceIsReportedLikeAnyOther(t *testing.T) {
	scheme := testScheme()
	caller := linkTestApp("hrportal-backend", "hrportal-test", 8080,
		kipperv1.AppLink{App: "ghost", Namespace: "hrportal-test"},
		kipperv1.AppLink{App: "portless", Namespace: "hrportal-test"})
	portless := linkTestApp("portless", "hrportal-test", 0)

	r := &AppReconciler{
		Client: crfake.NewClientBuilder().WithScheme(scheme).WithObjects(
			projectNS("hrportal-test", "hrportal"), caller, portless,
		).WithStatusSubresource(&kipperv1.App{}).Build(),
		Scheme: scheme,
	}

	live, blocked, err := ResolveLinks(context.Background(), r.Client, caller)
	require.NoError(t, err)
	assert.Empty(t, live, "neither target is usable")
	assert.Len(t, blocked, 2, "a same-namespace link that resolves to nothing is still a link that opens nothing")

	_, err = r.reconcileLinkPolicy(context.Background(), caller)
	require.NoError(t, err)
	cond := apimeta.FindStatusCondition(caller.Status.Conditions, kipperv1.ConditionLinksOpen)
	require.NotNil(t, cond, "the app must not report itself healthy over a dependency that is not there")
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Contains(t, cond.Message, "hrportal-test/ghost")
	assert.Contains(t, cond.Message, "hrportal-test/portless")

	assert.Empty(t, linkEnvVars(live), "and gets no address for either, which is what the condition explains")
}

// The condition says which links carry traffic. Writing it before the allowance
// is accepted turns it into a claim about what was intended: an app whose policy
// the API server refuses would report every link open, on every retry, for as
// long as the refusal lasted.
func TestLinksAreNotReportedOpenUntilTheAllowanceIsAccepted(t *testing.T) {
	scheme := testScheme()
	caller := linkTestApp("hrportal-backend", "hrportal-test", 8080,
		kipperv1.AppLink{App: "docuseal", Namespace: "docuseal-test"})
	target := linkTestApp("docuseal", "docuseal-test", 3000)

	client := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(withWorld(caller, target)...).WithStatusSubresource(&kipperv1.App{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.CreateOption) error {
				if _, isPolicy := obj.(*networkingv1.NetworkPolicy); isPolicy {
					return fmt.Errorf("the api server will not accept this policy")
				}
				return c.Create(ctx, obj, opts...)
			},
		}).Build()
	r := &AppReconciler{Client: client, Scheme: scheme}

	_, policyErr := r.reconcileLinkPolicy(context.Background(), caller)
	require.Error(t, policyErr,
		"a policy the cluster refuses must fail the reconcile")
	assert.Nil(t, apimeta.FindStatusCondition(caller.Status.Conditions, kipperv1.ConditionLinksOpen),
		"nothing may claim the link is open while the allowance it needs was refused")
}

// Two targets of the same name in different namespaces are distinct links, but
// they name one variable between them. Only one value reaches the pod, so the
// second must be refused rather than left declared, allowed, and unreachable.
func TestTwoLinksCannotClaimOneAddressVariable(t *testing.T) {
	scheme := testScheme()
	caller := linkTestApp("hrportal-backend", "hrportal-test", 8080,
		kipperv1.AppLink{App: "docuseal", Namespace: "docuseal-test"},
		kipperv1.AppLink{App: "docuseal", Namespace: "billing-test"})

	r := &AppReconciler{
		Client: crfake.NewClientBuilder().WithScheme(scheme).WithObjects(withWorld(
			caller,
			linkTestApp("docuseal", "docuseal-test", 3000),
			linkTestApp("docuseal", "billing-test", 4000),
		)...).WithStatusSubresource(&kipperv1.App{}).Build(),
		Scheme: scheme,
	}

	live, blocked, err := ResolveLinks(context.Background(), r.Client, caller)
	require.NoError(t, err)
	require.Len(t, live, 1, "one variable, one live link")
	assert.Equal(t, "docuseal-test", live[0].Link.Namespace, "the first declared keeps it")
	require.Len(t, blocked, 1)
	assert.Contains(t, blocked[0], "billing-test/docuseal")
	assert.Contains(t, blocked[0], "DOCUSEAL_URL")

	vars := linkEnvVars(live)
	require.Len(t, vars, 1, "the pod must not be given the same variable twice")
	assert.Equal(t, "http://docuseal.docuseal-test.svc.cluster.local:3000", vars[0].Value)
}
