package controllers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	kipperlabels "github.com/getkipper/kipper/controller/pkg/labels"
	"github.com/getkipper/kipper/controller/pkg/memberbinding"
)

// Revocation has to reach a namespace the reconcile will not grant in.
//
// The ordinary path refuses a namespace whose ownership it cannot prove and
// skips it, which is right for granting and wrong for taking away: a member
// removed from the project keeps whatever binding is already sitting there,
// and the namespace losing its label is precisely the state nobody is
// watching. So a second pass runs that can only remove.
//
// It is a separate pass rather than a reordering of the first. Running the
// grant-and-revoke reconcile earlier would let a project write its members
// into a namespace it does not own, and the later ownership refusal would
// leave that grant standing.

// memberBinding builds a managed member binding as the reconciler writes it.
func memberBinding(namespace, project string, emails ...string) *rbacv1.RoleBinding {
	subjects := make([]rbacv1.Subject, 0, len(emails))
	for _, e := range emails {
		subjects = append(subjects, rbacv1.Subject{
			APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: "oidc:" + e,
		})
	}
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kipper-project-owner",
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "kipper",
				kipperlabels.Project:           project,
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "kipper:project-owner",
		},
		Subjects: subjects,
	}
}

func revokeFixture(t *testing.T, project *kipperv1.Project, objs ...crclient.Object) crclient.Client {
	t.Helper()
	all := append([]crclient.Object{project}, objs...)
	return crfake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(all...).
		WithStatusSubresource(&kipperv1.Project{}).
		Build()
}

func subjectNames(b rbacv1.RoleBinding) []string {
	out := make([]string, 0, len(b.Subjects))
	for _, s := range b.Subjects {
		out = append(out, s.Name)
	}
	return out
}

// The security requirement: a member removed from the project loses access
// even in a namespace the reconcile refuses to touch.
func TestRevokeReachesANamespaceWhoseLabelIsGone(t *testing.T) {
	project := quotaTestProject("small", kipperv1.ProjectEnvironment{Name: "test"})
	project.Spec.Members = []kipperv1.ProjectMember{
		{Email: "anna@example.com", Role: kipperv1.ProjectRoleOwner},
	}
	c := revokeFixture(t, project,
		// The namespace lost its project label, so the ordinary reconcile will
		// not go near it. The binding this project wrote is still there, and
		// still carries ben, who has left.
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop-test"}},
		memberBinding("shop-test", "shop", "anna@example.com", "ben@example.com"),
	)

	r := &ProjectReconciler{Client: c, Scheme: testScheme(), APIReader: c}
	require.NoError(t, r.revokeStaleMemberBindings(context.Background(), project))

	var binding rbacv1.RoleBinding
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: "kipper-project-owner", Namespace: "shop-test"}, &binding))
	assert.NotContains(t, subjectNames(binding), "oidc:ben@example.com",
		"a member removed from the project kept their binding in a namespace whose label had gone")
	assert.Contains(t, subjectNames(binding), "oidc:anna@example.com",
		"revocation took away a member the project still lists")
}

// The other half, and the reason this is a separate pass: it may only remove.
// A member added to the project must not gain access in a namespace the
// project cannot prove is its own.
func TestRevokeNeverGrantsInANamespaceTheProjectCannotProve(t *testing.T) {
	project := quotaTestProject("small", kipperv1.ProjectEnvironment{Name: "test"})
	project.Spec.Members = []kipperv1.ProjectMember{
		{Email: "anna@example.com", Role: kipperv1.ProjectRoleOwner},
		{Email: "newcomer@example.com", Role: kipperv1.ProjectRoleOwner},
	}
	c := revokeFixture(t, project,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop-test"}},
		memberBinding("shop-test", "shop", "anna@example.com"),
	)

	r := &ProjectReconciler{Client: c, Scheme: testScheme(), APIReader: c}
	require.NoError(t, r.revokeStaleMemberBindings(context.Background(), project))

	var binding rbacv1.RoleBinding
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: "kipper-project-owner", Namespace: "shop-test"}, &binding))
	assert.NotContains(t, subjectNames(binding), "oidc:newcomer@example.com",
		"the revoke pass added a member to a namespace the project cannot prove it owns, which is the grant it exists to avoid")
}

// A binding left with nobody goes, rather than sitting there empty.
func TestRevokeDeletesABindingItEmpties(t *testing.T) {
	project := quotaTestProject("small", kipperv1.ProjectEnvironment{Name: "test"})
	project.Spec.Members = nil
	c := revokeFixture(t, project,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop-test"}},
		memberBinding("shop-test", "shop", "ben@example.com"),
	)

	r := &ProjectReconciler{Client: c, Scheme: testScheme(), APIReader: c}
	require.NoError(t, r.revokeStaleMemberBindings(context.Background(), project))

	var binding rbacv1.RoleBinding
	err := c.Get(context.Background(),
		types.NamespacedName{Name: "kipper-project-owner", Namespace: "shop-test"}, &binding)
	assert.True(t, apierrors.IsNotFound(err),
		"a binding whose every subject was revoked is still there, holding a RoleRef and an empty subject list")
}

// It touches nothing belonging to another project. The pass is scoped to
// bindings carrying this project's own label, and another tenant's binding
// carries theirs.
func TestRevokeLeavesAnotherProjectsBindingsAlone(t *testing.T) {
	project := quotaTestProject("small", kipperv1.ProjectEnvironment{Name: "test"})
	project.Spec.Members = nil
	c := revokeFixture(t, project,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "rival-prod"}},
		memberBinding("rival-prod", "rival", "rival@example.com"),
	)

	r := &ProjectReconciler{Client: c, Scheme: testScheme(), APIReader: c}
	require.NoError(t, r.revokeStaleMemberBindings(context.Background(), project))

	var binding rbacv1.RoleBinding
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: "kipper-project-owner", Namespace: "rival-prod"}, &binding),
		"another project's binding was deleted by this project's revoke pass")
	assert.Contains(t, subjectNames(binding), "oidc:rival@example.com")
}

// A RoleRef is immutable in Kubernetes, so a binding pointing at the wrong role
// cannot be corrected in place. The pass must not try: it deletes rather than
// edits, and it never writes a RoleRef at all.
func TestRevokeDoesNotRewriteARoleRef(t *testing.T) {
	project := quotaTestProject("small", kipperv1.ProjectEnvironment{Name: "test"})
	project.Spec.Members = []kipperv1.ProjectMember{
		{Email: "anna@example.com", Role: kipperv1.ProjectRoleOwner},
	}
	drifted := memberBinding("shop-test", "shop", "anna@example.com", "ben@example.com")
	drifted.RoleRef.Name = "kipper:project-viewer"

	c := revokeFixture(t, project,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop-test"}},
		drifted,
	)

	r := &ProjectReconciler{Client: c, Scheme: testScheme(), APIReader: c}
	require.NoError(t, r.revokeStaleMemberBindings(context.Background(), project))

	var binding rbacv1.RoleBinding
	if err := c.Get(context.Background(),
		types.NamespacedName{Name: "kipper-project-owner", Namespace: "shop-test"}, &binding); err == nil {
		assert.Equal(t, "kipper:project-viewer", binding.RoleRef.Name,
			"the revoke pass rewrote a RoleRef, which Kubernetes rejects and which is not its job")
		assert.NotContains(t, subjectNames(binding), "oidc:ben@example.com")
	}
}

// The pass records nothing. Writing the namespace into status would widen the
// Project finalizer's deletion backstop onto a namespace whose ownership was
// never proven, which is the change an earlier revision of the plan had to
// withdraw for making deletion destructive.
func TestRevokeRecordsNothingInStatus(t *testing.T) {
	project := quotaTestProject("small", kipperv1.ProjectEnvironment{Name: "test"})
	project.Spec.Members = nil
	c := revokeFixture(t, project,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop-test"}},
		memberBinding("shop-test", "shop", "ben@example.com"),
	)

	r := &ProjectReconciler{Client: c, Scheme: testScheme(), APIReader: c}
	require.NoError(t, r.revokeStaleMemberBindings(context.Background(), project))

	assert.Empty(t, project.Status.Namespaces,
		"the revoke pass recorded a namespace it only ever removed from, which would hand it to the deletion backstop")
}

// The two tests the plan asks for, through a whole reconcile rather than the
// pass on its own. Calling the pass directly proves it works; this proves the
// reconcile actually calls it, which is the part that was missing.
func TestAFullReconcileRevokesInANamespaceItRefusesToGrantIn(t *testing.T) {
	project := quotaTestProject("small", kipperv1.ProjectEnvironment{Name: "test"})
	project.Spec.Members = []kipperv1.ProjectMember{
		{Email: "anna@example.com", Role: kipperv1.ProjectRoleOwner},
	}
	c := crfake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(
			project,
			// Unlabelled, so reconcileNamespace refuses it and the grant path
			// skips the namespace entirely.
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop-test"}},
			memberBinding("shop-test", "shop", "anna@example.com", "ben@example.com"),
			nodeWithIP("worker-1", "ExternalIP", "203.0.113.9"),
		).
		WithStatusSubresource(&kipperv1.Project{}).
		Build()

	reconcileNamed(t, c, "shop")

	var binding rbacv1.RoleBinding
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: "kipper-project-owner", Namespace: "shop-test"}, &binding))
	assert.NotContains(t, subjectNames(binding), "oidc:ben@example.com",
		"a whole reconcile left a departed member bound in a namespace it refused to grant in, so the revoke pass is not wired in")

	// And it granted nothing there: the namespace is still unowned, so the
	// member who is in the project gained no new binding beyond the one that
	// was already there.
	var stored kipperv1.Project
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "shop"}, &stored))
	assert.NotContains(t, stored.Status.Namespaces, "shop-test",
		"the refused namespace was recorded in status, which hands it to the deletion backstop")
}

func TestAFullReconcileDoesNotGrantANewMemberInARefusedNamespace(t *testing.T) {
	project := quotaTestProject("small", kipperv1.ProjectEnvironment{Name: "test"})
	project.Spec.Members = []kipperv1.ProjectMember{
		{Email: "anna@example.com", Role: kipperv1.ProjectRoleOwner},
		{Email: "newcomer@example.com", Role: kipperv1.ProjectRoleOwner},
	}
	c := crfake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(
			project,
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop-test"}},
			memberBinding("shop-test", "shop", "anna@example.com"),
			nodeWithIP("worker-1", "ExternalIP", "203.0.113.9"),
		).
		WithStatusSubresource(&kipperv1.Project{}).
		Build()

	reconcileNamed(t, c, "shop")

	var binding rbacv1.RoleBinding
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: "kipper-project-owner", Namespace: "shop-test"}, &binding))
	assert.NotContains(t, subjectNames(binding), "oidc:newcomer@example.com",
		"a whole reconcile granted a new member in a namespace whose ownership the project cannot prove")
}

// The revoke pass has to cover both generations. One that recognised only the
// fixed names would take a departed member out of the old binding and leave
// them in the new one, which is the same access under a different name.
func TestRevokeCoversTheGeneratedGenerationToo(t *testing.T) {
	project := quotaTestProject("small", kipperv1.ProjectEnvironment{Name: "test"})
	project.Spec.Members = []kipperv1.ProjectMember{
		{Email: "anna@example.com", Role: kipperv1.ProjectRoleOwner},
	}

	generated := memberBinding("shop-test", "shop", "anna@example.com", "ben@example.com")
	generated.Name = memberbinding.Name("shop", "owner")

	c := revokeFixture(t, project,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop-test"}},
		generated,
	)

	r := &ProjectReconciler{Client: c, Scheme: testScheme(), APIReader: c}
	require.NoError(t, r.revokeStaleMemberBindings(context.Background(), project))

	var binding rbacv1.RoleBinding
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: memberbinding.Name("shop", "owner"), Namespace: "shop-test"}, &binding))
	assert.NotContains(t, subjectNames(binding), "oidc:ben@example.com",
		"a departed member is still bound under the generated name, so revoking the fixed one only moved their access")
}

// The generated name exists to carry a project identity that survives label
// drift, and the revoke pass was selecting on the label alone. So a binding
// whose label went was invisible to the one pass that exists to reach exactly
// that state, while the watch mapper could still route it.
func TestRevokeFindsAGeneratedBindingWhoseLabelIsGone(t *testing.T) {
	project := quotaTestProject("small", kipperv1.ProjectEnvironment{Name: "test"})
	project.Spec.Members = []kipperv1.ProjectMember{
		{Email: "anna@example.com", Role: kipperv1.ProjectRoleOwner},
	}

	stripped := memberBinding("shop-test", "shop", "anna@example.com", "ben@example.com")
	stripped.Name = memberbinding.Name("shop", "owner")
	delete(stripped.Labels, kipperlabels.Project)

	c := revokeFixture(t, project,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop-test"}},
		stripped,
	)

	r := &ProjectReconciler{Client: c, Scheme: testScheme(), APIReader: c}
	require.NoError(t, r.revokeStaleMemberBindings(context.Background(), project))

	var binding rbacv1.RoleBinding
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: memberbinding.Name("shop", "owner"), Namespace: "shop-test"}, &binding))
	assert.NotContains(t, subjectNames(binding), "oidc:ben@example.com",
		"a departed member kept access under a generated binding whose label had drifted, which is the state the digest in the name exists to survive")
}

// An RBAC subject is a tuple, not a name. Comparing names alone kept a Group
// subject that happened to share a member's name, and both usernames and groups
// carry the oidc: prefix on this cluster, so that is one drifted field away
// from granting everyone in a group what the project granted one person.
func TestRevokeRemovesASubjectThatOnlySharesAGrantedName(t *testing.T) {
	project := quotaTestProject("small", kipperv1.ProjectEnvironment{Name: "test"})
	project.Spec.Members = []kipperv1.ProjectMember{
		{Email: "anna@example.com", Role: kipperv1.ProjectRoleOwner},
	}

	drifted := memberBinding("shop-test", "shop")
	drifted.Subjects = []rbacv1.Subject{
		{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: "oidc:anna@example.com"},
		{APIGroup: rbacv1.GroupName, Kind: rbacv1.GroupKind, Name: "oidc:anna@example.com"},
	}

	c := revokeFixture(t, project,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop-test"}},
		drifted,
	)

	r := &ProjectReconciler{Client: c, Scheme: testScheme(), APIReader: c}
	require.NoError(t, r.revokeStaleMemberBindings(context.Background(), project))

	var binding rbacv1.RoleBinding
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: "kipper-project-owner", Namespace: "shop-test"}, &binding))

	require.Len(t, binding.Subjects, 1,
		"a Group subject sharing a member's name survived, so everyone in that group holds what the project granted one person")
	assert.Equal(t, rbacv1.UserKind, binding.Subjects[0].Kind)
}
