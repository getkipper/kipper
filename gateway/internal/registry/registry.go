package registry

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"time"
)

// Pin deadlines. A pending fingerprint is part of the accepted set the moment
// it is token-asserted, so it must not linger unobserved indefinitely — a
// stolen token would otherwise park an attacker fingerprint for later use. A
// previous fingerprint only bridges rotation propagation. Both windows cover
// the daily heartbeat, which re-asserts a rotation still in flight.
const (
	pendingPinTTL = 24 * time.Hour
	prevPinTTL    = 24 * time.Hour
)

// Proof-of-possession timings (B16).
const (
	// challengeTTL bounds how long an issued nonce stays usable — long enough
	// for the cluster's next heartbeat to sign it, short enough to bound
	// replay of a captured challenge.
	challengeTTL = 5 * time.Minute
	// proofLease is how long a completed proof keeps the entry routable. It
	// comfortably exceeds the daily heartbeat so a couple of missed beats (a
	// control-plane outage) do not suspend a live cluster, yet is far shorter
	// than the 30-day name retention so a decommissioned IP converges to
	// suspension within a week.
	proofLease = 7 * 24 * time.Hour
	// unprovenReservationTTL is how long a never-proven registration holds its
	// label before release, applied only while EnforceProof is on. It outlasts
	// a normal install-to-first-proof gap but stops a squatter parking a label
	// for the full 30-day retention.
	unprovenReservationTTL = 2 * time.Hour
)

// defaultTombstoneTTL reserves a lapsed label for its previous token holder
// for 90 days. Published app, console, and issuer URLs can outlive the cluster;
// reassigning the label would serve those URLs under the gateway's certificate.
const defaultTombstoneTTL = 90 * 24 * time.Hour

// Entry represents a registered subdomain mapping.
type Entry struct {
	Subdomain string    `json:"subdomain"`
	IP        string    `json:"ip"`
	Token     string    `json:"token"`
	CreatedAt time.Time `json:"created_at"`
	LastSeen  time.Time `json:"last_seen"`

	// Hop-certificate pins are lowercase SPKI SHA-256 fingerprints, so certificate
	// reissues with the same key preserve the pin. PendingFingerprint holds a
	// token-authenticated assertion awaiting observation; PrevFingerprint covers
	// rotation propagation. Transitions mark the registry dirty for persistence.
	CertFingerprint    string    `json:"cert_fingerprint,omitempty"`
	PendingFingerprint string    `json:"pending_fingerprint,omitempty"`
	PendingSince       time.Time `json:"pending_since,omitzero"`
	PrevFingerprint    string    `json:"prev_fingerprint,omitempty"`
	PrevSince          time.Time `json:"prev_since,omitzero"`
	PinUpdatedAt       time.Time `json:"pin_updated_at,omitzero"`
	// FirstPinnedAt anchors the one-time settle window for Traefik replicas
	// still serving their fallback certificate. Rotation preserves this deadline.
	// The proof gate independently requires the observed key to be proven.
	FirstPinnedAt time.Time `json:"first_pinned_at,omitzero"`

	// Proof state binds a renewable lease to possession of the key served at
	// IP:443. ChallengeNonce is a short-lived, single-use signing challenge.
	// ProofExpiry and ProofKeySPKI determine whether the proof authorizes routing.
	ProvenAt        time.Time `json:"proven_at,omitzero"`
	ProofExpiry     time.Time `json:"proof_expiry,omitzero"`
	ProofKeySPKI    string    `json:"proof_key_spki,omitempty"`
	ProofProtocol   string    `json:"proof_protocol,omitempty"`
	ChallengeNonce  string    `json:"challenge_nonce,omitempty"`
	ChallengeExpiry time.Time `json:"challenge_expiry,omitzero"`

	// FirstProvenAt records the first successful proof and survives address moves.
	// Tombstone eligibility uses this history even when the current proof is cleared.
	FirstProvenAt time.Time `json:"first_proven_at,omitzero"`
}

// Registry stores subdomain-to-IP mappings. In the MVP this is
// in-memory with a mutex. A future version will use a persistent store.
type Registry struct {
	mu      sync.RWMutex
	entries map[string]*Entry // keyed by subdomain
	tokens  map[string]string // token -> subdomain (one subdomain per token)

	// saveMu serialises SaveTo end to end — snapshot through rename — so
	// concurrent saves commit in run order and an older snapshot can't overwrite
	// newer state, and so two writers can't interleave into the same temp file
	// and rename a corrupt snapshot into place.
	saveMu sync.Mutex

	// dirty records unpersisted in-memory changes: a Ping-advanced LastSeen
	// or a pin-state transition. Pings are frequent, so they set this flag
	// instead of writing on every call; a periodic FlushIfDirty coalesces
	// them into at most one write per interval. Pin transitions also set it
	// so an activation whose immediate save failed is retried rather than
	// silently reverting to unpinned grace on restart. Guarded by mu.
	dirty bool

	// InactivityTTL is how long a subdomain can go without a ping
	// before it expires. Default: 30 days.
	InactivityTTL time.Duration

	// TombstoneTTL is how long a lapsed label is held for its previous token
	// holder before anyone else may register it. Default: 90 days.
	TombstoneTTL time.Duration

	// EnforceProof turns on the proof-before-route regime (B16): a never-proven
	// registration is released after unprovenReservationTTL instead of held for
	// the full InactivityTTL. It is the same cutover switch the proxy reads to
	// route only proven entries, so both halves of the regime flip together.
	// Off during the transition, so the fleet acquires proofs before any
	// enforcement takes effect. Set once at startup.
	EnforceProof bool

	// newToken and newNonce are where this registry's randomness comes from.
	// They are per-registry rather than package-level so a test driving an
	// entropy failure through the HTTP handler cannot reach any other registry
	// in the process — the failures they model are rare and their handling is
	// security-relevant, so a test for one must not silently arm another.
	// Set by New, like the maps beside them; a Registry that skipped New has no
	// entries map either and panics whichever field it reaches first.
	newToken func() (string, error)
	newNonce func() (string, error)
}

// New creates a registry with default settings.
func New() *Registry {
	return &Registry{
		entries:       make(map[string]*Entry),
		tokens:        make(map[string]string),
		InactivityTTL: 30 * 24 * time.Hour,
		TombstoneTTL:  defaultTombstoneTTL,
		newToken:      generateToken,
		newNonce:      generateNonce,
	}
}

// SetRandomSourcesForTest replaces token and nonce sources for gateway handler
// tests. A nil source preserves the current one.
func (r *Registry) SetRandomSourcesForTest(token, nonce func() (string, error)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if token != nil {
		r.newToken = token
	}
	if nonce != nil {
		r.newNonce = nonce
	}
}

// Outcome reports what a Register call did to the registration.
type Outcome int

const (
	// Created is a registration that did not exist before. The token is issued
	// here and disclosed to the caller exactly once.
	Created Outcome = iota
	// Renewed is the token holder confirming it is still live at the same
	// address. Only this refreshes LastSeen.
	Renewed
	// Moved is the token holder arriving at a different address. Pin and proof
	// state does not survive it — see Register.
	Moved
	// Unauthenticated is an anonymous request naming an existing registration.
	// It is not an error, so a cluster that has lost its token still gets a
	// coherent answer, but it proves nothing and so refreshes nothing.
	Unauthenticated
)

// ErrSubdomainTaken reports that a subdomain is held by another registration
// and the caller presented no token for it. It is the one Register failure the
// caller can do something about, by choosing a different name, and it is
// separate from the rest for that reason: the alternative is telling an operator
// their free name belongs to somebody else because this process could not read
// random bytes, and watching them rename a cluster over it.
var ErrSubdomainTaken = errors.New("subdomain is already registered")

// Register assigns a subdomain to an IP and returns its registration outcome.
// An existing registration requires its token for renewal or an address change.
// An unauthenticated same-address request returns the entry without refreshing it;
// an unauthenticated address change returns ErrSubdomainTaken.
//
// Moving an address clears pin and proof state, requiring a fresh assertion and
// proof at the destination. FirstProvenAt preserves the label's serving history.
func (r *Registry) Register(subdomain, ip, token string) (*Entry, Outcome, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if existing, ok := r.entries[subdomain]; ok && r.isReleasableLocked(existing) {
		// The tombstone is spent. Drop it here rather than waiting for the next
		// Cleanup, so the label is free to the caller asking for it now.
		delete(r.tokens, existing.Token)
		delete(r.entries, subdomain)
	}

	if existing, ok := r.entries[subdomain]; ok {
		authorised := token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(existing.Token)) == 1
		// Whether this call is reviving something that had stopped serving, read
		// before anything below changes it. A revival re-arms proof; a renewal of
		// a live registration must not, or the daily heartbeat would suspend the
		// cluster it is keeping alive.
		reviving := r.isExpiredLocked(existing)
		switch {
		case !authorised && existing.IP == ip:
			return existing, Unauthenticated, nil
		case !authorised:
			return nil, Unauthenticated, fmt.Errorf("%w: %q", ErrSubdomainTaken, subdomain)
		case existing.IP == ip:
			// Only the token holder reaches here, which is what makes this a
			// revival of a lapsed label rather than a takeover of it.
			if reviving {
				// The label stopped serving, so whatever answers at this address
				// now has to demonstrate control before traffic returns. Keeping
				// the old lease would route to it on the strength of a proof made
				// before the cluster went away.
				r.clearProofLocked(existing)
			}
			existing.LastSeen = time.Now()
			r.dirty = true
			return existing, Renewed, nil
		default:
			existing.IP = ip
			existing.LastSeen = time.Now()
			r.clearPinLocked(existing)
			r.clearProofLocked(existing)
			r.dirty = true
			return existing, Moved, nil
		}
	}

	newToken, err := r.newToken()
	if err != nil {
		return nil, Created, fmt.Errorf("generating token: %w", err)
	}

	entry := &Entry{
		Subdomain: subdomain,
		IP:        ip,
		Token:     newToken,
		CreatedAt: time.Now(),
		LastSeen:  time.Now(),
	}

	r.entries[subdomain] = entry
	r.tokens[newToken] = subdomain
	// Mark dirty so a failed immediate save is retried by the periodic
	// flush instead of the registration silently vanishing on restart.
	r.dirty = true

	return entry, Created, nil
}

// clearPinLocked drops every accepted fingerprint and the settle window, so the
// next handshake at the new address is unpinned grace again rather than being
// measured against a key the old machine served.
func (r *Registry) clearPinLocked(entry *Entry) {
	entry.CertFingerprint = ""
	entry.PendingFingerprint = ""
	entry.PendingSince = time.Time{}
	entry.PrevFingerprint = ""
	entry.PrevSince = time.Time{}
	entry.PinUpdatedAt = time.Now()
	entry.FirstPinnedAt = time.Time{}
}

// clearProofLocked drops the proof lease and any outstanding challenge. Under
// proof-before-route this suspends routing until the cluster proves possession
// at the new address, which is the intended consequence of a move.
func (r *Registry) clearProofLocked(entry *Entry) {
	entry.ProvenAt = time.Time{}
	entry.ProofExpiry = time.Time{}
	entry.ProofKeySPKI = ""
	entry.ProofProtocol = ""
	entry.ChallengeNonce = ""
	entry.ChallengeExpiry = time.Time{}
}

// Deregister releases a subdomain immediately using its management token and
// returns the label for caller cleanup. Deliberate releases bypass tombstones,
// allowing an operator to rebuild under the same name after uninstalling.
func (r *Registry) Deregister(token string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	subdomain, ok := r.tokens[token]
	if !ok {
		return "", fmt.Errorf("invalid token")
	}

	delete(r.entries, subdomain)
	delete(r.tokens, token)
	r.dirty = true

	return subdomain, nil
}

// HeldBy reports whether subdomain exists and token is its management token.
// It is what separates a renewal from a claim, so the guards that decide who may
// take a name are not applied to whoever already holds it.
func (r *Registry) HeldBy(subdomain, token string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	entry, ok := r.entries[subdomain]
	return ok && token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(entry.Token)) == 1
}

// Lookup returns a copy of the entry for a subdomain, or nil if not found.
// A copy, not the live pointer: pin state mutates after registration, so a
// caller holding the live entry would race those writes.
func (r *Registry) Lookup(subdomain string) *Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	entry, ok := r.entries[subdomain]
	if !ok {
		return nil
	}

	if r.isExpiredLocked(entry) {
		return nil
	}

	snapshot := *entry
	return &snapshot
}

// isExpiredLocked reports whether inactivity or the initial proof deadline
// has lapsed. isReleasableLocked separately decides when the label becomes
// available to another holder. Caller holds r.mu.
func (r *Registry) isExpiredLocked(entry *Entry) bool {
	_, expired := r.lapsedAtLocked(entry)
	return expired
}

// lapsedAtLocked returns the moment an entry stopped serving, and whether it
// has. The moment is derived rather than stamped, so a gateway that was down
// when a registration aged out still measures the tombstone from when the lapse
// actually began. Caller holds r.mu.
func (r *Registry) lapsedAtLocked(entry *Entry) (time.Time, bool) {
	now := time.Now()
	if inactive := entry.LastSeen.Add(r.InactivityTTL); now.After(inactive) {
		return inactive, true
	}
	// FirstProvenAt, not the current lease. A move and a revival both clear the
	// lease on purpose, so reading it here made a cluster that had served for
	// months lapse the instant it changed address: it could then get no challenge
	// to re-prove with, and its tombstone clock, anchored at this CreatedAt+2h,
	// was already spent, so the next sweep handed its name to the pool. The rule
	// is meant to deny a squatter a label nothing ever served under, and that is
	// exactly what FirstProvenAt records.
	if r.EnforceProof && entry.FirstProvenAt.IsZero() {
		if unproven := entry.CreatedAt.Add(unprovenReservationTTL); now.After(unproven) {
			return unproven, true
		}
	}
	return time.Time{}, false
}

// isReleasableLocked permits reuse after a lapsed label's tombstone expires.
// Labels that have never been proven are released immediately on expiry.
// FirstProvenAt preserves this distinction across moves that clear the current
// proof. Caller holds r.mu.
func (r *Registry) isReleasableLocked(entry *Entry) bool {
	at, lapsed := r.lapsedAtLocked(entry)
	if !lapsed {
		return false
	}
	if entry.FirstProvenAt.IsZero() {
		return true
	}
	return time.Since(at) > r.TombstoneTTL
}

// everProvenLocked checks for a recorded proof timestamp and key, independently
// of lease expiry. A move clears both; FirstProvenAt retains historical proof
// for tombstone decisions. Caller holds r.mu.
func everProvenLocked(entry *Entry) bool {
	return !entry.ProvenAt.IsZero() && entry.ProofKeySPKI != ""
}

// Ping renews a subdomain's last-seen timestamp.
//
// A registration that has already lapsed is refused rather than renewed. Coming
// back from a tombstone goes through Register, which re-establishes the address
// and re-arms the proof; a ping carries neither, so reviving on one would put a
// label back in service without anything having demonstrated control of it.
func (r *Registry) Ping(token string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	subdomain, ok := r.tokens[token]
	if !ok {
		return fmt.Errorf("invalid token")
	}

	entry, ok := r.entries[subdomain]
	if !ok {
		return fmt.Errorf("subdomain not found")
	}
	if r.isExpiredLocked(entry) {
		return fmt.Errorf("registration for %q has lapsed; re-register to bring it back", subdomain)
	}

	entry.LastSeen = time.Now()
	r.dirty = true
	return nil
}

// Cleanup removes all expired entries and clears expired pending/previous
// fingerprints, so the persisted state matches the set PinState enforces. It
// returns the subdomains it removed, so callers can drop per-registration state
// they hold alongside the registry. Call periodically.
func (r *Registry) Cleanup() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	var removed []string
	now := time.Now()
	for subdomain, entry := range r.entries {
		if r.isReleasableLocked(entry) {
			delete(r.tokens, entry.Token)
			delete(r.entries, subdomain)
			removed = append(removed, subdomain)
			continue
		}
		if entry.PendingFingerprint != "" && now.Sub(entry.PendingSince) > pendingPinTTL {
			entry.PendingFingerprint, entry.PendingSince = "", time.Time{}
			r.dirty = true
		}
		if entry.PrevFingerprint != "" && now.Sub(entry.PrevSince) > prevPinTTL {
			entry.PrevFingerprint, entry.PrevSince = "", time.Time{}
			r.dirty = true
		}
	}
	return removed
}

// Count returns the number of registrations currently serving. Tombstoned
// labels are held rather than served, so counting them would overstate the fleet
// wherever this is reported.
func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	live := 0
	for _, entry := range r.entries {
		if !r.isExpiredLocked(entry) {
			live++
		}
	}
	return live
}

func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// --- certificate pin state ---
//
// The gateway proxies *.kipper.run to the cluster over TLS it cannot verify
// against WebPKI (clusters serve a self-signed hop certificate). Instead it
// pins the SPKI SHA-256 the cluster asserts through a token-authenticated
// /register call. The token rides WebPKI-verified TLS to the gateway, so an
// on-path attacker between gateway and cluster can delay a pin from
// activating but can never poison it. The accepted set is strictly capped at
// one current, one pending, and one previous fingerprint.

// fingerprintPattern matches an SPKI SHA-256 in lowercase hex.
var fingerprintPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidFingerprint reports whether fp is a well-formed SPKI SHA-256
// fingerprint (64 lowercase hex chars). Malformed values are rejected before
// they can enter pin state.
func ValidFingerprint(fp string) bool {
	return fingerprintPattern.MatchString(fp)
}

// FingerprintsEqual compares two fingerprint strings in constant time.
func FingerprintsEqual(a, b string) bool {
	return len(a) == len(b) && a != "" &&
		subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// firstPinSettleWindow is how long after the first activation a non-matching
// leaf is still tolerated (see Entry.FirstPinnedAt). Traefik propagates
// dynamic configuration in seconds; two minutes is a generous bound.
const firstPinSettleWindow = 2 * time.Minute

// PinState is the deadline-filtered accepted-fingerprint set for one entry.
type PinState struct {
	Current string
	Pending string
	Prev    string
	// FirstPinnedAt is when the entry first left unpinned grace; zero while
	// unpinned.
	FirstPinnedAt time.Time
}

// InFirstPinSettle reports whether the entry is inside the settle window
// that follows its very first pin activation, during which a non-matching
// leaf is tolerated so a lagging Traefik replica cannot cause a 502.
func (s PinState) InFirstPinSettle() bool {
	return !s.FirstPinnedAt.IsZero() && time.Since(s.FirstPinnedAt) < firstPinSettleWindow
}

// Pinned reports whether an enforced (current) pin exists. Until then the
// entry proxies in unpinned grace.
func (s PinState) Pinned() bool { return s.Current != "" }

// PinState returns the accepted-fingerprint set for a subdomain with expired
// pending/previous values already filtered out.
func (r *Registry) PinState(subdomain string) PinState {
	r.mu.RLock()
	defer r.mu.RUnlock()

	entry, ok := r.entries[subdomain]
	if !ok {
		return PinState{}
	}

	now := time.Now()
	s := PinState{Current: entry.CertFingerprint, FirstPinnedAt: entry.FirstPinnedAt}
	if entry.PendingFingerprint != "" && now.Sub(entry.PendingSince) <= pendingPinTTL {
		s.Pending = entry.PendingFingerprint
	}
	if entry.PrevFingerprint != "" && now.Sub(entry.PrevSince) <= prevPinTTL {
		s.Prev = entry.PrevFingerprint
	}
	return s
}

// tokenMatches compares a presented token against the entry's in constant
// time. Never compare tokens any other way.
func tokenMatches(entry *Entry, token string) bool {
	return token != "" && subtle.ConstantTimeCompare([]byte(entry.Token), []byte(token)) == 1
}

// AssertResult is the outcome of a pre-dial pin assertion.
type AssertResult int

const (
	// AssertInvalidToken: the subdomain is unknown or the token does not match.
	AssertInvalidToken AssertResult = iota
	// AssertActive: the asserted fingerprint is already the enforced pin; any
	// rotation-grace fingerprints were cleared.
	AssertActive
	// AssertNeedsDial: the fingerprint is new — the caller must observe the
	// cluster before it can activate, or park it as pending.
	AssertNeedsDial
)

// AssertPin handles a token-authenticated fingerprint assertion up to the
// point where network observation is needed. Re-asserting the enforced pin
// ends any rotation: pending and previous fingerprints are cleared, so grace
// acceptance lasts no longer than the rotation it bridged.
func (r *Registry) AssertPin(subdomain, token, fp string) AssertResult {
	r.mu.Lock()
	defer r.mu.Unlock()

	entry, ok := r.entries[subdomain]
	if !ok || !tokenMatches(entry, token) {
		return AssertInvalidToken
	}
	if FingerprintsEqual(entry.CertFingerprint, fp) {
		if entry.PendingFingerprint != "" || entry.PrevFingerprint != "" {
			entry.PendingFingerprint, entry.PendingSince = "", time.Time{}
			entry.PrevFingerprint, entry.PrevSince = "", time.Time{}
			r.dirty = true
		}
		return AssertActive
	}
	return AssertNeedsDial
}

// ActivatePin makes fp the enforced pin after the caller observed the cluster
// serving it. The displaced pin stays accepted as previous for the rotation
// window. Returns false when the subdomain or token no longer checks out (the
// entry may have been deregistered during the observation dial).
func (r *Registry) ActivatePin(subdomain, token, fp string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	entry, ok := r.entries[subdomain]
	if !ok || !tokenMatches(entry, token) {
		return false
	}
	r.activateLocked(entry, fp)
	return true
}

// StorePendingPin records a token-asserted fingerprint that observation did
// not confirm. It joins the accepted set (a rotation must not 502 while it
// propagates) but never displaces the enforced pin. A newer assertion from
// the same token replaces an earlier pending value: the cluster is the sole
// authority on what it intends to serve, so latest-wins is the transition
// rule. Returns false when the subdomain or token no longer checks out.
func (r *Registry) StorePendingPin(subdomain, token, fp string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	entry, ok := r.entries[subdomain]
	if !ok || !tokenMatches(entry, token) {
		return false
	}
	entry.PendingFingerprint = fp
	entry.PendingSince = time.Now()
	entry.PinUpdatedAt = time.Now()
	r.dirty = true
	return true
}

// PromoteOnObserve promotes the pending fingerprint after a live handshake
// presented exactly that leaf key. Observation is the proof the cluster
// serves it, so no token is required; the fingerprint itself entered the
// pending slot token-authenticated. Reports whether a promotion happened.
func (r *Registry) PromoteOnObserve(subdomain, observed string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	entry, ok := r.entries[subdomain]
	if !ok {
		return false
	}
	if !FingerprintsEqual(entry.PendingFingerprint, observed) {
		return false
	}
	if time.Since(entry.PendingSince) > pendingPinTTL {
		return false
	}
	r.activateLocked(entry, observed)
	return true
}

// activateLocked installs fp as the current pin. Caller holds r.mu.
// The displaced key becomes PrevFingerprint if it matches the proven key or
// the entry has no proof key yet. Otherwise its grace period ends immediately.
func (r *Registry) activateLocked(entry *Entry, fp string) {
	if FingerprintsEqual(entry.CertFingerprint, fp) {
		return
	}
	switch {
	case entry.CertFingerprint == "":
		entry.FirstPinnedAt = time.Now()
	case entry.ProofKeySPKI == "" || FingerprintsEqual(entry.CertFingerprint, entry.ProofKeySPKI):
		entry.PrevFingerprint = entry.CertFingerprint
		entry.PrevSince = time.Now()
	default:
		entry.PrevFingerprint, entry.PrevSince = "", time.Time{}
	}
	entry.CertFingerprint = fp
	entry.PendingFingerprint, entry.PendingSince = "", time.Time{}
	entry.PinUpdatedAt = time.Now()
	r.dirty = true
}

// UnpinnedSummary reports how many unexpired registrations proxy without an
// enforced pin and the age of the oldest, so a cluster stuck in grace is
// visible without reading per-handshake logs.
func (r *Registry) UnpinnedSummary() (count int, oldest time.Duration) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	now := time.Now()
	for _, entry := range r.entries {
		// isExpiredLocked rather than the inactivity clock alone, so a
		// tombstoned label is left out. It carries no traffic and nothing can
		// pin it, and counting one would hold the cutover audit's
		// wait-for-zero open on a registration that is not serving.
		if r.isExpiredLocked(entry) || entry.CertFingerprint != "" {
			continue
		}
		count++
		if age := now.Sub(entry.CreatedAt); age > oldest {
			oldest = age
		}
	}
	return count, oldest
}

// --- registration proof of possession (B16) ---
//
// A registration proves control of its destination IP when the token holder
// signs a fresh gateway nonce with the private key whose public half the
// gateway observes at IP:443. Echoing the destination's public certificate is
// not enough: the nonce must be signed with the private key. The proof is a
// renewable lease, so a decommissioned or reassigned IP converges to
// suspension rather than staying "proven" forever.

// ErrChallengeUnavailable reports that a token was accepted but no challenge
// could be minted. It is distinct from a refusal on purpose: a caller cannot see
// the difference in the response, and the absence of a challenge is the client's
// evidence that its token was rejected. Collapsing the two tells a cluster its
// name belongs to somebody else because this process could not read random
// bytes — and on a move, after the registration has already been changed.
var ErrChallengeUnavailable = errors.New("could not mint a proof challenge")

// IssueChallenge returns an unexpired challenge or mints a single-use nonce
// for an authenticated, active registration. Check err before accepted:
//
//   - err != nil: nonce creation failed after authentication.
//   - accepted == false: the registration is unknown, expired, or unauthorized.
//   - accepted == true: nonce is the challenge to return.
func (r *Registry) IssueChallenge(subdomain, token string) (string, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	entry, ok := r.entries[subdomain]
	if !ok || !tokenMatches(entry, token) {
		return "", false, nil
	}
	// Proof accrues to a registration that is serving. A lapsed or released one
	// comes back through Register, which re-establishes the address the proof
	// would be about; issuing here would let a cluster sign a challenge, be told
	// it succeeded, and still carry no traffic.
	if r.isExpiredLocked(entry) {
		return "", false, nil
	}
	// Reuse an outstanding unexpired challenge instead of minting a new one on
	// every beat, so concurrent console-api replicas (a rollout surge, or an HA
	// setup) sign the same nonce and converge rather than churning each other's
	// challenge out. It is consumed single-use when one replica proves it; the
	// entry is then proven and the next beat mints a fresh one.
	if entry.ChallengeNonce != "" && time.Now().Before(entry.ChallengeExpiry) {
		return entry.ChallengeNonce, true, nil
	}
	nonce, err := r.newNonce()
	if err != nil {
		return "", true, fmt.Errorf("%w: %v", ErrChallengeUnavailable, err)
	}
	entry.ChallengeNonce = nonce
	entry.ChallengeExpiry = time.Now().Add(challengeTTL)
	r.dirty = true
	return nonce, true, nil
}

// ChallengeMatches reports, in constant time, whether token and nonce match the
// entry's authoritative credentials and its outstanding unexpired challenge.
// The proof handler uses it to reject an uncommittable request before the
// verification dial, without exposing the nonce or comparing it in variable
// time.
func (r *Registry) ChallengeMatches(subdomain, token, nonce string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	entry, ok := r.entries[subdomain]
	if !ok || !tokenMatches(entry, token) {
		return false
	}
	if entry.ChallengeNonce == "" || time.Now().After(entry.ChallengeExpiry) {
		return false
	}
	return nonceMatches(entry.ChallengeNonce, nonce)
}

// RecordProof completes a proof: the caller has verified (out of band) that
// the signature over the issued nonce validates against the key served at the
// entry's IP. It checks the token, that the nonce matches the outstanding
// unexpired challenge, consumes the nonce (single use), and starts a fresh
// lease. spki records which key possession was proven, for audit. Returns
// false when the token or nonce does not check out.
func (r *Registry) RecordProof(subdomain, token, nonce, spki, protocol string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	entry, ok := r.entries[subdomain]
	if !ok || !tokenMatches(entry, token) {
		return false
	}
	// A lapsed registration accrues no proof; see IssueChallenge.
	if r.isExpiredLocked(entry) {
		return false
	}
	if entry.ChallengeNonce == "" || !nonceMatches(entry.ChallengeNonce, nonce) {
		return false
	}
	if time.Now().After(entry.ChallengeExpiry) {
		return false
	}
	now := time.Now()
	if entry.FirstProvenAt.IsZero() {
		entry.FirstProvenAt = now
	}
	entry.ProvenAt = now
	entry.ProofExpiry = now.Add(proofLease)
	entry.ProofKeySPKI = spki
	entry.ProofProtocol = protocol
	// Single use: consume the nonce so a captured challenge cannot be replayed.
	entry.ChallengeNonce = ""
	entry.ChallengeExpiry = time.Time{}
	r.dirty = true
	return true
}

// Challenge returns the outstanding unexpired nonce for a subdomain, or "" —
// so the gateway's proof handler can check a submitted nonce without exposing
// the whole entry. Not token-gated: the nonce is not a secret.
func (r *Registry) Challenge(subdomain string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	entry, ok := r.entries[subdomain]
	if !ok || entry.ChallengeNonce == "" || time.Now().After(entry.ChallengeExpiry) {
		return ""
	}
	return entry.ChallengeNonce
}

// Routable reports whether a subdomain may be proxied under the proof regime:
// it exists, is unexpired, and holds a proof lease that authorises the key the
// data plane accepts. Callers gate routing on this only when proof enforcement
// is on; the method itself is always a truthful read of proof state.
func (r *Registry) Routable(subdomain string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	entry, ok := r.entries[subdomain]
	if !ok || r.isExpiredLocked(entry) {
		return false
	}
	return r.proofValidLocked(entry)
}

// ProofAuthorizes checks that the current proof lease covers the key observed
// on this handshake. Checking the leaf itself protects against pin changes
// between the routing decision and TLS verification.
func (r *Registry) ProofAuthorizes(subdomain, observed string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	entry, ok := r.entries[subdomain]
	if !ok || r.isExpiredLocked(entry) {
		return false
	}
	if !r.leaseCurrentLocked(entry) {
		return false
	}
	return FingerprintsEqual(entry.ProofKeySPKI, observed)
}

// proofValidLocked requires a current proof lease and, when pinned, a pin
// matching the proven key. ProofAuthorizes also checks the key actually served
// on each handshake. Caller holds r.mu.
func (r *Registry) proofValidLocked(entry *Entry) bool {
	if !r.leaseCurrentLocked(entry) {
		return false
	}
	return entry.CertFingerprint == "" || FingerprintsEqual(entry.CertFingerprint, entry.ProofKeySPKI)
}

// leaseCurrentLocked reports whether a proof lease is attributable to a key and
// has not lapsed. A lease with no key authorises nothing under the binding — every
// consumer must agree on that, or the cutover audit would report such an entry as
// proven while its handshakes are refused. Caller holds r.mu.
func (r *Registry) leaseCurrentLocked(entry *Entry) bool {
	return everProvenLocked(entry) && time.Now().Before(entry.ProofExpiry)
}

// ProvenKey returns the SPKI whose possession the subdomain has proven while
// that lease is current, and "" when nothing is currently proven. The proxy mixes
// it into its transport cache key: TLS verification runs once per connection, so
// a pooled connection must not outlive the proof that authorised it.
func (r *Registry) ProvenKey(subdomain string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	entry, ok := r.entries[subdomain]
	if !ok || !r.leaseCurrentLocked(entry) {
		return ""
	}
	return entry.ProofKeySPKI
}

// UnprovenSummary reports how many active registrations are not currently
// routable (never proven, lease-expired, or pinned to a key no current proof
// covers) and the age of the oldest such entry, for the pre-cutover
// zero-unproven audit and /status. It shares Routable's predicate, so the audit
// can never read zero while an entry would be refused at the gate.
func (r *Registry) UnprovenSummary() (count int, oldest time.Duration) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	now := time.Now()
	for _, entry := range r.entries {
		if r.isExpiredLocked(entry) {
			continue
		}
		if r.proofValidLocked(entry) {
			continue
		}
		count++
		if age := now.Sub(entry.CreatedAt); age > oldest {
			oldest = age
		}
	}
	return count, oldest
}

// generateNonce returns a 16-byte hex challenge nonce.
func generateNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// nonceMatches compares an issued nonce against a submitted one in constant
// time, so a submitted nonce cannot be discovered by timing.
func nonceMatches(issued, submitted string) bool {
	return submitted != "" && len(issued) == len(submitted) &&
		subtle.ConstantTimeCompare([]byte(issued), []byte(submitted)) == 1
}
