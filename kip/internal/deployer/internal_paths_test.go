package deployer

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
