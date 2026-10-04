package resourcebounds

import (
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

// Tracked names the metrics an app's autoscaler reads. Keeping their requests
// stable lets utilization changes drive replica counts instead of resource sizing.
type Tracked struct {
	CPU, Memory bool
}

// TrackedMetrics reports the metrics an app's autoscaler tracks. A usable
// policy tracks its positive targets. An enabled policy that is not usable
// leaves any autoscaler it had running unchanged, so the metrics come from
// hpa, which may be nil. A policy that is off tracks nothing.
func TrackedMetrics(app *kipperv1.App, hpa *autoscalingv2.HorizontalPodAutoscaler) Tracked {
	as := app.Spec.Autoscale
	if as == nil || !as.Enabled {
		return Tracked{}
	}
	if as.Policy().Usable() {
		return Tracked{CPU: ptr.Deref(as.CPUTarget, 0) > 0, Memory: ptr.Deref(as.MemoryTarget, 0) > 0}
	}
	return AutoscalerMetrics(hpa)
}

// AutoscalerMetrics reports the resource metrics an autoscaler reads. A nil
// autoscaler reads none.
func AutoscalerMetrics(hpa *autoscalingv2.HorizontalPodAutoscaler) Tracked {
	var t Tracked
	if hpa == nil {
		return t
	}
	for _, m := range hpa.Spec.Metrics {
		if m.Type != autoscalingv2.ResourceMetricSourceType || m.Resource == nil {
			continue
		}
		switch m.Resource.Name {
		case corev1.ResourceCPU:
			t.CPU = true
		case corev1.ResourceMemory:
			t.Memory = true
		}
	}
	return t
}

// Recommendation returns the part of a stored recommendation that automatic
// sizing may still apply: no CPU values when CPU is tracked, and no memory
// values when memory is tracked, unless they are the raise after an OOM kill.
// A record without a memory cause counts as routine.
func (t Tracked) Recommendation(status kipperv1.ResourceTuningStatus) kipperv1.TunedResources {
	rec := status.Recommendation
	if t.CPU {
		rec.CPURequest, rec.CPULimit = "", ""
	}
	if t.Memory && status.MemoryCause != kipperv1.MemoryCauseOOMKill {
		rec.MemoryRequest, rec.MemoryLimit = "", ""
	}
	return rec
}
