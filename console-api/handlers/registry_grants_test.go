package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/getkipper/kipper/console-api/internal/registrycred"
)

func storedRegistries(t *testing.T, entries ...registrycred.Entry) *corev1.Secret {
	t.Helper()
	data, err := json.Marshal(entries) //nolint:gosec // test fixture: the password is an invented string, and the store is a K8s Secret
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: registrycred.ConfigSecretName, Namespace: registrycred.Namespace},
		Data:       map[string][]byte{"registries": data},
	}
}

func postRegistry(t *testing.T, handler *Registry, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	r.Post("/settings/registries", handler.Add)

	req := httptest.NewRequest("POST", "/settings/registries", strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// The defect this exists for, and it is live on the fleet: the console's registry
// form edits the password, sends no allow-list, and the handler replaced the whole
// entry with what arrived. Rotating a password therefore revoked every project and
// the controllers pulled the staged pull secret out from under their workloads.
func TestAddRegistry_RotatingAPasswordKeepsTheProjectsAllowed(t *testing.T) {
	client := fake.NewClientset(storedRegistries(t, registrycred.Entry{
		Name: "ghcr", Server: "ghcr.io", Username: "deploy",
		Password: "the-old-password", AllowedProjects: []string{"shop", "blog"},
	}))
	handler := &Registry{Client: client}

	rec := postRegistry(t, handler,
		`{"name":"ghcr","server":"ghcr.io","username":"deploy","password":"the-new-password"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body)
	}

	entries, err := registrycred.Load(context.Background(), client)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if entries[0].Password != "the-new-password" {
		t.Errorf("password = %q", entries[0].Password)
	}
	if !entries[0].AllowsProject("shop") || !entries[0].AllowsProject("blog") {
		t.Errorf("rotating a password revoked every project: %v", entries[0].AllowedProjects)
	}
}

// A request that would rewrite an existing credential's grants is refused rather
// than answered with "saved" and applied in part. The published shape carries the
// field, so dropping it quietly would leave a caller believing an authorization
// change had been made.
func TestAddRegistry_RefusesAnAllowListThatChangesTheGrants(t *testing.T) {
	client := fake.NewClientset(storedRegistries(t, registrycred.Entry{
		Name: "ghcr", Server: "ghcr.io", Username: "deploy",
		Password: "p", AllowedProjects: []string{"shop", "blog"},
	}))
	handler := &Registry{Client: client}

	rec := postRegistry(t, handler,
		`{"name":"ghcr","server":"ghcr.io","username":"deploy","password":"p","allowedProjects":["shop"]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("an authorization change was answered with %d: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "kip registry allow") {
		t.Errorf("the refusal does not say where grants are changed: %s", rec.Body)
	}

	entries, _ := registrycred.Load(context.Background(), client)
	if !entries[0].AllowsProject("blog") {
		t.Errorf("a refused request revoked a project: %v", entries[0].AllowedProjects)
	}
}

// The read-modify-write an integration has been able to do since 0.9.0: read the
// entry, put a fresh password in it, send the whole thing back. It changes no
// grants, so it has to keep working.
func TestAddRegistry_AcceptsTheAllowListItAlreadyHas(t *testing.T) {
	client := fake.NewClientset(storedRegistries(t, registrycred.Entry{
		Name: "ghcr", Server: "ghcr.io", Username: "deploy",
		Password: "p", AllowedProjects: []string{"shop", "blog"},
	}))
	handler := &Registry{Client: client}

	rec := postRegistry(t, handler,
		`{"name":"ghcr","server":"ghcr.io","username":"deploy","password":"rotated","allowedProjects":["blog","shop"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("a rotation that changes no grant was refused: %d %s", rec.Code, rec.Body)
	}
}

// null replaced the list under the old shape, which makes it a revocation.
func TestAddRegistry_RefusesANullAllowListThatWouldRevoke(t *testing.T) {
	client := fake.NewClientset(storedRegistries(t, registrycred.Entry{
		Name: "ghcr", Server: "ghcr.io", Username: "deploy",
		Password: "p", AllowedProjects: []string{"shop"},
	}))
	handler := &Registry{Client: client}

	rec := postRegistry(t, handler,
		`{"name":"ghcr","server":"ghcr.io","username":"deploy","password":"p","allowedProjects":null}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a revocation was answered with %d: %s", rec.Code, rec.Body)
	}
}

// Creating is the one case where the list may be set here: nothing exists to
// overwrite, so there is no grant to lose.
func TestAddRegistry_TakesTheAllowListWhenCreating(t *testing.T) {
	client := fake.NewClientset()
	handler := &Registry{Client: client}

	rec := postRegistry(t, handler,
		`{"name":"ghcr","server":"ghcr.io","username":"deploy","password":"p","allowedProjects":["shop","shop"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body)
	}

	entries, _ := registrycred.Load(context.Background(), client)
	if len(entries[0].AllowedProjects) != 1 || !entries[0].AllowsProject("shop") {
		t.Errorf("allowed projects = %v", entries[0].AllowedProjects)
	}
}

// A credential nobody has granted allows nobody, and that is stored as a
// decision rather than as an absent field.
func TestAddRegistry_RecordsAnEmptyListWhenCreatingWithoutOne(t *testing.T) {
	client := fake.NewClientset()
	handler := &Registry{Client: client}

	postRegistry(t, handler, `{"name":"ghcr","server":"ghcr.io","username":"deploy","password":"p"}`)

	live, err := client.CoreV1().Secrets(registrycred.Namespace).Get(
		context.Background(), registrycred.ConfigSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !strings.Contains(string(live.Data["registries"]), `"allowedProjects":[]`) {
		t.Errorf("a new credential stored no decision at all: %s", live.Data["registries"])
	}
}
