package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/handlers"
	quotapkg "github.com/getkipper/kipper/console-api/quota"
	"github.com/getkipper/kipper/controller/pkg/labels"
)

func TestCheckNodePressureThrottleAndEscalation(t *testing.T) {
	const allocatable = int64(100 * 1024 * 1024 * 1024) // 100Gi

	nodes := []corev1.Node{{
		Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
			corev1.ResourceMemory: *resource.NewQuantity(allocatable, resource.BinarySI),
		}},
	}}
	atPct := func(pct float64) map[string][]podMetricsEntry {
		return map[string][]podMetricsEntry{"ns": {{
			Namespace: "ns", PodName: "p", MemoryBytes: int64(float64(allocatable) * pct / 100),
		}}}
	}
	alerted := func(entries []ResourceLogEntry) bool { return len(entries) > 0 }

	rc := NewResourceController(nil, nil)
	ctx := context.Background()

	// First warning-level pressure alerts.
	if !alerted(rc.checkNodePressure(ctx, atPct(85), nodes)) {
		t.Fatal("first warning pressure should alert")
	}
	// Sustained same severity within the cooldown is suppressed.
	if alerted(rc.checkNodePressure(ctx, atPct(85), nodes)) {
		t.Error("sustained warning within cooldown should not re-alert")
	}
	// Escalation warning -> critical alerts immediately despite the cooldown.
	if !alerted(rc.checkNodePressure(ctx, atPct(95), nodes)) {
		t.Error("escalation to critical should alert immediately")
	}
	// Oscillating back down to warning must not re-alert (de-escalation).
	if alerted(rc.checkNodePressure(ctx, atPct(85), nodes)) {
		t.Error("de-escalation to warning should stay suppressed")
	}
	if alerted(rc.checkNodePressure(ctx, atPct(95), nodes)) {
		t.Error("re-crossing to critical within cooldown should stay suppressed")
	}
	// Hovering in the 75-80 hysteresis band neither alerts nor resets.
	if alerted(rc.checkNodePressure(ctx, atPct(78), nodes)) {
		t.Error("pressure in the hysteresis band should not alert")
	}
	if alerted(rc.checkNodePressure(ctx, atPct(95), nodes)) {
		t.Error("cooldown should survive a dip into the hysteresis band")
	}
}

func TestRoundMemory(t *testing.T) {
	tests := []struct {
		name     string
		bytes    int64
		expected string
	}{
		{
			name:     "already aligned to 64Mi",
			bytes:    64 * 1024 * 1024,
			expected: "64Mi",
		},
		{
			name:     "rounds up to next 64Mi",
			bytes:    65 * 1024 * 1024,
			expected: "128Mi",
		},
		{
			name:     "128Mi stays at 128Mi",
			bytes:    128 * 1024 * 1024,
			expected: "128Mi",
		},
		{
			name:     "small value rounds up to 64Mi",
			bytes:    1024,
			expected: "64Mi",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := roundMemory(tt.bytes)
			if result.String() != tt.expected {
				t.Errorf("expected %s, got %s", tt.expected, result.String())
			}
		})
	}
}

func TestRoundCPU(t *testing.T) {
	tests := []struct {
		name     string
		millis   int64
		expected int64
	}{
		{
			name:     "already aligned to 50m",
			millis:   100,
			expected: 100,
		},
		{
			name:     "rounds up to next 50m",
			millis:   110,
			expected: 150,
		},
		{
			name:     "small value rounds to 50m",
			millis:   10,
			expected: 50,
		},
		{
			name:     "51 rounds to 100",
			millis:   51,
			expected: 100,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := roundCPU(tt.millis)
			if result.MilliValue() != tt.expected {
				t.Errorf("expected %dm, got %dm", tt.expected, result.MilliValue())
			}
		})
	}
}

func TestProfileDefaults(t *testing.T) {
	tests := []struct {
		profile string
		cpu     string
		memory  string
	}{
		{profile: "lightweight", cpu: "50m", memory: "64Mi"},
		{profile: "standard", cpu: "100m", memory: "128Mi"},
		{profile: "compute-heavy", cpu: "500m", memory: "256Mi"},
		{profile: "memory-heavy", cpu: "100m", memory: "512Mi"},
		{profile: "", cpu: "100m", memory: "128Mi"},
		{profile: "unknown", cpu: "100m", memory: "128Mi"},
	}

	for _, tt := range tests {
		t.Run(tt.profile, func(t *testing.T) {
			d := profileDefaults(tt.profile)
			if d.cpu != tt.cpu {
				t.Errorf("expected cpu %s, got %s", tt.cpu, d.cpu)
			}
			if d.memory != tt.memory {
				t.Errorf("expected memory %s, got %s", tt.memory, d.memory)
			}
		})
	}
}

func TestConsistentHighMemory(t *testing.T) {
	reqBytes := int64(128 * 1024 * 1024) // 128Mi

	high := []usageObservation{
		{MemoryBytes: int64(float64(reqBytes) * 0.85)},
		{MemoryBytes: int64(float64(reqBytes) * 0.90)},
		{MemoryBytes: int64(float64(reqBytes) * 0.88)},
	}
	if !consistentHighMemory(high, reqBytes) {
		t.Error("expected consistently high memory")
	}

	mixed := []usageObservation{
		{MemoryBytes: int64(float64(reqBytes) * 0.85)},
		{MemoryBytes: int64(float64(reqBytes) * 0.50)},
		{MemoryBytes: int64(float64(reqBytes) * 0.88)},
	}
	if consistentHighMemory(mixed, reqBytes) {
		t.Error("expected mixed memory not to be consistently high")
	}
}

func TestConsistentLowMemory(t *testing.T) {
	reqBytes := int64(128 * 1024 * 1024)

	low := []usageObservation{
		{MemoryBytes: int64(float64(reqBytes) * 0.10)},
		{MemoryBytes: int64(float64(reqBytes) * 0.15)},
		{MemoryBytes: int64(float64(reqBytes) * 0.05)},
	}
	if !consistentLowMemory(low, reqBytes) {
		t.Error("expected consistently low memory")
	}

	mixed := []usageObservation{
		{MemoryBytes: int64(float64(reqBytes) * 0.10)},
		{MemoryBytes: int64(float64(reqBytes) * 0.50)},
		{MemoryBytes: int64(float64(reqBytes) * 0.05)},
	}
	if consistentLowMemory(mixed, reqBytes) {
		t.Error("expected mixed memory not to be consistently low")
	}
}

func TestConsistentHighCPU(t *testing.T) {
	reqMillis := int64(100)

	high := []usageObservation{
		{CPUMillis: 85},
		{CPUMillis: 90},
		{CPUMillis: 88},
	}
	if !consistentHighCPU(high, reqMillis) {
		t.Error("expected consistently high CPU")
	}

	mixed := []usageObservation{
		{CPUMillis: 85},
		{CPUMillis: 50},
		{CPUMillis: 88},
	}
	if consistentHighCPU(mixed, reqMillis) {
		t.Error("expected mixed CPU not to be consistently high")
	}
}

func TestConsistentLowCPU(t *testing.T) {
	reqMillis := int64(100)

	low := []usageObservation{
		{CPUMillis: 10},
		{CPUMillis: 15},
		{CPUMillis: 5},
	}
	if !consistentLowCPU(low, reqMillis) {
		t.Error("expected consistently low CPU")
	}
}

func TestEvaluateAndAdjustAppliesDefaults(t *testing.T) {
	rc := NewResourceController(nil, nil)

	container := &corev1.Container{
		Name: "web",
	}

	labels := map[string]string{
		labels.ResourceProfile: "standard",
	}

	entries, _ := rc.evaluateAndAdjust("default", "web", container, nil, labels, 2, false, 0)

	if len(entries) != 1 {
		t.Fatalf("expected 1 log entry, got %d", len(entries))
	}

	if entries[0].Action != "applied default resources" {
		t.Errorf("expected action 'applied default resources', got %q", entries[0].Action)
	}

	cpuReq := container.Resources.Requests[corev1.ResourceCPU]
	if cpuReq.String() != "100m" {
		t.Errorf("expected CPU request 100m, got %s", cpuReq.String())
	}

	memReq := container.Resources.Requests[corev1.ResourceMemory]
	if memReq.String() != "128Mi" {
		t.Errorf("expected memory request 128Mi, got %s", memReq.String())
	}
}

func TestEvaluateAndAdjustOOMKill(t *testing.T) {
	rc := NewResourceController(nil, nil)

	memQty := resource.MustParse("128Mi")
	cpuQty := resource.MustParse("100m")
	container := &corev1.Container{
		Name: "web",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceMemory: memQty,
				corev1.ResourceCPU:    cpuQty,
			},
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: memQty,
				corev1.ResourceCPU:    cpuQty,
			},
		},
	}

	oomTime := time.Now()
	podEntries := []podMetricsEntry{
		{
			Namespace:   "default",
			PodName:     "web-abc123",
			CPUMillis:   50,
			MemoryBytes: 100 * 1024 * 1024,
			OOMKilled:   true,
			OOMAt:       oomTime,
		},
	}

	entries, mark := rc.evaluateAndAdjust("default", "web", container, podEntries, map[string]string{}, 2, false, 0)

	if len(entries) != 1 {
		t.Fatalf("expected 1 log entry, got %d", len(entries))
	}

	if entries[0].Action != "doubled memory (OOMKilled)" {
		t.Errorf("expected action 'doubled memory (OOMKilled)', got %q", entries[0].Action)
	}

	// The mark is committed only after the workload update succeeds; do that
	// now. The same stale OOM event on the next tick must then be a no-op:
	// the kubelet's LastTerminationState lingers, so re-acting would storm
	// alerts and re-double memory. The dedup keys on the OOM finish time.
	rc.commitOOMMark(mark)
	repeat, _ := rc.evaluateAndAdjust("default", "web", container, podEntries, map[string]string{}, 2, false, 0)
	if len(repeat) != 0 {
		t.Errorf("expected no entries for an already-handled OOM, got %d", len(repeat))
	}
	if mem := container.Resources.Limits[corev1.ResourceMemory]; mem.String() != "256Mi" {
		t.Errorf("memory should stay at 256Mi after a repeated OOM, got %s", mem.String())
	}

	newMem := container.Resources.Limits[corev1.ResourceMemory]
	if newMem.String() != "256Mi" {
		t.Errorf("expected memory limit 256Mi, got %s", newMem.String())
	}
}

func TestScaleDownRequiresThreeConsecutiveChecks(t *testing.T) {
	// Scale-down requires len(history) >= 3 with consistently low usage.
	// Verify that fewer observations do not trigger a scale-down.
	// Start above the profile floor (standard = 100m/128Mi) so there is room to scale down.
	rc := NewResourceController(nil, nil)

	memQty := resource.MustParse("512Mi")
	cpuQty := resource.MustParse("500m")
	reqBytes := memQty.Value()
	reqMillis := cpuQty.MilliValue()

	makeContainer := func() *corev1.Container {
		m := resource.MustParse("512Mi")
		c := resource.MustParse("500m")
		return &corev1.Container{
			Name: "web",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceMemory: m,
					corev1.ResourceCPU:    c,
				},
				Limits: corev1.ResourceList{
					corev1.ResourceMemory: m,
					corev1.ResourceCPU:    c,
				},
			},
		}
	}

	lowUsagePods := []podMetricsEntry{{
		Namespace:   "default",
		PodName:     "web-abc123",
		CPUMillis:   int64(float64(reqMillis) * 0.05),
		MemoryBytes: int64(float64(reqBytes) * 0.05),
		Age:         10 * time.Minute,
	}}

	labels := map[string]string{}

	// First observation: should not scale down
	container1 := makeContainer()
	entries1, _ := rc.evaluateAndAdjust("default", "scaledown", container1, lowUsagePods, labels, 2, false, 0)
	if len(entries1) != 0 {
		t.Errorf("expected no entries after 1 low observation, got %d", len(entries1))
	}

	// Second observation: should not scale down
	container2 := makeContainer()
	entries2, _ := rc.evaluateAndAdjust("default", "scaledown", container2, lowUsagePods, labels, 2, false, 0)
	if len(entries2) != 0 {
		t.Errorf("expected no entries after 2 low observations, got %d", len(entries2))
	}

	// Third observation: should trigger scale-down
	container3 := makeContainer()
	entries3, _ := rc.evaluateAndAdjust("default", "scaledown", container3, lowUsagePods, labels, 2, false, 0)
	if len(entries3) == 0 {
		t.Error("expected entries after 3 consecutive low observations, got 0")
	}
}

func TestScaleUpTriggersOnConsistentHighUsage(t *testing.T) {
	// Scale-up triggers when all observations in history are above 80%.
	// It does not require a minimum of 3 observations — it triggers as soon
	// as all existing history is consistently high.
	rc := NewResourceController(nil, nil)

	memQty := resource.MustParse("128Mi")
	cpuQty := resource.MustParse("100m")
	reqBytes := memQty.Value()
	reqMillis := cpuQty.MilliValue()

	container := &corev1.Container{
		Name: "web",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceMemory: memQty,
				corev1.ResourceCPU:    cpuQty,
			},
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: memQty,
				corev1.ResourceCPU:    cpuQty,
			},
		},
	}

	highUsagePods := []podMetricsEntry{{
		Namespace:   "default",
		PodName:     "web-abc123",
		CPUMillis:   int64(float64(reqMillis) * 0.90),
		MemoryBytes: int64(float64(reqBytes) * 0.90),
		Age:         10 * time.Minute,
	}}

	entries, _ := rc.evaluateAndAdjust("default", "scaleup", container, highUsagePods, map[string]string{}, 2, false, 0)
	if len(entries) == 0 {
		t.Error("expected scale-up entries on first consistently high observation, got 0")
	}
}

func TestScaleUpPreservesBurstableLimit(t *testing.T) {
	// A burstable jvm workload requests 100m CPU / 256Mi but is allowed to
	// burst to 1000m CPU / 2Gi. Sustained load must raise the requests without
	// collapsing those higher limits, or cold-start headroom is lost.
	rc := NewResourceController(nil, nil)

	cpuReq := resource.MustParse("100m")
	cpuLim := resource.MustParse("1000m")
	memReq := resource.MustParse("256Mi")
	memLim := resource.MustParse("2Gi")

	container := &corev1.Container{
		Name: "web",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: cpuReq, corev1.ResourceMemory: memReq},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: cpuLim, corev1.ResourceMemory: memLim},
		},
	}

	highUsagePods := []podMetricsEntry{{
		Namespace:   "default",
		PodName:     "web-abc123",
		CPUMillis:   int64(float64(cpuReq.MilliValue()) * 0.90),
		MemoryBytes: int64(float64(memReq.Value()) * 0.90),
		Age:         10 * time.Minute,
	}}

	entries, _ := rc.evaluateAndAdjust("default", "jvm-app", container, highUsagePods, map[string]string{}, 2, false, 0)
	if len(entries) == 0 {
		t.Fatal("expected scale-up entries on consistently high usage")
	}

	// Requests should have risen above their originals.
	if got := container.Resources.Requests[corev1.ResourceCPU]; got.MilliValue() <= cpuReq.MilliValue() {
		t.Errorf("cpu request should have increased, got %s", got.String())
	}
	// Limits must not be collapsed below the burstable ceiling.
	if got := container.Resources.Limits[corev1.ResourceCPU]; got.Cmp(cpuLim) < 0 {
		t.Errorf("cpu limit collapsed to %s, want >= %s", got.String(), cpuLim.String())
	}
	if got := container.Resources.Limits[corev1.ResourceMemory]; got.Cmp(memLim) < 0 {
		t.Errorf("memory limit collapsed to %s, want >= %s", got.String(), memLim.String())
	}
}

func TestScaleDownResetByNormalUsage(t *testing.T) {
	// Verify that a normal-usage observation between low-usage observations
	// prevents scale-down since the history is not consistently low.
	rc := NewResourceController(nil, nil)

	memQty := resource.MustParse("128Mi")
	cpuQty := resource.MustParse("100m")
	reqBytes := memQty.Value()
	reqMillis := cpuQty.MilliValue()

	makeContainer := func() *corev1.Container {
		m := resource.MustParse("128Mi")
		c := resource.MustParse("100m")
		return &corev1.Container{
			Name: "web",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceMemory: m,
					corev1.ResourceCPU:    c,
				},
				Limits: corev1.ResourceList{
					corev1.ResourceMemory: m,
					corev1.ResourceCPU:    c,
				},
			},
		}
	}

	lowPods := []podMetricsEntry{{
		Namespace:   "default",
		PodName:     "web-abc123",
		CPUMillis:   int64(float64(reqMillis) * 0.05),
		MemoryBytes: int64(float64(reqBytes) * 0.05),
		Age:         10 * time.Minute,
	}}

	normalPods := []podMetricsEntry{{
		Namespace:   "default",
		PodName:     "web-abc123",
		CPUMillis:   int64(float64(reqMillis) * 0.50),
		MemoryBytes: int64(float64(reqBytes) * 0.50),
		Age:         10 * time.Minute,
	}}

	labels := map[string]string{}

	// Low, normal, low — history is [low, normal, low], should not trigger scale-down
	rc.evaluateAndAdjust("default", "reset-test", makeContainer(), lowPods, labels, 2, false, 0)
	rc.evaluateAndAdjust("default", "reset-test", makeContainer(), normalPods, labels, 2, false, 0)
	rc.evaluateAndAdjust("default", "reset-test", makeContainer(), lowPods, labels, 2, false, 0)

	// History now has 3 entries but they are not all low — no scale-down
	container := makeContainer()
	entries, _ := rc.evaluateAndAdjust("default", "reset-test", container, lowPods, labels, 2, false, 0)
	// History is now [normal, low, low] — still not all low
	if len(entries) != 0 {
		t.Errorf("expected no entries when low usage is interrupted by normal usage, got %d", len(entries))
	}
}

func TestRoundMemoryLargeValues(t *testing.T) {
	tests := []struct {
		name     string
		bytes    int64
		expected string
	}{
		{
			name:     "256Mi stays at 256Mi",
			bytes:    256 * 1024 * 1024,
			expected: "256Mi",
		},
		{
			name:     "300Mi rounds up to 320Mi",
			bytes:    300 * 1024 * 1024,
			expected: "320Mi",
		},
		{
			name:     "512Mi stays at 512Mi",
			bytes:    512 * 1024 * 1024,
			expected: "512Mi",
		},
		{
			name:     "1Gi stays at 1Gi",
			bytes:    1024 * 1024 * 1024,
			expected: "1Gi",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := roundMemory(tt.bytes)
			if result.String() != tt.expected {
				t.Errorf("expected %s, got %s", tt.expected, result.String())
			}
		})
	}
}

func TestRoundCPUEdgeCases(t *testing.T) {
	tests := []struct {
		name     string
		millis   int64
		expected int64
	}{
		{
			name:     "1m rounds to 50m",
			millis:   1,
			expected: 50,
		},
		{
			name:     "250m stays at 250m",
			millis:   250,
			expected: 250,
		},
		{
			name:     "500m stays at 500m",
			millis:   500,
			expected: 500,
		},
		{
			name:     "999m rounds to 1000m",
			millis:   999,
			expected: 1000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := roundCPU(tt.millis)
			if result.MilliValue() != tt.expected {
				t.Errorf("expected %dm, got %dm", tt.expected, result.MilliValue())
			}
		})
	}
}

func TestProfileDefaultsComputeHeavy(t *testing.T) {
	d := profileDefaults("compute-heavy")
	if d.cpu != "500m" || d.memory != "256Mi" || d.name != "compute-heavy" {
		t.Errorf("unexpected compute-heavy defaults: cpu=%s memory=%s name=%s", d.cpu, d.memory, d.name)
	}
}

func TestProfileDefaultsMemoryHeavy(t *testing.T) {
	d := profileDefaults("memory-heavy")
	if d.cpu != "100m" || d.memory != "512Mi" || d.name != "memory-heavy" {
		t.Errorf("unexpected memory-heavy defaults: cpu=%s memory=%s name=%s", d.cpu, d.memory, d.name)
	}
}

func TestProfileDefaultsCaseInsensitive(t *testing.T) {
	d := profileDefaults("LIGHTWEIGHT")
	if d.cpu != "50m" || d.memory != "64Mi" {
		t.Errorf("expected lightweight defaults for uppercase input, got cpu=%s memory=%s", d.cpu, d.memory)
	}
}

func TestEvaluateAndAdjustAppliesLightweightProfile(t *testing.T) {
	rc := NewResourceController(nil, nil)

	container := &corev1.Container{Name: "worker"}
	labels := map[string]string{labels.ResourceProfile: "lightweight"}

	entries, _ := rc.evaluateAndAdjust("default", "worker", container, nil, labels, 2, false, 0)

	if len(entries) != 1 {
		t.Fatalf("expected 1 log entry, got %d", len(entries))
	}

	cpuReq := container.Resources.Requests[corev1.ResourceCPU]
	if cpuReq.String() != "50m" {
		t.Errorf("expected CPU request 50m, got %s", cpuReq.String())
	}

	memReq := container.Resources.Requests[corev1.ResourceMemory]
	if memReq.String() != "64Mi" {
		t.Errorf("expected memory request 64Mi, got %s", memReq.String())
	}
}

func TestNoOpWhenAlreadyAtMinimum(t *testing.T) {
	// When resources are at the minimum rounding boundary (50m CPU, 64Mi memory),
	// halving and re-rounding produces the same value. The controller must not
	// generate a log entry or update the deployment in this case.
	rc := NewResourceController(nil, nil)

	makeContainer := func() *corev1.Container {
		m := resource.MustParse("64Mi")
		c := resource.MustParse("50m")
		return &corev1.Container{
			Name: "web",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceMemory: m,
					corev1.ResourceCPU:    c,
				},
				Limits: corev1.ResourceList{
					corev1.ResourceMemory: m,
					corev1.ResourceCPU:    c,
				},
			},
		}
	}

	lowPods := []podMetricsEntry{{
		Namespace:   "default",
		PodName:     "web-abc123",
		CPUMillis:   1,
		MemoryBytes: 1024 * 1024, // ~1Mi
		Age:         10 * time.Minute,
	}}

	labels := map[string]string{}

	// Build up 3 observations so scale-down logic is eligible
	rc.evaluateAndAdjust("default", "min-test", makeContainer(), lowPods, labels, 2, false, 0)
	rc.evaluateAndAdjust("default", "min-test", makeContainer(), lowPods, labels, 2, false, 0)
	rc.evaluateAndAdjust("default", "min-test", makeContainer(), lowPods, labels, 2, false, 0)

	// Fourth tick — should produce no entries since values can't decrease further
	container := makeContainer()
	entries, _ := rc.evaluateAndAdjust("default", "min-test", container, lowPods, labels, 2, false, 0)
	if len(entries) != 0 {
		t.Errorf("expected no entries when already at minimum, got %d: %v", len(entries), entries)
	}
}

func TestIsOOMKilled(t *testing.T) {
	tests := []struct {
		name     string
		pod      *corev1.Pod
		expected bool
	}{
		{
			name: "terminated with OOMKilled",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{
							State: corev1.ContainerState{
								Terminated: &corev1.ContainerStateTerminated{
									Reason: "OOMKilled",
								},
							},
						},
					},
				},
			},
			expected: true,
		},
		{
			name: "last termination OOMKilled",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{
							LastTerminationState: corev1.ContainerState{
								Terminated: &corev1.ContainerStateTerminated{
									Reason: "OOMKilled",
								},
							},
						},
					},
				},
			},
			expected: true,
		},
		{
			name: "running pod",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{
							State: corev1.ContainerState{
								Running: &corev1.ContainerStateRunning{},
							},
						},
					},
				},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isOOMKilled(tt.pod)
			if result != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, result)
			}
		})
	}
}

func TestStartupGracePeriodIgnoresYoungPods(t *testing.T) {
	rc := NewResourceController(nil, nil)

	memQty := resource.MustParse("128Mi")
	cpuQty := resource.MustParse("100m")
	reqBytes := memQty.Value()
	reqMillis := cpuQty.MilliValue()

	container := &corev1.Container{
		Name: "web",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceMemory: memQty,
				corev1.ResourceCPU:    cpuQty,
			},
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: memQty,
				corev1.ResourceCPU:    cpuQty,
			},
		},
	}

	// High-but-not-saturated young pods are filtered by the grace period.
	// (The saturation override at >=95% kicks in independently and is
	// exercised by TestSaturationBypassesStartupGrace below.)
	youngPods := []podMetricsEntry{{
		Namespace:   "default",
		PodName:     "web-abc123",
		CPUMillis:   int64(float64(reqMillis) * 0.85),
		MemoryBytes: int64(float64(reqBytes) * 0.85),
		Age:         2 * time.Minute,
	}}

	entries, _ := rc.evaluateAndAdjust("default", "young-pods", container, youngPods, map[string]string{}, 2, false, 0)
	if len(entries) != 0 {
		t.Errorf("expected no entries for young pods, got %d: %v", len(entries), entries)
	}

	// Verify container resources were not changed
	if container.Resources.Requests.Cpu().String() != "100m" {
		t.Errorf("expected CPU unchanged at 100m, got %s", container.Resources.Requests.Cpu().String())
	}
	if container.Resources.Requests.Memory().String() != "128Mi" {
		t.Errorf("expected memory unchanged at 128Mi, got %s", container.Resources.Requests.Memory().String())
	}
}

func TestSaturationBypassesStartupGrace(t *testing.T) {
	rc := NewResourceController(nil, nil)

	cpuQty := resource.MustParse("100m")
	memQty := resource.MustParse("128Mi")

	container := &corev1.Container{
		Name: "web",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    cpuQty,
				corev1.ResourceMemory: memQty,
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    cpuQty,
				corev1.ResourceMemory: memQty,
			},
		},
	}

	// Pod past the saturation warmup but still inside the 5-min grace,
	// pinned at the CPU limit. This is the JVM-stuck-at-100m case: JIT can't
	// finish at this ceiling, so the pod sits at 100% forever. The grace
	// period must NOT mask this.
	pinned := []podMetricsEntry{{
		Namespace: "default",
		PodName:   "jvm-abc123",
		CPUMillis: cpuQty.MilliValue(), // 100% of limit
		Age:       3 * time.Minute,
	}}

	entries, _ := rc.evaluateAndAdjust("default", "stuck-jvm", container, pinned, map[string]string{}, 1, false, 0)
	if len(entries) != 1 {
		t.Fatalf("expected 1 saturation bump, got %d: %v", len(entries), entries)
	}
	if entries[0].Action != "increased CPU (saturated)" {
		t.Errorf("expected saturation action, got %q", entries[0].Action)
	}
	if container.Resources.Limits.Cpu().MilliValue() <= cpuQty.MilliValue() {
		t.Errorf("expected CPU limit raised above %s, got %s",
			cpuQty.String(), container.Resources.Limits.Cpu().String())
	}
}

// saturatedContainer builds a container whose CPU limit the test pods pin.
func saturatedContainer() *corev1.Container {
	cpuQty := resource.MustParse("100m")
	memQty := resource.MustParse("128Mi")
	return &corev1.Container{
		Name: "db",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    cpuQty,
				corev1.ResourceMemory: memQty,
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    cpuQty,
				corev1.ResourceMemory: memQty,
			},
		},
	}
}

// TestSaturationWarmupIgnoresBootingPods pins the restart-loop fix: a
// freshly started pod at 100% of its CPU limit is booting, not
// bottlenecked. Bumping on it rolls the workload, whose replacement boots
// pinned again — every 60 seconds.
func TestSaturationWarmupIgnoresBootingPods(t *testing.T) {
	rc := NewResourceController(nil, nil)
	container := saturatedContainer()

	booting := []podMetricsEntry{{
		Namespace: "default",
		PodName:   "web-abc123",
		CPUMillis: 100, // 100% of the 100m limit
		Age:       1 * time.Minute,
	}}

	entries, _ := rc.evaluateAndAdjust("default", "booting-jvm", container, booting, map[string]string{}, 1, false, 0)
	if len(entries) != 0 {
		t.Fatalf("expected no bump for a booting pod, got %d: %v", len(entries), entries)
	}
	if container.Resources.Limits.Cpu().String() != "100m" {
		t.Errorf("expected CPU limit unchanged at 100m, got %s", container.Resources.Limits.Cpu().String())
	}
}

// TestStatefulSetSaturationNeedsSustainedHeat pins the bulk-load fix: a
// StatefulSet hot for one tick (a restore, a bulk import) must not be
// rolled; only saturation sustained across the whole window bumps it.
func TestStatefulSetSaturationNeedsSustainedHeat(t *testing.T) {
	rc := NewResourceController(nil, nil)

	pinned := []podMetricsEntry{{
		Namespace: "default",
		PodName:   "mongodb-0",
		CPUMillis: 100, // 100% of the 100m limit
		Age:       20 * time.Minute,
	}}

	// First hot tick: starts the window, no bump.
	container := saturatedContainer()
	entries, _ := rc.evaluateAndAdjust("default", "mongodb", container, pinned, map[string]string{}, 1, false, statefulSetSaturationWindow)
	if len(entries) != 0 {
		t.Fatalf("expected no bump on the first hot tick, got %d: %v", len(entries), entries)
	}
	if container.Resources.Limits.Cpu().String() != "100m" {
		t.Errorf("expected CPU limit unchanged at 100m, got %s", container.Resources.Limits.Cpu().String())
	}

	// A cool tick resets the window: the heat was transient.
	idle := []podMetricsEntry{{
		Namespace: "default",
		PodName:   "mongodb-0",
		CPUMillis: 10,
		Age:       20 * time.Minute,
	}}
	rc.evaluateAndAdjust("default", "mongodb", saturatedContainer(), idle, map[string]string{}, 1, false, statefulSetSaturationWindow)
	if _, tracked := rc.cpuPinned["default/mongodb"]; tracked {
		t.Error("expected the pinned window to reset after a cool tick")
	}

	// Saturation sustained past the window: the bump fires. Backdate the
	// window to simulate the earlier hot ticks.
	container = saturatedContainer()
	rc.cpuPinned["default/mongodb"] = pinnedWindow{
		since:   time.Now().Add(-statefulSetSaturationWindow),
		lastHot: time.Now().Add(-time.Minute),
	}
	entries, _ = rc.evaluateAndAdjust("default", "mongodb", container, pinned, map[string]string{}, 1, false, statefulSetSaturationWindow)
	if len(entries) != 1 {
		t.Fatalf("expected 1 bump after sustained saturation, got %d: %v", len(entries), entries)
	}
	if entries[0].Action != "increased CPU (saturated)" {
		t.Errorf("expected saturation action, got %q", entries[0].Action)
	}
	if _, tracked := rc.cpuPinned["default/mongodb"]; tracked {
		t.Error("expected the pinned window to reset after the bump")
	}
}

// TestStatefulSetSaturationWindowNeedsContinuity pins the gap rule: two hot
// observations separated by an unobserved stretch (metrics outage, skipped
// ticks) are not "sustained" — the window restarts at the second one.
func TestStatefulSetSaturationWindowNeedsContinuity(t *testing.T) {
	rc := NewResourceController(nil, nil)

	pinned := []podMetricsEntry{{
		Namespace: "default",
		PodName:   "mongodb-0",
		CPUMillis: 100, // 100% of the 100m limit
		Age:       20 * time.Minute,
	}}

	// A hot observation long enough ago to satisfy the window length, but
	// with a gap since then that exceeds maxSaturationGap.
	staleHot := time.Now().Add(-maxSaturationGap - time.Minute)
	rc.cpuPinned["default/mongodb"] = pinnedWindow{since: staleHot, lastHot: staleHot}

	container := saturatedContainer()
	entries, _ := rc.evaluateAndAdjust("default", "mongodb", container, pinned, map[string]string{}, 1, false, statefulSetSaturationWindow)
	if len(entries) != 0 {
		t.Fatalf("expected no bump across an observation gap, got %d: %v", len(entries), entries)
	}

	w, tracked := rc.cpuPinned["default/mongodb"]
	if !tracked {
		t.Fatal("expected a restarted pinned window to be tracked")
	}
	if !w.since.After(staleHot) {
		t.Error("expected the window start to restart at the fresh observation")
	}
}

func TestSingleReplicaSkipsScaleDown(t *testing.T) {
	makeContainer := func() *corev1.Container {
		m := resource.MustParse("512Mi")
		c := resource.MustParse("500m")
		return &corev1.Container{
			Name: "web",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceMemory: m,
					corev1.ResourceCPU:    c,
				},
				Limits: corev1.ResourceList{
					corev1.ResourceMemory: m,
					corev1.ResourceCPU:    c,
				},
			},
		}
	}

	memQty := resource.MustParse("512Mi")
	cpuQty := resource.MustParse("500m")
	reqBytes := memQty.Value()
	reqMillis := cpuQty.MilliValue()

	lowPods := []podMetricsEntry{{
		Namespace:   "default",
		PodName:     "web-abc123",
		CPUMillis:   int64(float64(reqMillis) * 0.05),
		MemoryBytes: int64(float64(reqBytes) * 0.05),
		Age:         10 * time.Minute,
	}}
	labels := map[string]string{}

	// With replicas=1, scale-down should never trigger
	rc1 := NewResourceController(nil, nil)
	for i := 0; i < 4; i++ {
		entries, _ := rc1.evaluateAndAdjust("default", "single-replica", makeContainer(), lowPods, labels, 1, false, 0)
		if len(entries) != 0 {
			t.Errorf("replicas=1: expected no scale-down entries on tick %d, got %d: %v", i+1, len(entries), entries)
		}
	}

	// With replicas=2, scale-down should trigger after 3 observations
	rc2 := NewResourceController(nil, nil)
	rc2.evaluateAndAdjust("default", "multi-replica", makeContainer(), lowPods, labels, 2, false, 0)
	rc2.evaluateAndAdjust("default", "multi-replica", makeContainer(), lowPods, labels, 2, false, 0)
	container3 := makeContainer()
	entries, _ := rc2.evaluateAndAdjust("default", "multi-replica", container3, lowPods, labels, 2, false, 0)
	if len(entries) == 0 {
		t.Error("replicas=2: expected scale-down entries after 3 low observations, got 0")
	}
}

func TestScaleUpStillWorksForSingleReplica(t *testing.T) {
	rc := NewResourceController(nil, nil)

	memQty := resource.MustParse("128Mi")
	cpuQty := resource.MustParse("100m")
	reqBytes := memQty.Value()
	reqMillis := cpuQty.MilliValue()

	container := &corev1.Container{
		Name: "web",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceMemory: memQty,
				corev1.ResourceCPU:    cpuQty,
			},
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: memQty,
				corev1.ResourceCPU:    cpuQty,
			},
		},
	}

	highPods := []podMetricsEntry{{
		Namespace:   "default",
		PodName:     "web-abc123",
		CPUMillis:   int64(float64(reqMillis) * 0.90),
		MemoryBytes: int64(float64(reqBytes) * 0.90),
		Age:         10 * time.Minute,
	}}

	entries, _ := rc.evaluateAndAdjust("default", "single-scaleup", container, highPods, map[string]string{}, 1, false, 0)
	if len(entries) == 0 {
		t.Error("expected scale-up entries for single replica with high usage, got 0")
	}

	// Verify resources actually increased
	newCPU := container.Resources.Requests[corev1.ResourceCPU]
	if newCPU.MilliValue() <= cpuQty.MilliValue() {
		t.Errorf("expected CPU to increase beyond 100m, got %s", newCPU.String())
	}
	newMem := container.Resources.Requests[corev1.ResourceMemory]
	if newMem.Value() <= memQty.Value() {
		t.Errorf("expected memory to increase beyond 128Mi, got %s", newMem.String())
	}
}

func TestOOMHandlingIgnoresReplicaCount(t *testing.T) {
	rc := NewResourceController(nil, nil)

	memQty := resource.MustParse("128Mi")
	cpuQty := resource.MustParse("100m")
	container := &corev1.Container{
		Name: "web",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceMemory: memQty,
				corev1.ResourceCPU:    cpuQty,
			},
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: memQty,
				corev1.ResourceCPU:    cpuQty,
			},
		},
	}

	oomPods := []podMetricsEntry{{
		Namespace:   "default",
		PodName:     "web-abc123",
		CPUMillis:   50,
		MemoryBytes: 100 * 1024 * 1024,
		OOMKilled:   true,
		OOMAt:       time.Now(),
		Age:         10 * time.Minute,
	}}

	entries, _ := rc.evaluateAndAdjust("default", "oom-single", container, oomPods, map[string]string{}, 1, false, 0)
	if len(entries) != 1 {
		t.Fatalf("expected 1 log entry, got %d", len(entries))
	}
	if entries[0].Action != "doubled memory (OOMKilled)" {
		t.Errorf("expected action 'doubled memory (OOMKilled)', got %q", entries[0].Action)
	}

	newMem := container.Resources.Limits[corev1.ResourceMemory]
	if newMem.String() != "256Mi" {
		t.Errorf("expected memory limit 256Mi, got %s", newMem.String())
	}
}

func TestProfileFloorPreventsScaleDown(t *testing.T) {
	// Database profile has floor at 250m CPU / 256Mi memory.
	// Start at exactly those values with low usage — no scale-down should happen.
	makeContainer := func() *corev1.Container {
		m := resource.MustParse("256Mi")
		c := resource.MustParse("250m")
		return &corev1.Container{
			Name: "db",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceMemory: m,
					corev1.ResourceCPU:    c,
				},
				Limits: corev1.ResourceList{
					corev1.ResourceMemory: m,
					corev1.ResourceCPU:    c,
				},
			},
		}
	}

	lowPods := []podMetricsEntry{{
		Namespace:   "default",
		PodName:     "db-abc123",
		CPUMillis:   5,
		MemoryBytes: 10 * 1024 * 1024,
		Age:         10 * time.Minute,
	}}

	labels := map[string]string{labels.ResourceProfile: "database"}
	rc := NewResourceController(nil, nil)

	// Build up 3+ observations so scale-down logic is eligible
	for i := 0; i < 4; i++ {
		container := makeContainer()
		entries, _ := rc.evaluateAndAdjust("default", "db-floor", container, lowPods, labels, 2, false, 0)
		if len(entries) != 0 {
			t.Errorf("tick %d: expected no entries when at profile floor, got %d: %v", i+1, len(entries), entries)
		}
	}
}

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = kipperv1.AddToScheme(s)
	return s
}

func testCRClient(objs ...crclient.Object) crclient.Client {
	return crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(objs...).Build()
}

func TestSyncAppCRResources(t *testing.T) {
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myapp",
			Namespace: "default",
		},
		Spec: kipperv1.AppSpec{
			Image: "nginx:latest",
			Port:  8080,
			Resources: kipperv1.AppResources{
				Profile:       "standard",
				CPURequest:    "100m",
				CPULimit:      "100m",
				MemoryRequest: "128Mi",
				MemoryLimit:   "128Mi",
			},
		},
	}

	crClient := testCRClient(app)
	rc := NewResourceController(nil, crClient)

	// Simulate the resource controller having adjusted the container
	container := &corev1.Container{
		Name: "myapp",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("200m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("200m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
		},
	}

	rc.syncAppCRResources(context.TODO(), "default", "myapp", container)

	// Verify the App CR was updated
	var updated kipperv1.App
	err := crClient.Get(context.TODO(), crclient.ObjectKey{Namespace: "default", Name: "myapp"}, &updated)
	if err != nil {
		t.Fatalf("failed to get updated App CR: %v", err)
	}

	if updated.Spec.Resources.CPURequest != "200m" {
		t.Errorf("expected CPURequest 200m, got %s", updated.Spec.Resources.CPURequest)
	}
	if updated.Spec.Resources.CPULimit != "200m" {
		t.Errorf("expected CPULimit 200m, got %s", updated.Spec.Resources.CPULimit)
	}
	if updated.Spec.Resources.MemoryRequest != "256Mi" {
		t.Errorf("expected MemoryRequest 256Mi, got %s", updated.Spec.Resources.MemoryRequest)
	}
	if updated.Spec.Resources.MemoryLimit != "256Mi" {
		t.Errorf("expected MemoryLimit 256Mi, got %s", updated.Spec.Resources.MemoryLimit)
	}
	// Profile stays "standard" because the code only sets "custom" when profile is empty
	if updated.Spec.Resources.Profile != "standard" {
		t.Errorf("expected Profile 'standard' (unchanged), got %s", updated.Spec.Resources.Profile)
	}

	// Also verify that an App with no profile gets set to "custom"
	appNoProfile := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "noprefix",
			Namespace: "default",
		},
		Spec: kipperv1.AppSpec{
			Image: "nginx:latest",
			Port:  8080,
			Resources: kipperv1.AppResources{
				CPURequest:    "100m",
				CPULimit:      "100m",
				MemoryRequest: "128Mi",
				MemoryLimit:   "128Mi",
			},
		},
	}
	crClient2 := testCRClient(appNoProfile)
	rc2 := NewResourceController(nil, crClient2)
	rc2.syncAppCRResources(context.TODO(), "default", "noprefix", container)

	var updated2 kipperv1.App
	if err := crClient2.Get(context.TODO(), crclient.ObjectKey{Namespace: "default", Name: "noprefix"}, &updated2); err != nil {
		t.Fatalf("failed to get updated App CR: %v", err)
	}
	if updated2.Spec.Resources.Profile != "custom" {
		t.Errorf("expected Profile 'custom' when originally empty, got %s", updated2.Spec.Resources.Profile)
	}
}

func TestCheckImagePullBackOff(t *testing.T) {
	kipper := map[string]string{"app.kubernetes.io/managed-by": "kipper"}

	t.Run("detects ImagePullBackOff", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "web-abc123",
				Namespace: "staging",
				Labels:    map[string]string{"app.kubernetes.io/managed-by": "kipper", "app": "web"},
			},
			Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{
					{
						Name:  "web",
						State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}},
					},
				},
			},
		}
		rc := NewResourceController(fake.NewClientset(pod), nil)

		entries, _ := rc.checkPodProblems(context.Background())

		if len(entries) != 1 {
			t.Fatalf("expected 1 entry, got %d", len(entries))
		}
		if entries[0].Action != "ImagePullBackOff" {
			t.Errorf("expected action 'ImagePullBackOff', got %q", entries[0].Action)
		}
		if entries[0].App != "web" {
			t.Errorf("expected app 'web', got %q", entries[0].App)
		}
		if entries[0].Namespace != "staging" {
			t.Errorf("expected namespace 'staging', got %q", entries[0].Namespace)
		}
	})

	t.Run("detects ErrImagePull", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "api-xyz",
				Namespace: "prod",
				Labels:    map[string]string{"app.kubernetes.io/managed-by": "kipper", "app": "api"},
			},
			Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{
					{
						Name:  "api",
						State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ErrImagePull"}},
					},
				},
			},
		}
		rc := NewResourceController(fake.NewClientset(pod), nil)

		entries, _ := rc.checkPodProblems(context.Background())

		if len(entries) != 1 {
			t.Fatalf("expected 1 entry, got %d", len(entries))
		}
		if entries[0].Action != "ImagePullBackOff" {
			t.Errorf("expected action 'ImagePullBackOff', got %q", entries[0].Action)
		}
	})

	t.Run("ignores running container", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "web-running", Namespace: "staging", Labels: kipper},
			Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{
					{
						Name:  "web",
						State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
					},
				},
			},
		}
		rc := NewResourceController(fake.NewClientset(pod), nil)

		entries, _ := rc.checkPodProblems(context.Background())

		if len(entries) != 0 {
			t.Errorf("expected 0 entries for running container, got %d", len(entries))
		}
	})

	t.Run("ignores other waiting reasons", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "web-init", Namespace: "staging", Labels: kipper},
			Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{
					{
						Name:  "web",
						State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}},
					},
				},
			},
		}
		rc := NewResourceController(fake.NewClientset(pod), nil)

		entries, _ := rc.checkPodProblems(context.Background())

		if len(entries) != 0 {
			t.Errorf("expected 0 entries for ContainerCreating, got %d", len(entries))
		}
	})

	t.Run("returns empty when no pods exist", func(t *testing.T) {
		rc := NewResourceController(fake.NewClientset(), nil)

		entries, _ := rc.checkPodProblems(context.Background())

		if len(entries) != 0 {
			t.Errorf("expected 0 entries with no pods, got %d", len(entries))
		}
	})

	t.Run("respects 60-minute cooldown", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "web-abc",
				Namespace: "staging",
				Labels:    map[string]string{"app.kubernetes.io/managed-by": "kipper", "app": "web"},
			},
			Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{
					{
						Name:  "web",
						State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}},
					},
				},
			},
		}
		rc := NewResourceController(fake.NewClientset(pod), nil)

		first, marks := rc.checkPodProblems(context.Background())
		if len(first) != 1 {
			t.Fatalf("expected 1 entry on first call, got %d", len(first))
		}
		rc.commitMarks(marks)

		// Second call immediately after should return nothing (within cooldown)
		second, _ := rc.checkPodProblems(context.Background())
		if len(second) != 0 {
			t.Errorf("expected 0 entries within cooldown window, got %d", len(second))
		}
	})

	t.Run("alerts each affected container once per pod", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "web-multi",
				Namespace: "staging",
				Labels:    map[string]string{"app.kubernetes.io/managed-by": "kipper", "app": "web"},
			},
			Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{
					{
						Name:  "app",
						State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}},
					},
					{
						Name:  "sidecar",
						State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}},
					},
				},
			},
		}
		rc := NewResourceController(fake.NewClientset(pod), nil)

		entries, _ := rc.checkPodProblems(context.Background())

		if len(entries) != 2 {
			t.Errorf("expected 1 entry per failing container (2 total), got %d", len(entries))
		}
	})
}

func TestCheckPodProblemsCrashLoop(t *testing.T) {
	kipper := map[string]string{"app.kubernetes.io/managed-by": "kipper", "app": "web"}

	t.Run("alerts non-OOM crash loop", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "staging", Labels: kipper},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
				Name:                 "web",
				State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 1}},
			}}},
		}
		rc := NewResourceController(fake.NewClientset(pod), nil)
		entries, _ := rc.checkPodProblems(context.Background())
		if len(entries) != 1 || entries[0].Action != "CrashLoopBackOff" {
			t.Fatalf("expected 1 CrashLoopBackOff entry, got %+v", entries)
		}
	})

	t.Run("skips OOM crash loop (handled by the OOM path)", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "web-2", Namespace: "staging", Labels: kipper},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
				Name:                 "web",
				State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137}},
			}}},
		}
		rc := NewResourceController(fake.NewClientset(pod), nil)
		if entries, _ := rc.checkPodProblems(context.Background()); len(entries) != 0 {
			t.Fatalf("expected OOM crash loop to be skipped, got %+v", entries)
		}
	})
}

func TestCheckNodeReady(t *testing.T) {
	notReady := corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}},
	}
	rc := NewResourceController(fake.NewClientset(), nil)
	nodes := []corev1.Node{notReady}

	entries, marks := rc.checkNodeReady(nodes)
	if len(entries) != 1 || entries[0].Action != "node NotReady" || entries[0].Severity != "critical" {
		t.Fatalf("expected 1 critical node NotReady entry, got %+v", entries)
	}
	rc.commitMarks(marks)
	if again, _ := rc.checkNodeReady(nodes); len(again) != 0 {
		t.Fatalf("expected cooldown to suppress repeat, got %+v", again)
	}
}

func TestCheckFailedJobs(t *testing.T) {
	failedJob := func(uid string) *batchv1.Job {
		return &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "build-web-abc",
				Namespace: "staging",
				UID:       types.UID(uid),
				Labels:    map[string]string{"app.kubernetes.io/managed-by": "kipper", "app": "web"},
			},
			Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}},
		}
	}

	client := fake.NewClientset(failedJob("uid-1"))
	rc := NewResourceController(client, nil)

	entries, marks := rc.checkFailedJobs(context.Background())
	if len(entries) != 1 || entries[0].Action != "job failed" {
		t.Fatalf("expected 1 job-failed entry, got %+v", entries)
	}
	rc.commitMarks(marks)
	if again, _ := rc.checkFailedJobs(context.Background()); len(again) != 0 {
		t.Fatalf("expected cooldown to suppress repeat of the same Job, got %+v", again)
	}

	// A build Job recreated with the same name but a new UID is a genuinely
	// new failed run and must alert again.
	if err := client.BatchV1().Jobs("staging").Delete(context.Background(), "build-web-abc", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := client.BatchV1().Jobs("staging").Create(context.Background(), failedJob("uid-2"), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if rerun, _ := rc.checkFailedJobs(context.Background()); len(rerun) != 1 {
		t.Fatalf("expected a new failed Job (new UID) to alert, got %+v", rerun)
	}
}

func TestCheckStuckRollouts(t *testing.T) {
	stuck := appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "staging", Labels: map[string]string{"app": "web"}},
		Status: appsv1.DeploymentStatus{Conditions: []appsv1.DeploymentCondition{{
			Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded",
		}}},
	}
	rc := NewResourceController(nil, nil)

	entries, marks := rc.checkStuckRollouts([]appsv1.Deployment{stuck})
	if len(entries) != 1 || entries[0].Action != "rollout stuck" {
		t.Fatalf("expected 1 rollout-stuck entry, got %+v", entries)
	}
	rc.commitMarks(marks)
	if again, _ := rc.checkStuckRollouts([]appsv1.Deployment{stuck}); len(again) != 0 {
		t.Fatalf("expected cooldown to suppress repeat, got %+v", again)
	}
}

// A mass-crash tick that generates more failure alerts than the per-write cap
// must commit cooldown marks only for the alerts actually written. The oldest
// overflow alerts are deferred, so their marks stay uncommitted and they remain
// eligible to fire on the next tick.
func TestTickCommitsMarksOnlyForPersistedAlerts(t *testing.T) {
	n := handlers.MaxAlertsPerWrite + 5
	// Expert mode so the tick returns after the failure-alert path instead of
	// running the auto-tuning path, which would query metrics-server.
	objs := []runtime.Object{&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "kipper-mode", Namespace: "kipper-system"},
		Data:       map[string]string{"mode": "expert"},
	}}
	for i := 0; i < n; i++ {
		objs = append(objs, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("web-%d", i),
				Namespace: "staging",
				Labels:    map[string]string{"app.kubernetes.io/managed-by": "kipper", "app": fmt.Sprintf("web-%d", i)},
			},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "web",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}},
			}}},
		})
	}
	rc := NewResourceController(fake.NewClientset(objs...), nil)

	rc.tick(context.Background())

	rc.mu.Lock()
	committed := len(rc.imagePullAlerted)
	rc.mu.Unlock()
	if committed != handlers.MaxAlertsPerWrite {
		t.Fatalf("expected marks committed only for the %d persisted alerts, got %d of %d generated", handlers.MaxAlertsPerWrite, committed, n)
	}
}

// A cooldown mark is only committed after the alert write lands. Until then the
// same problem must stay eligible, so a failed write re-alerts next tick rather
// than suppressing it for the cooldown window.
func TestCheckPodProblemsDefersMarkUntilCommit(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-1",
			Namespace: "staging",
			Labels:    map[string]string{"app.kubernetes.io/managed-by": "kipper", "app": "web"},
		},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name:  "web",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}},
		}}},
	}
	rc := NewResourceController(fake.NewClientset(pod), nil)

	first, marks := rc.checkPodProblems(context.Background())
	if len(first) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(first))
	}
	if again, _ := rc.checkPodProblems(context.Background()); len(again) != 1 {
		t.Fatalf("an uncommitted mark must not suppress re-alert, got %d", len(again))
	}
	rc.commitMarks(marks)
	if third, _ := rc.checkPodProblems(context.Background()); len(third) != 0 {
		t.Fatalf("a committed mark must suppress within cooldown, got %d", len(third))
	}
}

// An OOM older than the actionable window (e.g. a LastTerminationState that
// lingers across a controller restart, leaving oomHandledAt empty) must not be
// re-actioned: no memory doubling, no mark, and it falls through to tuning.
func TestEvaluateAndAdjustStaleOOMNotReactioned(t *testing.T) {
	rc := NewResourceController(nil, nil)

	memQty := resource.MustParse("128Mi")
	container := &corev1.Container{
		Name: "web",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceMemory: memQty, corev1.ResourceCPU: resource.MustParse("100m")},
			Limits:   corev1.ResourceList{corev1.ResourceMemory: memQty, corev1.ResourceCPU: resource.MustParse("100m")},
		},
	}
	podEntries := []podMetricsEntry{{
		Namespace:   "default",
		PodName:     "web-abc",
		MemoryBytes: 100 * 1024 * 1024,
		OOMKilled:   true,
		OOMAt:       time.Now().Add(-oomActionableWindow - time.Minute),
	}}

	entries, mark := rc.evaluateAndAdjust("default", "web", container, podEntries, map[string]string{}, 2, false, 0)

	if mark != nil {
		t.Errorf("a stale OOM must not stage a mark, got %+v", mark)
	}
	for _, e := range entries {
		if strings.Contains(e.Action, "OOM") {
			t.Errorf("a stale OOM must not produce an OOM action, got %q", e.Action)
		}
	}
	if mem := container.Resources.Limits[corev1.ResourceMemory]; mem.String() == "256Mi" {
		t.Error("a stale OOM must not double memory")
	}
}

// A crash-looping pod is absent from metrics-server, so its only entry is the
// synthetic zero-usage OOM record. When that OOM is already handled (not
// actionable), the fall-through to usage tuning must drop the synthetic entry
// rather than average its zeros and scale the still-crashing workload down.
func TestEvaluateAndAdjustHandledSyntheticOOMDoesNotTuneDown(t *testing.T) {
	rc := NewResourceController(nil, nil)

	memQty := resource.MustParse("256Mi")
	cpuQty := resource.MustParse("500m")
	container := &corev1.Container{
		Name: "web",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceMemory: memQty, corev1.ResourceCPU: cpuQty},
			Limits:   corev1.ResourceList{corev1.ResourceMemory: memQty, corev1.ResourceCPU: cpuQty},
		},
	}

	oomAt := time.Now()
	// Pre-mark this OOM as handled so it is not actionable on this call.
	rc.oomHandledAt["default/web/web"] = oomAt
	podEntries := []podMetricsEntry{{
		Namespace: "default",
		PodName:   "web-abc",
		OOMKilled: true,
		OOMAt:     oomAt,
		Synthetic: true,
		Age:       time.Hour, // past the startup grace, so zeros would otherwise average in
	}}

	entries, mark := rc.evaluateAndAdjust("default", "web", container, podEntries, map[string]string{}, 2, false, 0)

	if mark != nil {
		t.Errorf("an already-handled OOM must not stage a mark, got %+v", mark)
	}
	if len(entries) != 0 {
		t.Fatalf("a handled synthetic OOM must not tune resources, got %+v", entries)
	}
	if cpu := container.Resources.Requests[corev1.ResourceCPU]; cpu.String() != "500m" {
		t.Errorf("CPU must be untouched, got %s", cpu.String())
	}
	if mem := container.Resources.Requests[corev1.ResourceMemory]; mem.String() != "256Mi" {
		t.Errorf("memory must be untouched, got %s", mem.String())
	}
}

func TestEvaluateAndAdjustOOMRetriesUntilCommitted(t *testing.T) {
	// If the workload update fails, the caller never commits the OOM mark, so
	// the same OOM event must stay actionable on the next tick rather than
	// being permanently suppressed.
	rc := NewResourceController(nil, nil)
	mk := func() *corev1.Container {
		q := resource.MustParse("128Mi")
		return &corev1.Container{
			Name: "web",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: q},
				Limits:   corev1.ResourceList{corev1.ResourceMemory: q},
			},
		}
	}
	pods := []podMetricsEntry{{Namespace: "default", PodName: "web-1", OOMKilled: true, OOMAt: time.Now()}}

	// First tick doubles memory but the (simulated) update fails, so the mark
	// is left uncommitted.
	first, mark := rc.evaluateAndAdjust("default", "web", mk(), pods, map[string]string{}, 2, false, 0)
	if len(first) != 1 || mark == nil {
		t.Fatalf("expected a doubling entry and a pending mark, got entries=%d mark=%v", len(first), mark)
	}

	// Next tick: same OOM finish time, mark still uncommitted, so it must act
	// again instead of being suppressed.
	second, _ := rc.evaluateAndAdjust("default", "web", mk(), pods, map[string]string{}, 2, false, 0)
	if len(second) != 1 {
		t.Fatalf("expected the OOM to remain actionable until the mark is committed, got %d entries", len(second))
	}
}

func TestOOMEntriesForUncovered(t *testing.T) {
	kipper := map[string]string{"app.kubernetes.io/managed-by": "kipper", "app": "web"}
	crashLooper := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-oom", Namespace: "default", Labels: kipper},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name:                 "web",
			State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", FinishedAt: metav1.Now()}},
		}}},
	}
	healthy := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-ok", Namespace: "default", Labels: kipper},
	}
	noLabel := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "orphan", Namespace: "default", Labels: map[string]string{"app.kubernetes.io/managed-by": "kipper"}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", FinishedAt: metav1.Now()}},
		}}},
	}
	podInfo := map[string]*corev1.Pod{
		"default/web-oom": crashLooper,
		"default/web-ok":  healthy,
		"default/orphan":  noLabel,
	}
	// web-ok is covered by metrics; the OOM crash-looper and orphan are not.
	covered := map[string]bool{"default/web-ok": true}

	out := oomEntriesForUncovered(podInfo, covered)

	// Only the OOM crash-looper with an app label produces an entry.
	if len(out) != 1 {
		t.Fatalf("expected 1 app key, got %d: %+v", len(out), out)
	}
	entries := out["default/web"]
	if len(entries) != 1 || !entries[0].OOMKilled || entries[0].PodName != "web-oom" {
		t.Fatalf("expected one OOM entry for web-oom, got %+v", entries)
	}
	if entries[0].CPUMillis != 0 || entries[0].MemoryBytes != 0 {
		t.Errorf("expected zero usage on a no-metrics entry, got cpu=%d mem=%d", entries[0].CPUMillis, entries[0].MemoryBytes)
	}
}

func TestScaledOutApps(t *testing.T) {
	scaled := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec:       kipperv1.AppSpec{Autoscale: &kipperv1.AppAutoscale{Enabled: true, MinReplicas: 2}},
	}
	notAutoscaled := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "default"},
		Spec:       kipperv1.AppSpec{Autoscale: &kipperv1.AppAutoscale{Enabled: false}},
	}
	min := int32(2)
	webHPA := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec:       autoscalingv2.HorizontalPodAutoscalerSpec{MinReplicas: &min},
		Status:     autoscalingv2.HorizontalPodAutoscalerStatus{CurrentReplicas: 5},
	}
	// A worker HPA sitting at min would not count even if autoscale were on.
	workerHPA := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "default"},
		Spec:       autoscalingv2.HorizontalPodAutoscalerSpec{MinReplicas: &min},
		Status:     autoscalingv2.HorizontalPodAutoscalerStatus{CurrentReplicas: 2},
	}

	rc := NewResourceController(fake.NewClientset(webHPA, workerHPA), testCRClient(scaled, notAutoscaled))
	out := rc.scaledOutApps(context.Background())

	if !out["default/web"] {
		t.Error("expected default/web to be scaled out")
	}
	if out["default/worker"] {
		t.Error("expected default/worker not to be scaled out")
	}
}

func TestApplyQuotaCeilingBlocksOverQuotaIncrease(t *testing.T) {
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "kipper-quota", Namespace: "shop-test"},
		Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
			corev1.ResourceRequestsMemory: resource.MustParse("6Gi"),
		}},
		Status: corev1.ResourceQuotaStatus{
			Hard: corev1.ResourceList{corev1.ResourceRequestsMemory: resource.MustParse("6Gi")},
			Used: corev1.ResourceList{corev1.ResourceRequestsMemory: resource.MustParse("5Gi")},
		},
	}
	rc := NewResourceController(fake.NewClientset(quota), nil)

	before := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
	}
	container := &corev1.Container{Name: "web", Resources: corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")},
	}}
	increase := []ResourceLogEntry{{Action: "doubled memory (OOMKilled)"}}

	// Doubling 1Gi -> 2Gi on 2 replicas projects 5Gi + 2Gi = 7Gi > 6Gi hard.
	entries, blocked := rc.applyQuotaCeiling(context.Background(), "shop-test", "web", container, before, 2, 0, nil, increase)
	if !blocked {
		t.Fatal("expected the increase to be blocked by quota")
	}
	if len(entries) != 1 || entries[0].Action != "resource increase blocked by project quota" {
		t.Fatalf("expected a single quota-blocked entry, got %+v", entries)
	}
	if entries[0].Severity != "critical" {
		t.Errorf("expected critical severity, got %q", entries[0].Severity)
	}
	if mem := container.Resources.Requests[corev1.ResourceMemory]; mem.String() != "1Gi" {
		t.Errorf("blocked increase must be reverted, requests.memory = %s", mem.String())
	}

	// A second blocked attempt within the cooldown reverts silently.
	container.Resources.Requests[corev1.ResourceMemory] = resource.MustParse("2Gi")
	container.Resources.Limits[corev1.ResourceMemory] = resource.MustParse("2Gi")
	entries, blocked = rc.applyQuotaCeiling(context.Background(), "shop-test", "web", container, before, 2, 0, nil, increase)
	if !blocked || len(entries) != 0 {
		t.Fatalf("expected silent suppression within cooldown, got blocked=%v entries=%+v", blocked, entries)
	}
	if mem := container.Resources.Requests[corev1.ResourceMemory]; mem.String() != "1Gi" {
		t.Errorf("suppressed increase must still be reverted, requests.memory = %s", mem.String())
	}
}

func TestApplyQuotaCeilingAllowsFittingIncrease(t *testing.T) {
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "kipper-quota", Namespace: "shop-test"},
		Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
			corev1.ResourceRequestsMemory: resource.MustParse("6Gi"),
		}},
		Status: corev1.ResourceQuotaStatus{
			Used: corev1.ResourceList{corev1.ResourceRequestsMemory: resource.MustParse("1Gi")},
		},
	}
	rc := NewResourceController(fake.NewClientset(quota), nil)

	before := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
	}
	container := &corev1.Container{Name: "web", Resources: corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")},
	}}
	increase := []ResourceLogEntry{{Action: "increased memory"}}

	entries, blocked := rc.applyQuotaCeiling(context.Background(), "shop-test", "web", container, before, 1, 0, nil, increase)
	if blocked {
		t.Fatal("a fitting increase must not be blocked")
	}
	if len(entries) != 1 || entries[0].Action != "increased memory" {
		t.Fatalf("original entries must pass through, got %+v", entries)
	}
	if mem := container.Resources.Requests[corev1.ResourceMemory]; mem.String() != "2Gi" {
		t.Errorf("fitting increase must be kept, requests.memory = %s", mem.String())
	}
}

func TestApplyQuotaCeilingNoQuotaNamespace(t *testing.T) {
	rc := NewResourceController(fake.NewClientset(), nil)

	before := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
	}
	container := &corev1.Container{Name: "web", Resources: corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")},
	}}
	increase := []ResourceLogEntry{{Action: "increased memory"}}

	entries, blocked := rc.applyQuotaCeiling(context.Background(), "shop-test", "web", container, before, 1, 0, nil, increase)
	if blocked || len(entries) != 1 {
		t.Fatalf("no quota object means no enforcement, got blocked=%v entries=%+v", blocked, entries)
	}
}

// The rollout peak, not the steady state, is what quota admission prices
// first: a Deployment's surge pod is admitted at the new size while every
// old pod still counts. Steady state fitting exactly is therefore not
// enough.
func TestApplyQuotaCeilingBlocksSurgePeak(t *testing.T) {
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "kipper-quota", Namespace: "shop-test"},
		Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
			corev1.ResourceRequestsMemory: resource.MustParse("6Gi"),
		}},
		Status: corev1.ResourceQuotaStatus{
			Used: corev1.ResourceList{corev1.ResourceRequestsMemory: resource.MustParse("5Gi")},
		},
	}
	rc := NewResourceController(fake.NewClientset(quota), nil)

	before := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
	}
	container := &corev1.Container{Name: "web", Resources: corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")},
	}}
	podSpec := &corev1.PodSpec{Containers: []corev1.Container{*container}}
	increase := []ResourceLogEntry{{Action: "increased memory"}}

	// Steady state fits exactly (5Gi - 1Gi + 2Gi = 6Gi), but the surge pod
	// peaks at 5Gi + 2Gi = 7Gi > 6Gi, so admission would reject it.
	entries, blocked := rc.applyQuotaCeiling(context.Background(), "shop-test", "web", container, before, 1, 1, podSpec, increase)
	if !blocked {
		t.Fatal("expected the increase to be blocked by the rollout surge peak")
	}
	if len(entries) != 1 || entries[0].Action != "resource increase blocked by project quota" {
		t.Fatalf("expected a quota-blocked entry, got %+v", entries)
	}
	if mem := container.Resources.Requests[corev1.ResourceMemory]; mem.String() != "1Gi" {
		t.Errorf("blocked increase must be reverted, requests.memory = %s", mem.String())
	}
}

func TestDeploymentSurgePods(t *testing.T) {
	plain := &appsv1.Deployment{}
	if got := quotapkg.DeploymentSurgePods(plain, 1); got != 1 {
		t.Errorf("default strategy with 1 replica: surge = %d, want 1 (25%% rounded up)", got)
	}
	if got := quotapkg.DeploymentSurgePods(plain, 8); got != 2 {
		t.Errorf("default strategy with 8 replicas: surge = %d, want 2", got)
	}

	recreate := &appsv1.Deployment{Spec: appsv1.DeploymentSpec{
		Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
	}}
	if got := quotapkg.DeploymentSurgePods(recreate, 4); got != 0 {
		t.Errorf("recreate strategy: surge = %d, want 0", got)
	}

	surge3 := intstr.FromInt32(3)
	custom := &appsv1.Deployment{Spec: appsv1.DeploymentSpec{
		Strategy: appsv1.DeploymentStrategy{
			Type:          appsv1.RollingUpdateDeploymentStrategyType,
			RollingUpdate: &appsv1.RollingUpdateDeployment{MaxSurge: &surge3},
		},
	}}
	if got := quotapkg.DeploymentSurgePods(custom, 4); got != 3 {
		t.Errorf("explicit maxSurge=3: surge = %d, want 3", got)
	}

	// An absolute maxSurge above the replica count is real: replicas=1
	// with maxSurge=5 admits up to six pods in the first wave, and the
	// quota projection has to price all five extras.
	surge5 := intstr.FromInt32(5)
	wide := &appsv1.Deployment{Spec: appsv1.DeploymentSpec{
		Strategy: appsv1.DeploymentStrategy{
			Type:          appsv1.RollingUpdateDeploymentStrategyType,
			RollingUpdate: &appsv1.RollingUpdateDeployment{MaxSurge: &surge5},
		},
	}}
	if got := quotapkg.DeploymentSurgePods(wide, 1); got != 5 {
		t.Errorf("explicit maxSurge=5 with 1 replica: surge = %d, want 5", got)
	}

	// An explicit surge-free strategy (maxSurge=0, maxUnavailable=1) must price
	// zero surge pods, or fitting increases get spuriously blocked.
	surge0 := intstr.FromInt32(0)
	unavail1 := intstr.FromInt32(1)
	surgeFree := &appsv1.Deployment{Spec: appsv1.DeploymentSpec{
		Strategy: appsv1.DeploymentStrategy{
			Type: appsv1.RollingUpdateDeploymentStrategyType,
			RollingUpdate: &appsv1.RollingUpdateDeployment{
				MaxSurge:       &surge0,
				MaxUnavailable: &unavail1,
			},
		},
	}}
	if got := quotapkg.DeploymentSurgePods(surgeFree, 4); got != 0 {
		t.Errorf("explicit maxSurge=0: surge = %d, want 0", got)
	}
}

// TestApplyQuotaCeilingBlocksMultiReplicaPeak covers the rollout peak with more
// replicas than surge pods, where a naive "used + newPod*surge" projection
// under-counts and wrongly admits an increase that then wedges at admission.
func TestApplyQuotaCeilingBlocksMultiReplicaPeak(t *testing.T) {
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "kipper-quota", Namespace: "shop-test"},
		Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
			corev1.ResourceRequestsMemory: resource.MustParse("8500Mi"),
		}},
		Status: corev1.ResourceQuotaStatus{
			Used: corev1.ResourceList{corev1.ResourceRequestsMemory: resource.MustParse("4Gi")},
		},
	}
	rc := NewResourceController(fake.NewClientset(quota), nil)

	before := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
	}
	container := &corev1.Container{Name: "web", Resources: corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")},
	}}
	podSpec := &corev1.PodSpec{Containers: []corev1.Container{*container}}
	increase := []ResourceLogEntry{{Action: "increased memory"}}

	// 4 replicas going 1Gi -> 2Gi. Steady = 4Gi + 4*1Gi = 8Gi (fits 8500Mi),
	// but at peak the new RS is at 4x2Gi while one 1Gi old pod lingers: 9Gi,
	// which exceeds the quota and would wedge the rollout.
	entries, blocked := rc.applyQuotaCeiling(context.Background(), "shop-test", "web", container, before, 4, 1, podSpec, increase)
	if !blocked {
		t.Fatal("expected the multi-replica rollout peak to be blocked")
	}
	if len(entries) != 1 || entries[0].Action != "resource increase blocked by project quota" {
		t.Fatalf("expected a quota-blocked entry, got %+v", entries)
	}
}

// TestTuningPausedSkipsWorkload pins the bulk-operation shield: a workload
// annotated with an unexpired kipper.run/tuning-paused-until must receive no
// resource changes even when pinned at its CPU limit, while an expired or
// malformed deadline leaves tuning active.
func TestTuningPausedSkipsWorkload(t *testing.T) {
	future := time.Now().Add(30 * time.Minute).Format(time.RFC3339)
	past := time.Now().Add(-time.Minute).Format(time.RFC3339)

	tests := []struct {
		name   string
		anno   map[string]string
		paused bool
	}{
		{"no annotation", nil, false},
		{"active pause", map[string]string{"kipper.run/tuning-paused-until": future}, true},
		{"expired pause", map[string]string{"kipper.run/tuning-paused-until": past}, false},
		{"malformed timestamp", map[string]string{"kipper.run/tuning-paused-until": "tomorrow"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tuningPaused(tt.anno); got != tt.paused {
				t.Errorf("tuningPaused(%v) = %v, want %v", tt.anno, got, tt.paused)
			}
		})
	}

	// End to end: a paused StatefulSet with a pod pinned at its limit gets
	// no bump; the same StatefulSet unpaused does.
	pinned := []podMetricsEntry{{
		Namespace: "default",
		PodName:   "mongodb-0",
		CPUMillis: 100,
		Age:       20 * time.Minute,
	}}
	makeSTS := func(anno map[string]string) *appsv1.StatefulSet {
		return &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: "mongodb", Namespace: "default", Annotations: anno},
			Spec: appsv1.StatefulSetSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{Containers: []corev1.Container{*saturatedContainer()}},
				},
			},
		}
	}
	metrics := map[string][]podMetricsEntry{"default/mongodb": pinned}

	rc := NewResourceController(fake.NewSimpleClientset(), nil)
	entries := rc.processStatefulSet(context.Background(), makeSTS(map[string]string{
		"kipper.run/tuning-paused-until": future,
	}), metrics)
	if len(entries) != 0 {
		t.Fatalf("expected no entries for a paused statefulset, got %d: %v", len(entries), entries)
	}

	// Unpaused control: the sustained-saturation window applies, so backdate
	// it to prove tuning is active again.
	rc.cpuPinned["default/mongodb"] = pinnedWindow{
		since:   time.Now().Add(-statefulSetSaturationWindow),
		lastHot: time.Now().Add(-time.Minute),
	}
	entries = rc.processStatefulSet(context.Background(), makeSTS(nil), metrics)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry for the unpaused statefulset, got %d: %v", len(entries), entries)
	}
}
