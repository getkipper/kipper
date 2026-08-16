package cmd

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/getkipper/kipper/controller/pkg/platform"
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
	monitoringOff := componentLine("prometheus", "nano", nil, nil, nil)
	assert.Contains(t, monitoringOff, "off", "monitoring components default to off on nano")

	traefikOn := componentLine("traefik", "nano", nil, nil, nil)
	assert.Contains(t, traefikOn, "on", "traefik keeps running on nano")

	kedaOn := componentLine("keda", "nano", nil, nil, nil)
	assert.Contains(t, kedaOn, "on")

	veleroOn := componentLine("velero", "nano", nil, nil, nil)
	assert.Contains(t, veleroOn, "on")
}

func TestIsKnownProfile(t *testing.T) {
	for _, p := range []string{"nano", "small", "medium", "large", "xlarge"} {
		assert.True(t, isKnownProfile(p), p)
	}
	assert.False(t, isKnownProfile("comfortable"))
	assert.False(t, isKnownProfile(""))
}

// The docs tell an operator to resize kube-state-metrics by name, and the CLI
// validates names against the path table. A component that is sized in the
// chart but missing from that table would reject the command the docs give.
func TestResizeAcceptsEveryComponentTheChartSizes(t *testing.T) {
	for _, name := range []string{"kube-state-metrics", "grafana", "prometheus"} {
		if err := validateComponent(name); err != nil {
			t.Errorf("kip platform resize %s: %v", name, err)
		}
	}
}

// A component that shares a chart runs or does not run with its sibling.
// kube-state-metrics and grafana live in kube-prometheus-stack, so a status
// line calling them "on" while prometheus is off describes pods that are not
// deployed.
func TestPlatformStatusFollowsTheChartSibling(t *testing.T) {
	off := false
	promDisabled := map[string]*bool{platform.ComponentPrometheus: &off}

	for _, name := range []string{platform.ComponentGrafana, platform.ComponentKubeStateMetrics} {
		line := componentLine(name, platform.ProfileMedium, nil, nil, promDisabled)
		if !strings.HasPrefix(line, "off") {
			t.Errorf("%s should read off when prometheus is disabled, got %q", name, line)
		}
	}

	// Loki's sibling is untouched by that, and traefik runs regardless.
	for _, name := range []string{platform.ComponentPromtail, platform.ComponentTraefik} {
		line := componentLine(name, platform.ProfileMedium, nil, nil, promDisabled)
		if !strings.HasPrefix(line, "on") {
			t.Errorf("%s should be unaffected by prometheus, got %q", name, line)
		}
	}
}

// nano ships without the monitoring stack, and everything inside that chart
// goes with it.
func TestPlatformStatusOnNanoShowsTheMonitoringStackOff(t *testing.T) {
	for _, name := range []string{
		platform.ComponentPrometheus,
		platform.ComponentGrafana,
		platform.ComponentKubeStateMetrics,
		platform.ComponentLoki,
		platform.ComponentPromtail,
	} {
		line := componentLine(name, platform.ProfileNano, nil, nil, nil)
		if !strings.HasPrefix(line, "off") {
			t.Errorf("%s should read off on nano, got %q", name, line)
		}
	}

	line := componentLine(platform.ComponentTraefik, platform.ProfileNano, nil, nil, nil)
	if !strings.HasPrefix(line, "on") {
		t.Errorf("traefik keeps running on nano, got %q", line)
	}
}

// Every platform component the CLI will resize should also be restartable:
// they are the same components, and a name that resizes but does not restart is
// a gap nobody discovers until they need the restart.
//
// The set is derived from the path table rather than listed here. A hand-kept
// list would pass for the next component added to one registry and forgotten in
// the other, which is the whole failure this pins.
func TestEveryResizableComponentCanBeRestarted(t *testing.T) {
	for _, name := range platform.SupportedComponents() {
		target, ok := restartTargets[name]
		if !ok {
			t.Errorf("%s can be resized but 'kip platform restart %s' has no target", name, name)
			continue
		}
		assert.NotEmpty(t, target.Namespace, name)
		assert.NotEmpty(t, target.Kind, name)
		assert.NotEmpty(t, target.Name, name)
	}
}
