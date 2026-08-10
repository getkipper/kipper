package webhook

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/getkipper/kipper/kip/internal/deployer"
)

// fakeAppClient returns a dynamic fake seeded with an App CR carrying the given
// spec.image, matching how the reconciler stores an app's desired image.
func fakeAppClient(t *testing.T, name, image string) *dynamicfake.FakeDynamicClient {
	t.Helper()
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "kipper.run", Version: "v1alpha1", Kind: "App"}, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "kipper.run", Version: "v1alpha1", Kind: "AppList"}, &unstructured.UnstructuredList{})
	app := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1",
		"kind":       "App",
		"metadata":   map[string]interface{}{"name": name, "namespace": "default"},
		"spec":       map[string]interface{}{"image": image},
	}}
	return dynamicfake.NewSimpleDynamicClient(scheme, app)
}

func appImage(t *testing.T, dyn *dynamicfake.FakeDynamicClient, name string) string {
	t.Helper()
	app, err := dyn.Resource(deployer.AppGVR).Namespace("default").Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(t, err)
	image, _, _ := unstructured.NestedString(app.Object, "spec", "image")
	return image
}

func TestRecordDeployCreatesHistory(t *testing.T) {
	dyn := fakeAppClient(t, "api", "api:v1")
	ctx := context.Background()

	err := RecordDeploy(ctx, dyn, "default", "api", "api:v1", "abc123", "manual")
	require.NoError(t, err)

	history, err := GetHistory(ctx, dyn, "default", "api")
	require.NoError(t, err)
	require.Len(t, history, 1)
	assert.Equal(t, 1, history[0].Revision)
	assert.Equal(t, "api:v1", history[0].Image)
	assert.Equal(t, "abc123", history[0].Commit)
	assert.Equal(t, "manual", history[0].Trigger)
}

func TestRecordDeployIncrementsRevision(t *testing.T) {
	dyn := fakeAppClient(t, "api", "api:v1")
	ctx := context.Background()

	_ = RecordDeploy(ctx, dyn, "default", "api", "api:v1", "abc123", "manual")
	_ = RecordDeploy(ctx, dyn, "default", "api", "api:v2", "def456", "webhook")

	history, _ := GetHistory(ctx, dyn, "default", "api")
	require.Len(t, history, 2)
	assert.Equal(t, 2, history[0].Revision)
	assert.Equal(t, "api:v2", history[0].Image)
	assert.Equal(t, "webhook", history[0].Trigger)
	assert.Equal(t, 1, history[1].Revision)
}

// A git-built deploy recorded by the build controller and a manual deploy
// recorded by kip land in the same App CR annotation, so kip history sees both.
func TestRecordDeploySharesStoreWithBuildController(t *testing.T) {
	dyn := fakeAppClient(t, "api", "api:v1")
	ctx := context.Background()

	// Simulate the build controller having already written a build entry onto the
	// App CR annotation (identical JSON shape).
	app, _ := dyn.Resource(deployer.AppGVR).Namespace("default").Get(ctx, "api", metav1.GetOptions{})
	require.NoError(t, setHistory(app, []DeployEntry{{Revision: 1, Image: "zot/api:built-1", Trigger: "build", Timestamp: "2026-07-10T10:00:00Z"}}))
	_, err := dyn.Resource(deployer.AppGVR).Namespace("default").Update(ctx, app, metav1.UpdateOptions{})
	require.NoError(t, err)

	// kip records a manual deploy on top.
	require.NoError(t, RecordDeploy(ctx, dyn, "default", "api", "api:v2", "", "manual"))

	history, _ := GetHistory(ctx, dyn, "default", "api")
	require.Len(t, history, 2)
	assert.Equal(t, "manual", history[0].Trigger)
	assert.Equal(t, 2, history[0].Revision)
	assert.Equal(t, "build", history[1].Trigger, "kip must see the build controller's entry, not a separate store")
}

func TestRecordDeployLimitsHistory(t *testing.T) {
	dyn := fakeAppClient(t, "worker", "worker:v1")
	ctx := context.Background()

	for i := 0; i < 15; i++ {
		_ = RecordDeploy(ctx, dyn, "default", "worker", "worker:v"+string(rune('a'+i)), "", "manual")
	}

	history, _ := GetHistory(ctx, dyn, "default", "worker")
	assert.Len(t, history, maxHistoryEntries)
	assert.Equal(t, 15, history[0].Revision)
}

func TestGetHistoryEmptyWhenNoAnnotation(t *testing.T) {
	dyn := fakeAppClient(t, "api", "api:v1")
	ctx := context.Background()

	history, err := GetHistory(ctx, dyn, "default", "api")
	require.NoError(t, err)
	assert.Empty(t, history)
}

func TestRollbackSetsAppImageNotDeployment(t *testing.T) {
	dyn := fakeAppClient(t, "api", "api:v2")
	ctx := context.Background()

	_ = RecordDeploy(ctx, dyn, "default", "api", "api:v1", "abc", "manual")
	_ = RecordDeploy(ctx, dyn, "default", "api", "api:v2", "def", "webhook")

	target, err := Rollback(ctx, dyn, "default", "api", 0)
	require.NoError(t, err)
	assert.Equal(t, "api:v1", target.Image)

	// Rollback must move the App CR's desired image, which the reconciler owns —
	// not patch a Deployment the reconciler would revert.
	assert.Equal(t, "api:v1", appImage(t, dyn, "api"), "rollback must set spec.image on the App CR")

	history, _ := GetHistory(ctx, dyn, "default", "api")
	assert.Equal(t, "rollback", history[0].Trigger)
	assert.Equal(t, 3, history[0].Revision)
}

func TestRollbackToSpecificRevision(t *testing.T) {
	dyn := fakeAppClient(t, "api", "api:v3")
	ctx := context.Background()

	_ = RecordDeploy(ctx, dyn, "default", "api", "api:v1", "", "manual")
	_ = RecordDeploy(ctx, dyn, "default", "api", "api:v2", "", "webhook")
	_ = RecordDeploy(ctx, dyn, "default", "api", "api:v3", "", "webhook")

	target, err := Rollback(ctx, dyn, "default", "api", 1)
	require.NoError(t, err)
	assert.Equal(t, "api:v1", target.Image)
	assert.Equal(t, "api:v1", appImage(t, dyn, "api"))
}

func TestRollbackErrorsWhenExplicitRevisionMissing(t *testing.T) {
	dyn := fakeAppClient(t, "api", "api:v2")
	ctx := context.Background()

	_ = RecordDeploy(ctx, dyn, "default", "api", "api:v1", "", "manual")
	_ = RecordDeploy(ctx, dyn, "default", "api", "api:v2", "", "webhook")

	// Revision 9 never existed. This must error, not silently roll to "previous".
	_, err := Rollback(ctx, dyn, "default", "api", 9)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "9")
	assert.Equal(t, "api:v2", appImage(t, dyn, "api"), "a failed rollback must not change the running image")
}

func TestRollbackErrorsWhenNoPreviousVersion(t *testing.T) {
	dyn := fakeAppClient(t, "api", "api:v1")
	ctx := context.Background()

	_ = RecordDeploy(ctx, dyn, "default", "api", "api:v1", "", "manual")

	_, err := Rollback(ctx, dyn, "default", "api", 0)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no previous version")
}
