package platform

import (
	"maps"
	"slices"
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
// restart makes it re-list all of them at once. On a live cluster, steady state was
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

func TestKubePrometheusStackHelmChart_GrafanaRollsOutWithoutASurgePod(t *testing.T) {
	values := seededValues(t, KubePrometheusStackHelmChart(ResourcesForProfile(ProfileNano)))
	grafana, _ := values["grafana"].(map[string]interface{})
	strategy, _ := grafana["deploymentStrategy"].(map[string]interface{})
	require.NotNil(t, strategy, "grafana.deploymentStrategy is missing")
	assert.Equal(t, "RollingUpdate", strategy["type"], "Grafana should retain the rolling-update strategy.")
	rolling, _ := strategy["rollingUpdate"].(map[string]interface{})
	assert.EqualValues(t, 0, rolling["maxSurge"], "Grafana rollouts should require no surge pod.")
	assert.EqualValues(t, 1, rolling["maxUnavailable"])
}

// Pin the scrape interval so chart upgrades preserve the sample density needed
// by the traffic view's 30s CPU rate windows.
func TestKubePrometheusStackHelmChart_PinsTheCAdvisorScrapeInterval(t *testing.T) {
	values := seededValues(t, KubePrometheusStackHelmChart(Resources{}))
	kubelet, _ := values["kubelet"].(map[string]interface{})
	sm, _ := kubelet["serviceMonitor"].(map[string]interface{})
	if sm["cAdvisorInterval"] != "10s" {
		t.Fatalf("kubelet.serviceMonitor.cAdvisorInterval = %v, want 10s", sm["cAdvisorInterval"])
	}
}

// valueAt walks a nested values map and fails when a key on the path is absent.
func valueAt(t *testing.T, values map[string]interface{}, path ...string) interface{} {
	t.Helper()
	var current interface{} = values
	for _, key := range path {
		m, ok := current.(map[string]interface{})
		require.True(t, ok, "%v: %q is not under a map", path, key)
		current, ok = m[key]
		require.True(t, ok, "%v: %q is not set", path, key)
	}
	return current
}

func TestLokiHelmChart_SendsNoUsageReports(t *testing.T) {
	values := seededValues(t, LokiHelmChart(ResourcesForProfile(ProfileMedium), LokiDefaultStorage))

	assert.Equal(t, false, valueAt(t, values, "loki", "analytics", "reporting_enabled"))
}

func TestKubePrometheusStackHelmChart_GrafanaSendsNothingUpstream(t *testing.T) {
	values := seededValues(t, KubePrometheusStackHelmChart(ResourcesForProfile(ProfileMedium)))
	analytics := []string{"grafana", "grafana.ini", "analytics"}

	for _, key := range []string{"reporting_enabled", "check_for_updates", "check_for_plugin_updates"} {
		assert.Equal(t, false, valueAt(t, values, append(analytics, key)...), key)
	}
	assert.Equal(t, true, valueAt(t, values, "grafana", "grafana.ini", "plugins", "preinstall_disabled"))
	assert.Equal(t, true, valueAt(t, values, "grafana", "grafana.ini", "plugins", "public_key_retrieval_disabled"))
	assert.Equal(t, true, valueAt(t, values, "grafana", "grafana.ini", "security", "disable_gravatar"))
}

// Loki's compactor enforces retention_period. Loki 3.x requires a delete
// request store to start with retention enabled.
func TestLokiHelmChart_CompactorAppliesRetention(t *testing.T) {
	values := seededValues(t, LokiHelmChart(ResourcesForProfile(ProfileMedium), LokiDefaultStorage))

	assert.Equal(t, "72h", valueAt(t, values, "loki", "limits_config", "retention_period"))
	assert.Equal(t, true, valueAt(t, values, "loki", "compactor", "retention_enabled"))
	assert.Equal(t, "filesystem", valueAt(t, values, "loki", "compactor", "delete_request_store"))
	// Retention marker files must survive pod restarts, so keep them on the
	// persistent volume mounted at /var/loki.
	assert.Equal(t, "/var/loki/compactor", valueAt(t, values, "loki", "compactor", "working_directory"))
}

// Retention enables the deletion API by default. It must stay disabled
// because Loki runs without authentication.
func TestLokiHelmChart_KeepsTheDeletionAPIOff(t *testing.T) {
	values := seededValues(t, LokiHelmChart(ResourcesForProfile(ProfileMedium), LokiDefaultStorage))

	assert.Equal(t, false, valueAt(t, values, "loki", "auth_enabled"))
	assert.Equal(t, "disabled", valueAt(t, values, "loki", "limits_config", "deletion_mode"))
}

// The canary and its dependent Helm test use top-level chart values.
func TestLokiHelmChart_DeploysNoCanary(t *testing.T) {
	values := seededValues(t, LokiHelmChart(ResourcesForProfile(ProfileMedium), LokiDefaultStorage))

	assert.Equal(t, false, valueAt(t, values, "lokiCanary", "enabled"))
	assert.Equal(t, false, valueAt(t, values, "test", "enabled"))
}

func TestPromtailHelmChart_RunsUnconfinedOnlyBySELinuxType(t *testing.T) {
	values := seededValues(t, PromtailHelmChart())

	assert.Equal(t, "spc_t", valueAt(t, values, "containerSecurityContext", "seLinuxOptions", "type"))
	assert.Equal(t, "RuntimeDefault", valueAt(t, values, "containerSecurityContext", "seccompProfile", "type"))
	security, ok := valueAt(t, values, "containerSecurityContext").(map[string]interface{})
	require.True(t, ok)
	assert.ElementsMatch(t, []string{"seLinuxOptions", "seccompProfile"}, slices.Collect(maps.Keys(security)), "the chart's own hardening must not be overridden")
}

func nodeExporterValues(t *testing.T) map[string]interface{} {
	t.Helper()
	values := seededValues(t, KubePrometheusStackHelmChart(ResourcesForProfile(ProfileMedium)))
	block, ok := values["prometheus-node-exporter"].(map[string]interface{})
	require.True(t, ok, "the chart carries no prometheus-node-exporter section")
	return block
}

func nestedValue(t *testing.T, values map[string]interface{}, path ...string) interface{} {
	t.Helper()
	got, found, err := unstructured.NestedFieldNoCopy(values, path...)
	require.NoError(t, err)
	require.True(t, found, "missing value %s", strings.Join(path, "."))
	return got
}

// Keep the proxy on the port allowed by the host firewall and the exporter on loopback.
func TestKubePrometheusStackHelmChart_NodeExporterBehindRBACProxy(t *testing.T) {
	ne := nodeExporterValues(t)

	assert.Equal(t, true, nestedValue(t, ne, "kubeRBACProxy", "enabled"))
	assert.EqualValues(t, 9100, nestedValue(t, ne, "service", "port"),
		"the proxy listens on service.port, which the peer firewall rules open")
	assert.EqualValues(t, 8100, nestedValue(t, ne, "kubeRBACProxy", "port"),
		"the exporter moves to 127.0.0.1 on kubeRBACProxy.port")
	assert.Equal(t, true, nestedValue(t, ne, "kubeRBACProxy", "tls", "enabled"))
	assert.Equal(t, true, nestedValue(t, ne, "tlsSecret", "enabled"))
	assert.Equal(t, NodeExporterCertName, nestedValue(t, ne, "tlsSecret", "secretName"))
}

func TestKubePrometheusStackHelmChart_NodeExporterScrapedOverVerifiedTLS(t *testing.T) {
	ne := nodeExporterValues(t)

	assert.Equal(t, "https", nestedValue(t, ne, "prometheus", "monitor", "scheme"))
	assert.Equal(t, "/var/run/secrets/kubernetes.io/serviceaccount/token",
		nestedValue(t, ne, "prometheus", "monitor", "bearerTokenFile"))
	tlsConfig, ok := nestedValue(t, ne, "prometheus", "monitor", "tlsConfig").(map[string]interface{})
	require.True(t, ok)
	assert.NotContains(t, tlsConfig, "insecureSkipVerify")
	assert.Equal(t, NodeExporterServerName, tlsConfig["serverName"])
	assert.Equal(t, NodeExporterCertName, nestedValue(t, tlsConfig, "ca", "secret", "name"))
	assert.Equal(t, "ca.crt", nestedValue(t, tlsConfig, "ca", "secret", "key"))
}

func TestNodeExporterTLSManifests_IssueTheProxyCertificate(t *testing.T) {
	var docs []map[string]interface{}
	for _, part := range strings.Split(NodeExporterTLSManifests(), "\n---\n") {
		var doc map[string]interface{}
		require.NoError(t, yaml.Unmarshal([]byte(part), &doc))
		docs = append(docs, doc)
	}

	byName := map[string]map[string]interface{}{}
	for _, doc := range docs {
		u := unstructured.Unstructured{Object: doc}
		assert.Equal(t, MonitoringNamespace, u.GetNamespace(), "%s/%s", u.GetKind(), u.GetName())
		byName[u.GetKind()+"/"+u.GetName()] = doc
	}

	serving, ok := byName["Certificate/"+NodeExporterCertName]
	require.True(t, ok, "no serving Certificate named %s", NodeExporterCertName)
	assert.Equal(t, NodeExporterCertName, nestedValue(t, serving, "spec", "secretName"))
	assert.Equal(t, []interface{}{NodeExporterServerName}, nestedValue(t, serving, "spec", "dnsNames"))

	issuerName, _ := nestedValue(t, serving, "spec", "issuerRef", "name").(string)
	issuer, ok := byName["Issuer/"+issuerName]
	require.True(t, ok, "the serving Certificate's issuer %q is not in the manifests", issuerName)
	caSecret, _ := nestedValue(t, issuer, "spec", "ca", "secretName").(string)
	require.NotEmpty(t, caSecret, "the serving issuer must be a CA issuer")

	var caCert map[string]interface{}
	for _, doc := range docs {
		u := unstructured.Unstructured{Object: doc}
		if u.GetKind() == "Certificate" && nestedValue(t, doc, "spec", "secretName") == caSecret {
			caCert = doc
		}
	}
	require.NotNil(t, caCert, "no Certificate writes the CA secret %s", caSecret)
	assert.Equal(t, true, nestedValue(t, caCert, "spec", "isCA"))
}

// Preserve the immutable claim template even when its claim has expanded.
func TestLokiStorageSize(t *testing.T) {
	assert.Equal(t, "5Gi", LokiDefaultStorage)
	assert.Equal(t, "5Gi", LokiStorageSize("", ""), "fresh install")
	assert.Equal(t, "10Gi", LokiStorageSize("10Gi", "10Gi"), "existing cluster")
	assert.Equal(t, "10Gi", LokiStorageSize("10Gi", "20Gi"), "a claim expanded under an unchanged template keeps the template")
	assert.Equal(t, "10Gi", LokiStorageSize("", "10Gi"), "only the claim is left after a disable")
	assert.Equal(t, "10Gi", LokiStorageSize("10Gi", ""), "template without a bound claim")
}

func TestLokiHelmChart_RequestsTheGivenStorage(t *testing.T) {
	values := seededValues(t, LokiHelmChart(ResourcesForProfile(ProfileMedium), "10Gi"))
	assert.Equal(t, "10Gi", valueAt(t, values, "singleBinary", "persistence", "size"))
	values = seededValues(t, LokiHelmChart(ResourcesForProfile(ProfileMedium), LokiDefaultStorage))
	assert.Equal(t, "5Gi", valueAt(t, values, "singleBinary", "persistence", "size"))
}
