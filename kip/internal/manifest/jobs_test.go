package manifest

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestJobRoundTripKeepsResourcesSecretsAndRetries(t *testing.T) {
	live := map[string]interface{}{
		"image":        "backup:1",
		"schedule":     "0 3 * * *",
		"resources":    map[string]interface{}{"cpuRequest": "100m", "cpuLimit": "500m", "memoryRequest": "128Mi", "memoryLimit": "256Mi"},
		"secretRefs":   []interface{}{"backup-credentials"},
		"backoffLimit": int64(5),
	}

	job := exportJob(live)
	got := convertJob("nightly", "shop-test", job).Object.Object["spec"]

	assert.Equal(t, live, got)
}

func TestJobExportOmitsTheDefaultBackoffLimit(t *testing.T) {
	job := exportJob(map[string]interface{}{"image": "backup:1", "backoffLimit": int64(3)})

	assert.Nil(t, job.BackoffLimit)
}
