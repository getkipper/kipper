package deployer

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestUpdateResourcesSetsOnCR(t *testing.T) {
	d, dynClient := testDeployer()
	ctx := context.Background()

	_ = d.Deploy(ctx, Options{
		Name: "api", Namespace: "default", Image: "nginx", Port: 80,
	})

	err := d.UpdateResources(ctx, "default", "api", "256Mi", "500m")
	require.NoError(t, err)

	app, _ := dynClient.Resource(AppGVR).Namespace("default").Get(ctx, "api", metav1.GetOptions{})
	// The CRD uses request/limit pairs, not bare memory/cpu keys.
	memReq, _, _ := unstructured.NestedString(app.Object, "spec", "resources", "memoryRequest")
	memLim, _, _ := unstructured.NestedString(app.Object, "spec", "resources", "memoryLimit")
	cpuReq, _, _ := unstructured.NestedString(app.Object, "spec", "resources", "cpuRequest")
	cpuLim, _, _ := unstructured.NestedString(app.Object, "spec", "resources", "cpuLimit")
	assert.Equal(t, "256Mi", memReq)
	assert.Equal(t, "256Mi", memLim)
	assert.Equal(t, "500m", cpuReq)
	assert.Equal(t, "500m", cpuLim)
}

func TestUpdateResourcesNotFoundReturnsError(t *testing.T) {
	d, _ := testDeployer()

	err := d.UpdateResources(context.Background(), "default", "nonexistent", "256Mi", "500m")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestDeployDefaultsReplicasToOne(t *testing.T) {
	d, dynClient := testDeployer()
	ctx := context.Background()

	err := d.Deploy(ctx, Options{
		Name: "api", Namespace: "default", Image: "nginx", Port: 80,
	})
	require.NoError(t, err)

	app, _ := dynClient.Resource(AppGVR).Namespace("default").Get(ctx, "api", metav1.GetOptions{})
	replicas, _, _ := unstructured.NestedInt64(app.Object, "spec", "replicas")
	assert.Equal(t, int64(1), replicas)
}

func TestDeployWithRouteGroupSetsRoute(t *testing.T) {
	d, dynClient := testDeployer()
	ctx := context.Background()

	err := d.Deploy(ctx, Options{
		Name:       "users",
		Namespace:  "default",
		Image:      "users:v1",
		Port:       3000,
		Domain:     "app.kipper.run",
		RouteGroup: "app",
		RoutePath:  "/api/users",
	})
	require.NoError(t, err)

	app, _ := dynClient.Resource(AppGVR).Namespace("default").Get(ctx, "users", metav1.GetOptions{})
	group, _, _ := unstructured.NestedString(app.Object, "spec", "route", "group")
	path, _, _ := unstructured.NestedString(app.Object, "spec", "route", "path")
	assert.Equal(t, "app", group)
	assert.Equal(t, "/api/users", path)
}

func TestRestartNotFoundReturnsError(t *testing.T) {
	d, _ := testDeployer()

	err := d.Restart(context.Background(), "default", "nonexistent")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestUpdateImageNotFoundReturnsError(t *testing.T) {
	d, _ := testDeployer()

	err := d.UpdateImage(context.Background(), "default", "nonexistent", "nginx:v2")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestScaleNotFoundReturnsError(t *testing.T) {
	d, _ := testDeployer()

	err := d.Scale(context.Background(), "default", "nonexistent", 3)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}
