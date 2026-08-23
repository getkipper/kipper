package controllers

import (
	"context"
	goerrors "errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	kipperlabels "github.com/getkipper/kipper/controller/pkg/labels"
)

// Deleting a namespace cannot be taken back, so every input to that decision is
// read from the API server rather than from the manager's cache.
//
// A cache is per replica and behind by an unbounded amount, and the objects this
// decision reads arrive on separate watches with no ordering between them. Every
// fixture elsewhere wires both readers to one store, which is right for an
// ordinary reconcile and hides exactly this: with the two agreeing, the reads
// could all come from the cache and nothing would notice. These give them
// different answers, and the API server's is the one that must win.
// A rival's claim published moments ago is in the API server and not yet in the
// cache, while the relabel that made this project a candidate is already
// cached. Reading rivals from the cache deletes the rival's live namespace.
func TestPruningReadsRivalClaimsFromTheAPIServer(t *testing.T) {
	scheme := testScheme()
	contested := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   "shop-prod",
		UID:    "the-live-object",
		Labels: map[string]string{kipperlabels.Project: "shop"},
	}}
	shop := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop"},
		Status:     kipperv1.ProjectStatus{Namespaces: []string{"shop-prod"}},
	}
	grocerWithout := &kipperv1.Project{ObjectMeta: metav1.ObjectMeta{Name: "grocer"}}
	grocerWith := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "grocer"},
		Status: kipperv1.ProjectStatus{
			NamespaceClaims: []kipperv1.NamespaceClaim{{Name: "shop-prod", UID: "the-live-object"}},
		},
	}

	cached := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(contested, shop, grocerWithout).WithStatusSubresource(shop, grocerWithout).Build()
	live := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(contested, shop, grocerWith).WithStatusSubresource(shop, grocerWith).Build()

	r := &ProjectReconciler{Client: cached, APIReader: live, Scheme: scheme}
	require.NoError(t, pruneRan(r.deleteProjectNamespaces(context.Background(), shop, false)))

	var ns corev1.Namespace
	require.NoError(t, cached.Get(context.Background(), types.NamespacedName{Name: "shop-prod"}, &ns),
		"the rival's claim was published before this ran and only the cache had not seen it, so a live namespace belonging to another project was destroyed")
}

// The spec that decides what to keep must be current too. An environment
// removed and added straight back leaves one replica holding the intermediate
// spec, and the namespace it would prune is this project's own — so the rival
// check, which skips this project by name, cannot save it.
func TestPruningReadsTheKeepSetFromTheAPIServer(t *testing.T) {
	scheme := testScheme()
	held := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   "shop-prod",
		UID:    "the-live-object",
		Labels: map[string]string{kipperlabels.Project: "shop"},
	}}
	// The cache saw prod removed.
	stale := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop"},
		Status:     kipperv1.ProjectStatus{Namespaces: []string{"shop-prod"}},
	}
	// The API server has it back.
	current := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop"},
		Spec:       kipperv1.ProjectSpec{Environments: []kipperv1.ProjectEnvironment{{Name: "prod"}}},
		Status:     kipperv1.ProjectStatus{Namespaces: []string{"shop-prod"}},
	}

	cached := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(held, stale).WithStatusSubresource(stale).Build()
	live := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(held, current).WithStatusSubresource(current).Build()

	r := &ProjectReconciler{Client: cached, APIReader: live, Scheme: scheme}
	// Pruning, with the keep set derived inside from whichever project is read.
	require.NoError(t, pruneRan(r.deleteProjectNamespaces(context.Background(), stale, true)))

	var ns corev1.Namespace
	require.NoError(t, cached.Get(context.Background(), types.NamespacedName{Name: "shop-prod"}, &ns),
		"a namespace the project still declares was pruned from a spec only one replica's cache believed")
}

// The claim names the object the pass proved, which is the object its own write
// landed on. Naming it from a fresh read of the name claims whatever carries the
// name at that moment: a namespace deleted and recreated in between is claimed
// without ever passing the label or the claimable check, and the claim then
// refuses it to everyone, including whoever created it.
func TestTheClaimNamesTheObjectThePassProved(t *testing.T) {
	scheme := testScheme()
	proved := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   "shop-test",
		UID:    "the-object-this-pass-proved",
		Labels: map[string]string{kipperlabels.Project: "shop", kipperLabel: kipperValue},
	}}
	project := &kipperv1.Project{ObjectMeta: metav1.ObjectMeta{Name: "shop"}}

	store := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(proved, project).Build()
	// Every namespace read after the one the pass decides from finds a
	// different object standing in the name, which is the recreate this has to
	// survive.
	reads := 0
	live := interceptor.NewClient(store, interceptor.Funcs{
		Get: func(ctx context.Context, c crclient.WithWatch, key crclient.ObjectKey, obj crclient.Object, opts ...crclient.GetOption) error {
			if err := c.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			if ns, ok := obj.(*corev1.Namespace); ok {
				reads++
				if reads > 1 {
					ns.UID = "a-namespace-recreated-since"
				}
			}
			return nil
		},
	})

	r := &ProjectReconciler{Client: store, APIReader: live, Scheme: scheme}
	uid, err := r.reconcileNamespace(context.Background(), project, "shop-test", "test", []string{"test"}, 0)
	require.NoError(t, err)

	assert.Equal(t, types.UID("the-object-this-pass-proved"), uid,
		"the claim named whatever held the name after the pass proved its namespace, rather than the object it proved")
}

// A project that has gone between the start of the pass and the delete takes
// nothing with it.
func TestPruningStopsWhenTheProjectHasGone(t *testing.T) {
	scheme := testScheme()
	orphan := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   "shop-prod",
		UID:    "the-live-object",
		Labels: map[string]string{kipperlabels.Project: "shop"},
	}}
	stale := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop"},
		Status:     kipperv1.ProjectStatus{Namespaces: []string{"shop-prod"}},
	}
	cached := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(orphan, stale).WithStatusSubresource(stale).Build()
	live := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(orphan).Build()

	r := &ProjectReconciler{Client: cached, APIReader: live, Scheme: scheme}
	require.NoError(t, pruneRan(r.deleteProjectNamespaces(context.Background(), stale, false)))

	var ns corev1.Namespace
	err := cached.Get(context.Background(), types.NamespacedName{Name: "shop-prod"}, &ns)
	assert.False(t, errors.IsNotFound(err),
		"a project the API server no longer has still deleted a namespace, from a copy only the cache held")
}

// A project deleted and recreated under the same name is a different object,
// which is the rule this branch applies everywhere else, including on the two
// other delete paths: both bind their delete to the UID they read.
//
// The re-read here is a Get by name. Without comparing what came back, a pass
// that began against a terminating project can find its successor standing in
// the same name, read that successor's own records, and delete the namespaces
// those records authorise. The UID precondition on each namespace does not
// help: the namespaces really are the live ones, checked against the very
// records that say they may be taken. The re-read hands the deletion its
// victim's own credentials.
func TestPruningStopsWhenTheProjectWasReplacedUnderItsName(t *testing.T) {
	scheme := testScheme()
	live := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   "shop-prod",
		UID:    "the-successors-namespace",
		Labels: map[string]string{kipperlabels.Project: "shop"},
	}}
	// The pass began against this one, which has since finished deleting.
	gone := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop", UID: "the-project-this-pass-is-about"},
		Status:     kipperv1.ProjectStatus{Namespaces: []string{"shop-prod"}},
	}
	// A different project now stands in the name, holding its own namespace.
	successor := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop", UID: "a-different-project"},
		Status: kipperv1.ProjectStatus{
			Namespaces:      []string{"shop-prod"},
			NamespaceClaims: []kipperv1.NamespaceClaim{{Name: "shop-prod", UID: "the-successors-namespace"}},
		},
	}

	cached := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(live, gone).WithStatusSubresource(gone).Build()
	apiServer := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(live, successor).WithStatusSubresource(successor).Build()

	r := &ProjectReconciler{Client: cached, APIReader: apiServer, Scheme: scheme}
	_, err := r.deleteProjectNamespaces(context.Background(), gone, false)
	require.ErrorIs(t, err, errProjectReplaced,
		"a pass that has proven its project replaced carried on to the revoke sweep and the status write, both of which act on the dead project's spec")

	var ns corev1.Namespace
	require.NoError(t, cached.Get(context.Background(), types.NamespacedName{Name: "shop-prod"}, &ns),
		"a pass about a project that has finished deleting read its successor's records and deleted the successor's live namespace")
}

// The candidate list is delete-deciding, so it is read live. A cached list can
// hold a namespace whose label has since moved to somebody else, and the UID
// precondition does not cover a relabel: the object is the same one, and what
// changed is who it answers to.
//
// The earlier tests put the contested namespace in both stores, so they do not
// separate the two readers for this read. This one does.
func TestPruningReadsTheCandidateListFromTheAPIServer(t *testing.T) {
	scheme := testScheme()
	ours := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "shop-prod", UID: "the-object",
		Labels: map[string]string{kipperlabels.Project: "shop"},
	}}
	// The API server has the same object, labelled for somebody else.
	theirs := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "shop-prod", UID: "the-object",
		Labels: map[string]string{kipperlabels.Project: "grocer"},
	}}
	shop := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop"},
		Status:     kipperv1.ProjectStatus{Namespaces: []string{"shop-prod"}},
	}

	cached := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(ours, shop).WithStatusSubresource(shop).Build()
	apiServer := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(theirs, shop).WithStatusSubresource(shop).Build()

	r := &ProjectReconciler{Client: cached, APIReader: apiServer, Scheme: scheme}
	require.NoError(t, pruneRan(r.deleteProjectNamespaces(context.Background(), shop, false)))

	// The delete writes through Client, so the cached store is where a wrong
	// answer shows up.
	var ns corev1.Namespace
	require.NoError(t, cached.Get(context.Background(), types.NamespacedName{Name: "shop-prod"}, &ns),
		"the label had already moved away at the API server and only the cache still showed it here, so a namespace that is no longer this project's was deleted")
}

// The unlabelled backstop reads the object it is about to collect, and reads it
// live for the same reason: a cached copy that still shows no label collects a
// namespace whose live label names whoever holds it now.
func TestTheBackstopReadsTheObjectFromTheAPIServer(t *testing.T) {
	scheme := testScheme()
	unlabelled := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "shop-prod", UID: "the-object",
	}}
	nowTheirs := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "shop-prod", UID: "the-object",
		Labels: map[string]string{kipperlabels.Project: "grocer"},
	}}
	shop := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop"},
		Status: kipperv1.ProjectStatus{
			Namespaces:      []string{"shop-prod"},
			NamespaceClaims: []kipperv1.NamespaceClaim{{Name: "shop-prod", UID: "the-object"}},
		},
	}

	cached := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(unlabelled, shop).WithStatusSubresource(shop).Build()
	apiServer := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(nowTheirs, shop).WithStatusSubresource(shop).Build()

	r := &ProjectReconciler{Client: cached, APIReader: apiServer, Scheme: scheme}
	require.NoError(t, pruneRan(r.deleteProjectNamespaces(context.Background(), shop, false)))

	var ns corev1.Namespace
	require.NoError(t, cached.Get(context.Background(), types.NamespacedName{Name: "shop-prod"}, &ns),
		"the backstop collected a namespace that carries another project's label at the API server, because the cache still showed it unlabelled")
}

// Adoption is an ownership decision, so the rivals it checks are read live: a
// cached list can be missing the claim that says somebody already holds this.
func TestAdoptionReadsRivalsFromTheAPIServer(t *testing.T) {
	scheme := testScheme()
	contested := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "shop-prod", UID: "the-object",
		Labels: map[string]string{kipperlabels.Project: "shop"},
	}}
	shop := &kipperv1.Project{ObjectMeta: metav1.ObjectMeta{Name: "shop"}}
	grocerWithout := &kipperv1.Project{ObjectMeta: metav1.ObjectMeta{Name: "grocer"}}
	grocerWith := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "grocer"},
		Status: kipperv1.ProjectStatus{
			NamespaceClaims: []kipperv1.NamespaceClaim{{Name: "shop-prod", UID: "the-object"}},
		},
	}

	cached := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(contested, shop, grocerWithout).WithStatusSubresource(shop, grocerWithout).Build()
	apiServer := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(contested, shop, grocerWith).WithStatusSubresource(shop, grocerWith).Build()

	r := &ProjectReconciler{Client: cached, APIReader: apiServer, Scheme: scheme}

	err := r.claimable(context.Background(), shop, contested)

	var conflict *namespaceConflictError
	require.ErrorAs(t, err, &conflict,
		"a namespace another project had already claimed was offered for adoption, because only the cache had not seen that claim")
}

// Publishing a claim replaces the pass's copy with the live one before writing,
// so the write lands on whatever carries the name. A project deleted and
// recreated under it would take the old pass's claim as its own, and a claim
// authorises deletion downstream. Same binding as the delete path.
func TestPublishingAClaimStopsWhenTheProjectWasReplacedUnderItsName(t *testing.T) {
	scheme := testScheme()
	// The cluster holds the successor. The pass is about its predecessor, which
	// has finished deleting and is no longer anywhere.
	successor := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop", UID: "a-different-project"},
	}
	gone := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop", UID: "the-project-this-pass-is-about"},
	}
	store := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(successor).WithStatusSubresource(successor).Build()

	r := &ProjectReconciler{Client: store, APIReader: store, Scheme: scheme}
	claims := []kipperv1.NamespaceClaim{{Name: "shop-test", UID: "the-object"}}
	require.ErrorIs(t, r.publishClaim(context.Background(), gone, "shop-test", claims), errProjectReplaced,
		"refusing the claim and returning success left the pass to write the dead project's quota and member bindings into the successor's namespaces")

	var stored kipperv1.Project
	require.NoError(t, store.Get(context.Background(), types.NamespacedName{Name: "shop"}, &stored))
	assert.Empty(t, stored.Status.NamespaceClaims,
		"a pass about a project that has gone wrote its claim into the successor standing in that name, and a claim is what authorises deleting the namespace")
}

// The claim is written against the status that is actually there, so the read
// that starts it does not go through the cache.
//
// One store here, not two: this call reads with one reader and writes with the
// other, so two stores would only prove which store got written. The cached
// reader is made to fail instead, and the call has to succeed anyway.
func TestPublishingAClaimDoesNotStartFromTheCache(t *testing.T) {
	scheme := testScheme()
	shop := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop", UID: "the-project"},
		Status: kipperv1.ProjectStatus{
			NamespaceClaims: []kipperv1.NamespaceClaim{{Name: "shop-test", UID: "the-first-object"}},
		},
	}
	store := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(shop).WithStatusSubresource(shop).Build()
	refusing := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(shop).WithStatusSubresource(shop).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, crclient.WithWatch, crclient.ObjectKey, crclient.Object, ...crclient.GetOption) error {
				return goerrors.New("the cache must not be the source for this read")
			},
		}).Build()

	r := &ProjectReconciler{Client: refusing, APIReader: store, Scheme: scheme}
	claims := []kipperv1.NamespaceClaim{{Name: "shop-prod", UID: "the-second-object"}}

	require.NoError(t, r.publishClaim(context.Background(), shop, "shop-prod", claims),
		"the status a claim is written onto was read through the cache")
}

// The claim retention drops a claim whose namespace is gone, and asks the API
// server whether it is: a cache that has not caught up answers NotFound for a
// namespace that is still there, and that claim is the strongest evidence of
// ownership the project has.
func TestClaimRetentionAsksTheAPIServerWhetherTheNamespaceIsGone(t *testing.T) {
	scheme := testScheme()
	stillThere := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "shop-prod", UID: "the-object",
		Labels: map[string]string{kipperlabels.Project: "shop"},
	}}
	shop := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop", UID: "the-project"},
		Spec:       kipperv1.ProjectSpec{Environments: []kipperv1.ProjectEnvironment{{Name: "prod"}}},
		Status: kipperv1.ProjectStatus{
			NamespaceClaims: []kipperv1.NamespaceClaim{{Name: "shop-prod", UID: "the-object"}},
		},
	}
	// The cache has never seen the namespace.
	cached := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(shop).WithStatusSubresource(shop).Build()
	apiServer := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(stillThere, shop).WithStatusSubresource(shop).Build()

	r := &ProjectReconciler{Client: cached, APIReader: apiServer, Scheme: scheme}

	kept := r.keepLiveClaims(context.Background(), shop, nil, []string{"prod"})

	require.Len(t, kept, 1,
		"a claim was dropped because the cache had not seen its namespace, and that claim is what says the project holds it")
	assert.Equal(t, types.UID("the-object"), kept[0].UID)
}

// And the older namespace record, for the same reason: on a cluster upgrading
// from a build that wrote no claims it is the only evidence there is.
func TestNamespaceRecordRetentionAsksTheAPIServer(t *testing.T) {
	scheme := testScheme()
	stillThere := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "shop-prod", UID: "the-object",
		Labels: map[string]string{kipperlabels.Project: "shop"},
	}}
	shop := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop", UID: "the-project"},
		Status:     kipperv1.ProjectStatus{Namespaces: []string{"shop-prod"}},
	}
	cached := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(shop).WithStatusSubresource(shop).Build()
	apiServer := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(stillThere, shop).WithStatusSubresource(shop).Build()

	r := &ProjectReconciler{Client: cached, APIReader: apiServer, Scheme: scheme}

	kept := r.keepLiveNamespaces(context.Background(), []string{"shop-prod"}, nil, []string{"prod"}, "shop")

	assert.Equal(t, []string{"shop-prod"}, kept,
		"the record was erased because the cache had not seen the namespace, and on a pre-claims cluster that record is the only evidence the project holds it")
}

// Ownership is decided from what the object says, so the namespace that decides
// it is read from the API server too. A cached copy can carry a label that has
// since moved to another project, and adopting on the strength of it relabels
// the namespace back and hands this project's members RoleBindings over
// whatever is running there.
func TestAdoptionReadsTheNamespaceFromTheAPIServer(t *testing.T) {
	scheme := testScheme()
	shop := &kipperv1.Project{ObjectMeta: metav1.ObjectMeta{Name: "shop"}}

	// The cache still shows the namespace as this project's.
	stale := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   "shop-prod",
		UID:    "the-live-object",
		Labels: map[string]string{kipperlabels.Project: "shop"},
	}}
	// The API server has it answering to somebody else.
	current := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   "shop-prod",
		UID:    "the-live-object",
		Labels: map[string]string{kipperlabels.Project: "grocer"},
	}}

	cached := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(stale, shop).Build()
	live := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(current, shop).Build()

	r := &ProjectReconciler{Client: cached, APIReader: live, Scheme: scheme}
	_, err := r.reconcileNamespace(context.Background(), shop, "shop-prod", "prod", []string{"prod"}, 0)

	var conflict *namespaceConflictError
	require.ErrorAs(t, err, &conflict,
		"a namespace whose label had already moved to another project was adopted, from a copy only one replica's cache still held")
	assert.Equal(t, "grocer", conflict.owner)
}

// The set both ownership records are pruned by is the project's spec, and it
// has to be as current as everything else this pass decides ownership from.
//
// Publishing a claim mid-pass syncs the live resourceVersion onto the pass's
// copy, which is what lets the end-of-pass write land even though the spec has
// moved on: the conflict that used to stop a stale spec reaching that write no
// longer fires. So a pass one watch event behind erases the record and the
// claim to a namespace that is standing and still declared, and those records
// are what adoption and the deletion backstop both read.
func TestTheRecordsArePrunedByTheSpecTheAPIServerHas(t *testing.T) {
	scheme := testScheme()
	held := func(name, env string) *corev1.Namespace {
		return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			UID:    types.UID("uid-" + name),
			Labels: map[string]string{kipperlabels.Project: "shop", kipperlabels.Environment: env},
		}}
	}
	// What the pass reads: prod has been taken out of the spec.
	stale := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop", Finalizers: []string{projectFinalizer}},
		Spec:       kipperv1.ProjectSpec{Environments: []kipperv1.ProjectEnvironment{{Name: "test"}}},
		Status: kipperv1.ProjectStatus{
			Namespaces:      []string{"shop-prod"},
			NamespaceClaims: []kipperv1.NamespaceClaim{{Name: "shop-prod", UID: "uid-shop-prod"}},
		},
	}
	// What the API server has: it was put straight back, and the namespace
	// never went anywhere.
	current := stale.DeepCopy()
	current.Spec.Environments = append(current.Spec.Environments, kipperv1.ProjectEnvironment{Name: "prod"})

	store := func(project *kipperv1.Project) crclient.WithWatch {
		return projectFakeBuilder().WithScheme(scheme).
			WithObjects(project, held("shop-test", "test"), held("shop-prod", "prod"),
				nodeWithIP("worker-1", "ExternalIP", "203.0.113.9")).
			WithStatusSubresource(&kipperv1.Project{}).
			Build()
	}
	cached, live := store(stale), store(current)

	// shop-test is reconciled and has no claim yet, so this pass writes one,
	// and that write is what syncs the resourceVersion.
	r := &ProjectReconciler{Client: cached, APIReader: live, Scheme: scheme}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "shop"}})
	require.NoError(t, err)

	var stored kipperv1.Project
	require.NoError(t, cached.Get(context.Background(), types.NamespacedName{Name: "shop"}, &stored))
	assert.Contains(t, stored.Status.Namespaces, "shop-prod",
		"the record of a live namespace the project declares was erased by a pass working from a spec only one replica's cache believed")
	_, claimed := namespaceClaimFor(t, cached, "shop", "shop-prod")
	assert.True(t, claimed,
		"the claim to a live namespace the project declares was erased by a pass working from a spec only one replica's cache believed")
}

// A pass creates namespaces and writes their egress policy before it reaches
// any other guard, so the incarnation is confirmed before that rather than at
// the claim.
//
// claimable skips every project sharing this one's name, so a successor
// standing in the name is never a rival and the stale pass is free to create a
// namespace the successor never declared. Nothing collects it afterwards: the
// pass ends before it records anything, and cleanup reaches a labelled
// namespace only while the records still name it.
func TestAReplacedProjectCreatesNoNamespace(t *testing.T) {
	scheme := testScheme()
	// The pass reads this one, and it declares an environment the successor
	// knows nothing about.
	gone := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "shop",
			UID:        "the-project-this-pass-is-about",
			Finalizers: []string{projectFinalizer},
		},
		Spec: kipperv1.ProjectSpec{Environments: []kipperv1.ProjectEnvironment{{Name: "legacy"}}},
	}
	successor := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "shop",
			UID:        "a-different-project",
			Finalizers: []string{projectFinalizer},
		},
		Spec: kipperv1.ProjectSpec{Environments: []kipperv1.ProjectEnvironment{{Name: "prod"}}},
	}

	cached := projectFakeBuilder().WithScheme(scheme).
		WithObjects(gone, nodeWithIP("worker-1", "ExternalIP", "203.0.113.9")).
		WithStatusSubresource(&kipperv1.Project{}).Build()
	live := projectFakeBuilder().WithScheme(scheme).
		WithObjects(successor, nodeWithIP("worker-1", "ExternalIP", "203.0.113.9")).
		WithStatusSubresource(&kipperv1.Project{}).Build()

	r := &ProjectReconciler{Client: cached, APIReader: live, Scheme: scheme}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "shop"}})
	require.ErrorIs(t, err, errProjectReplaced)

	var ns corev1.Namespace
	err = cached.Get(context.Background(), types.NamespacedName{Name: "shop-legacy"}, &ns)
	assert.True(t, errors.IsNotFound(err),
		"a pass about a project that has finished deleting created a namespace for it, and ended before recording anything that could collect it again")
}

// One spec decides both what is deleted and what stays recorded.
//
// Reading the project twice let a spec edit land in between, and the two
// answers then disagreed: the delete kept a namespace by the earlier spec and
// the record prune dropped it by the later one. The namespace stands with
// nothing naming it, and cleanup reaches a labelled namespace only while the
// records still name it, so it is stranded for good.
//
// One store, because the prune deletes through the cached client and reads
// through the API reader, and two stores would let this pass whichever spec the
// records were pruned by. The pass reaches only shop-prod, because a namespace
// this pass held is kept whatever the spec says and only a recorded one it did
// not reach can show the disagreement.
func TestThePruneAndTheRecordsAgreeOnOneSpec(t *testing.T) {
	scheme := testScheme()
	held := func(name, env string) *corev1.Namespace {
		return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			UID:    types.UID("uid-" + name),
			Labels: map[string]string{kipperlabels.Project: "shop", kipperlabels.Environment: env},
		}}
	}
	// The API server declares both; the cache below has lost dev, so the loop
	// never reaches shop-dev.
	shop := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop", UID: "shop", Finalizers: []string{projectFinalizer}},
		Spec: kipperv1.ProjectSpec{Environments: []kipperv1.ProjectEnvironment{
			{Name: "dev"}, {Name: "prod"},
		}},
		Status: kipperv1.ProjectStatus{
			Namespaces: []string{"shop-dev", "shop-prod"},
			NamespaceClaims: []kipperv1.NamespaceClaim{
				{Name: "shop-dev", UID: "uid-shop-dev"},
				{Name: "shop-prod", UID: "uid-shop-prod"},
			},
		},
	}
	store := projectFakeBuilder().WithScheme(scheme).
		WithObjects(shop, held("shop-dev", "dev"), held("shop-prod", "prod"),
			nodeWithIP("worker-1", "ExternalIP", "203.0.113.9")).
		WithStatusSubresource(&kipperv1.Project{}).Build()

	// The pass reads the project live three times: once to confirm the
	// incarnation, once inside publishClaim for the one environment it reaches,
	// and once inside the prune, which is the last of them and the one both
	// decisions rest on. Anything read after that finds dev gone, which is the
	// edit landing mid-pass.
	reads := 0
	reader := interceptor.NewClient(store, interceptor.Funcs{
		Get: func(ctx context.Context, c crclient.WithWatch, key crclient.ObjectKey, obj crclient.Object, opts ...crclient.GetOption) error {
			if err := c.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			p, ok := obj.(*kipperv1.Project)
			if !ok {
				return nil
			}
			reads++
			if reads > 3 {
				p.Spec.Environments = []kipperv1.ProjectEnvironment{{Name: "prod"}}
			}
			return nil
		},
	})

	// The same store seen through the manager cache, which is one watch event
	// behind and has lost dev.
	cached := interceptor.NewClient(store, interceptor.Funcs{
		Get: func(ctx context.Context, c crclient.WithWatch, key crclient.ObjectKey, obj crclient.Object, opts ...crclient.GetOption) error {
			if err := c.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			if p, ok := obj.(*kipperv1.Project); ok {
				p.Spec.Environments = []kipperv1.ProjectEnvironment{{Name: "prod"}}
			}
			return nil
		},
	})
	r := &ProjectReconciler{Client: cached, APIReader: reader, Scheme: scheme}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "shop"}})
	require.NoError(t, err)

	var ns corev1.Namespace
	require.NoError(t, store.Get(context.Background(), types.NamespacedName{Name: "shop-dev"}, &ns),
		"the prune deleted a namespace the spec it read still declared")

	var stored kipperv1.Project
	require.NoError(t, store.Get(context.Background(), types.NamespacedName{Name: "shop"}, &stored))
	assert.Contains(t, stored.Status.Namespaces, "shop-dev",
		"the record of a standing namespace was pruned by a spec read after the one the prune decided from, which strands it for good")
	_, claimed := namespaceClaimFor(t, store, "shop", "shop-dev")
	assert.True(t, claimed,
		"the claim to a standing namespace was pruned by a spec read after the one the prune decided from")

	assert.Equal(t, 3, reads,
		"the pass reads the project a different number of times than this test assumes, so it is no longer injecting the edit where it means to")
}

// A project that finishes deleting mid-pass ends the pass rather than pruning
// the records from a spec nobody has.
//
// Two reconcilers overlap for the length of every console-api rollout: the
// installer deploys one replica with the default rolling update, which surges to
// two, and there is no leader election. So one pod can complete the deletion
// while the other is still inside the loop. What the early return skips is the
// revoke sweep and the status write, both of which are about a project that no
// longer exists.
func TestAPassEndsWhenTheProjectFinishesDeletingUnderIt(t *testing.T) {
	scheme := testScheme()
	held := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   "shop-prod",
		UID:    "uid-shop-prod",
		Labels: map[string]string{kipperlabels.Project: "shop", kipperlabels.Environment: "prod"},
	}}
	// Still in this replica's cache, already gone from the API server.
	shop := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop", UID: "shop", Finalizers: []string{projectFinalizer}},
		Spec:       kipperv1.ProjectSpec{Environments: []kipperv1.ProjectEnvironment{{Name: "prod"}}},
		Status: kipperv1.ProjectStatus{
			Namespaces:      []string{"shop-prod"},
			NamespaceClaims: []kipperv1.NamespaceClaim{{Name: "shop-prod", UID: "uid-shop-prod"}},
		},
	}

	cached := projectFakeBuilder().WithScheme(scheme).
		WithObjects(shop, held, nodeWithIP("worker-1", "ExternalIP", "203.0.113.9")).
		WithStatusSubresource(&kipperv1.Project{}).Build()
	// The project is gone here; its namespace is not, because the pass that
	// removed the finalizer is still working through them.
	live := projectFakeBuilder().WithScheme(scheme).
		WithObjects(held, nodeWithIP("worker-1", "ExternalIP", "203.0.113.9")).
		WithStatusSubresource(&kipperv1.Project{}).Build()

	r := &ProjectReconciler{Client: cached, APIReader: live, Scheme: scheme}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "shop"}})
	require.NoError(t, err,
		"a project that finished deleting mid-pass was treated as a failure the pass should retry")
}

// And a project that goes after the pass has confirmed it still reaches the
// prune, which then has no spec to prune the records by. The pass ends there:
// deriving a declared set from a nil project is a panic, and deriving an empty
// one would erase the records of every namespace the project still holds.
func TestAPassEndsWhenTheProjectGoesBeforeThePrune(t *testing.T) {
	scheme := testScheme()
	// Unlabelled, so the loop refuses it and reaches the prune without ever
	// publishing a claim, which is the only route to the prune that does not
	// read the project again on the way.
	unlabelled := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "shop-test",
		UID:  "somebody-elses",
	}}
	shop := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop", UID: "shop", Finalizers: []string{projectFinalizer}},
		Status: kipperv1.ProjectStatus{
			Namespaces:      []string{"shop-test"},
			NamespaceClaims: []kipperv1.NamespaceClaim{{Name: "shop-test", UID: "somebody-elses"}},
		},
	}

	store := projectFakeBuilder().WithScheme(scheme).
		WithObjects(shop, unlabelled, nodeWithIP("worker-1", "ExternalIP", "203.0.113.9")).
		WithStatusSubresource(&kipperv1.Project{}).Build()
	// The other replica finishes the deletion once this pass has confirmed the
	// project and is working through its namespaces.
	reads := 0
	live := interceptor.NewClient(store, interceptor.Funcs{
		Get: func(ctx context.Context, c crclient.WithWatch, key crclient.ObjectKey, obj crclient.Object, opts ...crclient.GetOption) error {
			if _, ok := obj.(*kipperv1.Project); ok {
				reads++
				if reads > 1 {
					return errors.NewNotFound(kipperv1.GroupVersion.WithResource("projects").GroupResource(), key.Name)
				}
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})

	r := &ProjectReconciler{Client: store, APIReader: live, Scheme: scheme}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "shop"}})
	require.NoError(t, err,
		"a project that finished deleting after the pass had confirmed it was treated as a failure the pass should retry")
	require.Greater(t, reads, 1, "the prune never re-read the project, so this test never reached the branch it is about")
}

// The deletion path answers a vanished project the same way the pruning path
// does. Another replica can complete the cleanup and reap the project while this
// one still holds a cached copy carrying the deletion timestamp, and taking the
// finalizer off an object that is already gone reports a failure for something
// that has worked.
//
// One store, and the cache is an interceptor over it rather than a second store,
// because the finalizer write goes through the cached client: with two stores
// that write lands in the one that still has the project and the pass succeeds
// whether or not it checked.
func TestTheDeletionPathEndsWhenTheProjectHasAlreadyGone(t *testing.T) {
	scheme := testScheme()
	deleting := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "shop",
			UID:               "shop",
			Finalizers:        []string{projectFinalizer},
			DeletionTimestamp: &metav1.Time{Time: metav1.Now().Time},
		},
	}

	// The API server has already reaped it.
	store := projectFakeBuilder().WithScheme(scheme).
		WithStatusSubresource(&kipperv1.Project{}).Build()
	cached := interceptor.NewClient(store, interceptor.Funcs{
		Get: func(ctx context.Context, c crclient.WithWatch, key crclient.ObjectKey, obj crclient.Object, opts ...crclient.GetOption) error {
			if p, ok := obj.(*kipperv1.Project); ok && key.Name == "shop" {
				deleting.DeepCopyInto(p)
				return nil
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})

	r := &ProjectReconciler{Client: cached, APIReader: store, Scheme: scheme}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "shop"}})
	require.NoError(t, err,
		"the pass took the finalizer off a project that had already gone and reported the failure as a reconcile error")
}
