package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/getkipper/kipper/console-api/middleware"
	"github.com/getkipper/kipper/console-api/share"
	"github.com/getkipper/kipper/console-api/uisession"
)

const (
	shareHost       = "mailhog-supplemento-test.example.com"
	shareServiceUID = "uid-mailhog-1"
)

var shareTestKey = []byte("0123456789abcdef0123456789abcdef")

// shareFixture wires an AuthHandler with the v1 share path enabled: a
// keyring, a grant store over a fake cluster, a live-service UID
// registry, and a fixed clock, on top of the Dex-accepting base handler.
type shareFixture struct {
	h       *AuthHandler
	now     time.Time
	kr      *share.Keyring
	store   *share.GrantStore
	uids    map[string]string
	uiKR    *uisession.Keyring
	uiStore *uisession.RecordStore
}

func shareHandler(t *testing.T) *shareFixture {
	t.Helper()
	// Wall-clock based: the grant store's expiry checks run on real time.
	now := time.Now().Truncate(time.Second)
	f := &shareFixture{
		now:   now,
		kr:    &share.Keyring{CurrentKID: "kidtest01", CurrentKey: shareTestKey},
		store: share.NewGrantStore(k8sfake.NewSimpleClientset()),
		uids:  map[string]string{"supplemento-test/mailhog": shareServiceUID},
	}
	h := authCheckHandler("https://console.example.com", ".example.com")
	h.ShareKeyring = func() (*share.Keyring, bool) { return f.kr, true }
	h.ShareGrants = f.store
	h.ServiceUID = func(_ context.Context, ns, name string) (string, bool) {
		uid, ok := f.uids[ns+"/"+name]
		return uid, ok
	}
	// UI sessions run alongside share links on the same gate; wire them so
	// the "no lockout" invariant can be exercised against a real UI session.
	f.uiKR = testUIKeyring()
	f.uiStore = uisession.NewRecordStore(k8sfake.NewSimpleClientset(), uisession.SigningSecretNamespace)
	h.UISessionKeyring = func() (*uisession.Keyring, bool) { return f.uiKR, true }
	h.UISessions = f.uiStore
	h.RoleOf = func(email string) string {
		if email == "alice@example.com" {
			return middleware.RoleAdmin
		}
		return ""
	}
	h.Now = func() time.Time { return f.now }
	f.h = h
	return f
}

// mintSession seats a live UI session for host and returns its cookie.
func (f *shareFixture) mintSession(t *testing.T, email, host string) *http.Cookie {
	t.Helper()
	sid := "uisid-" + email
	if err := f.uiStore.Create(context.Background(), sid, email, email, host, f.now, f.now.Add(uisession.SessionAbsoluteTTL)); err != nil {
		t.Fatalf("seed ui record: %v", err)
	}
	tok, err := uisession.MintSession(f.uiKR, email, email, host, sid, f.now, f.now)
	if err != nil {
		t.Fatalf("MintSession: %v", err)
	}
	return &http.Cookie{Name: uisession.CookieName(host), Value: tok} //nolint:gosec // test fixture: client-side AddCookie ignores server-side attributes
}

// mint stores a grant and signs its token, the way the shares endpoint
// will.
func (f *shareFixture) mint(t *testing.T) (string, share.Grant) {
	t.Helper()
	g, err := share.NewGrant(shareServiceUID, "mailhog", "supplemento-test", shareHost, "PO review", "admin@example.com", 72*time.Hour, f.now)
	if err != nil {
		t.Fatalf("NewGrant: %v", err)
	}
	if err := f.store.Create(context.Background(), g); err != nil {
		t.Fatalf("storing grant: %v", err)
	}
	tok, err := share.MintGrant(f.kr, g, f.now)
	if err != nil {
		t.Fatalf("MintGrant: %v", err)
	}
	return tok, g
}

func shareRequest(host, uri string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/auth/check", nil)
	r.Header.Set("X-Forwarded-Host", host)
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Uri", uri)
	return r
}

func TestShare_ParamRedirectsAndSetsHostOnlyCookie(t *testing.T) {
	f := shareHandler(t)
	tok, _ := f.mint(t)

	r := shareRequest(shareHost, "/?kipper_share="+tok+"&view=1")
	w := httptest.NewRecorder()
	f.h.Check(w, r)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", w.Code)
	}
	loc := w.Header().Get("Location")
	if loc != "https://"+shareHost+"/?view=1" {
		t.Errorf("Location = %q; want the stripped absolute URL", loc)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Error("missing Cache-Control: no-store")
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("expected 1 cookie, got %d", len(cookies))
	}
	c := cookies[0]
	if !strings.HasPrefix(c.Name, "__Host-kipper-share-") {
		t.Errorf("cookie name %q lacks __Host- prefix", c.Name)
	}
	if c.Domain != "" {
		t.Errorf("cookie has Domain %q; must be host-only", c.Domain)
	}
	if !c.Secure || !c.HttpOnly || c.Path != "/" {
		t.Errorf("cookie attrs weak: secure=%v httponly=%v path=%q", c.Secure, c.HttpOnly, c.Path)
	}
	if c.Value != tok {
		t.Error("cookie does not carry the token")
	}
}

func TestShare_ValidCookieAuthorizes(t *testing.T) {
	f := shareHandler(t)
	tok, _ := f.mint(t)

	r := shareRequest(shareHost, "/")
	r.AddCookie(&http.Cookie{Name: shareCookieName(shareHost), Value: tok}) //nolint:gosec // test fixture: client-side AddCookie ignores server attrs
	w := httptest.NewRecorder()
	f.h.Check(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if u := w.Header().Get("X-Auth-User"); !strings.HasPrefix(u, "share:") {
		t.Errorf("X-Auth-User = %q, want share:<jti>", u)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Error("missing Cache-Control: no-store on the authorized response")
	}
}

// TestShare_RevokedGrantFallsThrough pins per-link revocation: the token
// is cryptographically pristine, but its grant is gone.
func TestShare_RevokedGrantFallsThrough(t *testing.T) {
	f := shareHandler(t)
	tok, g := f.mint(t)

	if err := f.store.Revoke(context.Background(), g.Namespace, g.Service, g.JTI); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	r := shareRequest(shareHost, "/")
	r.AddCookie(&http.Cookie{Name: shareCookieName(shareHost), Value: tok}) //nolint:gosec // test fixture
	w := httptest.NewRecorder()
	f.h.Check(w, r)

	if w.Code == http.StatusOK {
		t.Fatal("a revoked link still authorized")
	}
	var cleared bool
	for _, c := range w.Result().Cookies() {
		if c.Name == shareCookieName(shareHost) && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("revoked share cookie was not cleared")
	}
}

// TestShare_RecreatedServiceRejected pins the UID binding: same
// namespace/name, different live Service UID.
func TestShare_RecreatedServiceRejected(t *testing.T) {
	f := shareHandler(t)
	tok, _ := f.mint(t)

	f.uids["supplemento-test/mailhog"] = "uid-recreated-2"

	r := shareRequest(shareHost, "/?kipper_share="+tok)
	w := httptest.NewRecorder()
	f.h.Check(w, r)

	if w.Code == http.StatusOK {
		t.Fatal("a link for the deleted service opened its recreated namesake")
	}
	if w.Code == http.StatusFound && strings.HasPrefix(w.Header().Get("Location"), "https://"+shareHost+"/") {
		t.Fatal("a link for the deleted service produced a share redirect")
	}
}

// TestShare_DeletedServiceRejected: no live service at all.
func TestShare_DeletedServiceRejected(t *testing.T) {
	f := shareHandler(t)
	tok, _ := f.mint(t)

	delete(f.uids, "supplemento-test/mailhog")

	r := shareRequest(shareHost, "/?kipper_share="+tok)
	w := httptest.NewRecorder()
	f.h.Check(w, r)

	if w.Code == http.StatusOK {
		t.Fatal("a link authorized with no live service behind it")
	}
}

func TestShare_WrongHostRejected(t *testing.T) {
	f := shareHandler(t)
	// Token minted for the real host, presented on a different host.
	tok, _ := f.mint(t)

	r := shareRequest("mailhog-other-ns.storefront.com", "/")
	r.AddCookie(&http.Cookie{Name: shareCookieName("mailhog-other-ns.storefront.com"), Value: tok}) //nolint:gosec // test fixture
	w := httptest.NewRecorder()
	f.h.Check(w, r)

	// Falls through to Dex → redirect to login, never 200.
	if w.Code == http.StatusOK {
		t.Fatal("a token for another host authorized the wrong host")
	}
}

func TestShare_InvalidCookieClearedAndFallsThrough(t *testing.T) {
	f := shareHandler(t)

	r := shareRequest(shareHost, "/")
	r.AddCookie(&http.Cookie{Name: shareCookieName(shareHost), Value: "garbage"}) //nolint:gosec // test fixture
	w := httptest.NewRecorder()
	f.h.Check(w, r)

	if w.Code == http.StatusOK {
		t.Fatal("garbage cookie authorized")
	}
	// The stale cookie is cleared (MaxAge<0).
	var cleared bool
	for _, c := range w.Result().Cookies() {
		if c.Name == shareCookieName(shareHost) && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("invalid share cookie was not cleared")
	}
}

func TestShare_ExpiredParamFallsThrough(t *testing.T) {
	f := shareHandler(t)
	// Mint in the past so it's expired at the handler's clock.
	past := f.now.Add(-2 * time.Hour)
	g, err := share.NewGrant(shareServiceUID, "mailhog", "supplemento-test", shareHost, "", "admin@example.com", time.Hour, past)
	if err != nil {
		t.Fatalf("NewGrant: %v", err)
	}
	old, err := share.MintGrant(f.kr, g, past)
	if err != nil {
		t.Fatalf("MintGrant: %v", err)
	}
	r := shareRequest(shareHost, "/?kipper_share="+old)
	w := httptest.NewRecorder()
	f.h.Check(w, r)
	if w.Code == http.StatusFound && w.Header().Get("Location") == "https://"+shareHost+"/" {
		t.Fatal("expired token produced a share redirect instead of falling through")
	}
}

// TestShare_LoginRedirectRedactsToken: an invalid share param falls
// through to the login redirect, whose next= URL must not carry the
// token — from there it would reach console logs and browser history.
func TestShare_LoginRedirectRedactsToken(t *testing.T) {
	f := shareHandler(t)

	r := shareRequest(shareHost, "/inbox?kipper_share=some-dead-token&view=1")
	w := httptest.NewRecorder()
	f.h.Check(w, r)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302 to login", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "https://console.example.com/login") {
		t.Fatalf("Location = %q, want the console login", loc)
	}
	if strings.Contains(loc, "some-dead-token") || strings.Contains(loc, "kipper_share") {
		t.Errorf("the login redirect leaks the share token: %q", loc)
	}
	if !strings.Contains(loc, "view%3D1") && !strings.Contains(loc, "view=1") {
		t.Errorf("the login redirect lost the innocent query params: %q", loc)
	}
}

// A forwarded Host the cluster doesn't own must never ride the post-login
// redirect: next= is dropped rather than bouncing the user off an
// attacker-chosen authority.
func TestShare_LoginRedirectRejectsForeignHost(t *testing.T) {
	f := shareHandler(t)

	r := shareRequest("evil.attacker.example.net", "/inbox?view=1")
	w := httptest.NewRecorder()
	f.h.Check(w, r)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302 to login", w.Code)
	}
	loc := w.Header().Get("Location")
	if loc != "https://console.example.com/login" {
		t.Fatalf("Location = %q, want the bare login with no next= for a foreign host", loc)
	}
}

// The share gate never accepts a credential (or mints a redirect) on a
// plaintext hop.
func TestShare_PlaintextHopFallsThrough(t *testing.T) {
	f := shareHandler(t)
	tok, _ := f.mint(t)

	r := shareRequest(shareHost, "/?kipper_share="+tok)
	r.Header.Set("X-Forwarded-Proto", "http")
	w := httptest.NewRecorder()
	f.h.Check(w, r)

	if w.Code == http.StatusOK {
		t.Fatal("a share token authorized over plaintext http")
	}
	// No redirect to the token-stripped UI host either — it fell straight
	// through to the Dex login path.
	if loc := w.Header().Get("Location"); strings.HasPrefix(loc, "https://"+shareHost+"/") {
		t.Fatalf("plaintext hop still ran the share redirect: %q", loc)
	}
}

// A valid UI session must still work when the share path is enabled and no
// share credential is present.
func TestShare_UISessionStillWorksAlongside(t *testing.T) {
	f := shareHandler(t)
	cookie := f.mintSession(t, "alice@example.com", shareHost)

	r := shareRequest(shareHost, "/")
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	f.h.Check(w, r)

	if w.Code != http.StatusOK || w.Header().Get("X-Auth-User") != "alice@example.com" {
		t.Fatalf("UI-session path broke: status=%d user=%q", w.Code, w.Header().Get("X-Auth-User"))
	}
}

// TestShare_InvalidParamWithValidUISessionStillEnters pins the truth
// table's no-lockout rule: an invalid share credential never blocks an
// independently valid UI session.
func TestShare_InvalidParamWithValidUISessionStillEnters(t *testing.T) {
	f := shareHandler(t)
	cookie := f.mintSession(t, "alice@example.com", shareHost)

	r := shareRequest(shareHost, "/?kipper_share=garbage")
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	f.h.Check(w, r)

	if w.Code != http.StatusOK || w.Header().Get("X-Auth-User") != "alice@example.com" {
		t.Fatalf("an invalid share param locked out a valid UI session: status=%d user=%q", w.Code, w.Header().Get("X-Auth-User"))
	}
}

// A share token must never authenticate the REST API: that path runs
// middleware.Auth (RS256/Dex), which structurally rejects an HS256
// share token. This pins the isolation boundary.
func TestShare_TokenRejectedByRESTMiddleware(t *testing.T) {
	f := shareHandler(t)
	tok, _ := f.mint(t)

	r := httptest.NewRequest(http.MethodGet, "/api/v1/anything", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	if _, err := f.h.Auth.ValidateRequest(r); err == nil {
		t.Fatal("share token was accepted by the REST validator")
	}
}
