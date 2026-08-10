package ws

import (
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/getkipper/kipper/console-api/middleware"
)

func TestTokenFromRequest(t *testing.T) {
	jwt := "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ1In0.sig-part_9-8"

	tests := []struct {
		name   string
		header []string
		want   string
	}{
		{"sentinel then token", []string{"kipper.auth, " + jwt}, jwt},
		{"split across values", []string{"kipper.auth", jwt}, jwt},
		{"sentinel only", []string{"kipper.auth"}, ""},
		{"no header", nil, ""},
		{"token before sentinel", []string{jwt + ", kipper.auth"}, jwt},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &http.Request{Header: http.Header{}}
			for _, v := range tt.header {
				r.Header.Add("Sec-WebSocket-Protocol", v)
			}
			if got := tokenFromRequest(r); got != tt.want {
				t.Errorf("tokenFromRequest() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAuthenticatedEmailAudiences(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating RSA key: %v", err)
	}
	keyFunc := jwt.Keyfunc(func(*jwt.Token) (any, error) { return &key.PublicKey, nil })
	sign := func(audience string) string {
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, &middleware.Claims{
			Email: "admin@kipper.test",
			RegisteredClaims: jwt.RegisteredClaims{
				Issuer:    "https://dex.test",
				Audience:  jwt.ClaimStrings{audience},
				ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			},
		})
		signed, serr := tok.SignedString(key)
		if serr != nil {
			t.Fatalf("signing test token: %v", serr)
		}
		return signed
	}

	tests := []struct {
		name     string
		audience string
		wantOK   bool
	}{
		{"console audience", middleware.DefaultAudience, true},
		// The CLI never uses these WebSockets (it goes through the
		// Kubernetes API), so a CLI token must not open them.
		{"cli audience", middleware.CLIAudience, false},
		{"foreign audience", "some-other-client", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &http.Request{Header: http.Header{}}
			r.Header.Add("Sec-WebSocket-Protocol", authSubprotocol+", "+sign(tt.audience))
			email, ok := AuthenticatedEmail(r, "https://dex.test", middleware.DefaultAudience, keyFunc)
			if ok != tt.wantOK {
				t.Fatalf("AuthenticatedEmail ok = %v, want %v", ok, tt.wantOK)
			}
			if tt.wantOK && email != "admin@kipper.test" {
				t.Errorf("email = %q, want admin@kipper.test", email)
			}
		})
	}
}

func TestAuthenticatedEmailRejectsUnverifiedEmail(t *testing.T) {
	// These WebSockets authorize by email on a raw mux, so an explicitly
	// unverified email must not open a stream. An absent claim still works.
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating RSA key: %v", err)
	}
	keyFunc := jwt.Keyfunc(func(*jwt.Token) (any, error) { return &key.PublicKey, nil })
	boolPtr := func(b bool) *bool { return &b }
	sign := func(ev *bool) string {
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, &middleware.Claims{
			Email:         "admin@kipper.test",
			EmailVerified: ev,
			RegisteredClaims: jwt.RegisteredClaims{
				Issuer:    "https://dex.test",
				Audience:  jwt.ClaimStrings{middleware.DefaultAudience},
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
		name   string
		ev     *bool
		wantOK bool
	}{
		{"explicit false", boolPtr(false), false},
		{"explicit true", boolPtr(true), true},
		{"absent", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &http.Request{Header: http.Header{}}
			r.Header.Add("Sec-WebSocket-Protocol", authSubprotocol+", "+sign(tc.ev))
			_, ok := AuthenticatedEmail(r, "https://dex.test", middleware.DefaultAudience, keyFunc)
			if ok != tc.wantOK {
				t.Fatalf("AuthenticatedEmail ok = %v, want %v", ok, tc.wantOK)
			}
		})
	}
}

// TestAuthenticatedEmailValidationInvariants breaks one property of an
// otherwise-valid token per case. AuthenticatedEmail is its own
// ParseWithClaims call, so the middleware invariant tests don't cover it.
func TestAuthenticatedEmailValidationInvariants(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating RSA key: %v", err)
	}
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating RSA key: %v", err)
	}
	keyFunc := jwt.Keyfunc(func(*jwt.Token) (any, error) { return &key.PublicKey, nil })
	validClaims := func() *middleware.Claims {
		return &middleware.Claims{
			Email: "admin@kipper.test",
			RegisteredClaims: jwt.RegisteredClaims{
				Issuer:    "https://dex.test",
				Audience:  jwt.ClaimStrings{middleware.DefaultAudience},
				ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			},
		}
	}
	sign := func(t *testing.T, claims *middleware.Claims, signKey *rsa.PrivateKey) string {
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
			r := &http.Request{Header: http.Header{}}
			r.Header.Add("Sec-WebSocket-Protocol", authSubprotocol+", "+tt.token(t))
			if _, ok := AuthenticatedEmail(r, "https://dex.test", middleware.DefaultAudience, keyFunc); ok != tt.wantOK {
				t.Errorf("AuthenticatedEmail ok = %v, want %v", ok, tt.wantOK)
			}
		})
	}
}

func TestAuthenticatedEmailRejectsNonRS256Algorithm(t *testing.T) {
	// The keyfunc returns the HMAC secret, so this token verifies
	// cryptographically and has fully valid claims — only the RS256
	// method pin can reject it (algorithm-confusion guard).
	secret := []byte("test-secret-key")
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, &middleware.Claims{
		Email: "attacker@evil.test",
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    "https://dex.test",
			Audience:  jwt.ClaimStrings{middleware.DefaultAudience},
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	})
	signed, err := tok.SignedString(secret)
	if err != nil {
		t.Fatalf("signing test token: %v", err)
	}

	r := &http.Request{Header: http.Header{}}
	r.Header.Add("Sec-WebSocket-Protocol", authSubprotocol+", "+signed)
	keyFunc := jwt.Keyfunc(func(*jwt.Token) (any, error) { return secret, nil })
	if _, ok := AuthenticatedEmail(r, "https://dex.test", middleware.DefaultAudience, keyFunc); ok {
		t.Error("expected AuthenticatedEmail to reject an HS256 token")
	}
}
