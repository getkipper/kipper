package controllers

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/internal/resourcebounds"
	"github.com/getkipper/kipper/controller/pkg/labels"
	"github.com/getkipper/kipper/controller/pkg/rollout"
)

func tunedClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := kipperv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func memoryPair(req, lim string) *corev1.ResourceRequirements {
	return &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(req)},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(lim)},
	}
}

// controlledBy is the owner reference the auto-sizer puts on a record.
func controlledBy(kind, name string, uid types.UID) []metav1.OwnerReference {
	controller := true
	return []metav1.OwnerReference{{APIVersion: kipperv1.GroupVersion.String(), Kind: kind, Name: name, UID: uid, Controller: &controller}}
}

func memoryRecommendation() *kipperv1.ResourceTuning {
	req, lim := "768Mi", "768Mi"
	return &kipperv1.ResourceTuning{
		ObjectMeta: metav1.ObjectMeta{Name: resourcebounds.TuningName("Service", "db"), Namespace: "default", OwnerReferences: controlledBy("Service", "db", "uid-db")},
		Spec:       kipperv1.ResourceTuningSpec{Kind: "Service", Name: "db"},
		Status:     kipperv1.ResourceTuningStatus{Recommendation: kipperv1.TunedResources{MemoryRequest: req, MemoryLimit: lim}},
	}
}

func boundedMemory(t *testing.T) resourcebounds.Spec {
	t.Helper()
	spec, err := resourcebounds.OwnedSpec("", "", "512Mi", "2Gi")
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

func assertMemory(t *testing.T, got *corev1.ResourceRequirements, req, lim string) {
	t.Helper()
	r, l := got.Requests[corev1.ResourceMemory], got.Limits[corev1.ResourceMemory]
	if r.Cmp(resource.MustParse(req)) != 0 || l.Cmp(resource.MustParse(lim)) != 0 {
		t.Fatalf("memory = %s/%s, want %s/%s", r.String(), l.String(), req, lim)
	}
}

func TestTunedResourcesApplyTheRecommendationInsideTheBounds(t *testing.T) {
	c := tunedClient(t, memoryRecommendation())
	desired := memoryPair("512Mi", "2Gi")
	applyTunedResources(context.Background(), c, tunedWorkload{Namespace: "default", Kind: "Service", Name: "db", UID: "uid-db", Spec: boundedMemory(t), Desired: desired, Live: memoryPair("512Mi", "2Gi"), Replicas: 1})
	assertMemory(t, desired, "768Mi", "2Gi")
}

// An app deleted and created again under the same name must not inherit the
// old app's recommendation while garbage collection has yet to remove it.
func TestTunedResourcesIgnoreARecordLeftByAnEarlierOwner(t *testing.T) {
	c := tunedClient(t, memoryRecommendation())
	desired := memoryPair("512Mi", "2Gi")
	applyTunedResources(context.Background(), c, tunedWorkload{Namespace: "default", Kind: "Service", Name: "db", UID: "uid-new", Spec: boundedMemory(t), Desired: desired, Live: memoryPair("600Mi", "2Gi"), Replicas: 1})
	assertMemory(t, desired, "600Mi", "2Gi")
}

func TestTunedResourcesIgnoreTheRecommendationWhilePaused(t *testing.T) {
	c := tunedClient(t, memoryRecommendation())
	desired := memoryPair("512Mi", "2Gi")
	paused := map[string]string{labels.AnnoTuningPausedUntil: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}
	applyTunedResources(context.Background(), c, tunedWorkload{Namespace: "default", Kind: "Service", Name: "db", UID: "uid-db", Spec: boundedMemory(t), Desired: desired, Live: memoryPair("600Mi", "2Gi"), LiveAnnotations: paused, Replicas: 1})
	assertMemory(t, desired, "600Mi", "2Gi")
}

func TestTunedResourcesIgnoreTheRecommendationInExpertMode(t *testing.T) {
	expert := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: resourcebounds.ModeConfigMapName, Namespace: resourcebounds.ModeConfigMapNamespace},
		Data:       map[string]string{"mode": resourcebounds.ModeExpert},
	}
	c := tunedClient(t, memoryRecommendation(), expert)
	desired := memoryPair("512Mi", "2Gi")
	applyTunedResources(context.Background(), c, tunedWorkload{Namespace: "default", Kind: "Service", Name: "db", UID: "uid-db", Spec: boundedMemory(t), Desired: desired, Live: memoryPair("600Mi", "2Gi"), Replicas: 1})
	assertMemory(t, desired, "600Mi", "2Gi")
}

func TestTunedResourcesWithoutARecordKeepTheLiveSize(t *testing.T) {
	c := tunedClient(t)
	desired := memoryPair("128Mi", "128Mi")
	applyTunedResources(context.Background(), c, tunedWorkload{Namespace: "default", Kind: "Service", Name: "db", UID: "uid-db", Spec: resourcebounds.Spec{}, Desired: desired, Live: memoryPair("384Mi", "384Mi"), Replicas: 1})
	assertMemory(t, desired, "384Mi", "384Mi")
}

// A recommendation that no longer fits the project quota waits: the live size
// stays, inside the user's bounds, instead of a rollout that admission rejects.
func TestTunedResourcesKeepTheLiveSizeWhenTheQuotaIsFull(t *testing.T) {
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: kipperv1.ProjectQuotaName, Namespace: "default"},
		Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
			corev1.ResourceLimitsMemory: resource.MustParse("2Gi"), corev1.ResourceRequestsMemory: resource.MustParse("700Mi"),
		}},
		Status: corev1.ResourceQuotaStatus{Used: corev1.ResourceList{
			corev1.ResourceLimitsMemory: resource.MustParse("2Gi"), corev1.ResourceRequestsMemory: resource.MustParse("600Mi"),
		}},
	}
	c := tunedClient(t, memoryRecommendation(), quota)
	desired := memoryPair("512Mi", "2Gi")
	applyTunedResources(context.Background(), c, tunedWorkload{
		Namespace: "default", Kind: "Service", Name: "db", UID: "uid-db", Spec: boundedMemory(t),
		Desired: desired, Live: memoryPair("600Mi", "2Gi"), Replicas: 1,
	})
	assertMemory(t, desired, "600Mi", "2Gi")
}

func TestTunedResourcesReportARequestAboveItsLimit(t *testing.T) {
	spec, err := resourcebounds.OwnedSpec("", "", "4Gi", "2Gi")
	if err != nil {
		t.Fatal(err)
	}
	desired := memoryPair("2Gi", "2Gi")
	if !applyTunedResources(context.Background(), tunedClient(t), tunedWorkload{
		Namespace: "default", Kind: "Service", Name: "db", UID: "uid-db", Spec: spec, Desired: desired, Live: memoryPair("2Gi", "2Gi"), Replicas: 1,
	}) {
		t.Fatal("a request above its limit was not reported")
	}
	assertMemory(t, desired, "2Gi", "2Gi")
}

// Quota checks must include surge pods at the recommended size while old
// pods still consume resources.
func TestTunedResourcesPriceTheRolloutSurge(t *testing.T) {
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: kipperv1.ProjectQuotaName, Namespace: "default"},
		Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
			corev1.ResourceLimitsMemory: resource.MustParse("256Mi"),
		}},
		Status: corev1.ResourceQuotaStatus{Used: corev1.ResourceList{
			corev1.ResourceLimitsMemory: resource.MustParse("128Mi"),
		}},
	}
	rec := memoryRecommendation()
	rec.Status.Recommendation = kipperv1.TunedResources{MemoryRequest: "256Mi", MemoryLimit: "256Mi"}
	c := tunedClient(t, rec, quota)
	desired := memoryPair("128Mi", "128Mi")
	podSpec := &corev1.PodSpec{Containers: []corev1.Container{{Name: "db", Resources: *memoryPair("128Mi", "128Mi")}}}
	applyTunedResources(context.Background(), c, tunedWorkload{
		Namespace: "default", Kind: "Service", Name: "db", UID: "uid-db", Spec: resourcebounds.Spec{},
		Desired: desired, Live: memoryPair("128Mi", "128Mi"), Replicas: 1, SurgePods: 1, PodSpec: podSpec,
	})
	// 128Mi old pod plus 256Mi new pod is 384Mi, over the 256Mi quota.
	assertMemory(t, desired, "128Mi", "128Mi")
}

func TestRolloutReplicasCountsWhatTheHPASet(t *testing.T) {
	two, ten := int32(2), int32(10)
	if got := rolloutReplicas(&two, &ten); got != 10 {
		t.Fatalf("rolloutReplicas = %d, want the 10 the HPA runs", got)
	}
	if got := rolloutReplicas(&two, nil); got != 2 {
		t.Fatalf("rolloutReplicas = %d, want 2 before the first rollout", got)
	}
}

func TestTunedResourcesWaitForTheRollout(t *testing.T) {
	cpuRecommendation := func(req string) *kipperv1.ResourceTuning {
		return &kipperv1.ResourceTuning{
			ObjectMeta: metav1.ObjectMeta{Name: resourcebounds.TuningName("App", "shop"), Namespace: "default", OwnerReferences: controlledBy("App", "shop", "uid-shop")},
			Spec:       kipperv1.ResourceTuningSpec{Kind: "App", Name: "shop"},
			Status:     kipperv1.ResourceTuningStatus{Recommendation: kipperv1.TunedResources{CPURequest: req}},
		}
	}
	cpuBounds := func(min, max string) resourcebounds.Spec {
		spec, err := resourcebounds.OwnedSpec(min, max, "", "")
		if err != nil {
			t.Fatal(err)
		}
		return spec
	}
	cpu := func(req string) *corev1.ResourceRequirements {
		return &corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(req)}}
	}
	tests := []struct {
		name    string
		phase   rolloutPhase
		rec     string
		bounds  resourcebounds.Spec
		live    string
		wantCPU string
	}{
		{name: "settled: the recommendation applies", phase: phaseSettled, rec: "1500m", bounds: cpuBounds("500m", "3"), live: "3", wantCPU: "1500m"},
		{name: "in flight: the live size stays", phase: phaseInFlight, rec: "1500m", bounds: cpuBounds("500m", "3"), live: "3", wantCPU: "3"},
		{name: "in flight: a user bound still applies", phase: phaseInFlight, rec: "1500m", bounds: cpuBounds("500m", "2"), live: "3", wantCPU: "2"},
		{name: "unschedulable: a decrease applies", phase: phaseUnschedulable, rec: "1500m", bounds: cpuBounds("500m", "3"), live: "3", wantCPU: "1500m"},
		{name: "unschedulable: an increase waits", phase: phaseUnschedulable, rec: "2", bounds: cpuBounds("500m", "3"), live: "1", wantCPU: "1"},
		{name: "failed: an increase applies", phase: phaseFailed, rec: "2", bounds: cpuBounds("500m", "3"), live: "1", wantCPU: "2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := tunedClient(t, cpuRecommendation(tt.rec))
			desired := cpu(tt.live)
			applyTunedResources(context.Background(), c, tunedWorkload{
				Namespace: "default", Kind: "App", Name: "shop", UID: "uid-shop", Spec: tt.bounds,
				Desired: desired, Live: cpu(tt.live), Replicas: 2, Rollout: func() rolloutPhase { return tt.phase },
			})
			got := desired.Requests[corev1.ResourceCPU]
			if got.Cmp(resource.MustParse(tt.wantCPU)) != 0 {
				t.Fatalf("cpu request = %s, want %s", got.String(), tt.wantCPU)
			}
		})
	}
}

func TestRolloutPhaseFor(t *testing.T) {
	assert.Equal(t, phaseSettled, rolloutPhase(0))
	cases := map[rollout.Reason]rolloutPhase{
		rollout.Complete:         phaseSettled,
		rollout.InProgress:       phaseInFlight,
		rollout.Unschedulable:    phaseUnschedulable,
		rollout.QuotaExceeded:    phaseUnschedulable,
		rollout.PodsRefused:      phaseFailed,
		rollout.PodsNotStarting:  phaseInFlight,
		rollout.NotBecomingReady: phaseInFlight,
		rollout.DeadlineExceeded: phaseFailed,
	}
	for reason, want := range cases {
		assert.Equal(t, want, rolloutPhaseFor(reason), "reason %q", reason)
	}
}

// Reading the rollout phase lists pods, so it happens only when there is a
// recommendation to hold back.
func TestTunedResourcesReadTheRolloutOnlyForARecommendation(t *testing.T) {
	c := tunedClient(t)
	desired := memoryPair("128Mi", "128Mi")
	applyTunedResources(context.Background(), c, tunedWorkload{
		Namespace: "default", Kind: "App", Name: "shop", UID: "uid-shop", Desired: desired, Live: memoryPair("384Mi", "384Mi"), Replicas: 1,
		Rollout: func() rolloutPhase {
			t.Fatal("the rollout was read with no recommendation to gate")
			return phaseSettled
		},
	})
}
