package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestSetComponentField_NewComponent(t *testing.T) {
	pc := &unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{},
	}}
	require.NoError(t, setComponentField(pc, "prometheus", "memoryLimit", "2Gi"))

	raw, found, err := unstructured.NestedSlice(pc.Object, "spec", "components")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, raw, 1)
	entry := raw[0].(map[string]interface{})
	assert.Equal(t, "prometheus", entry["name"])
	assert.Equal(t, "2Gi", entry["memoryLimit"])
}

func TestSetComponentField_UpdatesExisting(t *testing.T) {
	pc := &unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{
			"components": []interface{}{
				map[string]interface{}{"name": "prometheus", "memoryLimit": "1Gi"},
				map[string]interface{}{"name": "loki", "memoryLimit": "512Mi"},
			},
		},
	}}
	require.NoError(t, setComponentField(pc, "prometheus", "memoryLimit", "3Gi"))

	raw, _, _ := unstructured.NestedSlice(pc.Object, "spec", "components")
	require.Len(t, raw, 2, "should not duplicate the prometheus entry")

	byName := map[string]map[string]interface{}{}
	for _, item := range raw {
		m := item.(map[string]interface{})
		byName[m["name"].(string)] = m
	}
	assert.Equal(t, "3Gi", byName["prometheus"]["memoryLimit"])
	assert.Equal(t, "512Mi", byName["loki"]["memoryLimit"], "the unrelated loki override must survive")
}

func TestSetComponentField_BooleanEnabled(t *testing.T) {
	pc := &unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{},
	}}
	require.NoError(t, setComponentField(pc, "loki", "enabled", false))

	raw, _, _ := unstructured.NestedSlice(pc.Object, "spec", "components")
	entry := raw[0].(map[string]interface{})
	assert.Equal(t, false, entry["enabled"], "explicit false must round-trip")
}

func TestValidateComponent(t *testing.T) {
	// Profile-driven and flat components both come from the shared path
	// table in controller/pkg/platform now.
	assert.NoError(t, validateComponent("prometheus"))
	assert.NoError(t, validateComponent("loki"))
	assert.NoError(t, validateComponent("grafana"))
	assert.NoError(t, validateComponent("traefik"))
	assert.NoError(t, validateComponent("keda"))

	// Components that aren't yet HelmChart-managed (raw Deployments) stay
	// rejected — resize doesn't know how to find their memory field.
	assert.Error(t, validateComponent("dex"))
	assert.Error(t, validateComponent("console-api"))
	assert.Error(t, validateComponent(""))
}

func TestValidateToggleComponent_OnlyPromAndLoki(t *testing.T) {
	// enable/disable is chart-level. Until the reconciler honors the
	// Enabled flag for the other charts, the CLI must reject them so a
	// user can't persist an override that silently does nothing.
	assert.NoError(t, validateToggleComponent("prometheus"))
	assert.NoError(t, validateToggleComponent("loki"))
	assert.Error(t, validateToggleComponent("traefik"))
	assert.Error(t, validateToggleComponent("grafana"))
	assert.Error(t, validateToggleComponent("promtail"))
	assert.Error(t, validateToggleComponent(""))
}

func TestComponentLine_NanoOnlyDisablesMonitoring(t *testing.T) {
	// On nano, traefik / keda / velero charts keep running. Only the
	// monitoring stack should default to "off"; the other supported
	// components stay on.
	monitoringOff := componentLine("prometheus", "nano", nil, nil)
	assert.Contains(t, monitoringOff, "off", "monitoring components default to off on nano")

	traefikOn := componentLine("traefik", "nano", nil, nil)
	assert.Contains(t, traefikOn, "on", "traefik keeps running on nano")

	kedaOn := componentLine("keda", "nano", nil, nil)
	assert.Contains(t, kedaOn, "on")

	veleroOn := componentLine("velero", "nano", nil, nil)
	assert.Contains(t, veleroOn, "on")
}

func TestIsKnownProfile(t *testing.T) {
	for _, p := range []string{"nano", "small", "medium", "large", "xlarge"} {
		assert.True(t, isKnownProfile(p), p)
	}
	assert.False(t, isKnownProfile("comfortable"))
	assert.False(t, isKnownProfile(""))
}
