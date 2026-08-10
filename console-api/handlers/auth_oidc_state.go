package handlers

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"time"
)

const (
	// oidcTxCookieName holds the login-transaction secret (state nonce + PKCE
	// verifier) while the browser is away at Dex. The __Host- prefix is
	// load-bearing, not cosmetic: a tenant service UI is a sibling subdomain
	// under the cluster domain and could otherwise plant a same-named
	// Domain=<cluster> cookie that shadows this host-only one, letting the
	// attacker choose the nonce+verifier and recreate login CSRF. The prefix
	// makes the browser refuse any cookie of this name carrying Domain or
	// served without Secure, so only the console's own host-only cookie exists.
	oidcTxCookieName = "__Host-kipper_oidc_tx" //nolint:gosec // cookie name, not a credential
	// oidcTxCookiePath must be "/" for the __Host- prefix. HttpOnly keeps it
	// out of scripts; only the auth handlers read it.
	oidcTxCookiePath = "/"
	// oidcTxCookieTTL bounds how long a started login stays valid.
	//
	// One login transaction lives in this single cookie, so two tabs starting
	// a login concurrently overwrite each other and the earlier one must be
	// restarted. This is an accepted trade-off: concurrent multi-tab login
	// initiation is rare, and the failure mode is a retry, never a security
	// weakening (the nonce/verifier are never exposed).
	oidcTxCookieTTL = 600
)

// oidcState is packed into the OAuth state parameter. Nonce binds the callback
// to the browser that started the login — its match lives in the HttpOnly
// transaction cookie — which defeats login-CSRF / session-swap. Next is the
// post-login redirect target.
type oidcState struct {
	Nonce string `json:"n"`
	Next  string `json:"u,omitempty"`
}

// oidcTx is the login-transaction secret held in the HttpOnly cookie: the nonce
// half of the state pair and the PKCE code verifier, neither of which an
// attacker can read or forge across origins.
type oidcTx struct {
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
}

// randomURLToken returns 32 bytes of CSPRNG entropy as an unpadded base64url
// string, suitable for a nonce or a PKCE verifier (32 bytes → 43 chars, within
// RFC 7636's 43-128 range).
func randomURLToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// pkceChallenge returns the S256 code challenge for a verifier.
func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func encodeOIDCState(nonce, next string) string {
	raw, _ := json.Marshal(oidcState{Nonce: nonce, Next: next}) //nolint:errchkjson // fixed struct, cannot fail
	return base64.RawURLEncoding.EncodeToString(raw)
}

// decodeOIDCState reverses encodeOIDCState. It reports false for malformed
// input or a missing nonce, so a login can never proceed without one.
func decodeOIDCState(state string) (oidcState, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(state)
	if err != nil {
		return oidcState{}, false
	}
	var s oidcState
	if err := json.Unmarshal(raw, &s); err != nil || s.Nonce == "" {
		return oidcState{}, false
	}
	return s, true
}

func setOIDCTxCookie(w http.ResponseWriter, tx oidcTx) {
	raw, _ := json.Marshal(tx) //nolint:errchkjson // fixed struct, cannot fail
	http.SetCookie(w, &http.Cookie{
		Name:     oidcTxCookieName,
		Value:    base64.RawURLEncoding.EncodeToString(raw),
		Path:     oidcTxCookiePath,
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   oidcTxCookieTTL,
	})
}

func readOIDCTxCookie(r *http.Request) (oidcTx, bool) {
	c, err := r.Cookie(oidcTxCookieName)
	if err != nil || c.Value == "" {
		return oidcTx{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		return oidcTx{}, false
	}
	var tx oidcTx
	if err := json.Unmarshal(raw, &tx); err != nil || tx.Nonce == "" || tx.Verifier == "" {
		return oidcTx{}, false
	}
	return tx, true
}

func clearOIDCTxCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     oidcTxCookieName,
		Value:    "",
		Path:     oidcTxCookiePath,
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
	})
}

// nonceMatches compares the state and cookie nonces in constant time.
func nonceMatches(a, b string) bool {
	return a != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
