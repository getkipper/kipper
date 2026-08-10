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
	reqs := mapMemberBindingToProject(context.Background(), managed)
	require.Len(t, reqs, 1)
	assert.Equal(t, "shop", reqs[0].Name)

	// Unmanaged bindings and unlabelled objects must not fan into project
	// reconciles.
	foreign := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{
		Name: "some-other-binding", Namespace: "shop-test",
		Labels: map[string]string{"kipper.run/project": "shop"},
	}}
	assert.Empty(t, mapMemberBindingToProject(context.Background(), foreign))

	unlabelled := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{
		Name: "kipper-project-owner", Namespace: "shop-test",
	}}
	assert.Empty(t, mapMemberBindingToProject(context.Background(), unlabelled))
}
