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

func TestHealthyCopies(t *testing.T) {
	healthy := CopyMarkers{HealthyAt: "2026-10-24T01:00:00Z", LastHealthyAt: "2026-10-24T01:00:00Z"}
	salvaged := CopyMarkers{HealthyAt: "2026-10-24T01:00:00Z", LastFailedAt: "2026-10-24T02:00:00Z", LastHealthyAt: "2026-10-24T02:00:10Z"}
	for name, tc := range map[string]struct {
		state    string
		seeded   bool
		replicas []CopyMarkers
		want     []bool
	}{
		"healthy and never failed":                    {"attached", false, []CopyMarkers{healthy}, []bool{true}},
		"healthy on a detached volume":                {"detached", false, []CopyMarkers{healthy}, []bool{true}},
		"rebuilt after its last failure":              {"attached", false, []CopyMarkers{{HealthyAt: "2026-10-24T03:00:00Z", LastFailedAt: "2026-10-24T02:00:00Z"}}, []bool{true}},
		"salvaged and serving again":                  {"attached", false, []CopyMarkers{salvaged}, []bool{true}},
		"salvaged, served, then detached":             {"detached", false, []CopyMarkers{salvaged}, []bool{true}},
		"salvaged and not serving since":              {"attached", false, []CopyMarkers{{HealthyAt: "2026-10-24T01:00:00Z", LastFailedAt: "2026-10-24T02:00:00Z", LastHealthyAt: "2026-10-24T01:30:00Z"}}, []bool{false}},
		"served in the same second it failed":         {"attached", false, []CopyMarkers{{HealthyAt: "2026-10-24T01:00:00Z", LastFailedAt: "2026-10-24T02:00:00Z", LastHealthyAt: "2026-10-24T02:00:00Z"}}, []bool{false}},
		"failed now":                                  {"attached", false, []CopyMarkers{{HealthyAt: "2026-10-24T01:00:00Z", FailedAt: "2026-10-24T02:00:00Z", LastFailedAt: "2026-10-24T02:00:00Z"}}, []bool{false}},
		"being built next to a healthy copy":          {"attached", false, []CopyMarkers{healthy, {}}, []bool{true, false}},
		"never attached":                              {"detached", false, []CopyMarkers{{}, {}}, []bool{true, true}},
		"never attached, one copy failed":             {"detached", false, []CopyMarkers{{}, {FailedAt: "2026-10-24T02:00:00Z"}}, []bool{false, false}},
		"attaching for the first time":                {"attaching", false, []CopyMarkers{{}}, []bool{false}},
		"reused after a failure on a detached volume": {"detached", false, []CopyMarkers{{LastFailedAt: "2026-10-24T02:00:00Z", LastHealthyAt: "2026-10-24T01:00:00Z"}}, []bool{false}},
		"never attached, waiting for its data":        {"detached", true, []CopyMarkers{{}}, []bool{false}},
		"unparsable healthyAt":                        {"attached", false, []CopyMarkers{{HealthyAt: "yesterday"}}, []bool{false}},
		"unparsable lastHealthyAt":                    {"attached", false, []CopyMarkers{{HealthyAt: "2026-10-24T01:00:00Z", LastFailedAt: "2026-10-24T02:00:00Z", LastHealthyAt: "later"}}, []bool{false}},
	} {
		assert.Equal(t, tc.want, HealthyCopies(CopyVolume{State: tc.state, Seeded: tc.seeded}, tc.replicas), name)
	}
}

func TestCopyVolumeOf(t *testing.T) {
	volume := func(spec, status map[string]interface{}) map[string]interface{} {
		return map[string]interface{}{"spec": spec, "status": status}
	}
	assert.Equal(t, CopyVolume{State: "detached"}, CopyVolumeOf(volume(map[string]interface{}{"fromBackup": "", "dataSource": ""}, map[string]interface{}{"state": "detached"})))
	for name, v := range map[string]map[string]interface{}{
		"from a backup":       volume(map[string]interface{}{"fromBackup": "s3://backups@example/?backup=b-1&volume=v-1"}, nil),
		"from a data source":  volume(map[string]interface{}{"dataSource": "vol://pvc-source"}, nil),
		"on a backing image":  volume(map[string]interface{}{"backingImage": "base-image"}, nil),
		"restore required":    volume(nil, map[string]interface{}{"restoreRequired": true}),
		"cloned from another": volume(nil, map[string]interface{}{"cloneStatus": map[string]interface{}{"sourceVolume": "pvc-source"}}),
	} {
		assert.True(t, CopyVolumeOf(v).Seeded, name)
	}
}
