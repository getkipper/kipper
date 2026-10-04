package controllers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/internal/resourcebounds"
)

func cpuAndMemory(cpu, memory string) *corev1.ResourceRequirements {
	return &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(memory)},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(memory)},
	}
}

func appRecommendation(cause string) *kipperv1.ResourceTuning {
	return &kipperv1.ResourceTuning{
		ObjectMeta: metav1.ObjectMeta{Name: resourcebounds.TuningName("App", "web"), Namespace: "default", OwnerReferences: controlledBy("App", "web", "uid-web")},
		Spec:       kipperv1.ResourceTuningSpec{Kind: "App", Name: "web"},
		Status: kipperv1.ResourceTuningStatus{
			Recommendation: kipperv1.TunedResources{CPURequest: "300m", CPULimit: "300m", MemoryRequest: "768Mi", MemoryLimit: "768Mi"},
			MemoryCause:    cause,
		},
	}
}

func assertCPU(t *testing.T, got *corev1.ResourceRequirements, want string) {
	t.Helper()
	r, l := got.Requests[corev1.ResourceCPU], got.Limits[corev1.ResourceCPU]
	if r.Cmp(resource.MustParse(want)) != 0 || l.Cmp(resource.MustParse(want)) != 0 {
		t.Fatalf("cpu = %s/%s, want %s/%s", r.String(), l.String(), want, want)
	}
}

func applyTracked(t *testing.T, cause string, tracked resourcebounds.Tracked) *corev1.ResourceRequirements {
	t.Helper()
	c := tunedClient(t, appRecommendation(cause))
	desired := cpuAndMemory("200m", "512Mi")
	applyTunedResources(context.Background(), c, tunedWorkload{
		Namespace: "default", Kind: "App", Name: "web", UID: "uid-web",
		Desired: desired, Live: cpuAndMemory("200m", "512Mi"), Replicas: 1, Tracked: tracked,
	})
	return desired
}

func TestTunedResourcesLeaveATrackedCPUAtItsLiveSize(t *testing.T) {
	got := applyTracked(t, "", resourcebounds.Tracked{CPU: true})
	assertCPU(t, got, "200m")
	assertMemory(t, got, "768Mi", "768Mi")
}

func TestTunedResourcesSkipARoutineValueForATrackedMemory(t *testing.T) {
	got := applyTracked(t, "", resourcebounds.Tracked{Memory: true})
	assertMemory(t, got, "512Mi", "512Mi")
	assertCPU(t, got, "300m")
}

func TestTunedResourcesApplyAnOOMRaiseForATrackedMemory(t *testing.T) {
	got := applyTracked(t, kipperv1.MemoryCauseOOMKill, resourcebounds.Tracked{CPU: true, Memory: true})
	assertMemory(t, got, "768Mi", "768Mi")
	assertCPU(t, got, "200m")
}

func memoryTrackingApp() *kipperv1.App {
	app := newTestApp()
	app.UID = "uid-my-app"
	app.Spec.Autoscale = &kipperv1.AppAutoscale{Enabled: true, MinReplicas: ptr.To[int32](1), MaxReplicas: ptr.To[int32](5), MemoryTarget: ptr.To[int32](80)}
	return app
}

// reconcileSettled runs the Deployment step, settles the first rollout and
// runs it again, so a recommendation has its chance to apply.
func reconcileSettled(t *testing.T, c crclient.Client, app *kipperv1.App) {
	t.Helper()
	ctx := context.Background()
	r := &AppReconciler{Client: c, Scheme: testScheme()}
	require.NoError(t, r.reconcileDeployment(ctx, app, nil, "gen-1", ""))
	settleDeployment(t, c, types.NamespacedName{Namespace: app.Namespace, Name: app.Name})
	require.NoError(t, r.reconcileDeployment(ctx, app, nil, "gen-1", ""))
}

// A recommendation stored before the policy tracked memory is filtered where
// it is applied, not only where it is made.
func TestAMemoryTrackedAppSkipsARoutineRecommendation(t *testing.T) {
	app := memoryTrackingApp()
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(app, appTuning(app, "384Mi")).Build()
	reconcileSettled(t, c, app)
	req, _ := appMemory(t, c, app)
	assert.NotEqual(t, "384Mi", req, "a routine memory value must not reach a memory-tracked app")
}

func TestAMemoryTrackedAppAppliesAnOOMRaise(t *testing.T) {
	app := memoryTrackingApp()
	rec := appTuning(app, "384Mi")
	rec.Status.MemoryCause = kipperv1.MemoryCauseOOMKill
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(app, rec).Build()
	reconcileSettled(t, c, app)
	req, lim := appMemory(t, c, app)
	assert.Equal(t, "384Mi", req)
	assert.Equal(t, "384Mi", lim)
}

// An unusable policy leaves its autoscaler running, so what that autoscaler
// reads still counts as tracked.
func TestAnAutoscalerKeptByAnUnusablePolicyStillTracks(t *testing.T) {
	app := memoryTrackingApp()
	app.Spec.Autoscale = &kipperv1.AppAutoscale{Enabled: true, MinReplicas: ptr.To[int32](6), MaxReplicas: ptr.To[int32](5), CPUTarget: ptr.To[int32](70)}
	min := int32(1)
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: app.Name, Namespace: app.Namespace, OwnerReferences: controlledBy("App", app.Name, app.UID)},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{MinReplicas: &min, MaxReplicas: 5, Metrics: []autoscalingv2.MetricSpec{{
			Type: autoscalingv2.ResourceMetricSourceType, Resource: &autoscalingv2.ResourceMetricSource{Name: corev1.ResourceMemory},
		}}},
	}
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(app, appTuning(app, "384Mi"), hpa).Build()
	reconcileSettled(t, c, app)
	req, _ := appMemory(t, c, app)
	assert.NotEqual(t, "384Mi", req, "the kept autoscaler tracks memory")
}

// The stop clears the OOM marker with the recommendation it explained, so a
// later value is not taken for an OOM raise.
func TestStop_ClearsTheOOMMarker(t *testing.T) {
	for name, rec := range map[string]kipperv1.TunedResources{
		"with its recommendation": {MemoryRequest: "1Gi", MemoryLimit: "1Gi"},
		"left on its own":         {},
	} {
		t.Run(name, func(t *testing.T) {
			app := newTestApp()
			app.UID = "uid-my-app"
			tuning := appTuning(app, "")
			tuning.Status = kipperv1.ResourceTuningStatus{Recommendation: rec, MemoryCause: kipperv1.MemoryCauseOOMKill}
			scheme := testScheme()
			c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, tuning).WithStatusSubresource(app, tuning).Build()
			h := &stopHarness{t: t, c: c, r: &AppReconciler{Client: c, Scheme: scheme},
				key: types.NamespacedName{Name: app.Name, Namespace: app.Namespace}}
			h.reconcile()

			h.edit(stop)
			h.reconcile()

			var got kipperv1.ResourceTuning
			require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: app.Namespace, Name: tuning.Name}, &got))
			assert.Equal(t, kipperv1.TunedResources{}, got.Status.Recommendation)
			assert.Empty(t, got.Status.MemoryCause)
		})
	}
}
