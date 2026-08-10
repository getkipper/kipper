package handlers

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/console-api/middleware"
)

// testAuthValidator validates RS256 tokens signed with testRSAKey, matching
// the console audience and Dex issuer the test fixtures use.
func testAuthValidator() *middleware.Auth {
	return &middleware.Auth{
		Issuer:   "https://dex.test/dex",
		Audience: middleware.DefaultAudience,
		KeyFunc:  func(_ *jwt.Token) (any, error) { return &testRSAKey.PublicKey, nil },
	}
}

// dexServingIDToken serves a Dex /token endpoint that returns the given signed
// ID token (and a rotated refresh token) on success.
func dexServingIDToken(t *testing.T, idToken string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id_token": %q, "refresh_token": "rotated-refresh", "expires_in": 900}`, idToken)
	}))
}

// callbackRequestWithTx builds a /auth/callback request carrying a valid login
// transaction (matching state nonce + tx cookie) so tests can reach the token
// exchange rather than stopping at the transaction check.
func callbackRequestWithTx(t *testing.T, code, next string) *http.Request {
	t.Helper()
	nonce, err := randomURLToken()
	require.NoError(t, err)
	verifier, err := randomURLToken()
	require.NoError(t, err)
	state := encodeOIDCState(nonce, next)
	body, _ := json.Marshal(map[string]string{"code": code, "state": state})
	req := httptest.NewRequest(http.MethodPost, "/auth/callback", strings.NewReader(string(body)))
	raw, _ := json.Marshal(oidcTx{Nonce: nonce, Verifier: verifier})
	req.AddCookie(&http.Cookie{Name: oidcTxCookieName, Value: base64.RawURLEncoding.EncodeToString(raw)}) //nolint:gosec // test fixture cookie
	return req
}

func TestCallbackRejectsUnverifiedEmail(t *testing.T) {
	// A callback whose ID token carries an explicitly-unverified email must not
	// begin a session: no refresh cookie, no token returned. (The login
	// transaction is valid here, so the request reaches the token exchange.)
	idToken := signEmailVerifiedToken(t, "user@example.com", false)
	dex := dexServingIDToken(t, idToken)
	defer dex.Close()

	a := &AuthHandler{DexIssuer: dex.URL, ClientID: "kipper-console", ClientSecret: "s3cret", Auth: testAuthValidator()}
	rec := httptest.NewRecorder()

	a.Callback(rec, callbackRequestWithTx(t, "auth-code", ""))

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Nil(t, refreshCookieFrom(t, rec), "no refresh cookie may be set for an untrusted email")
}

func TestCallbackRejectsMissingTransactionCookie(t *testing.T) {
	// Without the transaction cookie the browser never started this login, so
	// a code posted into it (login-CSRF) must be refused before any exchange.
	a := &AuthHandler{DexIssuer: "https://dex.invalid", ClientID: "kipper-console", ClientSecret: "s3cret", Auth: testAuthValidator()}
	body, _ := json.Marshal(map[string]string{"code": "stolen-code", "state": encodeOIDCState("attacker-nonce", "")})
	req := httptest.NewRequest(http.MethodPost, "/auth/callback", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()

	a.Callback(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestCallbackRejectsNonceMismatch(t *testing.T) {
	// A state whose nonce doesn't match the transaction cookie is a forged or
	// swapped login: it must be refused AND the suspect cookie cleared.
	a := &AuthHandler{DexIssuer: "https://dex.invalid", ClientID: "kipper-console", ClientSecret: "s3cret", Auth: testAuthValidator()}
	body, _ := json.Marshal(map[string]string{"code": "code", "state": encodeOIDCState("state-nonce", "")})
	req := httptest.NewRequest(http.MethodPost, "/auth/callback", strings.NewReader(string(body)))
	raw, _ := json.Marshal(oidcTx{Nonce: "different-cookie-nonce", Verifier: "v"})
	req.AddCookie(&http.Cookie{Name: oidcTxCookieName, Value: base64.RawURLEncoding.EncodeToString(raw)}) //nolint:gosec // test fixture cookie
	rec := httptest.NewRecorder()

	a.Callback(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	var cleared bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == oidcTxCookieName && c.MaxAge < 0 {
			cleared = true
		}
	}
	assert.True(t, cleared, "the suspect transaction cookie must be cleared")
}

func TestCallbackSendsExactPKCEVerifier(t *testing.T) {
	// The code exchange must carry exactly the verifier from the transaction
	// cookie so Dex can validate PKCE against the challenge it stored.
	nonce, err := randomURLToken()
	require.NoError(t, err)
	verifier, err := randomURLToken()
	require.NoError(t, err)

	var gotVerifier string
	dex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotVerifier = r.PostForm.Get("code_verifier")
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id_token": "x", "refresh_token": "r", "expires_in": 900}`)
	}))
	defer dex.Close()

	body, _ := json.Marshal(map[string]string{"code": "auth-code", "state": encodeOIDCState(nonce, "")})
	req := httptest.NewRequest(http.MethodPost, "/auth/callback", strings.NewReader(string(body)))
	raw, _ := json.Marshal(oidcTx{Nonce: nonce, Verifier: verifier})
	req.AddCookie(&http.Cookie{Name: oidcTxCookieName, Value: base64.RawURLEncoding.EncodeToString(raw)}) //nolint:gosec // test fixture cookie

	a := &AuthHandler{DexIssuer: dex.URL, ClientID: "kipper-console", ClientSecret: "s3cret"} // Auth nil: skip ID-token validation
	rec := httptest.NewRecorder()
	a.Callback(rec, req)

	assert.Equal(t, verifier, gotVerifier, "the code exchange must send the cookie's exact verifier")
}

func TestRefreshRejectsUnverifiedEmail(t *testing.T) {
	// A rotated ID token whose email is no longer trusted must end the session
	// and clear the durable cookie, not silently renew it.
	idToken := signEmailVerifiedToken(t, "user@example.com", false)
	dex := dexServingIDToken(t, idToken)
	defer dex.Close()

	a := &AuthHandler{DexIssuer: dex.URL, ClientID: "kipper-console", ClientSecret: "s3cret", Auth: testAuthValidator()}
	req := httptest.NewRequest(http.MethodPost, "/auth/refresh", nil)
	req.AddCookie(&http.Cookie{Name: refreshCookieName, Value: "old-refresh"}) //nolint:gosec // G124: request cookie carries no Secure/HttpOnly attributes
	rec := httptest.NewRecorder()

	a.Refresh(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	cookie := refreshCookieFrom(t, rec)
	require.NotNil(t, cookie, "the rejection must set an expiring cookie")
	assert.Less(t, cookie.MaxAge, 0, "the durable credential must be cleared on rejection")
}

// fakeDexToken serves a Dex-shaped token endpoint that records the grant it
// received and answers with the configured status.
func fakeDexToken(t *testing.T, status int, gotForm *map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" {
			http.NotFound(w, r)
			return
		}
		require.NoError(t, r.ParseForm())
		*gotForm = map[string]string{}
		for k := range r.PostForm {
			(*gotForm)[k] = r.PostForm.Get(k)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = fmt.Fprint(w, `{"id_token": "fresh-id-token", "refresh_token": "rotated-refresh", "expires_in": 900}`)
		} else {
			_, _ = fmt.Fprint(w, `{"error": "invalid_grant"}`)
		}
	}))
}

func refreshCookieFrom(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == refreshCookieName {
			return c
		}
	}
	return nil
}

func TestRefreshRotatesCookieAndReturnsToken(t *testing.T) {
	var gotForm map[string]string
	dex := fakeDexToken(t, http.StatusOK, &gotForm)
	defer dex.Close()

	a := &AuthHandler{DexIssuer: dex.URL, ClientID: "kipper-console", ClientSecret: "s3cret"}
	req := httptest.NewRequest(http.MethodPost, "/auth/refresh", nil)
	req.AddCookie(&http.Cookie{Name: refreshCookieName, Value: "old-refresh"}) //nolint:gosec // G124: a request cookie carries no Secure/HttpOnly attributes; they exist only on Set-Cookie
	rec := httptest.NewRecorder()

	a.Refresh(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "fresh-id-token")
	assert.Equal(t, "refresh_token", gotForm["grant_type"])
	assert.Equal(t, "old-refresh", gotForm["refresh_token"])
	assert.Equal(t, "s3cret", gotForm["client_secret"], "the confidential exchange stays server-side")

	cookie := refreshCookieFrom(t, rec)
	require.NotNil(t, cookie, "the rotated refresh token must land back in the cookie")
	assert.Equal(t, "rotated-refresh", cookie.Value)
	assert.True(t, cookie.HttpOnly)
	assert.True(t, cookie.Secure)
	assert.Equal(t, http.SameSiteStrictMode, cookie.SameSite)
	assert.Equal(t, refreshCookiePath, cookie.Path,
		"the durable credential must travel to its one consuming endpoint only")
}

func TestRefreshWithoutCookieIsUnauthorized(t *testing.T) {
	a := &AuthHandler{DexIssuer: "https://dex.invalid/dex", ClientID: "kipper-console"}
	rec := httptest.NewRecorder()
	a.Refresh(rec, httptest.NewRequest(http.MethodPost, "/auth/refresh", nil))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestRefreshFailureClearsCookie(t *testing.T) {
	var gotForm map[string]string
	dex := fakeDexToken(t, http.StatusBadRequest, &gotForm)
	defer dex.Close()

	a := &AuthHandler{DexIssuer: dex.URL, ClientID: "kipper-console", ClientSecret: "s3cret"}
	req := httptest.NewRequest(http.MethodPost, "/auth/refresh", nil)
	req.AddCookie(&http.Cookie{Name: refreshCookieName, Value: "dead-refresh"}) //nolint:gosec // G124: a request cookie carries no Secure/HttpOnly attributes; they exist only on Set-Cookie
	rec := httptest.NewRecorder()

	a.Refresh(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	cookie := refreshCookieFrom(t, rec)
	require.NotNil(t, cookie, "a dead refresh credential must be cleared, or the browser retries it forever")
	assert.Empty(t, cookie.Value)
	assert.Negative(t, cookie.MaxAge)
}

func TestLogoutClearsRefreshCookie(t *testing.T) {
	a := &AuthHandler{}
	rec := httptest.NewRecorder()
	a.Logout(rec, httptest.NewRequest(http.MethodPost, "/auth/logout", nil))

	cookie := refreshCookieFrom(t, rec)
	require.NotNil(t, cookie, "logout must kill the durable half of the session")
	assert.Empty(t, cookie.Value)
	assert.Negative(t, cookie.MaxAge)
}

func TestRefreshRejectsUnusableSuccessResponse(t *testing.T) {
	// A 200 whose body carries no rotated refresh token (or no ID token) is
	// a dead session wearing a success status; the durable cookie must die
	// with it.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id_token": "fresh", "expires_in": 900}`)
	}))
	defer srv.Close()

	a := &AuthHandler{DexIssuer: srv.URL, ClientID: "kipper-console", ClientSecret: "s3cret"}
	req := httptest.NewRequest(http.MethodPost, "/auth/refresh", nil)
	req.AddCookie(&http.Cookie{Name: refreshCookieName, Value: "old"}) //nolint:gosec // G124: request cookie
	rec := httptest.NewRecorder()
	a.Refresh(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	cookie := refreshCookieFrom(t, rec)
	require.NotNil(t, cookie)
	assert.Empty(t, cookie.Value)
	assert.Negative(t, cookie.MaxAge)
}
