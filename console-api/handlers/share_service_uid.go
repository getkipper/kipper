package handlers

import (
	"context"
	"sync"
	"time"

	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

// ServiceUIDResolver answers the share gate's "does this service still
// exist, and with which UID" question through a short TTL cache, so the
// public /auth/check endpoint costs at most one Service read per service
// per TTL instead of one per asset request. Positive and negative
// answers are cached alike; the TTL bounds how long a deleted service's
// links keep working, alongside the grant cache.
type ServiceUIDResolver struct {
	Client crclient.Client
	TTL    time.Duration

	mu    sync.Mutex
	cache map[string]uidCacheEntry
	now   func() time.Time
}

type uidCacheEntry struct {
	uid       string
	ok        bool
	fetchedAt time.Time
}

// NewServiceUIDResolver builds a resolver with the given trust window.
func NewServiceUIDResolver(client crclient.Client, ttl time.Duration) *ServiceUIDResolver {
	return &ServiceUIDResolver{
		Client: client,
		TTL:    ttl,
		cache:  make(map[string]uidCacheEntry),
		now:    time.Now,
	}
}

// Get returns the live Service CR's UID. ok is false when the service
// does not exist or cannot be read — the gate fails closed either way.
func (r *ServiceUIDResolver) Get(ctx context.Context, namespace, name string) (string, bool) {
	key := namespace + "/" + name
	now := r.now()

	r.mu.Lock()
	if entry, hit := r.cache[key]; hit && now.Sub(entry.fetchedAt) < r.TTL {
		r.mu.Unlock()
		return entry.uid, entry.ok
	}
	r.mu.Unlock()

	var svc kipperv1.Service
	uid, ok := "", false
	if err := r.Client.Get(ctx, crclient.ObjectKey{Namespace: namespace, Name: name}, &svc); err == nil {
		uid, ok = string(svc.UID), true
	}

	r.mu.Lock()
	r.cache[key] = uidCacheEntry{uid: uid, ok: ok, fetchedAt: now}
	for k, e := range r.cache {
		if now.Sub(e.fetchedAt) >= r.TTL {
			delete(r.cache, k)
		}
	}
	r.mu.Unlock()
	return uid, ok
}
