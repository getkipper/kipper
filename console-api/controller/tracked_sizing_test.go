package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/internal/resourcebounds"
)

// trackingApp is an automatically sized app whose usable autoscaling policy
// tracks the metrics with a positive target.
func trackingApp(cpuTarget, memoryTarget int32) *kipperv1.App {
	app := automaticApp()
	app.Spec.Autoscale = &kipperv1.AppAutoscale{
		Enabled: true, MinReplicas: ptr.To[int32](1), MaxReplicas: ptr.To[int32](5), CPUTarget: ptr.To[int32](cpuTarget), MemoryTarget: ptr.To[int32](memoryTarget),
	}
	return app
}

func TestACPUTrackedAppGetsNoCPUChange(t *testing.T) {
	crClient := tuningCRClient(trackingApp(70, 0))
	deploy := ownedDeployment("App", memoryContainer("512Mi", "512Mi"))
	rc := NewResourceController(fake.NewClientset(deploy), crClient)

	for tick := 0; tick < 3; tick++ {
		entries := rc.processDeployment(context.Background(), deploy.DeepCopy(), busy(180, "300Mi"), rc.appSizingRules(context.Background()))
		if hasAction(entries, "CPU") {
			t.Fatalf("tick %d: a CPU change for an app whose autoscaler tracks CPU: %v", tick, entries)
		}
	}
	rec := tuningOf(t, crClient, "App").Status.Recommendation
	if rec.CPURequest != "" || rec.CPULimit != "" {
		t.Fatalf("cpu recommendation = %s/%s, want none", rec.CPURequest, rec.CPULimit)
	}
}

// Leave CPU pressure to the autoscaler while continuing to size memory.
func TestACPUTrackedAppSkipsTheSaturationBump(t *testing.T) {
	crClient := tuningCRClient(trackingApp(70, 0))
	deploy := ownedDeployment("App", memoryContainer("512Mi", "512Mi"))
	rc := NewResourceController(fake.NewClientset(deploy), crClient)

	entries := rc.processDeployment(context.Background(), deploy.DeepCopy(), busy(199, "450Mi"), rc.appSizingRules(context.Background()))
	if hasAction(entries, "CPU") {
		t.Fatalf("the saturation bump fired for a CPU-tracked app: %v", entries)
	}
	if !hasAction(entries, "increased memory") {
		t.Fatalf("memory is not tracked, so memory at 88%% should still be raised: %v", entries)
	}
	if rec := tuningOf(t, crClient, "App").Status.Recommendation; rec.CPURequest != "" {
		t.Fatalf("cpu recommendation = %s, want none", rec.CPURequest)
	}
}

func TestACPUTrackedAppDropsAStoredCPURecommendation(t *testing.T) {
	crClient := tuningCRClient(trackingApp(70, 0), storedTuning(kipperv1.ResourceTuningStatus{
		Recommendation: kipperv1.TunedResources{CPURequest: "300m", CPULimit: "300m"},
	}))
	deploy := ownedDeployment("App", memoryContainer("512Mi", "512Mi"))
	rc := NewResourceController(fake.NewClientset(deploy), crClient)

	rc.processDeployment(context.Background(), deploy.DeepCopy(), usage(150, "300Mi"), rc.appSizingRules(context.Background()))
	if rec := tuningOf(t, crClient, "App").Status.Recommendation; rec.CPURequest != "" || rec.CPULimit != "" {
		t.Fatalf("cpu recommendation = %s/%s, want the stored one dropped", rec.CPURequest, rec.CPULimit)
	}
}

func TestAMemoryTrackedAppGetsNoRoutineMemoryIncrease(t *testing.T) {
	crClient := tuningCRClient(trackingApp(0, 80))
	deploy := ownedDeployment("App", memoryContainer("512Mi", "512Mi"))
	rc := NewResourceController(fake.NewClientset(deploy), crClient)

	entries := rc.processDeployment(context.Background(), deploy.DeepCopy(), busy(180, "450Mi"), rc.appSizingRules(context.Background()))
	if hasAction(entries, "memory") {
		t.Fatalf("a routine memory change for an app whose autoscaler tracks memory: %v", entries)
	}
	if !hasAction(entries, "increased CPU") {
		t.Fatalf("CPU is not tracked, so a busy CPU should still be raised: %v", entries)
	}
	rec := tuningOf(t, crClient, "App").Status
	if rec.Recommendation.MemoryRequest != "" || rec.MemoryCause != "" {
		t.Fatalf("memory recommendation = %q (cause %q), want none", rec.Recommendation.MemoryRequest, rec.MemoryCause)
	}
}

func TestAMemoryTrackedAppGetsNoRoutineMemoryDecrease(t *testing.T) {
	crClient := tuningCRClient(trackingApp(0, 80))
	deploy := ownedDeployment("App", memoryContainer("512Mi", "512Mi"))
	rc := NewResourceController(fake.NewClientset(deploy), crClient)

	for tick := 0; tick < 4; tick++ {
		entries := rc.processDeployment(context.Background(), deploy.DeepCopy(), usage(150, "20Mi"), rc.appSizingRules(context.Background()))
		if hasAction(entries, "memory") {
			t.Fatalf("tick %d: a routine memory decrease for a memory-tracked app: %v", tick, entries)
		}
	}
}

// Adding pods does not save a pod that runs out of memory, so the OOM
// doubling still applies to a memory-tracked app, and the record says why.
func TestAMemoryTrackedAppStillDoublesAfterAnOOM(t *testing.T) {
	crClient := tuningCRClient(trackingApp(0, 80))
	deploy := ownedDeployment("App", memoryContainer("128Mi", "128Mi"))
	rc := NewResourceController(fake.NewClientset(deploy), crClient)

	entries := rc.processDeployment(context.Background(), deploy.DeepCopy(), oomUsage(time.Now().Add(-time.Minute)), rc.appSizingRules(context.Background()))
	if !hasAction(entries, "doubled memory") {
		t.Fatalf("expected the OOM to double memory, got %v", entries)
	}
	st := tuningOf(t, crClient, "App").Status
	if st.Recommendation.MemoryRequest != "256Mi" || st.MemoryCause != kipperv1.MemoryCauseOOMKill {
		t.Fatalf("memory recommendation = %s (cause %q), want 256Mi marked OOMKill", st.Recommendation.MemoryRequest, st.MemoryCause)
	}
}

// The OOM raise stays recommended, with its marker, while it waits: across a
// tick that quota blocks and across a console-api restart.
func TestTheOOMMarkerStaysWhileTheRaiseIsPending(t *testing.T) {
	crClient := tuningCRClient(trackingApp(0, 80))
	deploy := ownedDeployment("App", memoryContainer("128Mi", "128Mi"))
	// Two replicas at 200m use 400m; a CPU increase does not fit, the memory
	// raise does.
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: kipperv1.ProjectQuotaName, Namespace: "default"},
		Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
			corev1.ResourceLimitsCPU: resource.MustParse("600m"), corev1.ResourceRequestsCPU: resource.MustParse("600m"),
		}},
		Status: corev1.ResourceQuotaStatus{Used: corev1.ResourceList{
			corev1.ResourceLimitsCPU: resource.MustParse("400m"), corev1.ResourceRequestsCPU: resource.MustParse("400m"),
		}},
	}
	client := fake.NewClientset(deploy, quota)
	rc := NewResourceController(client, crClient)

	rc.processDeployment(context.Background(), deploy.DeepCopy(), oomUsage(time.Now().Add(-time.Minute)), rc.appSizingRules(context.Background()))
	assertPendingOOMRaise(t, tuningOf(t, crClient, "App").Status, "after the OOM")

	// Not applied yet. The pod pins its CPU limit, so the tick proposes a CPU
	// increase, which the CPU quota blocks.
	rc.processDeployment(context.Background(), deploy.DeepCopy(), busy(199, "100Mi"), rc.appSizingRules(context.Background()))
	assertPendingOOMRaise(t, tuningOf(t, crClient, "App").Status, "after the blocked tick")

	restarted := NewResourceController(client, crClient)
	restarted.processDeployment(context.Background(), deploy.DeepCopy(), usage(150, "100Mi"), restarted.appSizingRules(context.Background()))
	assertPendingOOMRaise(t, tuningOf(t, crClient, "App").Status, "after a restart")
}

func assertPendingOOMRaise(t *testing.T, st kipperv1.ResourceTuningStatus, when string) {
	t.Helper()
	if st.Recommendation.MemoryRequest != "256Mi" || st.MemoryCause != kipperv1.MemoryCauseOOMKill {
		t.Fatalf("%s: memory recommendation = %s (cause %q), want the pending 256Mi marked OOMKill",
			when, st.Recommendation.MemoryRequest, st.MemoryCause)
	}
}

// A routine memory value that replaces the OOM raise drops the marker, so the
// value is not mistaken for an OOM raise if memory is tracked later.
func TestARoutineValueClearsTheOOMMarker(t *testing.T) {
	crClient := tuningCRClient(automaticApp())
	deploy := ownedDeployment("App", memoryContainer("128Mi", "128Mi"))
	rc := NewResourceController(fake.NewClientset(deploy), crClient)

	rc.processDeployment(context.Background(), deploy.DeepCopy(), oomUsage(time.Now().Add(-time.Minute)), nil)
	if cause := tuningOf(t, crClient, "App").Status.MemoryCause; cause != kipperv1.MemoryCauseOOMKill {
		t.Fatalf("after the OOM: memory cause = %q, want OOMKill", cause)
	}

	// The reconciler applied 256Mi; the pod now uses most of it.
	resized := ownedDeployment("App", memoryContainer("256Mi", "256Mi"))
	entries := rc.processDeployment(context.Background(), resized.DeepCopy(), usage(150, "240Mi"), nil)
	if !hasAction(entries, "increased memory") {
		t.Fatalf("expected a routine memory increase, got %v", entries)
	}
	st := tuningOf(t, crClient, "App").Status
	if st.Recommendation.MemoryRequest != "384Mi" || st.MemoryCause != "" {
		t.Fatalf("memory recommendation = %s (cause %q), want 384Mi with no cause", st.Recommendation.MemoryRequest, st.MemoryCause)
	}
}

// Once the OOM raise is applied, clear its marker and keep the raised size
// stable for memory tracking.
func TestTheOOMMarkerClearsOnceTheRaiseIsApplied(t *testing.T) {
	crClient := tuningCRClient(trackingApp(0, 80))
	deploy := ownedDeployment("App", memoryContainer("128Mi", "128Mi"))
	rc := NewResourceController(fake.NewClientset(deploy), crClient)

	rc.processDeployment(context.Background(), deploy.DeepCopy(), oomUsage(time.Now().Add(-time.Minute)), rc.appSizingRules(context.Background()))
	assertPendingOOMRaise(t, tuningOf(t, crClient, "App").Status, "after the OOM")

	// The reconciler applied 256Mi.
	resized := ownedDeployment("App", memoryContainer("256Mi", "256Mi"))
	entries := rc.processDeployment(context.Background(), resized.DeepCopy(), usage(150, "100Mi"), rc.appSizingRules(context.Background()))
	if hasAction(entries, "memory") {
		t.Fatalf("a memory change after the raise was applied: %v", entries)
	}
	st := tuningOf(t, crClient, "App").Status
	if st.MemoryCause != "" || st.Recommendation.MemoryRequest != "" {
		t.Fatalf("memory recommendation = %s (cause %q), want the applied raise cleared", st.Recommendation.MemoryRequest, st.MemoryCause)
	}
}

// A record written before the marker existed counts as routine, so a
// memory-tracked app does not carry its memory value forward.
func TestAnUnmarkedRecordIsSkippedForAMemoryTrackedApp(t *testing.T) {
	crClient := tuningCRClient(trackingApp(0, 80), storedTuning(kipperv1.ResourceTuningStatus{
		Recommendation: kipperv1.TunedResources{MemoryRequest: "1Gi", MemoryLimit: "1Gi"},
	}))
	deploy := ownedDeployment("App", memoryContainer("512Mi", "512Mi"))
	rc := NewResourceController(fake.NewClientset(deploy), crClient)

	rc.processDeployment(context.Background(), deploy.DeepCopy(), usage(150, "300Mi"), rc.appSizingRules(context.Background()))
	if rec := tuningOf(t, crClient, "App").Status.Recommendation; rec.MemoryRequest != "" || rec.MemoryLimit != "" {
		t.Fatalf("memory recommendation = %s/%s, want the unmarked one dropped", rec.MemoryRequest, rec.MemoryLimit)
	}
}

func TestAMemoryTrackedOOMRaiseStopsAtTheOOMCap(t *testing.T) {
	crClient := tuningCRClient(trackingApp(0, 80))
	deploy := ownedDeployment("App", memoryContainer("128Mi", "128Mi"))
	rc := NewResourceController(fake.NewClientset(deploy), crClient)
	rc.oomCapBytes = 192 * 1024 * 1024

	rc.processDeployment(context.Background(), deploy.DeepCopy(), oomUsage(time.Now().Add(-time.Minute)), rc.appSizingRules(context.Background()))
	st := tuningOf(t, crClient, "App").Status
	if st.Recommendation.MemoryRequest != "192Mi" || st.MemoryCause != kipperv1.MemoryCauseOOMKill {
		t.Fatalf("memory recommendation = %s (cause %q), want the 192Mi cap marked OOMKill", st.Recommendation.MemoryRequest, st.MemoryCause)
	}
}

// The user's memory bounds still stop an OOM raise for a memory-tracked app:
// a kill at the user's limit is reported, not raised past it.
func TestAMemoryTrackedOOMRaiseStopsAtTheUsersBounds(t *testing.T) {
	app := trackingApp(0, 80)
	app.Spec.Resources = kipperv1.AppResources{MemoryRequest: "128Mi", MemoryLimit: "192Mi"}
	app.ManagedFields = []metav1.ManagedFieldsEntry{{
		Manager: "kubectl", Operation: metav1.ManagedFieldsOperationApply, APIVersion: kipperv1.GroupVersion.String(), FieldsType: "FieldsV1",
		FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:spec":{"f:resources":{"f:memoryRequest":{},"f:memoryLimit":{}}}}`)},
	}}
	crClient := tuningCRClient(app)
	deploy := ownedDeployment("App", memoryContainer("128Mi", "192Mi"))
	rc := NewResourceController(fake.NewClientset(deploy), crClient)

	entries := rc.processDeployment(context.Background(), deploy.DeepCopy(), oomUsage(time.Now().Add(-time.Minute)), rc.appSizingRules(context.Background()))
	if !hasAction(entries, "OOMKilled at your limit") || hasAction(entries, "doubled memory") {
		t.Fatalf("entries = %v, want the OOM at the user's limit reported and nothing raised", entries)
	}
	st := tuningOf(t, crClient, "App").Status
	if limit, err := resource.ParseQuantity(st.Recommendation.MemoryLimit); err == nil && limit.Cmp(resource.MustParse("192Mi")) > 0 {
		t.Fatalf("memory limit recommendation = %s, above the user's 192Mi", st.Recommendation.MemoryLimit)
	}
	if st.MemoryCause != "" {
		t.Fatalf("memory cause = %q, want none without a raise", st.MemoryCause)
	}
}

// A quota-blocked OOM raise stores nothing and stays unhandled, so a
// memory-tracked app gets it, marked, once the quota allows it.
func TestAQuotaBlockedOOMRaiseForAMemoryTrackedAppIsRetried(t *testing.T) {
	crClient := tuningCRClient(trackingApp(0, 80))
	deploy := ownedDeployment("App", memoryContainer("128Mi", "128Mi"))
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: kipperv1.ProjectQuotaName, Namespace: "default"},
		Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
			corev1.ResourceLimitsMemory: resource.MustParse("300Mi"), corev1.ResourceRequestsMemory: resource.MustParse("300Mi"),
		}},
		Status: corev1.ResourceQuotaStatus{Used: corev1.ResourceList{
			corev1.ResourceLimitsMemory: resource.MustParse("256Mi"), corev1.ResourceRequestsMemory: resource.MustParse("256Mi"),
		}},
	}
	client := fake.NewClientset(deploy, quota)
	rc := NewResourceController(client, crClient)

	oomAt := time.Now().Add(-time.Minute)
	rc.processDeployment(context.Background(), deploy.DeepCopy(), oomUsage(oomAt), rc.appSizingRules(context.Background()))
	if st := tuningOf(t, crClient, "App").Status; st.Recommendation.MemoryRequest != "" || st.MemoryCause != "" {
		t.Fatalf("a quota-blocked raise stored %s (cause %q)", st.Recommendation.MemoryRequest, st.MemoryCause)
	}

	quota.Spec.Hard = corev1.ResourceList{
		corev1.ResourceLimitsMemory: resource.MustParse("4Gi"), corev1.ResourceRequestsMemory: resource.MustParse("4Gi"),
	}
	if _, err := client.CoreV1().ResourceQuotas("default").Update(context.Background(), quota, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	rc.processDeployment(context.Background(), deploy.DeepCopy(), oomUsage(oomAt), rc.appSizingRules(context.Background()))
	assertPendingOOMRaise(t, tuningOf(t, crClient, "App").Status, "once the quota allows it")
}

// The scaled-out block on decreases still holds for a metric the autoscaler
// does not track.
func TestAScaledOutAppKeepsItsUntrackedMemory(t *testing.T) {
	crClient := tuningCRClient(trackingApp(70, 0))
	deploy := ownedDeployment("App", memoryContainer("512Mi", "512Mi"))
	min := int32(1)
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: workloadName, Namespace: "default"},
		Spec:       autoscalingv2.HorizontalPodAutoscalerSpec{MinReplicas: &min, MaxReplicas: 5},
		Status:     autoscalingv2.HorizontalPodAutoscalerStatus{CurrentReplicas: 3},
	}
	rc := NewResourceController(fake.NewClientset(deploy, hpa), crClient)

	for tick := 0; tick < 4; tick++ {
		entries := rc.processDeployment(context.Background(), deploy.DeepCopy(), usage(150, "20Mi"), rc.appSizingRules(context.Background()))
		if hasAction(entries, "decreased memory") {
			t.Fatalf("tick %d: a scaled-out app had its memory decreased: %v", tick, entries)
		}
	}
}

func TestAppSizingRules(t *testing.T) {
	min := int32(1)
	hpaFor := func(name string, current int32, metrics ...corev1.ResourceName) *autoscalingv2.HorizontalPodAutoscaler {
		hpa := &autoscalingv2.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec:       autoscalingv2.HorizontalPodAutoscalerSpec{MinReplicas: &min, MaxReplicas: 5},
			Status:     autoscalingv2.HorizontalPodAutoscalerStatus{CurrentReplicas: current},
		}
		for _, m := range metrics {
			hpa.Spec.Metrics = append(hpa.Spec.Metrics, autoscalingv2.MetricSpec{
				Type: autoscalingv2.ResourceMetricSourceType, Resource: &autoscalingv2.ResourceMetricSource{Name: m},
			})
		}
		return hpa
	}
	app := func(name string, as *kipperv1.AppAutoscale) *kipperv1.App {
		return &kipperv1.App{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID("uid-" + name)}, Spec: kipperv1.AppSpec{Autoscale: as}}
	}
	apps := []*kipperv1.App{
		app("cpu", &kipperv1.AppAutoscale{Enabled: true, MinReplicas: ptr.To[int32](1), MaxReplicas: ptr.To[int32](5), CPUTarget: ptr.To[int32](70)}),
		app("memory", &kipperv1.AppAutoscale{Enabled: true, MinReplicas: ptr.To[int32](1), MaxReplicas: ptr.To[int32](5), MemoryTarget: ptr.To[int32](80)}),
		app("nohpa", &kipperv1.AppAutoscale{Enabled: true, MinReplicas: ptr.To[int32](1), MaxReplicas: ptr.To[int32](5), CPUTarget: ptr.To[int32](70)}),
		app("invalid", &kipperv1.AppAutoscale{Enabled: true, MinReplicas: ptr.To[int32](6), MaxReplicas: ptr.To[int32](5), CPUTarget: ptr.To[int32](70)}),
		app("off", &kipperv1.AppAutoscale{MinReplicas: ptr.To[int32](1), MaxReplicas: ptr.To[int32](5), CPUTarget: ptr.To[int32](70)}),
	}
	hpas := []runtime.Object{
		hpaFor("cpu", 1, corev1.ResourceCPU),
		hpaFor("memory", 3, corev1.ResourceMemory),
		hpaFor("invalid", 1, corev1.ResourceMemory),
		hpaFor("off", 3, corev1.ResourceCPU),
	}
	objs := make([]crclient.Object, 0, len(apps))
	for _, a := range apps {
		objs = append(objs, a)
	}

	t.Run("from the policy and the autoscaler", func(t *testing.T) {
		rc := NewResourceController(fake.NewClientset(hpas...), testCRClient(objs...))
		got := rc.appSizingRules(context.Background())
		want := map[string]appSizing{
			"default/cpu":     {Tracked: resourcebounds.Tracked{CPU: true}},
			"default/memory":  {ScaledOut: true, Tracked: resourcebounds.Tracked{Memory: true}},
			"default/nohpa":   {Tracked: resourcebounds.Tracked{CPU: true}},
			"default/invalid": {Tracked: resourcebounds.Tracked{Memory: true}},
		}
		assertSizingRules(t, got, want)
	})

	// Without the autoscaler list, a usable policy still says what it tracks;
	// an unusable one may have kept an autoscaler reading either metric.
	t.Run("autoscalers unreadable", func(t *testing.T) {
		client := fake.NewClientset(hpas...)
		client.PrependReactor("list", "horizontalpodautoscalers", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("the API server did not answer")
		})
		rc := NewResourceController(client, testCRClient(objs...))
		got := rc.appSizingRules(context.Background())
		want := map[string]appSizing{
			"default/cpu":     {Tracked: resourcebounds.Tracked{CPU: true}},
			"default/memory":  {Tracked: resourcebounds.Tracked{Memory: true}},
			"default/nohpa":   {Tracked: resourcebounds.Tracked{CPU: true}},
			"default/invalid": {Tracked: resourcebounds.Tracked{CPU: true, Memory: true}},
		}
		assertSizingRules(t, got, want)
	})
}

func assertSizingRules(t *testing.T, got, want map[string]appSizing) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("rules = %+v, want %+v", got, want)
	}
	for key, w := range want {
		if got[key] != w {
			t.Errorf("%s: rule = %+v, want %+v", key, got[key], w)
		}
	}
}

// storedTuning is the web app's tuning record holding the given status.
func storedTuning(status kipperv1.ResourceTuningStatus) *kipperv1.ResourceTuning {
	controller := true
	return &kipperv1.ResourceTuning{
		ObjectMeta: metav1.ObjectMeta{
			Name: resourcebounds.TuningName("App", workloadName), Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: kipperv1.GroupVersion.String(), Kind: "App", Name: workloadName,
				UID: types.UID("uid-" + workloadName), Controller: &controller,
			}},
		},
		Spec:   kipperv1.ResourceTuningSpec{Kind: "App", Name: workloadName},
		Status: status,
	}
}

func TestStatusEqualComparesTheMemoryCause(t *testing.T) {
	rec := kipperv1.TunedResources{MemoryRequest: "256Mi", MemoryLimit: "256Mi"}
	routine := &kipperv1.ResourceTuningStatus{Recommendation: rec}
	raised := &kipperv1.ResourceTuningStatus{Recommendation: rec, MemoryCause: kipperv1.MemoryCauseOOMKill}
	if statusEqual(routine, raised) {
		t.Fatal("statuses that differ only in the memory cause compare equal")
	}
}

// A routine memory value that a memory-tracked app will never get is left out
// of the quota check, so it cannot block a change the app can have.
func TestADroppedMemoryValueDoesNotBlockACPUChange(t *testing.T) {
	crClient := tuningCRClient(trackingApp(0, 80), storedTuning(kipperv1.ResourceTuningStatus{
		Recommendation: kipperv1.TunedResources{MemoryRequest: "4Gi", MemoryLimit: "4Gi"},
	}))
	deploy := ownedDeployment("App", memoryContainer("512Mi", "512Mi"))
	// Two replicas at 512Mi; 4Gi pods would not fit.
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: kipperv1.ProjectQuotaName, Namespace: "default"},
		Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
			corev1.ResourceLimitsMemory: resource.MustParse("2Gi"), corev1.ResourceRequestsMemory: resource.MustParse("2Gi"),
		}},
		Status: corev1.ResourceQuotaStatus{Used: corev1.ResourceList{
			corev1.ResourceLimitsMemory: resource.MustParse("1Gi"), corev1.ResourceRequestsMemory: resource.MustParse("1Gi"),
		}},
	}
	rc := NewResourceController(fake.NewClientset(deploy, quota), crClient)

	entries := rc.processDeployment(context.Background(), deploy.DeepCopy(), busy(180, "300Mi"), rc.appSizingRules(context.Background()))
	if !hasAction(entries, "increased CPU") {
		t.Fatalf("expected the CPU increase to go ahead, got %v", entries)
	}
	if rec := tuningOf(t, crClient, "App").Status.Recommendation; rec.CPURequest != "300m" || rec.MemoryRequest != "" {
		t.Fatalf("recommendation = %+v, want 300m CPU and no memory value", rec)
	}
}
