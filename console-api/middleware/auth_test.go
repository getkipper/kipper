package middleware

import (
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var testSecret = []byte("test-secret-key")

// signedRS256Token mints a valid RS256 token for the given audience and
// returns it with an Auth configured to verify it.
func signedRS256Token(t *testing.T, audience string) (string, *Auth) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating RSA key: %v", err)
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, &Claims{
		Email: "admin@kipper.test",
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    "https://dex.test",
			Audience:  jwt.ClaimStrings{audience},
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	})
	signed, err := tok.SignedString(key)
	if err != nil {
		t.Fatalf("signing test token: %v", err)
	}
	auth := &Auth{
		Issuer:   "https://dex.test",
		Audience: DefaultAudience,
		KeyFunc:  func(token *jwt.Token) (any, error) { return &key.PublicKey, nil },
	}
	return signed, auth
}

func TestAuthRejectsUnverifiedEmail(t *testing.T) {
	// Roles are granted by email, so a token whose issuer marks the email
	// unverified must be refused. An absent email_verified claim (the default
	// Dex local connector's shape) must still authenticate.
	boolPtr := func(b bool) *bool { return &b }
	cases := []struct {
		name          string
		emailVerified *bool
		wantStatus    int
	}{
		{"explicit false is rejected", boolPtr(false), http.StatusUnauthorized},
		{"explicit true is accepted", boolPtr(true), http.StatusOK},
		{"absent claim is accepted", nil, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key, err := rsa.GenerateKey(rand.Reader, 2048)
			if err != nil {
				t.Fatalf("generating RSA key: %v", err)
			}
			tok := jwt.NewWithClaims(jwt.SigningMethodRS256, &Claims{
				Email:         "user@kipper.test",
				EmailVerified: tc.emailVerified,
				RegisteredClaims: jwt.RegisteredClaims{
					Issuer:    "https://dex.test",
					Audience:  jwt.ClaimStrings{DefaultAudience},
					ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
				},
			})
			signed, err := tok.SignedString(key)
			if err != nil {
				t.Fatalf("signing test token: %v", err)
			}
			auth := &Auth{
				Issuer:   "https://dex.test",
				Audience: DefaultAudience,
				KeyFunc:  func(token *jwt.Token) (any, error) { return &key.PublicKey, nil },
			}

			called := false
			handler := auth.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.Header.Set("Authorization", "Bearer "+signed)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)

			if w.Code != tc.wantStatus {
				t.Errorf("expected status %d, got %d", tc.wantStatus, w.Code)
			}
			if (w.Code == http.StatusOK) != called {
				t.Errorf("handler called=%v but status=%d", called, w.Code)
			}
		})
	}
}

func TestValidateRequestAndIDTokenRejectUnverifiedEmail(t *testing.T) {
	// The email-trust gate must hold on every Dex-token entry point, not just
	// the Chi middleware: ValidateRequest (used by /auth/ui-code) and
	// ValidateIDToken (used by callback/refresh) must both refuse an
	// explicitly-unverified email and accept an absent one.
	boolPtr := func(b bool) *bool { return &b }
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating RSA key: %v", err)
	}
	auth := &Auth{
		Issuer:   "https://dex.test",
		Audience: DefaultAudience,
		KeyFunc:  func(*jwt.Token) (any, error) { return &key.PublicKey, nil },
	}
	sign := func(ev *bool) string {
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, &Claims{
			Email:         "user@kipper.test",
			EmailVerified: ev,
			RegisteredClaims: jwt.RegisteredClaims{
				Issuer:    "https://dex.test",
				Audience:  jwt.ClaimStrings{DefaultAudience},
				ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			},
		})
		signed, serr := tok.SignedString(key)
		if serr != nil {
			t.Fatalf("signing test token: %v", serr)
		}
		return signed
	}

	cases := []struct {
		name       string
		ev         *bool
		wantReject bool
	}{
		{"explicit false", boolPtr(false), true},
		{"explicit true", boolPtr(true), false},
		{"absent", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			signed := sign(tc.ev)

			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.Header.Set("Authorization", "Bearer "+signed)
			_, reqErr := auth.ValidateRequest(r)
			if (reqErr != nil) != tc.wantReject {
				t.Errorf("ValidateRequest reject=%v, want %v (err=%v)", reqErr != nil, tc.wantReject, reqErr)
			}

			_, idErr := auth.ValidateIDToken(signed)
			if (idErr != nil) != tc.wantReject {
				t.Errorf("ValidateIDToken reject=%v, want %v (err=%v)", idErr != nil, tc.wantReject, idErr)
			}
		})
	}
}

func TestAuthAcceptsFirstPartyAudiences(t *testing.T) {
	// The console client and the kip CLI client are both first-party;
	// tokens issued to either must authenticate. The CLI case is the
	// regression: kip service bind got 401 once audience validation
	// only knew the console client.
	for _, audience := range []string{DefaultAudience, CLIAudience} {
		t.Run(audience, func(t *testing.T) {
			signed, auth := signedRS256Token(t, audience)
			called := false
			handler := auth.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
			}))

			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.Header.Set("Authorization", "Bearer "+signed)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)

			if w.Code != http.StatusOK || !called {
				t.Errorf("expected 200 with handler called for audience %q, got %d (called=%v)", audience, w.Code, called)
			}
		})
	}
}

func TestAuthRejectsForeignAudience(t *testing.T) {
	signed, auth := signedRS256Token(t, "third-party-app")
	handler := auth.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler must not be called for a foreign audience")
	}))

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer "+signed)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for foreign audience, got %d", w.Code)
	}
}

// TestAuthValidationInvariants breaks exactly one property of an
// otherwise-valid RS256 token per case, so removing any single parse
// option surfaces as a test failure. Both bearer paths are exercised:
// the REST middleware (Handler) and the forward-auth gate.
func TestAuthValidationInvariants(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating RSA key: %v", err)
	}
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating RSA key: %v", err)
	}
	auth := &Auth{
		Issuer:   "https://dex.test",
		Audience: DefaultAudience,
		KeyFunc:  func(token *jwt.Token) (any, error) { return &key.PublicKey, nil },
	}
	validClaims := func() *Claims {
		return &Claims{
			Email: "admin@kipper.test",
			RegisteredClaims: jwt.RegisteredClaims{
				Issuer:    "https://dex.test",
				Audience:  jwt.ClaimStrings{DefaultAudience},
				ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			},
		}
	}
	sign := func(t *testing.T, claims *Claims, signKey *rsa.PrivateKey) string {
		t.Helper()
		signed, serr := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(signKey)
		if serr != nil {
			t.Fatalf("signing test token: %v", serr)
		}
		return signed
	}

	tests := []struct {
		name   string
		token  func(t *testing.T) string
		wantOK bool
	}{
		{"valid", func(t *testing.T) string { return sign(t, validClaims(), key) }, true},
		{"wrong issuer", func(t *testing.T) string {
			c := validClaims()
			c.Issuer = "https://evil.test"
			return sign(t, c, key)
		}, false},
		{"expired", func(t *testing.T) string {
			c := validClaims()
			c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Hour))
			return sign(t, c, key)
		}, false},
		{"missing expiry", func(t *testing.T) string {
			c := validClaims()
			c.ExpiresAt = nil
			return sign(t, c, key)
		}, false},
		{"wrong signing key", func(t *testing.T) string { return sign(t, validClaims(), otherKey) }, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			signed := tt.token(t)

			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.Header.Set("Authorization", "Bearer "+signed)
			w := httptest.NewRecorder()
			auth.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(w, r)
			if ok := w.Code == http.StatusOK; ok != tt.wantOK {
				t.Errorf("Handler: authenticated = %v, want %v (status %d)", ok, tt.wantOK, w.Code)
			}

			// ValidateIDToken is header-independent (Callback uses it on the
			// token Dex just issued); it applies the same RS256/issuer/console-
			// audience/expiry pins as the request path.
			if _, ferr := auth.ValidateIDToken(signed); (ferr == nil) != tt.wantOK {
				t.Errorf("ValidateIDToken: err = %v, want ok = %v", ferr, tt.wantOK)
			}
		})
	}
}

func TestValidateIDTokenRejectsCLIAudience(t *testing.T) {
	// ValidateIDToken is console-audience only; a CLI token must never pass
	// it (see CLIAudience). The browser-SSO gate no longer trusts any Dex
	// token from a cookie, so this pin lives here and at /auth/ui-code.
	signed, auth := signedRS256Token(t, CLIAudience)

	if _, err := auth.ValidateIDToken(signed); err == nil {
		t.Error("expected ValidateIDToken to reject a CLI-audience token")
	}
}

func TestAuthRejectsMissingAudience(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating RSA key: %v", err)
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, &Claims{
		Email: "admin@kipper.test",
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    "https://dex.test",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	})
	signed, err := tok.SignedString(key)
	if err != nil {
		t.Fatalf("signing test token: %v", err)
	}
	auth := &Auth{
		Issuer:   "https://dex.test",
		Audience: DefaultAudience,
		KeyFunc:  func(token *jwt.Token) (any, error) { return &key.PublicKey, nil },
	}
	handler := auth.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler must not be called for a token without an audience")
	}))

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer "+signed)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for missing audience, got %d", w.Code)
	}
}

func TestAuthRejectsNonRS256Algorithm(t *testing.T) {
	// Algorithm-confusion guard. The keyfunc here returns testSecret, so
	// an HS256 token signed with that same value verifies cryptographically
	// — exactly the attack where a forged HS256 token is signed with the
	// public key the verifier hands back. Pinning RS256 via WithValidMethods
	// must reject it before the signature is ever checked. Without the pin
	// this request would authenticate.
	// The claims are otherwise fully valid — accepted audience included —
	// so only the signing-method pin can reject this token. Without the
	// audience the test would stay green even if the RS256 pin vanished.
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, &Claims{
		Email: "attacker@evil.test",
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    "https://dex.test",
			Audience:  jwt.ClaimStrings{DefaultAudience},
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	})
	signed, err := tok.SignedString(testSecret)
	if err != nil {
		t.Fatalf("signing test token: %v", err)
	}

	auth := &Auth{
		Issuer:   "https://dex.test",
		Audience: DefaultAudience,
		KeyFunc:  func(token *jwt.Token) (any, error) { return testSecret, nil },
	}
	handler := auth.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler must not be called for an HS256 token")
	}))

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer "+signed)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for HS256 token, got %d", w.Code)
	}

	// ValidateIDToken is a separate parse with its own method pin.
	if _, err := auth.ValidateIDToken(signed); err == nil {
		t.Error("expected ValidateIDToken to reject an HS256 token")
	}
}

func TestAuthRejectsMissingHeader(t *testing.T) {
	auth := &Auth{
		Issuer:  "https://dex.test",
		KeyFunc: func(token *jwt.Token) (any, error) { return testSecret, nil },
	}

	handler := auth.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	}))

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestAuthRejectsInvalidToken(t *testing.T) {
	auth := &Auth{
		Issuer:  "https://dex.test",
		KeyFunc: func(token *jwt.Token) (any, error) { return testSecret, nil },
	}

	handler := auth.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	}))

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer invalid-token")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestAuthRejectsMalformedHeader(t *testing.T) {
	auth := &Auth{
		Issuer:  "https://dex.test",
		KeyFunc: func(token *jwt.Token) (any, error) { return testSecret, nil },
	}

	handler := auth.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	}))

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "NotBearer some-token")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestUserFromContextReturnsNilWhenNotSet(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	claims := UserFromContext(r.Context())
	if claims != nil {
		t.Error("expected nil claims from empty context")
	}
}
