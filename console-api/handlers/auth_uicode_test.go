package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"

	"github.com/getkipper/kipper/console-api/middleware"
)

// signAudienceToken signs an RS256 Dex-style token with an explicit audience
// set, so the /auth/ui-code audience boundary can be exercised.
func signAudienceToken(t *testing.T, email string, aud []string) string {
	t.Helper()
	claims := &middleware.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    "https://dex.test/dex",
			Audience:  jwt.ClaimStrings(aud),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
		Email: email,
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(testRSAKey)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return signed
}

// signEmailVerifiedToken signs a console-audience token with an explicit
// email_verified value, so the email-trust gate can be exercised.
func signEmailVerifiedToken(t *testing.T, email string, emailVerified bool) string {
	t.Helper()
	claims := &middleware.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    "https://dex.test/dex",
			Audience:  jwt.ClaimStrings{middleware.DefaultAudience},
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
		Email:         email,
		EmailVerified: &emailVerified,
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(testRSAKey)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return signed
}

func TestUISessionCode_RejectsUnverifiedEmail(t *testing.T) {
	// An unverified email must not mint an SSO code: UISessionCode authorizes
	// through ValidateRequest, which now gates on email trust.
	f := uiHandler(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))
	token := signEmailVerifiedToken(t, "ops@example.com", false)

	w := httptest.NewRecorder()
	f.h.UISessionCode(w, uiCodeRequest(t, token, uiHost))

	assert.Equal(t, http.StatusUnauthorized, w.Code, "an explicitly-unverified email must not mint an SSO code")
}

func uiCodeRequest(t *testing.T, token, host string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/auth/ui-code", strings.NewReader(`{"host":"`+host+`"}`))
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

func TestUISessionCode_MintsForConsoleToken(t *testing.T) {
	f := uiHandler(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))
	token := signAudienceToken(t, "ops@example.com", []string{middleware.DefaultAudience})

	w := httptest.NewRecorder()
	f.h.UISessionCode(w, uiCodeRequest(t, token, uiHost))

	assert.Equal(t, http.StatusOK, w.Code)
	var body struct {
		Code string `json:"code"`
	}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.NotEmpty(t, body.Code, "a console-audience operator must get an SSO code")
}

func TestUISessionCode_RejectsCLIAudience(t *testing.T) {
	f := uiHandler(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))
	token := signAudienceToken(t, "ops@example.com", []string{middleware.CLIAudience})

	w := httptest.NewRecorder()
	f.h.UISessionCode(w, uiCodeRequest(t, token, uiHost))

	assert.Equal(t, http.StatusForbidden, w.Code, "a CLI token must not open browser SSO")
}

func TestUISessionCode_RejectsMultiAudienceContainingConsole(t *testing.T) {
	// The audience check is equality, not membership: a token that lists the
	// console audience beside the CLI one must not open SSO.
	f := uiHandler(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))
	token := signAudienceToken(t, "ops@example.com", []string{middleware.CLIAudience, middleware.DefaultAudience})

	w := httptest.NewRecorder()
	f.h.UISessionCode(w, uiCodeRequest(t, token, uiHost))

	assert.Equal(t, http.StatusForbidden, w.Code, "a multi-audience token containing the console audience must still be refused")
}

func TestUISessionCode_RejectsUserWithoutRole(t *testing.T) {
	f := uiHandler(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))
	token := signAudienceToken(t, "stranger@example.com", []string{middleware.DefaultAudience})

	w := httptest.NewRecorder()
	f.h.UISessionCode(w, uiCodeRequest(t, token, uiHost))

	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestUISessionCode_RejectsConsoleHostTarget(t *testing.T) {
	// A code may open a service UI, never the console itself.
	f := uiHandler(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))
	token := signAudienceToken(t, "ops@example.com", []string{middleware.DefaultAudience})

	w := httptest.NewRecorder()
	f.h.UISessionCode(w, uiCodeRequest(t, token, "console.example.com"))

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUISessionCode_RejectsForeignHost(t *testing.T) {
	f := uiHandler(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))
	token := signAudienceToken(t, "ops@example.com", []string{middleware.DefaultAudience})

	w := httptest.NewRecorder()
	f.h.UISessionCode(w, uiCodeRequest(t, token, "mailhog.attacker.example.net"))

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUISessionCode_NilWiringFailsClosed(t *testing.T) {
	h := &AuthHandler{Auth: &middleware.Auth{Audience: middleware.DefaultAudience}}
	token := signAudienceToken(t, "ops@example.com", []string{middleware.DefaultAudience})

	w := httptest.NewRecorder()
	h.UISessionCode(w, uiCodeRequest(t, token, uiHost))

	assert.Equal(t, http.StatusInternalServerError, w.Code)
}
