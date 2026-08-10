package hopproof

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"testing"
)

func mustKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

const (
	nonce  = "a1b2c3d4"
	label  = "acme"
	ip     = "203.0.113.10"
	origin = "kipper.run"
	token  = "deadbeef"
)

func TestSignVerifyRoundTrip(t *testing.T) {
	key := mustKey(t)
	sig, err := Sign(key, nonce, label, ip, origin, token)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if !Verify(&key.PublicKey, nonce, label, ip, origin, token, sig) {
		t.Error("a signature must verify against its own public key and message")
	}
}

func TestVerifyRejectsWrongKey(t *testing.T) {
	signer := mustKey(t)
	attacker := mustKey(t)
	sig, _ := Sign(signer, nonce, label, ip, origin, token)

	// The forgery in the design: an attacker observes the victim's public
	// certificate but does not hold its private key. Verifying the victim's
	// signature against the victim's public key is fine; an attacker cannot
	// produce a signature that verifies under the victim's key.
	if Verify(&attacker.PublicKey, nonce, label, ip, origin, token, sig) {
		t.Error("a signature must not verify under a different public key")
	}
}

func TestVerifyRejectsTamperedFields(t *testing.T) {
	key := mustKey(t)
	sig, _ := Sign(key, nonce, label, ip, origin, token)

	tampered := []struct {
		name                            string
		nonce, label, ip, origin, token string
	}{
		{"nonce", "ffffffff", label, ip, origin, token},
		{"label", nonce, "evil", ip, origin, token},
		{"ip", nonce, label, "198.51.100.9", origin, token},
		{"origin", nonce, label, ip, "evil.example", token},
		{"token", nonce, label, ip, origin, "0000"},
	}
	for _, tc := range tampered {
		if Verify(&key.PublicKey, tc.nonce, tc.label, tc.ip, tc.origin, tc.token, sig) {
			t.Errorf("a signature must not verify when %s is changed", tc.name)
		}
	}
}

func TestVerifyRejectsMalformedSignature(t *testing.T) {
	key := mustKey(t)
	for _, bad := range []string{"", "not-base64!!", "aGVsbG8="} {
		if Verify(&key.PublicKey, nonce, label, ip, origin, token, bad) {
			t.Errorf("malformed signature %q must not verify", bad)
		}
	}
	if Verify(nil, nonce, label, ip, origin, token, "aGVsbG8=") {
		t.Error("a nil public key must not verify")
	}
}

func TestDigestIsStable(t *testing.T) {
	a := Digest(nonce, label, ip, origin, token)
	b := Digest(nonce, label, ip, origin, token)
	if string(a) != string(b) {
		t.Error("the digest must be deterministic for the same inputs")
	}
	if len(a) != 32 {
		t.Errorf("expected a 32-byte SHA-256 digest, got %d", len(a))
	}
}
