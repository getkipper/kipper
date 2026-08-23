package cmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/getkipper/kipper/kip/internal/manifest"
)

// Deleting a project deletes its namespaces, and the label does not say which
// those are. Anyone who can write a namespace can point its label at another
// project, so deleting on the label alone turns an ordinary `kip project delete`
// into the destruction of somebody else's namespace, with nothing to warn the
// operator running it.
func labelledNamespace(name, uid string) corev1.Namespace {
	return corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(uid)}}
}

func namesOf(namespaces []corev1.Namespace) []string {
	out := make([]string, 0, len(namespaces))
	for _, ns := range namespaces {
		out = append(out, ns.Name)
	}
	return out
}

func TestOnlyTheNamespacesTheProjectRecordedAreDeleted(t *testing.T) {
	project := projectClaiming(claimOf("shop-test", "the-namespace"))
	project.Object["status"].(map[string]any)["namespaces"] = []any{"shop-prod"}

	held := heldByProject(project, nil, []corev1.Namespace{
		labelledNamespace("shop-test", "the-namespace"),
		labelledNamespace("shop-prod", "the-other-namespace"),
		labelledNamespace("victim-prod", "the-victims-namespace"),
	})

	assert.ElementsMatch(t, []string{"shop-test", "shop-prod"}, namesOf(held),
		"a namespace only pointed at this project by its label was going to be deleted with it")
}

// A claim names an object. One naming a namespace that has since been replaced
// says nothing about the replacement, and the older record is what decides then.
func TestANamespaceOnNeitherRecordIsLeftAlone(t *testing.T) {
	project := projectClaiming(claimOf("shop-test", "the-object-that-is-gone"))

	held := heldByProject(project, nil, []corev1.Namespace{labelledNamespace("shop-test", "the-new-object")})

	assert.Empty(t, namesOf(held),
		"a namespace this project neither claims nor recorded was deleted on the strength of its label")
}

// The older record carries a name and no object, so it answers for whatever
// carries the name. That is what collects a namespace recreated out of band,
// which is the one that most needs collecting.
func TestARecordedNameIsDeletedWhateverObjectCarriesIt(t *testing.T) {
	project := projectClaiming(claimOf("shop-test", "the-object-that-is-gone"))
	project.Object["status"].(map[string]any)["namespaces"] = []any{"shop-test"}

	held := heldByProject(project, nil, []corev1.Namespace{labelledNamespace("shop-test", "the-new-object")})

	assert.Equal(t, []string{"shop-test"}, namesOf(held),
		"a namespace this project recorded holding outlived it because the object had been replaced")
}

// A claim on the live object outranks any record of the name, whoever holds it.
// Two projects can both carry a namespace on record and only one can hold the
// object; without this the loser's record plus a rewritten label deletes the
// winner's namespace, and rewriting the label is the move this gate exists to
// survive.
func TestANamespaceAnotherProjectClaimsIsLeftAlone(t *testing.T) {
	project := projectClaiming()
	project.Object["status"].(map[string]any)["namespaces"] = []any{"shop-prod"}
	grocer := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kipper.run/v1alpha1",
		"kind":       "Project",
		"metadata":   map[string]any{"name": "grocer"},
		"status": map[string]any{"namespaceClaims": []any{
			map[string]any{"name": "shop-prod", "uid": "the-live-object"},
		}},
	}}

	held := heldByProject(project, []unstructured.Unstructured{*grocer},
		[]corev1.Namespace{labelledNamespace("shop-prod", "the-live-object")})

	assert.Empty(t, namesOf(held),
		"a live namespace another project claims was going to be deleted because this project had the name on record")
}

// Both deletes name the object they were authorised against, and nothing else
// here would notice if they stopped: the fake clients apply a delete by name
// whether or not a precondition is set. So assert that they are sent.
//
// The project's own delete is the one with the widest window, because the
// confirmation prompt in front of it is as long as somebody takes to answer.
func TestDeletingAProjectBindsBothDeletesToTheObjectsItRead(t *testing.T) {
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(
		manifest.ProjectGVR.GroupVersion().WithKind("ProjectList"), &unstructured.UnstructuredList{})
	dyn := dynamicfake.NewSimpleDynamicClient(scheme)

	var projectDelete k8stesting.DeleteActionImpl
	dyn.PrependReactor("delete", "projects", func(a k8stesting.Action) (bool, runtime.Object, error) {
		projectDelete = a.(k8stesting.DeleteActionImpl)
		return true, nil, nil
	})
	require.NoError(t, deleteProjectCR(context.Background(), dyn, "shop", "shop", "the-project"))

	require.NotNil(t, projectDelete.DeleteOptions.Preconditions,
		"the project delete carried no precondition, so a project recreated under this name while the operator answered the prompt is deleted instead")
	require.NotNil(t, projectDelete.DeleteOptions.Preconditions.UID)
	assert.Equal(t, types.UID("the-project"), *projectDelete.DeleteOptions.Preconditions.UID)

	clientset := k8sfake.NewClientset()
	var namespaceDelete k8stesting.DeleteActionImpl
	clientset.PrependReactor("delete", "namespaces", func(a k8stesting.Action) (bool, runtime.Object, error) {
		namespaceDelete = a.(k8stesting.DeleteActionImpl)
		return true, nil, nil
	})
	collectNamespaces(context.Background(), clientset,
		[]corev1.Namespace{labelledNamespace("shop-test", "the-namespace")})

	require.NotNil(t, namespaceDelete.DeleteOptions.Preconditions,
		"the namespace delete carried no precondition, so a namespace recreated under that name between the check and the delete is destroyed on its predecessor's authorisation")
	require.NotNil(t, namespaceDelete.DeleteOptions.Preconditions.UID)
	assert.Equal(t, types.UID("the-namespace"), *namespaceDelete.DeleteOptions.Preconditions.UID)
}
