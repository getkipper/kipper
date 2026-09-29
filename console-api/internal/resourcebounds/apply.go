package resourcebounds

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

// Spec holds the workload's four resource quantities and their sources.
type Spec struct {
	CPURequest, CPULimit, MemoryRequest, MemoryLimit Quantity
}

// AppSpec reads an App's quantities and classifies each from the App's
// managedFields.
func AppSpec(app *kipperv1.App) (Spec, error) {
	r := app.Spec.Resources
	classify := func(value, field string) (Quantity, error) {
		return parseQuantity(value, field, ClassifyAppQuantity(value, app.ManagedFields, field))
	}
	return buildSpec(classify, r.CPURequest, r.CPULimit, r.MemoryRequest, r.MemoryLimit)
}

// OwnedSpec reads a Service's or Function's quantities, which are always the
// user's when set.
func OwnedSpec(cpuRequest, cpuLimit, memoryRequest, memoryLimit string) (Spec, error) {
	classify := func(value, field string) (Quantity, error) {
		return parseQuantity(value, field, ClassifyOwnedQuantity(value))
	}
	return buildSpec(classify, cpuRequest, cpuLimit, memoryRequest, memoryLimit)
}

func buildSpec(classify func(value, field string) (Quantity, error), cpuReq, cpuLim, memReq, memLim string) (Spec, error) {
	var s Spec
	var err error
	if s.CPURequest, err = classify(cpuReq, "cpuRequest"); err != nil {
		return Spec{}, err
	}
	if s.CPULimit, err = classify(cpuLim, "cpuLimit"); err != nil {
		return Spec{}, err
	}
	if s.MemoryRequest, err = classify(memReq, "memoryRequest"); err != nil {
		return Spec{}, err
	}
	if s.MemoryLimit, err = classify(memLim, "memoryLimit"); err != nil {
		return Spec{}, err
	}
	return s, nil
}

func parseQuantity(value, field string, source Source) (Quantity, error) {
	if source == Unset {
		return Quantity{}, nil
	}
	v, err := resource.ParseQuantity(value)
	if err != nil {
		return Quantity{}, fmt.Errorf("%s %q: %w", field, value, err)
	}
	return Quantity{Value: v, Source: source}, nil
}

// Apply resolves CPU and memory in desired, which starts with the spec and
// profile defaults. live is nil before the first rollout; rec is nil when no
// recommendation applies. Without a recommendation, live values are preserved
// within the user's bounds.
func Apply(desired *corev1.ResourceRequirements, spec Spec, rec *kipperv1.TunedResources, live *corev1.ResourceRequirements) {
	if desired.Requests == nil {
		desired.Requests = corev1.ResourceList{}
	}
	if desired.Limits == nil {
		desired.Limits = corev1.ResourceList{}
	}
	var cpuRec, memRec *Pair
	if rec != nil {
		cpuRec = recommendedPair(rec.CPURequest, rec.CPULimit)
		memRec = recommendedPair(rec.MemoryRequest, rec.MemoryLimit)
	}
	applyDimension(desired, corev1.ResourceCPU, spec.CPURequest, spec.CPULimit, cpuRec, live)
	applyDimension(desired, corev1.ResourceMemory, spec.MemoryRequest, spec.MemoryLimit, memRec, live)
}

func applyDimension(desired *corev1.ResourceRequirements, name corev1.ResourceName, request, limit Quantity, rec *Pair, live *corev1.ResourceRequirements) {
	fallback, ok := PairOf(live, name)
	if !ok {
		fallback, ok = PairOf(desired, name)
	}
	if !ok && rec == nil && request.Source == Unset && limit.Source == Unset {
		return
	}
	p, _ := Resolve(request, limit, rec, fallback, AutoRange{})
	desired.Requests[name] = p.Request
	desired.Limits[name] = p.Limit
}

// PairOf reads one resource's request and limit, copying a lone side to the
// other. It reports false when neither is set.
func PairOf(r *corev1.ResourceRequirements, name corev1.ResourceName) (Pair, bool) {
	if r == nil {
		return Pair{}, false
	}
	req, hasReq := r.Requests[name]
	lim, hasLim := r.Limits[name]
	switch {
	case hasReq && hasLim:
		return Pair{Request: req, Limit: lim}, true
	case hasReq:
		return Pair{Request: req, Limit: req}, true
	case hasLim:
		return Pair{Request: lim, Limit: lim}, true
	default:
		return Pair{}, false
	}
}

// recommendedPair parses one resource's recommendation, nil when it is absent
// or unreadable.
func recommendedPair(request, limit string) *Pair {
	if request == "" {
		return nil
	}
	req, err := resource.ParseQuantity(request)
	if err != nil {
		return nil
	}
	lim := req
	if limit != "" {
		if lim, err = resource.ParseQuantity(limit); err != nil {
			return nil
		}
	}
	return &Pair{Request: req, Limit: lim}
}
