package main

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Decision labels stay a small fixed set; per-key labels would blow up
// cardinality and belong in UsageRollups, not Prometheus.
var (
	metricDecisions = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "authz_requests_total",
		Help: "Authorization decisions by outcome.",
	}, []string{"decision"})

	// Registered for its side effect; Prometheus reads it via the
	// registry, not through this identifier.
	_ = promauto.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "authz_cache_fresh_seconds",
		Help: "Seconds since the last successful freshness probe; -1 before the first success. Distinguishes 'authz down' from 'authz cache stale'.",
	}, func() float64 {
		if globalFreshness == nil {
			return -1
		}
		return globalFreshness.Age()
	})

	metricFlushFailures = promauto.NewCounter(prometheus.CounterOpts{
		Name: "authz_rollup_flush_failures_total",
		Help: "UsageRollup batches that failed to flush and were requeued.",
	})
)

// globalFreshness backs the gauge; set once at startup.
var globalFreshness *Freshness

func decisionLabel(d Decision) string {
	switch d {
	case DecisionAllow:
		return "allow"
	case DecisionDenyKey:
		return "deny_key"
	case DecisionDenyRate:
		return "deny_rate"
	case DecisionDenyQuota:
		return "deny_quota"
	default:
		return "unavailable"
	}
}
