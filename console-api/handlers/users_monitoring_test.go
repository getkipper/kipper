package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/getkipper/kipper/console-api/middleware"
	"github.com/getkipper/kipper/console-api/security"
)

const monitoringTestUsers = `{"admin@test.com":"admin","dev@test.com":"deployer","viewer@test.com":"viewer"}`

func monitoringRouter(handler *Users) *chi.Mux {
	r := chi.NewRouter()
	r.Get("/api/v1/users", handler.List)
	r.Put("/api/v1/users/{email}/monitoring", handler.GrantMonitoring)
	r.Delete("/api/v1/users/{email}/monitoring", handler.RevokeMonitoring)
	return r
}

func storedMonitoring(t *testing.T, client *fake.Clientset) string {
	t.Helper()
	cm, err := client.CoreV1().ConfigMaps("kipper-system").Get(context.Background(), "kipper-users", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading kipper-users: %v", err)
	}
	return cm.Data["monitoring"]
}

func TestUsers_GrantAndRevokeMonitoring(t *testing.T) {
	client := fake.NewClientset(kipperSystemNamespace(), roleConfigMap(monitoringTestUsers))
	handler := &Users{Client: client, RoleStore: middleware.NewRoleStore(client)}
	r := monitoringRouter(handler)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("PUT", "/api/v1/users/dev@test.com/monitoring", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("grant: expected 204, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if got := storedMonitoring(t, client); got != `["dev@test.com"]` {
		t.Fatalf("stored grants = %s", got)
	}

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("DELETE", "/api/v1/users/dev@test.com/monitoring", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("revoke: expected 204, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if got := storedMonitoring(t, client); got != `[]` {
		t.Fatalf("stored grants after revoke = %s", got)
	}
}

func TestUsers_GrantMonitoring_UnknownUser(t *testing.T) {
	client := fake.NewClientset(kipperSystemNamespace(), roleConfigMap(monitoringTestUsers))
	handler := &Users{Client: client, RoleStore: middleware.NewRoleStore(client)}

	rec := httptest.NewRecorder()
	monitoringRouter(handler).ServeHTTP(rec, httptest.NewRequest("PUT", "/api/v1/users/stranger@test.com/monitoring", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestUsers_List_ReportsMonitoring(t *testing.T) {
	cm := roleConfigMap(monitoringTestUsers)
	cm.Data["monitoring"] = `["dev@test.com"]`
	client := fake.NewClientset(kipperSystemNamespace(), cm)
	handler := &Users{Client: client, RoleStore: middleware.NewRoleStore(client)}

	rec := httptest.NewRecorder()
	monitoringRouter(handler).ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/users", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var users []struct {
		Email      string `json:"email"`
		Monitoring bool   `json:"monitoring"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &users); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := map[string]bool{"admin@test.com": true, "dev@test.com": true, "viewer@test.com": false}
	for _, u := range users {
		if u.Monitoring != want[u.Email] {
			t.Errorf("%s monitoring = %v, want %v", u.Email, u.Monitoring, want[u.Email])
		}
	}
}

func TestUsers_GrantMonitoring_AlertsAdmins(t *testing.T) {
	client := fake.NewClientset(kipperSystemNamespace(), roleConfigMap(monitoringTestUsers))
	store := middleware.NewRoleStore(client)

	var mu sync.Mutex
	var emailed []string
	notifier := &security.Notifier{Console: security.ConsoleHooks{
		Email: func(ctx context.Context, to, subject, htmlBody string) error {
			mu.Lock()
			emailed = append(emailed, to)
			mu.Unlock()
			return nil
		},
		Admins: func() []string { return []string{"admin@test.com"} },
	}}
	handler := &Users{Client: client, RoleStore: store, Security: notifier}

	rec := httptest.NewRecorder()
	monitoringRouter(handler).ServeHTTP(rec, httptest.NewRequest("PUT", "/api/v1/users/dev@test.com/monitoring", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", rec.Code)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := append([]string(nil), emailed...)
		mu.Unlock()
		for _, to := range got {
			if to == "admin@test.com" {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("granting monitoring did not alert the admins")
}

func meRequest(t *testing.T, handler *Users, store *middleware.RoleStore, email string) map[string]any {
	t.Helper()
	r := chi.NewRouter()
	r.Use(middleware.RoleMiddleware(store))
	r.Get("/api/v1/me", handler.Me)

	req := httptest.NewRequest("GET", "/api/v1/me", nil)
	req = req.WithContext(context.WithValue(req.Context(), middleware.UserContextKey, &middleware.Claims{Email: email}))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp
}

func TestUsers_Me_GrafanaURLOnlyForMonitoringHolders(t *testing.T) {
	cm := roleConfigMap(monitoringTestUsers)
	cm.Data["monitoring"] = `["dev@test.com"]`
	client := fake.NewClientset(kipperSystemNamespace(), cm)
	store := middleware.NewRoleStore(client)
	handler := &Users{Client: client, RoleStore: store, GrafanaURL: func(context.Context) string { return "https://grafana.example.com" }}

	for _, tt := range []struct {
		email          string
		wantMonitoring bool
		wantURL        string
	}{
		{"admin@test.com", true, "https://grafana.example.com"},
		{"dev@test.com", true, "https://grafana.example.com"},
		{"viewer@test.com", false, ""},
	} {
		t.Run(tt.email, func(t *testing.T) {
			resp := meRequest(t, handler, store, tt.email)
			if resp["monitoring"] != tt.wantMonitoring {
				t.Errorf("monitoring = %v, want %v", resp["monitoring"], tt.wantMonitoring)
			}
			got, _ := resp["grafanaUrl"].(string)
			if got != tt.wantURL {
				t.Errorf("grafanaUrl = %q, want %q", got, tt.wantURL)
			}
		})
	}
}

func TestUsers_Me_NoGrafanaURLWhileRouteIsDown(t *testing.T) {
	cm := roleConfigMap(monitoringTestUsers)
	client := fake.NewClientset(kipperSystemNamespace(), cm)
	store := middleware.NewRoleStore(client)
	handler := &Users{Client: client, RoleStore: store, GrafanaURL: func(context.Context) string { return "" }}

	resp := meRequest(t, handler, store, "admin@test.com")
	if _, ok := resp["grafanaUrl"]; ok {
		t.Errorf("grafanaUrl present while the route is down: %v", resp["grafanaUrl"])
	}
}
