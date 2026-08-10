package share

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"k8s.io/client-go/kubernetes/fake"
)

func TestKeyringCreateAndReload(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()

	kr, err := LoadOrCreateKeyring(ctx, client)
	if err != nil {
		t.Fatalf("LoadOrCreateKeyring: %v", err)
	}
	if kr.CurrentKID == "" || len(kr.CurrentKey) != 32 {
		t.Fatalf("fresh keyring is malformed: %+v", kr)
	}
	if kr.PreviousKID != "" {
		t.Error("a fresh keyring must have no previous key")
	}

	again, err := LoadOrCreateKeyring(ctx, client)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if again.CurrentKID != kr.CurrentKID || !bytes.Equal(again.CurrentKey, kr.CurrentKey) {
		t.Error("reload must return the same keyring")
	}
}

// A signing Secret without a current slot (the retired v0 shape included)
// is malformed: minting refuses it with a message naming the way out.
func TestKeyringRejectsSlotlessSecret(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset(legacySigningSecret())

	if _, err := LoadOrCreateKeyring(ctx, client); err == nil {
		t.Fatal("a slotless signing secret must be rejected")
	}
}

func TestKeyringRotation(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()

	first, err := LoadOrCreateKeyring(ctx, client)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	second, err := RotateKeyring(ctx, client)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if second.CurrentKID == first.CurrentKID {
		t.Error("rotation must install a fresh current kid")
	}
	if second.PreviousKID != first.CurrentKID || !bytes.Equal(second.PreviousKey, first.CurrentKey) {
		t.Error("rotation must demote current to previous")
	}

	// A second rotation retires the original key entirely.
	third, err := RotateKeyring(ctx, client)
	if err != nil {
		t.Fatalf("second rotate: %v", err)
	}
	if third.KeyFor(first.CurrentKID) != nil {
		t.Error("two rotations must retire the original key")
	}
	if third.PreviousKID != second.CurrentKID {
		t.Error("previous must be the last current")
	}
}

// TestGrantTokenValidationMatrix drives ValidateGrantToken through the
// kid-selection and claim requirements the gate depends on.
func TestGrantTokenValidationMatrix(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	kr := &Keyring{
		CurrentKID:  "kidA",
		CurrentKey:  []byte("0123456789abcdef0123456789abcdef"),
		PreviousKID: "kidB",
		PreviousKey: []byte("fedcba9876543210fedcba9876543210"),
	}
	g := testGrant(t, now)

	tok, err := MintGrant(kr, g, now)
	if err != nil {
		t.Fatalf("MintGrant: %v", err)
	}

	if _, err := ValidateGrantToken(kr, tok, g.Host, now); err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}

	// Signed with the previous key: still valid until it expires.
	prevRing := &Keyring{CurrentKID: "kidB", CurrentKey: kr.PreviousKey}
	prevTok, err := MintGrant(prevRing, g, now)
	if err != nil {
		t.Fatalf("MintGrant previous: %v", err)
	}
	if _, err := ValidateGrantToken(kr, prevTok, g.Host, now); err != nil {
		t.Error("token signed by the previous key must stay valid")
	}

	// Signed with a retired key (kid unknown to the ring): rejected.
	retired := &Keyring{CurrentKID: "kidZ", CurrentKey: []byte("00000000000000000000000000000000")}
	retiredTok, err := MintGrant(retired, g, now)
	if err != nil {
		t.Fatalf("MintGrant retired: %v", err)
	}
	if _, err := ValidateGrantToken(kr, retiredTok, g.Host, now); err == nil {
		t.Error("token with an unknown kid must be rejected")
	}

	// A token with no kid header must be rejected: the validator selects
	// the key strictly by kid and never falls back to trying a key blind,
	// even when the raw current key would verify the signature.
	kidless := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    Issuer,
			Subject:   Subject,
			Audience:  jwt.ClaimStrings{g.Host},
			ID:        g.JTI,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
		},
		Service:    g.Namespace + "/" + g.Service,
		ServiceUID: g.ServiceUID,
	})
	kidlessTok, err := kidless.SignedString(kr.CurrentKey)
	if err != nil {
		t.Fatalf("signing kid-less token: %v", err)
	}
	if _, err := ValidateGrantToken(kr, kidlessTok, g.Host, now); err == nil {
		t.Error("a kid-less token must be rejected")
	}

	// Wrong host and expiry behave as in v0.
	if _, err := ValidateGrantToken(kr, tok, "other-host.storefront.com", now); err == nil {
		t.Error("wrong host accepted")
	}
	if _, err := ValidateGrantToken(kr, tok, g.Host, g.ExpiresAt.Add(time.Minute)); err == nil {
		t.Error("expired token accepted")
	}
}
