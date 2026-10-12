package deployer

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
)

func TestDeployWritesTheRefusedPaths(t *testing.T) {
	d, dynClient := testDeployer()
	ctx := context.Background()

	require.NoError(t, d.Deploy(ctx, Options{
		Name: "api", Namespace: "default", Image: "nginx:latest", Port: 8080,
		Domain:        "api-test.kipper.run",
		InternalPaths: []string{"/admin"},
		PublicPaths:   []string{"/actuator/prometheus"},
	}))

	app, err := dynClient.Resource(AppGVR).Namespace("default").Get(ctx, "api", metav1.GetOptions{})
	require.NoError(t, err)

	internal, _, _ := unstructured.NestedStringSlice(app.Object, "spec", "route", "internalPaths")
	assert.Equal(t, []string{"/admin"}, internal)
	public, _, _ := unstructured.NestedStringSlice(app.Object, "spec", "route", "publicPaths")
	assert.Equal(t, []string{"/actuator/prometheus"}, public)
}

func TestRedeployKeepsTheRefusedPaths(t *testing.T) {
	d, dynClient := testDeployer()
	ctx := context.Background()

	require.NoError(t, d.Deploy(ctx, Options{
		Name: "api", Namespace: "default", Image: "nginx:latest", Port: 8080,
		Domain: "api-test.kipper.run", InternalPaths: []string{"/admin"},
	}))
	require.NoError(t, d.Deploy(ctx, Options{
		Name: "api", Namespace: "default", Image: "nginx:v2", Port: 8080,
		Changed: map[string]bool{"image": true},
	}))

	app, err := dynClient.Resource(AppGVR).Namespace("default").Get(ctx, "api", metav1.GetOptions{})
	require.NoError(t, err)
	internal, _, _ := unstructured.NestedStringSlice(app.Object, "spec", "route", "internalPaths")
	assert.Equal(t, []string{"/admin"}, internal, "a redeploy that only changes the image must leave the refusals alone")
}

func TestDeployNormalisesThePathsItWrites(t *testing.T) {
	d, dynClient := testDeployer()
	ctx := context.Background()

	require.NoError(t, d.Deploy(ctx, Options{
		Name: "api", Namespace: "default", Image: "nginx:latest", Port: 8080,
		Domain:        "api-test.kipper.run",
		InternalPaths: []string{"/admin/", "/admin", "/ops/"},
	}))

	app, err := dynClient.Resource(AppGVR).Namespace("default").Get(ctx, "api", metav1.GetOptions{})
	require.NoError(t, err)
	internal, _, _ := unstructured.NestedStringSlice(app.Object, "spec", "route", "internalPaths")
	assert.Equal(t, []string{"/admin", "/ops"}, internal)
}

func TestUpdateInternalPathsReplacesAndClears(t *testing.T) {
	d, dynClient := testDeployer()
	ctx := context.Background()

	require.NoError(t, d.Deploy(ctx, Options{
		Name: "api", Namespace: "default", Image: "nginx:latest", Port: 8080,
		Domain: "api-test.kipper.run", InternalPaths: []string{"/admin"},
	}))

	require.NoError(t, d.UpdateInternalPaths(ctx, "default", "api", []string{"/ops", "/admin"}))
	app, err := dynClient.Resource(AppGVR).Namespace("default").Get(ctx, "api", metav1.GetOptions{})
	require.NoError(t, err)
	internal, _, _ := unstructured.NestedStringSlice(app.Object, "spec", "route", "internalPaths")
	assert.Equal(t, []string{"/ops", "/admin"}, internal)

	require.NoError(t, d.UpdateInternalPaths(ctx, "default", "api", nil))
	app, err = dynClient.Resource(AppGVR).Namespace("default").Get(ctx, "api", metav1.GetOptions{})
	require.NoError(t, err)
	_, found, _ := unstructured.NestedStringSlice(app.Object, "spec", "route", "internalPaths")
	assert.False(t, found, "an empty list clears the field rather than writing an empty one")
}

func TestUpdateInternalPathsRefusesAnAppWithNoRoute(t *testing.T) {
	d, _ := testDeployer()
	ctx := context.Background()

	require.NoError(t, d.Deploy(ctx, Options{
		Name: "worker", Namespace: "default", Image: "nginx:latest", Port: 8080,
	}))

	err := d.UpdateInternalPaths(ctx, "default", "worker", []string{"/admin"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no route")
}

func TestUpdateRouteClosedSetsAndClears(t *testing.T) {
	d, dynClient := testDeployer()
	ctx := context.Background()
	require.NoError(t, d.Deploy(ctx, Options{Name: "pay", Namespace: "default", Image: "pay:1", Port: 8080, Domain: "pay.example.com"}))

	require.NoError(t, d.UpdateRouteClosed(ctx, "default", "pay", true))
	app, err := dynClient.Resource(AppGVR).Namespace("default").Get(ctx, "pay", metav1.GetOptions{})
	require.NoError(t, err)
	closed, _, _ := unstructured.NestedBool(app.Object, "spec", "route", "closed")
	assert.True(t, closed)

	require.NoError(t, d.UpdateRouteClosed(ctx, "default", "pay", false))
	app, err = dynClient.Resource(AppGVR).Namespace("default").Get(ctx, "pay", metav1.GetOptions{})
	require.NoError(t, err)
	_, found, _ := unstructured.NestedBool(app.Object, "spec", "route", "closed")
	assert.False(t, found, "opening the route removes the field")
}

func TestUpdateRouteClosedRefusesWhenTheClusterDropsTheField(t *testing.T) {
	d, dynClient := testDeployer()
	ctx := context.Background()
	require.NoError(t, d.Deploy(ctx, Options{Name: "pay", Namespace: "default", Image: "pay:1", Port: 8080, Domain: "pay.example.com"}))
	dynClient.PrependReactor("update", "apps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		obj := action.(k8stesting.UpdateAction).GetObject().(*unstructured.Unstructured).DeepCopy()
		unstructured.RemoveNestedField(obj.Object, "spec", "route", "closed")
		return true, obj, nil
	})

	err := d.UpdateRouteClosed(ctx, "default", "pay", true)

	assert.ErrorContains(t, err, "kip upgrade")
}
