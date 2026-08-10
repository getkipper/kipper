package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

// Decision is the outcome of one authorization check, mapped to an HTTP
// status by the handler.
type Decision int

const (
	// DecisionAllow admits the request.
	DecisionAllow Decision = iota
	// DecisionDenyKey rejects a missing, unknown, disabled, or
	// out-of-scope key (HTTP 401).
	DecisionDenyKey
	// DecisionDenyRate rejects a request over the plan's token bucket
	// (HTTP 429).
	DecisionDenyRate
	// DecisionDenyQuota rejects a request over the plan's period quota
	// (HTTP 429).
	DecisionDenyQuota
	// DecisionUnavailable refuses to answer because the replica cannot
	// decide: its view of keys and plans is stale or unsynced, or a usage
	// read for the quota check failed. The route fails closed (HTTP 503).
	DecisionUnavailable
)

// keyPrefixField is the cache index on ApiKey spec.prefix, so validation is
// one indexed lookup instead of a scan over all key hashes.
const keyPrefixField = "spec.prefix"

// Authorizer validates API keys against the informer cache, applies the
// plan's token bucket per replica, and counts usage in memory for the
// flusher. It performs no API-server calls on the request path.
type Authorizer struct {
	reader    client.Reader // the manager's cached client
	freshness *Freshness
	usage     *UsageBuffer

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	limiter *rate.Limiter
	rate    int
	burst   int
}

// NewAuthorizer wires the request-path dependencies together.
func NewAuthorizer(reader client.Reader, freshness *Freshness, usage *UsageBuffer) *Authorizer {
	return &Authorizer{
		reader:    reader,
		freshness: freshness,
		usage:     usage,
		buckets:   make(map[string]*bucket),
	}
}

// Result carries a decision plus the retry advice denials expose to
// clients through Retry-After.
type Result struct {
	Decision Decision
	// RetryAfter is set on rate and quota denials: how long until the
	// client can expect budget again.
	RetryAfter time.Duration
	// KeyPrefix and KeyName carry the calling consumer's non-secret
	// identity on an allow, forwarded to the backend by the handler. The
	// prefix is the durable identifier; the name is a convenience label and
	// may be empty.
	KeyPrefix string
	KeyName   string
}

func deny(d Decision) Result { return Result{Decision: d} }

// Authorize decides one request for the app in the namespace, presented
// with the raw X-API-Key value.
func (a *Authorizer) Authorize(ctx context.Context, namespace, app, rawKey string) Result {
	// Fail closed before consulting the cache: a stale cache silently
	// serving revoked keys is the failure mode this contract exists to
	// prevent.
	if !a.freshness.Fresh() {
		return deny(DecisionUnavailable)
	}

	prefix, ok := keyPrefix(rawKey)
	if !ok {
		return deny(DecisionDenyKey)
	}

	var keys kipperv1.ApiKeyList
	if err := a.reader.List(ctx, &keys,
		client.InNamespace(namespace),
		client.MatchingFields{keyPrefixField: prefix},
	); err != nil || len(keys.Items) == 0 {
		return deny(DecisionDenyKey)
	}
	// A prefix matching more than one CR is ambiguous: list order is not
	// stable and rollup attribution keys on the prefix. Only direct CR
	// writes can produce this state; deny it rather than guessing.
	if len(keys.Items) != 1 {
		return deny(DecisionDenyKey)
	}
	key := &keys.Items[0]

	if !hashMatches(rawKey, key.Spec.HashSHA256) {
		return deny(DecisionDenyKey)
	}
	now := time.Now().UTC()
	// An expired key is rejected on the same uniform 401 path as a disabled
	// or out-of-scope one, so nothing about it leaks to the caller.
	if !key.IsEnabled() || key.IsExpired(now) || !keyCoversApp(key, app) {
		return deny(DecisionDenyKey)
	}

	var plan kipperv1.UsagePlan
	if err := a.reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: key.Spec.Plan}, &plan); err != nil {
		// A key pointing at a deleted plan has no limits to enforce;
		// admitting it would be unmetered fail-open.
		return deny(DecisionDenyKey)
	}

	day := now.Format("2006-01-02")

	// Check the longer-window quota before spending a rate-bucket token. A
	// usage-store read failure then returns gate_unavailable without having
	// consumed a token, so a burst of 503 retries during an outage cannot
	// drain the bucket and spuriously rate-limit legitimate traffic once the
	// store recovers. The rate limiter still sheds after: the common allowed
	// path runs both checks regardless, so only excess traffic on a quota'd
	// key pays for the (indexed, in-memory) usage read before being shed.
	if q := plan.Spec.Quota; q != nil {
		used, err := a.periodUsage(ctx, namespace, prefix, q.Period, now)
		if err != nil {
			// A read failure means the gate cannot decide, not that the
			// key is over quota. Fail closed as gate_unavailable so the
			// client retries in seconds, and don't record a quota denial
			// that never happened.
			return deny(DecisionUnavailable)
		}
		if used >= q.Requests {
			a.usage.Add(namespace, prefix, day, CounterDeniedQuota)
			return Result{Decision: DecisionDenyQuota, RetryAfter: periodNext(now, q.Period).Sub(now)}
		}
	}

	if !a.takeToken(namespace+"/"+prefix, plan.Spec.Rate, plan.Spec.Burst) {
		a.usage.Add(namespace, prefix, day, CounterDeniedRate)
		// Plans use integer per-second rates, so the bucket refills within
		// a second; Retry-After speaks whole seconds, making 1 always
		// correct without touching the limiter's reservation state.
		return Result{Decision: DecisionDenyRate, RetryAfter: time.Second}
	}

	a.usage.Add(namespace, prefix, day, CounterAllowed)
	return Result{Decision: DecisionAllow, KeyPrefix: prefix, KeyName: key.Spec.DisplayName}
}

// takeToken applies the per-replica token bucket for the key, rebuilding
// the limiter when the plan's rate or burst changed.
func (a *Authorizer) takeToken(key string, planRate, planBurst int) bool {
	a.mu.Lock()
	b, ok := a.buckets[key]
	if !ok || b.rate != planRate || b.burst != planBurst {
		b = &bucket{
			limiter: rate.NewLimiter(rate.Limit(planRate), planBurst),
			rate:    planRate,
			burst:   planBurst,
		}
		a.buckets[key] = b
	}
	a.mu.Unlock()
	return b.limiter.Allow()
}

// periodUsage sums the key's flushed daily rollups covering the calendar
// period containing now, plus this replica's unflushed allowed count. The
// caller's clock sample is shared so the quota window and the Retry-After
// advice cannot straddle a period boundary and disagree.
func (a *Authorizer) periodUsage(ctx context.Context, namespace, prefix, period string, now time.Time) (int64, error) {
	var rollups kipperv1.UsageRollupList
	if err := a.reader.List(ctx, &rollups,
		client.InNamespace(namespace),
		client.MatchingFields{rollupKeyField: prefix},
	); err != nil {
		return 0, err
	}

	start := periodStart(now, period)
	// Floors cover the window between a successful flush and the cache
	// observing it: for each day, count at least what this replica already
	// wrote.
	floors := a.usage.AllowedFloors(namespace, prefix, start)
	var total int64
	for _, r := range rollups.Items {
		day, err := time.Parse("2006-01-02", r.Spec.Day)
		if err != nil || day.Before(start) {
			continue
		}
		allowed := r.Spec.Allowed
		if floor, ok := floors[r.Spec.Day]; ok {
			if floor > allowed {
				allowed = floor
			}
			delete(floors, r.Spec.Day)
		}
		total += allowed
	}
	// Rollups we wrote that the cache has not shown at all yet.
	for _, floor := range floors {
		total += floor
	}
	total += a.usage.UnflushedAllowed(namespace, prefix, start)
	return total, nil
}

// periodStart returns the UTC start of the calendar period containing now.
// Weeks start on Monday, matching ISO 8601.
func periodStart(now time.Time, period string) time.Time {
	switch period {
	case "week":
		daysSinceMonday := (int(now.Weekday()) + 6) % 7
		return time.Date(now.Year(), now.Month(), now.Day()-daysSinceMonday, 0, 0, 0, 0, time.UTC)
	case "month":
		return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	default: // day
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	}
}

// periodNext returns the UTC start of the calendar period after the one
// containing now: the moment a quota-exhausted key has budget again.
func periodNext(now time.Time, period string) time.Time {
	start := periodStart(now, period)
	switch period {
	case "week":
		return start.AddDate(0, 0, 7)
	case "month":
		return start.AddDate(0, 1, 0)
	default: // day
		return start.AddDate(0, 0, 1)
	}
}

// keyPrefix extracts the non-secret lookup handle from an issued key of the
// form kip_<prefix>_<secret>.
func keyPrefix(rawKey string) (string, bool) {
	parts := strings.SplitN(rawKey, "_", 3)
	if len(parts) != 3 || parts[0] != "kip" || parts[1] == "" || parts[2] == "" {
		return "", false
	}
	return parts[1], true
}

// hashMatches compares the SHA-256 of the presented key against the stored
// digest in constant time.
func hashMatches(rawKey, storedHex string) bool {
	sum := sha256.Sum256([]byte(rawKey))
	stored, err := hex.DecodeString(storedHex)
	if err != nil || len(stored) != len(sum) {
		return false
	}
	return subtle.ConstantTimeCompare(sum[:], stored) == 1
}

func keyCoversApp(key *kipperv1.ApiKey, app string) bool {
	if len(key.Spec.Apps) == 0 {
		return true
	}
	for _, a := range key.Spec.Apps {
		if a == app {
			return true
		}
	}
	return false
}
