package controllers

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	kipperlabels "github.com/getkipper/kipper/controller/pkg/labels"
)

// A namespace whose project label has gone is reported and never repaired.
//
// Repairing it looks like the kind thing to do and is the dangerous one. The
// label is the only thing saying whose namespace it is, so a reconcile that
// wrote the label back would be deciding ownership from the project's own
// wish to have it. Two projects can resolve to one namespace name, so the
// first to reconcile would take whatever is running there and hand it to its
// own members.
//
// So it stays unlabelled, the Project says why, and a person decides.
func unlabelledFixture(t *testing.T, claimants ...*kipperv1.Project) crclient.Client {
	t.Helper()
	objs := []crclient.Object{
		// The namespace project shop's "test" environment resolves to, with
		// its label stripped and somebody else's workload inside.
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop-test"}},
	}
	for _, p := range claimants {
		objs = append(objs, p)
	}
	objs = append(objs, nodeWithIP("worker-1", "ExternalIP", "203.0.113.9"))

	return crfake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(objs...).
		WithStatusSubresource(&kipperv1.Project{}).
		Build()
}

func reconcileNamed(t *testing.T, c crclient.Client, name string) {
	t.Helper()
	r := &ProjectReconciler{Client: c, Scheme: testScheme(), APIReader: c}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
	require.NoError(t, err)
}

func TestAnUnlabelledNamespaceIsNeverAdoptedOrRelabelled(t *testing.T) {
	shop := quotaTestProject("small", kipperv1.ProjectEnvironment{Name: "test"})
	shop.Spec.Members = []kipperv1.ProjectMember{
		{Email: "anna@example.com", Role: kipperv1.ProjectRoleOwner},
	}
	// A second project whose default environment resolves to the same name.
	// Two claimants, because one project reconciling repeatedly proves less
	// than two of them racing for the same namespace.
	other := quotaTestProject("small")
	other.Name = "shop-test"
	other.Spec.Members = []kipperv1.ProjectMember{
		{Email: "rival@example.com", Role: kipperv1.ProjectRoleOwner},
	}

	c := unlabelledFixture(t, shop, other)

	for i := 0; i < 3; i++ {
		reconcileNamed(t, c, "shop")
		reconcileNamed(t, c, "shop-test")
	}

	var ns corev1.Namespace
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "shop-test"}, &ns))
	if owner, ok := ns.Labels[kipperlabels.Project]; ok {
		t.Errorf("the namespace was relabelled for %q after six reconciles: whichever project wrote it now owns whatever was already running there", owner)
	}

	// Adoption is the other half. A project that cannot prove the namespace is
	// its own must not put its members' bindings inside it.
	for _, role := range []string{"owner", "deployer", "viewer"} {
		var binding rbacv1.RoleBinding
		err := c.Get(context.Background(),
			types.NamespacedName{Name: "kipper-project-" + role, Namespace: "shop-test"}, &binding)
		if apierrors.IsNotFound(err) {
			continue
		}
		require.NoError(t, err)
		t.Errorf("a %s binding was written into an unlabelled namespace, so its contents are now readable by a project that never owned it", role)
	}
}

// The condition has to say which of the two things happened. "Another project
// owns it" and "nothing says who owns it" need different repairs: the first is
// a rename, the second is a person deciding whose the namespace is and
// labelling it, or deleting it.
//
// They shared a reason, so an operator filtering on it was told the wrong
// story for the case where nothing owns the namespace at all.
func TestTheConditionSaysTheLabelIsMissingRatherThanThatAnotherProjectOwnsIt(t *testing.T) {
	shop := quotaTestProject("small", kipperv1.ProjectEnvironment{Name: "test"})
	c := unlabelledFixture(t, shop)
	reconcileNamed(t, c, "shop")

	var stored kipperv1.Project
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "shop"}, &stored))

	cond := apimeta.FindStatusCondition(stored.Status.Conditions, conditionNamespaceConflict)
	require.NotNil(t, cond, "an unlabelled namespace was refused and the Project says nothing about it")
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, reasonNamespaceNotLabelled, cond.Reason,
		"an unlabelled namespace reports the reason used for a namespace another project owns, which sends an operator to the wrong repair")
	assert.Contains(t, cond.Message, "shop-test")
}

// And the reason for a genuine collision keeps saying that, so splitting the
// two did not collapse them the other way.
func TestTheConditionStillNamesTheOwnerWhenAnotherProjectHasIt(t *testing.T) {
	shop := quotaTestProject("small", kipperv1.ProjectEnvironment{Name: "test"})
	owned := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   "shop-test",
		Labels: map[string]string{kipperlabels.Project: "someone-else"},
	}}
	c := crfake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(shop, owned, nodeWithIP("worker-1", "ExternalIP", "203.0.113.9")).
		WithStatusSubresource(&kipperv1.Project{}).
		Build()
	reconcileNamed(t, c, "shop")

	var stored kipperv1.Project
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "shop"}, &stored))

	cond := apimeta.FindStatusCondition(stored.Status.Conditions, conditionNamespaceConflict)
	require.NotNil(t, cond)
	assert.Equal(t, reasonNamespaceOwnedByAnotherProject, cond.Reason)
	assert.True(t, strings.Contains(cond.Message, "someone-else"),
		"the message does not name the project holding the namespace, which is the one fact needed to fix it")
}
