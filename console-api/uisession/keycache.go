package uisession

import (
	"context"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// LoadKeyring reads and parses the signing Secret without creating
// anything. Used where a read-only view is wanted; the gate uses the
// self-healing KeyCache instead.
func LoadKeyring(ctx context.Context, client kubernetes.Interface) (*Keyring, error) {
	secret, err := client.CoreV1().Secrets(SigningSecretNamespace).Get(ctx, SigningSecretName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return parseKeyring(secret.Data)
}

// KeyCache serves the UI-session signing keyring to the forwardAuth gate
// without a Kubernetes read per request. It refreshes at most once per
// ttl, so a key rotation (revoke-all) takes effect within ttl.
//
// Unlike the share-link cache, its loader is LoadOrCreateKeyring: a
// missing signing Secret self-heals rather than failing closed, because
// UI sessions are the primary way operators reach service UIs and a
// deleted Secret must not lock every UI out permanently. Recreating the
// keyring is equivalent to a bulk revocation (every prior cookie fails on
// an unknown kid), which is the correct answer to a lost signing key. A
// corrupt Secret is likewise replaced with a fresh keyring. A transient
// API error keeps the last-good keyring only until its trust window ends,
// then fails closed.
type KeyCache struct {
	client kubernetes.Interface
	ttl    time.Duration
	now    func() time.Time

	mu        sync.Mutex
	ring      *Keyring
	ok        bool
	validThru time.Time
	retryAt   time.Time
}

// NewKeyCache builds a cache with the given refresh interval.
func NewKeyCache(client kubernetes.Interface, ttl time.Duration) *KeyCache {
	return &KeyCache{client: client, ttl: ttl, now: time.Now}
}

// Get returns the current keyring. ok is false only when a load failure
// has left the cache without a value still inside its trust window — it
// fails closed rather than serve keys past their promised staleness
// bound. Safe for concurrent use.
func (c *KeyCache) Get() (*Keyring, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	if c.ok && now.Before(c.validThru) {
		return c.ring, true
	}
	if now.Before(c.retryAt) {
		return c.currentLocked(now)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	kr, err := LoadOrCreateKeyring(ctx, c.client)
	if err == nil {
		c.ring, c.ok, c.validThru = kr, true, now.Add(c.ttl)
		c.retryAt = time.Time{}
	} else {
		// Transient failure: keep the last good keyring only until validThru,
		// then fall closed. Back off before the next read.
		c.retryAt = now.Add(c.ttl)
	}
	return c.currentLocked(now)
}

func (c *KeyCache) currentLocked(now time.Time) (*Keyring, bool) {
	if c.ok && now.Before(c.validThru) {
		return c.ring, true
	}
	return nil, false
}
