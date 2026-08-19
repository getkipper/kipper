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

	"github.com/getkipper/kipper/controller/pkg/sharedcred"
)

func storedCredentials(t *testing.T, entries ...sharedcred.Entry) *corev1.Secret {
	t.Helper()
	data, err := json.Marshal(entries)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: sharedcred.ConfigSecretName, Namespace: sharedcred.Namespace},
		Data:       map[string][]byte{"credentials": data},
	}
}

func postCredential(t *testing.T, handler *GitCredentials, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	r.Post("/settings/git-credentials", handler.Add)

	req := httptest.NewRequest("POST", "/settings/git-credentials", strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// The console form edits the token, not who may use it, and its request carries
// no allow-list at all. Replacing the entry with what arrived revoked every
// project as a side effect of rotating a token, and the token health panel is
// what sends an admin to rotate one.
func TestAddGitCredential_RotatingATokenKeepsTheProjectsAllowed(t *testing.T) {
	client := fake.NewClientset(storedCredentials(t, sharedcred.Entry{
		Name: "forge", Server: "git.example.com", Username: "deploy",
		Token: "the-old-token", AllowedProjects: []string{"shop", "blog"},
	}))
	handler := &GitCredentials{Client: client, CRClient: testCRClient()}

	rec := postCredential(t, handler,
		`{"name":"forge","server":"git.example.com","username":"deploy","token":"the-new-token"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body)
	}

	entries, err := sharedcred.Load(context.Background(), client)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if entries[0].Token != "the-new-token" {
		t.Errorf("token = %q", entries[0].Token)
	}
	if !entries[0].AllowsProject("shop") || !entries[0].AllowsProject("blog") {
		t.Errorf("rotating a token revoked every project: %v", entries[0].AllowedProjects)
	}
}

// A new credential allows nobody, and that has to be stored as a decision.
// Stored as nothing, the next upgrade reads it as a credential predating
// allow-lists and grants it whatever references it.
func TestAddGitCredential_RecordsThatANewCredentialAllowsNobody(t *testing.T) {
	client := fake.NewClientset()
	handler := &GitCredentials{Client: client, CRClient: testCRClient()}

	rec := postCredential(t, handler, `{"name":"forge","server":"git.example.com","token":"a-token"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body)
	}

	live, err := client.CoreV1().Secrets(sharedcred.Namespace).Get(
		context.Background(), sharedcred.ConfigSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !strings.Contains(string(live.Data["credentials"]), `"allowedProjects":[]`) {
		t.Errorf("a new credential stored no decision at all: %s", live.Data["credentials"])
	}
}

// The console must not take the whole object with it. Anything else stored on
// the Secret, and its own metadata, belongs to whoever put it there.
func TestAddGitCredential_KeepsWhatElseIsOnTheSecret(t *testing.T) {
	secret := storedCredentials(t, sharedcred.Entry{Name: "forge", Server: "git.example.com", Token: "a-token"})
	secret.Data["unrelated"] = []byte("kept")
	secret.Annotations = map[string]string{"kipper.run/note": "kept"}
	client := fake.NewClientset(secret)
	handler := &GitCredentials{Client: client, CRClient: testCRClient()}

	postCredential(t, handler, `{"name":"forge","server":"git.example.com","token":"a-new-token"}`)

	live, err := client.CoreV1().Secrets(sharedcred.Namespace).Get(
		context.Background(), sharedcred.ConfigSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(live.Data["unrelated"]) != "kept" || live.Annotations["kipper.run/note"] != "kept" {
		t.Error("the write replaced the Secret rather than editing it")
	}
}

func TestRemoveGitCredential_ReportsANameThatIsNotThere(t *testing.T) {
	client := fake.NewClientset(storedCredentials(t, sharedcred.Entry{Name: "forge"}))
	handler := &GitCredentials{Client: client, CRClient: testCRClient()}

	r := chi.NewRouter()
	r.Delete("/settings/git-credentials/{name}", handler.Remove)

	req := httptest.NewRequest("DELETE", "/settings/git-credentials/missing", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", rec.Code, rec.Body)
	}
}

// A request that would rewrite an existing credential's grants is refused rather
// than answered with "saved" and applied in part. It accepted one until 0.13, so
// dropping the field quietly would leave unattended callers believing an
// authorization change had been made.
func TestAddGitCredential_RefusesAnAllowListThatChangesTheGrants(t *testing.T) {
	client := fake.NewClientset(storedCredentials(t, sharedcred.Entry{
		Name: "forge", Server: "git.example.com", Token: "a-token",
		AllowedProjects: []string{"shop", "blog"},
	}))
	handler := &GitCredentials{Client: client, CRClient: testCRClient()}

	rec := postCredential(t, handler,
		`{"name":"forge","server":"git.example.com","token":"a-new-token","allowedProjects":["shop"]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("an authorization change was answered with %d rather than refused: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "kip credentials allow") {
		t.Errorf("the refusal does not say where grants are changed: %s", rec.Body)
	}

	entries, err := sharedcred.Load(context.Background(), client)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !entries[0].AllowsProject("blog") {
		t.Errorf("a refused request revoked a project: %v", entries[0].AllowedProjects)
	}
	if entries[0].Token != "a-token" {
		t.Errorf("a refused request rotated the token anyway: %q", entries[0].Token)
	}
}

// The read-modify-write an integration has been able to do since 0.9.0: read the
// entry, put a fresh token in it, send the whole thing back. It changes no
// grants, so it has to keep working.
func TestAddGitCredential_AcceptsTheAllowListItAlreadyHas(t *testing.T) {
	client := fake.NewClientset(storedCredentials(t, sharedcred.Entry{
		Name: "forge", Server: "git.example.com", Token: "a-token",
		AllowedProjects: []string{"shop", "blog"},
	}))
	handler := &GitCredentials{Client: client, CRClient: testCRClient()}

	rec := postCredential(t, handler,
		`{"name":"forge","server":"git.example.com","token":"a-new-token","allowedProjects":["blog","shop"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("a rotation that changes no grant was refused: %d %s", rec.Code, rec.Body)
	}

	entries, err := sharedcred.Load(context.Background(), client)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if entries[0].Token != "a-new-token" {
		t.Errorf("token = %q", entries[0].Token)
	}
	if !entries[0].AllowsProject("shop") || !entries[0].AllowsProject("blog") {
		t.Errorf("allowed projects = %v", entries[0].AllowedProjects)
	}
}

// null replaced the list under the old shape, which makes it a revocation. A
// Go slice cannot tell it from a field left out, so the raw body decides.
func TestAddGitCredential_RefusesANullAllowListThatWouldRevoke(t *testing.T) {
	client := fake.NewClientset(storedCredentials(t, sharedcred.Entry{
		Name: "forge", Server: "git.example.com", Token: "a-token",
		AllowedProjects: []string{"shop"},
	}))
	handler := &GitCredentials{Client: client, CRClient: testCRClient()}

	rec := postCredential(t, handler,
		`{"name":"forge","server":"git.example.com","token":"a-new-token","allowedProjects":null}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a revocation was answered with %d: %s", rec.Code, rec.Body)
	}

	entries, err := sharedcred.Load(context.Background(), client)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !entries[0].AllowsProject("shop") || entries[0].Token != "a-token" {
		t.Errorf("a refused request changed something: %+v", entries[0])
	}
}

// Creating is the one case where the list may be set here: nothing exists to
// overwrite, so there is no grant to lose, and this is how the published shape
// has always created a credential with projects on it.
func TestAddGitCredential_TakesTheAllowListWhenCreating(t *testing.T) {
	client := fake.NewClientset()
	handler := &GitCredentials{Client: client, CRClient: testCRClient()}

	rec := postCredential(t, handler,
		`{"name":"forge","server":"git.example.com","token":"a-token","allowedProjects":["shop"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body)
	}

	entries, err := sharedcred.Load(context.Background(), client)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !entries[0].AllowsProject("shop") {
		t.Errorf("allowed projects = %v", entries[0].AllowedProjects)
	}
}

// Who may build is a question about membership. A name repeated in a create is
// stored once, and a later rotation carrying either spelling changes nothing, so
// neither may be refused as a change.
func TestAddGitCredential_TreatsARepeatedProjectAsTheSameAuthorization(t *testing.T) {
	client := fake.NewClientset()
	handler := &GitCredentials{Client: client, CRClient: testCRClient()}

	rec := postCredential(t, handler,
		`{"name":"forge","server":"git.example.com","token":"a-token","allowedProjects":["shop","shop"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body)
	}

	entries, err := sharedcred.Load(context.Background(), client)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(entries[0].AllowedProjects) != 1 {
		t.Errorf("a repeated name was stored twice: %v", entries[0].AllowedProjects)
	}

	rotated := postCredential(t, handler,
		`{"name":"forge","server":"git.example.com","token":"a-new-token","allowedProjects":["shop"]}`)
	if rotated.Code != http.StatusOK {
		t.Fatalf("a rotation carrying the same set was refused: %d %s", rotated.Code, rotated.Body)
	}
}

// An empty list against a credential nobody has decided about is a decision,
// even though it changes nothing about who may build: the upgrade reads an
// absent list as one it may fill from the apps that reference the credential, so
// keeping the absent list would let the migration undo the caller's revocation.
func TestAddGitCredential_RecordsAnEmptyDecisionOnACredentialNobodyHasDecided(t *testing.T) {
	client := fake.NewClientset(storedCredentials(t, sharedcred.Entry{
		Name: "forge", Server: "git.example.com", Token: "a-token",
	}))
	handler := &GitCredentials{Client: client, CRClient: testCRClient()}

	rec := postCredential(t, handler,
		`{"name":"forge","server":"git.example.com","token":"a-new-token","allowedProjects":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body)
	}

	live, err := client.CoreV1().Secrets(sharedcred.Namespace).Get(
		context.Background(), sharedcred.ConfigSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !strings.Contains(string(live.Data["credentials"]), `"allowedProjects":[]`) {
		t.Errorf("an explicit revocation was stored as no decision at all: %s", live.Data["credentials"])
	}
}
