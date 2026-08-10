package main

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// canaryAnnotationPrefix namespaces the per-replica probe annotations on the
// canary object.
const canaryAnnotationPrefix = "authz.kipper.run/probe-"

// Freshness is this replica's cache-health clock, the mechanism behind the
// fail-closed contract. Informer HasSynced only proves the initial LIST, a
// quiet watch is ambiguous, and a successful direct LIST proves only that
// the API server answers — none of those prove that the informer cache the
// authorizer reads has observed recent events. A wedged watch behind healthy
// LISTs would serve revoked keys forever.
//
// So the clock is advanced by a write-through-canary round trip: each probe
// writes a fresh timestamp annotation onto a dedicated canary ApiKey with a
// direct request, then waits until that exact value is visible through the
// informer cache. Only when the watch pipeline demonstrably delivered the
// event does the clock advance. Once the clock exceeds the stale bound, the
// replica reports unready (Traefik stops routing to it) and denies whatever
// still arrives.
//
// The stale bound is the maximum time a revoked key may keep working
// through this replica.
//
// The authorizer reads three types from the cache — ApiKey (validity), UsagePlan
// (limits), and UsageRollup (quota counts) — each behind its own watch. A single
// canary would only prove one of them, leaving a wedged plan or rollup watch to
// serve stale limits or quota counts on a replica that still reports fresh. So
// the probe round-trips one canary per type and advances the clock only when
// every watch delivered its write.
type Freshness struct {
	direct     client.Reader // uncached reads for the read-modify-write
	cached     client.Client // the informer-backed client the authorizer uses
	interval   time.Duration
	staleBound time.Duration
	synced     func() bool
	canaries   []canaryTarget
	replicaID  string

	mu        sync.RWMutex
	lastFresh time.Time
}

// canaryTarget is one write-through probe: a typed object (so the Get/Update
// runs against that type's informer) at a fixed key. template is a zero-valued
// object copied for each request.
type canaryTarget struct {
	key      client.ObjectKey
	template client.Object
}

// NewFreshness builds the clock. direct must bypass the informer cache;
// cached must be the same cache-backed client the authorizer reads, so the
// probe validates the exact view decisions are made from. Each canary probes a
// distinct type's watch; the clock advances only when all of them round-trip.
func NewFreshness(direct client.Reader, cached client.Client, synced func() bool, interval, staleBound time.Duration, canaries []canaryTarget, replicaID string) *Freshness {
	return &Freshness{
		direct:     direct,
		cached:     cached,
		interval:   interval,
		staleBound: staleBound,
		synced:     synced,
		canaries:   canaries,
		replicaID:  replicaID,
	}
}

// Run probes until the context ends.
func (f *Freshness) Run(ctx context.Context) {
	// Probe immediately so a healthy replica becomes ready without
	// waiting a full interval.
	f.probe(ctx)
	ticker := time.NewTicker(f.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			f.probe(ctx)
		}
	}
}

// probe performs one write-and-observe round trip across every canary. It
// writes each canary directly, then waits until all of them are visible through
// the cache. The clock advances only when every watch delivered its write, so a
// single wedged informer (ApiKey, UsagePlan, or UsageRollup) stalls the replica.
func (f *Freshness) probe(ctx context.Context) {
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	value := time.Now().UTC().Format(time.RFC3339Nano)
	for i := range f.canaries {
		if !f.writeCanary(probeCtx, f.canaries[i], value) {
			return
		}
	}

	// The watch pipeline must deliver our own writes into the cache. A healthy
	// informer does this in milliseconds; a wedged one never does, and the
	// clock stops. Observed is indexed rather than keyed by object key because
	// the canaries share a name across types.
	annoKey := canaryAnnotationPrefix + f.replicaID
	observed := make([]bool, len(f.canaries))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		allSeen := true
		for i := range f.canaries {
			if observed[i] {
				continue
			}
			obj := f.canaries[i].template.DeepCopyObject().(client.Object)
			if err := f.cached.Get(probeCtx, f.canaries[i].key, obj); err == nil &&
				obj.GetAnnotations()[annoKey] == value {
				observed[i] = true
				continue
			}
			allSeen = false
		}
		if allSeen {
			f.mu.Lock()
			f.lastFresh = time.Now()
			f.mu.Unlock()
			return
		}
		select {
		case <-probeCtx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
	for i := range f.canaries {
		if !observed[i] {
			log.Printf("authz: freshness probe wrote canary %T %s but the informer cache never showed it — watch pipeline unhealthy", f.canaries[i].template, f.canaries[i].key)
		}
	}
}

// writeCanary stamps this replica's annotation on one canary with a direct
// read-modify-write, retrying the sibling replica's write conflicts. Stale
// annotations from pods that no longer exist are pruned along the way.
func (f *Freshness) writeCanary(ctx context.Context, t canaryTarget, value string) bool {
	annoKey := canaryAnnotationPrefix + f.replicaID
	for attempt := 0; attempt < 3; attempt++ {
		obj := t.template.DeepCopyObject().(client.Object)
		if err := f.direct.Get(ctx, t.key, obj); err != nil {
			if errors.IsNotFound(err) {
				// A deleted canary fails this replica closed within the stale
				// bound. Nothing recreates it, so name the recovery: authz has
				// no create permission on the canary types by design (it is the
				// key validator).
				log.Printf("authz: freshness probe: canary %T %s is missing — it may have been deleted; re-apply deploy/authz.yaml to restore it", t.template, t.key)
			} else {
				log.Printf("authz: freshness probe: reading canary %T %s: %v", t.template, t.key, err)
			}
			return false
		}
		anns := obj.GetAnnotations()
		if anns == nil {
			anns = map[string]string{}
		}
		anns[annoKey] = value
		pruneStaleProbes(anns, annoKey)
		obj.SetAnnotations(anns)

		err := f.cached.Update(ctx, obj)
		if err == nil {
			return true
		}
		if !errors.IsConflict(err) {
			log.Printf("authz: freshness probe: writing canary %T %s: %v", t.template, t.key, err)
			return false
		}
	}
	log.Printf("authz: freshness probe: canary %T %s update kept conflicting", t.template, t.key)
	return false
}

// pruneStaleProbes drops probe annotations from replicas that stopped
// writing over an hour ago, so rollouts do not accumulate annotations
// forever.
func pruneStaleProbes(annotations map[string]string, ownKey string) {
	cutoff := time.Now().Add(-time.Hour)
	for key, raw := range annotations {
		if key == ownKey || !strings.HasPrefix(key, canaryAnnotationPrefix) {
			continue
		}
		if ts, err := time.Parse(time.RFC3339Nano, raw); err != nil || ts.Before(cutoff) {
			delete(annotations, key)
		}
	}
}

// Fresh reports whether this replica may serve authorization decisions.
func (f *Freshness) Fresh() bool {
	if !f.synced() {
		return false
	}
	f.mu.RLock()
	last := f.lastFresh
	f.mu.RUnlock()
	return !last.IsZero() && time.Since(last) < f.staleBound
}

// Age returns seconds since the last successful probe, for the
// authz_cache_fresh_seconds metric. A replica that never probed
// successfully reports -1.
func (f *Freshness) Age() float64 {
	f.mu.RLock()
	last := f.lastFresh
	f.mu.RUnlock()
	if last.IsZero() {
		return -1
	}
	return time.Since(last).Seconds()
}
