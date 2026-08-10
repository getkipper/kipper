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

func TestFlusher_CreatesAndIncrementsRollups(t *testing.T) {
	fakeClient := crfake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	buffer := NewUsageBuffer()
	flusher := NewFlusher(fakeClient, fakeClient, buffer, time.Minute)

	buffer.Add("shop-prod", testPrefix, "2026-07-08", CounterAllowed)
	buffer.Add("shop-prod", testPrefix, "2026-07-08", CounterAllowed)
	buffer.Add("shop-prod", testPrefix, "2026-07-08", CounterDeniedRate)
	flusher.Flush(context.Background())

	var rollup kipperv1.UsageRollup
	name := RollupName(testPrefix, "2026-07-08")
	if err := fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "shop-prod", Name: name}, &rollup); err != nil {
		t.Fatalf("rollup not created: %v", err)
	}
	if rollup.Spec.Allowed != 2 || rollup.Spec.DeniedRate != 1 {
		t.Fatalf("first flush: allowed=%d deniedRate=%d, want 2/1", rollup.Spec.Allowed, rollup.Spec.DeniedRate)
	}

	// A second batch increments the same object instead of replacing it.
	buffer.Add("shop-prod", testPrefix, "2026-07-08", CounterAllowed)
	buffer.Add("shop-prod", testPrefix, "2026-07-08", CounterDeniedQuota)
	flusher.Flush(context.Background())

	if err := fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "shop-prod", Name: name}, &rollup); err != nil {
		t.Fatal(err)
	}
	if rollup.Spec.Allowed != 3 || rollup.Spec.DeniedRate != 1 || rollup.Spec.DeniedQuota != 1 {
		t.Fatalf("second flush: %+v", rollup.Spec)
	}

	// The buffer is empty after a successful flush.
	if got := buffer.UnflushedAllowed("shop-prod", testPrefix, time.Time{}); got != 0 {
		t.Errorf("buffer must be drained after flush, got %d", got)
	}
}

func TestFlusher_MergesIntoExistingRollupFromOtherReplica(t *testing.T) {
	existing := &kipperv1.UsageRollup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      RollupName(testPrefix, "2026-07-08"),
			Namespace: "shop-prod",
		},
		Spec: kipperv1.UsageRollupSpec{KeyPrefix: testPrefix, Day: "2026-07-08", Allowed: 40},
	}
	fakeClient := crfake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()
	buffer := NewUsageBuffer()
	flusher := NewFlusher(fakeClient, fakeClient, buffer, time.Minute)

	buffer.Add("shop-prod", testPrefix, "2026-07-08", CounterAllowed)
	flusher.Flush(context.Background())

	var rollup kipperv1.UsageRollup
	if err := fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "shop-prod", Name: existing.Name}, &rollup); err != nil {
		t.Fatal(err)
	}
	if rollup.Spec.Allowed != 41 {
		t.Fatalf("allowed = %d, want 41 (another replica's 40 plus ours)", rollup.Spec.Allowed)
	}
}

func TestUsageBuffer_RestoreAfterFailedFlush(t *testing.T) {
	buffer := NewUsageBuffer()
	buffer.Add("shop-prod", testPrefix, "2026-07-08", CounterAllowed)

	counts := buffer.drain()
	if got := buffer.UnflushedAllowed("shop-prod", testPrefix, time.Time{}); got != 0 {
		t.Fatalf("drain must empty the buffer, got %d", got)
	}

	// New traffic lands while the flush is in flight and failing.
	buffer.Add("shop-prod", testPrefix, "2026-07-08", CounterAllowed)
	buffer.restore(counts)

	if got := buffer.UnflushedAllowed("shop-prod", testPrefix, time.Time{}); got != 2 {
		t.Fatalf("restored + new counts = %d, want 2 (failed samples must not be lost)", got)
	}
}

func TestRollupName(t *testing.T) {
	if got := RollupName("ab12cd34", "2026-07-08"); got != "rollup-ab12cd34-20260708" {
		t.Fatalf("RollupName = %q", got)
	}
}

// The floor bridges the gap between a successful flush and the informer
// cache observing it: without it, the drained buffer and the not-yet-visible
// rollup update would both be invisible to the quota check.
func TestUsageBuffer_PruneFloors(t *testing.T) {
	b := NewUsageBuffer()
	recent := usageKey{namespace: "shop", prefix: "abc", day: "2026-07-08"}
	old := usageKey{namespace: "shop", prefix: "abc", day: "2026-01-01"}
	b.setFloor(recent, 5)
	b.setFloor(old, 9)

	b.pruneFloors(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))

	got := b.AllowedFloors("shop", "abc", time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	if _, ok := got["2026-01-01"]; ok {
		t.Error("floor older than the cutoff should have been pruned")
	}
	if got["2026-07-08"] != 5 {
		t.Errorf("recent floor should survive, got %v", got)
	}
}

func TestQuota_FloorsPreventUnderCountAfterFlush(t *testing.T) {
	// The authorizer reads from cachedView; the flusher writes to
	// directStore. The cache never observes the flush, modelling the lag.
	directStore := crfake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	cachedView := crfake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(testApiKey(), testPlan(func(p *kipperv1.UsagePlan) {
			p.Spec.Quota = &kipperv1.PlanQuota{Requests: 3, Period: "day"}
		})).
		WithIndex(&kipperv1.ApiKey{}, keyPrefixField,
			func(o client.Object) []string { return []string{o.(*kipperv1.ApiKey).Spec.Prefix} }).
		WithIndex(&kipperv1.UsageRollup{}, rollupKeyField,
			func(o client.Object) []string { return []string{o.(*kipperv1.UsageRollup).Spec.KeyPrefix} }).
		Build()

	buffer := NewUsageBuffer()
	flusher := NewFlusher(directStore, directStore, buffer, time.Minute)
	authorizer := NewAuthorizer(cachedView, alwaysFresh(), buffer)

	// Two allowed requests, then a flush the cache never sees.
	if d := authorizer.Authorize(context.Background(), "shop-prod", "api", testKey).Decision; d != DecisionAllow {
		t.Fatalf("first request: %v", d)
	}
	if d := authorizer.Authorize(context.Background(), "shop-prod", "api", testKey).Decision; d != DecisionAllow {
		t.Fatalf("second request: %v", d)
	}
	flusher.Flush(context.Background())

	// Quota is 3: exactly one more request may pass. Without floors the
	// flushed 2 would vanish and two more would be admitted.
	if d := authorizer.Authorize(context.Background(), "shop-prod", "api", testKey).Decision; d != DecisionAllow {
		t.Fatalf("third request should still fit the quota: %v", d)
	}
	if d := authorizer.Authorize(context.Background(), "shop-prod", "api", testKey).Decision; d != DecisionDenyQuota {
		t.Fatalf("fourth request must be denied despite the cache lag, got %v", d)
	}
}
