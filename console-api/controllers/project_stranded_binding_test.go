package controllers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/memberbinding"
)

// The revoke pass has to reach a binding after everything mutable about it has
// gone: the label off the binding, the label off the namespace, the environment
// out of the spec, and the namespace out of status.
//
// That combination is the whole reason the generated name exists. The name
// carries a digest of the project and cannot be edited in place, so the binding
// is still attributable when nothing else about it is. Deriving candidate
// namespaces from the spec and from status could not see this one, so a member
// removed from the project kept owner access to whatever was running there,
// indefinitely and silently.
func TestARemovedMemberLosesABindingStrandedWithNoLabelAndNoRecord(t *testing.T) {
	project := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop"},
		Spec: kipperv1.ProjectSpec{
			Members: []kipperv1.ProjectMember{
				{Email: "anna@example.com", Role: kipperv1.ProjectRoleOwner},
			},
		},
	}

	// Alice was an owner. Her binding sits in a namespace this project no
	// longer declares and never recorded, and it carries no project label.
	stranded := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      memberbinding.Name("shop", "owner"),
			Namespace: "shop-old",
		},
		RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: memberClusterRoles[kipperv1.ProjectRoleOwner]},
		Subjects: []rbacv1.Subject{
			{APIGroup: rbacv1.GroupName, Kind: "User", Name: "oidc:alice@example.com"},
		},
	}

	c := projectFakeBuilder().
		WithScheme(testScheme()).
		WithObjects(project, stranded).
		WithStatusSubresource(&kipperv1.Project{}).
		Build()

	r := &ProjectReconciler{Client: c, Scheme: testScheme(), APIReader: c}
	require.NoError(t, r.revokeStaleMemberBindings(context.Background(), project))

	var after rbacv1.RoleBinding
	err := c.Get(context.Background(), types.NamespacedName{Name: stranded.Name, Namespace: "shop-old"}, &after)
	if apierrors.IsNotFound(err) {
		return // emptied to nothing and deleted, which is the desired end
	}
	require.NoError(t, err)
	assert.Empty(t, after.Subjects,
		"a departed member still holds owner access in %s through a binding nothing but its own name can attribute", stranded.Namespace)
}

// The index answers for one project only. A prefix that could match another
// project's bindings would let this pass edit a tenant it has no business in.
func TestTheIndexDoesNotHandOverAnotherProjectsBinding(t *testing.T) {
	rival := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{
		Name:      memberbinding.Name("rival", "owner"),
		Namespace: "rival-prod",
	}}
	keys := MemberBindingProjectKeys(rival)
	require.Len(t, keys, 1)
	assert.NotEqual(t, memberbinding.Prefix("shop"), keys[0],
		"one project's binding indexes under another's prefix, so the revoke pass reaches into a tenant it does not own")
	assert.Equal(t, memberbinding.Prefix("rival"), keys[0])
}

// Anything that is not a member binding stays out of the index entirely, so the
// list stays proportional to the bindings this feature wrote.
func TestTheIndexIgnoresBindingsThisFeatureDidNotWrite(t *testing.T) {
	assert.Empty(t, MemberBindingProjectKeys(&rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "somebody-elses-binding", Namespace: "shop-prod"},
	}))
	assert.Empty(t, MemberBindingProjectKeys(&rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "kipper-project-owner", Namespace: "shop-prod"},
	}), "the fixed legacy name carries no digest, so indexing it under a prefix would be a guess")
}
