package migration

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

// A redirect host moves with its app and needs its DNS record changed like the
// app's own host, so the plan names it next to the host it redirects to.
func TestPlanAppDomain_NamesTheRedirectHosts(t *testing.T) {
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "website", Namespace: "site-prod"},
		Spec: kipperv1.AppSpec{Route: &kipperv1.AppRoute{
			Host:         "www.example.org",
			RedirectFrom: []string{"example.org"},
		}},
	}
	h := &Handler{Domain: "apps.source.example.com"}
	item := planItem{Kind: "app", Name: "website", Namespace: "site-prod"}

	h.planAppDomain(app, "site-prod", "prod", &Token{BaseDomain: "apps.target.example.com"}, map[string]bool{}, false, &item)

	assert.Equal(t, []string{"example.org"}, item.RedirectFrom)
	assert.Contains(t, item.Binding, "redirect=example.org", "the consent digest must cover the redirect hosts the operator saw")
}

// Only a moving route is applied on the target at cutover. An app whose host
// stays behind keeps its redirect hosts on the source, so the plan lists
// only redirect hosts that move.
func TestPlanAppDomain_LeavesOutRedirectHostsThatStayBehind(t *testing.T) {
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "website", Namespace: "site-prod"},
		Spec:       kipperv1.AppSpec{Route: &kipperv1.AppRoute{RedirectFrom: []string{"example.org"}}},
	}
	h := &Handler{Domain: "apps.source.example.com"}
	item := planItem{Kind: "app", Name: "website", Namespace: "site-prod"}

	h.planAppDomain(app, "site-prod", "prod", &Token{BaseDomain: "apps.target.example.com"}, map[string]bool{}, false, &item)

	assert.NotEqual(t, string(dispositionMove), item.Disposition)
	assert.Empty(t, item.RedirectFrom)
}

// The DNS screen marks a domain green only once it points at the target. A
// redirect host left off it reads as done while it still points at the source.
func TestDNSStatus_ListsTheRedirectHosts(t *testing.T) {
	h := &Handler{Sessions: NewSessionStore()}
	session := &Session{ID: "s1", SavedRoutes: map[string]map[string]interface{}{
		"site-prod/website": {"host": "www.example.invalid", "redirectFrom": []interface{}{"example.invalid"}},
	}}
	h.Sessions.Put(session)

	router := chi.NewRouter()
	router.Get("/{session}/dns", h.DNSStatusHandler)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/s1/dns", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var domains []struct {
		Domain     string `json:"domain"`
		RedirectTo string `json:"redirect_to"`
		App        string `json:"app"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &domains))
	redirectsTo := map[string]string{}
	for _, d := range domains {
		redirectsTo[d.Domain] = d.RedirectTo
		assert.Equal(t, "website", d.App)
	}
	assert.Equal(t, map[string]string{"www.example.invalid": "", "example.invalid": "www.example.invalid"}, redirectsTo)
}
