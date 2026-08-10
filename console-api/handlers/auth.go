package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/getkipper/kipper/console-api/middleware"
	"github.com/getkipper/kipper/console-api/share"
	"github.com/getkipper/kipper/console-api/uisession"
)

// AuthHandler handles the OAuth2 code exchange with Dex.
type AuthHandler struct {
	DexIssuer    string
	ClientID     string
	ClientSecret string
	RedirectURI  string
	// Auth runs token validation for the /auth/check forwardAuth
	// endpoint. Same machinery the middleware chain uses, so the
	// two paths can't drift on accepted issuers or key sources.
	Auth *middleware.Auth
	// UIDomain is the cluster's base domain (e.g. "example.com", leading
	// dot tolerated). It scopes which hosts a post-login redirect or an
	// SSO-code mint may target: the console itself, or a service UI under
	// this domain. It must stay empty on shared *.kipper.run clusters, where
	// every tenant host is a sibling under the same apex — a non-empty value
	// there would make another tenant's host a valid redirect/mint target
	// (an open redirect). The installer sets it via UIDomainFor, which
	// returns "" for *.kipper.run precisely for this reason.
	UIDomain string
	// ConsoleURL is where unauthenticated /auth/check requests get
	// redirected to so the user can log in and come back. Without
	// it the endpoint returns 401 and the user sees a blank Traefik
	// page rather than a login prompt.
	ConsoleURL string
	// ShareKeyring returns the current share-link signing keyring, or
	// ok=false when share links are unavailable (no keyring yet, or a
	// transient load failure — fail closed). Kept behind a func so the
	// handler stays testable and main.go can cache the Secret read.
	// Nil disables the share path entirely.
	ShareKeyring func() (*share.Keyring, bool)
	// ShareGrants resolves token ids to their stored grants. Nil
	// disables the share path entirely — a token without a live grant
	// is worthless by design.
	ShareGrants *share.GrantStore
	// ServiceUID reports the live Service CR's UID, or ok=false when
	// the service is gone or unreadable. The gate compares it against
	// the token's uid claim so links minted for a deleted service can
	// never open its recreated namesake, even if the grant survived a
	// bypassed finalizer. Nil disables the share path entirely.
	ServiceUID func(ctx context.Context, namespace, name string) (string, bool)
	// Now supplies the clock for share-token validation; nil means
	// time.Now. Tests set it.
	Now func() time.Time

	// UISessionKeyring returns the current UI-session signing keyring, or
	// ok=false when unavailable (fail closed). The backing cache self-heals
	// a missing Secret, so this covers both minting and validation. Nil
	// disables the UI-session path entirely.
	UISessionKeyring func() (*uisession.Keyring, bool)
	// UISessions is the server-side session record store: the authoritative
	// liveness switch and single-use guard for SSO codes. Nil disables the
	// UI-session path entirely.
	UISessions *uisession.RecordStore
	// UISessionReset rotates the signing keyring and is the keyring half of
	// revoke-all. Nil means revoke-all cannot reset keys (records only).
	UISessionReset func(ctx context.Context) error
	// RoleOf returns the caller's global role for an email, "" when none.
	// Gates who may mint an SSO code and backs the session role check as
	// defense-in-depth. Nil disables the UI-session path entirely.
	RoleOf func(email string) string
}

// shareCookieName is the browser cookie that carries a validated share
// token. The __Host- prefix makes browsers refuse it if it ever
// carries a Domain attribute or is set over plain HTTP, which defeats
// cookie-tossing from a sibling subdomain. The service+namespace suffix
// keeps one host's share cookie from shadowing another's.
func shareCookieName(host string) string {
	// host is already canonicalised; the first label is <svc>-<ns>.
	label := host
	if i := strings.IndexByte(host, '.'); i > 0 {
		label = host[:i]
	}
	return "__Host-kipper-share-" + label
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	IDToken      string `json:"id_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
}

// refreshCookieName holds the Dex refresh token as an HttpOnly cookie scoped
// to the refresh endpoint only. The browser never exposes it to script, so
// XSS can at worst mint short-lived ID tokens while a page is open — it
// cannot steal the durable credential itself.
const refreshCookieName = "kipper_refresh"

// refreshCookiePath is the exact endpoint path as the browser sees it
// through the console ingress; the cookie travels nowhere else.
const refreshCookiePath = "/auth/refresh"

// refreshCookieMaxAge matches Dex's absolute refresh-token lifetime (720h):
// the cookie and the credential inside it expire together.
const refreshCookieMaxAge = 720 * 60 * 60

// dexHTTPClient bounds every token-endpoint call so a stalled Dex cannot
// hold handler goroutines open indefinitely.
var dexHTTPClient = &http.Client{Timeout: 15 * time.Second}

type loginResponse struct {
	Token string `json:"token"`
	Email string `json:"email"`
	// Next, when present, is the URL the frontend should navigate
	// to after login. Carried through the OIDC `state` parameter
	// so a user who opened a service UI URL while signed out
	// lands back on that URL after authenticating, instead of
	// the dashboard.
	Next string `json:"next,omitempty"`
}

// LoginURL returns the Dex authorization URL for the frontend to redirect to.
// Accepts an optional `next` query parameter that the callback
// echoes back so deep-links survive the OIDC round trip.
// GET /auth/login?next=<url>
func (a *AuthHandler) LoginURL(w http.ResponseWriter, r *http.Request) {
	next := r.URL.Query().Get("next")

	// Start a login transaction: a state nonce bound to this browser via an
	// HttpOnly cookie, and a PKCE verifier whose S256 challenge goes to Dex.
	// The callback proves both, so a code obtained by an attacker can't be
	// injected into a victim's browser (login-CSRF) or exchanged elsewhere.
	nonce, err := randomURLToken()
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to start login")
		return
	}
	verifier, err := randomURLToken()
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to start login")
		return
	}
	setOIDCTxCookie(w, oidcTx{Nonce: nonce, Verifier: verifier})

	state := encodeOIDCState(nonce, next)
	authURL := fmt.Sprintf("%s/auth?client_id=%s&redirect_uri=%s&response_type=code&scope=%s&state=%s&code_challenge=%s&code_challenge_method=S256",
		a.DexIssuer,
		url.QueryEscape(a.ClientID),
		url.QueryEscape(a.RedirectURI),
		url.QueryEscape("openid email profile offline_access"),
		url.QueryEscape(state),
		url.QueryEscape(pkceChallenge(verifier)),
	)
	respondJSON(w, http.StatusOK, map[string]string{"url": authURL})
}

// Callback exchanges an OAuth2 authorization code for a JWT token.
// Also returns the decoded `next` URL from the OIDC state so the
// frontend can route the user back to wherever the login flow
// started — typically a service UI subdomain they tried to open
// while signed out.
// POST /auth/callback { "code": "...", "state": "..." }
func (a *AuthHandler) Callback(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code  string `json:"code"`
		State string `json:"state"`
	}
	if err := decodeJSON(r, &req); err != nil || req.Code == "" {
		respondError(w, http.StatusBadRequest, "code is required")
		return
	}

	// Verify the login transaction before spending the code: the state's nonce
	// must match the one in the HttpOnly cookie this browser got when it
	// started the login. A cross-site attacker can neither read nor set that
	// cookie, so a code they obtained can't be replayed into a victim's
	// session. The cookie is single-use.
	tx, ok := readOIDCTxCookie(r)
	if !ok {
		respondError(w, http.StatusBadRequest, "login session missing or expired")
		return
	}
	clearOIDCTxCookie(w)
	state, ok := decodeOIDCState(req.State)
	if !ok || !nonceMatches(state.Nonce, tx.Nonce) {
		respondError(w, http.StatusUnauthorized, "login state mismatch")
		return
	}

	// Exchange the code for tokens at Dex's token endpoint. The PKCE verifier
	// binds the code to this login: Dex rejects the exchange unless its S256
	// hash matches the challenge sent in the authorization request.
	tokenURL := a.DexIssuer + "/token"
	data := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {req.Code},
		"redirect_uri":  {a.RedirectURI},
		"client_id":     {a.ClientID},
		"client_secret": {a.ClientSecret},
		"code_verifier": {tx.Verifier},
	}

	resp, err := dexHTTPClient.Post(tokenURL, "application/x-www-form-urlencoded", strings.NewReader(data.Encode())) //nolint:gosec,noctx // URL from trusted Dex config
	if err != nil {
		respondError(w, http.StatusBadGateway, "failed to contact identity provider")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		respondError(w, http.StatusUnauthorized, fmt.Sprintf("token exchange failed: %s", string(body)))
		return
	}

	var tokens tokenResponse
	if err := json.Unmarshal(body, &tokens); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to parse token response")
		return
	}

	// A 200 with an unusable body must not become a nominal login: an empty
	// token would strand the user in a confusing half-session.
	if tokens.IDToken == "" || tokens.ExpiresIn <= 0 {
		respondError(w, http.StatusBadGateway, "identity provider returned an unusable token response")
		return
	}

	// Validate the ID token Dex just issued the same way every inbound token
	// is (RS256, Dex issuer, console audience, and email trust) BEFORE
	// establishing any session. A token whose email the issuer marked
	// unverified must not begin a login, so validation happens before the
	// refresh cookie is set rather than after. No forwardAuth cookie is set:
	// service-UI SSO runs on per-host __Host- session cookies minted at the
	// gate, fully decoupled from the console session.
	email := ""
	if a.Auth != nil {
		claims, verr := a.Auth.ValidateIDToken(tokens.IDToken)
		if verr != nil {
			respondError(w, http.StatusUnauthorized, "identity provider returned an untrusted token")
			return
		}
		email = claims.Email
	}

	a.setRefreshCookie(w, tokens.RefreshToken)

	// Return the ID token plus the decoded `next` URL (if any) so
	// the frontend can route the user back to a service UI they
	// opened while signed out.
	respondJSON(w, http.StatusOK, loginResponse{
		Token: tokens.IDToken,
		Email: email,
		Next:  a.safeRedirectTarget(state.Next),
	})
}

// safeRedirectTarget validates that a `next` URL is safe to send the user
// to and returns it rebuilt from its canonical authority. It rejects
// anything that isn't an absolute https URL whose host is one of our own —
// the console, or a service UI under the cluster domain — and strips
// userinfo, ports, and trailing dots so the returned link can't carry a
// deceptive authority. Without this a crafted next (an attacker host, or
// https://user@console.example.com bouncing off our real login) would let
// an attacker phish users.
func (a *AuthHandler) safeRedirectTarget(next string) string {
	if next == "" {
		return ""
	}
	u, err := url.Parse(next)
	if err != nil {
		return ""
	}
	// Userinfo (user[:pass]@host) makes a foreign-looking URL read as one
	// of ours; refuse it outright rather than trying to sanitise it.
	if u.Scheme != "https" || u.Host == "" || u.User != nil {
		return ""
	}
	host, err := share.CanonicalHost(u.Host)
	if err != nil {
		return ""
	}
	host = strings.TrimSuffix(host, ".")
	if !a.redirectHostAllowed(host) {
		return ""
	}
	// Rebuild from the canonical host so the returned URL carries no port,
	// userinfo, or non-canonical spelling — only the path and query the
	// user was heading to.
	clean := url.URL{Scheme: "https", Host: host, Path: u.Path, RawQuery: u.RawQuery, Fragment: u.Fragment}
	return clean.String()
}

// redirectHostAllowed reports whether a post-login redirect may target
// host. Allowed hosts are the console itself and any service UI under the
// cluster domain (UIDomain, e.g. ".example.com"). Everything else —
// including bare externally-controlled authorities — is refused, so a
// forwarded Host header can't turn our login into an open redirect.
func (a *AuthHandler) redirectHostAllowed(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" {
		return false
	}
	if ch := a.consoleHost(); ch != "" && host == ch {
		return true
	}
	if base := strings.TrimPrefix(a.UIDomain, "."); base != "" {
		base = strings.ToLower(base)
		if host == base || strings.HasSuffix(host, "."+base) {
			return true
		}
	}
	return false
}

// consoleHost is the bare hostname of ConsoleURL, or "" if unset or
// unparseable.
func (a *AuthHandler) consoleHost() string {
	if a.ConsoleURL == "" {
		return ""
	}
	u, err := url.Parse(a.ConsoleURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// Check is the Traefik forwardAuth target: every request to a
// service UI hostname goes through this endpoint first. Returns
// 200 + X-Auth-User if the request carries a valid JWT (header
// or cookie); 302s to the console login otherwise so the browser
// rounds through Dex and lands back at the original URL.
// GET /auth/check
func (a *AuthHandler) Check(w http.ResponseWriter, r *http.Request) {
	// Every gate response is a per-request auth decision, including the
	// misconfiguration and error paths below; nothing here may land in a
	// shared cache.
	w.Header().Set("Cache-Control", "no-store")
	if a.Auth == nil {
		// Misconfiguration — refuse rather than silently allow.
		http.Error(w, "forwardAuth not configured", http.StatusInternalServerError)
		return
	}

	// Share links: a bounded, Dex-free way into one service UI. Tried
	// before Dex so a valid share credential is accepted, but a share
	// failure never blocks a Dex user — it falls through to the
	// unchanged Dex paths below. Confined to this endpoint, so a share
	// token can't reach the REST API or WebSocket validators.
	if user, done := a.tryShare(w, r); done {
		// The first-visit path already wrote its redirect and reports no
		// user; writing again would only log a superfluous WriteHeader.
		if user != "" {
			w.Header().Set("X-Auth-User", user)
			w.WriteHeader(http.StatusOK)
		}
		return
	}

	// UI sessions: the browser path into a service UI. A valid per-host
	// session cookie (or a fresh single-use SSO code) authenticates the
	// request on identity the operator proved to the console, with no Dex
	// token ever riding in a cookie. Like tryShare, a miss here falls
	// through to the login redirect rather than blocking.
	if user, done := a.tryUISession(w, r); done {
		if user != "" {
			// Pass user identity to the backend service UI for services
			// that grow their own user concept. Most UIs today (MailHog,
			// etc.) ignore it; harmless if present.
			w.Header().Set("X-Auth-User", user)
			w.WriteHeader(http.StatusOK)
		}
		return
	}

	// Not authenticated → redirect to console login with `next`
	// pointing back at the original URL. Traefik forwards the
	// X-Forwarded-{Proto,Host,Uri} headers so we can reconstruct it.
	if a.ConsoleURL == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// The share token must never ride along into the login URL: from
	// there it would land in the console's request logs and the user's
	// browser history, both of which outlive the redirect.
	uri := stripGateParams(r.Header.Get("X-Forwarded-Uri"))
	if uri == "" {
		uri = "/"
	}
	// Build next= only for a forwarded host we recognise as our own, and
	// always over https. An unrecognised or missing host drops next
	// entirely (the user lands on the bare dashboard) rather than letting
	// an attacker-chosen authority ride the redirect back out.
	next := ""
	if host, herr := share.CanonicalHost(r.Header.Get("X-Forwarded-Host")); herr == nil && a.redirectHostAllowed(host) {
		next = fmt.Sprintf("https://%s%s", host, uri)
	}
	loginURL := strings.TrimRight(a.ConsoleURL, "/") + "/login"
	if next != "" {
		loginURL += "?next=" + url.QueryEscape(next)
	}
	http.Redirect(w, r, loginURL, http.StatusFound)
}

// tryShare handles the share-link half of forwardAuth. It returns
// (user, true) when the request carries a valid share credential for
// the forwarded host, having already written any redirect/Set-Cookie
// needed. It returns done=false when there is no share credential or an
// invalid one, in which case the caller continues to the Dex paths. An
// invalid or stale share cookie is cleared on the way through so a
// dead link doesn't wedge the browser.
func (a *AuthHandler) tryShare(w http.ResponseWriter, r *http.Request) (string, bool) {
	if a.ShareKeyring == nil || a.ShareGrants == nil || a.ServiceUID == nil {
		return "", false
	}
	// The share cookie is __Host-prefixed and Secure, so it only rides
	// over https anyway. Enforce that here too: never mint a redirect or
	// accept a share credential on a plaintext hop.
	if !strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		return "", false
	}
	host, err := share.CanonicalHost(r.Header.Get("X-Forwarded-Host"))
	if err != nil {
		return "", false
	}
	kr, ok := a.ShareKeyring()
	if !ok {
		return "", false
	}
	now := time.Now
	if a.Now != nil {
		now = a.Now
	}
	cookieName := shareCookieName(host)

	// First visit: token in the URL. Validate, then redirect to the
	// same URL with the token stripped and the credential moved into a
	// host-only cookie, so the address bar (and anything that later
	// copies it) no longer carries the capability.
	uri := r.Header.Get("X-Forwarded-Uri")
	if raw := shareParam(uri); raw != "" {
		claims := a.validShareClaims(r.Context(), kr, raw, host, now())
		if claims == nil {
			a.clearShareCookie(w, cookieName)
			return "", false
		}
		http.SetCookie(w, shareCookie(cookieName, raw, claims.ExpiresAt.Time, now()))
		// host is canonicalised (CanonicalHost) and equals the token's
		// validated audience, so the redirect target is provably one of
		// our own service-UI hosts, not an attacker-controlled value.
		//nolint:gosec // G710: host is validated against the token audience
		http.Redirect(w, r, cleanURL(host, uri), http.StatusFound)
		return "", true
	}

	// Return visit: token in the cookie.
	c, cerr := r.Cookie(cookieName)
	if cerr != nil || c.Value == "" {
		return "", false
	}
	claims := a.validShareClaims(r.Context(), kr, c.Value, host, now())
	if claims == nil {
		a.clearShareCookie(w, cookieName)
		return "", false
	}
	return "share:" + share.JTIPrefix(claims.ID), true
}

// validShareClaims runs the full v1 share check: token signature and
// claims against the keyring, the grant's existence (revocation), every
// duplicated field between grant and claims, and finally the live
// Service's UID — a link minted for a deleted service can never open a
// recreated namesake. Any failure returns nil, indistinguishably.
func (a *AuthHandler) validShareClaims(ctx context.Context, kr *share.Keyring, token, host string, now time.Time) *share.Claims {
	claims, err := share.ValidateGrantToken(kr, token, host, now)
	if err != nil {
		return nil
	}
	grant := a.ShareGrants.Get(ctx, claims.ID)
	if grant == nil || !grant.Matches(claims, host) {
		return nil
	}
	uid, ok := a.ServiceUID(ctx, grant.Namespace, grant.Service)
	if !ok || uid != claims.ServiceUID {
		return nil
	}
	return claims
}

// tryUISession handles the UI-session half of forwardAuth. It returns
// (email, true) when the request carries a valid session cookie for the
// forwarded host (renewing the cookie in place when it is close to
// expiry), or ("", true) when a single-use SSO code in the URL was
// redeemed into a fresh session (having already written the Set-Cookie
// and the 302 to the code-stripped URL). It returns done=false when there
// is no UI-session credential or an invalid one, so the caller falls
// through to the login redirect. Every failure is refused
// indistinguishably — no oracle for why a credential was rejected.
func (a *AuthHandler) tryUISession(w http.ResponseWriter, r *http.Request) (string, bool) {
	if a.UISessionKeyring == nil || a.UISessions == nil || a.RoleOf == nil {
		return "", false
	}
	// The session cookie is __Host-prefixed and Secure; never accept a UI
	// credential or mint one on a plaintext hop.
	if !strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		return "", false
	}
	host, err := share.CanonicalHost(r.Header.Get("X-Forwarded-Host"))
	if err != nil {
		return "", false
	}
	kr, ok := a.UISessionKeyring()
	if !ok {
		return "", false
	}
	now := time.Now
	if a.Now != nil {
		now = a.Now
	}
	cookieName := uisession.CookieName(host)
	uri := r.Header.Get("X-Forwarded-Uri")

	// First visit: a single-use SSO code in the URL. Validate it, then
	// create the session record keyed by the code's sid — Create-atomicity
	// makes redemption single-use across any replica count (a replay hits
	// AlreadyExists). On success, mint the session cookie and 302 to the
	// same URL with the code stripped so it never lands in history or logs.
	if code := ssoParam(uri); code != "" {
		v, verr := uisession.ValidateCode(kr, code, host, now())
		if verr != nil {
			return "", false
		}
		authTime := now()
		if cerr := a.UISessions.Create(r.Context(), v.SID, v.Sub, v.Email, host, authTime, authTime.Add(uisession.SessionAbsoluteTTL)); cerr != nil {
			// Replay (AlreadyExists) or a store error — refuse, no oracle.
			return "", false
		}
		tok, merr := uisession.MintSession(kr, v.Sub, v.Email, host, v.SID, authTime, authTime)
		if merr != nil {
			return "", false
		}
		http.SetCookie(w, sessionCookie(cookieName, tok, sessionCookieExpiry(authTime, authTime), now()))
		// host is canonicalised and equals the code's validated audience, so
		// the redirect target is provably one of our own service-UI hosts.
		//nolint:gosec // G710: host is validated against the code audience
		http.Redirect(w, r, cleanURL(host, uri), http.StatusFound)
		return "", true
	}

	// Return visit: the session cookie.
	c, cerr := r.Cookie(cookieName)
	if cerr != nil || c.Value == "" {
		return "", false
	}
	v, verr := uisession.ValidateSession(kr, c.Value, host, now())
	if verr != nil {
		a.clearUICookie(w, cookieName)
		return "", false
	}
	// Authoritative liveness is the record (fresh, fails closed on any API
	// error). The role check is defense-in-depth and no longer load-bearing
	// for the revocation SLA (see the record store's package comment).
	if !a.UISessions.Live(r.Context(), v.SID) {
		a.clearUICookie(w, cookieName)
		return "", false
	}
	if a.RoleOf(v.Email) == "" {
		a.clearUICookie(w, cookieName)
		return "", false
	}
	a.maybeRenewSession(w, kr, v, host, now())
	return v.Email, true
}

// maybeRenewSession re-mints the session cookie in place when less than the
// renew window remains and the session is still inside its absolute cap.
// The sid and auth_time carry over, so renewal slides the idle window
// without extending the 12h ceiling. Concurrent renewals are harmless
// (same sid, last write wins).
func (a *AuthHandler) maybeRenewSession(w http.ResponseWriter, kr *uisession.Keyring, v *uisession.Validated, host string, now time.Time) {
	if v.Expiry.Sub(now) >= uisession.SessionRenewBefore {
		return
	}
	absCap := v.AuthTime.Add(uisession.SessionAbsoluteTTL)
	if !now.Before(absCap) {
		return
	}
	tok, err := uisession.MintSession(kr, v.Sub, v.Email, host, v.SID, v.AuthTime, now)
	if err != nil {
		return
	}
	http.SetCookie(w, sessionCookie(uisession.CookieName(host), tok, sessionCookieExpiry(v.AuthTime, now), now))
}

// UISessionCode mints a single-use SSO code for the console→service-UI
// handoff. It requires a console-audience Dex token (a stolen CLI token
// must never open browser SSO) and a live role, and only for a host that
// is one of our own service UIs under the cluster domain.
// POST /auth/ui-code { "host": "mailhog-dev.example.com" }
func (a *AuthHandler) UISessionCode(w http.ResponseWriter, r *http.Request) {
	if a.Auth == nil || a.UISessionKeyring == nil || a.RoleOf == nil {
		respondError(w, http.StatusInternalServerError, "ui sessions not configured")
		return
	}
	claims, err := a.Auth.ValidateRequest(r)
	if err != nil {
		respondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	// extractAndValidate admits the CLI audience too; the browser-SSO gate
	// is console-only. Require the console audience exactly — a single-entry
	// audience equal to the console client — so a token that merely lists the
	// console audience alongside the CLI one (or any other) cannot open SSO.
	if !isExactlyConsoleAudience(claims, a.Auth.Audience) {
		respondError(w, http.StatusForbidden, "forbidden")
		return
	}
	if a.RoleOf(claims.Email) == "" {
		respondError(w, http.StatusForbidden, "no role assigned")
		return
	}
	var req struct {
		Host string `json:"host"`
	}
	if derr := decodeJSON(r, &req); derr != nil || req.Host == "" {
		respondError(w, http.StatusBadRequest, "host is required")
		return
	}
	host, herr := share.CanonicalHost(req.Host)
	if herr != nil || !a.redirectHostAllowed(host) || host == a.consoleHost() {
		respondError(w, http.StatusBadRequest, "invalid host")
		return
	}
	kr, ok := a.UISessionKeyring()
	if !ok {
		respondError(w, http.StatusServiceUnavailable, "signing unavailable")
		return
	}
	now := time.Now
	if a.Now != nil {
		now = a.Now
	}
	code, _, cerr := uisession.MintCode(kr, claims.Subject, claims.Email, host, now())
	if cerr != nil {
		respondError(w, http.StatusInternalServerError, "failed to mint code")
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"code": code})
}

// RevokeAllUISessions deletes every UI-session record and rotates the
// signing keyring, killing every outstanding session cookie and SSO code
// within the caches' TTLs (≤30s). Admin-only via its route middleware.
// POST /api/v1/sessions/revoke-all
func (a *AuthHandler) RevokeAllUISessions(w http.ResponseWriter, r *http.Request) {
	if a.UISessions == nil {
		respondError(w, http.StatusInternalServerError, "ui sessions not configured")
		return
	}
	if err := a.UISessions.DeleteAll(r.Context()); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to delete sessions")
		return
	}
	if a.UISessionReset != nil {
		if err := a.UISessionReset(r.Context()); err != nil {
			respondError(w, http.StatusInternalServerError, "failed to reset signing key")
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// isExactlyConsoleAudience reports whether the token's audience is exactly
// the console client and nothing else. Equality, not membership: the
// browser-SSO gate must reject a token that lists the console audience
// beside the CLI one, so a stolen CLI credential can never open SSO by
// virtue of a multi-audience token. This mirrors the single-audience host
// pin the uisession validator applies.
func isExactlyConsoleAudience(claims *middleware.Claims, console string) bool {
	return len(claims.Audience) == 1 && claims.Audience[0] == console
}

// sessionCookieExpiry is the earlier of the sliding idle window and the
// absolute cap, so the cookie's Max-Age never outlives the session's real
// end (the gate enforces both, but a matching Max-Age stops the browser
// sending a certainly-dead cookie).
func sessionCookieExpiry(authTime, now time.Time) time.Time {
	idle := now.Add(uisession.SessionIdleTTL)
	absCap := authTime.Add(uisession.SessionAbsoluteTTL)
	if absCap.Before(idle) {
		return absCap
	}
	return idle
}

// sessionCookie builds the per-host UI-session cookie. __Host-prefixed
// (no Domain, Secure, Path=/), HttpOnly so script can't read it, and
// SameSite=Lax so it rides top-level navigation into the UI host. Max-Age
// tracks the remaining life; the token's own exp and the record remain the
// authority checked on every request.
func sessionCookie(name, value string, expiresAt, now time.Time) *http.Cookie {
	maxAge := int(expiresAt.Sub(now).Seconds())
	if maxAge < 1 {
		maxAge = 1
	}
	return &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   maxAge,
	}
}

func (a *AuthHandler) clearUICookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// shareParamName / ssoParamName are the two single-use capability
// parameters the gate accepts in a forwarded URL: a share token and a UI
// SSO code. Both are stripped wherever the URI leaves the gate.
const (
	shareParamName = "kipper_share"
	ssoParamName   = "kipper_sso"
)

// queryParam extracts a named query value from a forwarded request URI.
func queryParam(uri, name string) string {
	i := strings.IndexByte(uri, '?')
	if i < 0 {
		return ""
	}
	q, err := url.ParseQuery(uri[i+1:])
	if err != nil {
		return ""
	}
	return q.Get(name)
}

func shareParam(uri string) string { return queryParam(uri, shareParamName) }
func ssoParam(uri string) string   { return queryParam(uri, ssoParamName) }

// stripGateParams removes both single-use capability parameters from a
// forwarded URI, keeping every other parameter. Used wherever the URI
// leaves the gate (the login redirect's next=), so a capability never
// lands in logs or history via a side door.
func stripGateParams(uri string) string {
	i := strings.IndexByte(uri, '?')
	if i < 0 {
		return uri
	}
	q, err := url.ParseQuery(uri[i+1:])
	if err != nil {
		return uri[:i]
	}
	q.Del(shareParamName)
	q.Del(ssoParamName)
	if encoded := q.Encode(); encoded != "" {
		return uri[:i] + "?" + encoded
	}
	return uri[:i]
}

// cleanURL rebuilds the absolute service-UI URL with both single-use
// capability params removed, using only the canonicalised host so a
// spoofed X-Forwarded-Host can't redirect elsewhere. Always https: the UI
// hosts are TLS-only.
func cleanURL(host, uri string) string {
	path := uri
	query := ""
	if i := strings.IndexByte(uri, '?'); i >= 0 {
		path = uri[:i]
		q, err := url.ParseQuery(uri[i+1:])
		if err == nil {
			q.Del(shareParamName)
			q.Del(ssoParamName)
			query = q.Encode()
		}
	}
	if path == "" {
		path = "/"
	}
	// A forwarded URI is origin-form from any real browser, but a crafted
	// non-slash path ("@evil.com/...") would otherwise turn the rebuilt
	// URL's host into userinfo and redirect off-cluster.
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	out := "https://" + host + path
	if query != "" {
		out += "?" + query
	}
	return out
}

// shareCookie builds the host-only cookie carrying a validated token.
// Max-Age tracks the token's remaining life so the browser stops
// sending a certainly-dead cookie; the token's own exp remains the
// authority checked on every request.
func shareCookie(name, value string, expiresAt, now time.Time) *http.Cookie {
	maxAge := int(expiresAt.Sub(now).Seconds())
	if maxAge < 1 {
		maxAge = 1
	}
	return &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   maxAge,
	}
}

func (a *AuthHandler) clearShareCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// Logout kills the durable refresh credential and revokes the caller's
// service-UI sessions. The refresh cookie is the only server-relevant
// state for the console session; deleting the UI-session records signs
// the operator out of every service UI within the record cache TTL
// (≤30s). The SPA sends its Bearer so we know whose records to delete;
// without it, logout still clears the refresh cookie (best effort).
// POST /auth/logout
func (a *AuthHandler) Logout(w http.ResponseWriter, r *http.Request) {
	// Kill the refresh credential first: it is the durable half of the
	// session, and a lingering one would silently resurrect the login.
	http.SetCookie(w, &http.Cookie{
		Name:     refreshCookieName,
		Value:    "",
		Path:     refreshCookiePath,
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
	})
	// Revoke this operator's UI sessions. Best effort and identity-scoped:
	// a missing or invalid Bearer just skips it (the refresh cookie is
	// already gone), and a record-store error must not fail the logout.
	if a.UISessions != nil && a.Auth != nil {
		if claims, verr := a.Auth.ValidateRequest(r); verr == nil && claims.Subject != "" {
			if derr := a.UISessions.DeleteBySubject(r.Context(), claims.Subject); derr != nil {
				log.Printf("logout: revoking UI sessions for subject: %v", derr)
			}
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// setRefreshCookie stores (or, with an empty token, leaves untouched) the
// rotated refresh token. Path-scoped to the refresh endpoint and HttpOnly:
// the SPA never sees the value, the browser presents it only where it is
// consumed, and SameSite=Strict keeps cross-site requests from carrying it.
func (a *AuthHandler) setRefreshCookie(w http.ResponseWriter, refreshToken string) {
	if refreshToken == "" {
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     refreshCookieName,
		Value:    refreshToken,
		Path:     refreshCookiePath,
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   refreshCookieMaxAge,
	})
}

// clearRefreshCookie expires the refresh cookie so the browser stops
// presenting a credential the server has rejected.
func clearRefreshCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     refreshCookieName,
		Value:    "",
		Path:     refreshCookiePath,
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
	})
}

// Refresh exchanges the HttpOnly refresh cookie for a fresh ID token, so the
// console can renew its session silently while ID-token lifetimes stay
// short. Dex rotates the refresh token on every use; the rotated token goes
// straight back into the cookie. Concurrent tabs race through the same
// cookie jar, which Dex's reuse interval absorbs.
// POST /auth/refresh
func (a *AuthHandler) Refresh(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(refreshCookieName)
	if err != nil || cookie.Value == "" {
		respondError(w, http.StatusUnauthorized, "no session to refresh")
		return
	}

	data := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {cookie.Value},
		"client_id":     {a.ClientID},
		"client_secret": {a.ClientSecret},
	}
	resp, err := dexHTTPClient.Post(a.DexIssuer+"/token", "application/x-www-form-urlencoded", strings.NewReader(data.Encode())) //nolint:gosec,noctx // URL from trusted Dex config
	if err != nil {
		respondError(w, http.StatusBadGateway, "failed to contact identity provider")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		// The refresh credential is dead (revoked, expired, or rotated away
		// by a failure); clear it so the browser stops presenting it.
		clearRefreshCookie(w)
		respondError(w, http.StatusUnauthorized, "session expired")
		return
	}

	var tokens tokenResponse
	if err := json.Unmarshal(body, &tokens); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to parse token response")
		return
	}

	// Rotation is the invariant: a 200 that carries no usable ID token or no
	// rotated refresh token is a dead session, and the durable cookie must
	// not survive it — the browser would retry it forever.
	if tokens.IDToken == "" || tokens.RefreshToken == "" || tokens.ExpiresIn <= 0 {
		clearRefreshCookie(w)
		respondError(w, http.StatusUnauthorized, "identity provider returned an unusable token response")
		return
	}

	// Validate the rotated ID token before continuing the session, so a token
	// whose email is no longer trusted ends the session instead of silently
	// renewing it.
	if a.Auth != nil {
		if _, verr := a.Auth.ValidateIDToken(tokens.IDToken); verr != nil {
			clearRefreshCookie(w)
			respondError(w, http.StatusUnauthorized, "session no longer valid")
			return
		}
	}

	a.setRefreshCookie(w, tokens.RefreshToken)

	respondJSON(w, http.StatusOK, loginResponse{Token: tokens.IDToken})
}

// NewAuthHandler creates an AuthHandler from environment variables.
func NewAuthHandler() *AuthHandler {
	issuer := os.Getenv("DEX_ISSUER")
	if issuer == "" {
		issuer = "https://dex.localhost/dex"
	}

	clientSecret := os.Getenv("DEX_CLIENT_SECRET")
	redirectURI := os.Getenv("DEX_REDIRECT_URI")
	if redirectURI == "" {
		redirectURI = "https://console.localhost/callback"
	}

	// The client ID is the audience the API validates on every token, so
	// the login flow and the validators must read the same value.
	clientID := os.Getenv("DEX_CLIENT_ID")
	if clientID == "" {
		clientID = middleware.DefaultAudience
	}

	return &AuthHandler{
		DexIssuer:    issuer,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURI:  redirectURI,
	}
}
