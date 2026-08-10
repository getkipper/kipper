package uisession

import (
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func testKeyring(t *testing.T) *Keyring {
	t.Helper()
	kid, err := newKID()
	if err != nil {
		t.Fatalf("newKID: %v", err)
	}
	key, err := newKey()
	if err != nil {
		t.Fatalf("newKey: %v", err)
	}
	return &Keyring{CurrentKID: kid, CurrentKey: key}
}

var epoch = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func TestCodeRoundTrip(t *testing.T) {
	kr := testKeyring(t)
	code, sid, err := MintCode(kr, "CgVhZG1pbg", "ops@example.com", "mailhog.apps.example.com", epoch)
	if err != nil {
		t.Fatalf("MintCode: %v", err)
	}
	v, err := ValidateCode(kr, code, "mailhog.apps.example.com", epoch.Add(10*time.Second))
	if err != nil {
		t.Fatalf("ValidateCode: %v", err)
	}
	if v.Sub != "CgVhZG1pbg" || v.Email != "ops@example.com" || v.SID != sid {
		t.Fatalf("claims mismatch: %+v (sid %s)", v, sid)
	}
}

func TestSessionRoundTrip(t *testing.T) {
	kr := testKeyring(t)
	tok, err := MintSession(kr, "CgVhZG1pbg", "ops@example.com", "mailhog.apps.example.com", "sid123", epoch, epoch)
	if err != nil {
		t.Fatalf("MintSession: %v", err)
	}
	v, err := ValidateSession(kr, tok, "mailhog.apps.example.com", epoch.Add(5*time.Minute))
	if err != nil {
		t.Fatalf("ValidateSession: %v", err)
	}
	if v.SID != "sid123" || !v.AuthTime.Equal(epoch) {
		t.Fatalf("session claims mismatch: %+v", v)
	}
}

func TestWrongHostRejected(t *testing.T) {
	kr := testKeyring(t)
	code, _, _ := MintCode(kr, "sub", "ops@example.com", "mailhog.apps.example.com", epoch)
	if _, err := ValidateCode(kr, code, "rabbitmq.apps.example.com", epoch.Add(time.Second)); err == nil {
		t.Fatal("code for one host validated for another")
	}
}

func TestCodeNotAcceptedAsSession(t *testing.T) {
	kr := testKeyring(t)
	code, _, _ := MintCode(kr, "sub", "ops@example.com", "mailhog.apps.example.com", epoch)
	if _, err := ValidateSession(kr, code, "mailhog.apps.example.com", epoch.Add(time.Second)); err == nil {
		t.Fatal("code accepted as session (issuer pin bypassed)")
	}
}

func TestSessionNotAcceptedAsCode(t *testing.T) {
	kr := testKeyring(t)
	tok, _ := MintSession(kr, "sub", "ops@example.com", "mailhog.apps.example.com", "sid", epoch, epoch)
	if _, err := ValidateCode(kr, tok, "mailhog.apps.example.com", epoch.Add(time.Second)); err == nil {
		t.Fatal("session accepted as code (issuer pin bypassed)")
	}
}

func TestExpiredCodeRejected(t *testing.T) {
	kr := testKeyring(t)
	code, _, _ := MintCode(kr, "sub", "ops@example.com", "mailhog.apps.example.com", epoch)
	// Past TTL + leeway.
	if _, err := ValidateCode(kr, code, "mailhog.apps.example.com", epoch.Add(CodeTTL+leeway+time.Second)); err == nil {
		t.Fatal("expired code validated")
	}
}

func TestSessionAbsoluteCap(t *testing.T) {
	kr := testKeyring(t)
	// Re-mint fresh idle window but authTime is 13h old: absolute cap must bite.
	authTime := epoch
	tok, _ := MintSession(kr, "sub", "ops@example.com", "mailhog.apps.example.com", "sid", authTime, epoch.Add(13*time.Hour))
	if _, err := ValidateSession(kr, tok, "mailhog.apps.example.com", epoch.Add(13*time.Hour)); err == nil {
		t.Fatal("session past 12h absolute cap validated")
	}
}

func TestNoneAlgRejected(t *testing.T) {
	kr := testKeyring(t)
	c := claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    issuerSession,
			Subject:   "sub",
			Audience:  jwt.ClaimStrings{"mailhog.apps.example.com"},
			IssuedAt:  jwt.NewNumericDate(epoch),
			ExpiresAt: jwt.NewNumericDate(epoch.Add(SessionIdleTTL)),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodNone, c)
	tok.Header["kid"] = kr.CurrentKID
	raw, err := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("sign none: %v", err)
	}
	if _, err := ValidateSession(kr, raw, "mailhog.apps.example.com", epoch.Add(time.Second)); err == nil {
		t.Fatal("alg=none token validated")
	}
}

func TestWrongKeyRejected(t *testing.T) {
	kr := testKeyring(t)
	// Forge a token claiming the real kid but signed with a different key.
	attacker := make([]byte, 32)
	for i := range attacker {
		attacker[i] = 0x41
	}
	c := claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    issuerSession,
			Subject:   "sub",
			Audience:  jwt.ClaimStrings{"mailhog.apps.example.com"},
			IssuedAt:  jwt.NewNumericDate(epoch),
			ExpiresAt: jwt.NewNumericDate(epoch.Add(SessionIdleTTL)),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, c)
	tok.Header["kid"] = kr.CurrentKID
	raw, _ := tok.SignedString(attacker)
	if _, err := ValidateSession(kr, raw, "mailhog.apps.example.com", epoch.Add(time.Second)); err == nil {
		t.Fatal("token signed with wrong key validated")
	}
}

func TestUnknownKidRejected(t *testing.T) {
	kr := testKeyring(t)
	tok, _ := MintSession(kr, "sub", "ops@example.com", "mailhog.apps.example.com", "sid", epoch, epoch)
	// Rotate so the signing kid is no longer present in the keyring at all.
	other := testKeyring(t)
	if _, err := ValidateSession(other, tok, "mailhog.apps.example.com", epoch.Add(time.Second)); err == nil {
		t.Fatal("token with unknown kid validated")
	}
}

func TestPreviousKeyStillValidates(t *testing.T) {
	prev := testKeyring(t)
	tok, _ := MintSession(prev, "sub", "ops@example.com", "mailhog.apps.example.com", "sid", epoch, epoch)
	// Simulate a rotation: current is new, previous is the key that signed tok.
	cur := testKeyring(t)
	rotated := &Keyring{
		CurrentKID:  cur.CurrentKID,
		CurrentKey:  cur.CurrentKey,
		PreviousKID: prev.CurrentKID,
		PreviousKey: prev.CurrentKey,
	}
	if _, err := ValidateSession(rotated, tok, "mailhog.apps.example.com", epoch.Add(time.Second)); err != nil {
		t.Fatalf("token signed by previous key rejected after rotation: %v", err)
	}
}

func TestTamperedTokenRejected(t *testing.T) {
	kr := testKeyring(t)
	tok, _ := MintSession(kr, "sub", "ops@example.com", "mailhog.apps.example.com", "sid", epoch, epoch)
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("unexpected token shape")
	}
	// Flip a byte in the signature.
	sig := []byte(parts[2])
	sig[0] ^= 0xFF
	tampered := parts[0] + "." + parts[1] + "." + string(sig)
	if _, err := ValidateSession(kr, tampered, "mailhog.apps.example.com", epoch.Add(time.Second)); err == nil {
		t.Fatal("tampered token validated")
	}
}

func TestCookieNamePerHost(t *testing.T) {
	a := CookieName("mailhog.apps.example.com")
	b := CookieName("rabbitmq.apps.example.com")
	if a == b {
		t.Fatalf("cookie names collide across hosts: %s", a)
	}
	if !strings.HasPrefix(a, "__Host-") {
		t.Fatalf("cookie name missing __Host- prefix: %s", a)
	}
}
