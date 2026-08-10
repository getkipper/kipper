package handlers

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"k8s.io/client-go/kubernetes"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/getkipper/kipper/console-api/middleware"
	"github.com/getkipper/kipper/console-api/uisession"
)

// testRSAKey signs the test tokens. Dex issues RS256-signed JWTs and the
// validator pins RS256, so the fixtures must use a real RSA key rather
// than a symmetric secret.
var testRSAKey = mustGenerateRSAKey()

func mustGenerateRSAKey() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return key
}

// authCheckHandler returns an AuthHandler whose Auth validator accepts
// RS256 JWTs signed with testRSAKey. It wires no UI-session or share path,
// so tryUISession/tryShare short-circuit and Check exercises the redirect
// paths.
func authCheckHandler(consoleURL, uiDomain string) *AuthHandler {
	a := &middleware.Auth{
		Issuer:   "https://dex.test/dex",
		Audience: middleware.DefaultAudience,
		KeyFunc:  func(_ *jwt.Token) (any, error) { return &testRSAKey.PublicKey, nil },
	}
	return &AuthHandler{
		Auth:       a,
		ConsoleURL: consoleURL,
		UIDomain:   uiDomain,
	}
}

const uiHost = "mailhog-blog.example.com"

func testUIKeyring() *uisession.Keyring {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 7)
	}
	return &uisession.Keyring{CurrentKID: "uikid01", CurrentKey: key}
}

// uiFixture wires an AuthHandler with the UI-session path enabled over a
// fake cluster and a fixed clock.
type uiFixture struct {
	h      *AuthHandler
	kr     *uisession.Keyring
	store  *uisession.RecordStore
	client kubernetes.Interface
	now    time.Time
}

// uiHandler builds a UI-session-wired handler. Only ops@example.com holds
// a role, so the role check can be exercised by minting for another email.
func uiHandler(now time.Time) *uiFixture {
	kr := testUIKeyring()
	client := k8sfake.NewSimpleClientset()
	store := uisession.NewRecordStore(client, uisession.SigningSecretNamespace)
	h := authCheckHandler("https://console.example.com", ".example.com")
	h.UISessionKeyring = func() (*uisession.Keyring, bool) { return kr, true }
	h.UISessions = store
	h.RoleOf = func(email string) string {
		if email == "ops@example.com" {
			return middleware.RoleAdmin
		}
		return ""
	}
	h.Now = func() time.Time { return now }
	return &uiFixture{h: h, kr: kr, store: store, client: client, now: now}
}

func uiRequest(host, uri string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/auth/check", nil)
	r.Header.Set("X-Forwarded-Host", host)
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Uri", uri)
	return r
}

// seatSession mints a uiHost session cookie and its backing record so the
// return path validates. authTime and mintNow let a test place the cookie's
// windows relative to the handler clock.
func (f *uiFixture) seatSession(t *testing.T, sub, email string, authTime, mintNow time.Time) *http.Cookie {
	t.Helper()
	sid := "sid-" + sub
	if err := f.store.Create(context.Background(), sid, sub, email, uiHost, authTime, authTime.Add(uisession.SessionAbsoluteTTL)); err != nil {
		t.Fatalf("seed record: %v", err)
	}
	tok, err := uisession.MintSession(f.kr, sub, email, uiHost, sid, authTime, mintNow)
	if err != nil {
		t.Fatalf("MintSession: %v", err)
	}
	return &http.Cookie{Name: uisession.CookieName(uiHost), Value: tok} //nolint:gosec // test fixture: client-side AddCookie ignores server-side attributes
}

func uiCookie(w *httptest.ResponseRecorder, host string) *http.Cookie {
	name := uisession.CookieName(host)
	for _, c := range w.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func TestUISession_ValidCookieAuthorizes(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	f := uiHandler(now)
	cookie := f.seatSession(t, "u1", "ops@example.com", now.Add(-time.Minute), now.Add(-time.Minute))

	r := uiRequest(uiHost, "/messages")
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	f.h.Check(w, r)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "ops@example.com", w.Header().Get("X-Auth-User"))
}

func TestUISession_NoRecordFallsThrough(t *testing.T) {
	// A validly-signed cookie whose record was deleted must not authorize:
	// the record is the authoritative liveness switch.
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	f := uiHandler(now)
	sid := "orphan"
	tok, err := uisession.MintSession(f.kr, "u1", "ops@example.com", uiHost, sid, now, now)
	if err != nil {
		t.Fatalf("MintSession: %v", err)
	}

	r := uiRequest(uiHost, "/messages")
	r.AddCookie(&http.Cookie{Name: uisession.CookieName(uiHost), Value: tok}) //nolint:gosec // test fixture: client-side AddCookie ignores server-side attributes
	w := httptest.NewRecorder()
	f.h.Check(w, r)

	assert.Equal(t, http.StatusFound, w.Code, "no record → fall through to login")
	assert.Empty(t, w.Header().Get("X-Auth-User"))
}

func TestUISession_RoleRemovedRefused(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	f := uiHandler(now)
	// gone@ has a live record but no role — defense-in-depth refuses it.
	cookie := f.seatSession(t, "u2", "gone@example.com", now, now)

	r := uiRequest(uiHost, "/messages")
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	f.h.Check(w, r)

	assert.Equal(t, http.StatusFound, w.Code)
	assert.Empty(t, w.Header().Get("X-Auth-User"))
}

func TestUISession_ExpiredCookieFallsThrough(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	f := uiHandler(now)
	// Minted 40m ago: past the 30m idle window.
	cookie := f.seatSession(t, "u3", "ops@example.com", now.Add(-40*time.Minute), now.Add(-40*time.Minute))

	r := uiRequest(uiHost, "/messages")
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	f.h.Check(w, r)

	assert.Equal(t, http.StatusFound, w.Code)
}

func TestUISession_PastAbsoluteCapFallsThrough(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	f := uiHandler(now)
	// auth_time 13h ago, but re-minted moments ago so the idle window is
	// fresh: only the absolute cap can reject it.
	cookie := f.seatSession(t, "u4", "ops@example.com", now.Add(-13*time.Hour), now.Add(-time.Minute))

	r := uiRequest(uiHost, "/messages")
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	f.h.Check(w, r)

	assert.Equal(t, http.StatusFound, w.Code, "past 12h cap → reject even with a fresh idle window")
}

func TestUISession_RenewsWhenCloseToExpiry(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	f := uiHandler(now)
	// Minted 20m ago → 10m of idle remains (< 15m renew window).
	cookie := f.seatSession(t, "u5", "ops@example.com", now.Add(-20*time.Minute), now.Add(-20*time.Minute))

	r := uiRequest(uiHost, "/messages")
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	f.h.Check(w, r)

	assert.Equal(t, http.StatusOK, w.Code)
	renewed := uiCookie(w, uiHost)
	if assert.NotNil(t, renewed, "a session near expiry must be re-minted") {
		assert.NotEqual(t, cookie.Value, renewed.Value, "renewal must issue a fresh token")
		assert.True(t, renewed.Secure && renewed.HttpOnly)
	}
}

func TestUISession_CodeRedemptionSetsCookieAndStrips(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	f := uiHandler(now)
	code, _, err := uisession.MintCode(f.kr, "u6", "ops@example.com", uiHost, now)
	if err != nil {
		t.Fatalf("MintCode: %v", err)
	}

	r := uiRequest(uiHost, "/messages?kipper_sso="+code+"&view=1")
	w := httptest.NewRecorder()
	f.h.Check(w, r)

	assert.Equal(t, http.StatusFound, w.Code)
	assert.Equal(t, "https://"+uiHost+"/messages?view=1", w.Header().Get("Location"),
		"the SSO code must be stripped from the redirect")
	c := uiCookie(w, uiHost)
	if assert.NotNil(t, c, "redemption must set the session cookie") {
		assert.True(t, strings.HasPrefix(c.Name, "__Host-kipper-ui-"))
		assert.Empty(t, c.Domain, "session cookie must be host-only")
		assert.True(t, c.Secure && c.HttpOnly && c.Path == "/")
		assert.Equal(t, http.SameSiteLaxMode, c.SameSite)
	}
}

func TestUISession_ReplayedCodeRefused(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	f := uiHandler(now)
	code, _, err := uisession.MintCode(f.kr, "u7", "ops@example.com", uiHost, now)
	if err != nil {
		t.Fatalf("MintCode: %v", err)
	}

	// First redemption creates the record.
	first := httptest.NewRecorder()
	f.h.Check(first, uiRequest(uiHost, "/messages?kipper_sso="+code))
	assert.Equal(t, http.StatusFound, first.Code)
	assert.NotNil(t, uiCookie(first, uiHost))

	// Replay: the record already exists (AlreadyExists), so the code is
	// refused and no session cookie is issued.
	second := httptest.NewRecorder()
	f.h.Check(second, uiRequest(uiHost, "/messages?kipper_sso="+code))
	assert.Nil(t, uiCookie(second, uiHost), "a replayed code must not mint a session")
	assert.Contains(t, second.Header().Get("Location"), "console.example.com/login")
}

func TestUISession_WrongHostCodeRefused(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	f := uiHandler(now)
	code, _, err := uisession.MintCode(f.kr, "u8", "ops@example.com", uiHost, now)
	if err != nil {
		t.Fatalf("MintCode: %v", err)
	}

	other := "rabbitmq-blog.example.com"
	w := httptest.NewRecorder()
	f.h.Check(w, uiRequest(other, "/?kipper_sso="+code))

	assert.Nil(t, uiCookie(w, other), "a code minted for one host must not open another")
}

func TestUISession_ExpiredCodeRefused(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	f := uiHandler(now)
	// Minted 2 minutes ago, past the 60s code TTL.
	code, _, err := uisession.MintCode(f.kr, "u9", "ops@example.com", uiHost, now.Add(-2*time.Minute))
	if err != nil {
		t.Fatalf("MintCode: %v", err)
	}

	w := httptest.NewRecorder()
	f.h.Check(w, uiRequest(uiHost, "/?kipper_sso="+code))

	assert.Nil(t, uiCookie(w, uiHost), "an expired code must not open a session")
}

func TestAuthHandler_Check_RedirectsUnauthenticatedToLogin(t *testing.T) {
	// Forwarded headers (Traefik standard) give us the original URL so the
	// login page can bounce the user back.
	h := authCheckHandler("https://console.example.com", ".example.com")

	req := httptest.NewRequest("GET", "/auth/check", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "mailhog-blog-test.example.com")
	req.Header.Set("X-Forwarded-Uri", "/messages")
	rec := httptest.NewRecorder()
	h.Check(rec, req)

	assert.Equal(t, http.StatusFound, rec.Code, "unauthenticated must redirect, not 401, so the browser actually navigates")
	loc := rec.Header().Get("Location")
	assert.Contains(t, loc, "https://console.example.com/login")
	assert.Contains(t, loc, "next=https%3A%2F%2Fmailhog-blog-test.example.com%2Fmessages",
		"next= must round-trip the original URL so the user lands back on the page they tried to open")
}

func TestAuthHandler_Check_FallbackToUnauthorizedWithoutConsoleURL(t *testing.T) {
	// Misconfiguration guard: without a console URL there's no sensible
	// redirect target, so return 401.
	h := authCheckHandler("", "")

	req := httptest.NewRequest("GET", "/auth/check", nil)
	rec := httptest.NewRecorder()
	h.Check(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestAuthHandler_Check_RefusesIfMisconfigured(t *testing.T) {
	// Auth is nil — refuse loudly rather than fall through to 200 which
	// would silently allow every request.
	h := &AuthHandler{}
	req := httptest.NewRequest("GET", "/auth/check", nil)
	rec := httptest.NewRecorder()
	h.Check(rec, req)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestAuthHandler_SafeRedirectTarget(t *testing.T) {
	h := authCheckHandler("https://console.example.com", ".example.com")

	cases := []struct {
		name string
		next string
		want string
	}{
		{"console with path", "https://console.example.com/apps?tab=1", "https://console.example.com/apps?tab=1"},
		{"service ui under cluster domain", "https://mailhog-supplemento.example.com/inbox", "https://mailhog-supplemento.example.com/inbox"},
		{"userinfo on an allowed host is refused", "https://user@console.example.com/", ""},
		{"explicit port is stripped and host still allowed", "https://console.example.com:8443/apps", "https://console.example.com/apps"},
		{"trailing dot is normalised", "https://console.example.com./apps", "https://console.example.com/apps"},
		{"mixed case host is normalised", "https://Console.Example.COM/apps", "https://console.example.com/apps"},
		{"foreign host refused", "https://evil.attacker.example.net/", ""},
		{"suffix trick refused", "https://console.example.com.evil.test/", ""},
		{"prefix trick refused", "https://evilexample.com/", ""},
		{"http scheme refused", "http://console.example.com/", ""},
		{"empty stays empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := h.safeRedirectTarget(tc.next); got != tc.want {
				t.Errorf("safeRedirectTarget(%q) = %q, want %q", tc.next, got, tc.want)
			}
		})
	}
}
