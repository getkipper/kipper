package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func settingsRouter(s *Settings) *chi.Mux {
	r := chi.NewRouter()
	r.Get("/api/v1/projects/{name}/apps/{app}/settings", s.Get)
	r.Put("/api/v1/projects/{name}/apps/{app}/settings", s.Update)
	return r
}

func TestSettings_InternalPathsRoundTrip(t *testing.T) {
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop"},
		Spec:       kipperv1.AppSpec{Route: &kipperv1.AppRoute{}},
	}
	s := &Settings{Client: fake.NewClientset(), CRClient: testCRClient(app)}
	r := settingsRouter(s)

	body := `{"internal_paths":["/admin"],"public_paths":["/actuator/prometheus"]}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/projects/shop/apps/web/settings", strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, body %s", rec.Code, rec.Body)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/projects/shop/apps/web/settings", nil)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	var got appSettings
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.InternalPaths == nil || len(*got.InternalPaths) != 1 || (*got.InternalPaths)[0] != "/admin" {
		t.Errorf("internal_paths = %v, want [/admin]", got.InternalPaths)
	}
	if got.PublicPaths == nil || len(*got.PublicPaths) != 1 || (*got.PublicPaths)[0] != "/actuator/prometheus" {
		t.Errorf("public_paths = %v, want [/actuator/prometheus]", got.PublicPaths)
	}
}

// Older console bundles omit both fields on unrelated settings saves.
func TestSettings_AnAbsentPathListLeavesTheRefusalsAlone(t *testing.T) {
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop"},
		Spec: kipperv1.AppSpec{Route: &kipperv1.AppRoute{
			InternalPaths: []string{"/admin"},
			PublicPaths:   []string{"/actuator/prometheus"},
		}},
	}
	s := &Settings{Client: fake.NewClientset(), CRClient: testCRClient(app)}
	r := settingsRouter(s)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/projects/shop/apps/web/settings",
		strings.NewReader(`{"security_headers":true,"rate_limit":0}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, body %s", rec.Code, rec.Body)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/projects/shop/apps/web/settings", nil)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var got appSettings
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.InternalPaths == nil || len(*got.InternalPaths) != 1 {
		t.Errorf("internal_paths = %v, want the app's own [/admin]", got.InternalPaths)
	}
	if got.PublicPaths == nil || len(*got.PublicPaths) != 1 {
		t.Errorf("public_paths = %v, want [/actuator/prometheus]", got.PublicPaths)
	}
}

func TestSettings_RefusesAPathItCannotExpress(t *testing.T) {
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop"},
		Spec:       kipperv1.AppSpec{Route: &kipperv1.AppRoute{}},
	}
	s := &Settings{Client: fake.NewClientset(), CRClient: testCRClient(app)}
	r := settingsRouter(s)

	body := "{\"internal_paths\":[\"/ad`min\"]}"
	req := httptest.NewRequest(http.MethodPut, "/api/v1/projects/shop/apps/web/settings", strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}
