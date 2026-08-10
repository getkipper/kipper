package ai

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

const gib int64 = 1024 * 1024 * 1024

func readyNode(name, allocatable string, labels map[string]string, gpuCount string) *corev1.Node {
	allocatableList := corev1.ResourceList{
		corev1.ResourceMemory: resource.MustParse(allocatable),
	}
	capacity := corev1.ResourceList{
		corev1.ResourceMemory: resource.MustParse(allocatable),
	}
	if gpuCount != "" {
		gpuQty := resource.MustParse(gpuCount)
		capacity[gpuResourceName] = gpuQty
		allocatableList[gpuResourceName] = gpuQty
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Status: corev1.NodeStatus{
			Allocatable: allocatableList,
			Capacity:    capacity,
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
		},
	}
}

func notReadyNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("32Gi"),
			},
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionFalse},
			},
		},
	}
}

func podWithRequests(name, nodeName, phase, memReq string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
			Containers: []corev1.Container{
				{
					Name: "c",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceMemory: resource.MustParse(memReq),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodPhase(phase)},
	}
}

func podWithGPURequest(name, nodeName, gpuCount string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
			Containers: []corev1.Container{
				{
					Name: "c",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							gpuResourceName: resource.MustParse(gpuCount),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func TestDetectNoNodesYieldsTierNone(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck
	report, err := Detect(context.Background(), client)
	require.NoError(t, err)
	assert.Equal(t, TierNone, report.Tier())
	assert.Equal(t, int64(0), report.TotalFree)
}

func TestDetectTinyNodeBelowMinimum(t *testing.T) {
	client := fake.NewSimpleClientset(readyNode("vps", "4Gi", nil, "")) //nolint:staticcheck
	report, err := Detect(context.Background(), client)
	require.NoError(t, err)
	assert.Equal(t, TierNone, report.Tier())
	assert.False(t, report.Tier().MeetsMinimum())
}

func TestDetectSubtractsPodRequests(t *testing.T) {
	client := fake.NewSimpleClientset( //nolint:staticcheck
		readyNode("box", "16Gi", nil, ""),
		podWithRequests("traefik", "box", "Running", "9Gi"),
	)
	report, err := Detect(context.Background(), client)
	require.NoError(t, err)
	// 16 GiB allocatable - 9 GiB requested = 7 GiB free, below tier 1.
	assert.Equal(t, TierNone, report.Tier())
	assert.Equal(t, int64(7)*gib, report.TotalFree)
}

func TestDetectIgnoresTerminatedPods(t *testing.T) {
	client := fake.NewSimpleClientset( //nolint:staticcheck
		readyNode("box", "16Gi", nil, ""),
		podWithRequests("running", "box", "Running", "2Gi"),
		podWithRequests("done", "box", "Succeeded", "8Gi"),
		podWithRequests("crashed", "box", "Failed", "8Gi"),
	)
	report, err := Detect(context.Background(), client)
	require.NoError(t, err)
	// Only "running" counts: 16 - 2 = 14 GiB free, tier 1.
	assert.Equal(t, TierOne, report.Tier())
	assert.Equal(t, int64(14)*gib, report.TotalFree)
}

func TestDetectTierOneCPUOnly(t *testing.T) {
	client := fake.NewSimpleClientset(readyNode("box", "10Gi", nil, "")) //nolint:staticcheck
	report, err := Detect(context.Background(), client)
	require.NoError(t, err)
	assert.Equal(t, TierOne, report.Tier())
}

func TestDetectTierTwoCPUOnly(t *testing.T) {
	client := fake.NewSimpleClientset(readyNode("box", "32Gi", nil, "")) //nolint:staticcheck
	report, err := Detect(context.Background(), client)
	require.NoError(t, err)
	assert.Equal(t, TierTwo, report.Tier())
	assert.False(t, report.HasGPU)
}

func TestDetectTierThreeGPUSmallVRAM(t *testing.T) {
	// 8 GiB GPU (8192 MiB), 32 GiB CPU memory.
	labels := map[string]string{gpuMemoryLabel: "8192"}
	client := fake.NewSimpleClientset(readyNode("box", "32Gi", labels, "1")) //nolint:staticcheck
	report, err := Detect(context.Background(), client)
	require.NoError(t, err)
	assert.Equal(t, TierThree, report.Tier())
	assert.True(t, report.HasGPU)
	assert.Equal(t, int64(8)*gib, report.MaxGPUMemory)
}

func TestDetectTierThreeGPUUnknownVRAM(t *testing.T) {
	// GPU present but no memory label means we cannot promote to tier 4.
	client := fake.NewSimpleClientset(readyNode("box", "32Gi", nil, "1")) //nolint:staticcheck
	report, err := Detect(context.Background(), client)
	require.NoError(t, err)
	assert.Equal(t, TierThree, report.Tier())
	assert.True(t, report.HasGPU)
	assert.Equal(t, int64(0), report.MaxGPUMemory)
}

func TestDetectTierFourLargeGPU(t *testing.T) {
	// 24 GiB GPU.
	labels := map[string]string{gpuMemoryLabel: "24576"}
	client := fake.NewSimpleClientset(readyNode("box", "64Gi", labels, "1")) //nolint:staticcheck
	report, err := Detect(context.Background(), client)
	require.NoError(t, err)
	assert.Equal(t, TierFour, report.Tier())
	assert.Equal(t, int64(24)*gib, report.MaxGPUMemory)
}

func TestDetectTier4RequiresSingleGPUAtThreshold(t *testing.T) {
	// Two 12 GiB GPUs do NOT qualify as tier 4: the Ollama manifest
	// requests one GPU, and we never split a model across devices, so a
	// single 12 GiB device is the real ceiling.
	labels := map[string]string{gpuMemoryLabel: "12288"}
	client := fake.NewSimpleClientset(readyNode("box", "64Gi", labels, "2")) //nolint:staticcheck
	report, err := Detect(context.Background(), client)
	require.NoError(t, err)
	assert.Equal(t, TierThree, report.Tier())
	assert.Equal(t, int64(12)*gib, report.BestNode.PerGPUMemoryBytes)
	assert.Equal(t, int64(12)*gib, report.MaxGPUMemory, "max is per-device, not summed")
}

func TestDetectGPUWithLowFreeMemoryFallsBackToTierOne(t *testing.T) {
	// GPU present but free CPU memory below tier 2 threshold.
	labels := map[string]string{gpuMemoryLabel: "8192"}
	client := fake.NewSimpleClientset( //nolint:staticcheck
		readyNode("box", "10Gi", labels, "1"),
	)
	report, err := Detect(context.Background(), client)
	require.NoError(t, err)
	assert.Equal(t, TierOne, report.Tier())
	assert.True(t, report.HasGPU)
}

func TestDetectExcludesUnschedulableNode(t *testing.T) {
	cordoned := readyNode("ready", "32Gi", nil, "")
	cordoned.Spec.Unschedulable = true
	client := fake.NewSimpleClientset(cordoned) //nolint:staticcheck
	report, err := Detect(context.Background(), client)
	require.NoError(t, err)
	assert.Equal(t, TierNone, report.Tier(), "cordoned node must not pass preflight")
	assert.Equal(t, []string{"ready"}, report.UnreadyNodes)
}

func TestDetectExcludesTaintedNode(t *testing.T) {
	tainted := readyNode("gpu-only", "64Gi", nil, "")
	tainted.Spec.Taints = []corev1.Taint{
		{Key: "nvidia.com/gpu", Effect: corev1.TaintEffectNoSchedule},
	}
	client := fake.NewSimpleClientset(tainted) //nolint:staticcheck
	report, err := Detect(context.Background(), client)
	require.NoError(t, err)
	assert.Equal(t, TierNone, report.Tier(), "NoSchedule-tainted node must not pass preflight")
}

func TestDetectGPUAllocatableZeroFallsBackToCPUTier(t *testing.T) {
	// Capacity says 1 GPU but allocatable is 0 (device plugin not yet
	// reporting). The node should classify as CPU-only.
	node := readyNode("box", "32Gi", nil, "1")
	node.Status.Allocatable[gpuResourceName] = resource.MustParse("0")
	client := fake.NewSimpleClientset(node) //nolint:staticcheck

	report, err := Detect(context.Background(), client)
	require.NoError(t, err)
	assert.Equal(t, TierTwo, report.Tier(), "zero allocatable GPU must not promote to GPU tier")
	assert.False(t, report.HasGPU)
}

func TestDetectExcludesNotReadyNodes(t *testing.T) {
	client := fake.NewSimpleClientset( //nolint:staticcheck
		readyNode("ready", "10Gi", nil, ""),
		notReadyNode("broken"),
	)
	report, err := Detect(context.Background(), client)
	require.NoError(t, err)
	assert.Equal(t, TierOne, report.Tier())
	assert.Equal(t, []string{"broken"}, report.UnreadyNodes)
	assert.Len(t, report.Nodes, 1)
}

func TestDetectDoesNotAggregateAcrossNodes(t *testing.T) {
	// Two 9 GiB nodes do not become tier 2: no single node has the 16 GiB
	// needed to run a 7B model, and the bundle has no scheduler hints
	// telling Kubernetes to spread workload across nodes.
	client := fake.NewSimpleClientset( //nolint:staticcheck
		readyNode("a", "9Gi", nil, ""),
		readyNode("b", "9Gi", nil, ""),
	)
	report, err := Detect(context.Background(), client)
	require.NoError(t, err)
	assert.Equal(t, TierOne, report.Tier())
	assert.Equal(t, int64(18)*gib, report.TotalFree, "total stays informational")
	assert.Equal(t, int64(9)*gib, report.BestNode.FreeMemory)
}

func TestDetectExcludesAlreadyAllocatedGPU(t *testing.T) {
	// Node has 1 GPU at the chart's threshold, but another workload has
	// already requested that single GPU. We must not classify as tier 4
	// just because capacity exists; the install would render Ollama with
	// gpu.enabled=true and the pod would stay Pending.
	labels := map[string]string{gpuMemoryLabel: "24576"}
	client := fake.NewSimpleClientset( //nolint:staticcheck
		readyNode("box", "64Gi", labels, "1"),
		podWithGPURequest("foreign-trainer", "box", "1"),
	)
	report, err := Detect(context.Background(), client)
	require.NoError(t, err)
	assert.Equal(t, TierTwo, report.Tier(), "GPU is consumed; classify as CPU-only tier 2")
	assert.False(t, report.HasGPU)
	assert.Equal(t, int64(0), report.BestNode.GPUCount)
}

func TestDetectCountsRemainingGPUsAfterPartialAllocation(t *testing.T) {
	// 2 GPUs total, 1 already requested. One GPU remains free, so the
	// node still qualifies for GPU tiers but uses per-device VRAM (24
	// GiB) for tier 4 selection.
	labels := map[string]string{gpuMemoryLabel: "24576"}
	client := fake.NewSimpleClientset( //nolint:staticcheck
		readyNode("box", "64Gi", labels, "2"),
		podWithGPURequest("foreign-trainer", "box", "1"),
	)
	report, err := Detect(context.Background(), client)
	require.NoError(t, err)
	assert.Equal(t, TierFour, report.Tier())
	assert.Equal(t, int64(1), report.BestNode.GPUCount)
}

func TestDetectIgnoresGPUOnDifferentNodeFromBestMemory(t *testing.T) {
	// CPU-rich node has the most free memory, GPU lives on a smaller node.
	// We tier on the best node, so this is tier 2 (CPU only), not tier 3.
	gpuNode := readyNode("gpu", "10Gi", map[string]string{gpuMemoryLabel: "24576"}, "1")
	cpuNode := readyNode("cpu", "32Gi", nil, "")
	client := fake.NewSimpleClientset(gpuNode, cpuNode) //nolint:staticcheck

	report, err := Detect(context.Background(), client)
	require.NoError(t, err)
	assert.Equal(t, TierTwo, report.Tier())
	assert.Equal(t, "cpu", report.BestNode.Name)
	assert.True(t, report.HasGPU, "informational HasGPU still flips on")
}

func TestDetectIgnoresUnparseableGPUMemoryLabel(t *testing.T) {
	labels := map[string]string{gpuMemoryLabel: "not-a-number"}
	client := fake.NewSimpleClientset(readyNode("box", "32Gi", labels, "1")) //nolint:staticcheck
	report, err := Detect(context.Background(), client)
	require.NoError(t, err)
	assert.Equal(t, TierThree, report.Tier())
	assert.Equal(t, int64(0), report.MaxGPUMemory)
}

func TestTierStringAndDefaultModel(t *testing.T) {
	tests := []struct {
		tier  Tier
		name  string
		model string
	}{
		{TierNone, "none", ""},
		{TierOne, "tier 1 (CPU, 8 GiB)", "qwen2.5:3b-instruct-q4_K_M"},
		{TierTwo, "tier 2 (CPU, 16 GiB)", "qwen2.5:7b-instruct-q4_K_M"},
		{TierThree, "tier 3 (GPU)", "qwen2.5:7b-instruct-q4_K_M"},
		{TierFour, "tier 4 (GPU, 16+ GiB VRAM)", "qwen2.5:14b-instruct-q4_K_M"},
		{Tier(99), "unknown tier (99)", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.name, tt.tier.String())
			assert.Equal(t, tt.model, tt.tier.DefaultModel())
		})
	}
}

func TestTierMeetsMinimum(t *testing.T) {
	assert.False(t, TierNone.MeetsMinimum())
	assert.True(t, TierOne.MeetsMinimum())
	assert.True(t, TierFour.MeetsMinimum())
}

func TestDetectClampsNegativeFreeMemory(t *testing.T) {
	// Pod requests overshoot allocatable: free should clamp to zero, not go
	// negative and silently classify as a high tier.
	client := fake.NewSimpleClientset( //nolint:staticcheck
		readyNode("box", "8Gi", nil, ""),
		podWithRequests("greedy", "box", "Running", "16Gi"),
	)
	report, err := Detect(context.Background(), client)
	require.NoError(t, err)
	assert.Equal(t, int64(0), report.TotalFree)
	assert.Equal(t, TierNone, report.Tier())
}
