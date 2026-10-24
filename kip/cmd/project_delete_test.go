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
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/getkipper/kipper/controller/pkg/labels"
	"github.com/getkipper/kipper/controller/pkg/platform"
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

func moveFixture(t *testing.T, namespaces []runtime.Object, services ...*unstructured.Unstructured) (*k8sfake.Clientset, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	objects := make([]runtime.Object, 0, len(services))
	for _, svc := range services {
		objects = append(objects, svc)
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{manifest.ProjectGVR: "ProjectList", manifest.ServiceGVR: "ServiceList"},
		objects...)
	return k8sfake.NewClientset(namespaces...), dyn
}

func markedService(namespace string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kipper.run/v1alpha1",
		"kind":       "Service",
		"metadata": map[string]any{"name": "pg", "namespace": namespace,
			"annotations": map[string]any{platform.StorageMoveAnnotation: "kipper-storage-move-1"}},
	}}
}

// The namespace delete that follows a project delete cannot finish while a
// volume in it is moving: admission keeps the marked Service, so the namespace
// stays Terminating with the move half done.
func TestAProjectWithAMovingNamespaceIsNotDeleted(t *testing.T) {
	ns := labelledNamespace("shop-prod", "the-namespace")
	ns.Labels = map[string]string{platform.StorageMoveNamespaceLabel: "true"}
	project := projectClaiming(claimOf("shop-prod", "the-namespace"))
	clientset, dyn := moveFixture(t, []runtime.Object{&ns})

	err := refuseDuringStorageMove(context.Background(), clientset, dyn, project)

	require.Error(t, err, "a project was deleted while a volume in its namespace was moving to Longhorn")
	assert.Contains(t, err.Error(), "shop-prod")
}

// The namespace label and the Service's mark are written at different steps,
// so either one alone identifies a move.
func TestAMarkedServiceAloneBlocksTheProjectDelete(t *testing.T) {
	ns := labelledNamespace("shop-prod", "the-namespace")
	project := projectClaiming(claimOf("shop-prod", "the-namespace"))
	clientset, dyn := moveFixture(t, []runtime.Object{&ns}, markedService("shop-prod"))

	err := refuseDuringStorageMove(context.Background(), clientset, dyn, project)

	require.Error(t, err, "a project was deleted while one of its services carried a storage move mark")
	assert.Contains(t, err.Error(), "shop-prod")
}

// The refusal names the moving service with the flags that reach it, so the
// command it prints runs as it stands.
func TestTheProjectDeleteRefusalNamesTheMovingService(t *testing.T) {
	ns := labelledNamespace("shop-staging", "the-namespace")
	ns.Labels = map[string]string{platform.StorageMoveNamespaceLabel: "true", labels.Project: "shop", labels.Environment: "staging"}
	project := projectClaiming(claimOf("shop-staging", "the-namespace"))
	clientset, dyn := moveFixture(t, []runtime.Object{&ns}, markedService("shop-staging"))

	err := refuseDuringStorageMove(context.Background(), clientset, dyn, project)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Finish the move with 'kip service storage longhorn pg --project shop --environment staging', or undo it with --rollback, then delete the project")

	err = refuseEnvRemovalDuringStorageMove(context.Background(), clientset, dyn, project, "shop-staging")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Finish the move with 'kip service storage longhorn pg --project shop --environment staging', or undo it with --rollback, then remove the environment")
}

// A namespace that names no project is reached with --project and its own
// name, which kip accepts, so the printed command still finds the service.
func TestTheProjectDeleteRefusalReachesANamespaceWithoutAProjectLabel(t *testing.T) {
	ns := labelledNamespace("shop-staging", "the-namespace")
	ns.Labels = map[string]string{platform.StorageMoveNamespaceLabel: "true"}
	project := projectClaiming(claimOf("shop-staging", "the-namespace"))
	clientset, dyn := moveFixture(t, []runtime.Object{&ns}, markedService("shop-staging"))

	err := refuseDuringStorageMove(context.Background(), clientset, dyn, project)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Finish the move with 'kip service storage longhorn pg --project shop-staging', or undo it with --rollback")
}

func TestAMoveInAnotherProjectDoesNotBlockTheDelete(t *testing.T) {
	mine := labelledNamespace("shop-prod", "the-namespace")
	theirs := labelledNamespace("grocer-prod", "their-namespace")
	theirs.Labels = map[string]string{platform.StorageMoveNamespaceLabel: "true"}
	project := projectClaiming(claimOf("shop-prod", "the-namespace"))
	clientset, dyn := moveFixture(t, []runtime.Object{&mine, &theirs}, markedService("grocer-prod"))

	assert.NoError(t, refuseDuringStorageMove(context.Background(), clientset, dyn, project),
		"a move in a namespace this project does not hold blocked its delete")
}

// Removing an environment deletes its namespace through the project
// reconciler, so it has the same problem as deleting the project.
func TestAnEnvironmentWithAMoveIsNotRemoved(t *testing.T) {
	prod := labelledNamespace("shop-prod", "the-namespace")
	project := projectClaiming(claimOf("shop-prod", "the-namespace"))
	clientset, dyn := moveFixture(t, []runtime.Object{&prod}, markedService("shop-prod"))

	err := refuseEnvRemovalDuringStorageMove(context.Background(), clientset, dyn, project, "shop-prod")

	require.Error(t, err, "an environment was removed while a volume in its namespace was moving to Longhorn")
	assert.Contains(t, err.Error(), "shop-prod")
}

func TestAnEnvironmentWithoutAMoveCanBeRemoved(t *testing.T) {
	prod := labelledNamespace("shop-prod", "the-namespace")
	test := labelledNamespace("shop-test", "the-other-namespace")
	test.Labels = map[string]string{platform.StorageMoveNamespaceLabel: "true"}
	project := projectClaiming(claimOf("shop-prod", "the-namespace"), claimOf("shop-test", "the-other-namespace"))
	clientset, dyn := moveFixture(t, []runtime.Object{&prod, &test}, markedService("shop-test"))

	assert.NoError(t, refuseEnvRemovalDuringStorageMove(context.Background(), clientset, dyn, project, "shop-prod"),
		"a move in another environment blocked removing this one")
	assert.NoError(t, refuseEnvRemovalDuringStorageMove(context.Background(), clientset, dyn, project, "shop-gone"),
		"an environment whose namespace is already gone could not be removed")
}

// The reconciler deletes only namespaces the project holds. A declaration whose
// namespace another project holds is removed without touching it.
func TestAMoveInANamespaceTheProjectDoesNotHoldDoesNotBlockTheRemoval(t *testing.T) {
	prod := labelledNamespace("shop-prod", "their-namespace")
	prod.Labels = map[string]string{platform.StorageMoveNamespaceLabel: "true"}
	project := projectClaiming()
	clientset, dyn := moveFixture(t, []runtime.Object{&prod})

	assert.NoError(t, refuseEnvRemovalDuringStorageMove(context.Background(), clientset, dyn, project, "shop-prod"),
		"a move in a namespace this project does not hold blocked removing its environment")
}
