package registry

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRegisterAndLookup(t *testing.T) {
	r := New()

	entry, _, err := r.Register("myapp", "198.51.100.1", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if entry.Subdomain != "myapp" {
		t.Errorf("expected myapp, got %s", entry.Subdomain)
	}
	if entry.IP != "198.51.100.1" {
		t.Errorf("expected 198.51.100.1, got %s", entry.IP)
	}
	if entry.Token == "" {
		t.Error("expected non-empty token")
	}

	found := r.Lookup("myapp")
	if found == nil {
		t.Fatal("expected to find entry")
	}
	if found.IP != "198.51.100.1" {
		t.Errorf("expected 198.51.100.1, got %s", found.IP)
	}
}

func TestRegisterDuplicateSubdomainReturnsError(t *testing.T) {
	r := New()

	_, _, err := r.Register("myapp", "198.51.100.1", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, _, err = r.Register("myapp", "10.0.0.1", "")
	if err == nil {
		t.Fatal("expected error for duplicate subdomain with different IP")
	}
}

func TestRegisterSameIPIsRenewal(t *testing.T) {
	r := New()

	entry1, _, _ := r.Register("myapp", "198.51.100.1", "")
	entry2, _, err := r.Register("myapp", "198.51.100.1", "")
	if err != nil {
		t.Fatalf("unexpected error on renewal: %v", err)
	}

	if entry1.Token != entry2.Token {
		t.Error("expected same token on renewal")
	}
}

func TestDeregister(t *testing.T) {
	r := New()

	entry, _, _ := r.Register("myapp", "198.51.100.1", "")

	if _, err := r.Deregister(entry.Token); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if r.Lookup("myapp") != nil {
		t.Error("expected nil after deregistration")
	}

	if r.Count() != 0 {
		t.Errorf("expected 0 entries, got %d", r.Count())
	}
}

func TestDeregisterInvalidToken(t *testing.T) {
	r := New()
	if _, err := r.Deregister("bogus-token"); err == nil {
		t.Fatal("expected error for invalid token")
	}
}

func TestLookupReturnsNilForUnknown(t *testing.T) {
	r := New()
	if r.Lookup("nonexistent") != nil {
		t.Error("expected nil for unknown subdomain")
	}
}

func TestLookupReturnsNilForExpired(t *testing.T) {
	r := New()
	r.InactivityTTL = 1 * time.Millisecond

	_, _, _ = r.Register("myapp", "198.51.100.1", "")
	time.Sleep(5 * time.Millisecond)

	if r.Lookup("myapp") != nil {
		t.Error("expected nil for expired subdomain")
	}
}

func TestPingRenewsLastSeen(t *testing.T) {
	r := New()
	r.InactivityTTL = 50 * time.Millisecond

	entry, _, _ := r.Register("myapp", "198.51.100.1", "")
	time.Sleep(30 * time.Millisecond)

	if err := r.Ping(entry.Token); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	time.Sleep(30 * time.Millisecond)

	// Should still be alive because we pinged
	if r.Lookup("myapp") == nil {
		t.Error("expected entry to be alive after ping")
	}
}

func TestPingInvalidToken(t *testing.T) {
	r := New()
	if err := r.Ping("bogus"); err == nil {
		t.Fatal("expected error for invalid token")
	}
}

func TestCleanupRemovesExpired(t *testing.T) {
	r := New()
	r.InactivityTTL = 1 * time.Millisecond

	_, _, _ = r.Register("app1", "10.0.0.1", "")
	_, _, _ = r.Register("app2", "10.0.0.2", "")
	time.Sleep(5 * time.Millisecond)

	removed := len(r.Cleanup())
	if removed != 2 {
		t.Errorf("expected 2 removed, got %d", removed)
	}
	if r.Count() != 0 {
		t.Errorf("expected 0 entries, got %d", r.Count())
	}
}

func TestCount(t *testing.T) {
	r := New()
	if r.Count() != 0 {
		t.Errorf("expected 0, got %d", r.Count())
	}

	_, _, _ = r.Register("a", "203.0.113.1", "")
	_, _, _ = r.Register("b", "203.0.113.2", "")
	if r.Count() != 2 {
		t.Errorf("expected 2, got %d", r.Count())
	}
}

// --- pin state ---

var (
	testFPA = strings.Repeat("a", 64)
	testFPB = strings.Repeat("b", 64)
)

func TestAssertPin(t *testing.T) {
	r := New()
	entry, _, _ := r.Register("myapp", "203.0.113.1", "")

	if got := r.AssertPin("myapp", "wrong", testFPA); got != AssertInvalidToken {
		t.Errorf("bad token: expected AssertInvalidToken, got %v", got)
	}
	if got := r.AssertPin("missing", entry.Token, testFPA); got != AssertInvalidToken {
		t.Errorf("unknown subdomain: expected AssertInvalidToken, got %v", got)
	}
	if got := r.AssertPin("myapp", entry.Token, testFPA); got != AssertNeedsDial {
		t.Errorf("new fingerprint: expected AssertNeedsDial, got %v", got)
	}
	if !r.ActivatePin("myapp", entry.Token, testFPA) {
		t.Fatal("activate")
	}
	if got := r.AssertPin("myapp", entry.Token, testFPA); got != AssertActive {
		t.Errorf("enforced fingerprint: expected AssertActive, got %v", got)
	}
}

func TestAssertPinEndsRotationGrace(t *testing.T) {
	r := New()
	entry, _, _ := r.Register("myapp", "203.0.113.1", "")
	if !r.ActivatePin("myapp", entry.Token, testFPA) || !r.ActivatePin("myapp", entry.Token, testFPB) {
		t.Fatal("activate")
	}
	if !r.StorePendingPin("myapp", entry.Token, strings.Repeat("c", 64)) {
		t.Fatal("store pending")
	}
	if got := r.AssertPin("myapp", entry.Token, testFPB); got != AssertActive {
		t.Fatalf("expected AssertActive, got %v", got)
	}
	if s := r.PinState("myapp"); s.Current != testFPB || s.Prev != "" || s.Pending != "" {
		t.Errorf("re-asserting the enforced pin must clear grace state, got %+v", s)
	}
}

func TestPromoteOnObserve(t *testing.T) {
	r := New()
	entry, _, _ := r.Register("myapp", "203.0.113.1", "")
	if !r.StorePendingPin("myapp", entry.Token, testFPA) {
		t.Fatal("store pending")
	}
	if r.PromoteOnObserve("myapp", testFPB) {
		t.Error("observing a non-pending fingerprint must not promote")
	}
	if !r.PromoteOnObserve("myapp", testFPA) {
		t.Error("observing the pending fingerprint must promote")
	}
	if s := r.PinState("myapp"); s.Current != testFPA || s.Pending != "" {
		t.Errorf("expected promotion to current, got %+v", s)
	}
	if r.PromoteOnObserve("myapp", testFPA) {
		t.Error("a second observation of the same fingerprint must be a no-op")
	}
}

func TestPinStateSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	r := New()
	entry, _, _ := r.Register("myapp", "203.0.113.1", "")
	if !r.ActivatePin("myapp", entry.Token, testFPA) || !r.ActivatePin("myapp", entry.Token, testFPB) {
		t.Fatal("activate")
	}
	if err := r.SaveTo(path); err != nil {
		t.Fatalf("save: %v", err)
	}

	restarted := New()
	if err := restarted.LoadFrom(path); err != nil {
		t.Fatalf("load: %v", err)
	}
	s := restarted.PinState("myapp")
	if s.Current != testFPB || s.Prev != testFPA {
		t.Errorf("pin state must survive a restart, got %+v", s)
	}
}

func TestPinTransitionsMarkDirty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	r := New()
	entry, _, _ := r.Register("myapp", "203.0.113.1", "")
	if err := r.SaveTo(path); err != nil {
		t.Fatalf("save: %v", err)
	}

	steps := []struct {
		name string
		fn   func() bool
	}{
		{"StorePendingPin", func() bool { return r.StorePendingPin("myapp", entry.Token, testFPA) }},
		{"PromoteOnObserve", func() bool { return r.PromoteOnObserve("myapp", testFPA) }},
		{"ActivatePin", func() bool { return r.ActivatePin("myapp", entry.Token, testFPB) }},
	}
	for _, step := range steps {
		if !step.fn() {
			t.Fatalf("%s failed", step.name)
		}
		wrote, err := r.FlushIfDirty(path)
		if err != nil {
			t.Fatalf("%s: flush: %v", step.name, err)
		}
		if !wrote {
			t.Errorf("%s must mark the registry dirty", step.name)
		}
	}
}

func TestExpiredPendingPinFiltered(t *testing.T) {
	// Craft persisted state whose pending assertion is past its deadline; the
	// deadline caps how long a token-asserted-but-unobserved key stays in the
	// accepted set.
	path := filepath.Join(t.TempDir(), "registry.json")
	old := time.Now().Add(-25 * time.Hour).UTC().Format(time.RFC3339)
	now := time.Now().UTC().Format(time.RFC3339)
	snapshot := `{"entries":[{"subdomain":"myapp","ip":"203.0.113.1","token":"tok","created_at":"` + now +
		`","last_seen":"` + now + `","pending_fingerprint":"` + testFPA + `","pending_since":"` + old +
		`","prev_fingerprint":"` + testFPB + `","prev_since":"` + old + `"}]}`
	if err := os.WriteFile(path, []byte(snapshot), 0o600); err != nil {
		t.Fatal(err)
	}

	r := New()
	if err := r.LoadFrom(path); err != nil {
		t.Fatalf("load: %v", err)
	}
	if s := r.PinState("myapp"); s.Pending != "" || s.Prev != "" {
		t.Errorf("expired grace fingerprints must be filtered, got %+v", s)
	}
	if r.PromoteOnObserve("myapp", testFPA) {
		t.Error("an expired pending fingerprint must not promote")
	}
	r.Cleanup()
	if wrote, err := r.FlushIfDirty(path); err != nil || !wrote {
		t.Errorf("cleanup must clear expired fingerprints and mark dirty (wrote=%v err=%v)", wrote, err)
	}
}

func TestLookupReturnsCopy(t *testing.T) {
	r := New()
	entry, _, _ := r.Register("myapp", "203.0.113.1", "")

	before := r.Lookup("myapp")
	if !r.ActivatePin("myapp", entry.Token, testFPA) {
		t.Fatal("activate")
	}
	if before.CertFingerprint != "" {
		t.Error("Lookup must return a copy, not the live entry")
	}
	after := r.Lookup("myapp")
	after.CertFingerprint = "mutated"
	if r.PinState("myapp").Current != testFPA {
		t.Error("mutating a Lookup result must not affect registry state")
	}
}

func TestUnpinnedSummary(t *testing.T) {
	r := New()
	a, _, _ := r.Register("pinned", "203.0.113.1", "")
	_, _, _ = r.Register("bare", "203.0.113.2", "")
	if !r.ActivatePin("pinned", a.Token, testFPA) {
		t.Fatal("activate")
	}

	count, oldest := r.UnpinnedSummary()
	if count != 1 {
		t.Errorf("expected 1 unpinned registration, got %d", count)
	}
	if oldest < 0 {
		t.Errorf("expected a non-negative age, got %v", oldest)
	}
}

func TestValidFingerprint(t *testing.T) {
	valid := []string{testFPA, strings.Repeat("0", 64), "0123456789abcdef" + strings.Repeat("f", 48)}
	for _, fp := range valid {
		if !ValidFingerprint(fp) {
			t.Errorf("expected %q valid", fp)
		}
	}
	invalid := []string{"", "short", strings.Repeat("A", 64), strings.Repeat("g", 64), strings.Repeat("a", 63), strings.Repeat("a", 65)}
	for _, fp := range invalid {
		if ValidFingerprint(fp) {
			t.Errorf("expected %q invalid", fp)
		}
	}
}

func TestFirstPinnedAtSetOnceAndSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	r := New()
	entry, _, _ := r.Register("myapp", "203.0.113.1", "")

	if r.PinState("myapp").InFirstPinSettle() {
		t.Error("an unpinned entry is not in the settle window")
	}
	if !r.ActivatePin("myapp", entry.Token, testFPA) {
		t.Fatal("activate")
	}
	first := r.PinState("myapp").FirstPinnedAt
	if first.IsZero() || !r.PinState("myapp").InFirstPinSettle() {
		t.Error("the first activation must start the settle window")
	}

	// A rotation keeps the original first-pin timestamp.
	if !r.ActivatePin("myapp", entry.Token, testFPB) {
		t.Fatal("rotate")
	}
	if got := r.PinState("myapp").FirstPinnedAt; !got.Equal(first) {
		t.Errorf("rotation must not reset FirstPinnedAt: %v → %v", first, got)
	}

	if err := r.SaveTo(path); err != nil {
		t.Fatal(err)
	}
	restarted := New()
	if err := restarted.LoadFrom(path); err != nil {
		t.Fatal(err)
	}
	if got := restarted.PinState("myapp").FirstPinnedAt; !got.Equal(first) {
		t.Errorf("FirstPinnedAt must survive a restart: %v → %v", first, got)
	}
}

// --- proof of possession (B16) ---

func TestIssueAndRecordProof(t *testing.T) {
	r := New()
	entry, _, _ := r.Register("myapp", "203.0.113.1", "")

	if _, ok, _ := r.IssueChallenge("myapp", "wrong"); ok {
		t.Error("a bad token must not get a challenge")
	}
	nonce, ok, _ := r.IssueChallenge("myapp", entry.Token)
	if !ok || nonce == "" {
		t.Fatal("expected a challenge for the token holder")
	}
	if r.Challenge("myapp") != nonce {
		t.Error("the outstanding challenge must match the issued nonce")
	}

	if r.RecordProof("myapp", "wrong", nonce, "spki", "kipper-hop-proof-v1") {
		t.Error("a bad token must not record a proof")
	}
	if r.RecordProof("myapp", entry.Token, "wrongnonce", "spki", "kipper-hop-proof-v1") {
		t.Error("a wrong nonce must not record a proof")
	}
	if !r.RecordProof("myapp", entry.Token, nonce, "spkiabc", "kipper-hop-proof-v1") {
		t.Fatal("a valid token+nonce must record a proof")
	}
	if !r.Routable("myapp") {
		t.Error("a freshly proven entry must be routable")
	}
	// Single use: the nonce is consumed.
	if r.Challenge("myapp") != "" {
		t.Error("the nonce must be consumed after a successful proof")
	}
	if r.RecordProof("myapp", entry.Token, nonce, "spki", "kipper-hop-proof-v1") {
		t.Error("a consumed nonce must not record a second proof (replay)")
	}
}

func TestProofRejectsExpiredChallenge(t *testing.T) {
	// Craft an entry whose outstanding challenge is already past its deadline.
	path := filepath.Join(t.TempDir(), "registry.json")
	old := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	now := time.Now().UTC().Format(time.RFC3339)
	snap := `{"entries":[{"subdomain":"myapp","ip":"203.0.113.1","token":"tok","created_at":"` + now +
		`","last_seen":"` + now + `","challenge_nonce":"abcd","challenge_expiry":"` + old + `"}]}`
	if err := os.WriteFile(path, []byte(snap), 0o600); err != nil {
		t.Fatal(err)
	}
	r := New()
	if err := r.LoadFrom(path); err != nil {
		t.Fatal(err)
	}
	if r.Challenge("myapp") != "" {
		t.Error("an expired challenge must not be reported as outstanding")
	}
	if r.RecordProof("myapp", "tok", "abcd", "spki", "kipper-hop-proof-v1") {
		t.Error("an expired challenge must not record a proof")
	}
}

func TestRoutableRequiresUnexpiredLease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	now := time.Now().UTC().Format(time.RFC3339)
	expired := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)

	load := func(provenAt, expiry string) *Registry {
		snap := `{"entries":[{"subdomain":"myapp","ip":"203.0.113.1","token":"tok","created_at":"` + now +
			`","last_seen":"` + now + `","proven_at":"` + provenAt + `","proof_expiry":"` + expiry +
			`","proof_key_spki":"` + testFPA + `"}]}`
		if err := os.WriteFile(path, []byte(snap), 0o600); err != nil {
			t.Fatal(err)
		}
		r := New()
		if err := r.LoadFrom(path); err != nil {
			t.Fatal(err)
		}
		return r
	}

	if load(now, future).Routable("myapp") != true {
		t.Error("a proven, unexpired-lease entry must be routable")
	}
	if load(now, expired).Routable("myapp") != false {
		t.Error("a lease-expired entry must not be routable (converges to suspension)")
	}
	// Never proven.
	if New2Proven(t).Routable("myapp") != false {
		t.Error("a never-proven entry must not be routable")
	}
}

// New2Proven returns a registry with one registered-but-never-proven entry.
func New2Proven(t *testing.T) *Registry {
	t.Helper()
	r := New()
	_, _, _ = r.Register("myapp", "203.0.113.1", "")
	return r
}

func TestNeverProvenReleasedOnlyWhenEnforcing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	oldCreate := time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339)
	snap := `{"entries":[{"subdomain":"squat","ip":"203.0.113.1","token":"tok","created_at":"` + oldCreate +
		`","last_seen":"` + oldCreate + `"}]}`
	if err := os.WriteFile(path, []byte(snap), 0o600); err != nil {
		t.Fatal(err)
	}

	// Transition mode (EnforceProof off): a never-proven entry older than the
	// short TTL is NOT released — it keeps the full 30-day retention.
	r := New()
	if err := r.LoadFrom(path); err != nil {
		t.Fatal(err)
	}
	if r.Lookup("squat") == nil {
		t.Error("with enforcement off, a never-proven entry must not be released early")
	}

	// Enforcing: the same entry is released after the short reservation TTL.
	r2 := New()
	r2.EnforceProof = true
	if err := r2.LoadFrom(path); err != nil {
		t.Fatal(err)
	}
	if r2.Lookup("squat") != nil {
		t.Error("with enforcement on, a never-proven entry past the TTL must be released")
	}
	if removed := len(r2.Cleanup()); removed != 1 {
		t.Errorf("cleanup should delete the released squat entry, removed %d", removed)
	}
}

func TestProofRecordSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	r := New()
	entry, _, _ := r.Register("myapp", "203.0.113.1", "")
	nonce, _, _ := r.IssueChallenge("myapp", entry.Token)
	if !r.RecordProof("myapp", entry.Token, nonce, "spki-xyz", "kipper-hop-proof-v1") {
		t.Fatal("record proof")
	}
	if err := r.SaveTo(path); err != nil {
		t.Fatal(err)
	}

	restarted := New()
	if err := restarted.LoadFrom(path); err != nil {
		t.Fatal(err)
	}
	if !restarted.Routable("myapp") {
		t.Error("the proof lease must survive a restart, so the fleet doesn't drop to unproven")
	}
}

// proveKey records a proof of possession of spki for an already-registered entry.
func proveKey(t *testing.T, r *Registry, entry *Entry, spki string) {
	t.Helper()
	nonce, ok, _ := r.IssueChallenge(entry.Subdomain, entry.Token)
	if !ok {
		t.Fatal("issue challenge")
	}
	if !r.RecordProof(entry.Subdomain, entry.Token, nonce, spki, "kipper-hop-proof-v1") {
		t.Fatal("record proof")
	}
}

// The lease authorises the key it was obtained for, nothing else. A token holder
// who parks their own key and gets it observed on the wire must not inherit the
// proven key's remaining lease — that is interception with the token plus an
// on-path position, and no hop private key (EOD Critical, Codex Finding 1).
func TestRoutableRequiresProofForThePinnedKey(t *testing.T) {
	r := New()
	entry, _, _ := r.Register("myapp", "203.0.113.1", "")
	if !r.ActivatePin("myapp", entry.Token, testFPA) {
		t.Fatal("activate")
	}
	proveKey(t, r, entry, testFPA)
	if !r.Routable("myapp") {
		t.Fatal("a cluster pinned to the key it proved must be routable")
	}

	// Attacker key: token-asserted, then promoted by observation on the path.
	if !r.StorePendingPin("myapp", entry.Token, testFPB) {
		t.Fatal("store pending")
	}
	if r.Routable("myapp") != true {
		t.Error("a parked pending key must not by itself suspend routing on the proven pin")
	}
	if !r.PromoteOnObserve("myapp", testFPB) {
		t.Fatal("promote")
	}
	if r.Routable("myapp") {
		t.Error("a pin promoted onto a key with no proof must not route on the previous key's lease")
	}
	// The proven key keeps its rotation grace, so the real cluster still serves.
	if s := r.PinState("myapp"); s.Current != testFPB || s.Prev != testFPA {
		t.Errorf("expected the proven key to stay accepted as previous, got %+v", s)
	}

	// The cluster pins its own key back: routable again on the same lease, and
	// the attacker's displaced key loses acceptance instead of lingering for the
	// whole previous-pin window.
	if !r.ActivatePin("myapp", entry.Token, testFPA) {
		t.Fatal("re-activate")
	}
	if !r.Routable("myapp") {
		t.Error("pinning the proven key back must restore routing")
	}
	if s := r.PinState("myapp"); s.Prev != "" {
		t.Errorf("an unproven displaced key must not keep rotation grace, got %+v", s)
	}
}

// The data plane authorises the key it observes, not the registry's pin: the pin
// set accepts several fingerprints, and a pin change can land between the routing
// gate and the handshake.
func TestProofAuthorizesOnlyTheProvenKey(t *testing.T) {
	r := New()
	entry, _, _ := r.Register("myapp", "203.0.113.1", "")

	if r.ProofAuthorizes("myapp", testFPA) {
		t.Error("a never-proven entry must authorise no key")
	}
	proveKey(t, r, entry, testFPA)
	if !r.ProofAuthorizes("myapp", testFPA) {
		t.Error("the proven key must be authorised")
	}
	for _, observed := range []string{testFPB, "", strings.Repeat("c", 64)} {
		if r.ProofAuthorizes("myapp", observed) {
			t.Errorf("key %q must not be authorised by a proof for another key", observed)
		}
	}
	if r.ProofAuthorizes("ghost", testFPA) {
		t.Error("an unknown subdomain must authorise nothing")
	}

	// A lapsed lease authorises nothing, so a decommissioned IP converges to
	// refusing handshakes and not just to refusing the route.
	expired := New()
	expired.entries["myapp"] = &Entry{
		Subdomain: "myapp", IP: "203.0.113.1", Token: "tok",
		CreatedAt: time.Now().Add(-8 * 24 * time.Hour), LastSeen: time.Now(),
		ProvenAt: time.Now().Add(-8 * 24 * time.Hour), ProofExpiry: time.Now().Add(-time.Hour),
		ProofKeySPKI: testFPA,
	}
	if expired.ProofAuthorizes("myapp", testFPA) {
		t.Error("an expired lease must authorise nothing")
	}
}

// A real key rotation re-proves the new key before it routes. The beat that
// activates the new pin also proves it (/register then /proof), so the gap is one
// round trip; a data-plane promotion between beats widens it to one beat.
func TestRotationRoutesOnlyAfterReProof(t *testing.T) {
	r := New()
	entry, _, _ := r.Register("myapp", "203.0.113.1", "")
	if !r.ActivatePin("myapp", entry.Token, testFPA) {
		t.Fatal("activate")
	}
	proveKey(t, r, entry, testFPA)

	if !r.ActivatePin("myapp", entry.Token, testFPB) {
		t.Fatal("rotate")
	}
	if r.Routable("myapp") {
		t.Error("a rotated pin must not route on the old key's lease")
	}
	count, _ := r.UnprovenSummary()
	if count != 1 {
		t.Errorf("the cutover audit must see the mismatch as unproven, got %d", count)
	}

	proveKey(t, r, entry, testFPB)
	if !r.Routable("myapp") {
		t.Error("re-proving the rotated key must restore routing")
	}
	if s := r.PinState("myapp"); s.Prev != testFPA {
		t.Errorf("the pre-rotation key must stay accepted while propagation completes, got %+v", s)
	}
}

// Before a registration has ever proved anything (transition mode), pin rotation
// keeps B5's unconditional grace: there is no proof regime to bind to yet and a
// lagging Traefik replica must not 502.
func TestRotationGracePreservedWithoutAProofRecord(t *testing.T) {
	r := New()
	entry, _, _ := r.Register("myapp", "203.0.113.1", "")
	if !r.ActivatePin("myapp", entry.Token, testFPA) || !r.ActivatePin("myapp", entry.Token, testFPB) {
		t.Fatal("activate")
	}
	if s := r.PinState("myapp"); s.Current != testFPB || s.Prev != testFPA {
		t.Errorf("a never-proven entry keeps unconditional rotation grace, got %+v", s)
	}
}

// An unpinned hop is in B5 grace: there is no key to bind the lease to, so a
// proven entry still routes.
func TestRoutableWhileUnpinned(t *testing.T) {
	r := New()
	entry, _, _ := r.Register("myapp", "203.0.113.1", "")
	proveKey(t, r, entry, testFPA)
	if !r.Routable("myapp") {
		t.Error("a proven but not yet pinned entry must route (unpinned grace)")
	}
}

// A lease persisted without the key it covers authorises nothing: it cannot be
// bound to a served key, so it fails closed at the gate, at the handshake, and in
// the cutover audit alike — an audit that called such an entry proven would send
// the fleet into enforcement with clusters that cannot complete a handshake.
func TestLeaseWithoutAProvenKeyAuthorisesNothing(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339)
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)

	for _, pin := range []string{testFPA, ""} {
		path := filepath.Join(t.TempDir(), "registry.json")
		snap := `{"entries":[{"subdomain":"myapp","ip":"203.0.113.1","token":"tok","created_at":"` + now +
			`","last_seen":"` + now + `","cert_fingerprint":"` + pin +
			`","proven_at":"` + now + `","proof_expiry":"` + future + `"}]}`
		if err := os.WriteFile(path, []byte(snap), 0o600); err != nil {
			t.Fatal(err)
		}
		r := New()
		if err := r.LoadFrom(path); err != nil {
			t.Fatal(err)
		}
		label := "pinned"
		if pin == "" {
			label = "unpinned"
		}
		if r.Routable("myapp") {
			t.Errorf("%s: an entry whose lease names no key must not route", label)
		}
		if r.ProofAuthorizes("myapp", testFPA) || r.ProvenKey("myapp") != "" {
			t.Errorf("%s: an entry whose lease names no key must authorise no handshake", label)
		}
		if count, _ := r.UnprovenSummary(); count != 1 {
			t.Errorf("%s: the cutover audit must count it as unproven, got %d", label, count)
		}
	}

	// The reservation release reads a lease the same way: an entry that never
	// proved anything attributable does not hold its label for the full retention
	// window once enforcement is on.
	path := filepath.Join(t.TempDir(), "registry.json")
	old := time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339)
	snap := `{"entries":[{"subdomain":"myapp","ip":"203.0.113.1","token":"tok","created_at":"` + old +
		`","last_seen":"` + now + `","proven_at":"` + old + `","proof_expiry":"` + future + `"}]}`
	if err := os.WriteFile(path, []byte(snap), 0o600); err != nil {
		t.Fatal(err)
	}
	enforcing := New()
	enforcing.EnforceProof = true
	if err := enforcing.LoadFrom(path); err != nil {
		t.Fatal(err)
	}
	if enforcing.Lookup("myapp") != nil {
		t.Error("an unattributable lease must not reserve the label past the short TTL")
	}
	if removed := len(enforcing.Cleanup()); removed != 1 {
		t.Errorf("cleanup must release it, removed %d", removed)
	}
}

func TestUnprovenSummary(t *testing.T) {
	r := New()
	a, _, _ := r.Register("proven", "203.0.113.1", "")
	_, _, _ = r.Register("unproven", "203.0.113.2", "")
	nonce, _, _ := r.IssueChallenge("proven", a.Token)
	if !r.RecordProof("proven", a.Token, nonce, "spki", "kipper-hop-proof-v1") {
		t.Fatal("record proof")
	}

	count, oldest := r.UnprovenSummary()
	if count != 1 {
		t.Errorf("expected 1 unproven registration, got %d", count)
	}
	if oldest < 0 {
		t.Errorf("expected a non-negative oldest age, got %v", oldest)
	}
}

func TestIssueChallengeReusesOutstanding(t *testing.T) {
	r := New()
	entry, _, _ := r.Register("myapp", "203.0.113.1", "")

	// Concurrent replicas must get the SAME outstanding nonce, not churn each
	// other's out, so they converge instead of endlessly re-issuing.
	n1, _, _ := r.IssueChallenge("myapp", entry.Token)
	n2, _, _ := r.IssueChallenge("myapp", entry.Token)
	if n1 == "" || n1 != n2 {
		t.Errorf("repeated challenges must reuse the outstanding nonce: %q vs %q", n1, n2)
	}

	// After one replica proves (consuming the nonce), the next issue mints a
	// fresh one.
	if !r.RecordProof("myapp", entry.Token, n1, "spki", "kipper-hop-proof-v1") {
		t.Fatal("record proof")
	}
	n3, _, _ := r.IssueChallenge("myapp", entry.Token)
	if n3 == "" || n3 == n1 {
		t.Errorf("a fresh challenge must be minted after the previous was consumed, got %q", n3)
	}
}

func TestChallengeMatches(t *testing.T) {
	r := New()
	entry, _, _ := r.Register("myapp", "203.0.113.1", "")
	nonce, _, _ := r.IssueChallenge("myapp", entry.Token)

	if !r.ChallengeMatches("myapp", entry.Token, nonce) {
		t.Error("the right token and nonce must match")
	}
	if r.ChallengeMatches("myapp", "wrong-token", nonce) {
		t.Error("a wrong token must not match")
	}
	if r.ChallengeMatches("myapp", entry.Token, "wrong-nonce") {
		t.Error("a wrong nonce must not match")
	}
	if r.ChallengeMatches("ghost", entry.Token, nonce) {
		t.Error("an unknown subdomain must not match")
	}
}

func TestRecordProofStoresProtocol(t *testing.T) {
	r := New()
	entry, _, _ := r.Register("myapp", "203.0.113.1", "")
	nonce, _, _ := r.IssueChallenge("myapp", entry.Token)
	if !r.RecordProof("myapp", entry.Token, nonce, "spki", "kipper-hop-proof-v1") {
		t.Fatal("record proof")
	}
	if got := r.Lookup("myapp").ProofProtocol; got != "kipper-hop-proof-v1" {
		t.Errorf("the proof record must persist the protocol version, got %q", got)
	}
}

func TestConcurrentReplicasConverge(t *testing.T) {
	r := New()
	entry, _, _ := r.Register("myapp", "203.0.113.1", "")

	// Two console-api replicas repeatedly issue and prove a challenge. Because
	// IssueChallenge reuses the outstanding nonce, they never churn each other
	// out and the entry converges to proven — validated under -race.
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if nonce, ok, _ := r.IssueChallenge("myapp", entry.Token); ok {
					r.RecordProof("myapp", entry.Token, nonce, "spki", "kipper-hop-proof-v1")
				}
			}
		}()
	}
	wg.Wait()

	if !r.Routable("myapp") {
		t.Error("concurrent replicas must converge to a proven, routable entry")
	}
}

// Anyone can read a label and an address from public DNS. If an anonymous
// request naming both refreshed the inactivity clock, whoever learned the pair
// could hold the label forever and keep it from being released — including
// after the real cluster is gone.
func TestAnonymousRegisterDoesNotRefreshTheInactivityClock(t *testing.T) {
	r := New()
	entry, outcome, err := r.Register("myapp", "203.0.113.1", "")
	if err != nil || outcome != Created {
		t.Fatalf("create: outcome=%v err=%v", outcome, err)
	}
	stale := time.Now().Add(-20 * 24 * time.Hour)
	entry.LastSeen = stale

	got, outcome, err := r.Register("myapp", "203.0.113.1", "")
	if err != nil {
		t.Fatalf("an anonymous request for an existing registration must be answered, got %v", err)
	}
	if outcome != Unauthenticated {
		t.Errorf("outcome = %v, want Unauthenticated", outcome)
	}
	if !got.LastSeen.Equal(stale) {
		t.Error("an anonymous request refreshed the inactivity clock, so a squatter can hold the label")
	}
}

// The token holder is the cluster, and its heartbeat is what should keep the
// registration alive.
func TestTokenRenewalRefreshesTheInactivityClock(t *testing.T) {
	r := New()
	entry, _, err := r.Register("myapp", "203.0.113.1", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	token := entry.Token
	stale := time.Now().Add(-20 * 24 * time.Hour)
	entry.LastSeen = stale

	got, outcome, err := r.Register("myapp", "203.0.113.1", token)
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	if outcome != Renewed {
		t.Errorf("outcome = %v, want Renewed", outcome)
	}
	if !got.LastSeen.After(stale) {
		t.Error("a token-authenticated renewal must refresh the inactivity clock")
	}
	if got.Token != token {
		t.Error("a renewal must not rotate the token")
	}
}

// A cluster whose address changes could not recover: the register returned 409
// forever and the heartbeat just backed off, so it stayed unroutable until the
// entry aged out. The token holder may move.
func TestTokenAuthenticatedMoveUpdatesTheAddress(t *testing.T) {
	r := New()
	entry, _, err := r.Register("myapp", "203.0.113.1", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	token := entry.Token

	got, outcome, err := r.Register("myapp", "198.51.100.7", token)
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	if outcome != Moved {
		t.Errorf("outcome = %v, want Moved", outcome)
	}
	if got.IP != "198.51.100.7" {
		t.Errorf("address is %q, want the new one", got.IP)
	}
	if r.Lookup("myapp").IP != "198.51.100.7" {
		t.Error("the lookup path still resolves the old address")
	}
}

// A pin says which key is served at an address and a proof says who held it.
// Neither is true of a different machine, so a move must drop both: carrying
// them would let a registration inherit a proof made for another host, which is
// the property the proof exists to establish.
func TestMoveClearsPinAndProof(t *testing.T) {
	r := New()
	r.EnforceProof = true
	entry, _, err := r.Register("myapp", "203.0.113.1", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	token := entry.Token

	const fp = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if !r.ActivatePin("myapp", token, fp) {
		t.Fatal("activating the pin failed")
	}
	nonce, ok, _ := r.IssueChallenge("myapp", token)
	if !ok {
		t.Fatal("issuing a challenge failed")
	}
	if !r.RecordProof("myapp", token, nonce, fp, "test") {
		t.Fatal("recording the proof failed")
	}
	if !r.Routable("myapp") {
		t.Fatal("the entry should be routable once proven")
	}

	if _, outcome, err := r.Register("myapp", "198.51.100.7", token); err != nil || outcome != Moved {
		t.Fatalf("move: outcome=%v err=%v", outcome, err)
	}

	moved := r.Lookup("myapp")
	if moved.CertFingerprint != "" || moved.PendingFingerprint != "" || moved.PrevFingerprint != "" {
		t.Error("pin state survived a move, so the new address inherits a key it never served")
	}
	if !moved.FirstPinnedAt.IsZero() {
		t.Error("the settle window survived a move")
	}
	if moved.ProofKeySPKI != "" || !moved.ProvenAt.IsZero() || !moved.ProofExpiry.IsZero() {
		t.Error("proof state survived a move, so the new address inherits possession it never demonstrated")
	}
	if moved.ChallengeNonce != "" {
		t.Error("a challenge issued to the old address survived the move")
	}
	if r.Routable("myapp") {
		t.Error("a moved registration routes without re-proving possession at its new address")
	}
	if r.ProvenKey("myapp") != "" {
		t.Error("the proven key survived the move")
	}
}

// Without the token a different address is still someone else's registration.
func TestAnonymousMoveIsStillRefused(t *testing.T) {
	r := New()
	if _, _, err := r.Register("myapp", "203.0.113.1", ""); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, err := r.Register("myapp", "198.51.100.7", ""); err == nil {
		t.Error("an anonymous request must not move a registration to another address")
	}
	if _, _, err := r.Register("myapp", "198.51.100.7", "wrong-token"); err == nil {
		t.Error("a wrong token must not move a registration")
	}
	if r.Lookup("myapp").IP != "203.0.113.1" {
		t.Error("a refused move changed the address anyway")
	}
}

// --- tombstones ---
//
// A lapsed label is not free the moment it stops serving. The gateway
// terminates TLS for *.kipper.run with its own wildcard, so a stranger who
// picks up an abandoned name serves the previous operator's published links,
// bookmarked console URL and OIDC issuer host behind a valid padlock. Taking
// "lab" takes console--lab, dex--lab and every app route with it.

// proven returns a registry holding one entry that has completed a proof, which
// is what earns a label its tombstone.
func proven(t *testing.T) (*Registry, string) {
	t.Helper()
	const (
		subdomain = "lab"
		ip        = "203.0.113.1"
	)
	r := New()
	entry, _, err := r.Register(subdomain, ip, "")
	if err != nil {
		t.Fatalf("registering: %v", err)
	}
	nonce, ok, err := r.IssueChallenge(subdomain, entry.Token)
	if err != nil || !ok {
		t.Fatalf("issuing a challenge: ok=%v err=%v", ok, err)
	}
	if !r.RecordProof(subdomain, entry.Token, nonce, testFPA, "kipper-hop-proof-v1") {
		t.Fatal("recording the proof failed")
	}
	return r, entry.Token
}

func TestLapsedLabelStopsRoutingButIsNotFree(t *testing.T) {
	r, _ := proven(t)
	r.InactivityTTL = 1 * time.Millisecond
	time.Sleep(5 * time.Millisecond)

	if r.Lookup("lab") != nil {
		t.Error("a lapsed label must stop routing")
	}
	if _, _, err := r.Register("lab", "203.0.113.99", ""); !errors.Is(err, ErrSubdomainTaken) {
		t.Errorf("a stranger claiming a lapsed label must be refused, got %v", err)
	}
	if removed := len(r.Cleanup()); removed != 0 {
		t.Errorf("cleanup must hold a tombstoned label, removed %d", removed)
	}
}

func TestTokenHolderRevivesALapsedLabel(t *testing.T) {
	r, token := proven(t)
	r.InactivityTTL = 1 * time.Millisecond
	time.Sleep(5 * time.Millisecond)

	if _, _, err := r.Register("lab", "203.0.113.1", token); err != nil {
		t.Fatalf("the token holder must be able to revive its own label: %v", err)
	}
	if r.Lookup("lab") == nil {
		t.Error("a revived label must route again")
	}
}

func TestTombstoneLapsesAndFreesTheLabel(t *testing.T) {
	r, _ := proven(t)
	r.InactivityTTL = 1 * time.Millisecond
	r.TombstoneTTL = 1 * time.Millisecond
	time.Sleep(10 * time.Millisecond)

	if removed := len(r.Cleanup()); removed != 1 {
		t.Errorf("a tombstone past its window must be pruned, removed %d", removed)
	}
	entry, outcome, err := r.Register("lab", "203.0.113.99", "")
	if err != nil {
		t.Fatalf("a freed label must be registrable: %v", err)
	}
	if outcome != Created {
		t.Errorf("outcome = %v, want Created", outcome)
	}
	if entry.IP != "203.0.113.99" {
		t.Errorf("IP = %q, want the new holder's address", entry.IP)
	}
}

// The unproven reservation exists to stop a squatter parking a label it never
// serves. A tombstone on top of it would hand that squatter the window it was
// written to deny, so a label earns its tombstone by having been proven once.
func TestNeverProvenLabelEarnsNoTombstone(t *testing.T) {
	r := New()
	_, _, _ = r.Register("squat", "203.0.113.1", "")
	r.InactivityTTL = 1 * time.Millisecond
	time.Sleep(5 * time.Millisecond)

	if removed := len(r.Cleanup()); removed != 1 {
		t.Errorf("a never-proven lapsed label must be released outright, removed %d", removed)
	}
	if _, _, err := r.Register("squat", "203.0.113.99", ""); err != nil {
		t.Errorf("the label must be free after release: %v", err)
	}
}

func TestDeregisterTombstonesAProvenLabel(t *testing.T) {
	r, token := proven(t)

	if _, err := r.Deregister(token); err != nil {
		t.Fatalf("deregistering: %v", err)
	}
	if r.Lookup("lab") != nil {
		t.Error("a released label must stop routing at once")
	}
	if r.Count() != 0 {
		t.Errorf("a released label must not count as a live registration, got %d", r.Count())
	}
	// The operator's links outlive their uninstall, so a deliberate release
	// holds the name too.
	if _, _, err := r.Register("lab", "203.0.113.99", ""); !errors.Is(err, ErrSubdomainTaken) {
		t.Errorf("a stranger claiming a just-released label must be refused, got %v", err)
	}
	if _, _, err := r.Register("lab", "203.0.113.1", token); err != nil {
		t.Errorf("the previous holder must be able to reclaim it: %v", err)
	}
}

// An install that fails before its cluster ever proved control releases the name
// on the way out. Holding that for the tombstone window would strand a name
// nothing ever served.
func TestDeregisterFreesANeverProvenLabel(t *testing.T) {
	r := New()
	entry, _, _ := r.Register("halfbuilt", "203.0.113.1", "")

	if _, err := r.Deregister(entry.Token); err != nil {
		t.Fatalf("deregistering: %v", err)
	}
	if _, _, err := r.Register("halfbuilt", "203.0.113.99", ""); err != nil {
		t.Errorf("a never-proven released label must be free at once: %v", err)
	}
}

// Revival re-establishes the address and re-arms proof, which is why it goes
// through Register. A ping carries neither, so it must not bring a lapsed
// registration back.
func TestPingCannotReviveALapsedLabel(t *testing.T) {
	r, token := proven(t)
	r.InactivityTTL = 1 * time.Millisecond
	time.Sleep(5 * time.Millisecond)

	if err := r.Ping(token); err == nil {
		t.Error("a ping must not revive a lapsed registration")
	}
	if r.Lookup("lab") != nil {
		t.Error("the registration must still be lapsed after a refused ping")
	}
}

func TestCountReportsLiveRegistrationsOnly(t *testing.T) {
	r, _ := proven(t)
	if r.Count() != 1 {
		t.Fatalf("Count = %d, want 1", r.Count())
	}
	r.InactivityTTL = 1 * time.Millisecond
	time.Sleep(5 * time.Millisecond)

	if r.Count() != 0 {
		t.Errorf("Count = %d, want 0; a tombstone is held, not served", r.Count())
	}
}

func TestTombstoneSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	r, token := proven(t)
	if _, err := r.Deregister(token); err != nil {
		t.Fatal(err)
	}
	if err := r.SaveTo(path); err != nil {
		t.Fatal(err)
	}

	// A restart that forgot the tombstone would hand the name to the next
	// caller, which is the whole failure being prevented.
	r2 := New()
	if err := r2.LoadFrom(path); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r2.Register("lab", "203.0.113.99", ""); !errors.Is(err, ErrSubdomainTaken) {
		t.Errorf("the tombstone must survive a restart, got %v", err)
	}
	if _, _, err := r2.Register("lab", "203.0.113.1", token); err != nil {
		t.Errorf("the previous holder must still be able to reclaim it: %v", err)
	}
}

// The /status counters drive the proof-before-route cutover audit, which waits
// for unpinned to reach zero. A tombstone carries no traffic, so counting one
// would hold the gate open on a registration that cannot be pinned by anyone.
func TestSummariesIgnoreTombstonedEntries(t *testing.T) {
	r, token := proven(t)
	if _, err := r.Deregister(token); err != nil {
		t.Fatal(err)
	}

	if count, _ := r.UnpinnedSummary(); count != 0 {
		t.Errorf("UnpinnedSummary counted %d, want 0: a released label is not awaiting a pin", count)
	}
	if count, _ := r.UnprovenSummary(); count != 0 {
		t.Errorf("UnprovenSummary counted %d, want 0", count)
	}
}

// Proof accrues to a registration that is serving. A lapsed one has to come back
// through Register first, which re-establishes the address. Accepting a proof
// here instead would let a cluster sign a challenge, be told it succeeded, and
// still carry no traffic, which is the silent-dark-cluster failure this
// heartbeat's whole design works to avoid.
func TestProofIsRefusedForALapsedRegistration(t *testing.T) {
	r, token := proven(t)
	if _, err := r.Deregister(token); err != nil {
		t.Fatal(err)
	}

	if _, accepted, err := r.IssueChallenge("lab", token); accepted || err != nil {
		t.Errorf("IssueChallenge on a released registration: accepted=%v err=%v, want refused", accepted, err)
	}
	if r.RecordProof("lab", token, "any-nonce", testFPA, "kipper-hop-proof-v1") {
		t.Error("RecordProof on a released registration must be refused")
	}

	// Coming back the proper way still works, and re-arms proof.
	if _, _, err := r.Register("lab", "203.0.113.1", token); err != nil {
		t.Fatalf("reviving: %v", err)
	}
	nonce, accepted, err := r.IssueChallenge("lab", token)
	if err != nil || !accepted {
		t.Fatalf("a revived registration must get a challenge: accepted=%v err=%v", accepted, err)
	}
	if !r.RecordProof("lab", token, nonce, testFPA, "kipper-hop-proof-v1") {
		t.Error("a revived registration must be able to prove again")
	}
}

// Tombstone eligibility is a fact about the label's past, so it cannot live in
// the fields that carry the current proof. A move clears the lease and the key
// on purpose, and reading those to decide the tombstone meant a cluster that
// moved and then died was treated as never having served: its name went free at
// once, and a stranger inherited every link published under it.
func TestAMovedRegistrationKeepsItsTombstone(t *testing.T) {
	r, token := proven(t)

	// The move clears the current proof, as it must: nothing has demonstrated
	// control at the new address yet.
	if _, _, err := r.Register("lab", "203.0.113.2", token); err != nil {
		t.Fatalf("moving: %v", err)
	}
	if r.Routable("lab") {
		t.Error("a moved registration must not route until it proves the new address")
	}

	// The replacement host never comes up, so the name lapses unproven.
	r.InactivityTTL = 1 * time.Millisecond
	time.Sleep(5 * time.Millisecond)

	if removed := len(r.Cleanup()); removed != 0 {
		t.Errorf("a label that once served must keep its tombstone across a move, removed %d", removed)
	}
	if _, _, err := r.Register("lab", "203.0.113.99", ""); !errors.Is(err, ErrSubdomainTaken) {
		t.Errorf("a stranger claiming it must still be refused, got %v", err)
	}
}

// Reviving a tombstone must re-arm proof, not inherit the lease the label held
// when it was released. Otherwise a name released with days left on its lease
// starts routing again the moment it is re-registered, to whatever now answers
// at that address, before anything has demonstrated control of it.
func TestRevivalReArmsProof(t *testing.T) {
	r, token := proven(t)
	if !r.Routable("lab") {
		t.Fatal("a freshly proven entry must be routable")
	}
	if _, err := r.Deregister(token); err != nil {
		t.Fatal(err)
	}

	if _, _, err := r.Register("lab", "203.0.113.1", token); err != nil {
		t.Fatalf("reviving: %v", err)
	}
	if r.Routable("lab") {
		t.Error("a revived registration must not route until it proves control again")
	}

	nonce, accepted, err := r.IssueChallenge("lab", token)
	if err != nil || !accepted {
		t.Fatalf("a revived registration must get a challenge: accepted=%v err=%v", accepted, err)
	}
	if !r.RecordProof("lab", token, nonce, testFPA, "kipper-hop-proof-v1") {
		t.Fatal("re-proving a revived registration must work")
	}
	if !r.Routable("lab") {
		t.Error("a re-proven registration must route again")
	}
}

// A live cluster's daily heartbeat is a renewal too. It must not drop the lease
// it just refreshed, or every beat would suspend the cluster until it re-proved.
func TestRenewingALiveRegistrationKeepsItsProof(t *testing.T) {
	r, token := proven(t)

	if _, _, err := r.Register("lab", "203.0.113.1", token); err != nil {
		t.Fatalf("renewing: %v", err)
	}
	if !r.Routable("lab") {
		t.Error("a routine renewal must leave a live cluster routable")
	}
}
