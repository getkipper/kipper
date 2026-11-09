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

// Each hop is the latest patch of its minor as of 2026-11-09. 1.12.0 and
// 1.12.1 carry the rebuild bug in Longhorn issue 13571 and must never be a stop.
func TestLonghornPathUsesTheCheckedPatches(t *testing.T) {
	var versions []string
	for _, r := range longhornPath {
		versions = append(versions, r.Version)
	}
	assert.Equal(t, []string{"v1.7.3", "v1.8.2", "v1.9.2", "v1.10.2", "v1.11.3", "v1.12.2"}, versions)
	for _, v := range versions {
		assert.NotContains(t, []string{"v1.12.0", "v1.12.1"}, v)
	}
}

// The floors come from each release's notes: 1.11.3 ships a CSI
// external-provisioner that needs Kubernetes 1.34, and 1.12.2 ships an older
// one that accepts 1.25.
func TestLonghornPathKubernetesFloors(t *testing.T) {
	for version, floor := range map[string]string{"v1.11.3": "v1.34", "v1.12.2": "v1.25"} {
		r, ok := longhornReleaseFor(version)
		require.True(t, ok, version)
		assert.Equal(t, floor, r.MinKubernetes, version)
		assert.Equal(t, 8, r.EngineCLIAPIMin, version)
		assert.Equal(t, 4, r.EngineControllerAPIMin, version)
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
	assert.Equal(t, []string{"v1.8.2", "v1.9.2", "v1.10.2", "v1.11.3", "v1.12.2"}, plan.Hops)
}

func TestPlanLonghornUpgradeFromTheLastPin(t *testing.T) {
	plan, err := PlanLonghornUpgrade("v1.11.3")
	require.NoError(t, err)
	assert.Equal(t, []string{"v1.12.2"}, plan.Hops)
}

// A cluster someone moved to 1.12.0 or 1.12.1 by hand gets the patch with the
// fix for Longhorn issue 13571.
func TestPlanLonghornUpgradeMovesAnAvoidedPatchToTheFixedOne(t *testing.T) {
	for _, from := range []string{"v1.12.0", "v1.12.1"} {
		plan, err := PlanLonghornUpgrade(from)
		require.NoError(t, err, from)
		assert.Equal(t, []string{"v1.12.2"}, plan.Hops, from)
	}
}

func TestPlanLonghornUpgradeTakesTheLatestPatchOfItsOwnMinorFirst(t *testing.T) {
	plan, err := PlanLonghornUpgrade("v1.9.0")
	require.NoError(t, err)
	assert.Equal(t, []string{"v1.9.2", "v1.10.2", "v1.11.3", "v1.12.2"}, plan.Hops)

	plan, err = PlanLonghornUpgrade("v1.10.0-hotfix-1")
	require.NoError(t, err)
	assert.Equal(t, []string{"v1.10.2", "v1.11.3", "v1.12.2"}, plan.Hops)
}

func TestPlanLonghornUpgradeAtTheTargetHasNoHops(t *testing.T) {
	plan, err := PlanLonghornUpgrade(longhornVersion)
	require.NoError(t, err)
	assert.Empty(t, plan.Hops)
}

func TestPlanLonghornUpgradeRefusesWhatItCannotPlace(t *testing.T) {
	for in, want := range map[string]string{
		"v1.12.3": "newer than",
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
	_, ok = longhornReleaseFor("v1.13.0")
	assert.False(t, ok)

	r, ok = longhornReleaseFor("v1.10.1")
	require.True(t, ok, "another patch of a minor on the path shares its requirements")
	assert.Equal(t, "v1.10.2", r.Version)
}
