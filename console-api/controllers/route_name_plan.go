package controllers

import (
	"reflect"
	"slices"
	"sort"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/getkipper/kipper/controller/pkg/routename"
)

// routeNameReloadLag allows time for Traefik to reload before attributing
// traffic to a formerly shared key.
const routeNameReloadLag = 30 * time.Second

// routeNameProducer is one backend an Ingress routes to.
type routeNameProducer struct {
	Namespace string
	Service   string
	Port      int32
	Created   time.Time
}

func (p routeNameProducer) key() string {
	return routename.Key(p.Namespace, p.Service)
}

// routeNameSnapshot is what the sweeper read in one pass.
type routeNameSnapshot struct {
	Now        time.Time
	Generation string
	// Bootstrap is set on the first pass of a generation, after every older
	// console-api pod has stopped. Only then can a key leave the shared state.
	Bootstrap bool
	// Heartbeat is the previous generation's last heartbeat, read on a
	// bootstrap pass.
	Heartbeat time.Time
	// Suspended pauses attribution while another binary is live or after a
	// scan gap. Open intervals close by SuspendedAt; new ones wait for a
	// later, unsuspended scan.
	Suspended   bool
	SuspendedAt time.Time
	Producers   []routeNameProducer
	Claims      map[string]routeNameClaim
	// Namespaces maps each live namespace to its UID.
	Namespaces map[string]types.UID
	// Workloads lists, per namespace, the names of its Apps and stateful
	// services, any of which could publish a route under its name.
	Workloads map[string]map[string]bool
	// Unreadable lists keys whose claim could not be parsed; they are left
	// untouched.
	Unreadable []string
}

// routeNamePlan is what a pass changes: claims to write and keys whose claims
// to delete.
type routeNamePlan struct {
	Write  []routeNameClaim
	Delete []string
}

// planRouteNames plans claim ownership, sharing and attribution intervals
// from one snapshot. Route removal is handled separately by the reconciler.
func planRouteNames(s routeNameSnapshot) routeNamePlan {
	byKey := map[string][]routeNameProducer{}
	for _, p := range s.Producers {
		byKey[p.key()] = append(byKey[p.key()], p)
	}
	keys := map[string]bool{}
	for k := range byKey {
		keys[k] = true
	}
	for k := range s.Claims {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)

	var plan routeNamePlan
	for _, key := range sorted {
		if slices.Contains(s.Unreadable, key) {
			continue
		}
		old, had := s.Claims[key]
		next, keep := s.planKey(key, old, had, byKey[key])
		if !keep {
			if had {
				plan.Delete = append(plan.Delete, key)
			}
			continue
		}
		next.Intervals = pruneIntervals(next.Intervals, s.Now)
		if !had || !sameRouteNameClaim(old, next) {
			plan.Write = append(plan.Write, next)
		}
	}
	return plan
}

// planKey returns the claim for one key and whether it should exist at all.
func (s routeNameSnapshot) planKey(key string, old routeNameClaim, had bool, producers []routeNameProducer) (routeNameClaim, bool) {
	if platform, ok := routename.Platform(key); ok {
		var tenant []routeNameProducer
		for _, p := range producers {
			if p.Namespace != platform.Namespace {
				tenant = append(tenant, p)
			}
		}
		return s.planOccupied(key, old, had, tenant)
	}

	earliest := earliestByNamespace(producers)
	c := copyRouteNameClaim(old)
	holderLive := had && c.Namespace != "" && c.UID != "" && string(s.Namespaces[c.Namespace]) == c.UID

	if !holderLive && had && len(c.Shared) > 0 {
		// A shared key keeps its sharers until a bootstrap, when no reconcile
		// that relied on the shared state can still be publishing.
		if !s.Bootstrap {
			return c, true
		}
		justLeftShared := s.resolveShared(&c, key, earliest)
		return s.withInterval(c, earliest, justLeftShared), true
	}
	if !holderLive {
		if len(earliest) == 0 {
			if !had || claimExpired(c, s.Now) {
				return c, false
			}
			closeOpenInterval(&c, s.Now)
			return c, true
		}
		holder := oldestNamespace(earliest)
		c = routeNameClaim{Key: key, Namespace: holder, UID: string(s.Namespaces[holder]), configMap: old.configMap}
		c.setShared(namespacesExcept(earliest, holder), s.Namespaces)
		return s.withInterval(c, earliest, false), true
	}

	if len(earliest) == 0 && len(c.Shared) == 0 && !s.canPublish(c.Namespace, key) && claimExpired(c, s.Now) {
		// Nothing produces the key, no workload could publish it, and its
		// history has aged out, as with a cert-manager solver's route.
		return c, false
	}
	others := namespacesExcept(earliest, c.Namespace)
	justLeftShared := false
	if s.Bootstrap {
		closeAt := s.Heartbeat
		if closeAt.IsZero() || closeAt.After(s.Now) {
			closeAt = s.Now
		}
		if at, ok := earliestOf(earliest, others); ok && at.Before(closeAt) {
			closeAt = at
		}
		closeOpenInterval(&c, s.boundedClose(closeAt))
		if len(c.Shared) > 0 || len(others) > 0 {
			justLeftShared = s.resolveShared(&c, key, earliest)
		}
	} else if len(others) > 0 {
		at, _ := earliestOf(earliest, others)
		if at.After(s.Now) {
			at = s.Now
		}
		closeOpenInterval(&c, s.boundedClose(at))
		c.setShared(unionSorted(s.validSharers(c), others), s.Namespaces)
	}
	return s.withInterval(c, earliest, justLeftShared), true
}

// resolveShared recomputes a shared key on a bootstrap pass, when no
// reconcile from before can still be publishing. A namespace drops out once
// it neither produces the key nor has a workload that could publish it. It
// reports whether the key stopped being shared.
func (s routeNameSnapshot) resolveShared(c *routeNameClaim, key string, earliest map[string]time.Time) bool {
	var recorded []string
	if string(s.Namespaces[c.Namespace]) == c.UID && c.UID != "" {
		recorded = append(recorded, c.Namespace)
	}
	candidates := unionSorted(append(recorded, s.validSharers(*c)...), namespacesExcept(earliest, ""))
	var remaining []string
	for _, ns := range candidates {
		if _, producing := earliest[ns]; producing || s.canPublish(ns, key) {
			remaining = append(remaining, ns)
		}
	}
	switch len(remaining) {
	case 0:
		c.setShared(nil, nil)
		return true
	case 1:
		if remaining[0] != c.Namespace || string(s.Namespaces[remaining[0]]) != c.UID {
			c.Namespace = remaining[0]
			c.UID = string(s.Namespaces[remaining[0]])
			c.Intervals = nil
		}
		c.setShared(nil, nil)
		return true
	}
	holder := c.Namespace
	if !contains(remaining, holder) {
		holder = oldestNamespace(pick(earliest, remaining))
		if holder == "" {
			holder = remaining[0]
		}
		c.Namespace = holder
		c.UID = string(s.Namespaces[holder])
		c.Intervals = nil
	}
	if string(s.Namespaces[holder]) != c.UID {
		c.UID = string(s.Namespaces[holder])
		c.Intervals = nil
	}
	var shared []string
	for _, ns := range remaining {
		if ns != holder {
			shared = append(shared, ns)
		}
	}
	c.setShared(shared, s.Namespaces)
	return false
}

// validSharers returns the recorded sharers whose namespace still has the UID
// it had when it was recorded.
func (s routeNameSnapshot) validSharers(c routeNameClaim) []string {
	var out []string
	for _, ns := range c.Shared {
		if c.sharedBy(ns, s.Namespaces[ns]) {
			out = append(out, ns)
		}
	}
	return out
}

// severalServices detects distinct Services with the same normalized key
// in one namespace, regardless of port.
func (s routeNameSnapshot) severalServices(key, namespace string) bool {
	seen := ""
	for _, p := range s.Producers {
		if p.Namespace != namespace || p.key() != key {
			continue
		}
		if seen != "" && p.Service != seen {
			return true
		}
		seen = p.Service
	}
	return false
}

// canPublish reports whether a workload in namespace has a name that produces
// key, so it could publish a route under it.
func (s routeNameSnapshot) canPublish(namespace, key string) bool {
	for name := range s.Workloads[namespace] {
		if routename.Key(namespace, name) == key {
			return true
		}
	}
	return false
}

// withInterval opens an interval when the holder alone produces the key and
// the generation is working, and closes one otherwise.
func (s routeNameSnapshot) withInterval(c routeNameClaim, earliest map[string]time.Time, justLeftShared bool) routeNameClaim {
	_, holderProduces := earliest[c.Namespace]
	canOpen := !s.Suspended && len(c.Shared) == 0 && !c.Occupied && holderProduces && !s.severalServices(c.Key, c.Namespace)
	open := openIntervalIndex(c)
	switch {
	case canOpen && open < 0:
		from := s.Now
		if justLeftShared {
			from = s.Now.Add(routeNameReloadLag)
		}
		c.Intervals = append(c.Intervals, attributionInterval{From: from, Generation: s.Generation})
	case !canOpen && open >= 0:
		closeOpenInterval(&c, s.boundedClose(s.Now))
	}
	return c
}

// boundedClose caps intervals at the suspension boundary to exclude time
// when route ownership was uncertain.
func (s routeNameSnapshot) boundedClose(at time.Time) time.Time {
	if s.Suspended && !s.SuspendedAt.IsZero() && s.SuspendedAt.Before(at) {
		return s.SuspendedAt
	}
	return at
}

// planOccupied records tenant use of a platform key and closes attribution
// intervals. A platform key with no tenant producers needs no claim.
func (s routeNameSnapshot) planOccupied(key string, old routeNameClaim, had bool, tenant []routeNameProducer) (routeNameClaim, bool) {
	if len(tenant) == 0 {
		return old, false
	}
	earliest := earliestByNamespace(tenant)
	holder := oldestNamespace(earliest)
	c := copyRouteNameClaim(old)
	if !had || c.Namespace != holder || c.UID != string(s.Namespaces[holder]) {
		c = routeNameClaim{Key: key, Namespace: holder, UID: string(s.Namespaces[holder]), configMap: old.configMap}
	}
	c.Occupied = true
	c.setShared(namespacesExcept(earliest, holder), s.Namespaces)
	closeOpenInterval(&c, s.Now)
	return c, true
}

func earliestByNamespace(producers []routeNameProducer) map[string]time.Time {
	out := map[string]time.Time{}
	for _, p := range producers {
		if t, ok := out[p.Namespace]; !ok || p.Created.Before(t) {
			out[p.Namespace] = p.Created
		}
	}
	return out
}

func oldestNamespace(earliest map[string]time.Time) string {
	var best string
	var bestAt time.Time
	for ns, at := range earliest {
		if best == "" || at.Before(bestAt) || (at.Equal(bestAt) && ns < best) {
			best, bestAt = ns, at
		}
	}
	return best
}

func namespacesExcept(earliest map[string]time.Time, except string) []string {
	var out []string
	for ns := range earliest {
		if ns != except {
			out = append(out, ns)
		}
	}
	sort.Strings(out)
	return out
}

func earliestOf(earliest map[string]time.Time, namespaces []string) (time.Time, bool) {
	var at time.Time
	found := false
	for _, ns := range namespaces {
		if t, ok := earliest[ns]; ok && (!found || t.Before(at)) {
			at, found = t, true
		}
	}
	return at, found
}

func pick(earliest map[string]time.Time, namespaces []string) map[string]time.Time {
	out := map[string]time.Time{}
	for _, ns := range namespaces {
		if t, ok := earliest[ns]; ok {
			out[ns] = t
		}
	}
	return out
}

func unionSorted(a, b []string) []string {
	set := map[string]bool{}
	for _, v := range append(append([]string{}, a...), b...) {
		set[v] = true
	}
	var out []string
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func openIntervalIndex(c routeNameClaim) int {
	for i := range c.Intervals {
		if c.Intervals[i].To == nil {
			return i
		}
	}
	return -1
}

// closeOpenInterval ends the open interval at at, or at its start when at
// lies before it, which leaves an empty interval for pruning to drop.
func closeOpenInterval(c *routeNameClaim, at time.Time) {
	i := openIntervalIndex(*c)
	if i < 0 {
		return
	}
	if at.Before(c.Intervals[i].From) {
		at = c.Intervals[i].From
	}
	c.Intervals[i].To = &at
}

// claimExpired reports whether every interval ended before the retention window.
func claimExpired(c routeNameClaim, now time.Time) bool {
	cutoff := now.Add(-attributionRetention)
	for _, iv := range c.Intervals {
		if iv.To == nil || !iv.To.Before(cutoff) {
			return false
		}
	}
	return true
}

func copyRouteNameClaim(c routeNameClaim) routeNameClaim {
	out := c
	out.Shared = append([]string(nil), c.Shared...)
	out.SharedUIDs = nil
	for k, v := range c.SharedUIDs {
		if out.SharedUIDs == nil {
			out.SharedUIDs = map[string]string{}
		}
		out.SharedUIDs[k] = v
	}
	out.Intervals = nil
	for _, iv := range c.Intervals {
		cp := iv
		if iv.To != nil {
			to := *iv.To
			cp.To = &to
		}
		out.Intervals = append(out.Intervals, cp)
	}
	return out
}

func sameRouteNameClaim(a, b routeNameClaim) bool {
	a.configMap, b.configMap = nil, nil
	if len(a.Shared) == 0 {
		a.Shared, a.SharedUIDs = nil, nil
	}
	if len(b.Shared) == 0 {
		b.Shared, b.SharedUIDs = nil, nil
	}
	if len(a.Intervals) == 0 {
		a.Intervals = nil
	}
	if len(b.Intervals) == 0 {
		b.Intervals = nil
	}
	return reflect.DeepEqual(a, b)
}
