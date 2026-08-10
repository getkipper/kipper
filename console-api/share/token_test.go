package share

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var testKey = []byte("0123456789abcdef0123456789abcdef")

const (
	testHost = "mailhog-supplemento-test.example.com"
	testKID  = "kidtoken1"
	testUID  = "uid-token-1"
)

// tokenTestKeyring is the keyring the token tests validate against.
func tokenTestKeyring() *Keyring {
	return &Keyring{CurrentKID: testKID, CurrentKey: testKey}
}

// signRaw signs arbitrary claims with a method and kid of our choosing so
// the negative tests can forge tokens the honest MintGrant path would
// never produce. A kid header is set so the token reaches the claim checks
// rather than failing at key selection (unless the case is about the kid).
func signRaw(t *testing.T, method jwt.SigningMethod, key any, kid string, claims jwt.Claims) string {
	t.Helper()
	tok := jwt.NewWithClaims(method, claims)
	if kid != "" {
		tok.Header["kid"] = kid
	}
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatalf("signing raw token: %v", err)
	}
	return s
}

// validClaims is an otherwise-valid claim set the negative tests mutate
// one field at a time.
func validClaims(now time.Time) Claims {
	return Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    Issuer,
			Subject:   Subject,
			Audience:  jwt.ClaimStrings{testHost},
			ID:        "abc",
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
		},
		Service:    "supplemento-test/mailhog",
		ServiceUID: testUID,
	}
}

func TestGrantMintRejectsBadLifetime(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	kr := tokenTestKeyring()
	for _, d := range []time.Duration{0, -time.Hour, MaxLifetime + time.Hour} {
		g, err := NewGrant(testUID, "mailhog", "supplemento-test", testHost, "", "admin@example.com", 72*time.Hour, now)
		if err != nil {
			t.Fatalf("NewGrant: %v", err)
		}
		g.ExpiresAt = now.Add(d)
		if _, err := MintGrant(kr, g, now); err == nil {
			t.Errorf("MintGrant accepted lifetime %s", d)
		}
	}
}

func TestValidateGrantRejects(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	kr := tokenTestKeyring()
	good := signRaw(t, jwt.SigningMethodHS256, testKey, testKID, validClaims(now))

	tests := []struct {
		name  string
		token string
		host  string
		at    time.Time
	}{
		{"wrong host", good, "mailhog-other-ns.example.com", now},
		{"expired", good, testHost, now.Add(73 * time.Hour)},
		{"empty token", "", testHost, now},
		{"garbage", "not.a.jwt", testHost, now},
		{"empty host", good, "", now},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ValidateGrantToken(kr, tt.token, tt.host, tt.at); err == nil {
				t.Error("expected ErrInvalid")
			}
		})
	}
}

func TestValidateGrantRejectsWrongKey(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	kr := tokenTestKeyring()
	// Signed with the right kid but the wrong key bytes.
	forged := signRaw(t, jwt.SigningMethodHS256, []byte("ffffffffffffffffffffffffffffffff"), testKID, validClaims(now))
	if _, err := ValidateGrantToken(kr, forged, testHost, now); err == nil {
		t.Error("accepted a token signed with the wrong key")
	}
}

func TestValidateGrantRejectsUnknownKID(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	kr := tokenTestKeyring()
	unknown := signRaw(t, jwt.SigningMethodHS256, testKey, "kidnope", validClaims(now))
	if _, err := ValidateGrantToken(kr, unknown, testHost, now); err == nil {
		t.Error("accepted a token with a kid the keyring does not know")
	}
}

func TestValidateGrantRejectsAlgorithmConfusion(t *testing.T) {
	// A token signed "none" must be rejected even though the claims are
	// otherwise valid.
	now := time.Unix(1_700_000_000, 0)
	kr := tokenTestKeyring()
	none := signRaw(t, jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, testKID, validClaims(now))
	if _, err := ValidateGrantToken(kr, none, testHost, now); err == nil {
		t.Error("accepted alg=none token")
	}
}

func TestValidateGrantRejectsWrongIssuerSubject(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	kr := tokenTestKeyring()

	wrongIss := validClaims(now)
	wrongIss.Issuer = "https://dex.example.com/dex"
	wrongSub := validClaims(now)
	wrongSub.Subject = "other"

	for name, c := range map[string]Claims{"wrong issuer": wrongIss, "wrong subject": wrongSub} {
		t.Run(name, func(t *testing.T) {
			tok := signRaw(t, jwt.SigningMethodHS256, testKey, testKID, c)
			if _, err := ValidateGrantToken(kr, tok, testHost, now); err == nil {
				t.Error("expected rejection")
			}
		})
	}
}

func TestValidateGrantRejectsFutureIssuedAt(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	kr := tokenTestKeyring()
	c := validClaims(now)
	c.IssuedAt = jwt.NewNumericDate(now.Add(time.Hour))
	c.ExpiresAt = jwt.NewNumericDate(now.Add(2 * time.Hour))
	tok := signRaw(t, jwt.SigningMethodHS256, testKey, testKID, c)
	if _, err := ValidateGrantToken(kr, tok, testHost, now); err == nil {
		t.Error("accepted future-issued token")
	}
}

func TestValidateGrantRejectsOverlongLifetime(t *testing.T) {
	// exp alone looks plausible (unexpired), but exp-iat exceeds the cap.
	now := time.Unix(1_700_000_000, 0)
	kr := tokenTestKeyring()
	c := validClaims(now)
	c.ExpiresAt = jwt.NewNumericDate(now.Add(MaxLifetime + 24*time.Hour))
	tok := signRaw(t, jwt.SigningMethodHS256, testKey, testKID, c)
	if _, err := ValidateGrantToken(kr, tok, testHost, now.Add(time.Hour)); err == nil {
		t.Error("accepted over-long token")
	}
}

func TestValidateGrantRejectsMultiAudience(t *testing.T) {
	// A token minted for two hosts must not open either: bound to one UI.
	now := time.Unix(1_700_000_000, 0)
	kr := tokenTestKeyring()
	c := validClaims(now)
	c.Audience = jwt.ClaimStrings{testHost, "mailhog-other.example.com"}
	tok := signRaw(t, jwt.SigningMethodHS256, testKey, testKID, c)
	if _, err := ValidateGrantToken(kr, tok, testHost, now); err == nil {
		t.Error("accepted a multi-audience token")
	}
}

func TestValidateGrantRejectsNonPositiveLifetime(t *testing.T) {
	// exp <= iat: within leeway a token could otherwise sneak through.
	now := time.Unix(1_700_000_000, 0)
	kr := tokenTestKeyring()
	c := validClaims(now)
	c.IssuedAt = jwt.NewNumericDate(now.Add(10 * time.Second))
	c.ExpiresAt = jwt.NewNumericDate(now)
	tok := signRaw(t, jwt.SigningMethodHS256, testKey, testKID, c)
	if _, err := ValidateGrantToken(kr, tok, testHost, now); err == nil {
		t.Error("accepted a token with exp <= iat")
	}
}

func TestValidateGrantRejectsMissingJTI(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	kr := tokenTestKeyring()
	c := validClaims(now)
	c.ID = ""
	tok := signRaw(t, jwt.SigningMethodHS256, testKey, testKID, c)
	if _, err := ValidateGrantToken(kr, tok, testHost, now); err == nil {
		t.Error("accepted token with no jti")
	}
}

func TestValidateGrantRejectsMissingUID(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	kr := tokenTestKeyring()
	c := validClaims(now)
	c.ServiceUID = ""
	tok := signRaw(t, jwt.SigningMethodHS256, testKey, testKID, c)
	if _, err := ValidateGrantToken(kr, tok, testHost, now); err == nil {
		t.Error("accepted token with no uid claim")
	}
}

func TestCanonicalHost(t *testing.T) {
	ok := map[string]string{
		"mailhog-x.example.com":     "mailhog-x.example.com",
		"MailHog-X.example.com":     "mailhog-x.example.com",
		"mailhog-x.example.com:443": "mailhog-x.example.com",
		"  mailhog-x.example.com  ": "mailhog-x.example.com",
	}
	for in, want := range ok {
		got, err := CanonicalHost(in)
		if err != nil || got != want {
			t.Errorf("CanonicalHost(%q) = %q, %v; want %q", in, got, err, want)
		}
	}

	bad := []string{"", "evil.com/path", "user@evil.com", "a?b", "a#b", "[::1]", "a\\b"}
	for _, in := range bad {
		if _, err := CanonicalHost(in); err == nil {
			t.Errorf("CanonicalHost(%q) accepted", in)
		}
	}
}
