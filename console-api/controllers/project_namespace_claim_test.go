package controllers

import (
	"context"
	goerrors "errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	kipperlabels "github.com/getkipper/kipper/controller/pkg/labels"
)

// A namespace's project label is the only thing saying whose it is, and it is
// mutable by anyone who can write the object. Recording what the reconcile
// actually took gives a second record that a relabel does not touch.
//
// The claim is written in this release and required in the next. Project.status
// is written whole by every controller already running, so a pod whose struct
// lacks the field drops it; trusting it now would be trusting a gap one old pod
// wide. That split is the thing this file holds.

func claimFixture(t *testing.T, objs ...crclient.Object) crclient.Client {
	t.Helper()
	all := make([]crclient.Object, 0, len(objs)+1)
	all = append(all, objs...)
	all = append(all, nodeWithIP("worker-1", "ExternalIP", "203.0.113.9"))
	return crfake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(all...).
		WithStatusSubresource(&kipperv1.Project{}).
		Build()
}

func namespaceClaimFor(t *testing.T, c crclient.Client, project, namespace string) (kipperv1.NamespaceClaim, bool) {
	t.Helper()
	var stored kipperv1.Project
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: project}, &stored))
	for _, claim := range stored.Status.NamespaceClaims {
		if claim.Name == namespace {
			return claim, true
		}
	}
	return kipperv1.NamespaceClaim{}, false
}

// The claim names the object, not the name. The fixture gives the namespace a
// UID because the fake client assigns none on create, and a test whose
// namespace and claim both carried an empty UID would compare nothing with
// nothing.
func TestAProjectClaimsTheNamespaceByItsObject(t *testing.T) {
	c := claimFixture(t,
		quotaTestProject("small", kipperv1.ProjectEnvironment{Name: "test"}),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name:   "shop-test",
			UID:    "the-object",
			Labels: map[string]string{kipperlabels.Project: "shop"},
		}},
	)
	reconcileNamed(t, c, "shop")

	claim, ok := namespaceClaimFor(t, c, "shop", "shop-test")
	require.True(t, ok, "the project holds a namespace and recorded no claim to it, so the label is still the only thing that says whose it is")
	assert.Equal(t, types.UID("the-object"), claim.UID,
		"the claim carries no UID, so a namespace deleted and recreated under the same name would inherit it")
}

// A namespace whose UID cannot be read yields no claim rather than a claim with
// an empty one. An empty UID matches no live namespace, so the project would
// not recognise its own, while still reading as a claim to anything looking at
// the name alone.
func TestNoUIDMeansNoClaim(t *testing.T) {
	c := claimFixture(t, quotaTestProject("small", kipperv1.ProjectEnvironment{Name: "test"}))
	reconcileNamed(t, c, "shop")

	claim, ok := namespaceClaimFor(t, c, "shop", "shop-test")
	assert.False(t, ok, "a claim was recorded with UID %q, which matches nothing and reads as ownership to anything checking the name", claim.UID)
}

// The rule, in one form: adopt what nobody claims, whatever created it, and
// never take what another project claims.
//
// This half is the adoption. A namespace `kip` made, or one a restore brought
// back, is not this reconcile's creation and still has to be usable.
func TestAProjectAdoptsANamespaceNobodyClaims(t *testing.T) {
	c := claimFixture(t,
		quotaTestProject("small", kipperv1.ProjectEnvironment{Name: "test"}),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name:   "shop-test",
			UID:    "restored",
			Labels: map[string]string{kipperlabels.Project: "shop"},
		}},
	)
	reconcileNamed(t, c, "shop")

	_, ok := namespaceClaimFor(t, c, "shop", "shop-test")
	assert.True(t, ok, "a namespace nobody claims was refused, so a restore or a kip-created namespace leaves the project unable to use its own environment")
}

// A project that already holds claims still adopts an unclaimed one, because
// adding an environment is an ordinary thing to do and the rule is about what
// may be adopted rather than when.
func TestAProjectWithClaimsStillAdoptsAnUnclaimedNamespace(t *testing.T) {
	project := quotaTestProject("small",
		kipperv1.ProjectEnvironment{Name: "test"},
		kipperv1.ProjectEnvironment{Name: "prod"},
	)
	project.Status.NamespaceClaims = []kipperv1.NamespaceClaim{{Name: "shop-test", UID: "old"}}
	c := claimFixture(t, project,
		// Labelled for this project and claimed by nobody. The label still
		// governs adoption in this release; the claim is an additional record,
		// not a replacement, until the resolver requires it.
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name:   "shop-prod",
			UID:    "fresh",
			Labels: map[string]string{kipperlabels.Project: "shop"},
		}},
	)
	reconcileNamed(t, c, "shop")

	_, ok := namespaceClaimFor(t, c, "shop", "shop-prod")
	assert.True(t, ok, "a project that already holds a claim refused a new unclaimed namespace, so adding an environment is impossible")
}

// The other half, and the one that matters. Another project's claim is not
// this project's to take, whatever the label says.
func TestAProjectNeverTakesANamespaceAnotherProjectClaims(t *testing.T) {
	shop := quotaTestProject("small", kipperv1.ProjectEnvironment{Name: "test"})
	rival := quotaTestProject("small")
	rival.Name = "rival"
	rival.Status.NamespaceClaims = []kipperv1.NamespaceClaim{{Name: "shop-test", UID: "held"}}

	c := claimFixture(t, shop, rival,
		// The label has been rewritten to shop, which is the drift the claim
		// exists to survive.
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name:   "shop-test",
			UID:    "held",
			Labels: map[string]string{kipperlabels.Project: "shop"},
		}},
	)
	reconcileNamed(t, c, "shop")

	_, ok := namespaceClaimFor(t, c, "shop", "shop-test")
	assert.False(t, ok, "a project took a namespace another project claims, on the strength of a label anyone can write")

	var stored kipperv1.Project
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "shop"}, &stored))
	cond := apimeta.FindStatusCondition(stored.Status.Conditions, conditionNamespaceConflict)
	require.NotNil(t, cond, "the namespace was refused and the project says nothing about it")
	assert.Contains(t, cond.Message, "rival",
		"the message does not name the project holding the claim, which is the one fact needed to resolve it")
}

// A name outlives the object it named. A namespace deleted and recreated is a
// different namespace, so a claim naming the old UID says nothing about the new
// object and must not be read as "we already hold this".
//
// The fixture makes the UID load-bearing: shop holds a stale claim, the live
// namespace is a different object, and another project resolves to the same
// name. Matching on name alone would let the stale claim short-circuit the
// collision check and hand shop a namespace it no longer holds.
func TestAStaleClaimDoesNotStandInForTheObjectItNamed(t *testing.T) {
	shop := quotaTestProject("small", kipperv1.ProjectEnvironment{Name: "prod"})
	shop.Status.NamespaceClaims = []kipperv1.NamespaceClaim{{Name: "shop-prod", UID: "the-old-one"}}

	// Resolves to shop-prod through its default environment.
	other := quotaTestProject("small", kipperv1.ProjectEnvironment{Name: "default"})
	other.Name = "shop-prod"

	c := claimFixture(t, shop, other,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name:   "shop-prod",
			UID:    "the-new-one",
			Labels: map[string]string{kipperlabels.Project: "shop"},
		}},
	)
	reconcileNamed(t, c, "shop")

	claim, ok := namespaceClaimFor(t, c, "shop", "shop-prod")
	assert.False(t, ok,
		"a claim naming a UID that is gone was read as current, so the project took a recreated namespace another project also resolves to (claim=%v)", claim)
}

// Two projects resolving to one name, with nobody holding it yet. Handing it to
// whichever reconciles first is a race that decides ownership, so neither gets
// it and the collision is reported.
func TestANamespaceTwoProjectsWouldClaimIsGivenToNeither(t *testing.T) {
	shop := quotaTestProject("small", kipperv1.ProjectEnvironment{Name: "prod"})
	// A project literally named shop-prod, whose default environment resolves
	// to its own name. That is the collision: shop's "prod" and shop-prod's
	// default both mean the namespace shop-prod.
	other := quotaTestProject("small", kipperv1.ProjectEnvironment{Name: "default"})
	other.Name = "shop-prod"

	c := claimFixture(t, shop, other,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name:   "shop-prod",
			UID:    "unclaimed",
			Labels: map[string]string{kipperlabels.Project: "shop"},
		}},
	)
	reconcileNamed(t, c, "shop")
	reconcileNamed(t, c, "shop-prod")

	_, shopHas := namespaceClaimFor(t, c, "shop", "shop-prod")
	_, otherHas := namespaceClaimFor(t, c, "shop-prod", "shop-prod")
	assert.False(t, shopHas && otherHas, "both projects claim the same namespace")
	assert.False(t, shopHas || otherHas,
		"a namespace two projects resolve to was given to whichever reconciled first, so ownership is decided by a race")
}

// Nothing reads the claim yet. An older pod's whole-status write drops it, so a
// release that required it would refuse a project its own namespaces one old
// pod after a rollout began.
func TestNothingRequiresTheClaimYet(t *testing.T) {
	c := claimFixture(t,
		quotaTestProject("small", kipperv1.ProjectEnvironment{Name: "test"}),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name:   "shop-test",
			UID:    "the-object",
			Labels: map[string]string{kipperlabels.Project: "shop"},
		}},
	)
	reconcileNamed(t, c, "shop")

	var stored kipperv1.Project
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "shop"}, &stored))
	stored.Status.NamespaceClaims = nil
	require.NoError(t, c.Status().Update(context.Background(), &stored))

	reconcileNamed(t, c, "shop")

	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "shop"}, &stored))
	assert.Equal(t, "Active", stored.Status.Phase,
		"erasing the claim changed what the reconcile did, so something already requires it")
	assert.Contains(t, stored.Status.Namespaces, "shop-test",
		"erasing the claim cost the project a namespace it owns, which is what an older pod in a rolling window would do")
	_, ok := namespaceClaimFor(t, c, "shop", "shop-test")
	assert.True(t, ok, "the reconcile did not put the claim back after an older pod dropped it")
}

// The claim survives a failure in the work that comes after it. Quota,
// bindings, shared storage and pruning all run later and any of them can fail,
// and a claim written only once they had all succeeded would leave the
// namespace unclaimed for as long as one kept failing. The add-environment flow
// waits on the claim, so that is a hang rather than a delay.
func TestTheClaimSurvivesAFailureLaterInThePass(t *testing.T) {
	project := quotaTestProject("small", kipperv1.ProjectEnvironment{Name: "test"})
	c := crfake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(
			project,
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
				Name:   "shop-test",
				UID:    "the-object",
				Labels: map[string]string{kipperlabels.Project: "shop"},
			}},
			nodeWithIP("worker-1", "ExternalIP", "203.0.113.9"),
		).
		WithStatusSubresource(&kipperv1.Project{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl crclient.WithWatch, obj crclient.Object, opts ...crclient.CreateOption) error {
				// Quota comes after the claim. Failing it stops the pass
				// before the end-of-pass status write.
				if _, ok := obj.(*corev1.ResourceQuota); ok {
					return apierrors.NewInternalError(errQuotaRefused)
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).
		Build()

	r := &ProjectReconciler{Client: c, Scheme: testScheme(), APIReader: c}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "shop"}})
	require.Error(t, err, "the fixture is meant to fail the pass after the claim, and the pass succeeded")

	_, ok := namespaceClaimFor(t, c, "shop", "shop-test")
	assert.True(t, ok,
		"the claim was lost because a later step failed, so the namespace stays unclaimed for as long as that step keeps failing and add-environment waits forever")
}

var errQuotaRefused = goerrors.New("quota refused by the test")

// The same collision, with the namespace absent. Two projects that resolve to
// one name and find nothing there would otherwise both create it, and whichever
// lost the race would carry on as though it had not: ownership settled by
// scheduling, which is what the refusal exists to prevent.
//
// The existing-namespace case is checked above and does not cover this one,
// because it never reaches the create path.
func TestANamespaceTwoProjectsWouldCreateIsCreatedByNeither(t *testing.T) {
	shop := quotaTestProject("small", kipperv1.ProjectEnvironment{Name: "prod"})
	other := quotaTestProject("small", kipperv1.ProjectEnvironment{Name: "default"})
	other.Name = "shop-prod"

	c := claimFixture(t, shop, other)
	reconcileNamed(t, c, "shop")

	var ns corev1.Namespace
	err := c.Get(context.Background(), types.NamespacedName{Name: "shop-prod"}, &ns)
	assert.True(t, apierrors.IsNotFound(err),
		"a namespace two projects resolve to was created by whichever reconciled first, so the other one's environment silently became this one's")
}
