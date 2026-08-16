package platform

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

func TestKubePrometheusStackHelmChart_NoPublicGrafanaIngress(t *testing.T) {
	// Grafana's provisioned datasource spans every tenant's logs and metrics,
	// so it must not be published on a public ingress. A regression that
	// re-enables the ingress would expose all-tenant logs to the internet.
	res := ResourcesForProfile(ProfileMedium)
	got := KubePrometheusStackHelmChart(res)

	assert.Contains(t, got, "ingress:\n        enabled: false",
		"Grafana must not carry a public ingress")
	assert.NotContains(t, got, "letsencrypt-prod",
		"no public TLS ingress should be provisioned for Grafana")
	// The Loki datasource is still wired for in-cluster (port-forward) access.
	assert.Contains(t, got, "type: loki")
}

func TestKubePrometheusStackHelmChart_GrafanaAdminFromSecret(t *testing.T) {
	// The admin password must come from the generated grafana-admin Secret,
	// never a static default baked into the chart values.
	got := KubePrometheusStackHelmChart(ResourcesForProfile(ProfileMedium))

	assert.NotContains(t, got, "adminPassword: kipper",
		"Grafana must not ship a static admin password")
	assert.NotContains(t, got, "adminPassword:",
		"Grafana admin password must come from the existing Secret, not a values field")
	assert.Contains(t, got, "existingSecret: "+GrafanaAdminSecretName)
	assert.Contains(t, got, "userKey: "+GrafanaAdminUserKey)
	assert.Contains(t, got, "passwordKey: "+GrafanaAdminPasswordKey)
}

// kube-state-metrics holds every watched object in memory, and an API server
// restart makes it re-list all of them at once. On labb-tools, steady state was
// 23Mi against a 64Mi limit, and the re-list after a control-plane restart
// OOM-killed it nine times before one attempt got through. The limit has to
// leave room for that spike rather than for the steady state.
func TestKubePrometheusStackHelmChart_KubeStateMetricsSurvivesAResync(t *testing.T) {
	chart := KubePrometheusStackHelmChart(ResourcesForProfile(ProfileNano))

	block := kubeStateMetricsBlock(t, chart)
	if !strings.Contains(block, "memory: "+kubeStateMetricsMemoryLimit) {
		t.Errorf("kube-state-metrics limit should be %s, got block:\n%s", kubeStateMetricsMemoryLimit, block)
	}

	limit := resource.MustParse(kubeStateMetricsMemoryLimit)
	steady := resource.MustParse("33Mi") // the highest steady-state reading across the fleet
	if limit.Value() < 4*steady.Value() {
		t.Errorf("a limit of %s leaves too little headroom over a %s steady state for a full re-list",
			kubeStateMetricsMemoryLimit, steady.String())
	}
}

// The path table says its flat defaults mirror what the installer seeds into
// the chart. Nothing enforced that, so a raised default in one place would sit
// beside the old value in the other and the drift would only show as a resize
// that appears to change nothing.
//
// The assertion reads the component's own declared path rather than searching
// the document: several components share the same quantities, so "192Mi occurs
// somewhere" would pass while kube-state-metrics carried 64Mi.
func TestComponentDefaultsMirrorTheSeededChart(t *testing.T) {
	values := seededValues(t, KubePrometheusStackHelmChart(ResourcesForProfile(ProfileNano)))

	for _, component := range []string{ComponentGrafana, ComponentKubeStateMetrics} {
		paths, ok := PathFor(component)
		if !ok {
			t.Fatalf("%s is not in the path table", component)
		}
		for _, field := range []struct {
			name string
			path []string
			want string
		}{
			{"request", paths.MemoryRequestPath, paths.DefaultMemoryRequest},
			{"limit", paths.MemoryLimitPath, paths.DefaultMemoryLimit},
		} {
			if field.want == "" {
				t.Fatalf("%s declares no flat %s default", component, field.name)
			}
			got, found, err := unstructured.NestedString(values, field.path...)
			require.NoError(t, err)
			require.True(t, found, "%s: the chart has nothing at %s", component, strings.Join(field.path, "."))
			assert.Equal(t, field.want, got,
				"%s %s: the path table and the seeded chart disagree", component, field.name)
		}
	}
}

// seededValues parses the valuesContent out of a rendered HelmChart, so a test
// can read a value where the reconciler will later look for it.
func seededValues(t *testing.T, chart string) map[string]interface{} {
	t.Helper()
	var doc struct {
		Spec struct {
			ValuesContent string `json:"valuesContent"`
		} `json:"spec"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(chart), &doc))
	require.NotEmpty(t, doc.Spec.ValuesContent, "the chart carries no valuesContent")

	var values map[string]interface{}
	require.NoError(t, yaml.Unmarshal([]byte(doc.Spec.ValuesContent), &values))
	return values
}

// kubeStateMetricsBlock returns the chart's kube-state-metrics section, so an
// assertion cannot pass on a value that belongs to another component.
func kubeStateMetricsBlock(t *testing.T, chart string) string {
	t.Helper()
	const marker = "    kube-state-metrics:\n"
	start := strings.Index(chart, marker)
	if start < 0 {
		t.Fatal("the chart carries no kube-state-metrics section")
	}
	rest := chart[start+len(marker):]
	if end := strings.Index(rest, "\n    prometheus-node-exporter:"); end >= 0 {
		return rest[:end]
	}
	return rest
}
