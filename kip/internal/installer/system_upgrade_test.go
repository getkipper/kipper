package installer

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSystemComponents(t *testing.T) {
	t.Run("returns the expected component set in declared order for medium profile", func(t *testing.T) {
		components := SystemComponents("example.com", nil, nil, PlatformState{Profile: ProfileMedium})
		// Order matters: traefik (ingress) must come before things that
		// rely on it; storage (longhorn) before workloads that need PVCs.
		// `cert-manager-dns` re-applies the cert-manager pod's
		// dnsConfig override (idempotent strategic merge patch) so
		// HTTP-01 self-checks keep bypassing the cluster's CoreDNS
		// chain even if the controller pod was recreated for some
		// other reason. A re-ordering should be deliberate and visible
		// in the diff.
		want := []string{
			"traefik",
			"cert-manager-dns",
			"security-middleware",
			"longhorn",
			"keda",
			"authz",
			"loki",
			"kube-prometheus-stack",
			"velero",
			"zot",
			"build-isolation",
		}
		got := make([]string, len(components))
		for i, c := range components {
			got[i] = c.Name
		}
		assert.Equal(t, want, got)
	})

	t.Run("omits monitoring stack on nano profile", func(t *testing.T) {
		components := SystemComponents("example.com", nil, nil, PlatformState{Profile: ProfileNano})
		got := make([]string, len(components))
		for i, c := range components {
			got[i] = c.Name
		}
		assert.NotContains(t, got, "loki", "nano profile should not upgrade loki")
		assert.NotContains(t, got, "kube-prometheus-stack",
			"nano profile should not upgrade kube-prometheus-stack")
		assert.Contains(t, got, "traefik")
		assert.Contains(t, got, "longhorn")
	})

	t.Run("explicit disable wins over profile default", func(t *testing.T) {
		// A user disabled Loki via `kip platform disable loki` on a medium
		// cluster. Upgrade must not bring it back.
		state := PlatformState{
			Profile:          ProfileMedium,
			EnabledOverrides: map[string]bool{"loki": false},
		}
		components := SystemComponents("example.com", nil, nil, state)
		got := make([]string, len(components))
		for i, c := range components {
			got[i] = c.Name
		}
		assert.NotContains(t, got, "loki", "explicit disable must be honored on upgrade")
		assert.Contains(t, got, "kube-prometheus-stack",
			"prometheus stays in the upgrade set because it wasn't disabled")
	})

	t.Run("explicit enable on nano keeps the component in the upgrade set", func(t *testing.T) {
		state := PlatformState{
			Profile:          ProfileNano,
			EnabledOverrides: map[string]bool{"prometheus": true},
		}
		components := SystemComponents("example.com", nil, nil, state)
		got := make([]string, len(components))
		for i, c := range components {
			got[i] = c.Name
		}
		assert.Contains(t, got, "kube-prometheus-stack",
			"an explicit enable must override the nano default")
	})

	t.Run("every component has an Apply function wired up", func(t *testing.T) {
		// A nil Apply would crash RunSystemUpgrade with a useless
		// stack trace. Catch the wiring mistake here instead.
		for _, profile := range []string{ProfileNano, ProfileSmall, ProfileMedium, ProfileLarge, ProfileXLarge} {
			components := SystemComponents("example.com", nil, nil, PlatformState{Profile: profile})
			for _, c := range components {
				assert.NotNilf(t, c.Apply, "component %q has nil Apply on profile %q", c.Name, profile)
			}
		}
	})
}

func TestPlatformStateEffectiveResources(t *testing.T) {
	t.Run("profile default when no override", func(t *testing.T) {
		state := PlatformState{Profile: ProfileMedium}
		got := state.EffectiveResources()
		assert.Equal(t, "1Gi", got.PrometheusMemoryLimit)
		assert.Equal(t, "512Mi", got.LokiMemoryLimit)
	})

	t.Run("memory override wins over profile default", func(t *testing.T) {
		state := PlatformState{
			Profile:         ProfileMedium,
			MemoryOverrides: map[string]string{"prometheus": "3Gi"},
		}
		got := state.EffectiveResources()
		assert.Equal(t, "3Gi", got.PrometheusMemoryLimit,
			"a previously-bumped Prometheus must not be downsized on upgrade")
		assert.Equal(t, "512Mi", got.LokiMemoryLimit,
			"untouched components keep the profile default")
	})

	t.Run("override below profile request clamps the request at upgrade time too", func(t *testing.T) {
		// The previous adversarial pass flagged that the reconciler had
		// the clamp but the upgrade path didn't. The clamp now lives in
		// platform.EffectiveResources so both code paths share it.
		state := PlatformState{
			Profile:         ProfileMedium,
			MemoryOverrides: map[string]string{"prometheus": "128Mi"},
		}
		got := state.EffectiveResources()
		assert.Equal(t, "128Mi", got.PrometheusMemoryRequest,
			"upgrade-time render must clamp request to limit, not produce request > limit")
		assert.Equal(t, "128Mi", got.PrometheusMemoryLimit)
	})
}
