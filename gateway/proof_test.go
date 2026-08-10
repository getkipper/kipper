package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/getkipper/kipper/controller/pkg/hopproof"
	"github.com/getkipper/kipper/gateway/internal/registry"
)

func genKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// stubObserve returns an observeKeyFunc that always reports pub as the key
// served at the dialed IP — standing in for the gateway's IP:443 dial.
func stubObserve(pub *ecdsa.PublicKey, spki string) observeKeyFunc {
	return func(ip, sni string) (*ecdsa.PublicKey, string, error) {
		return pub, spki, nil
	}
}

func proofSetup(t *testing.T) (*registry.Registry, *registry.Entry) {
	t.Helper()
	dataPath = filepath.Join(t.TempDir(), "registry.json")
	reg := registry.New()
	entry, _, err := reg.Register("myapp", "203.0.113.1", "")
	if err != nil {
		t.Fatal(err)
	}
	return reg, entry
}

func postProof(t *testing.T, handler http.HandlerFunc, req proofRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(req)
	r := httptest.NewRequest(http.MethodPost, "/proof", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func TestProofSucceedsWithPrivateKeyPossession(t *testing.T) {
	reg, entry := proofSetup(t)
	key := genKey(t)
	nonce, _, _ := reg.IssueChallenge("myapp", entry.Token)

	sig, err := hopproof.Sign(key, nonce, "myapp", entry.IP, "kipper.run", entry.Token)
	if err != nil {
		t.Fatal(err)
	}
	handler := handleProof(reg, "kipper.run", stubObserve(&key.PublicKey, "spki-xyz"))
	w := postProof(t, handler, proofRequest{Subdomain: "myapp", Token: entry.Token, Nonce: nonce, Signature: sig})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !reg.Routable("myapp") {
		t.Error("a completed proof must make the entry routable")
	}
}

// The defining B16 test: an attacker registers evil→victim-IP, reads the
// victim's PUBLIC certificate (so the gateway's dial observes the victim's
// key), but cannot sign the challenge without the victim's PRIVATE key. The
// forgery that defeats the FirstPinnedAt design must be rejected here.
func TestProofRejectsPublicKeyEchoForgery(t *testing.T) {
	reg, entry := proofSetup(t)
	victimKey := genKey(t)   // the key the gateway observes at the victim IP
	attackerKey := genKey(t) // all the attacker holds
	nonce, _, _ := reg.IssueChallenge("myapp", entry.Token)

	// The attacker signs with their own key (they lack the victim's private key).
	sig, _ := hopproof.Sign(attackerKey, nonce, "myapp", entry.IP, "kipper.run", entry.Token)
	// The gateway dials the victim IP and observes the VICTIM's key.
	handler := handleProof(reg, "kipper.run", stubObserve(&victimKey.PublicKey, "victim-spki"))
	w := postProof(t, handler, proofRequest{Subdomain: "myapp", Token: entry.Token, Nonce: nonce, Signature: sig})

	if w.Code != http.StatusForbidden {
		t.Fatalf("the public-key-echo forgery must be rejected with 403, got %d", w.Code)
	}
	if reg.Routable("myapp") {
		t.Error("a forged proof must never make the entry routable")
	}
}

func TestProofRejectsNonOutstandingNonce(t *testing.T) {
	reg, entry := proofSetup(t)
	key := genKey(t)
	_, _, _ = reg.IssueChallenge("myapp", entry.Token) // some outstanding nonce

	// Submit a nonce that is not the outstanding one (never issued, replayed,
	// or expired). It must be rejected before the entry can be proven.
	bogus := strings.Repeat("f", 32)
	sig, _ := hopproof.Sign(key, bogus, "myapp", entry.IP, "kipper.run", entry.Token)
	handler := handleProof(reg, "kipper.run", stubObserve(&key.PublicKey, "spki"))
	w := postProof(t, handler, proofRequest{Subdomain: "myapp", Token: entry.Token, Nonce: bogus, Signature: sig})

	if w.Code != http.StatusConflict {
		t.Fatalf("a non-outstanding nonce must be rejected with 409, got %d", w.Code)
	}
	if reg.Routable("myapp") {
		t.Error("a rejected proof must not make the entry routable")
	}
}

func TestProofRetriesWhenDialFails(t *testing.T) {
	reg, entry := proofSetup(t)
	key := genKey(t)
	nonce, _, _ := reg.IssueChallenge("myapp", entry.Token)
	sig, _ := hopproof.Sign(key, nonce, "myapp", entry.IP, "kipper.run", entry.Token)

	failDial := func(ip, sni string) (*ecdsa.PublicKey, string, error) {
		return nil, "", fmt.Errorf("connection refused")
	}
	handler := handleProof(reg, "kipper.run", failDial)
	w := postProof(t, handler, proofRequest{Subdomain: "myapp", Token: entry.Token, Nonce: nonce, Signature: sig})

	if w.Code != http.StatusAccepted {
		t.Fatalf("an unreachable cluster must get 202 to retry, got %d", w.Code)
	}
	if reg.Routable("myapp") {
		t.Error("a proof that could not be verified must not make the entry routable")
	}
}

func TestProofRejectsUnknownSubdomain(t *testing.T) {
	reg, _ := proofSetup(t)
	handler := handleProof(reg, "kipper.run", stubObserve(&genKey(t).PublicKey, "spki"))
	w := postProof(t, handler, proofRequest{Subdomain: "ghost", Token: "x", Nonce: "y", Signature: "z"})
	if w.Code != http.StatusNotFound {
		t.Fatalf("an unknown subdomain must get 404, got %d", w.Code)
	}
}

func TestRegisterIssuesChallengeWithToken(t *testing.T) {
	dataPath = filepath.Join(t.TempDir(), "registry.json")
	reg := registry.New()
	entry, _, _ := reg.Register("myapp", "203.0.113.1", "")
	handler := handleRegister(reg, "kipper.run", neverObserve)

	body := fmt.Sprintf(`{"subdomain":"myapp","ip":"203.0.113.1","token":%q}`, entry.Token)
	r := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	var resp registerResponse
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if resp.Challenge == "" {
		t.Error("a token-authenticated renewal must receive a proof challenge")
	}

	// A plain renewal without a token gets no challenge.
	plain := `{"subdomain":"myapp","ip":"203.0.113.1"}`
	r2 := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(plain))
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, r2)
	var resp2 registerResponse
	_ = json.NewDecoder(w2.Body).Decode(&resp2)
	if resp2.Challenge != "" {
		t.Error("an unauthenticated renewal must not receive a challenge")
	}
}

// End-to-end regression for the EOD Critical: a cluster proves and pins key A,
// then the holder of the management token asserts their own key B and answers
// the gateway's verification dial with it (an on-path position). The pin
// activates on B — and routing stops, instead of riding out the remainder of A's
// seven-day lease and delivering the victim's traffic to B.
func TestPinnedAttackerKeyDoesNotRouteOnTheProvenLease(t *testing.T) {
	dataPath = filepath.Join(t.TempDir(), "registry.json")
	reg := registry.New()
	entry, _, _ := reg.Register("myapp", "203.0.113.1", "")

	victimFP := strings.Repeat("a", 64)
	if !reg.ActivatePin("myapp", entry.Token, victimFP) {
		t.Fatal("activate")
	}
	nonce, _, _ := reg.IssueChallenge("myapp", entry.Token)
	if !reg.RecordProof("myapp", entry.Token, nonce, victimFP, hopproof.Protocol) {
		t.Fatal("record proof")
	}
	if !reg.Routable("myapp") {
		t.Fatal("the proven, pinned cluster must be routable")
	}

	attackerFP := strings.Repeat("b", 64)
	handler := handleRegister(reg, "kipper.run", fixedObserve(attackerFP))
	body := fmt.Sprintf(`{"subdomain":"myapp","ip":"203.0.113.1","token":%q,"certFingerprint":%q}`,
		entry.Token, attackerFP)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(body)))

	if w.Code != http.StatusCreated {
		t.Fatalf("the assertion was expected to activate, got %d: %s", w.Code, w.Body.String())
	}
	if s := reg.PinState("myapp"); s.Current != attackerFP {
		t.Fatalf("expected the asserted key pinned, got %+v", s)
	}
	if reg.Routable("myapp") {
		t.Error("the proof lease must not authorise a key it was not obtained for")
	}
	count, _ := reg.UnprovenSummary()
	if count != 1 {
		t.Errorf("/status must report the cluster as unproven, got %d", count)
	}
}

func TestProofRejectsBadTokenBeforeDial(t *testing.T) {
	reg, entry := proofSetup(t)
	nonce, _, _ := reg.IssueChallenge("myapp", entry.Token)

	dialed := false
	countingObserve := func(ip, sni string) (*ecdsa.PublicKey, string, error) {
		dialed = true
		return &genKey(t).PublicKey, "spki", nil
	}
	handler := handleProof(reg, "kipper.run", countingObserve)
	// Wrong token: must be rejected in constant time before any network dial.
	w := postProof(t, handler, proofRequest{Subdomain: "myapp", Token: "wrong", Nonce: nonce, Signature: "x"})

	if w.Code != http.StatusConflict {
		t.Fatalf("a bad token must be rejected, got %d", w.Code)
	}
	if dialed {
		t.Error("an uncommittable request must be rejected before the verification dial")
	}
}
