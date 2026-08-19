package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/getkipper/kipper/console-api/middleware"
	"github.com/getkipper/kipper/controller/pkg/sharedcred"
)

const testAdminEmail = "admin@kipper.local"
const testAdminPassword = "correct-horse-battery-staple"

func seedGitCredential(t *testing.T, client *fake.Clientset) {
	t.Helper()
	entries := []sharedcred.Entry{
		{Name: "git-acme-tools", Server: "git.example.com", Username: "kipper-deploy", Token: "glpa-abc123"},
	}
	data, _ := json.Marshal(entries)
	_, err := client.CoreV1().Secrets(sharedcred.Namespace).Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: sharedcred.ConfigSecretName, Namespace: sharedcred.Namespace},
		Data:       map[string][]byte{"credentials": data},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
}

func seedRegistry(t *testing.T, client *fake.Clientset, name, password string) {
	t.Helper()
	entries := []registryEntry{
		{Name: name, Server: "ghcr.io", Username: "acme", Password: password},
	}
	data, _ := json.Marshal(entries) //nolint:gosec // test fixture: serialising the registry entry shape is the whole point of the test
	_, err := client.CoreV1().Secrets(registryNamespace).Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: registryConfigName, Namespace: registryNamespace},
		Data:       map[string][]byte{"registries": data},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
}

func revealRequest(t *testing.T, name, password string) *http.Request {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"password": password})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/settings/git-credentials/"+name+"/reveal", bytes.NewReader(body))
	ctx := context.WithValue(req.Context(), middleware.UserContextKey, &middleware.Claims{Email: testAdminEmail})
	return req.WithContext(ctx)
}

func gitRouter(gc *GitCredentials) *chi.Mux {
	r := chi.NewRouter()
	r.Post("/api/v1/settings/git-credentials/{name}/reveal", gc.Reveal)
	return r
}

func registryRouter(reg *Registry) *chi.Mux {
	r := chi.NewRouter()
	r.Post("/api/v1/settings/registries/{name}/reveal", reg.Reveal)
	return r
}

func TestGitCredentialReveal_HappyPath(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck // matches project test pattern
	seedDexConfig(t, client)
	seedGitCredential(t, client)

	w := httptest.NewRecorder()
	gitRouter(&GitCredentials{Client: client}).ServeHTTP(w, revealRequest(t, "git-acme-tools", testAdminPassword))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if resp["token"] != "glpa-abc123" {
		t.Errorf("expected plaintext token, got %q", resp["token"])
	}
}

func TestGitCredentialReveal_WrongPassword(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck // matches project test pattern
	seedDexConfig(t, client)
	seedGitCredential(t, client)

	w := httptest.NewRecorder()
	gitRouter(&GitCredentials{Client: client}).ServeHTTP(w, revealRequest(t, "git-acme-tools", "nope"))

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGitCredentialReveal_UnknownCredential(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck // matches project test pattern
	seedDexConfig(t, client)
	seedGitCredential(t, client)

	w := httptest.NewRecorder()
	gitRouter(&GitCredentials{Client: client}).ServeHTTP(w, revealRequest(t, "does-not-exist", testAdminPassword))

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestGitCredentialReveal_MissingPassword(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck // matches project test pattern
	seedGitCredential(t, client)

	w := httptest.NewRecorder()
	gitRouter(&GitCredentials{Client: client}).ServeHTTP(w, revealRequest(t, "git-acme-tools", ""))

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestGitCredentialReveal_NotAuthenticated(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck // matches project test pattern
	seedDexConfig(t, client)
	seedGitCredential(t, client)

	body, _ := json.Marshal(map[string]string{"password": testAdminPassword})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/settings/git-credentials/git-acme-tools/reveal", bytes.NewReader(body))
	// No user in context.

	w := httptest.NewRecorder()
	gitRouter(&GitCredentials{Client: client}).ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestRegistryReveal_HappyPath(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck // matches project test pattern
	seedDexConfig(t, client)
	seedRegistry(t, client, "ghcr-io", "ghp_xyz789")

	body, _ := json.Marshal(map[string]string{"password": testAdminPassword})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/settings/registries/ghcr-io/reveal", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), middleware.UserContextKey, &middleware.Claims{Email: testAdminEmail}))

	w := httptest.NewRecorder()
	registryRouter(&Registry{Client: client}).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if resp["password"] != "ghp_xyz789" {
		t.Errorf("expected plaintext password, got %q", resp["password"])
	}
}

func TestRegistryReveal_WrongPassword(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck // matches project test pattern
	seedDexConfig(t, client)
	seedRegistry(t, client, "ghcr-io", "ghp_xyz789")

	body, _ := json.Marshal(map[string]string{"password": "wrong"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/settings/registries/ghcr-io/reveal", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), middleware.UserContextKey, &middleware.Claims{Email: testAdminEmail}))

	w := httptest.NewRecorder()
	registryRouter(&Registry{Client: client}).ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}
