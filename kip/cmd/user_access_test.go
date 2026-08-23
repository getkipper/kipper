package cmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

func projectScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(projectGVR.GroupVersion().WithKind("Project"), &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(projectGVR.GroupVersion().WithKind("ProjectList"), &unstructured.UnstructuredList{})
	return scheme
}

func projectWithMembers(members ...map[string]any) *unstructured.Unstructured {
	list := make([]any, 0, len(members))
	for _, m := range members {
		list = append(list, m)
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kipper.run/v1alpha1",
		"kind":       "Project",
		"metadata":   map[string]any{"name": "shop"},
		"spec":       map[string]any{"members": list},
	}}
}

func adminBinding(subjects ...string) *rbacv1.ClusterRoleBinding {
	binding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: initialAdminBindingName},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-admin"},
	}
	for _, s := range subjects {
		binding.Subjects = append(binding.Subjects, rbacv1.Subject{
			APIGroup: rbacv1.GroupName, Kind: "User", Name: s,
		})
	}
	return binding
}

// memberBinding is what the reconciler projects into an environment namespace
// once a membership takes effect. It is the object the API server consults, so
// it is the one this reads.
func memberBinding(namespace, role string, subjects ...string) *rbacv1.RoleBinding {
	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "kipper-project-" + role, Namespace: namespace},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "kipper:project-" + role},
	}
	for _, s := range subjects {
		rb.Subjects = append(rb.Subjects, rbacv1.Subject{APIGroup: rbacv1.GroupName, Kind: "User", Name: s})
	}
	return rb
}

func accessFor(t *testing.T, binding *rbacv1.ClusterRoleBinding, objects []runtime.Object, projects ...*unstructured.Unstructured) map[string]string {
	t.Helper()
	if binding != nil {
		objects = append(objects, binding)
	}
	dynObjects := make([]runtime.Object, 0, len(projects))
	for _, p := range projects {
		dynObjects = append(dynObjects, p)
	}
	access, err := clusterAccessByEmail(
		context.Background(),
		k8sfake.NewClientset(objects...),
		dynamicfake.NewSimpleDynamicClient(projectScheme(), dynObjects...),
	)
	require.NoError(t, err)
	return access
}

// The confusion that cost a day: five identities listed as console admins,
// none of whom could run a single kip command, because a console role grants
// nothing at the Kubernetes API.
func TestClusterAccessReportsTheBootstrapAdmin(t *testing.T) {
	access := accessFor(t, adminBinding("oidc:admin@shop.example"), nil)

	assert.Equal(t, "cluster-admin", access["admin@shop.example"])
	assert.Empty(t, access["sam@shop.example"], "a console role is not cluster access")
}

// The ordinary route, once the reconciler has projected it. The RoleBinding is
// what the API server consults, so it is what "cluster access" reports.
func TestClusterAccessReportsAnEffectiveRoleBinding(t *testing.T) {
	access := accessFor(t, nil,
		[]runtime.Object{memberBinding("shop-test", "deployer", "oidc:sam@shop.example")},
		projectWithMembers(map[string]any{"email": "sam@shop.example", "role": "deployer"}))

	assert.Equal(t, "deployer in shop-test", access["sam@shop.example"])
}

// Membership is desired state and its projection is asynchronous. Reporting it
// as access would repeat the defect this whole change set is about: a
// plausible statement that is not evidence. Pending is true and actionable.
func TestClusterAccessReportsUnprojectedMembershipAsPending(t *testing.T) {
	access := accessFor(t, nil, nil,
		projectWithMembers(map[string]any{"email": "sam@shop.example", "role": "deployer"}))

	assert.Equal(t, "pending on shop", access["sam@shop.example"],
		"a membership the reconciler has not projected is not access yet")
}

// Several namespaces, reported in a stable order rather than whichever the
// iteration happened to reach last.
func TestClusterAccessReportsEveryNamespaceItHolds(t *testing.T) {
	access := accessFor(t, nil, []runtime.Object{
		memberBinding("shop-test", "deployer", "oidc:sam@shop.example"),
		memberBinding("shop-prod", "viewer", "oidc:sam@shop.example"),
	})

	assert.Equal(t, "deployer in shop-test, viewer in shop-prod", access["sam@shop.example"])
}

// Holding both, the wider grant is the true answer: a cluster-admin is not
// limited to the projects they happen to be a member of.
func TestClusterAccessPrefersTheWiderGrant(t *testing.T) {
	access := accessFor(t,
		adminBinding("oidc:admin@shop.example"),
		[]runtime.Object{memberBinding("shop-test", "viewer", "oidc:admin@shop.example")},
		projectWithMembers(map[string]any{"email": "admin@shop.example", "role": "viewer"}))

	assert.Equal(t, "cluster-admin", access["admin@shop.example"])
}

// A cluster with no binding at all is the state that made every OIDC login
// useless, and it has to read as "nobody", not as an error.
func TestClusterAccessReportsNobodyWhenThereIsNoBinding(t *testing.T) {
	access := accessFor(t, nil, nil, projectWithMembers())

	assert.Empty(t, access)
}

func TestNoneHaveAccessSpotsAClusterRunningOnItsSharedCertificate(t *testing.T) {
	emails := []string{"admin@shop.example", "sam@shop.example"}

	assert.True(t, noneHaveAccess(map[string]string{}, emails))
	assert.False(t, noneHaveAccess(map[string]string{"sam@shop.example": "deployer on shop"}, emails))
}
