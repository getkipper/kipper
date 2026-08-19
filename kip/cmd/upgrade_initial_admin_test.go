package cmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func bindingScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(
		consoleRBACGVRs["ClusterRoleBinding"].GroupVersion().WithKind("ClusterRoleBindingList"),
		&unstructured.UnstructuredList{},
	)
	return scheme
}

func existingBinding(subjects ...string) *unstructured.Unstructured {
	list := make([]any, 0, len(subjects))
	for _, s := range subjects {
		list = append(list, map[string]any{
			"apiGroup": "rbac.authorization.k8s.io", "kind": "User", "name": s,
		})
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1",
		"kind":       "ClusterRoleBinding",
		"metadata":   map[string]any{"name": initialAdminBindingName},
		"roleRef": map[string]any{
			"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": "cluster-admin",
		},
		"subjects": list,
	}}
}

func readBinding(t *testing.T, client *dynamicfake.FakeDynamicClient) *unstructured.Unstructured {
	t.Helper()
	got, err := client.Resource(consoleRBACGVRs["ClusterRoleBinding"]).
		Get(context.Background(), initialAdminBindingName, metav1.GetOptions{})
	require.NoError(t, err)
	return got
}

// A cluster installed before the bootstrap grant existed has no route to it:
// the install created it, the upgrade deliberately skips it, and without it no
// OIDC identity can do anything at all. That was a live cluster, repaired by
// hand over SSH.
func TestUpgradeCreatesAMissingInitialAdminBinding(t *testing.T) {
	client := dynamicfake.NewSimpleDynamicClient(bindingScheme())

	created, err := createInitialAdminBindingIfMissing(context.Background(), client, "shop.example")

	require.NoError(t, err)
	assert.True(t, created)

	binding := readBinding(t, client)
	subjects, _, _ := unstructured.NestedSlice(binding.Object, "subjects")
	require.Len(t, subjects, 1, "only the install's own admin identity is bound")
	name, _, _ := unstructured.NestedString(subjects[0].(map[string]any), "name")
	assert.Equal(t, "oidc:admin@shop.example", name)

	role, _, _ := unstructured.NestedString(binding.Object, "roleRef", "name")
	assert.Equal(t, "cluster-admin", role)
}

// The reason the upgrade skips this binding in the first place: its subjects
// are live state, and re-applying the install-time copy would revoke every
// admin added since. Creating when absent must not become overwriting when
// present.
func TestUpgradeLeavesAnExistingBindingExactlyAsItIs(t *testing.T) {
	client := dynamicfake.NewSimpleDynamicClient(bindingScheme(),
		existingBinding("oidc:admin@shop.example", "oidc:sam@shop.example"))

	created, err := createInitialAdminBindingIfMissing(context.Background(), client, "shop.example")

	require.NoError(t, err)
	assert.False(t, created)

	subjects, _, _ := unstructured.NestedSlice(readBinding(t, client).Object, "subjects")
	assert.Len(t, subjects, 2, "an admin added since the install must survive an upgrade")
}

// Two upgrades at once, or an upgrade racing an install: the second one finds
// the goal state already reached and says so rather than failing.
func TestUpgradeTreatsALostCreateRaceAsSuccess(t *testing.T) {
	client := dynamicfake.NewSimpleDynamicClient(bindingScheme())

	for range 3 {
		_, err := createInitialAdminBindingIfMissing(context.Background(), client, "shop.example")
		require.NoError(t, err)
	}

	subjects, _, _ := unstructured.NestedSlice(readBinding(t, client).Object, "subjects")
	assert.Len(t, subjects, 1)
}
