package controllers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	kipperlabels "github.com/getkipper/kipper/controller/pkg/labels"
)

// The claim's whole promise is that ownership rests on something a relabel does
// not touch. The end-of-pass write is where that promise was broken: it replaced
// the list with what this pass reached, and the one thing that stops a pass
// reaching a namespace it already holds is somebody rewriting the label. So the
// write that exists to record ownership was released by exactly the edit the
// record defends against.
//
// It keeps rather than replaces now, and what it keeps is bounded: the object
// has to still be there under the same UID, and the project has to still be
// asking for it. Liveness alone would let a claim outlive the project's right to
// the namespace and block anyone else from ever adopting it.
// The contested namespace in every fixture here, and the project it gets
// relabelled to, which is any project that is not the one holding it.
const (
	retentionNamespace = "shop-prod"
	rivalProject       = "grocer"
)

func relabelled(t *testing.T, c crclient.Client) {
	t.Helper()
	var ns corev1.Namespace
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: retentionNamespace}, &ns))
	ns.Labels[kipperlabels.Project] = rivalProject
	require.NoError(t, c.Update(context.Background(), &ns))
}

// Every fixture here is one project holding two namespaces, so the project is
// named once rather than passed through each helper.
const retentionProject = "shop"

func projectHolding(envs []string, namespaces ...string) *kipperv1.Project {
	p := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: retentionProject, Finalizers: []string{projectFinalizer}},
	}
	for _, env := range envs {
		p.Spec.Environments = append(p.Spec.Environments, kipperv1.ProjectEnvironment{Name: env})
	}
	for _, ns := range namespaces {
		p.Status.Namespaces = append(p.Status.Namespaces, ns)
		p.Status.NamespaceClaims = append(p.Status.NamespaceClaims,
			kipperv1.NamespaceClaim{Name: ns, UID: types.UID("uid-" + ns)})
	}
	return p
}

func heldNamespace(name string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   name,
		UID:    types.UID("uid-" + name),
		Labels: map[string]string{kipperlabels.Project: retentionProject, kipperlabels.Environment: "prod"},
	}}
}

func TestARelabelledNamespaceKeepsItsClaim(t *testing.T) {
	shop := projectHolding([]string{"test", "prod"}, "shop-test", "shop-prod")
	c := claimFixture(t, shop, heldNamespace("shop-test"), heldNamespace("shop-prod"))

	relabelled(t, c)
	reconcileNamed(t, c, retentionProject)

	claim, ok := namespaceClaimFor(t, c, "shop", "shop-prod")
	require.True(t, ok, "rewriting a namespace's label erased the claim recording that this project took it, which is the one edit the claim exists to survive")
	assert.Equal(t, types.UID("uid-shop-prod"), claim.UID, "the claim was kept but no longer names the object it was made for")

	var stored kipperv1.Project
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "shop"}, &stored))
	assert.NotNil(t, apimeta.FindStatusCondition(stored.Status.Conditions, conditionNamespaceConflict),
		"the pass skipped a namespace it holds and said nothing about it")
}

// The other half, so the union is not mistaken for "claims are never pruned". A
// claim naming an object that is gone is worth nothing and must not linger.
func TestADeletedNamespaceLosesItsClaim(t *testing.T) {
	// The environment is gone from the spec and so is the namespace, which is
	// what the pass after a prune looks like. A namespace whose environment is
	// still declared would simply be created again, and the claim would follow
	// it rather than being dropped.
	shop := projectHolding([]string{"test"}, "shop-test", "shop-prod")
	c := claimFixture(t, shop, heldNamespace("shop-test"))

	reconcileNamed(t, c, retentionProject)

	if claim, ok := namespaceClaimFor(t, c, "shop", "shop-prod"); ok {
		t.Errorf("a claim on a namespace that no longer exists was kept: %+v", claim)
	}
}

// Liveness on its own is not enough to keep a claim. A namespace that was
// relabelled away and whose environment has since left the spec is one this
// project neither holds nor asks for, and a claim kept on the strength of the
// object still existing would sit there permanently, refusing the namespace to
// the project that does hold it.
func TestAClaimIsDroppedOnceTheProjectStopsAskingForTheNamespace(t *testing.T) {
	shop := projectHolding([]string{"test", "prod"}, "shop-test", "shop-prod")
	c := claimFixture(t, shop, heldNamespace("shop-test"), heldNamespace("shop-prod"))

	relabelled(t, c)

	var stored kipperv1.Project
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "shop"}, &stored))
	stored.Spec.Environments = []kipperv1.ProjectEnvironment{{Name: "test"}}
	require.NoError(t, c.Update(context.Background(), &stored))

	reconcileNamed(t, c, retentionProject)

	if claim, ok := namespaceClaimFor(t, c, "shop", "shop-prod"); ok {
		t.Errorf("a project kept its claim on a namespace it no longer declares, so nobody else can ever adopt it: %+v", claim)
	}
}

// A namespace the project still declares and still holds keeps its claim
// through an ordinary pass, which is the case the two above are exceptions to.
func TestAHeldNamespaceKeepsItsClaimThroughAnOrdinaryPass(t *testing.T) {
	shop := projectHolding([]string{"test", "prod"}, "shop-test", "shop-prod")
	c := claimFixture(t, shop, heldNamespace("shop-test"), heldNamespace("shop-prod"))

	reconcileNamed(t, c, retentionProject)

	for _, ns := range []string{"shop-test", "shop-prod"} {
		claim, ok := namespaceClaimFor(t, c, "shop", ns)
		require.True(t, ok, "an ordinary pass dropped the claim to %s", ns)
		assert.Equal(t, types.UID("uid-"+ns), claim.UID)
	}
}

// The namespace list is the pre-claims record, and on a cluster upgrading from
// a release that wrote no claims it is the only evidence a project holds
// anything at all. The end-of-pass write rebuilt it from what the pass reached,
// so one pass that skipped a namespace erased the record of it permanently:
// nothing can prune it, deleting the project strands it, and no later pass can
// adopt it, because every one of those decisions now reads the record that the
// skip destroyed.
func TestASkippedNamespaceKeepsItsPlaceInTheRecord(t *testing.T) {
	shop := projectHolding([]string{"test", "prod"}, "shop-test", "shop-prod")
	// The upgrade case: a record from the previous release and no claims.
	shop.Status.NamespaceClaims = nil
	c := claimFixture(t, shop, heldNamespace("shop-test"), heldNamespace("shop-prod"))

	relabelled(t, c)
	reconcileNamed(t, c, retentionProject)

	var stored kipperv1.Project
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: retentionProject}, &stored))
	assert.Contains(t, stored.Status.Namespaces, "shop-prod",
		"one pass that skipped this namespace erased the only record that the project ever held it, so nothing can prune it, deleting the project strands it, and no later pass can take it back")
}

// A namespace that is genuinely gone still leaves the record, or the list grows
// forever and keeps authorising names nothing answers to.
func TestADeletedNamespaceLeavesTheRecord(t *testing.T) {
	// The environment is gone from the spec and so is the namespace, which is
	// what the pass after a prune looks like. A namespace whose environment is
	// still declared would simply be created again.
	shop := projectHolding([]string{"test"}, "shop-test", "shop-prod")
	c := claimFixture(t, shop, heldNamespace("shop-test"))

	reconcileNamed(t, c, retentionProject)

	var stored kipperv1.Project
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: retentionProject}, &stored))
	assert.NotContains(t, stored.Status.Namespaces, "shop-prod",
		"a namespace that no longer exists stayed on the project's record")
}

// Retention is bounded by what the project still asks for, and not by the object
// merely existing.
//
// Keeping a name on existence alone let the record outlive the project's right
// to it, and the record authorises deletion. A project that stops declaring an
// environment whose namespace has been relabelled away keeps neither the claim
// nor any other tie to it, but an unbounded record would hold that name for
// good. Another project then legitimately adopts and claims the namespace, and
// the day somebody points the label back at the first project, its stale record
// authorises deleting a live namespace that now belongs to somebody else.
//
// The label is not the guard against that. Rewriting the label is the move the
// whole deletion gate exists to survive, so it cannot also be the thing that
// makes the stale record safe.
func TestARecordIsDroppedOnceTheProjectStopsAskingForTheNamespace(t *testing.T) {
	shop := projectHolding([]string{"test", "prod"}, "shop-test", "shop-prod")
	c := claimFixture(t, shop, heldNamespace("shop-test"), heldNamespace("shop-prod"))

	relabelled(t, c)

	var stored kipperv1.Project
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: retentionProject}, &stored))
	stored.Spec.Environments = []kipperv1.ProjectEnvironment{{Name: "test"}}
	require.NoError(t, c.Update(context.Background(), &stored))

	reconcileNamed(t, c, retentionProject)

	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: retentionProject}, &stored))
	assert.NotContains(t, stored.Status.Namespaces, "shop-prod",
		"a project kept a namespace on its record after it stopped declaring it and lost it to another project, and that record is what authorises deleting it")
}

// The claim that no longer matches the live object is the evidence, so it has to
// outlive the pass that noticed the mismatch.
//
// A namespace deleted and recreated unlabelled is somebody else's until a pass
// adopts it, and the reconcile refuses it and says so. What makes the refusal
// stick is the project's own claim naming the object that went away: the
// unlabelled backstop asks for the object, and the stale claim is what tells it
// the live one is not ours. Dropping that claim on the mismatch leaves only the
// name-only record, which answers yes, and the third party's namespace is
// collected on the next cleanup that runs.
//
// One pass hides this. The claim is still there while the pass that saw the
// mismatch is running, so a single-pass test passes and the second pass erases
// what it proved.
func TestTheClaimOnAReplacedObjectSurvivesThePassThatNoticed(t *testing.T) {
	shop := projectHolding([]string{"test", "prod"}, "shop-test", "shop-prod")
	c := claimFixture(t, shop, heldNamespace("shop-test"), heldNamespace("shop-prod"))

	// Deleted and recreated by somebody else, carrying no label.
	var gone corev1.Namespace
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: retentionNamespace}, &gone))
	require.NoError(t, c.Delete(context.Background(), &gone))
	require.NoError(t, c.Create(context.Background(), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: retentionNamespace, UID: "somebody-elses-object"},
	}))

	reconcileNamed(t, c, retentionProject)

	claim, ok := namespaceClaimFor(t, c, retentionProject, retentionNamespace)
	require.True(t, ok, "the claim naming the object that went away was dropped, so nothing is left to tell the cleanup paths that the namespace now carrying that name is not this project's")
	assert.Equal(t, types.UID("uid-shop-prod"), claim.UID, "the claim was repointed at an object the reconcile refused")
}

// And the whole way through: one full pass, then delete the project, and the
// namespace that is not this project's is still there.
func TestDeletingTheProjectAPassLaterStillLeavesTheReplacement(t *testing.T) {
	shop := projectHolding([]string{"test", "prod"}, "shop-test", "shop-prod")
	c := claimFixture(t, shop, heldNamespace("shop-test"), heldNamespace("shop-prod"))

	var gone corev1.Namespace
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: retentionNamespace}, &gone))
	require.NoError(t, c.Delete(context.Background(), &gone))
	require.NoError(t, c.Create(context.Background(), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: retentionNamespace, UID: "somebody-elses-object"},
	}))

	reconcileNamed(t, c, retentionProject)

	var stored kipperv1.Project
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: retentionProject}, &stored))
	r := &ProjectReconciler{Client: c, Scheme: testScheme(), APIReader: c}
	require.NoError(t, pruneRan(r.deleteProjectNamespaces(context.Background(), &stored, false)))

	var ns corev1.Namespace
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: retentionNamespace}, &ns),
		"deleting the project destroyed a namespace that is not its own, one pass after the reconcile had refused that very namespace and told the operator it would not take it over")
}
