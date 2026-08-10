package installer

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParsePlatformStateJSONPath(t *testing.T) {
	t.Run("empty input yields empty state", func(t *testing.T) {
		state := parsePlatformStateJSONPath("")
		assert.Empty(t, state.Profile)
		assert.Empty(t, state.MemoryOverrides)
		assert.Empty(t, state.EnabledOverrides)
	})

	t.Run("profile only when no components are set", func(t *testing.T) {
		state := parsePlatformStateJSONPath("medium|")
		assert.Equal(t, "medium", state.Profile)
		assert.Empty(t, state.MemoryOverrides)
		assert.Empty(t, state.EnabledOverrides)
	})

	t.Run("memory override is captured", func(t *testing.T) {
		state := parsePlatformStateJSONPath("large|prometheus=2Gi;\nloki=512Mi;\n")
		assert.Equal(t, "large", state.Profile)
		assert.Equal(t, "2Gi", state.MemoryOverrides["prometheus"])
		assert.Equal(t, "512Mi", state.MemoryOverrides["loki"])
		assert.Empty(t, state.EnabledOverrides)
	})

	t.Run("enabled false survives", func(t *testing.T) {
		state := parsePlatformStateJSONPath("medium|loki=;false\n")
		assert.Equal(t, "medium", state.Profile)
		v, ok := state.EnabledOverrides["loki"]
		assert.True(t, ok, "explicit false must be parsed and stored")
		assert.False(t, v)
	})

	t.Run("mixed overrides", func(t *testing.T) {
		state := parsePlatformStateJSONPath("xlarge|prometheus=3Gi;true\nloki=;false\n")
		assert.Equal(t, "xlarge", state.Profile)
		assert.Equal(t, "3Gi", state.MemoryOverrides["prometheus"])
		assert.NotContains(t, state.MemoryOverrides, "loki", "empty memory limit should not register an override")
		assert.True(t, state.EnabledOverrides["prometheus"])
		assert.False(t, state.EnabledOverrides["loki"])
	})
}

func TestPlatformStateReinstallScenario(t *testing.T) {
	// A user previously ran `kip platform disable loki` on a medium cluster.
	// PlatformConfig records the override. They then re-run `kip install`.
	// The installer must consult the CR rather than blindly re-applying
	// Loki from the freshly-detected medium profile.
	state := PlatformState{
		Profile:          ProfileMedium,
		EnabledOverrides: map[string]bool{"loki": false},
	}
	assert.False(t, state.LokiEnabled(), "explicit disable must win on reinstall")
	assert.True(t, state.PrometheusEnabled(), "unrelated components stay at profile default (enabled on medium)")
}
