package migration

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestRequireMigrationSecret(t *testing.T) {
	h := &Handler{Sessions: NewSessionStore()}
	h.Sessions.Put(&Session{ID: "live", Secret: "correct-horse", ExpiresAt: time.Now().Add(time.Hour)})
	h.Sessions.Put(&Session{ID: "stale", Secret: "correct-horse", ExpiresAt: time.Now().Add(-time.Minute)})

	router := chi.NewRouter()
	router.With(h.RequireMigrationSecret).Post("/{session}/resource", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	tests := []struct {
		name    string
		session string
		secret  string
		setHdr  bool
		want    int
	}{
		{"valid session and secret", "live", "correct-horse", true, http.StatusNoContent},
		{"wrong secret", "live", "guessed", true, http.StatusUnauthorized},
		{"missing secret header", "live", "", false, http.StatusUnauthorized},
		{"empty secret header", "live", "", true, http.StatusUnauthorized},
		{"unknown session", "does-not-exist", "correct-horse", true, http.StatusUnauthorized},
		{"expired session", "stale", "correct-horse", true, http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/"+tt.session+"/resource", nil)
			if tt.setHdr {
				req.Header.Set("X-Migration-Secret", tt.secret)
			}
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d", rec.Code, tt.want)
			}
		})
	}
}

func TestRequireMigrationSecret_ExpiredSessionIsEvicted(t *testing.T) {
	h := &Handler{Sessions: NewSessionStore()}
	h.Sessions.Put(&Session{ID: "stale", Secret: "s", ExpiresAt: time.Now().Add(-time.Minute)})

	router := chi.NewRouter()
	router.With(h.RequireMigrationSecret).Post("/{session}/resource", func(w http.ResponseWriter, r *http.Request) {})

	req := httptest.NewRequest(http.MethodPost, "/stale/resource", nil)
	req.Header.Set("X-Migration-Secret", "s")
	router.ServeHTTP(httptest.NewRecorder(), req)

	if _, ok := h.Sessions.Get("stale"); ok {
		t.Fatal("expired session should be evicted from the store")
	}
}

func TestConsumeTokenIsSingleUse(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: tokenSecretName, Namespace: tokenSecretNamespace},
	})
	ctx := context.Background()

	if err := ConsumeToken(ctx, client); err != nil {
		t.Fatalf("first consume should succeed: %v", err)
	}
	if err := ConsumeToken(ctx, client); err == nil {
		t.Fatal("second consume should fail: the token is already consumed")
	}
}
