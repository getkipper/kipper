package k8s

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
)

func readyNode(unschedulable bool) corev1.Node {
	n := corev1.Node{Spec: corev1.NodeSpec{Unschedulable: unschedulable}}
	n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
	return n
}

func TestNodeToInfoSaysWhenANodeIsCordoned(t *testing.T) {
	assert.Equal(t, "Ready", nodeToInfo(readyNode(false)).Status)
	cordoned := nodeToInfo(readyNode(true))
	assert.True(t, cordoned.Cordoned)
	assert.Equal(t, "Ready", cordoned.Status, "readiness keeps its own value for callers that compare it")
	assert.Equal(t, "Ready,SchedulingDisabled", cordoned.Summary())
}
