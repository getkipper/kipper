package deployer

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestSetRouteGivesAnAppWithoutOneARoute(t *testing.T) {
	d, dynClient := testDeployer()
	ctx := context.Background()
	seedApp(t, dynClient, map[string]interface{}{"image": "web:1", "port": int64(8080)})

	require.NoError(t, d.SetRoute(ctx, "default", "api", "api.example.com", ""))

	app, err := dynClient.Resource(AppGVR).Namespace("default").Get(ctx, "api", metav1.GetOptions{})
	require.NoError(t, err)
	route, _, _ := unstructured.NestedMap(app.Object, "spec", "route")
	assert.Equal(t, map[string]interface{}{"host": "api.example.com", "path": "/"}, route)
}

func TestSetRouteChangesOnlyWhatItIsGiven(t *testing.T) {
	d, dynClient := testDeployer()
	ctx := context.Background()
	seedApp(t, dynClient, map[string]interface{}{"image": "web:1", "port": int64(8080), "route": map[string]interface{}{
		"host": "old.example.com", "path": "/app", "basicAuth": true, "internalPaths": []interface{}{"/actuator"},
	}})

	require.NoError(t, d.SetRoute(ctx, "default", "api", "new.example.com", ""))

	app, err := dynClient.Resource(AppGVR).Namespace("default").Get(ctx, "api", metav1.GetOptions{})
	require.NoError(t, err)
	route, _, _ := unstructured.NestedMap(app.Object, "spec", "route")
	assert.Equal(t, "new.example.com", route["host"])
	assert.Equal(t, "/app", route["path"])
	assert.Equal(t, true, route["basicAuth"])
	assert.Equal(t, []interface{}{"/actuator"}, route["internalPaths"])
}

func TestRemoveRouteReportsTheGatesItDrops(t *testing.T) {
	d, dynClient := testDeployer()
	ctx := context.Background()
	seedApp(t, dynClient, map[string]interface{}{"image": "web:1", "port": int64(8080), "route": map[string]interface{}{
		"host": "api.example.com", "basicAuth": true, "requireApiKey": true,
		"internalPaths": []interface{}{"/actuator"}, "redirectFrom": []interface{}{"www.example.com"},
	}})

	dropped, err := d.RemoveRoute(ctx, "default", "api")

	require.NoError(t, err)
	assert.Equal(t, []string{"basic auth", "API key gate", "internal paths /actuator", "redirect domains www.example.com"}, dropped)
	app, err := dynClient.Resource(AppGVR).Namespace("default").Get(ctx, "api", metav1.GetOptions{})
	require.NoError(t, err)
	_, found, _ := unstructured.NestedMap(app.Object, "spec", "route")
	assert.False(t, found)
}

func TestRemoveRouteRefusesAnAppWithoutOne(t *testing.T) {
	d, dynClient := testDeployer()
	seedApp(t, dynClient, map[string]interface{}{"image": "web:1", "port": int64(8080)})

	_, err := d.RemoveRoute(context.Background(), "default", "api")

	assert.ErrorContains(t, err, "no route")
}

func TestRouteGatesNameAClosedRoute(t *testing.T) {
	gates := RouteGates(map[string]interface{}{"closed": true, "publicPaths": []interface{}{"/hook"}})

	assert.Equal(t, []string{"closed", "public paths /hook"}, gates)
}
