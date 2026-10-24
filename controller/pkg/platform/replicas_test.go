package platform

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
)

func TestReplicaCurrent(t *testing.T) {
	for name, tc := range map[string]struct {
		healthyAt, failedAt, lastFailedAt string
		current                           bool
	}{
		"healthy and never failed":            {"2026-10-24T01:00:00Z", "", "", true},
		"rebuilt after its last failure":      {"2026-10-24T03:00:00Z", "", "2026-10-24T02:00:00Z", true},
		"failed":                              {"2026-10-24T01:00:00Z", "2026-10-24T02:00:00Z", "2026-10-24T02:00:00Z", false},
		"salvaged without a rebuild":          {"2026-10-24T01:00:00Z", "", "2026-10-24T02:00:00Z", false},
		"failed at the moment it was healthy": {"2026-10-24T01:00:00Z", "", "2026-10-24T01:00:00Z", false},
		"never healthy":                       {"", "", "", false},
		"unparsable healthyAt":                {"yesterday", "", "", false},
		"unparsable lastFailedAt":             {"2026-10-24T01:00:00Z", "", "yesterday", false},
	} {
		assert.Equal(t, tc.current, ReplicaCurrent(tc.healthyAt, tc.failedAt, tc.lastFailedAt), name)
	}
}

func readyNode(status corev1.ConditionStatus, taints ...corev1.Taint) *corev1.Node {
	return &corev1.Node{
		Spec:   corev1.NodeSpec{Taints: taints},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status}}},
	}
}

func TestNodeAvailable(t *testing.T) {
	assert.True(t, NodeAvailable(readyNode(corev1.ConditionTrue)))
	assert.False(t, NodeAvailable(readyNode(corev1.ConditionFalse)))
	assert.False(t, NodeAvailable(readyNode(corev1.ConditionUnknown)))
	assert.False(t, NodeAvailable(&corev1.Node{}), "a node without a Ready condition is not available")
	assert.False(t, NodeAvailable(nil))
	fenced := corev1.Taint{Key: OutOfServiceTaint, Value: "nodeshutdown", Effect: corev1.TaintEffectNoExecute}
	assert.False(t, NodeAvailable(readyNode(corev1.ConditionTrue, fenced)), "a fenced host can report Ready after booting")
	other := corev1.Taint{Key: "example.com/maintenance", Effect: corev1.TaintEffectNoSchedule}
	assert.True(t, NodeAvailable(readyNode(corev1.ConditionTrue, other)))
}
