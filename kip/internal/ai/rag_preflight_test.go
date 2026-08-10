package ai

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// availableOllamaDeployment builds an Ollama Deployment that
// isDeploymentAvailable accepts: generations matched, one replica
// rolled out and ready, Available=True condition. nodeSelector pins to
// the given hostname so DetectRAG can identify the target node.
func availableOllamaDeployment(nodeName string) *appsv1.Deployment {
	one := int32(1)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:       ollamaDeploymentName,
			Namespace:  Namespace,
			Generation: 1,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &one,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					NodeSelector: map[string]string{hostnameLabel: nodeName},
				},
			},
		},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 1,
			Replicas:           1,
			UpdatedReplicas:    1,
			AvailableReplicas:  1,
			ReadyReplicas:      1,
			Conditions: []appsv1.DeploymentCondition{
				{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue},
			},
		},
	}
}

// rollingOllamaDeployment is an Ollama Deployment that exists but has
// not finished rolling out. isDeploymentAvailable rejects it.
func rollingOllamaDeployment(nodeName string) *appsv1.Deployment {
	dep := availableOllamaDeployment(nodeName)
	dep.Status.AvailableReplicas = 0
	dep.Status.ReadyReplicas = 0
	dep.Status.Conditions = nil
	return dep
}

func TestDetectRAGPhase1NotInstalled(t *testing.T) {
	// No Ollama Deployment in the cluster: Phase 1 has not been
	// installed. DetectRAG must report Phase1Installed=false with a
	// reason that points at 'kip ai install'.
	client := fake.NewSimpleClientset(readyNode("box", "16Gi", nil, "")) //nolint:staticcheck

	report, err := DetectRAG(context.Background(), client)
	require.NoError(t, err)
	assert.False(t, report.Phase1Installed)
	assert.False(t, report.Eligible())
	assert.Contains(t, report.Phase1Reason, "not found")
	assert.Contains(t, report.Phase1Reason, "kip ai install")
}

func TestDetectRAGPhase1NotYetAvailable(t *testing.T) {
	// Ollama Deployment exists but isDeploymentAvailable rejects it
	// (mid-rollout, no replicas ready). Phase1Installed must stay
	// false; the user needs to wait, not run a different install.
	client := fake.NewSimpleClientset( //nolint:staticcheck
		readyNode("box", "16Gi", nil, ""),
		rollingOllamaDeployment("box"),
	)

	report, err := DetectRAG(context.Background(), client)
	require.NoError(t, err)
	assert.False(t, report.Phase1Installed)
	assert.False(t, report.Eligible())
	assert.Contains(t, report.Phase1Reason, "not Available")
	assert.Contains(t, report.Phase1Reason, "wait")
}

func TestDetectRAGEligibleWithHeadroom(t *testing.T) {
	// Tier-1 box with 16 GiB allocatable, Ollama using 6 GiB. That
	// leaves 10 GiB free, well above the 4 GiB ragRequiredHeadroom.
	client := fake.NewSimpleClientset( //nolint:staticcheck
		readyNode("box", "16Gi", nil, ""),
		availableOllamaDeployment("box"),
		podWithRequests("ollama-abc", "box", "Running", "6Gi"),
	)

	report, err := DetectRAG(context.Background(), client)
	require.NoError(t, err)
	assert.True(t, report.Phase1Installed)
	assert.True(t, report.HeadroomSufficient)
	assert.True(t, report.Eligible())
	assert.Equal(t, "box", report.TargetNode)
	assert.Equal(t, int64(10)*gib, report.TargetFreeMemory)
	assert.Equal(t, ragRequiredHeadroom, report.RequiredHeadroomBytes)
	assert.Empty(t, report.Phase1Reason)
}

func TestDetectRAGInsufficientHeadroom(t *testing.T) {
	// Phase 1 is fully installed but the node only has 3 GiB free
	// after Ollama + a busy app. Below the 4 GiB threshold; not
	// eligible, but Phase1Installed stays true so the CLI can show
	// "Phase 1 OK, but you need to free 1 GiB".
	client := fake.NewSimpleClientset( //nolint:staticcheck
		readyNode("box", "16Gi", nil, ""),
		availableOllamaDeployment("box"),
		podWithRequests("ollama-abc", "box", "Running", "6Gi"),
		podWithRequests("busy-app", "box", "Running", "7Gi"),
	)

	report, err := DetectRAG(context.Background(), client)
	require.NoError(t, err)
	assert.True(t, report.Phase1Installed)
	assert.False(t, report.HeadroomSufficient)
	assert.False(t, report.Eligible())
	assert.Equal(t, int64(3)*gib, report.TargetFreeMemory)
}

func TestDetectRAGAtExactThreshold(t *testing.T) {
	// 4 GiB free is exactly at the threshold; the comparison is >=
	// so this must be classified as eligible. A < check would
	// silently refuse a borderline-but-fine cluster.
	client := fake.NewSimpleClientset( //nolint:staticcheck
		readyNode("box", "16Gi", nil, ""),
		availableOllamaDeployment("box"),
		podWithRequests("ollama-abc", "box", "Running", "6Gi"),
		podWithRequests("other", "box", "Running", "6Gi"),
	)

	report, err := DetectRAG(context.Background(), client)
	require.NoError(t, err)
	assert.True(t, report.Eligible(), "boundary case at exactly 4 GiB must be eligible")
	assert.Equal(t, int64(4)*gib, report.TargetFreeMemory)
}

func TestDetectRAGOllamaWithoutNodeSelector(t *testing.T) {
	// Ollama Deployment exists, is Available, but is missing the
	// kubernetes.io/hostname nodeSelector that Phase 1's installer
	// sets. That is an install bug (someone hand-edited the chart),
	// not a capacity problem; surface it as an error so the user
	// knows to fix Phase 1.
	dep := availableOllamaDeployment("box")
	dep.Spec.Template.Spec.NodeSelector = nil
	client := fake.NewSimpleClientset(readyNode("box", "16Gi", nil, ""), dep) //nolint:staticcheck

	_, err := DetectRAG(context.Background(), client)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nodeSelector")
}

func TestDetectRAGOllamaPinnedToMissingNode(t *testing.T) {
	// Ollama is pinned to a node that exists in the cluster but is
	// cordoned, so Detect skips it. Returning HeadroomSufficient=true
	// or false would be misleading; surface it as an error so the
	// caller can show "Phase 1 is unhealthy on a cordoned node".
	cordoned := readyNode("cordoned", "16Gi", nil, "")
	cordoned.Spec.Unschedulable = true
	client := fake.NewSimpleClientset( //nolint:staticcheck
		cordoned,
		availableOllamaDeployment("cordoned"),
		podWithRequests("ollama-abc", "cordoned", "Running", "6Gi"),
	)

	_, err := DetectRAG(context.Background(), client)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cordoned")
	assert.True(t, strings.Contains(err.Error(), "not Ready") || strings.Contains(err.Error(), "schedulable"))
}

func TestRAGReportEligibleRequiresBothChecks(t *testing.T) {
	tests := []struct {
		name             string
		phase1Installed  bool
		headroomOK       bool
		expectedEligible bool
	}{
		{"both ok", true, true, true},
		{"no phase 1", false, true, false},
		{"no headroom", true, false, false},
		{"neither", false, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := RAGReport{Phase1Installed: tt.phase1Installed, HeadroomSufficient: tt.headroomOK}
			assert.Equal(t, tt.expectedEligible, r.Eligible())
		})
	}
}
