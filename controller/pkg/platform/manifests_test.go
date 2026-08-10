package platform

import (
	"testing"

	"github.com/stretchr/testify/assert"
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
