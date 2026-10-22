package resourcebounds

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

// WithChangedRequests returns a copy of rec with the user's request and limit
// for each bounded resource whose request differs from under, so the edit
// applies before tuning resumes. Requests are compared as Kubernetes quantities.
// An empty or invalid recorded request counts as changed. A nil under preserves
// rec for compatibility with records created before requests were recorded.
func WithChangedRequests(rec kipperv1.TunedResources, spec Spec, under *kipperv1.TunedResources) kipperv1.TunedResources {
	cpu, memory := changedRequests(spec, under)
	if cpu {
		rec.CPURequest, rec.CPULimit = spec.CPURequest.Value.String(), spec.CPULimit.Value.String()
	}
	if memory {
		rec.MemoryRequest, rec.MemoryLimit = spec.MemoryRequest.Value.String(), spec.MemoryLimit.Value.String()
	}
	return rec
}

// PendingRequests reports whether each bounded request differs from under and
// is still awaiting adoption in live, the workload's current container resources.
// The auto-sizer holds pending resources at the user's request. A nil under
// returns false for both resources. A nil live or a missing live request keeps
// a changed request pending.
func PendingRequests(spec Spec, under *kipperv1.TunedResources, live *corev1.ResourceRequirements) (cpu, memory bool) {
	cpu, memory = changedRequests(spec, under)
	cpu = cpu && !runsRequest(live, corev1.ResourceCPU, spec.CPURequest.Value)
	memory = memory && !runsRequest(live, corev1.ResourceMemory, spec.MemoryRequest.Value)
	return cpu, memory
}

// NextRecommendedUnder returns a new snapshot of the user's requests to store
// with the next recommendation. A changed bounded request keeps its previous
// value until live adopts it, so the edit remains detectable. A nil live keeps
// changed requests pending. A nil under starts the snapshot from spec.
// Requests from other sources and all limits are empty.
func NextRecommendedUnder(spec Spec, under *kipperv1.TunedResources, live *corev1.ResourceRequirements) *kipperv1.TunedResources {
	next := kipperv1.TunedResources{}
	if spec.CPURequest.Source == User {
		next.CPURequest = spec.CPURequest.Value.String()
	}
	if spec.MemoryRequest.Source == User {
		next.MemoryRequest = spec.MemoryRequest.Value.String()
	}
	cpu, memory := PendingRequests(spec, under, live)
	if cpu {
		next.CPURequest = under.CPURequest
	}
	if memory {
		next.MemoryRequest = under.MemoryRequest
	}
	return &next
}

func changedRequests(spec Spec, under *kipperv1.TunedResources) (cpu, memory bool) {
	if under == nil {
		return false, false
	}
	cpu = ModeOf(spec.CPURequest, spec.CPULimit) == ModeBounded && !sameQuantity(under.CPURequest, spec.CPURequest.Value)
	memory = ModeOf(spec.MemoryRequest, spec.MemoryLimit) == ModeBounded && !sameQuantity(under.MemoryRequest, spec.MemoryRequest.Value)
	return cpu, memory
}

func runsRequest(live *corev1.ResourceRequirements, name corev1.ResourceName, request resource.Quantity) bool {
	if live == nil {
		return false
	}
	have, ok := live.Requests[name]
	return ok && have.Cmp(request) == 0
}

// sameQuantity reports whether s is a valid Kubernetes quantity equal to q.
// Empty or invalid quantities return false.
func sameQuantity(s string, q resource.Quantity) bool {
	v, err := resource.ParseQuantity(s)
	return err == nil && v.Cmp(q) == 0
}
