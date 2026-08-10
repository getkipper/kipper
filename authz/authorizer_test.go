package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

const (
	testKey    = "kip_ab12cd34_supersecretrandomvalue0000000000000000000"
	testPrefix = "ab12cd34"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := kipperv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func hashOf(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func testApiKey(mutate ...func(*kipperv1.ApiKey)) *kipperv1.ApiKey {
	key := &kipperv1.ApiKey{
		ObjectMeta: metav1.ObjectMeta{Name: "partner-key", Namespace: "shop-prod"},
		Spec: kipperv1.ApiKeySpec{
			Plan:       "bronze",
			Prefix:     testPrefix,
			HashSHA256: hashOf(testKey),
			Apps:       []string{"api"},
		},
	}
	for _, m := range mutate {
		m(key)
	}
	return key
}

func testPlan(mutate ...func(*kipperv1.UsagePlan)) *kipperv1.UsagePlan {
	plan := &kipperv1.UsagePlan{
		ObjectMeta: metav1.ObjectMeta{Name: "bronze", Namespace: "shop-prod"},
		Spec:       kipperv1.UsagePlanSpec{Rate: 100, Burst: 200},
	}
	for _, m := range mutate {
		m(plan)
	}
	return plan
}

// alwaysFresh is a freshness clock pinned to healthy for request-path tests.
func alwaysFresh() *Freshness {
	f := NewFreshness(nil, nil, func() bool { return true }, time.Hour, time.Hour, nil, "test")
	f.lastFresh = time.Now()
	return f
}

func testAuthorizer(t *testing.T, fresh *Freshness, objs ...client.Object) *Authorizer {
	t.Helper()
	fakeClient := crfake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(objs...).
		WithIndex(&kipperv1.ApiKey{}, keyPrefixField,
			func(o client.Object) []string { return []string{o.(*kipperv1.ApiKey).Spec.Prefix} }).
		WithIndex(&kipperv1.UsageRollup{}, rollupKeyField,
			func(o client.Object) []string { return []string{o.(*kipperv1.UsageRollup).Spec.KeyPrefix} }).
		Build()
	return NewAuthorizer(fakeClient, fresh, NewUsageBuffer())
}

func TestAuthorize_Allow(t *testing.T) {
	a := testAuthorizer(t, alwaysFresh(), testApiKey(), testPlan())

	if d := a.Authorize(context.Background(), "shop-prod", "api", testKey).Decision; d != DecisionAllow {
		t.Fatalf("valid key: decision = %v, want allow", d)
	}
	if got := a.usage.UnflushedAllowed("shop-prod", testPrefix, time.Time{}); got != 1 {
		t.Errorf("allowed request must be counted, got %d", got)
	}
}

func TestAuthorize_DeniesBadKeys(t *testing.T) {
	a := testAuthorizer(t, alwaysFresh(), testApiKey(), testPlan())

	cases := map[string]string{ //nolint:gosec // deliberately wrong test fixtures, not credentials
		"empty key":      "",
		"malformed":      "not-a-key",
		"unknown prefix": "kip_zzzzzzzz_supersecretrandomvalue0000000000000000000",
		"wrong secret":   "kip_ab12cd34_wrongsecret",
		"missing secret": "kip_ab12cd34_",
	}
	for name, raw := range cases {
		if d := a.Authorize(context.Background(), "shop-prod", "api", raw).Decision; d != DecisionDenyKey {
			t.Errorf("%s: decision = %v, want deny_key", name, d)
		}
	}
}

func TestAuthorize_DeniesDisabledKey(t *testing.T) {
	off := false
	a := testAuthorizer(t, alwaysFresh(),
		testApiKey(func(k *kipperv1.ApiKey) { k.Spec.Enabled = &off }), testPlan())

	if d := a.Authorize(context.Background(), "shop-prod", "api", testKey).Decision; d != DecisionDenyKey {
		t.Fatalf("disabled key: decision = %v, want deny_key", d)
	}
}

func TestAuthorize_DeniesExpiredKey(t *testing.T) {
	a := testAuthorizer(t, alwaysFresh(),
		testApiKey(func(k *kipperv1.ApiKey) {
			k.Spec.ExpiresAt = &metav1.Time{Time: time.Now().Add(-time.Hour)}
		}), testPlan())

	if d := a.Authorize(context.Background(), "shop-prod", "api", testKey).Decision; d != DecisionDenyKey {
		t.Fatalf("expired key: decision = %v, want deny_key", d)
	}
}

func TestAuthorize_AllowsKeyBeforeExpiry(t *testing.T) {
	a := testAuthorizer(t, alwaysFresh(),
		testApiKey(func(k *kipperv1.ApiKey) {
			k.Spec.ExpiresAt = &metav1.Time{Time: time.Now().Add(time.Hour)}
		}), testPlan())

	if d := a.Authorize(context.Background(), "shop-prod", "api", testKey).Decision; d != DecisionAllow {
		t.Fatalf("key before expiry: decision = %v, want allow", d)
	}
}

func TestAuthorize_DeniesOutOfScopeApp(t *testing.T) {
	a := testAuthorizer(t, alwaysFresh(), testApiKey(), testPlan())

	if d := a.Authorize(context.Background(), "shop-prod", "admin-panel", testKey).Decision; d != DecisionDenyKey {
		t.Fatalf("out-of-scope app: decision = %v, want deny_key", d)
	}
}

func TestAuthorize_EmptyScopeCoversAllApps(t *testing.T) {
	a := testAuthorizer(t, alwaysFresh(),
		testApiKey(func(k *kipperv1.ApiKey) { k.Spec.Apps = nil }), testPlan())

	if d := a.Authorize(context.Background(), "shop-prod", "anything", testKey).Decision; d != DecisionAllow {
		t.Fatalf("empty scope: decision = %v, want allow", d)
	}
}

func TestAuthorize_DeniesKeyFromAnotherNamespace(t *testing.T) {
	a := testAuthorizer(t, alwaysFresh(), testApiKey(), testPlan())

	if d := a.Authorize(context.Background(), "other-ns", "api", testKey).Decision; d != DecisionDenyKey {
		t.Fatalf("cross-namespace key: decision = %v, want deny_key", d)
	}
}

func TestAuthorize_DeniesWhenPlanMissing(t *testing.T) {
	a := testAuthorizer(t, alwaysFresh(), testApiKey()) // no plan object

	if d := a.Authorize(context.Background(), "shop-prod", "api", testKey).Decision; d != DecisionDenyKey {
		t.Fatalf("missing plan: decision = %v, want deny_key (unmetered fail-open is not an option)", d)
	}
}

func TestAuthorize_OverRate(t *testing.T) {
	a := testAuthorizer(t, alwaysFresh(), testApiKey(),
		testPlan(func(p *kipperv1.UsagePlan) { p.Spec.Rate = 1; p.Spec.Burst = 2 }))

	decisions := make(map[Decision]int)
	for i := 0; i < 5; i++ {
		decisions[a.Authorize(context.Background(), "shop-prod", "api", testKey).Decision]++
	}
	if decisions[DecisionAllow] < 2 {
		t.Errorf("burst of 2 should admit at least 2, got %d", decisions[DecisionAllow])
	}
	if decisions[DecisionDenyRate] == 0 {
		t.Error("draining the bucket must produce deny_rate decisions")
	}
	if got := a.usage.counts[usageKey{"shop-prod", testPrefix, time.Now().UTC().Format("2006-01-02")}]; got == nil || got.deniedRate == 0 {
		t.Error("rate denials must be counted for the rollup")
	}
}

func TestAuthorize_OverQuota(t *testing.T) {
	today := time.Now().UTC().Format("2006-01-02")
	rollup := &kipperv1.UsageRollup{
		ObjectMeta: metav1.ObjectMeta{Name: RollupName(testPrefix, today), Namespace: "shop-prod"},
		Spec:       kipperv1.UsageRollupSpec{KeyPrefix: testPrefix, Day: today, Allowed: 10},
	}
	a := testAuthorizer(t, alwaysFresh(), testApiKey(), rollup,
		testPlan(func(p *kipperv1.UsagePlan) {
			p.Spec.Quota = &kipperv1.PlanQuota{Requests: 10, Period: "day"}
		}))

	res := a.Authorize(context.Background(), "shop-prod", "api", testKey)
	if res.Decision != DecisionDenyQuota {
		t.Fatalf("quota exhausted by flushed rollups: decision = %v, want deny_quota", res.Decision)
	}
	// A daily quota resets at the next UTC midnight, so the advice must be
	// positive and never exceed a day.
	if res.RetryAfter <= 0 || res.RetryAfter > 24*time.Hour {
		t.Errorf("quota denial RetryAfter = %v, want within (0, 24h]", res.RetryAfter)
	}
}

// failRollupListReader delegates every call to the fake client except a List
// of UsageRollups, which it fails, standing in for a cache read error on the
// quota path.
type failRollupListReader struct {
	client.Reader
}

func (f failRollupListReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*kipperv1.UsageRollupList); ok {
		return errRollupList
	}
	return f.Reader.List(ctx, list, opts...)
}

var errRollupList = errString("simulated rollup list failure")

type errString string

func (e errString) Error() string { return string(e) }

func TestAuthorize_UnreadableUsageIsUnavailableNotQuota(t *testing.T) {
	inner := crfake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(testApiKey(), testPlan(func(p *kipperv1.UsagePlan) {
			p.Spec.Quota = &kipperv1.PlanQuota{Requests: 10, Period: "month"}
		})).
		WithIndex(&kipperv1.ApiKey{}, keyPrefixField,
			func(o client.Object) []string { return []string{o.(*kipperv1.ApiKey).Spec.Prefix} }).
		WithIndex(&kipperv1.UsageRollup{}, rollupKeyField,
			func(o client.Object) []string { return []string{o.(*kipperv1.UsageRollup).Spec.KeyPrefix} }).
		Build()
	a := NewAuthorizer(failRollupListReader{inner}, alwaysFresh(), NewUsageBuffer())

	res := a.Authorize(context.Background(), "shop-prod", "api", testKey)
	if res.Decision != DecisionUnavailable {
		t.Fatalf("unreadable usage history: decision = %v, want unavailable", res.Decision)
	}
	// A read failure must record no client counter at all: it is neither an
	// allow nor a quota denial, so the buffer stays empty.
	if drained := a.usage.drain(); len(drained) != 0 {
		t.Errorf("read failure recorded %d usage counters, want 0", len(drained))
	}
	// The quota check runs before the rate bucket, so a gate_unavailable must
	// not have spent a token — no bucket is even created. Otherwise a retry
	// storm during an outage would drain the bucket and rate-limit legitimate
	// traffic once the store recovers.
	a.mu.Lock()
	_, bucketCreated := a.buckets["shop-prod/"+testPrefix]
	a.mu.Unlock()
	if bucketCreated {
		t.Error("gate_unavailable consumed a rate token; the quota read must precede takeToken")
	}
}

func TestAuthorize_QuotaCountsUnflushedUsage(t *testing.T) {
	a := testAuthorizer(t, alwaysFresh(), testApiKey(),
		testPlan(func(p *kipperv1.UsagePlan) {
			p.Spec.Quota = &kipperv1.PlanQuota{Requests: 3, Period: "day"}
		}))

	var allowed, quotaDenied int
	for i := 0; i < 5; i++ {
		switch a.Authorize(context.Background(), "shop-prod", "api", testKey).Decision {
		case DecisionAllow:
			allowed++
		case DecisionDenyQuota:
			quotaDenied++
		}
	}
	if allowed != 3 || quotaDenied != 2 {
		t.Fatalf("quota of 3 with local counting: allowed=%d denied=%d, want 3/2", allowed, quotaDenied)
	}
}

func TestAuthorize_FailsClosedWhenStale(t *testing.T) {
	// Synced once, but the last successful probe is beyond the stale
	// bound: the replica must refuse decisions rather than serve
	// possibly-revoked keys.
	stale := NewFreshness(nil, nil, func() bool { return true }, time.Hour, 90*time.Second, nil, "test")
	stale.lastFresh = time.Now().Add(-2 * time.Minute)
	a := testAuthorizer(t, stale, testApiKey(), testPlan())

	if d := a.Authorize(context.Background(), "shop-prod", "api", testKey).Decision; d != DecisionUnavailable {
		t.Fatalf("stale cache: decision = %v, want unavailable", d)
	}
}

func TestAuthorize_FailsClosedBeforeInitialSync(t *testing.T) {
	unsynced := NewFreshness(nil, nil, func() bool { return false }, time.Hour, time.Hour, nil, "test")
	unsynced.lastFresh = time.Now()
	a := testAuthorizer(t, unsynced, testApiKey(), testPlan())

	if d := a.Authorize(context.Background(), "shop-prod", "api", testKey).Decision; d != DecisionUnavailable {
		t.Fatalf("unsynced cache: decision = %v, want unavailable", d)
	}
}

func TestPeriodStart(t *testing.T) {
	// 2026-07-08 is a Wednesday.
	now := time.Date(2026, 7, 8, 15, 4, 5, 0, time.UTC)

	if got := periodStart(now, "day"); !got.Equal(time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("day start = %v", got)
	}
	if got := periodStart(now, "week"); !got.Equal(time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("week start = %v, want Monday 2026-07-06", got)
	}
	if got := periodStart(now, "month"); !got.Equal(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("month start = %v", got)
	}
	// Sunday belongs to the week that started the previous Monday.
	sunday := time.Date(2026, 7, 12, 1, 0, 0, 0, time.UTC)
	if got := periodStart(sunday, "week"); !got.Equal(time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("Sunday week start = %v, want Monday 2026-07-06", got)
	}
}

func TestPeriodNext(t *testing.T) {
	now := time.Date(2026, 7, 8, 15, 30, 0, 0, time.UTC)
	if got := periodNext(now, "day"); !got.Equal(time.Date(2026, 7, 9, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("next day = %v, want 2026-07-09T00:00Z", got)
	}
	if got := periodNext(now, "week"); !got.Equal(time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("next week = %v, want Monday 2026-07-13T00:00Z", got)
	}
	if got := periodNext(now, "month"); !got.Equal(time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("next month = %v, want 2026-08-01T00:00Z", got)
	}
	// Month arithmetic at year end must roll the year, not normalise oddly.
	dec := time.Date(2026, 12, 31, 23, 0, 0, 0, time.UTC)
	if got := periodNext(dec, "month"); !got.Equal(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("next month over year end = %v, want 2027-01-01T00:00Z", got)
	}
}

func TestAuthorize_DeniesAmbiguousDuplicatePrefix(t *testing.T) {
	duplicate := testApiKey(func(k *kipperv1.ApiKey) { k.Name = "impostor" })
	a := testAuthorizer(t, alwaysFresh(), testApiKey(), duplicate, testPlan())

	if d := a.Authorize(context.Background(), "shop-prod", "api", testKey).Decision; d != DecisionDenyKey {
		t.Fatalf("two CRs sharing a prefix are ambiguous and must be denied, got %v", d)
	}
}
