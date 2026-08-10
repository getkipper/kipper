package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func roleConfigMap(usersJSON string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      roleConfigMapName,
			Namespace: roleConfigMapNamespace,
		},
		Data: map[string]string{
			"users": usersJSON,
		},
	}
}

func kipperNamespace() *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: roleConfigMapNamespace},
	}
}

func TestRoleStore_GetRole_ReturnsCorrectRole(t *testing.T) {
	cm := roleConfigMap(`{"admin@test.com":"admin","dev@test.com":"deployer","viewer@test.com":"viewer"}`)
	client := fake.NewClientset(kipperNamespace(), cm)
	store := NewRoleStore(client)

	tests := []struct {
		email    string
		expected string
	}{
		{"admin@test.com", RoleAdmin},
		{"dev@test.com", RoleDeployer},
		{"viewer@test.com", RoleViewer},
	}

	for _, tt := range tests {
		t.Run(tt.email, func(t *testing.T) {
			role := store.GetRole(tt.email)
			if role != tt.expected {
				t.Errorf("expected role %q for %s, got %q", tt.expected, tt.email, role)
			}
		})
	}
}

func TestRoleStore_GetRole_NoConfigMap_FailsClosed(t *testing.T) {
	// With no kipper-users ConfigMap (and no admin seeded), access must be
	// denied rather than auto-granting admin to whoever logs in first.
	client := fake.NewClientset(kipperNamespace())
	store := NewRoleStore(client)

	role := store.GetRole("anyone@test.com")
	if role != "" {
		t.Errorf("expected empty role (fail closed) when no ConfigMap exists, got %q", role)
	}
}

func TestRoleStore_GetRole_UnknownUser_ReturnsEmpty(t *testing.T) {
	cm := roleConfigMap(`{"admin@test.com":"admin"}`)
	client := fake.NewClientset(kipperNamespace(), cm)
	store := NewRoleStore(client)

	role := store.GetRole("unknown@test.com")
	if role != "" {
		t.Errorf("expected empty role for unknown user, got %q", role)
	}
}

func TestRoleStore_SetRole(t *testing.T) {
	client := fake.NewClientset(kipperNamespace())
	store := NewRoleStore(client)

	ctx := context.Background()
	if err := store.SetRole(ctx, "new@test.com", RoleDeployer); err != nil {
		t.Fatalf("SetRole failed: %v", err)
	}

	// Force cache expiry by resetting lastFetch
	store.mu.Lock()
	store.lastFetch = store.lastFetch.Add(-store.cacheTTL * 2)
	store.mu.Unlock()

	role := store.GetRole("new@test.com")
	if role != RoleDeployer {
		t.Errorf("expected role %q after SetRole, got %q", RoleDeployer, role)
	}
}

func TestRoleStore_RemoveUser(t *testing.T) {
	cm := roleConfigMap(`{"admin@test.com":"admin","remove@test.com":"viewer"}`)
	client := fake.NewClientset(kipperNamespace(), cm)
	store := NewRoleStore(client)

	ctx := context.Background()
	if err := store.RemoveUser(ctx, "remove@test.com"); err != nil {
		t.Fatalf("RemoveUser failed: %v", err)
	}

	store.mu.Lock()
	store.lastFetch = store.lastFetch.Add(-store.cacheTTL * 2)
	store.mu.Unlock()

	role := store.GetRole("remove@test.com")
	if role != "" {
		t.Errorf("expected empty role after removal, got %q", role)
	}

	// The other user should still be present
	adminRole := store.GetRole("admin@test.com")
	if adminRole != RoleAdmin {
		t.Errorf("expected admin role to persist, got %q", adminRole)
	}
}

func TestRoleStore_ListUsers(t *testing.T) {
	cm := roleConfigMap(`{"admin@test.com":"admin","dev@test.com":"deployer"}`)
	client := fake.NewClientset(kipperNamespace(), cm)
	store := NewRoleStore(client)

	users := store.ListUsers()
	if len(users) != 2 {
		t.Fatalf("expected 2 users, got %d", len(users))
	}
	if users["admin@test.com"] != RoleAdmin {
		t.Errorf("expected admin role, got %q", users["admin@test.com"])
	}
	if users["dev@test.com"] != RoleDeployer {
		t.Errorf("expected deployer role, got %q", users["dev@test.com"])
	}
}

func TestRoleMiddleware_InjectsRole(t *testing.T) {
	cm := roleConfigMap(`{"admin@test.com":"admin"}`)
	client := fake.NewClientset(kipperNamespace(), cm)
	store := NewRoleStore(client)

	var capturedRole string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedRole = RoleFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	handler := RoleMiddleware(store)(inner)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	claims := &Claims{Email: "admin@test.com", Name: "Admin"}
	ctx := context.WithValue(r.Context(), UserContextKey, claims)
	r = r.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if capturedRole != RoleAdmin {
		t.Errorf("expected role %q in context, got %q", RoleAdmin, capturedRole)
	}
}

func TestRoleMiddleware_NoRole_Returns403(t *testing.T) {
	cm := roleConfigMap(`{"admin@test.com":"admin"}`)
	client := fake.NewClientset(kipperNamespace(), cm)
	store := NewRoleStore(client)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called for user without role")
	})

	handler := RoleMiddleware(store)(inner)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	claims := &Claims{Email: "nobody@test.com", Name: "Nobody"}
	ctx := context.WithValue(r.Context(), UserContextKey, claims)
	r = r.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", w.Code)
	}
}

func TestRequireRole_AllowsAdmin(t *testing.T) {
	var called bool
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	handler := RequireRole(RoleAdmin)(inner)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx := context.WithValue(r.Context(), RoleContextKey, RoleAdmin)
	r = r.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if !called {
		t.Error("expected inner handler to be called for admin")
	}
}

func TestRequireRole_DeniesViewer(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called for viewer on admin-only route")
	})

	handler := RequireRole(RoleAdmin)(inner)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx := context.WithValue(r.Context(), RoleContextKey, RoleViewer)
	r = r.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", w.Code)
	}
}

func TestRoleFromContext_ReturnsEmptyWhenNotSet(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	role := RoleFromContext(r.Context())
	if role != "" {
		t.Errorf("expected empty role from context without value, got %q", role)
	}
}
