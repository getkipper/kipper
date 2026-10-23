package installer

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Longhorn refuses to skip a minor, so the path must hold one release per
// minor and end at the version kip installs.
func TestLonghornPathIsContiguousAndEndsAtTheTarget(t *testing.T) {
	require.NotEmpty(t, longhornPath)
	assert.Equal(t, longhornVersion, longhornPath[len(longhornPath)-1].Version)
	for i, r := range longhornPath {
		v, err := parseLonghornVersion(r.Version)
		require.NoError(t, err, r.Version)
		assert.Equal(t, 1, v.major)
		if i == 0 {
			continue
		}
		prev, _ := parseLonghornVersion(longhornPath[i-1].Version)
		assert.Equal(t, prev.minor+1, v.minor, "%s follows %s", r.Version, longhornPath[i-1].Version)
		assert.Positive(t, r.EngineCLIAPIMin)
		assert.Positive(t, r.EngineControllerAPIMin)
		assert.NotEmpty(t, r.MinKubernetes)
	}
}

// Each hop is the latest patch of its minor as of 2026-10-24. 1.12.0 and
// 1.12.1 carry the 13571 rebuild bug and must never be a stop.
func TestLonghornPathUsesTheCheckedPatches(t *testing.T) {
	var versions []string
	for _, r := range longhornPath {
		versions = append(versions, r.Version)
	}
	assert.Equal(t, []string{"v1.7.3", "v1.8.2", "v1.9.2", "v1.10.2", "v1.11.3"}, versions)
	assert.Equal(t, "v1.34", longhornPath[len(longhornPath)-1].MinKubernetes,
		"1.11.3 needs Kubernetes 1.34 for its CSI external-provisioner")
	for _, v := range versions {
		assert.NotContains(t, []string{"v1.12.0", "v1.12.1"}, v)
	}
}

func TestParseLonghornVersion(t *testing.T) {
	for in, want := range map[string]longhornVer{
		"v1.7.3":           {1, 7, 3},
		"v1.10.0-hotfix-1": {1, 10, 0},
		"v1.11.3":          {1, 11, 3},
	} {
		got, err := parseLonghornVersion(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	for _, bad := range []string{"", "1.7.3", "v1.7", "v1.7.3-rc1", "latest", "v1.7.3 "} {
		_, err := parseLonghornVersion(bad)
		assert.Error(t, err, bad)
	}
}

func TestPlanLonghornUpgradeFromTheOldPin(t *testing.T) {
	plan, err := PlanLonghornUpgrade("v1.7.3")
	require.NoError(t, err)
	assert.Equal(t, "v1.7.3", plan.From)
	assert.Equal(t, []string{"v1.8.2", "v1.9.2", "v1.10.2", "v1.11.3"}, plan.Hops)
}

func TestPlanLonghornUpgradeTakesTheLatestPatchOfItsOwnMinorFirst(t *testing.T) {
	plan, err := PlanLonghornUpgrade("v1.9.0")
	require.NoError(t, err)
	assert.Equal(t, []string{"v1.9.2", "v1.10.2", "v1.11.3"}, plan.Hops)

	plan, err = PlanLonghornUpgrade("v1.10.0-hotfix-1")
	require.NoError(t, err)
	assert.Equal(t, []string{"v1.10.2", "v1.11.3"}, plan.Hops)
}

func TestPlanLonghornUpgradeAtTheTargetHasNoHops(t *testing.T) {
	plan, err := PlanLonghornUpgrade(longhornVersion)
	require.NoError(t, err)
	assert.Empty(t, plan.Hops)
}

func TestPlanLonghornUpgradeRefusesWhatItCannotPlace(t *testing.T) {
	for in, want := range map[string]string{
		"v1.12.1": "newer than",
		"v1.13.0": "newer than",
		"v2.0.0":  "newer than",
		"v1.6.4":  "older than",
		"garbage": "cannot read",
	} {
		_, err := PlanLonghornUpgrade(in)
		assert.ErrorContains(t, err, want, in)
	}
}

func TestLonghornReleaseLooksUpAPathEntry(t *testing.T) {
	r, ok := longhornReleaseFor("v1.11.3")
	require.True(t, ok)
	assert.Equal(t, "v1.34", r.MinKubernetes)
	_, ok = longhornReleaseFor("v1.12.1")
	assert.False(t, ok)

	r, ok = longhornReleaseFor("v1.10.1")
	require.True(t, ok, "another patch of a minor on the path shares its requirements")
	assert.Equal(t, "v1.10.2", r.Version)
}
