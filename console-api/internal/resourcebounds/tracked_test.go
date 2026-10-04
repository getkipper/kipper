package resourcebounds

import (
	"testing"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func autoscaledApp(as *kipperv1.AppAutoscale) *kipperv1.App {
	return &kipperv1.App{Spec: kipperv1.AppSpec{Autoscale: as}}
}

func hpaTracking(names ...corev1.ResourceName) *autoscalingv2.HorizontalPodAutoscaler {
	hpa := &autoscalingv2.HorizontalPodAutoscaler{}
	for _, n := range names {
		hpa.Spec.Metrics = append(hpa.Spec.Metrics, autoscalingv2.MetricSpec{
			Type:     autoscalingv2.ResourceMetricSourceType,
			Resource: &autoscalingv2.ResourceMetricSource{Name: n},
		})
	}
	return hpa
}

func TestTrackedMetrics(t *testing.T) {
	cpuAndMemory := hpaTracking(corev1.ResourceCPU, corev1.ResourceMemory)
	cases := []struct {
		name string
		app  *kipperv1.App
		hpa  *autoscalingv2.HorizontalPodAutoscaler
		want Tracked
	}{
		{"no autoscaling block", autoscaledApp(nil), cpuAndMemory, Tracked{}},
		{"policy off", autoscaledApp(&kipperv1.AppAutoscale{MaxReplicas: ptr.To[int32](5), CPUTarget: ptr.To[int32](70)}), cpuAndMemory, Tracked{}},
		{"usable, CPU only", autoscaledApp(&kipperv1.AppAutoscale{Enabled: true, MinReplicas: ptr.To[int32](1), MaxReplicas: ptr.To[int32](5), CPUTarget: ptr.To[int32](70)}), nil, Tracked{CPU: true}},
		{"usable, memory only", autoscaledApp(&kipperv1.AppAutoscale{Enabled: true, MinReplicas: ptr.To[int32](1), MaxReplicas: ptr.To[int32](5), MemoryTarget: ptr.To[int32](80)}), nil, Tracked{Memory: true}},
		{"usable, both", autoscaledApp(&kipperv1.AppAutoscale{Enabled: true, MinReplicas: ptr.To[int32](1), MaxReplicas: ptr.To[int32](5), CPUTarget: ptr.To[int32](70), MemoryTarget: ptr.To[int32](80)}), nil, Tracked{CPU: true, Memory: true}},
		{"usable policy wins over a stale autoscaler", autoscaledApp(&kipperv1.AppAutoscale{Enabled: true, MinReplicas: ptr.To[int32](1), MaxReplicas: ptr.To[int32](5), CPUTarget: ptr.To[int32](70)}), hpaTracking(corev1.ResourceMemory), Tracked{CPU: true}},
		{"unusable, autoscaler left running", autoscaledApp(&kipperv1.AppAutoscale{Enabled: true, MinReplicas: ptr.To[int32](6), MaxReplicas: ptr.To[int32](5), CPUTarget: ptr.To[int32](70)}), hpaTracking(corev1.ResourceMemory), Tracked{Memory: true}},
		{"zero minimum, autoscaler left running", autoscaledApp(&kipperv1.AppAutoscale{Enabled: true, MinReplicas: ptr.To[int32](0), MaxReplicas: ptr.To[int32](5), CPUTarget: ptr.To[int32](70)}), hpaTracking(corev1.ResourceMemory), Tracked{Memory: true}},
		{"usable, zero memory target beside a CPU target", autoscaledApp(&kipperv1.AppAutoscale{Enabled: true, MinReplicas: ptr.To[int32](1), MaxReplicas: ptr.To[int32](5), CPUTarget: ptr.To[int32](70), MemoryTarget: ptr.To[int32](0)}), hpaTracking(corev1.ResourceMemory), Tracked{CPU: true}},
		{"unusable, no autoscaler", autoscaledApp(&kipperv1.AppAutoscale{Enabled: true, MinReplicas: ptr.To[int32](1), CPUTarget: ptr.To[int32](70)}), nil, Tracked{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := TrackedMetrics(tc.app, tc.hpa); got != tc.want {
				t.Fatalf("TrackedMetrics = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestTrackedRecommendation(t *testing.T) {
	rec := kipperv1.TunedResources{CPURequest: "300m", CPULimit: "300m", MemoryRequest: "1Gi", MemoryLimit: "1Gi"}
	cpuOnly := kipperv1.TunedResources{CPURequest: "300m", CPULimit: "300m"}
	memoryOnly := kipperv1.TunedResources{MemoryRequest: "1Gi", MemoryLimit: "1Gi"}
	cases := []struct {
		name    string
		tracked Tracked
		cause   string
		want    kipperv1.TunedResources
	}{
		{"nothing tracked", Tracked{}, "", rec},
		{"CPU tracked drops CPU", Tracked{CPU: true}, "", memoryOnly},
		{"CPU tracked drops CPU after an OOM too", Tracked{CPU: true}, kipperv1.MemoryCauseOOMKill, memoryOnly},
		{"memory tracked drops a routine value", Tracked{Memory: true}, "", cpuOnly},
		{"memory tracked keeps an OOM raise", Tracked{Memory: true}, kipperv1.MemoryCauseOOMKill, rec},
		{"both tracked keep only an OOM raise", Tracked{CPU: true, Memory: true}, kipperv1.MemoryCauseOOMKill, memoryOnly},
		{"both tracked, routine", Tracked{CPU: true, Memory: true}, "", kipperv1.TunedResources{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.tracked.Recommendation(kipperv1.ResourceTuningStatus{Recommendation: rec, MemoryCause: tc.cause})
			if got != tc.want {
				t.Fatalf("Recommendation = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestAutoscalerMetricsReadsOnlyResourceMetrics(t *testing.T) {
	resource := func(name corev1.ResourceName) autoscalingv2.MetricSpec {
		return autoscalingv2.MetricSpec{Type: autoscalingv2.ResourceMetricSourceType, Resource: &autoscalingv2.ResourceMetricSource{Name: name}}
	}
	for _, tc := range []struct {
		name string
		hpa  *autoscalingv2.HorizontalPodAutoscaler
		want Tracked
	}{
		{"nil autoscaler", nil, Tracked{}},
		{"memory then cpu", &autoscalingv2.HorizontalPodAutoscaler{Spec: autoscalingv2.HorizontalPodAutoscalerSpec{Metrics: []autoscalingv2.MetricSpec{
			resource(corev1.ResourceMemory), resource(corev1.ResourceCPU), resource(corev1.ResourceCPU),
		}}}, Tracked{CPU: true, Memory: true}},
		{"other metrics ignored", &autoscalingv2.HorizontalPodAutoscaler{Spec: autoscalingv2.HorizontalPodAutoscalerSpec{Metrics: []autoscalingv2.MetricSpec{
			{Type: autoscalingv2.ResourceMetricSourceType},
			{Type: autoscalingv2.PodsMetricSourceType},
			resource(corev1.ResourceEphemeralStorage),
		}}}, Tracked{}},
	} {
		if got := AutoscalerMetrics(tc.hpa); got != tc.want {
			t.Errorf("%s: AutoscalerMetrics = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}
