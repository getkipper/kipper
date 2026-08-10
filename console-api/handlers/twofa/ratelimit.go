package twofa

import (
	"sync"
	"time"
)

const (
	// rateLimitWindow and rateLimitMax bound code-guessing: five failures in
	// five minutes locks the identity out for the rest of the window. The
	// lockout always expires, so a flood degrades into delay rather than a
	// permanent denial of service.
	rateLimitWindow = 5 * time.Minute
	rateLimitMax    = 5
)

// rateLimiter tracks failed code attempts per identity, in memory. The
// console-api runs as a single replica, so process-local state covers every
// verification path.
type rateLimiter struct {
	mu       sync.Mutex
	failures map[string][]time.Time
}

func newRateLimiter() *rateLimiter {
	return &rateLimiter{failures: make(map[string][]time.Time)}
}

// allowed reports whether the identity may attempt a code right now.
func (r *rateLimiter) allowed(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.recent(key)) < rateLimitMax
}

// recordFailure notes one failed attempt and reports whether this failure
// crossed the lockout threshold — the moment worth a security event, rather
// than one per failure.
func (r *rateLimiter) recordFailure(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	recent := append(r.recent(key), time.Now())
	r.failures[key] = recent
	return len(recent) == rateLimitMax
}

// reset clears the identity's failures after a successful verification.
func (r *rateLimiter) reset(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.failures, key)
}

// recent prunes and returns the failures still inside the window. Callers
// hold the lock.
func (r *rateLimiter) recent(key string) []time.Time {
	cutoff := time.Now().Add(-rateLimitWindow)
	var kept []time.Time
	for _, t := range r.failures[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(r.failures, key)
	} else {
		r.failures[key] = kept
	}
	return kept
}
