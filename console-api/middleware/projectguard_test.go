package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"k8s.io/client-go/kubernetes/fake"
)

func newQueryScopeResolver(t *testing.T) *ProjectAccessResolver {
	t.Helper()
	client := fake.NewClientset(
		kipperNamespace(),
		projectNamespace("blog", "blog"),
		projectNamespace("shop", "shop"),
	)
	roles := NewRoleStore(fake.NewClientset(kipperNamespace(), roleConfigMap(
		`{"root@test.com":"admin","dev@test.com":"member","outsider@test.com":"member"}`,
	)))
	members := stubMembers{
		"blog": {"dev@test.com": "deployer"},
	}
	return NewProjectAccessResolver(client, roles, members, ownersFor(t,
		projectNamespace("blog", "blog"),
		projectNamespace("shop", "shop"),
	))
}

func requestWithUser(method, target, email string) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	if email != "" {
		ctx := context.WithValue(req.Context(), UserContextKey, &Claims{Email: email})
		req = req.WithContext(ctx)
	}
	return req
}

func TestProjectScopeQuery(t *testing.T) {
	resolver := newQueryScopeResolver(t)
	scoped := ProjectScopeQuery(resolver)

	tests := []struct {
		name   string
		email  string
		target string
		want   int
	}{
		{"member reaches their project", "dev@test.com", "/services/db?namespace=blog", http.StatusOK},
		{"non-member denied", "dev@test.com", "/services/db?namespace=shop", http.StatusForbidden},
		{"unknown user denied", "ghost@test.com", "/services/db?namespace=blog", http.StatusForbidden},
		{"admin reaches any project", "root@test.com", "/services/db?namespace=shop", http.StatusOK},
		{"missing namespace is a bad request", "dev@test.com", "/services/db", http.StatusBadRequest},
		{"missing user is unauthorized", "", "/services/db?namespace=blog", http.StatusUnauthorized},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			h := scoped(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.WriteHeader(http.StatusOK)
			}))

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, requestWithUser("GET", tt.target, tt.email))

			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d", rec.Code, tt.want)
			}
			if tt.want == http.StatusOK && !called {
				t.Fatal("expected the next handler to run")
			}
			if tt.want != http.StatusOK && called {
				t.Fatal("next handler ran despite a denial")
			}
		})
	}
}
