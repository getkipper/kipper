package controllers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	kipperlabels "github.com/getkipper/kipper/controller/pkg/labels"
	"github.com/getkipper/kipper/controller/pkg/memberbinding"
)

// Release 1 writes both generations of member binding into a namespace it owns.
//
// Alongside, never instead. A cluster running a mix of this build and the
// previous one has a controller in it that knows only the fixed names, so
// deleting those would have the older pod put them straight back and the two
// would fight. The fixed generation goes in a later release, once nothing
// writes it.
func TestBothBindingGenerationsAreWrittenInAnOwnedNamespace(t *testing.T) {
	project := projectWithMembers(
		kipperv1.ProjectMember{Email: "anna@example.com", Role: kipperv1.ProjectRoleOwner},
	)
	c := reconcileProject(t, project)

	var legacy rbacv1.RoleBinding
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: "kipper-project-owner", Namespace: "shop-test"}, &legacy),
		"the fixed-name binding is gone: an older controller in a rolling window would recreate it and the two would fight")
	assert.Equal(t, []string{"oidc:anna@example.com"}, subjectNames(legacy))

	generated := memberbinding.Name("shop", "owner")
	var current rbacv1.RoleBinding
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: generated, Namespace: "shop-test"}, &current),
		"no generated binding was written, so nothing carries a project digest and the prefix listing finds nothing")
	assert.Equal(t, []string{"oidc:anna@example.com"}, subjectNames(current))
	assert.Equal(t, "shop", current.Labels[kipperlabels.Project])
	assert.Equal(t, legacy.RoleRef, current.RoleRef,
		"the two generations grant different things, so which one applies depends on which build is reading")
}

// Both generations track membership. A member removed from the project has to
// lose both, or the older name keeps them in.
func TestRemovingAMemberEmptiesBothGenerations(t *testing.T) {
	project := projectWithMembers(
		kipperv1.ProjectMember{Email: "anna@example.com", Role: kipperv1.ProjectRoleOwner},
		kipperv1.ProjectMember{Email: "ben@example.com", Role: kipperv1.ProjectRoleOwner},
	)
	c := reconcileProject(t, project)

	var stored kipperv1.Project
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "shop"}, &stored))
	stored.Spec.Members = []kipperv1.ProjectMember{
		{Email: "anna@example.com", Role: kipperv1.ProjectRoleOwner},
	}
	require.NoError(t, c.Update(context.Background(), &stored))

	r := &ProjectReconciler{Client: c, Scheme: testScheme(), APIReader: c}
	require.NoError(t, r.reconcileMemberBindings(context.Background(), &stored, "shop-test"))

	for _, name := range []string{"kipper-project-owner", memberbinding.Name("shop", "owner")} {
		var binding rbacv1.RoleBinding
		require.NoError(t, c.Get(context.Background(),
			types.NamespacedName{Name: name, Namespace: "shop-test"}, &binding))
		assert.NotContains(t, subjectNames(binding), "oidc:ben@example.com",
			"a departed member is still in %s, so one generation revokes and the other does not", name)
	}
}

// The mapper runs backwards from an object to its Project, and a digest does
// not run backwards. It reads the label when it is there.
func TestTheMapperRoutesAGeneratedBindingByItsLabel(t *testing.T) {
	binding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{
		Name:      memberbinding.Name("shop", "owner"),
		Namespace: "shop-test",
		Labels:    map[string]string{kipperlabels.Project: "shop"},
	}}
	got := memberBindingProjects(context.Background(), nil, binding)
	require.Len(t, got, 1,
		"an out-of-band edit to a generated binding maps to no project, so nothing repairs it until an unrelated event")
	assert.Equal(t, "shop", got[0].Name)
}

// And when the label has drifted it generates rather than inverts: it walks the
// Projects it can see and matches each one's prefix against the name.
func TestTheMapperRoutesAGeneratedBindingWithNoLabelByGeneratingPrefixes(t *testing.T) {
	shop := &kipperv1.Project{ObjectMeta: metav1.ObjectMeta{Name: "shop"}}
	other := &kipperv1.Project{ObjectMeta: metav1.ObjectMeta{Name: "rival"}}
	c := projectFakeBuilder().WithScheme(testScheme()).WithObjects(shop, other).Build()

	binding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{
		Name:      memberbinding.Name("shop", "owner"),
		Namespace: "shop-test",
	}}

	got := memberBindingProjects(context.Background(), c, binding)
	require.Len(t, got, 1,
		"a generated binding whose label drifted maps to nothing, and its digest cannot be inverted, so only generating each project's prefix finds it")
	assert.Equal(t, "shop", got[0].Name)
}

// A fixed name carries no digest at all, so generating prefixes cannot find it
// either. The containing namespace's label is the last thing that says whose
// it is.
func TestTheMapperRoutesAFixedNameWithNoLabelByItsNamespace(t *testing.T) {
	shop := &kipperv1.Project{ObjectMeta: metav1.ObjectMeta{Name: "shop"}}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   "shop-test",
		Labels: map[string]string{kipperlabels.Project: "shop"},
	}}
	c := projectFakeBuilder().WithScheme(testScheme()).WithObjects(shop, ns).Build()

	binding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{
		Name:      "kipper-project-owner",
		Namespace: "shop-test",
	}}

	got := memberBindingProjects(context.Background(), c, binding)
	require.Len(t, got, 1,
		"a fixed-name binding stripped of its label is unmappable by label or digest, so the namespace is the only thing left and it was not read")
	assert.Equal(t, "shop", got[0].Name)
}

// Nothing else is routed. An object that is not a member binding must not
// enqueue a reconcile, or every RoleBinding write on the cluster wakes the
// project controller.
func TestTheMapperIgnoresWhatIsNotAMemberBinding(t *testing.T) {
	c := projectFakeBuilder().WithScheme(testScheme()).Build()
	binding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{
		Name:      "someone-elses-binding",
		Namespace: "shop-test",
		Labels:    map[string]string{kipperlabels.Project: "shop"},
	}}
	assert.Empty(t, memberBindingProjects(context.Background(), c, binding))
}

// A generated name carries a digest nothing can edit in place; a label can be
// rewritten by anyone who can write the object. Where the two disagree the name
// is the one to believe.
//
// Taking the label's word sent the binding to a project that cannot recognise
// the name, while the project that could was never woken. That is drift routed
// into silence, which is worse than drift left alone.
func TestTheMapperBelievesTheNameOverALabelThatDisagrees(t *testing.T) {
	shop := &kipperv1.Project{ObjectMeta: metav1.ObjectMeta{Name: "shop"}}
	rival := &kipperv1.Project{ObjectMeta: metav1.ObjectMeta{Name: "rival"}}
	c := projectFakeBuilder().WithScheme(testScheme()).WithObjects(shop, rival).Build()

	binding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{
		Name:      memberbinding.Name("shop", "owner"),
		Namespace: "shop-test",
		Labels:    map[string]string{kipperlabels.Project: "rival"},
	}}

	got := memberBindingProjects(context.Background(), c, binding)
	require.Len(t, got, 1)
	assert.Equal(t, "shop", got[0].Name,
		"the mapper routed a generated binding by a label that contradicts the digest in its own name, waking a project that cannot repair it and leaving the one that can asleep")
}

// A fixed name has no digest to check a label against, so there the label is
// all there is and is still used.
func TestTheMapperStillUsesTheLabelForAFixedName(t *testing.T) {
	c := projectFakeBuilder().WithScheme(testScheme()).Build()
	binding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{
		Name:      "kipper-project-owner",
		Namespace: "shop-test",
		Labels:    map[string]string{kipperlabels.Project: "shop"},
	}}
	got := memberBindingProjects(context.Background(), c, binding)
	require.Len(t, got, 1)
	assert.Equal(t, "shop", got[0].Name)
}
