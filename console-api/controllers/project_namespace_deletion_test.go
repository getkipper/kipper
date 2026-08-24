package controllers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	kipperlabels "github.com/getkipper/kipper/controller/pkg/labels"
)

// Deletion is the one decision a namespace's label must never be able to make
// on its own.
//
// Pruning reaches every namespace carrying the project's label that the project
// does not currently declare, and the label is writable by anyone who can write
// a namespace. Pointing a victim's namespace at another project therefore hands
// that project's next pass a namespace it never held and no reason to keep it,
// and the pass deletes it with everything inside. Disclosure can be undone;
// this cannot.
//
// So a candidate needs evidence that this project actually held the namespace,
// and the evidence is the same record the rest of the reconcile already keeps:
// a claim naming the object, or the namespace in the status this project last
// wrote. Neither is reachable by writing a label.
func relabelledFixture(t *testing.T, attacker *kipperv1.Project) *ProjectReconciler {
	t.Helper()
	scheme := testScheme()
	// The victim's namespace, relabelled. The attacker's project has never held
	// it: no claim names it and its status has never mentioned it.
	victim := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   "victim-prod",
		UID:    "the-victims-namespace",
		Labels: map[string]string{kipperlabels.Project: attacker.Name},
	}}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(victim, attacker).WithStatusSubresource(attacker).Build()
	r := &ProjectReconciler{
		Client:    c,
		APIReader: c,
		Scheme:    scheme,
	}
	return r
}

func TestARelabelledNamespaceIsNotPrunedByTheProjectItWasPointedAt(t *testing.T) {
	attacker := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "attacker"},
		Spec: kipperv1.ProjectSpec{
			Environments: []kipperv1.ProjectEnvironment{{Name: "test"}},
		},
		Status: kipperv1.ProjectStatus{Namespaces: []string{"attacker-test"}},
	}
	r := relabelledFixture(t, attacker)

	require.NoError(t, pruneRan(r.deleteProjectNamespaces(context.Background(), attacker, true)))

	var ns corev1.Namespace
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "victim-prod"}, &ns),
		"a namespace was deleted by a project that never held it, on the strength of a label anyone who can write a namespace can set")
}

// The finalizer runs the same pruning with nothing kept, so it deletes strictly
// more. Proving the prune path alone would leave the wider one open.
func TestARelabelledNamespaceIsNotCollectedWhenThatProjectIsDeleted(t *testing.T) {
	// A project that declares no environments still gets one, called test, so
	// the namespace it holds is attacker-test rather than its own bare name.
	attacker := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "attacker"},
		Status:     kipperv1.ProjectStatus{Namespaces: []string{"attacker-test"}},
	}
	r := relabelledFixture(t, attacker)

	require.NoError(t, pruneRan(r.deleteProjectNamespaces(context.Background(), attacker, false)))

	var ns corev1.Namespace
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "victim-prod"}, &ns),
		"deleting a project took a namespace with it that the project had only been pointed at")
}

// The claim is the earlier of the two records: it is published the moment a
// namespace is proven this project's, while status is written at the end of the
// pass. A pass that adopted a namespace and then failed leaves the claim and no
// status, and the namespace must still be collectable, or deleting the project
// strands it with its workloads and its member bindings.
func TestANamespaceClaimedButNotYetRecordedIsStillCollected(t *testing.T) {
	scheme := testScheme()
	adopted := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   "shop-test",
		UID:    "the-namespace",
		Labels: map[string]string{kipperlabels.Project: "shop"},
	}}
	project := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop"},
		Status: kipperv1.ProjectStatus{
			NamespaceClaims: []kipperv1.NamespaceClaim{{Name: "shop-test", UID: "the-namespace"}},
		},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(adopted, project).WithStatusSubresource(project).Build()
	r := &ProjectReconciler{
		Client:    c,
		APIReader: c,
		Scheme:    scheme,
	}

	require.NoError(t, pruneRan(r.deleteProjectNamespaces(context.Background(), project, false)))

	var ns corev1.Namespace
	err := r.Get(context.Background(), types.NamespacedName{Name: "shop-test"}, &ns)
	assert.True(t, errors.IsNotFound(err),
		"a namespace this project had claimed outlived the project, so its workloads and member bindings have nothing left to collect them")
}

// A claim names an object, not a name. One naming a namespace that has since
// been deleted and recreated says nothing about the replacement, so it cannot
// be the evidence that authorises deleting it.
func TestAClaimOnTheObjectThatIsGoneDoesNotAuthoriseDeletingItsReplacement(t *testing.T) {
	scheme := testScheme()
	replacement := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   "shop-test",
		UID:    "the-new-object",
		Labels: map[string]string{kipperlabels.Project: "shop"},
	}}
	project := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop"},
		Status: kipperv1.ProjectStatus{
			NamespaceClaims: []kipperv1.NamespaceClaim{{Name: "shop-test", UID: "the-old-object"}},
		},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(replacement, project).WithStatusSubresource(project).Build()
	r := &ProjectReconciler{
		Client:    c,
		APIReader: c,
		Scheme:    scheme,
	}

	require.NoError(t, pruneRan(r.deleteProjectNamespaces(context.Background(), project, false)))

	var ns corev1.Namespace
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "shop-test"}, &ns),
		"a claim naming an object that is gone authorised deleting the different object now carrying its name")
}

// Cleanup and resolution ask different questions of the same records, and the
// difference is the object.
//
// Resolution asks whether the project holds *this object*, so a claim naming the
// name at a different object settles it: the replacement is not what the project
// took. Cleanup asks whether the project ever took *this name*, because what it
// is collecting is a namespace carrying the project's label that will otherwise
// outlive it. Requiring object identity there strands exactly the namespace that
// most needs collecting: one recreated out of band, still labelled for the
// project, with the project's own record naming it and a claim that no longer
// matches.
func TestARecreatedNamespaceTheProjectRecordedIsStillCollected(t *testing.T) {
	scheme := testScheme()
	recreated := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   "shop-test",
		UID:    "the-new-object",
		Labels: map[string]string{kipperlabels.Project: "shop"},
	}}
	project := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop"},
		Status: kipperv1.ProjectStatus{
			Namespaces:      []string{"shop-test"},
			NamespaceClaims: []kipperv1.NamespaceClaim{{Name: "shop-test", UID: "the-object-that-is-gone"}},
		},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(recreated, project).WithStatusSubresource(project).Build()
	r := &ProjectReconciler{
		Client:    c,
		APIReader: c,
		Scheme:    scheme,
	}

	require.NoError(t, pruneRan(r.deleteProjectNamespaces(context.Background(), project, false)))

	var ns corev1.Namespace
	err := r.Get(context.Background(), types.NamespacedName{Name: "shop-test"}, &ns)
	assert.True(t, errors.IsNotFound(err),
		"a namespace this project recorded holding, still carrying its label, outlived the project because a claim named an older object; its workloads and member bindings have nothing left to collect them")
}

// The backstop collects what the label query cannot see, and it has to reach the
// same two records the rest of the gate reads.
//
// A claim is published the moment a namespace is proven this project's, before
// anything that can fail, so a pass that adopted a namespace and then failed
// leaves a claim and no namespace record. Strip that namespace's label and the
// label query cannot see it either. Walking only the recorded names then reaches
// nothing, the finalizer removes itself, and the namespace is stranded with the
// workloads and member bindings the pass had already put in it.
func TestAClaimedNamespaceStrippedOfItsLabelIsStillCollected(t *testing.T) {
	scheme := testScheme()
	stripped := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "shop-test",
		UID:  "the-namespace",
	}}
	project := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop"},
		Status: kipperv1.ProjectStatus{
			NamespaceClaims: []kipperv1.NamespaceClaim{{Name: "shop-test", UID: "the-namespace"}},
		},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(stripped, project).WithStatusSubresource(project).Build()
	r := &ProjectReconciler{
		Client:    c,
		APIReader: c,
		Scheme:    scheme,
	}

	require.NoError(t, pruneRan(r.deleteProjectNamespaces(context.Background(), project, false)))

	var ns corev1.Namespace
	err := r.Get(context.Background(), types.NamespacedName{Name: "shop-test"}, &ns)
	assert.True(t, errors.IsNotFound(err),
		"a namespace this project had claimed and whose label was then removed outlived the project, so its workloads and member bindings have nothing left to collect them")
}

// The same backstop must not collect a different object that happens to carry a
// recorded name.
//
// A namespace deleted and recreated unlabelled is somebody else's until a pass
// adopts it, and the reconcile says so: it refuses the unlabelled object and
// reports that it will not take it over. The project's own claim still names the
// object that went away, and that mismatch is the evidence. Reading the name
// alone here destroys a third party's namespace and contradicts the refusal the
// operator has just been shown.
func TestAnUnlabelledReplacementUnderARecordedNameIsNotCollected(t *testing.T) {
	scheme := testScheme()
	replacement := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "shop-test",
		UID:  "somebody-elses-object",
	}}
	project := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop"},
		Status: kipperv1.ProjectStatus{
			Namespaces:      []string{"shop-test"},
			NamespaceClaims: []kipperv1.NamespaceClaim{{Name: "shop-test", UID: "the-object-that-is-gone"}},
		},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(replacement, project).WithStatusSubresource(project).Build()
	r := &ProjectReconciler{
		Client:    c,
		APIReader: c,
		Scheme:    scheme,
	}

	require.NoError(t, pruneRan(r.deleteProjectNamespaces(context.Background(), project, false)))

	var ns corev1.Namespace
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "shop-test"}, &ns),
		"a namespace that is not this project's, carrying no label and a name the project used to hold, was destroyed with everything in it")
}

// A project's own record is evidence about the project, not about the object,
// and another project's claim is evidence about the object.
//
// Two projects can both have a namespace on record: one held it, lost it, and
// still declares the environment whose name resolves to it, which is exactly
// the state claim retention keeps alive so a relabel cannot erase it. The other
// holds it now and has published a claim naming the live object. If the label
// is then pointed back at the first, its name-only record says "mine" and
// nothing consults the claim that says otherwise, so cleanup deletes a live
// namespace belonging to somebody else.
//
// Rewriting the label is the move every gate here exists to survive, so it
// cannot be what tips this. An exact claim on the object outranks any name-only
// record, whoever holds it.
func TestANamespaceAnotherProjectClaimsIsNeverCollected(t *testing.T) {
	scheme := testScheme()
	contested := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   "shop-prod",
		UID:    "the-live-object",
		Labels: map[string]string{kipperlabels.Project: "shop"},
	}}
	// shop held it once and still declares the environment, so retention keeps
	// the name on its record. It has no claim: the reconcile refuses it.
	shop := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop"},
		Spec:       kipperv1.ProjectSpec{Environments: []kipperv1.ProjectEnvironment{{Name: "prod"}}},
		Status:     kipperv1.ProjectStatus{Namespaces: []string{"shop-prod"}},
	}
	// grocer holds it now, and its claim names the live object.
	grocer := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "grocer"},
		Status: kipperv1.ProjectStatus{
			Namespaces:      []string{"shop-prod"},
			NamespaceClaims: []kipperv1.NamespaceClaim{{Name: "shop-prod", UID: "the-live-object"}},
		},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(contested, shop, grocer).WithStatusSubresource(shop, grocer).Build()
	r := &ProjectReconciler{
		Client:    c,
		APIReader: c,
		Scheme:    scheme,
	}

	require.NoError(t, pruneRan(r.deleteProjectNamespaces(context.Background(), shop, false)))

	var ns corev1.Namespace
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "shop-prod"}, &ns),
		"a live namespace another project holds and claims was destroyed, with everything in it, because a stale name on this project's record outranked that claim")
}

// The same record still collects a namespace nobody else claims, which is what
// it is for.
func TestANamespaceNoOtherProjectClaimsIsStillCollected(t *testing.T) {
	scheme := testScheme()
	held := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   "shop-prod",
		UID:    "the-live-object",
		Labels: map[string]string{kipperlabels.Project: "shop"},
	}}
	shop := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop"},
		Status:     kipperv1.ProjectStatus{Namespaces: []string{"shop-prod"}},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(held, shop).WithStatusSubresource(shop).Build()
	r := &ProjectReconciler{
		Client:    c,
		APIReader: c,
		Scheme:    scheme,
	}

	require.NoError(t, pruneRan(r.deleteProjectNamespaces(context.Background(), shop, false)))

	var ns corev1.Namespace
	err := r.Get(context.Background(), types.NamespacedName{Name: "shop-prod"}, &ns)
	assert.True(t, errors.IsNotFound(err), "the project's own namespace outlived it")
}

// pruneRan drops the live project the prune decided from, for the tests that
// care about which namespaces survived rather than which spec decided it.
func pruneRan(_ *kipperv1.Project, err error) error {
	return err
}

// The prune binds each delete to the object its authorisation was checked
// against, and nothing else in these tests would notice if it stopped: the fake
// client honours only the resourceVersion half of a delete precondition
// (`fake/client.go`: "Check the ResourceVersion if that Precondition was
// specified"), so a UID-only one is invisible to it and a delete by name goes
// through either way. The HTTP path already asserts this for the same reason.
//
// What it is for: a namespace can finish terminating and be recreated under the
// same name between the read that authorised the delete and the delete itself,
// and by name alone the prune takes the replacement on the strength of a check
// made against its predecessor. That one has no undo.
func TestThePruneBindsEachDeleteToTheObjectItChecked(t *testing.T) {
	scheme := testScheme()
	dropped := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   "shop-old",
		UID:    "the-object-the-records-name",
		Labels: map[string]string{kipperlabels.Project: "shop"},
	}}
	// Declares nothing, so the namespace it still records is pruned.
	shop := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop"},
		Status:     kipperv1.ProjectStatus{Namespaces: []string{"shop-old"}},
	}

	pinned := map[string]types.UID{}
	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(dropped, shop).WithStatusSubresource(shop).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.DeleteOption) error {
				var options crclient.DeleteOptions
				options.ApplyOptions(opts)
				if options.Preconditions != nil && options.Preconditions.UID != nil {
					pinned[obj.GetName()] = *options.Preconditions.UID
				}
				return c.Delete(ctx, obj, opts...)
			},
		}).Build()
	r := &ProjectReconciler{Client: c, APIReader: c, Scheme: scheme}

	require.NoError(t, pruneRan(r.deleteProjectNamespaces(context.Background(), shop, true)))

	var ns corev1.Namespace
	require.True(t, errors.IsNotFound(c.Get(context.Background(), types.NamespacedName{Name: "shop-old"}, &ns)),
		"the namespace the project no longer declares was not pruned, so this test proves nothing about how it was deleted")
	assert.Equal(t, types.UID("the-object-the-records-name"), pinned["shop-old"],
		"the prune deleted a namespace by name, so a namespace recreated under that name between the check and the delete is destroyed on its predecessor's authorisation")
}
