package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/getkipper/kipper/console-api/middleware"
	"github.com/getkipper/kipper/console-api/security"
	"github.com/getkipper/kipper/console-api/uisession"
)

func roleConfigMap(usersJSON string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kipper-users",
			Namespace: "kipper-system",
		},
		Data: map[string]string{
			"users": usersJSON,
		},
	}
}

func dexConfigMap() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dex-config",
			Namespace: "dex",
		},
		Data: map[string]string{
			"config.yaml": "issuer: https://dex.test\nstaticPasswords: []\n",
		},
	}
}

func dexDeployment() *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dex",
			Namespace: "dex",
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "dex"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "dex"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "dex", Image: "dex:latest"}}},
			},
		},
	}
}

func kipperSystemNamespace() *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "kipper-system"},
	}
}

func dexNamespace() *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "dex"},
	}
}

func TestUsers_List(t *testing.T) {
	cm := roleConfigMap(`{"admin@test.com":"admin","dev@test.com":"deployer"}`)
	client := fake.NewClientset(kipperSystemNamespace(), cm)
	store := middleware.NewRoleStore(client)

	handler := &Users{Client: client, RoleStore: store}

	r := chi.NewRouter()
	r.Get("/api/v1/users", handler.List)

	req := httptest.NewRequest("GET", "/api/v1/users", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var users []userResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &users); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(users) != 2 {
		t.Fatalf("expected 2 users, got %d", len(users))
	}

	// Results should be sorted by email
	if users[0].Email != "admin@test.com" {
		t.Errorf("expected first user admin@test.com, got %s", users[0].Email)
	}
	if users[0].Role != "admin" {
		t.Errorf("expected admin role, got %s", users[0].Role)
	}
	if users[1].Email != "dev@test.com" {
		t.Errorf("expected second user dev@test.com, got %s", users[1].Email)
	}
	if users[1].Role != "deployer" {
		t.Errorf("expected deployer role, got %s", users[1].Role)
	}
}

func TestUsers_Create(t *testing.T) {
	cm := roleConfigMap(`{"admin@test.com":"admin"}`)
	client := fake.NewClientset(
		kipperSystemNamespace(), dexNamespace(),
		cm, dexConfigMap(), dexDeployment(),
	)
	store := middleware.NewRoleStore(client)

	handler := &Users{Client: client, RoleStore: store}

	r := chi.NewRouter()
	r.Post("/api/v1/users", handler.Create)

	body := `{"email":"new@test.com","password":"Secure-pass1!","role":"deployer"}`
	req := httptest.NewRequest("POST", "/api/v1/users", strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var resp userResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Email != "new@test.com" {
		t.Errorf("expected email new@test.com, got %s", resp.Email)
	}
	if resp.Role != "deployer" {
		t.Errorf("expected role deployer, got %s", resp.Role)
	}

	// Verify the user was added to the role store
	users := store.ListUsers()
	if users["new@test.com"] != "deployer" {
		t.Errorf("expected new user in role store with deployer role, got %q", users["new@test.com"])
	}

	// Verify the Dex ConfigMap was updated
	dexCM, err := client.CoreV1().ConfigMaps("dex").Get(context.Background(), "dex-config", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to read Dex config: %v", err)
	}
	if !strings.Contains(dexCM.Data["config.yaml"], "new@test.com") {
		t.Error("expected Dex config to contain new user email")
	}
}

func TestUsers_Create_ValidationErrors(t *testing.T) {
	client := fake.NewClientset(kipperSystemNamespace(), dexNamespace())
	store := middleware.NewRoleStore(client)
	handler := &Users{Client: client, RoleStore: store}

	tests := []struct {
		name        string
		body        string
		expectedErr string
	}{
		{
			name:        "rejects missing email",
			body:        `{"password":"Secure-pass1!","role":"admin"}`,
			expectedErr: "email and password are required",
		},
		{
			name:        "rejects missing password",
			body:        `{"email":"test@test.com","role":"admin"}`,
			expectedErr: "email and password are required",
		},
		{
			name:        "rejects weak password",
			body:        `{"email":"test@test.com","password":"weak","role":"deployer"}`,
			expectedErr: "password must be at least 8 characters",
		},
		{
			name:        "rejects invalid role",
			body:        `{"email":"test@test.com","password":"Secure-pass1!","role":"superuser"}`,
			expectedErr: "role must be admin, deployer, or viewer",
		},
		{
			name:        "rejects invalid JSON",
			body:        `{{{invalid`,
			expectedErr: "invalid request body",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := chi.NewRouter()
			r.Post("/api/v1/users", handler.Create)

			req := httptest.NewRequest("POST", "/api/v1/users", strings.NewReader(tt.body))
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("expected 400, got %d; body: %s", rec.Code, rec.Body.String())
			}

			var errResp map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
				t.Fatalf("failed to decode error: %v", err)
			}
			if errResp["error"] != tt.expectedErr {
				t.Errorf("expected error %q, got %q", tt.expectedErr, errResp["error"])
			}
		})
	}
}

func TestUsers_UpdateRole(t *testing.T) {
	cm := roleConfigMap(`{"admin@test.com":"admin","dev@test.com":"deployer"}`)
	client := fake.NewClientset(kipperSystemNamespace(), cm)
	store := middleware.NewRoleStore(client)

	handler := &Users{Client: client, RoleStore: store}

	r := chi.NewRouter()
	r.Put("/api/v1/users/{email}/role", handler.UpdateRole)

	body := `{"role":"viewer"}`
	req := httptest.NewRequest("PUT", "/api/v1/users/dev@test.com/role", strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var resp userResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Email != "dev@test.com" {
		t.Errorf("expected email dev@test.com, got %s", resp.Email)
	}
	if resp.Role != "viewer" {
		t.Errorf("expected role viewer, got %s", resp.Role)
	}

	// Verify the role was persisted
	users := store.ListUsers()
	if users["dev@test.com"] != "viewer" {
		t.Errorf("expected updated role in store, got %q", users["dev@test.com"])
	}
}

func TestUsers_UpdateRole_InvalidRole(t *testing.T) {
	cm := roleConfigMap(`{"admin@test.com":"admin"}`)
	client := fake.NewClientset(kipperSystemNamespace(), cm)
	store := middleware.NewRoleStore(client)

	handler := &Users{Client: client, RoleStore: store}

	r := chi.NewRouter()
	r.Put("/api/v1/users/{email}/role", handler.UpdateRole)

	body := `{"role":"superuser"}`
	req := httptest.NewRequest("PUT", "/api/v1/users/admin@test.com/role", strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestUsers_Delete(t *testing.T) {
	cm := roleConfigMap(`{"admin@test.com":"admin","remove@test.com":"viewer"}`)
	client := fake.NewClientset(
		kipperSystemNamespace(), dexNamespace(),
		cm, dexConfigMap(), dexDeployment(),
	)
	store := middleware.NewRoleStore(client)

	handler := &Users{Client: client, RoleStore: store}

	r := chi.NewRouter()
	r.Delete("/api/v1/users/{email}", handler.Delete)

	req := httptest.NewRequest("DELETE", "/api/v1/users/remove@test.com", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d; body: %s", rec.Code, rec.Body.String())
	}

	// Verify the user was removed from the role store
	users := store.ListUsers()
	if _, exists := users["remove@test.com"]; exists {
		t.Error("expected user to be removed from role store")
	}

	// The other user should still be present
	if users["admin@test.com"] != "admin" {
		t.Error("expected admin user to persist")
	}
}

func TestUsers_Delete_AbortsWhenSessionRevocationFails(t *testing.T) {
	// Record deletion is authoritative: if it fails, the whole delete must
	// abort with no account state changed, so a retry is clean and no
	// "account removed but sessions survive" state can exist.
	cm := roleConfigMap(`{"admin@test.com":"admin","remove@test.com":"viewer"}`)
	client := fake.NewClientset(
		kipperSystemNamespace(), dexNamespace(),
		cm, dexConfigMap(), dexDeployment(),
	)
	store := middleware.NewRoleStore(client)

	// A separate cluster for the session record store whose secret listing
	// always fails, so DeleteBySubject errors.
	recClient := fake.NewClientset()
	recClient.PrependReactor("list", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("down")
	})
	handler := &Users{
		Client:     client,
		RoleStore:  store,
		UISessions: uisession.NewRecordStore(recClient, uisession.SigningSecretNamespace),
	}

	r := chi.NewRouter()
	r.Delete("/api/v1/users/{email}", handler.Delete)
	req := httptest.NewRequest("DELETE", "/api/v1/users/remove@test.com", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 when session revocation fails, got %d; body: %s", rec.Code, rec.Body.String())
	}
	// Nothing else may have been removed.
	if users := store.ListUsers(); users["remove@test.com"] != "viewer" {
		t.Error("role must be intact after an aborted delete so a retry is clean")
	}
}

func TestUsers_Me(t *testing.T) {
	cm := roleConfigMap(`{"admin@kipper.local":"admin"}`)
	client := fake.NewClientset(kipperSystemNamespace(), cm)
	store := middleware.NewRoleStore(client)

	handler := &Users{Client: client, RoleStore: store}

	r := chi.NewRouter()
	r.Use(middleware.RoleMiddleware(store))
	r.Get("/api/v1/me", handler.Me)

	// Inject the authenticated user the way the auth middleware would.
	claims := &middleware.Claims{Email: "admin@kipper.local", Name: "Admin"}
	req := httptest.NewRequest("GET", "/api/v1/me", nil)
	req = req.WithContext(context.WithValue(req.Context(), middleware.UserContextKey, claims))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp["email"] != "admin@kipper.local" {
		t.Errorf("expected email admin@kipper.local, got %s", resp["email"])
	}
	if resp["name"] != "Admin" {
		t.Errorf("expected name Admin, got %s", resp["name"])
	}
	if resp["role"] != "admin" {
		t.Errorf("expected role admin, got %s", resp["role"])
	}
}

func TestUsers_Me_Unauthenticated(t *testing.T) {
	client := fake.NewClientset(kipperSystemNamespace())
	store := middleware.NewRoleStore(client)
	handler := &Users{Client: client, RoleStore: store}

	r := chi.NewRouter()
	r.Get("/api/v1/me", handler.Me)

	req := httptest.NewRequest("GET", "/api/v1/me", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// TestUsers_DeleteAlertsPreMutationRecipients proves the alert about
// deleting an admin reaches the deleted admin: the recipient set is pinned
// before the mutation, so the victim cannot be cut out of their own alert.
func TestUsers_DeleteAlertsPreMutationRecipients(t *testing.T) {
	cm := roleConfigMap(`{"admin@test.com":"admin","victim@test.com":"admin"}`)
	client := fake.NewClientset(
		kipperSystemNamespace(), dexNamespace(),
		cm, dexConfigMap(), dexDeployment(),
	)
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
		// Admins resolves against the store, which no longer contains the
		// victim by delivery time — the snapshot must win over this.
		Admins: func() []string {
			var admins []string
			for email, role := range store.ListUsers() {
				if role == middleware.RoleAdmin {
					admins = append(admins, email)
				}
			}
			return admins
		},
	}}

	handler := &Users{Client: client, RoleStore: store, Security: notifier}

	r := chi.NewRouter()
	r.Delete("/api/v1/users/{email}", handler.Delete)

	req := httptest.NewRequest("DELETE", "/api/v1/users/victim@test.com", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d; body: %s", rec.Code, rec.Body.String())
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := append([]string(nil), emailed...)
		mu.Unlock()
		if len(got) >= 2 {
			found := false
			for _, to := range got {
				if to == "victim@test.com" {
					found = true
				}
			}
			if !found {
				t.Fatalf("deleted admin missing from alert recipients: %v", got)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("security email delivery not observed before deadline")
}
