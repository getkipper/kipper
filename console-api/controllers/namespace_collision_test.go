package controllers

import (
	"context"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	kipperlabels "github.com/getkipper/kipper/controller/pkg/labels"
)

// Namespace names are not unique across projects: "shop" with an environment
// "prod" and "shop-prod" with a default environment both resolve to "shop-prod".
// Adopting a namespace another project owns relabels it and puts this project's
// member RoleBindings in it, so both projects' members end up with access to one
// namespace and to whatever runs there. It has to be refused.
func TestANamespaceAnotherProjectOwnsIsNotAdopted(t *testing.T) {
	scheme := testScheme()
	claimed := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   "shop-prod",
		Labels: map[string]string{kipperlabels.Project: "shop", kipperlabels.Environment: "prod"},
	}}
	claimant := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-prod"},
		Spec:       kipperv1.ProjectSpec{Environments: []kipperv1.ProjectEnvironment{{Name: "default"}}},
	}

	client := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(claimed, claimant).Build()
	c := client
	r := &ProjectReconciler{
		Client:    c,
		APIReader: c,
	}

	_, err := r.reconcileNamespace(context.Background(), claimant, "shop-prod", "default", []string{"default"}, 0)
	require.Error(t, err, "a namespace another project owns must not be adopted")

	var conflict *namespaceConflictError
	require.ErrorAs(t, err, &conflict)
	assert.Equal(t, "shop", conflict.owner)
	assert.Equal(t, "shop-prod", conflict.claimant)

	// The namespace is left exactly as it was: still the other project's.
	var after corev1.Namespace
	require.NoError(t, client.Get(context.Background(), types.NamespacedName{Name: "shop-prod"}, &after))
	assert.Equal(t, "shop", after.Labels[kipperlabels.Project],
		"the label was overwritten, so the other project's namespace now answers to this one")
}

// A project reconciling its own namespace again is the ordinary case and must
// keep working, including when the namespace carries no project label yet.
func TestAProjectStillReconcilesItsOwnNamespace(t *testing.T) {
	scheme := testScheme()
	project := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop"},
		Spec:       kipperv1.ProjectSpec{Environments: []kipperv1.ProjectEnvironment{{Name: "default"}}},
	}

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "shop", Labels: map[string]string{kipperlabels.Project: "shop"}}}
	client := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(ns, project).Build()
	c := client
	r := &ProjectReconciler{
		Client:    c,
		APIReader: c,
	}

	_, err := r.reconcileNamespace(context.Background(), project, "shop", "default", []string{"default"}, 0)
	require.NoError(t, err)

	var after corev1.Namespace
	require.NoError(t, client.Get(context.Background(), types.NamespacedName{Name: "shop"}, &after))
	assert.Equal(t, "shop", after.Labels[kipperlabels.Project])
}

// A namespace Kipper did not create is not free for the taking. An absent
// ownership label means somebody else made it, and adopting it would hand this
// project's members RoleBindings over whatever is already running there.
func TestAnUnlabelledNamespaceIsNotAdopted(t *testing.T) {
	scheme := testScheme()
	project := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop"},
		Spec:       kipperv1.ProjectSpec{Environments: []kipperv1.ProjectEnvironment{{Name: "default"}}},
	}
	// Made with kubectl, and it happens to match a new project's name.
	foreign := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop"}}

	client := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(foreign, project).Build()
	c := client
	r := &ProjectReconciler{
		Client:    c,
		APIReader: c,
	}

	_, err := r.reconcileNamespace(context.Background(), project, "shop", "default", []string{"default"}, 0)
	require.Error(t, err, "a namespace Kipper did not create must not be adopted")
	assert.Contains(t, err.Error(), "not created by Kipper")

	var after corev1.Namespace
	require.NoError(t, client.Get(context.Background(), types.NamespacedName{Name: "shop"}, &after))
	assert.Empty(t, after.Labels[kipperlabels.Project],
		"the namespace was claimed, so its workloads are now visible to this project's members")
}

// A namespace whose Kipper label is gone is invisible to the label query that
// project deletion runs on, so nothing collects it: the Project is
// cluster-scoped, and no owner reference reaches down to a namespace, its
// workloads, or the member RoleBindings installed in it. Status is the record
// that this project held it as its own, and deletion falls back to that.
func TestDeletingAProjectCollectsANamespaceThatLostItsLabel(t *testing.T) {
	scheme := testScheme()
	stripped := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop-prod"}}
	labelled := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "shop-test", Labels: map[string]string{kipperlabels.Project: "shop"}}}
	project := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop"},
		Status:     kipperv1.ProjectStatus{Namespaces: []string{"shop-prod", "shop-test"}},
	}

	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(stripped, labelled, project).WithStatusSubresource(project).Build()
	r := &ProjectReconciler{
		Client:    c,
		APIReader: c,
		Scheme:    scheme,
	}
	require.NoError(t, pruneRan(r.deleteProjectNamespaces(context.Background(), project, false)))

	var ns corev1.Namespace
	err := r.Get(context.Background(), types.NamespacedName{Name: "shop-prod"}, &ns)
	assert.True(t, errors.IsNotFound(err),
		"a namespace this project created and recorded must be collected even with its label gone")
	err = r.Get(context.Background(), types.NamespacedName{Name: "shop-test"}, &ns)
	assert.True(t, errors.IsNotFound(err), "the labelled namespace goes too")
}

// The same fallback must not reach into a namespace that now answers to
// somebody else. A project whose status still names it has a stale record, and
// acting on that would delete a live namespace out from under the project that
// legitimately holds it.
func TestDeletingAProjectLeavesANamespaceAnotherProjectNowOwns(t *testing.T) {
	scheme := testScheme()
	reassigned := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "shop-prod", Labels: map[string]string{kipperlabels.Project: "retail"}}}
	project := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop"},
		Status:     kipperv1.ProjectStatus{Namespaces: []string{"shop-prod"}},
	}

	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(reassigned, project).WithStatusSubresource(project).Build()
	r := &ProjectReconciler{
		Client:    c,
		APIReader: c,
		Scheme:    scheme,
	}
	require.NoError(t, pruneRan(r.deleteProjectNamespaces(context.Background(), project, false)))

	var ns corev1.Namespace
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "shop-prod"}, &ns),
		"a namespace labelled for another project must survive this project's deletion")
	assert.Equal(t, "retail", ns.Labels[kipperlabels.Project])
}

// A project that declares no environments still gets one, so a check reading
// only what a project declares is blind to the namespace its default already
// occupies. That was the same drift the ResolveNamespace extraction set out to
// end, one rule further up.
func TestProjectEnvironmentsIncludesTheOneTheReconcilerAdds(t *testing.T) {
	declared := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop"},
		Spec:       kipperv1.ProjectSpec{Environments: []kipperv1.ProjectEnvironment{{Name: "prod"}}},
	}
	assert.Equal(t, []kipperv1.ProjectEnvironment{{Name: "prod"}}, ProjectEnvironments(declared))

	silent := &kipperv1.Project{ObjectMeta: metav1.ObjectMeta{Name: "shop"}}
	assert.Equal(t, []kipperv1.ProjectEnvironment{{Name: DefaultEnvironmentName}}, ProjectEnvironments(silent),
		"a project declaring nothing still resolves to one namespace, and everything guarding names has to see it")
	assert.Equal(t, "shop-test", ResolveNamespace(silent.Name, ProjectEnvironments(silent)[0].Name))
}

// Reporting one conflict at a time turns a project with two colliding
// environments into a rename, a wait, and the next surprise.
func TestEveryConflictedNamespaceIsReported(t *testing.T) {
	scheme := testScheme()
	project := &kipperv1.Project{ObjectMeta: metav1.ObjectMeta{Name: "shop"}}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(project).WithStatusSubresource(project).Build()
	r := &ProjectReconciler{
		Client:    c,
		APIReader: c,
		Scheme:    scheme,
	}

	r.setNamespaceConflictCondition(project, []*namespaceConflictError{
		{namespace: "shop-prod", owner: "retail", claimant: "shop"},
		{namespace: "shop-staging", owner: "retail", claimant: "shop"},
	})

	cond := apimeta.FindStatusCondition(project.Status.Conditions, conditionNamespaceConflict)
	require.NotNil(t, cond)
	assert.Contains(t, cond.Message, "shop-prod")
	assert.Contains(t, cond.Message, "shop-staging", "the second collision must be reported alongside the first")

	r.setNamespaceConflictCondition(project, nil)
	assert.Nil(t, apimeta.FindStatusCondition(project.Status.Conditions, conditionNamespaceConflict),
		"the condition goes when the collisions do")
}

// A project's environment decides two names: the namespace it runs in and the
// subdomain its apps serve on. They have to agree about which environment is
// the default one, or the cluster serves one hostname while every surface that
// reads the environment back reports another.
//
// It did not agree. The namespace label was worked back out of the namespace
// name by stripping "<project>-", which a default environment has no suffix to
// strip — so project "shop" running in namespace "shop" was labelled
// environment "shop", and its app's host came out "web-shop" while the console,
// reading the declared name, said "web-default".
func TestTheNamespaceAndTheHostAgreeAboutTheDefaultEnvironment(t *testing.T) {
	tests := []struct {
		env        string
		wantNS     string
		wantPrefix string
	}{
		{"", "shop", "web"},
		{"default", "shop", "web"},
		{"prod", "shop-prod", "web-prod"},
		// The environment a project gets when it declares none is named, not
		// default, and is suffixed like any other.
		{DefaultEnvironmentName, "shop-test", "web-test"},
	}
	for _, tt := range tests {
		t.Run("env="+tt.env, func(t *testing.T) {
			assert.Equal(t, tt.wantNS, ResolveNamespace("shop", tt.env))
			assert.Equal(t, tt.wantPrefix, AppHostPrefix("web", tt.env))
		})
	}
}

// The label is the environment the project declares, not a guess made from the
// namespace name. Everything that builds a hostname reads it back.
func TestTheNamespaceIsLabelledWithTheDeclaredEnvironment(t *testing.T) {
	scheme := testScheme()
	project := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop"},
		Spec:       kipperv1.ProjectSpec{Environments: []kipperv1.ProjectEnvironment{{Name: "default"}}},
	}
	client := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(project).Build()
	c := client
	r := &ProjectReconciler{
		Client:    c,
		APIReader: c,
	}

	_, err := r.reconcileNamespace(context.Background(), project, "shop", "default", []string{"default"}, 0)
	require.NoError(t, err)

	var ns corev1.Namespace
	require.NoError(t, client.Get(context.Background(), types.NamespacedName{Name: "shop"}, &ns))
	assert.Equal(t, "default", ns.Labels[kipperlabels.Environment],
		"a default environment's namespace was labelled with the project's name, so its apps served on the wrong host")
}

// Pruning decides what to delete, so it must not guess. Both "" and "default"
// resolve to the bare project name, and working the environment back out of the
// namespace could only pick one of them — picking the other deleted a live
// namespace and everything in it.
func TestPruningKeepsTheDefaultNamespaceWhicheverWayItIsSpelled(t *testing.T) {
	for _, env := range []string{"", "default"} {
		t.Run("declared as "+strconv.Quote(env), func(t *testing.T) {
			scheme := testScheme()
			bare := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
				Name: "shop", Labels: map[string]string{kipperlabels.Project: "shop"}}}
			stale := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
				Name: "shop-old", Labels: map[string]string{kipperlabels.Project: "shop"}}}
			project := &kipperv1.Project{
				ObjectMeta: metav1.ObjectMeta{Name: "shop"},
				Spec:       kipperv1.ProjectSpec{Environments: []kipperv1.ProjectEnvironment{{Name: env}}},
				Status:     kipperv1.ProjectStatus{Namespaces: []string{"shop", "shop-old"}},
			}
			c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(bare, stale, project).WithStatusSubresource(project).Build()
			r := &ProjectReconciler{
				Client:    c,
				APIReader: c,
				Scheme:    scheme,
			}

			require.NoError(t, pruneRan(r.deleteProjectNamespaces(context.Background(), project, true)))

			var ns corev1.Namespace
			require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "shop"}, &ns),
				"the namespace this environment resolves to must survive its own pruning")
			err := r.Get(context.Background(), types.NamespacedName{Name: "shop-old"}, &ns)
			assert.True(t, errors.IsNotFound(err), "an environment no longer declared is still pruned")
		})
	}
}
