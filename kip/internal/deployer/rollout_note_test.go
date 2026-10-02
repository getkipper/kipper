package deployer

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func appWithRollout(conditions ...interface{}) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"name": "api"},
		"spec":     map[string]interface{}{"image": "example.com/api:2"},
		"status": map[string]interface{}{
			"phase": "Running", "image": "example.com/api:2", "replicas": int64(2), "readyReplicas": int64(2),
			"conditions": conditions,
		},
	}}
}

// Show rollout progress separately from the current pods' healthy status.
func TestAppStatusCarriesAWaitingRollout(t *testing.T) {
	stalled := appStatusFromCR(appWithRollout(map[string]interface{}{
		"type": "RolloutComplete", "status": "False", "reason": "Unschedulable",
		"message": "A new pod cannot be placed: 0/1 nodes are available: 1 Insufficient cpu. The current pods keep serving.",
	}))
	assert.Contains(t, stalled.RolloutWaiting, "Insufficient cpu")

	done := appStatusFromCR(appWithRollout(map[string]interface{}{
		"type": "RolloutComplete", "status": "True", "reason": "Complete", "message": "every pod runs the current version",
	}))
	assert.Empty(t, done.RolloutWaiting)

	assert.Empty(t, appStatusFromCR(appWithRollout()).RolloutWaiting, "a controller too old to write the condition says nothing")
}
