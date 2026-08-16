package platform

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSupportedComponents(t *testing.T) {
	got := SupportedComponents()
	want := []string{
		ComponentGrafana,
		ComponentKeda,
		ComponentKubeStateMetrics,
		ComponentLoki,
		ComponentPrometheus,
		ComponentPromtail,
		ComponentTraefik,
		ComponentVelero,
	}
	sort.Strings(want)
	assert.Equal(t, want, got)
}

func TestPathFor_KnownAndUnknown(t *testing.T) {
	p, ok := PathFor(ComponentPrometheus)
	require.True(t, ok)
	assert.Equal(t, chartKubePrometheusStack, p.ChartName)
	assert.Equal(t, []string{"prometheus", "prometheusSpec", "resources", "limits", "memory"}, p.MemoryLimitPath)

	_, ok = PathFor("dex")
	assert.False(t, ok, "components not yet served by HelmChart resize should miss")
}

func TestComponentsByChart_GroupsMultiComponentCharts(t *testing.T) {
	groups := ComponentsByChart()

	prom := groups[chartKubePrometheusStack]
	assert.Contains(t, prom, ComponentPrometheus)
	assert.Contains(t, prom, ComponentGrafana)
	assert.Contains(t, prom, ComponentKubeStateMetrics)
	assert.Len(t, prom, 3, "prometheus, grafana and kube-state-metrics share kube-prometheus-stack")

	assert.Equal(t, []string{ComponentLoki}, groups[chartLoki])
	assert.Equal(t, []string{ComponentPromtail}, groups[chartPromtail])
	assert.Equal(t, []string{ComponentTraefik}, groups[chartTraefik])
	assert.Equal(t, []string{ComponentKeda}, groups[chartKeda])
	assert.Equal(t, []string{ComponentVelero}, groups[chartVelero])
}

func TestEffectiveLimit_OverrideWins(t *testing.T) {
	tests := []struct {
		name      string
		component string
		profile   string
		override  string
		want      string
	}{
		{"override always wins", ComponentPrometheus, ProfileMedium, "3Gi", "3Gi"},
		{"prom medium default", ComponentPrometheus, ProfileMedium, "", "1Gi"},
		{"prom xlarge default", ComponentPrometheus, ProfileXLarge, "", "2Gi"},
		{"loki medium default", ComponentLoki, ProfileMedium, "", "512Mi"},
		{"grafana flat default", ComponentGrafana, ProfileMedium, "", "128Mi"},
		{"grafana ignores profile", ComponentGrafana, ProfileXLarge, "", "128Mi"},
		{"promtail flat default", ComponentPromtail, ProfileMedium, "", "128Mi"},
		{"traefik flat default", ComponentTraefik, ProfileMedium, "", "256Mi"},
		{"keda flat default", ComponentKeda, ProfileMedium, "", "256Mi"},
		{"velero flat default", ComponentVelero, ProfileMedium, "", "512Mi"},
		{"unknown component returns empty", "dex", ProfileMedium, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EffectiveLimit(tt.component, tt.profile, tt.override)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestEffectiveRequest_ClampsAgainstLimit(t *testing.T) {
	// Profile-default request would be 512Mi for prometheus on medium; if
	// the user overrides the limit down to 256Mi the request must clamp.
	got := EffectiveRequest(ComponentPrometheus, ProfileMedium, "256Mi")
	assert.Equal(t, "256Mi", got, "request clamps down to limit when override is smaller")

	// Override above default leaves request at the profile default.
	got = EffectiveRequest(ComponentPrometheus, ProfileMedium, "4Gi")
	assert.Equal(t, "512Mi", got)

	// Flat components surface their declared default request.
	got = EffectiveRequest(ComponentGrafana, ProfileMedium, "")
	assert.Equal(t, "64Mi", got)

	// Unknown components return "" — caller must check PathFor first.
	got = EffectiveRequest("dex", ProfileMedium, "")
	assert.Equal(t, "", got)
}

func TestComponentPaths_HavePathsAndChartName(t *testing.T) {
	// Cheap structural assertion: every entry must be fully populated so
	// the reconciler can't accidentally pass an empty path slice into
	// rewriteValuesContent (which would no-op silently and look fine).
	for _, name := range SupportedComponents() {
		p, ok := PathFor(name)
		require.True(t, ok, "PathFor(%s) must succeed", name)
		assert.NotEmpty(t, p.ChartName, "%s missing chart name", name)
		assert.NotEmpty(t, p.MemoryRequestPath, "%s missing request path", name)
		assert.NotEmpty(t, p.MemoryLimitPath, "%s missing limit path", name)
		assert.NotEmpty(t, p.MemoryMin, "%s missing min bound", name)
		assert.NotEmpty(t, p.MemoryMax, "%s missing max bound", name)
	}
}

func TestIsToggleable(t *testing.T) {
	assert.True(t, IsToggleable(ComponentPrometheus))
	assert.True(t, IsToggleable(ComponentLoki))
	assert.False(t, IsToggleable(ComponentGrafana), "grafana follows prometheus")
	assert.False(t, IsToggleable(ComponentPromtail), "promtail follows loki")
	assert.False(t, IsToggleable(ComponentTraefik), "traefik is always on")
	assert.False(t, IsToggleable(ComponentKeda))
	assert.False(t, IsToggleable(ComponentVelero))
	assert.False(t, IsToggleable("dex"), "raw-Deployment components aren't in the table")
}

func TestEffectiveEnabled(t *testing.T) {
	yes := true
	no := false

	tests := []struct {
		name     string
		comp     string
		profile  string
		ovs      map[string]*bool
		expected bool
	}{
		{"prom default on medium", ComponentPrometheus, ProfileMedium, nil, true},
		{"prom default off on nano", ComponentPrometheus, ProfileNano, nil, false},
		{"prom explicit off", ComponentPrometheus, ProfileMedium, map[string]*bool{ComponentPrometheus: &no}, false},
		{"prom explicit on overrides nano", ComponentPrometheus, ProfileNano, map[string]*bool{ComponentPrometheus: &yes}, true},

		{"grafana follows prom (off)", ComponentGrafana, ProfileMedium, map[string]*bool{ComponentPrometheus: &no}, false},
		{"grafana follows prom (on)", ComponentGrafana, ProfileMedium, nil, true},
		{"grafana off on nano via prom default", ComponentGrafana, ProfileNano, nil, false},

		{"promtail follows loki (off)", ComponentPromtail, ProfileMedium, map[string]*bool{ComponentLoki: &no}, false},

		{"traefik always on, ignores nano", ComponentTraefik, ProfileNano, nil, true},
		{"keda always on, ignores explicit override", ComponentKeda, ProfileMedium, map[string]*bool{ComponentKeda: &no}, true},
		{"velero always on", ComponentVelero, ProfileNano, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EffectiveEnabled(tt.comp, tt.ovs, tt.profile)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestValidateMemoryLimit(t *testing.T) {
	tests := []struct {
		name      string
		component string
		value     string
		wantErr   string
	}{
		{"prom in range", ComponentPrometheus, "2Gi", ""},
		{"prom at min", ComponentPrometheus, "256Mi", ""},
		{"prom at max", ComponentPrometheus, "8Gi", ""},
		{"prom below min", ComponentPrometheus, "128Mi", "below"},
		{"prom above max", ComponentPrometheus, "16Gi", "above"},
		{"grafana above max", ComponentGrafana, "1Gi", "above"},
		{"keda above max", ComponentKeda, "1Gi", "above"},
		{"malformed quantity", ComponentLoki, "twelve gigs", "valid Kubernetes quantity"},
		{"unknown component", "dex", "1Gi", "unknown component"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateMemoryLimit(tt.component, tt.value)
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}
