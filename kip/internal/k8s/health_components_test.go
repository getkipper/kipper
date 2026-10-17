package k8s

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func platformConfig(spec map[string]interface{}) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{"spec": spec}}
}

func labels(checks []healthCheck) []string {
	var out []string
	for _, c := range checks {
		out = append(out, c.label)
	}
	return out
}

func TestHealthChecksLeaveOutWhatTheClusterDoesNotRun(t *testing.T) {
	got := labels(installedChecks(platformConfig(map[string]interface{}{"profile": "nano", "storage": "local", "backup": "none"})))
	assert.NotContains(t, got, "Longhorn")
	assert.NotContains(t, got, "Velero", "a cluster without backups runs no Velero")
	assert.NotContains(t, got, "Loki")
	assert.NotContains(t, got, "Promtail")
	assert.NotContains(t, got, "Prometheus")
	assert.NotContains(t, got, "Grafana")
	assert.Contains(t, got, "Traefik")
	assert.Contains(t, got, "Console API")

	got = labels(installedChecks(platformConfig(map[string]interface{}{
		"profile":    "nano",
		"components": []interface{}{map[string]interface{}{"name": "loki", "enabled": true}},
	})))
	assert.Contains(t, got, "Longhorn", "a legacy nano runs Longhorn")
	assert.Contains(t, got, "Loki", "an explicit enable is checked")
	assert.Contains(t, got, "Promtail")
	assert.NotContains(t, got, "Prometheus")
}

func TestHealthChecksCoverEverythingWithoutAPlatformConfig(t *testing.T) {
	assert.Equal(t, labels(healthChecks), labels(installedChecks(nil)))
}

func TestStorageModeOf(t *testing.T) {
	assert.Equal(t, "local", storageModeOf(platformConfig(map[string]interface{}{"storage": "local"})))
	assert.Equal(t, "longhorn", storageModeOf(platformConfig(map[string]interface{}{"profile": "small"})), "a legacy cluster runs Longhorn")
	assert.Equal(t, "longhorn", storageModeOf(nil), "an unreadable PlatformConfig reads as Longhorn")
}
