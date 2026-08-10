package cmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/getkipper/kipper/kip/internal/deployer"
)

func seedApp(t *testing.T, dyn *dynamicfake.FakeDynamicClient, ns, name string, spec map[string]interface{}) {
	t.Helper()
	app := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1",
		"kind":       "App",
		"metadata":   map[string]interface{}{"name": name, "namespace": ns},
		"spec":       spec,
	}}
	_, err := dyn.Resource(deployer.AppGVR).Namespace(ns).Create(context.Background(), app, metav1.CreateOptions{})
	require.NoError(t, err)
}

func storedImage(t *testing.T, dyn *dynamicfake.FakeDynamicClient, ns, name string) string {
	t.Helper()
	app, err := dyn.Resource(deployer.AppGVR).Namespace(ns).Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(t, err)
	image, _, _ := unstructured.NestedString(app.Object, "spec", "image")
	return image
}

// The whole bug: promote wrote the Deployment, which the App reconciler rebuilds
// from spec.image, so the promotion was undone within milliseconds and the
// desired state never moved. It has to write the CR.
func TestPromoteApp_WritesTheDesiredStateNotTheDeployment(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seedApp(t, dyn, "hrportal-test", "backend", map[string]interface{}{"image": "ghcr.io/acme/backend:2026-08-02"})
	seedApp(t, dyn, "hrportal-prod", "backend", map[string]interface{}{"image": "ghcr.io/acme/backend:2026-06-29"})

	require.NoError(t, promoteApp(context.Background(), dyn, "hrportal-test", "hrportal-prod", "backend", "test", "prod"))
	assert.Equal(t, "ghcr.io/acme/backend:2026-08-02", storedImage(t, dyn, "hrportal-prod", "backend"),
		"the App CR is the desired state, and it is what the reconciler builds from")
	assert.Equal(t, "ghcr.io/acme/backend:2026-08-02", storedImage(t, dyn, "hrportal-test", "backend"),
		"and the environment it came from is only read")
}

// The tick has to follow the cluster's answer, not the call returning. Reporting
// success against an unchanged spec is what cost a day.
func TestPromoteApp_FailsWhenTheClusterDidNotStoreTheImage(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seedApp(t, dyn, "hrportal-test", "backend", map[string]interface{}{"image": "ghcr.io/acme/backend:2026-08-02"})
	seedApp(t, dyn, "hrportal-prod", "backend", map[string]interface{}{"image": "ghcr.io/acme/backend:2026-06-29"})

	// An admission webhook, or anything else, quietly keeps the old value.
	dyn.PrependReactor("update", "apps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		obj := action.(k8stesting.UpdateAction).GetObject().(*unstructured.Unstructured).DeepCopy()
		require.NoError(t, unstructured.SetNestedField(obj.Object, "ghcr.io/acme/backend:2026-06-29", "spec", "image"))
		return true, obj, nil
	})

	err := promoteApp(context.Background(), dyn, "hrportal-test", "hrportal-prod", "backend", "test", "prod")
	require.Error(t, err, "a promotion that did not land must not report success")
	assert.Contains(t, err.Error(), "2026-06-29")
}

// The reconciler writes status and finalizers onto the same object, so a plain
// update loses to it often enough to matter, and a lost update here is a
// promotion that silently did not happen.
func TestPromoteApp_RetriesAConflictingWrite(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seedApp(t, dyn, "hrportal-test", "backend", map[string]interface{}{"image": "ghcr.io/acme/backend:2026-08-02"})
	seedApp(t, dyn, "hrportal-prod", "backend", map[string]interface{}{"image": "ghcr.io/acme/backend:2026-06-29"})

	updates := 0
	dyn.PrependReactor("update", "apps", func(k8stesting.Action) (bool, runtime.Object, error) {
		updates++
		if updates == 1 {
			return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "kipper.run", Resource: "apps"}, "backend", context.DeadlineExceeded)
		}
		return false, nil, nil
	})

	require.NoError(t, promoteApp(context.Background(), dyn, "hrportal-test", "hrportal-prod", "backend", "test", "prod"))
	assert.Equal(t, "ghcr.io/acme/backend:2026-08-02", storedImage(t, dyn, "hrportal-prod", "backend"))
	assert.Equal(t, 2, updates, "the conflict is retried rather than lost")
}

// Creating a bare Deployment for a missing target left a workload no App CR
// owns, which is the orphan `kip discover` exists to find. Say what to run.
func TestPromoteApp_RefusesWhenTheTargetDoesNotExist(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seedApp(t, dyn, "hrportal-test", "backend", map[string]interface{}{"image": "ghcr.io/acme/backend:2026-08-02"})

	err := promoteApp(context.Background(), dyn, "hrportal-test", "hrportal-prod", "backend", "test", "prod")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kip app deploy")

	_, getErr := dyn.Resource(deployer.AppGVR).Namespace("hrportal-prod").Get(context.Background(), "backend", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(getErr), "and nothing is created behind the operator's back")
}

// A git app's image is build output the controller owns, so an image written
// there is undone by the next build.
func TestPromoteApp_RefusesToPromoteOntoAGitApp(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seedApp(t, dyn, "hrportal-test", "backend", map[string]interface{}{"image": "ghcr.io/acme/backend:2026-08-02"})
	seedApp(t, dyn, "hrportal-prod", "backend", map[string]interface{}{
		"image": "ghcr.io/acme/backend:2026-06-29",
		"git":   map[string]interface{}{"url": "https://github.com/acme/backend.git", "branch": "main"},
	})

	err := promoteApp(context.Background(), dyn, "hrportal-test", "hrportal-prod", "backend", "test", "prod")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "builds from git")
	assert.Equal(t, "ghcr.io/acme/backend:2026-06-29", storedImage(t, dyn, "hrportal-prod", "backend"),
		"and the build's own image is left alone")
}

// A git app in the source has no image until its first build finishes, and
// promoting the placeholder would put busybox in front of the next environment.
func TestPromoteApp_RefusesWhenTheSourceHasNoImage(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seedApp(t, dyn, "hrportal-test", "frontend", map[string]interface{}{
		"git": map[string]interface{}{"url": "https://github.com/acme/frontend.git"},
	})
	seedApp(t, dyn, "hrportal-prod", "frontend", map[string]interface{}{"image": "ghcr.io/acme/frontend:2026-06-29"})

	err := promoteApp(context.Background(), dyn, "hrportal-test", "hrportal-prod", "frontend", "test", "prod")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no image")
	assert.Equal(t, "ghcr.io/acme/frontend:2026-06-29", storedImage(t, dyn, "hrportal-prod", "frontend"))
}

// The environment it came from is recorded, so the next person can see it.
func TestPromoteApp_RecordsWhereTheImageCameFrom(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seedApp(t, dyn, "hrportal-test", "backend", map[string]interface{}{"image": "ghcr.io/acme/backend:2026-08-02"})
	seedApp(t, dyn, "hrportal-prod", "backend", map[string]interface{}{"image": "ghcr.io/acme/backend:2026-06-29"})

	require.NoError(t, promoteApp(context.Background(), dyn, "hrportal-test", "hrportal-prod", "backend", "test", "prod"))
	app, err := dyn.Resource(deployer.AppGVR).Namespace("hrportal-prod").Get(context.Background(), "backend", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "test", app.GetAnnotations()["kipper.run/promoted-from"])
	assert.NotEmpty(t, app.GetAnnotations()["kipper.run/promoted-at"])
}

// An app waiting on its finalizer accepts the write and then goes, so reporting
// the promotion would be the same misleading tick in a different place.
func TestPromoteApp_RefusesATargetThatIsBeingDeleted(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seedApp(t, dyn, "hrportal-test", "backend", map[string]interface{}{"image": "ghcr.io/acme/backend:2026-08-02"})
	seedApp(t, dyn, "hrportal-prod", "backend", map[string]interface{}{"image": "ghcr.io/acme/backend:2026-06-29"})

	dyn.PrependReactor("get", "apps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.(k8stesting.GetAction).GetNamespace() != "hrportal-prod" {
			return false, nil, nil
		}
		terminating := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "kipper.run/v1alpha1",
			"kind":       "App",
			"metadata": map[string]interface{}{
				"name": "backend", "namespace": "hrportal-prod",
				"deletionTimestamp": "2026-08-03T08:00:00Z",
				"finalizers":        []interface{}{"kipper.run/cleanup"},
			},
			"spec": map[string]interface{}{"image": "ghcr.io/acme/backend:2026-06-29"},
		}}
		return true, terminating, nil
	})

	err := promoteApp(context.Background(), dyn, "hrportal-test", "hrportal-prod", "backend", "test", "prod")
	require.Error(t, err, "an app that is going does not get a tick")
	assert.Contains(t, err.Error(), "being deleted")
}
