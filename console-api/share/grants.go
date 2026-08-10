package share

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Grants live one Secret per share in kipper-system. Create and delete are
// atomic object operations, so concurrent mints and revokes never fight
// over a shared document, and revocation is simply deleting the grant.
const (
	grantSecretPrefix = "kipper-share-"
	grantField        = "grant"

	// GrantLabel marks every grant Secret; the service labels scope
	// per-service list, revoke-all, and the reconciler's delete-time
	// cleanup.
	GrantLabel          = "kipper.run/share"
	GrantServiceLabel   = "kipper.run/share-service"
	GrantNamespaceLabel = "kipper.run/share-namespace"

	// GrantVersion is the schema version stored in every grant. A grant
	// with any other version fails validation closed.
	GrantVersion = 1

	// MaxGrantsPerService bounds how many live links one service can
	// carry — a runaway minter fills a quota, not etcd.
	MaxGrantsPerService = 20

	// grantCacheTTL is how long the gate trusts a cached lookup, positive
	// or negative. Together with the key cache it defines the documented
	// revocation SLA (≤30s).
	grantCacheTTL = 15 * time.Second
)

// jtiPattern is the only jti shape v1 mints (128-bit lowercase hex). The
// grant Secret's name embeds the jti, so anything else must fail closed
// before touching the API.
var jtiPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

// Grant is the stored record of one share link. Every field duplicated in
// the token is compared at validation, so a token can never smuggle claims
// its grant does not carry.
type Grant struct {
	V          int       `json:"v"`
	JTI        string    `json:"jti"`
	ServiceUID string    `json:"serviceUID"`
	Service    string    `json:"service"`
	Namespace  string    `json:"namespace"`
	Host       string    `json:"host"`
	Label      string    `json:"label,omitempty"`
	CreatedBy  string    `json:"createdBy"`
	CreatedAt  time.Time `json:"createdAt"`
	ExpiresAt  time.Time `json:"expiresAt"`
}

// ErrGrantLimit is returned when a service already carries its maximum
// number of live grants.
var ErrGrantLimit = fmt.Errorf("share: service already has %d active links — revoke one first", MaxGrantsPerService)

// ErrGrantNotFound is returned when a revoke names a grant that does not
// exist (or belongs to a different service). It is distinct from a
// storage failure so callers can map the two to different status codes.
var ErrGrantNotFound = errors.New("share: unknown link id")

type grantCacheEntry struct {
	grant     *Grant // nil = negative entry; never handed out directly
	fetchedAt time.Time
}

// GrantStore persists grants as individual Secrets and serves the gate's
// lookups through a small TTL cache.
type GrantStore struct {
	client kubernetes.Interface

	mu    sync.Mutex
	cache map[string]grantCacheEntry
	now   func() time.Time
}

// NewGrantStore builds a store around the cluster client.
func NewGrantStore(client kubernetes.Interface) *GrantStore {
	return &GrantStore{
		client: client,
		cache:  make(map[string]grantCacheEntry),
		now:    time.Now,
	}
}

func grantSecretName(jti string) string {
	return grantSecretPrefix + jti
}

func grantLabels(g Grant) map[string]string {
	return map[string]string{
		"app.kubernetes.io/managed-by": "kipper",
		GrantLabel:                     "true",
		GrantServiceLabel:              g.Service,
		GrantNamespaceLabel:            g.Namespace,
	}
}

func serviceSelector(namespace, service string) string {
	return fmt.Sprintf("%s=true,%s=%s,%s=%s", GrantLabel, GrantServiceLabel, service, GrantNamespaceLabel, namespace)
}

// Create persists a new grant. It enforces the per-service cap and prunes
// that service's expired grants on the way, so the mint path keeps the
// store bounded without a global sweeper in the hot path.
func (s *GrantStore) Create(ctx context.Context, g Grant) error {
	if g.V != GrantVersion {
		return fmt.Errorf("share: grant version %d is not %d", g.V, GrantVersion)
	}
	if !jtiPattern.MatchString(g.JTI) {
		return fmt.Errorf("share: malformed grant id")
	}
	if g.ServiceUID == "" || g.Service == "" || g.Namespace == "" || g.Host == "" {
		return fmt.Errorf("share: grant is incomplete")
	}

	// The cap is a soft quota against runaway minters, not a security
	// boundary: concurrent mints can briefly overshoot it (list-then-
	// create has no coordination point by design — per-grant Secrets were
	// chosen precisely to avoid a contended shared document).
	live, err := s.pruneAndList(ctx, g.Namespace, g.Service)
	if err != nil {
		return err
	}
	if len(live) >= MaxGrantsPerService {
		return ErrGrantLimit
	}

	raw, err := json.Marshal(g)
	if err != nil {
		return fmt.Errorf("share: encoding grant: %w", err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      grantSecretName(g.JTI),
			Namespace: SigningSecretNamespace,
			Labels:    grantLabels(g),
		},
		Data: map[string][]byte{grantField: raw},
	}
	if _, err := s.client.CoreV1().Secrets(SigningSecretNamespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("share: storing grant: %w", err)
	}
	s.forget(g.JTI)
	return nil
}

// Get resolves a jti to its grant through the TTL cache. A missing,
// expired, malformed, or version-unknown grant returns nil — the gate
// treats every nil identically (fail closed, no oracle).
func (s *GrantStore) Get(ctx context.Context, jti string) *Grant {
	if !jtiPattern.MatchString(jti) {
		return nil
	}

	now := s.now()
	s.mu.Lock()
	if entry, ok := s.cache[jti]; ok && now.Sub(entry.fetchedAt) < grantCacheTTL {
		s.mu.Unlock()
		return liveGrant(entry.grant, now)
	}
	s.mu.Unlock()

	grant := s.fetch(ctx, jti)
	s.mu.Lock()
	s.cache[jti] = grantCacheEntry{grant: grant, fetchedAt: now}
	// Drop stale entries opportunistically so the cache stays bounded by
	// the set of jtis seen within the TTL, not by history.
	for k, e := range s.cache {
		if now.Sub(e.fetchedAt) >= grantCacheTTL {
			delete(s.cache, k)
		}
	}
	s.mu.Unlock()
	return liveGrant(grant, now)
}

// liveGrant returns a copy of an unexpired grant. Copying keeps the cache
// authoritative: a caller mutating its result cannot poison the cached
// authorization state another request will read.
func liveGrant(g *Grant, now time.Time) *Grant {
	if g == nil || now.After(g.ExpiresAt) {
		return nil
	}
	out := *g
	return &out
}

func (s *GrantStore) fetch(ctx context.Context, jti string) *Grant {
	secret, err := s.client.CoreV1().Secrets(SigningSecretNamespace).Get(ctx, grantSecretName(jti), metav1.GetOptions{})
	if err != nil {
		return nil
	}
	return parseGrant(secret, jti)
}

func parseGrant(secret *corev1.Secret, jti string) *Grant {
	var g Grant
	if err := json.Unmarshal(secret.Data[grantField], &g); err != nil {
		return nil
	}
	if g.V != GrantVersion || g.JTI != jti {
		return nil
	}
	if g.ServiceUID == "" || g.Service == "" || g.Namespace == "" || g.Host == "" {
		return nil
	}
	return &g
}

// Matches reports whether the token's claims agree with the grant on every
// duplicated field. host must already be canonicalised.
func (g *Grant) Matches(claims *Claims, host string) bool {
	if g == nil || claims == nil {
		return false
	}
	return g.JTI == claims.ID &&
		g.ServiceUID == claims.ServiceUID &&
		g.Host == host &&
		g.Namespace+"/"+g.Service == claims.Service &&
		claims.ExpiresAt != nil && g.ExpiresAt.Equal(claims.ExpiresAt.Time)
}

// List returns the service's live grants, skipping expired and malformed
// entries.
func (s *GrantStore) List(ctx context.Context, namespace, service string) ([]Grant, error) {
	secrets, err := s.client.CoreV1().Secrets(SigningSecretNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: serviceSelector(namespace, service),
	})
	if err != nil {
		return nil, fmt.Errorf("share: listing grants: %w", err)
	}
	now := s.now()
	var out []Grant
	for i := range secrets.Items {
		jti, ok := jtiFromSecretName(secrets.Items[i].Name)
		if !ok {
			continue
		}
		if g := liveGrant(parseGrant(&secrets.Items[i], jti), now); g != nil {
			out = append(out, *g)
		}
	}
	return out, nil
}

// Revoke deletes one grant. It refuses a jti that belongs to a different
// service, so a revoke request cannot reach across services.
func (s *GrantStore) Revoke(ctx context.Context, namespace, service, jti string) error {
	if !jtiPattern.MatchString(jti) {
		return ErrGrantNotFound
	}
	secret, err := s.client.CoreV1().Secrets(SigningSecretNamespace).Get(ctx, grantSecretName(jti), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return ErrGrantNotFound
	}
	if err != nil {
		return fmt.Errorf("share: reading grant: %w", err)
	}
	g := parseGrant(secret, jti)
	if g == nil || g.Namespace != namespace || g.Service != service {
		return ErrGrantNotFound
	}
	if err := s.client.CoreV1().Secrets(SigningSecretNamespace).Delete(ctx, secret.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("share: revoking link: %w", err)
	}
	s.forget(jti)
	return nil
}

// RevokeAllForService deletes every grant of one service — the reconciler
// calls this from the service finalizer, and the endpoints expose it as
// per-service revoke-all.
func (s *GrantStore) RevokeAllForService(ctx context.Context, namespace, service string) error {
	return s.deleteBySelector(ctx, serviceSelector(namespace, service))
}

// RevokeAll deletes every grant in the cluster (the emergency lever,
// paired with a key rotation in the compromise runbook).
func (s *GrantStore) RevokeAll(ctx context.Context) error {
	return s.deleteBySelector(ctx, GrantLabel+"=true")
}

// maxRevokeSweeps bounds the re-sweep in deleteBySelector. A grant minted
// concurrently with a revoke lands after the list that missed it, so one
// more pass catches it. Convergence normally happens on the first or second
// pass; the bound only matters under a pathological mint storm.
const maxRevokeSweeps = 4

// deleteBySelector lists and deletes grant by grant, re-sweeping until a
// list returns nothing. It only reports success once it has observed an
// empty pass, so a caller that treats the error as fail-closed (the service
// finalizer) never releases while grants may remain. If the store won't
// converge within maxRevokeSweeps — sustained concurrent minting, or grants
// that keep reappearing — it returns an error rather than falsely reporting
// a clean sweep. Per-object deletes keep behaviour identical across real
// clusters and test fakes.
//
// The guaranteed kill switch for a leaked signing key is still key rotation
// (two rotations retire a key); revoke-all clears the grant store and pairs
// with rotation in the compromise runbook.
func (s *GrantStore) deleteBySelector(ctx context.Context, selector string) error {
	for sweep := 0; ; sweep++ {
		secrets, err := s.client.CoreV1().Secrets(SigningSecretNamespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return fmt.Errorf("share: listing grants: %w", err)
		}
		if len(secrets.Items) == 0 {
			s.forgetAll()
			return nil
		}
		if sweep >= maxRevokeSweeps {
			return fmt.Errorf("share: revoke did not converge after %d sweeps, %d link(s) may remain", maxRevokeSweeps, len(secrets.Items))
		}
		for i := range secrets.Items {
			if err := s.client.CoreV1().Secrets(SigningSecretNamespace).Delete(ctx, secrets.Items[i].Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("share: revoking links: %w", err)
			}
		}
	}
}

// pruneAndList removes the service's expired grants and returns the live
// ones.
func (s *GrantStore) pruneAndList(ctx context.Context, namespace, service string) ([]Grant, error) {
	secrets, err := s.client.CoreV1().Secrets(SigningSecretNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: serviceSelector(namespace, service),
	})
	if err != nil {
		return nil, fmt.Errorf("share: listing grants: %w", err)
	}
	now := s.now()
	var live []Grant
	for i := range secrets.Items {
		name := secrets.Items[i].Name
		jti, ok := jtiFromSecretName(name)
		var g *Grant
		if ok {
			g = parseGrant(&secrets.Items[i], jti)
		}
		if g != nil && !now.After(g.ExpiresAt) {
			live = append(live, *g)
			continue
		}
		// Expired or unparseable: garbage either way, delete it.
		if derr := s.client.CoreV1().Secrets(SigningSecretNamespace).Delete(ctx, name, metav1.DeleteOptions{}); derr != nil && !apierrors.IsNotFound(derr) {
			return nil, fmt.Errorf("share: pruning grant: %w", derr)
		}
	}
	return live, nil
}

func jtiFromSecretName(name string) (string, bool) {
	if len(name) <= len(grantSecretPrefix) {
		return "", false
	}
	jti := name[len(grantSecretPrefix):]
	if !jtiPattern.MatchString(jti) {
		return "", false
	}
	return jti, true
}

func (s *GrantStore) forget(jti string) {
	s.mu.Lock()
	delete(s.cache, jti)
	s.mu.Unlock()
}

func (s *GrantStore) forgetAll() {
	s.mu.Lock()
	s.cache = make(map[string]grantCacheEntry)
	s.mu.Unlock()
}

// NewGrant assembles a grant for a mint request with a fresh id.
func NewGrant(serviceUID, service, namespace, host, label, createdBy string, lifetime time.Duration, now time.Time) (Grant, error) {
	if lifetime <= 0 || lifetime > MaxLifetime {
		return Grant{}, fmt.Errorf("share: lifetime must be between 0 and %s", MaxLifetime)
	}
	jti, err := randomID()
	if err != nil {
		return Grant{}, err
	}
	// Second precision throughout: the JWT exp claim carries whole
	// seconds, and Matches compares the grant's expiry against it exactly.
	return Grant{
		V:          GrantVersion,
		JTI:        jti,
		ServiceUID: serviceUID,
		Service:    service,
		Namespace:  namespace,
		Host:       host,
		Label:      label,
		CreatedBy:  createdBy,
		CreatedAt:  now.UTC().Truncate(time.Second),
		ExpiresAt:  now.Add(lifetime).UTC().Truncate(time.Second),
	}, nil
}
