package resourcebounds

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func ownedSpec(t *testing.T, cpuRequest, cpuLimit, memoryRequest, memoryLimit string) Spec {
	t.Helper()
	s, err := OwnedSpec(cpuRequest, cpuLimit, memoryRequest, memoryLimit)
	require.NoError(t, err)
	return s
}

func running(cpuRequest, memoryRequest string) *corev1.ResourceRequirements {
	return &corev1.ResourceRequirements{Requests: corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse(cpuRequest),
		corev1.ResourceMemory: resource.MustParse(memoryRequest),
	}}
}

func TestWithChangedRequestsStartsAtAChangedRequest(t *testing.T) {
	spec := ownedSpec(t, "100m", "2", "", "")
	rec := kipperv1.TunedResources{CPURequest: "2", CPULimit: "2", MemoryRequest: "300Mi", MemoryLimit: "600Mi"}

	got := WithChangedRequests(rec, spec, &kipperv1.TunedResources{CPURequest: "1"})

	assert.Equal(t, "100m", got.CPURequest)
	assert.Equal(t, "2", got.CPULimit)
	assert.Equal(t, "300Mi", got.MemoryRequest, "automatic memory keeps the auto-sizer's value")
	assert.Equal(t, "600Mi", got.MemoryLimit)
}

func TestWithChangedRequestsStartsMemoryAtAChangedRequest(t *testing.T) {
	spec := ownedSpec(t, "", "", "512Mi", "2Gi")
	rec := kipperv1.TunedResources{MemoryRequest: "1Gi", MemoryLimit: "2Gi"}

	got := WithChangedRequests(rec, spec, &kipperv1.TunedResources{MemoryRequest: "1Gi"})

	assert.Equal(t, "512Mi", got.MemoryRequest)
	assert.Equal(t, "2Gi", got.MemoryLimit)
}

func TestWithChangedRequestsKeepsATunedValueUnderTheSameRequest(t *testing.T) {
	spec := ownedSpec(t, "100m", "1", "", "")
	rec := kipperv1.TunedResources{CPURequest: "150m", CPULimit: "1"}

	got := WithChangedRequests(rec, spec, &kipperv1.TunedResources{CPURequest: "0.1"})

	assert.Equal(t, "150m", got.CPURequest, "0.1 and 100m are the same request")
}

func TestWithChangedRequestsStartsAtTheRequestAfterAFixedSize(t *testing.T) {
	spec := ownedSpec(t, "100m", "2", "", "")

	got := WithChangedRequests(kipperv1.TunedResources{}, spec, &kipperv1.TunedResources{CPURequest: "2"})

	assert.Equal(t, "100m", got.CPURequest)
	assert.Equal(t, "2", got.CPULimit)
}

func TestWithChangedRequestsLeavesAnOlderRecordAlone(t *testing.T) {
	spec := ownedSpec(t, "100m", "2", "512Mi", "2Gi")
	rec := kipperv1.TunedResources{CPURequest: "2", CPULimit: "2", MemoryRequest: "2Gi", MemoryLimit: "2Gi"}

	assert.Equal(t, rec, WithChangedRequests(rec, spec, nil), "a record without requests cannot tell an edit from tuning")
}

func TestWithChangedRequestsLeavesFixedResourcesAlone(t *testing.T) {
	spec := ownedSpec(t, "2", "2", "1536Mi", "1536Mi")

	got := WithChangedRequests(kipperv1.TunedResources{}, spec, &kipperv1.TunedResources{CPURequest: "100m"})

	assert.Equal(t, kipperv1.TunedResources{}, got)
}

func TestPendingRequestsLastUntilTheWorkloadRunsTheRequest(t *testing.T) {
	spec := ownedSpec(t, "100m", "2", "512Mi", "2Gi")
	under := &kipperv1.TunedResources{CPURequest: "2", MemoryRequest: "1Gi"}

	cpu, memory := PendingRequests(spec, under, running("2", "1Gi"))
	assert.True(t, cpu)
	assert.True(t, memory)

	cpu, memory = PendingRequests(spec, under, running("100m", "1Gi"))
	assert.False(t, cpu, "the workload runs the new CPU request")
	assert.True(t, memory)

	cpu, memory = PendingRequests(spec, under, running("2", "512Mi"))
	assert.True(t, cpu)
	assert.False(t, memory, "the workload runs the new memory request")

	cpu, memory = PendingRequests(spec, under, nil)
	assert.True(t, cpu, "a workload not running yet carries nothing")
	assert.True(t, memory)
}

func TestNextRecommendedUnderKeepsAPendingRequest(t *testing.T) {
	spec := ownedSpec(t, "100m", "2", "512Mi", "2Gi")
	under := &kipperv1.TunedResources{CPURequest: "2", MemoryRequest: "1Gi"}

	got := NextRecommendedUnder(spec, under, running("100m", "1Gi"))

	require.NotNil(t, got)
	assert.Equal(t, kipperv1.TunedResources{CPURequest: "100m", MemoryRequest: "1Gi"}, *got)
}

func TestNextRecommendedUnderStartsAnOlderRecordFromTheSpec(t *testing.T) {
	got := NextRecommendedUnder(ownedSpec(t, "", "", "512Mi", "1Gi"), nil, running("2", "1Gi"))

	require.NotNil(t, got)
	assert.Equal(t, kipperv1.TunedResources{MemoryRequest: "512Mi"}, *got, "an automatic CPU request stays empty")
}

func TestNextRecommendedUnderRecordsAUserCPURequest(t *testing.T) {
	got := NextRecommendedUnder(ownedSpec(t, "250m", "1", "", ""), nil, nil)

	require.NotNil(t, got)
	assert.Equal(t, kipperv1.TunedResources{CPURequest: "250m"}, *got)
}
