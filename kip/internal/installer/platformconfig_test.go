package installer

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/getkipper/kipper/controller/pkg/platform"
)

func TestPickProfile(t *testing.T) {
	tests := []struct {
		name  string
		ramMB int
		want  string
	}{
		// nano: clearly below 4 GB.
		{"tiny 2 GB box", 2048, ProfileNano},
		{"3 GB reported", 3000, ProfileNano},
		{"just under nano threshold", 3499, ProfileNano},

		// small: marketed 4-8 GB.
		{"4 GB box reporting 3900 MB", 3900, ProfileSmall},
		{"4 GB box reporting 4096 MB", 4096, ProfileSmall},
		{"6 GB box", 6144, ProfileSmall},
		{"just under small ceiling", 7499, ProfileSmall},

		// medium: marketed 8-16 GB.
		{"8 GB box reporting 7700 MB", 7700, ProfileMedium},
		{"8 GB box reporting 8192 MB", 8192, ProfileMedium},
		{"12 GB box", 12288, ProfileMedium},
		{"just under medium ceiling", 14999, ProfileMedium},

		// large: marketed 16-32 GB.
		{"16 GB box reporting 15800 MB", 15800, ProfileLarge},
		{"16 GB box reporting 16384 MB", 16384, ProfileLarge},
		{"24 GB box", 24576, ProfileLarge},
		{"just under xlarge threshold", 29999, ProfileLarge},

		// xlarge: marketed > 32 GB.
		{"32 GB box reporting 30000 MB", 30000, ProfileXLarge},
		{"32 GB box reporting 32768 MB", 32768, ProfileXLarge},
		{"64 GB cluster node", 64253, ProfileXLarge},
		{"128 GB box", 131072, ProfileXLarge},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, pickProfile(tt.ramMB, 8))
		})
	}
}

func TestPickProfileCovers64GBProductionNode(t *testing.T) {
	// The xlarge profile gives Prometheus a higher memory limit on large hosts.
	assert.Equal(t, ProfileXLarge, pickProfile(64253, 16))
}

func TestPickProfileStepsDownWhenTheCPUsCannotCarryTheRAMPick(t *testing.T) {
	tests := []struct {
		name  string
		ramMB int
		cpus  int
		want  string
	}{
		{"8 GB on 2 vCPU drops from medium to small", 8192, 2, ProfileSmall},
		{"32 GB on 2 vCPU drops from xlarge to small", 32768, 2, ProfileSmall},
		{"4 GB on 2 vCPU stays small", 4096, 2, ProfileSmall},
		{"8 GB on 4 vCPU keeps medium", 8192, 4, ProfileMedium},
		{"8 GB on 3 vCPU keeps medium", 8192, 3, ProfileMedium},
		{"1 vCPU lands on nano, which is the floor", 8192, 1, ProfileNano},
		{"an unknown CPU count leaves the RAM pick alone", 8192, 0, ProfileMedium},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, pickProfile(tt.ramMB, tt.cpus))
		})
	}
}

func TestProfilePickNoteSaysWhenTheCPUsLoweredThePick(t *testing.T) {
	note := profilePickNote(8192, 2, ProfileSmall)
	assert.Contains(t, note, "small")
	assert.Contains(t, note, "medium")
	assert.Contains(t, note, "2 CPUs")
	assert.Contains(t, note, "500m")

	assert.Empty(t, profilePickNote(8192, 4, ProfileMedium), "the RAM pick was used as is")
	assert.Empty(t, profilePickNote(8192, 0, ProfileMedium), "no CPU count, no note")

	// Nano is the minimum even when its estimated CPU requests exceed capacity.
	floor := profilePickNote(8192, 1, ProfileNano)
	assert.Contains(t, floor, "1 CPU")
	assert.Contains(t, floor, "smallest")
	assert.NotContains(t, floor, "still fits")
}

func TestKeptProfileNotesCheckTheProfileThatStays(t *testing.T) {
	notes := keptProfileNotes(ProfileSmall, ProfileMedium, 2)
	if assert.Len(t, notes, 2) {
		assert.Contains(t, notes[0], "already runs the 'medium' profile")
		assert.Contains(t, notes[0], "kip platform profile set small")
		assert.Contains(t, notes[1], "500m")
	}
	assert.Len(t, keptProfileNotes(ProfileSmall, ProfileMedium, 4), 1, "medium fits 4 CPUs")
	assert.Empty(t, keptProfileNotes(ProfileMedium, ProfileMedium, 2))
	assert.Empty(t, keptProfileNotes(ProfileMedium, "", 2), "a fresh cluster has nothing recorded")
}

func TestResolveProfileHonorsAnExplicitChoice(t *testing.T) {
	sys := SystemInfo{RAMMB: 8192, CPUs: 2}
	assert.Equal(t, ProfileMedium, resolveProfile(ProfileMedium, "", sys))
	assert.Equal(t, ProfileNano, resolveProfile(ProfileNano, "", sys))
	assert.Equal(t, ProfileSmall, resolveProfile("", "", sys), "no --profile uses the RAM and CPU pick")
}

// Preflight must use the profile retained by the existing cluster.
func TestResolveProfileKeepsASavedProfile(t *testing.T) {
	sys := SystemInfo{RAMMB: 8192, CPUs: 2}
	assert.Equal(t, ProfileMedium, resolveProfile("", ProfileMedium, sys))
	assert.Equal(t, ProfileNano, resolveProfile(ProfileMedium, ProfileNano, sys))
}

func TestReadSavedPlatformStateAcceptsOnlyAKnownProfile(t *testing.T) {
	state := readSavedPlatformState(&scriptedRunner{out: "'small|loki=;false\nprometheus=1Gi;\n'\n"})
	assert.Equal(t, ProfileSmall, state.Profile)
	assert.False(t, state.LokiEnabled(), "the explicit disable travels with the profile")
	assert.True(t, state.PrometheusEnabled())
	assert.Equal(t, ProfileMedium, readSavedPlatformState(&scriptedRunner{out: "medium|"}).Profile)
	assert.Empty(t, readSavedPlatformState(&scriptedRunner{out: ""}).Profile, "no cluster, no CR")
	assert.Empty(t, readSavedPlatformState(&scriptedRunner{out: "error: the server could not be reached"}).Profile)
	assert.Empty(t, readSavedPlatformState(&scriptedRunner{err: errors.New("ssh: connection lost")}).Profile)
}

func TestResourcesForProfile(t *testing.T) {
	tests := []struct {
		profile   string
		promReq   string
		promLim   string
		lokiReq   string
		lokiLim   string
		reasoning string
	}{
		{
			profile:   ProfileSmall,
			promReq:   "256Mi",
			promLim:   "512Mi",
			lokiReq:   "128Mi",
			lokiLim:   "384Mi",
			reasoning: "4-8 GB box: monitoring runs but tight, matching the current pre-incident defaults",
		},
		{
			profile:   ProfileMedium,
			promReq:   "512Mi",
			promLim:   "1Gi",
			lokiReq:   "256Mi",
			lokiLim:   "512Mi",
			reasoning: "8-16 GB box: enough headroom for typical small-team clusters",
		},
		{
			profile:   ProfileLarge,
			promReq:   "512Mi",
			promLim:   "1Gi",
			lokiReq:   "256Mi",
			lokiLim:   "512Mi",
			reasoning: "16-32 GB box shares limits with medium; the tier exists for honest user-facing language",
		},
		{
			profile:   ProfileXLarge,
			promReq:   "1Gi",
			promLim:   "2Gi",
			lokiReq:   "512Mi",
			lokiLim:   "1Gi",
			reasoning: "64 GB production node: Prometheus 2 Gi would have absorbed the 2026-05-11 OOM at ~540 MB working set",
		},
	}

	for _, tt := range tests {
		t.Run(tt.profile, func(t *testing.T) {
			got := platform.ResourcesForProfile(tt.profile)
			assert.Equal(t, tt.promReq, got.PrometheusMemoryRequest, tt.reasoning)
			assert.Equal(t, tt.promLim, got.PrometheusMemoryLimit, tt.reasoning)
			assert.Equal(t, tt.lokiReq, got.LokiMemoryRequest, tt.reasoning)
			assert.Equal(t, tt.lokiLim, got.LokiMemoryLimit, tt.reasoning)
		})
	}
}

func TestResourcesForProfileFallsBackToSmall(t *testing.T) {
	// nano should never reach this function (install path skips observability)
	// but if it does, we return small's conservative numbers rather than
	// rendering an empty resources block in the manifest.
	got := platform.ResourcesForProfile(ProfileNano)
	assert.Equal(t, "512Mi", got.PrometheusMemoryLimit)
	assert.Equal(t, "384Mi", got.LokiMemoryLimit)

	// Unknown string (e.g. from a future profile not yet handled) hits the
	// same safe default.
	got = platform.ResourcesForProfile("garbage-from-future")
	assert.Equal(t, "512Mi", got.PrometheusMemoryLimit)
}

func TestIgnoredProfileFlagNamesTheProfileCommand(t *testing.T) {
	note := ignoredProfileNote(ProfileMedium, ProfileSmall)
	assert.Contains(t, note, "--profile small")
	assert.Contains(t, note, "'medium'")
	assert.Contains(t, note, "kip platform profile set small")
	assert.Empty(t, ignoredProfileNote(ProfileMedium, ProfileMedium))
	assert.Empty(t, ignoredProfileNote(ProfileMedium, ""), "no --profile given")
}
