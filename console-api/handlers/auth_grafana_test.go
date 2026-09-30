package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/getkipper/kipper/console-api/middleware"
	"github.com/getkipper/kipper/console-api/uisession"
)

const grafanaHost = "grafana.example.com"

// grafanaHandler extends the UI-session fixture with the Grafana gate:
// ops is an admin, dev a deployer holding monitoring, viewer a viewer without it.
func grafanaHandler(now time.Time, hosts ...string) *uiFixture {
	f := uiHandler(now)
	if len(hosts) == 0 {
		hosts = []string{grafanaHost}
	}
	roles := map[string]string{
		"ops@example.com":    middleware.RoleAdmin,
		"dev@example.com":    middleware.RoleDeployer,
		"viewer@example.com": middleware.RoleViewer,
	}
	f.h.RoleOf = func(email string) string { return roles[email] }
	f.h.MonitoringAccess = func(email string) (string, bool) {
		switch email {
		case "ops@example.com", "dev@example.com":
			return roles[email], true
		}
		return "", false
	}
	f.h.GrafanaHosts = func() []string { return hosts }
	return f
}

func (f *uiFixture) seatGrafanaSession(t *testing.T, host, email string) *http.Cookie {
	t.Helper()
	sid := "sid-" + email + "-" + host
	authTime := f.now.Add(-time.Minute)
	if err := f.store.Create(context.Background(), sid, email, email, host, authTime, authTime.Add(uisession.SessionAbsoluteTTL)); err != nil {
		t.Fatalf("seed record: %v", err)
	}
	tok, err := uisession.MintSession(f.kr, email, email, host, sid, authTime, authTime)
	if err != nil {
		t.Fatalf("MintSession: %v", err)
	}
	return &http.Cookie{Name: uisession.CookieName(host), Value: tok} //nolint:gosec // test fixture: client-side AddCookie ignores server-side attributes
}

func grafanaCheck(f *uiFixture, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	f.h.CheckGrafana(w, r)
	return w
}

func assertNoIdentity(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	assert.Empty(t, w.Header().Get("X-WEBAUTH-USER"))
	assert.Empty(t, w.Header().Get("X-WEBAUTH-ROLE"))
}

func TestCheckGrafana_AdminIsGrafanaAdmin(t *testing.T) {
	f := grafanaHandler(time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC))
	r := uiRequest(grafanaHost, "/d/abc")
	r.AddCookie(f.seatGrafanaSession(t, grafanaHost, "ops@example.com"))

	w := grafanaCheck(f, r)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "ops@example.com", w.Header().Get("X-WEBAUTH-USER"))
	assert.Equal(t, "Admin", w.Header().Get("X-WEBAUTH-ROLE"))
	assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
}

func TestCheckGrafana_GrantHolderIsEditor(t *testing.T) {
	f := grafanaHandler(time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC))
	r := uiRequest(grafanaHost, "/explore")
	r.AddCookie(f.seatGrafanaSession(t, grafanaHost, "dev@example.com"))

	w := grafanaCheck(f, r)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "dev@example.com", w.Header().Get("X-WEBAUTH-USER"))
	assert.Equal(t, "Editor", w.Header().Get("X-WEBAUTH-ROLE"))
}

func TestCheckGrafana_UserWithoutGrantIsForbidden(t *testing.T) {
	f := grafanaHandler(time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC))
	r := uiRequest(grafanaHost, "/")
	r.AddCookie(f.seatGrafanaSession(t, grafanaHost, "viewer@example.com"))

	w := grafanaCheck(f, r)

	assert.Equal(t, http.StatusForbidden, w.Code, "a logged-in user without the grant gets 403, not a login loop")
	assertNoIdentity(t, w)
}

func TestCheckGrafana_RefusesWhenGrantStateIsUnavailable(t *testing.T) {
	f := grafanaHandler(time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC))
	f.h.MonitoringAccess = func(string) (string, bool) { return "", false }
	r := uiRequest(grafanaHost, "/")
	r.AddCookie(f.seatGrafanaSession(t, grafanaHost, "ops@example.com"))

	w := grafanaCheck(f, r)

	assert.Equal(t, http.StatusForbidden, w.Code)
	assertNoIdentity(t, w)
}

func TestCheckGrafana_NoSessionRedirectsToLogin(t *testing.T) {
	f := grafanaHandler(time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC))

	w := grafanaCheck(f, uiRequest(grafanaHost, "/explore?left=x"))

	assert.Equal(t, http.StatusFound, w.Code)
	loc, err := url.Parse(w.Header().Get("Location"))
	if assert.NoError(t, err) {
		assert.Equal(t, "console.example.com", loc.Host)
		assert.Equal(t, "/login", loc.Path)
		assert.Equal(t, "https://"+grafanaHost+"/explore?left=x", loc.Query().Get("next"))
	}
	assertNoIdentity(t, w)
}

func TestCheckGrafana_RefusesHostsThatAreNotGrafana(t *testing.T) {
	// A valid service-UI session must not open the Grafana gate, whatever
	// X-Forwarded-Host says.
	f := grafanaHandler(time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC))
	r := uiRequest(uiHost, "/")
	r.AddCookie(f.seatGrafanaSession(t, uiHost, "ops@example.com"))

	w := grafanaCheck(f, r)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assertNoIdentity(t, w)
}

func TestCheckGrafana_RefusesSessionMintedForAnotherHost(t *testing.T) {
	f := grafanaHandler(time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC))
	other := f.seatGrafanaSession(t, uiHost, "ops@example.com")
	r := uiRequest(grafanaHost, "/")
	r.AddCookie(&http.Cookie{Name: uisession.CookieName(grafanaHost), Value: other.Value}) //nolint:gosec // test fixture

	w := grafanaCheck(f, r)

	assert.NotEqual(t, http.StatusOK, w.Code)
	assertNoIdentity(t, w)
}

func TestCheckGrafana_RefusesPlainHTTP(t *testing.T) {
	f := grafanaHandler(time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC))
	r := uiRequest(grafanaHost, "/")
	r.Header.Set("X-Forwarded-Proto", "http")
	r.AddCookie(f.seatGrafanaSession(t, grafanaHost, "ops@example.com"))

	w := grafanaCheck(f, r)

	assert.NotEqual(t, http.StatusOK, w.Code)
	assertNoIdentity(t, w)
}

func TestCheckGrafana_CodeRedemptionSetsGrafanaCookie(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	f := grafanaHandler(now)
	code, _, err := uisession.MintCode(f.kr, "dev", "dev@example.com", grafanaHost, now)
	if err != nil {
		t.Fatalf("MintCode: %v", err)
	}

	w := grafanaCheck(f, uiRequest(grafanaHost, "/explore?kipper_sso="+code))

	assert.Equal(t, http.StatusFound, w.Code)
	assert.Equal(t, "https://"+grafanaHost+"/explore", w.Header().Get("Location"))
	assert.NotNil(t, uiCookie(w, grafanaHost))
	assertNoIdentity(t, w)
}

func TestCheckGrafana_ServesEveryHostDuringATransition(t *testing.T) {
	const newHost = "grafana.example.org"
	f := grafanaHandler(time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC), grafanaHost, newHost)

	for _, host := range []string{grafanaHost, newHost} {
		r := uiRequest(host, "/")
		r.AddCookie(f.seatGrafanaSession(t, host, "dev@example.com"))
		w := grafanaCheck(f, r)
		assert.Equal(t, http.StatusOK, w.Code, host)
		assert.Equal(t, "Editor", w.Header().Get("X-WEBAUTH-ROLE"), host)
	}
}

func TestCheckGrafana_MisconfiguredRefuses(t *testing.T) {
	f := grafanaHandler(time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC))
	f.h.GrafanaHosts = nil

	w := grafanaCheck(f, uiRequest(grafanaHost, "/"))

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assertNoIdentity(t, w)
}

func TestCheckGrafana_FreeClusterHost(t *testing.T) {
	// On *.kipper.run the UI domain is empty; the exact Grafana host must still
	// work, and no other kipper.run host may ride the login redirect.
	const freeHost = "grafana--acme.kipper.run"
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	f := grafanaHandler(now, freeHost)
	f.h.UIDomain = ""
	f.h.ConsoleURL = "https://console--acme.kipper.run"

	w := grafanaCheck(f, uiRequest(freeHost, "/d/x"))
	loc, _ := url.Parse(w.Header().Get("Location"))
	assert.Equal(t, "https://"+freeHost+"/d/x", loc.Query().Get("next"))

	assert.Equal(t, "https://"+freeHost+"/d/x", f.h.safeRedirectTarget("https://"+freeHost+"/d/x"))
	assert.Empty(t, f.h.safeRedirectTarget("https://grafana--other.kipper.run/"))

	token := signAudienceToken(t, "dev@example.com", []string{middleware.DefaultAudience})
	cw := httptest.NewRecorder()
	f.h.UISessionCode(cw, uiCodeRequest(t, token, freeHost))
	assert.Equal(t, http.StatusOK, cw.Code, "grant holder gets an SSO code for the free-cluster Grafana host")
}

func TestUISessionCode_GrafanaHostRequiresGrant(t *testing.T) {
	f := grafanaHandler(time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC))

	for _, tt := range []struct {
		email string
		host  string
		want  int
	}{
		{"dev@example.com", grafanaHost, http.StatusOK},
		{"ops@example.com", grafanaHost, http.StatusOK},
		{"viewer@example.com", grafanaHost, http.StatusForbidden},
		{"viewer@example.com", uiHost, http.StatusOK},
	} {
		token := signAudienceToken(t, tt.email, []string{middleware.DefaultAudience})
		w := httptest.NewRecorder()
		f.h.UISessionCode(w, uiCodeRequest(t, token, tt.host))
		assert.Equal(t, tt.want, w.Code, "%s → %s", tt.email, tt.host)
	}
}
