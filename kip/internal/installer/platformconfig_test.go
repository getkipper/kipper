package installer

import (
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
			assert.Equal(t, tt.want, pickProfile(tt.ramMB))
		})
	}
}

func TestPickProfileCovers64GBProductionNode(t *testing.T) {
	// Concrete sanity check tied to the incident that motivated this work:
	// kipper.example.com (64 GB) must land in xlarge so Prometheus gets the
	// 2 Gi default that would have prevented the 2026-05-11 OOM.
	assert.Equal(t, ProfileXLarge, pickProfile(64253))
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
