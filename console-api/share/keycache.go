package share

import (
	"context"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// LoadKeyring reads and parses the signing Secret without creating or
// migrating anything — the gate is a read-only consumer. A legacy v0
// Secret (no current slot) is an error: the mint path migrates it, and
// until then the gate fails closed, which is exactly the moment v0's
// kid-less tokens stop working.
func LoadKeyring(ctx context.Context, client kubernetes.Interface) (*Keyring, error) {
	secret, err := client.CoreV1().Secrets(SigningSecretNamespace).Get(ctx, SigningSecretName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return parseKeyring(secret.Data)
}

// KeyCache serves the share signing keyring to the forwardAuth gate
// without a Kubernetes read on every request (the gate is public and
// would otherwise amplify load onto the API server). It refreshes at
// most once per ttl, so a key rotation takes effect within ttl. A
// missing or unparseable Secret is cached as "no keyring", making the
// gate fail closed for share links while leaving Dex auth untouched.
type KeyCache struct {
	client kubernetes.Interface
	ttl    time.Duration
	now    func() time.Time

	mu        sync.Mutex
	ring      *Keyring
	ok        bool
	validThru time.Time // last-good keyring is trusted only until this instant
	retryAt   time.Time // next allowed API read after a transient failure
}

// NewKeyCache builds a cache with the given refresh interval.
func NewKeyCache(client kubernetes.Interface, ttl time.Duration) *KeyCache {
	return &KeyCache{client: client, ttl: ttl, now: time.Now}
}

// Get returns the current keyring. ok is false when none exists, or when
// a load failure has left the cache without a value still inside its
// trust window — the cache fails closed rather than serve keys past
// their promised staleness bound. Safe for concurrent use.
func (c *KeyCache) Get() (ring *Keyring, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()

	// A fresh value inside its trust window is served without a read.
	if c.ok && now.Before(c.validThru) {
		return c.ring, c.ok
	}
	// Back off between reads so a persistent failure doesn't hammer the
	// API server, but never serve keys past validThru while backing off.
	if now.Before(c.retryAt) {
		return c.currentLocked(now)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	kr, err := LoadKeyring(ctx, c.client)
	switch {
	case err == nil:
		c.ring, c.ok, c.validThru = kr, true, now.Add(c.ttl)
		c.retryAt = time.Time{}
	case apierrors.IsNotFound(err):
		// Definitively no keyring: fail share auth closed, and cache that
		// answer like any other. A cluster that never minted a link would
		// otherwise pay one API read per unauthenticated request to any
		// service-UI host — the exact amplification this cache prevents.
		c.ring, c.ok, c.validThru = nil, false, time.Time{}
		c.retryAt = now.Add(c.ttl)
	default:
		// Transient or parse error: keep the last good keyring only until
		// validThru, then fall closed. Schedule the next retry.
		c.retryAt = now.Add(c.ttl)
	}
	return c.currentLocked(now)
}

// currentLocked returns the cached keyring only if it's still inside its
// trust window; expired values fail closed. Caller holds c.mu.
func (c *KeyCache) currentLocked(now time.Time) (*Keyring, bool) {
	if c.ok && now.Before(c.validThru) {
		return c.ring, true
	}
	return nil, false
}
