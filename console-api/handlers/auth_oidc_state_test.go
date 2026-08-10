package handlers

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoginURLSetsTransactionAndPKCE(t *testing.T) {
	a := &AuthHandler{
		DexIssuer:   "https://dex.example.com/dex",
		ClientID:    "kipper-console",
		RedirectURI: "https://console.example.com/callback",
	}
	req := httptest.NewRequest(http.MethodGet, "/auth/login?next=https://console.example.com/apps", nil)
	rec := httptest.NewRecorder()

	a.LoginURL(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	// The transaction cookie must carry the __Host- contract: prefixed name,
	// Secure, HttpOnly, Path=/, no Domain.
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == oidcTxCookieName {
			cookie = c
		}
	}
	require.NotNil(t, cookie, "login must set the transaction cookie")
	assert.Equal(t, "__Host-kipper_oidc_tx", cookie.Name, "the cookie must use the __Host- prefix so a sibling domain can't shadow it")
	assert.True(t, cookie.HttpOnly, "transaction cookie must be HttpOnly")
	assert.True(t, cookie.Secure, "transaction cookie must be Secure")
	assert.Equal(t, "/", cookie.Path, "the __Host- prefix requires Path=/")
	assert.Empty(t, cookie.Domain, "the __Host- prefix forbids a Domain")
	assert.Equal(t, http.SameSiteLaxMode, cookie.SameSite)
	assert.Positive(t, cookie.MaxAge, "the transaction cookie must expire")

	raw, err := base64.RawURLEncoding.DecodeString(cookie.Value)
	require.NoError(t, err)
	var tx oidcTx
	require.NoError(t, json.Unmarshal(raw, &tx))
	require.NotEmpty(t, tx.Verifier, "the cookie must carry a PKCE verifier")
	require.NotEmpty(t, tx.Nonce, "the cookie must carry a nonce")

	// The auth URL's challenge must be the S256 hash of the cookie's verifier,
	// and its state nonce must equal the cookie's nonce — proving the pieces
	// are bound, not just independently present.
	var body struct {
		URL string `json:"url"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	u, err := url.Parse(body.URL)
	require.NoError(t, err)
	q := u.Query()
	assert.Equal(t, "S256", q.Get("code_challenge_method"))
	assert.Equal(t, pkceChallenge(tx.Verifier), q.Get("code_challenge"),
		"the auth URL's challenge must be S256(verifier from the cookie)")

	state, ok := decodeOIDCState(q.Get("state"))
	require.True(t, ok, "state must decode")
	assert.Equal(t, tx.Nonce, state.Nonce, "the state nonce must equal the cookie nonce")
	assert.Equal(t, "https://console.example.com/apps", state.Next)
}

func TestPKCEChallengeMatchesRFC7636Vector(t *testing.T) {
	// RFC 7636 appendix B: this exact verifier hashes to this exact challenge.
	// Pins the S256 encoding (unpadded base64url of SHA-256) against drift.
	const (
		verifier  = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
		challenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	)
	assert.Equal(t, challenge, pkceChallenge(verifier))
	assert.NotContains(t, pkceChallenge(verifier), "=", "challenge must be unpadded base64url")
}
