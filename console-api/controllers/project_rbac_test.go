package controllers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func projectWithMembers(members ...kipperv1.ProjectMember) *kipperv1.Project {
	p := quotaTestProject("small", kipperv1.ProjectEnvironment{Name: "test"})
	p.Spec.Members = members
	return p
}

func TestMemberBindingsProjectMembershipOntoRBAC(t *testing.T) {
	project := projectWithMembers(
		kipperv1.ProjectMember{Email: "anna@example.com", Role: kipperv1.ProjectRoleOwner},
		kipperv1.ProjectMember{Email: "ben@example.com", Role: kipperv1.ProjectRoleDeployer},
		kipperv1.ProjectMember{Email: "carla@example.com", Role: kipperv1.ProjectRoleDeployer},
	)
	fakeClient := reconcileProject(t, project)

	var owner rbacv1.RoleBinding
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "kipper-project-owner", Namespace: "shop-test"}, &owner))
	assert.Equal(t, "ClusterRole", owner.RoleRef.Kind)
	assert.Equal(t, "kipper:project-owner", owner.RoleRef.Name)
	require.Len(t, owner.Subjects, 1)
	assert.Equal(t, "User", owner.Subjects[0].Kind)
	assert.Equal(t, "oidc:anna@example.com", owner.Subjects[0].Name,
		"subjects must carry the OIDC prefix: an unprefixed subject can never match an authenticated identity, and could collide with built-in names")

	var deployer rbacv1.RoleBinding
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "kipper-project-deployer", Namespace: "shop-test"}, &deployer))
	require.Len(t, deployer.Subjects, 2)
	assert.Equal(t, "oidc:ben@example.com", deployer.Subjects[0].Name)
	assert.Equal(t, "oidc:carla@example.com", deployer.Subjects[1].Name)

	// No viewers → no viewer binding at all, rather than one with an empty
	// subject list.
	var viewer rbacv1.RoleBinding
	err := fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "kipper-project-viewer", Namespace: "shop-test"}, &viewer)
	assert.True(t, errors.IsNotFound(err), "expected no viewer binding, got err=%v", err)
}

func TestMemberBindingsRemoveDepartedMembers(t *testing.T) {
	project := projectWithMembers(
		kipperv1.ProjectMember{Email: "anna@example.com", Role: kipperv1.ProjectRoleOwner},
		kipperv1.ProjectMember{Email: "ben@example.com", Role: kipperv1.ProjectRoleOwner},
	)
	fakeClient := reconcileProject(t, project)

	// Ben leaves the project; the next reconcile must drop his subject.
	var stored kipperv1.Project
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: "shop"}, &stored))
	stored.Spec.Members = []kipperv1.ProjectMember{{Email: "anna@example.com", Role: kipperv1.ProjectRoleOwner}}
	require.NoError(t, fakeClient.Update(context.Background(), &stored))

	r := &ProjectReconciler{Client: fakeClient, Scheme: testScheme(), APIReader: fakeClient}
	require.NoError(t, r.reconcileMemberBindings(context.Background(), &stored, "shop-test"))

	var owner rbacv1.RoleBinding
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "kipper-project-owner", Namespace: "shop-test"}, &owner))
	require.Len(t, owner.Subjects, 1)
	assert.Equal(t, "oidc:anna@example.com", owner.Subjects[0].Name)

	// The last member of a role leaving deletes the binding entirely.
	stored.Spec.Members = nil
	require.NoError(t, r.reconcileMemberBindings(context.Background(), &stored, "shop-test"))
	err := fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "kipper-project-owner", Namespace: "shop-test"}, &owner)
	assert.True(t, errors.IsNotFound(err), "expected the owner binding to be deleted, got err=%v", err)
}

func TestProjectNamespacesEnforcePodSecurityBaseline(t *testing.T) {
	fakeClient := reconcileProject(t, projectWithMembers())

	var ns corev1.Namespace
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: "shop-test"}, &ns))

	// Baseline enforcement closes the privileged/hostPath/hostNetwork pod
	// shapes that would let a namespace editor take the node; restricted
	// stays advisory until workloads converge.
	assert.Equal(t, "baseline", ns.Labels["pod-security.kubernetes.io/enforce"])
	assert.Equal(t, "restricted", ns.Labels["pod-security.kubernetes.io/warn"])
	assert.Equal(t, "restricted", ns.Labels["pod-security.kubernetes.io/audit"])
}

func TestMemberBindingsRepairRoleRefDrift(t *testing.T) {
	// A binding under a managed name referencing a different role (backup
	// restore, manual object) can never be updated in place: RoleRef is
	// immutable, and rewriting its subjects would grant members whatever it
	// happens to reference. The reconciler must replace it whole.
	drifted := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "kipper-project-owner", Namespace: "shop-test"},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     "kipper:project-viewer",
		},
		Subjects: []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: "User", Name: "oidc:stale@example.com"}},
	}
	project := projectWithMembers(
		kipperv1.ProjectMember{Email: "anna@example.com", Role: kipperv1.ProjectRoleOwner},
	)
	fakeClient := reconcileProject(t, project, drifted)

	var owner rbacv1.RoleBinding
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "kipper-project-owner", Namespace: "shop-test"}, &owner))
	assert.Equal(t, "kipper:project-owner", owner.RoleRef.Name, "the drifted RoleRef must be replaced")
	require.Len(t, owner.Subjects, 1)
	assert.Equal(t, "oidc:anna@example.com", owner.Subjects[0].Name)
}

func TestMemberBindingsTrimEmailWhitespace(t *testing.T) {
	project := projectWithMembers(
		kipperv1.ProjectMember{Email: "  anna@example.com ", Role: kipperv1.ProjectRoleOwner},
		kipperv1.ProjectMember{Email: "   ", Role: kipperv1.ProjectRoleViewer},
	)
	fakeClient := reconcileProject(t, project)

	var owner rbacv1.RoleBinding
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "kipper-project-owner", Namespace: "shop-test"}, &owner))
	require.Len(t, owner.Subjects, 1)
	assert.Equal(t, "oidc:anna@example.com", owner.Subjects[0].Name,
		"whitespace in a stored email must not produce an unmatchable subject")

	var viewer rbacv1.RoleBinding
	err := fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "kipper-project-viewer", Namespace: "shop-test"}, &viewer)
	assert.True(t, errors.IsNotFound(err), "a blank email must not become a subject or an empty binding")
}

func TestMapMemberBindingToProject(t *testing.T) {
	managed := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{
		Name:      "kipper-project-owner",
		Namespace: "shop-test",
		Labels:    map[string]string{"kipper.run/project": "shop"},
	}}
	reqs := memberBindingProjects(context.Background(), nil, managed)
	require.Len(t, reqs, 1)
	assert.Equal(t, "shop", reqs[0].Name)

	// Unmanaged bindings and unlabelled objects must not fan into project
	// reconciles.
	foreign := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{
		Name: "some-other-binding", Namespace: "shop-test",
		Labels: map[string]string{"kipper.run/project": "shop"},
	}}
	assert.Empty(t, memberBindingProjects(context.Background(), nil, foreign))

	unlabelled := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{
		Name: "kipper-project-owner", Namespace: "shop-test",
	}}
	assert.Empty(t, memberBindingProjects(context.Background(), nil, unlabelled))
}

// A role this build does not know grants nothing. The projection walks the
// three roles it knows and binds the members holding each, so a name outside
// that set matches none of them and the member appears in no binding.
//
// This is the direction that has to hold when a Project arrives from a restore,
// from kubectl, or from a migration off a cluster that had roles this one does
// not: less than the name says, never a silent grant of something else.
func TestAnUnrecognisedRoleBindsNobody(t *testing.T) {
	project := projectWithMembers(
		kipperv1.ProjectMember{Email: "anna@example.com", Role: kipperv1.ProjectRoleOwner},
		kipperv1.ProjectMember{Email: "stranger@example.com", Role: kipperv1.ProjectMemberRole("acme.support")},
	)
	fakeClient := reconcileProject(t, project)

	for _, role := range []string{"owner", "deployer", "viewer"} {
		var binding rbacv1.RoleBinding
		err := fakeClient.Get(context.Background(),
			types.NamespacedName{Name: "kipper-project-" + role, Namespace: "shop-test"}, &binding)
		if errors.IsNotFound(err) {
			continue
		}
		require.NoError(t, err)
		for _, s := range binding.Subjects {
			assert.NotEqual(t, "oidc:stranger@example.com", s.Name,
				"the member holding an unrecognised role was bound to %s, which grants them access nobody chose", role)
		}
	}
}

// Revocation must not depend on understanding what somebody held. A member
// removed from the project loses every binding whatever their role was, which
// matters most for the role nobody recognises: that is the one an operator is
// likeliest to be removing in a hurry.
func TestRemovingAMemberRevokesWhateverRoleTheyHeld(t *testing.T) {
	const strange = kipperv1.ProjectMemberRole("acme.support")
	project := projectWithMembers(
		kipperv1.ProjectMember{Email: "anna@example.com", Role: kipperv1.ProjectRoleOwner},
		kipperv1.ProjectMember{Email: "ben@example.com", Role: kipperv1.ProjectRoleDeployer},
		kipperv1.ProjectMember{Email: "stranger@example.com", Role: strange},
	)
	fakeClient := reconcileProject(t, project)

	var stored kipperv1.Project
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: "shop"}, &stored))

	// Ben's role changes to the unrecognised one, which must take his access
	// away rather than leave the deployer binding as it was.
	stored.Spec.Members = []kipperv1.ProjectMember{
		{Email: "anna@example.com", Role: kipperv1.ProjectRoleOwner},
		{Email: "ben@example.com", Role: strange},
	}
	require.NoError(t, fakeClient.Update(context.Background(), &stored))

	r := &ProjectReconciler{Client: fakeClient, Scheme: testScheme(), APIReader: fakeClient}
	require.NoError(t, r.reconcileMemberBindings(context.Background(), &stored, "shop-test"))

	var deployer rbacv1.RoleBinding
	err := fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "kipper-project-deployer", Namespace: "shop-test"}, &deployer)
	if !errors.IsNotFound(err) {
		require.NoError(t, err)
		for _, s := range deployer.Subjects {
			assert.NotEqual(t, "oidc:ben@example.com", s.Name,
				"moving a member to a role this build does not know left their old binding in place")
		}
	}

	// And the member who was only ever unrecognised is gone from the object
	// entirely, with nothing left behind.
	var owner rbacv1.RoleBinding
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "kipper-project-owner", Namespace: "shop-test"}, &owner))
	for _, s := range owner.Subjects {
		assert.NotEqual(t, "oidc:stranger@example.com", s.Name, "a departed member is still bound")
	}
}
