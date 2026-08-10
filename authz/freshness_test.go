package main

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func canaryObject() *kipperv1.ApiKey {
	return &kipperv1.ApiKey{
		ObjectMeta: metav1.ObjectMeta{Name: "authz-canary", Namespace: "kipper-system"},
		Spec: kipperv1.ApiKeySpec{
			Plan:       "canary",
			Prefix:     "canary00",
			HashSHA256: "0000000000000000000000000000000000000000000000000000000000000000",
		},
	}
}

func canaryKey() client.ObjectKey {
	return client.ObjectKey{Namespace: "kipper-system", Name: "authz-canary"}
}

// apiKeyCanaryTargets is the single-type probe set used by the ApiKey-focused
// tests below.
func apiKeyCanaryTargets() []canaryTarget {
	return []canaryTarget{{key: canaryKey(), template: &kipperv1.ApiKey{}}}
}

// allCanaryObjects seeds one canary per request-path type.
func allCanaryObjects() []client.Object {
	return []client.Object{
		canaryObject(),
		&kipperv1.UsagePlan{
			ObjectMeta: metav1.ObjectMeta{Name: "authz-canary", Namespace: "kipper-system"},
			Spec:       kipperv1.UsagePlanSpec{Rate: 1, Burst: 1},
		},
		&kipperv1.UsageRollup{
			ObjectMeta: metav1.ObjectMeta{Name: "authz-canary", Namespace: "kipper-system"},
			Spec:       kipperv1.UsageRollupSpec{KeyPrefix: "canary00", Day: "2000-01-01"},
		},
	}
}

// allCanaryTargets probes all three request-path types.
func allCanaryTargets() []canaryTarget {
	return []canaryTarget{
		{key: canaryKey(), template: &kipperv1.ApiKey{}},
		{key: canaryKey(), template: &kipperv1.UsagePlan{}},
		{key: canaryKey(), template: &kipperv1.UsageRollup{}},
	}
}

// One fake client acting as both the direct and cached view models a healthy
// watch pipeline: the write is immediately observable.
func TestFreshness_ProbeAdvancesWhenCacheObservesWrite(t *testing.T) {
	fakeClient := crfake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(canaryObject()).Build()
	f := NewFreshness(fakeClient, fakeClient, func() bool { return true }, time.Hour, time.Hour, apiKeyCanaryTargets(), "pod-a")

	if f.Fresh() {
		t.Fatal("clock must start stale")
	}
	f.probe(context.Background())
	if !f.Fresh() {
		t.Fatal("a write observed through the cache must advance the clock")
	}
	if f.Age() < 0 {
		t.Fatal("age must be non-negative after a successful probe")
	}
}

// Two separate stores model the wedged-watch failure: the direct write
// succeeds, but the cached view never receives the event. The clock must NOT
// advance — this is exactly the state where a plain LIST probe would keep
// reporting healthy while the authorizer serves revoked keys.
func TestFreshness_ProbeStallsWhenWatchDeliversNothing(t *testing.T) {
	direct := crfake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(canaryObject()).Build()
	wedgedCache := crfake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(canaryObject()).Build()

	f := NewFreshness(direct, wedgedCacheClient{direct: direct, cache: wedgedCache}, func() bool { return true },
		time.Hour, time.Hour, apiKeyCanaryTargets(), "pod-a")

	done := make(chan struct{})
	go func() {
		f.probe(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("probe did not finish")
	}

	if f.Fresh() {
		t.Fatal("the clock must not advance when the cache never observes the write")
	}
}

// wedgedCacheClient writes to the direct store (like a real cached client,
// whose writes always go to the API server) but reads from a cache that the
// watch never updates.
type wedgedCacheClient struct {
	client.Client
	direct client.Client
	cache  client.Client
}

func (w wedgedCacheClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	return w.cache.Get(ctx, key, obj, opts...)
}

func (w wedgedCacheClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	return w.direct.Update(ctx, obj, opts...)
}

func TestFreshness_ProbePrunesStaleReplicaAnnotations(t *testing.T) {
	canary := canaryObject()
	canary.Annotations = map[string]string{
		canaryAnnotationPrefix + "dead-pod": time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339Nano),
		canaryAnnotationPrefix + "live-pod": time.Now().UTC().Format(time.RFC3339Nano),
		"unrelated":                         "stays",
	}
	fakeClient := crfake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(canary).Build()
	f := NewFreshness(fakeClient, fakeClient, func() bool { return true }, time.Hour, time.Hour, apiKeyCanaryTargets(), "pod-a")

	f.probe(context.Background())

	var stored kipperv1.ApiKey
	if err := fakeClient.Get(context.Background(), canaryKey(), &stored); err != nil {
		t.Fatal(err)
	}
	if _, ok := stored.Annotations[canaryAnnotationPrefix+"dead-pod"]; ok {
		t.Error("hour-old replica annotations must be pruned")
	}
	if _, ok := stored.Annotations[canaryAnnotationPrefix+"live-pod"]; !ok {
		t.Error("recent sibling annotations must survive")
	}
	if stored.Annotations["unrelated"] != "stays" {
		t.Error("non-probe annotations must never be touched")
	}
}

// With a healthy watch for every request-path type, all three canaries
// round-trip and the clock advances.
func TestFreshness_ProbeAdvancesWhenAllTypesObserved(t *testing.T) {
	fakeClient := crfake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(allCanaryObjects()...).Build()
	f := NewFreshness(fakeClient, fakeClient, func() bool { return true }, time.Hour, time.Hour, allCanaryTargets(), "pod-a")

	f.probe(context.Background())

	if !f.Fresh() {
		t.Fatal("all canary types observed through the cache must advance the clock")
	}
}

// A wedged watch on a single type (UsagePlan) must stall the clock even when
// the ApiKey and UsageRollup watches are healthy — a fresh replica must never
// serve stale plan or quota data.
func TestFreshness_ProbeStallsWhenOneTypeWatchWedged(t *testing.T) {
	direct := crfake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(allCanaryObjects()...).Build()
	frozen := crfake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(allCanaryObjects()...).Build()

	cached := perTypeWedgedCache{direct: direct, frozen: frozen}
	f := NewFreshness(direct, cached, func() bool { return true }, time.Hour, time.Hour, allCanaryTargets(), "pod-a")

	done := make(chan struct{})
	go func() {
		f.probe(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("probe did not finish")
	}

	if f.Fresh() {
		t.Fatal("the clock must not advance while any one type's watch is wedged")
	}
}

// perTypeWedgedCache reflects writes for every type except UsagePlan, whose
// reads come from a frozen store the probe never updates — modelling a watch
// pipeline wedged for that one GVK.
type perTypeWedgedCache struct {
	client.Client
	direct client.Client
	frozen client.Client
}

func (w perTypeWedgedCache) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*kipperv1.UsagePlan); ok {
		return w.frozen.Get(ctx, key, obj, opts...)
	}
	return w.direct.Get(ctx, key, obj, opts...)
}

func (w perTypeWedgedCache) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	return w.direct.Update(ctx, obj, opts...)
}
