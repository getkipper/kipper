package platform

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	networkingv1 "k8s.io/api/networking/v1"
	"sigs.k8s.io/yaml"
)

func grafanaValues(t *testing.T) map[string]interface{} {
	t.Helper()
	values := seededValues(t, KubePrometheusStackHelmChart(ResourcesForProfile(ProfileMedium)))
	g, ok := values["grafana"].(map[string]interface{})
	require.True(t, ok, "no grafana block")
	return g
}

func nested(t *testing.T, m map[string]interface{}, keys ...string) interface{} {
	t.Helper()
	var cur interface{} = m
	for _, k := range keys {
		mm, ok := cur.(map[string]interface{})
		require.True(t, ok, "%v is not a map at %q", keys, k)
		cur = mm[k]
	}
	return cur
}

func TestKubePrometheusStackHelmChart_GrafanaAuthProxy(t *testing.T) {
	g := grafanaValues(t)

	proxy := nested(t, g, "grafana.ini", "auth.proxy").(map[string]interface{})
	assert.Equal(t, true, proxy["enabled"])
	assert.Equal(t, "X-WEBAUTH-USER", proxy["header_name"])
	assert.Equal(t, "email", proxy["header_property"])
	assert.Equal(t, true, proxy["auto_sign_up"])
	assert.EqualValues(t, 0, proxy["sync_ttl"], "the role header must apply on every request")
	assert.Equal(t, "Role:X-WEBAUTH-ROLE", proxy["headers"])
	assert.Equal(t, false, proxy["enable_login_token"], "proxy users must get no Grafana session cookie")

	assert.Equal(t, true, nested(t, g, "grafana.ini", "auth", "disable_login_form"))
	assert.Equal(t, false, nested(t, g, "grafana.ini", "auth.anonymous", "enabled"))
	assert.Equal(t, false, nested(t, g, "grafana.ini", "users", "allow_sign_up"))
	assert.EqualValues(t, 0, nested(t, g, "grafana.ini", "live", "max_connections"), "no WebSocket may outlive a revoked grant")
	assert.Equal(t, true, nested(t, g, "grafana.ini", "security", "csrf_always_check"), "cookies are stripped, so the Origin check must not depend on one")

	basic, _ := nested(t, g, "grafana.ini").(map[string]interface{})["auth.basic"].(map[string]interface{})
	assert.NotEqual(t, false, basic["enabled"], "the sidecars reload provisioning over localhost with Basic auth")
}

func TestKubePrometheusStackHelmChart_GrafanaReadsRootURLAndMarksAuth(t *testing.T) {
	g := grafanaValues(t)

	env, ok := g["envFromConfigMaps"].([]interface{})
	require.True(t, ok, "envFromConfigMaps must be a list")
	require.Len(t, env, 1)
	entry := env[0].(map[string]interface{})
	assert.Equal(t, GrafanaServerConfigMapName, entry["name"], "entries are objects with a name, as the chart's pod template expects")
	assert.Equal(t, true, entry["optional"], "Grafana must start before the root-URL ConfigMap exists")

	assert.Equal(t, GrafanaAuthVersion, nested(t, g, "podAnnotations", GrafanaAuthAnnotation))
	assert.Equal(t, false, nested(t, g, "ingress", "enabled"), "the chart's own ingress stays off; Kipper publishes the gated route")
}

func grafanaPolicy(t *testing.T, peer IngressPeer) *networkingv1.NetworkPolicy {
	t.Helper()
	var np networkingv1.NetworkPolicy
	require.NoError(t, yaml.Unmarshal([]byte(GrafanaNetworkPolicy(peer)), &np))
	return &np
}

func TestGrafanaNetworkPolicy_AdmitsOnlyIngressAndPrometheus(t *testing.T) {
	np := grafanaPolicy(t, IngressPeer{Namespace: "traefik", LabelKey: "app", LabelValue: "edge"})

	assert.Equal(t, MonitoringNamespace, np.Namespace)
	assert.Equal(t, GrafanaPodLabels(), np.Spec.PodSelector.MatchLabels)
	assert.Equal(t, "grafana", np.Spec.PodSelector.MatchLabels["app.kubernetes.io/name"])
	assert.Equal(t, KubePrometheusStackRelease, np.Spec.PodSelector.MatchLabels["app.kubernetes.io/instance"],
		"the chart labels Grafana's pods with the release name; any other value selects nothing and leaves the port open")
	assert.Equal(t, []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}, np.Spec.PolicyTypes)
	require.Len(t, np.Spec.Ingress, 1)
	rule := np.Spec.Ingress[0]
	require.Len(t, rule.Ports, 1)
	assert.Equal(t, int32(3000), rule.Ports[0].Port.IntVal, "the container port, not the Service port")

	require.Len(t, rule.From, 2)
	for _, peer := range rule.From {
		require.NotNil(t, peer.NamespaceSelector, "every peer is pinned to a namespace")
		require.NotNil(t, peer.PodSelector, "every peer is pinned to pod labels")
		assert.NotEmpty(t, peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"])
	}
	assert.Equal(t, "traefik", rule.From[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"])
	assert.Equal(t, "edge", rule.From[0].PodSelector.MatchLabels["app"])
	assert.Equal(t, MonitoringNamespace, rule.From[1].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"])
	assert.Equal(t, "prometheus", rule.From[1].PodSelector.MatchLabels["app.kubernetes.io/name"])
}

func TestGrafanaNetworkPolicy_UnknownIngressNeverWidens(t *testing.T) {
	// Without a configured namespace, a tenant pod labelled like Traefik in its
	// own namespace must not be admitted.
	np := grafanaPolicy(t, IngressPeer{})

	from := np.Spec.Ingress[0].From[0]
	assert.Equal(t, TraefikNamespace, from.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"])
	assert.Equal(t, "traefik", from.PodSelector.MatchLabels["app.kubernetes.io/name"])
}

func TestKubePrometheusStackHelmChart_ReleaseNameMatchesGrafanaPolicy(t *testing.T) {
	var doc struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(KubePrometheusStackHelmChart(ResourcesForProfile(ProfileMedium))), &doc))
	assert.Equal(t, KubePrometheusStackRelease, doc.Metadata.Name, "the HelmChart name is the release name the policy selects on")
}
