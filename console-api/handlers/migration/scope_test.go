package migration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func projectNamespace(name, project string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{"kipper.run/project": project},
		},
	}
}

func TestProjectAccepted(t *testing.T) {
	h := &Handler{Sessions: NewSessionStore()}
	h.Sessions.Put(&Session{ID: "s1", Projects: []string{"shop", "billing"}, Secret: "test-session-secret"})

	tests := []struct {
		name    string
		session string
		project string
		want    bool
	}{
		{"accepted project", "s1", "shop", true},
		{"other accepted project", "s1", "billing", true},
		{"project not accepted", "s1", "payments", false},
		{"empty project", "s1", "", false},
		{"unknown session", "nope", "shop", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := h.projectAccepted(tt.session, tt.project); got != tt.want {
				t.Fatalf("projectAccepted(%q, %q) = %v, want %v", tt.session, tt.project, got, tt.want)
			}
		})
	}
}

// A project owns one namespace per environment, tagged with kipper.run/project.
// The scope check must resolve the namespace to its project rather than compare
// names, or a normal migration of project "shop" into namespace "shop-test"
// would be refused.
func TestNamespaceInScope(t *testing.T) {
	h := &Handler{
		Sessions: NewSessionStore(),
		Client: fake.NewSimpleClientset(
			projectNamespace("shop-test", "shop"),
			projectNamespace("shop-prod", "shop"),
			projectNamespace("payments-prod", "payments"),
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "unlabelled"}},
		),
		// Scope resolves ownership from the claim, so the projects that hold
		// these namespaces have to exist. "unlabelled" deliberately has no
		// owner, which is what makes it out of scope.
		CRClient: migrationOwners(t),
	}
	h.Sessions.Put(&Session{ID: "s1", Projects: []string{"shop"}, Secret: "test-session-secret"})

	tests := []struct {
		name      string
		namespace string
		want      bool
	}{
		{"accepted project, test env", "shop-test", true},
		{"accepted project, prod env", "shop-prod", true},
		{"unaccepted project namespace", "payments-prod", false},
		{"namespace without a project label", "unlabelled", false},
		{"namespace that does not exist", "ghost", false},
		{"empty namespace", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := h.namespaceInScope(context.Background(), "s1", tt.namespace); got != tt.want {
				t.Fatalf("namespaceInScope(%q) = %v, want %v", tt.namespace, got, tt.want)
			}
		})
	}
}

// A valid migration secret must not be enough to write into a project the
// target admin never accepted, even when the environment namespace name differs
// from the project name.
func TestReceiveResourceHandler_RejectsOutOfScope(t *testing.T) {
	h := &Handler{
		Sessions: NewSessionStore(),
		Client:   fake.NewSimpleClientset(projectNamespace("payments-prod", "payments")),
		CRClient: migrationOwners(t),
	}
	h.Sessions.Put(&Session{ID: "s1", Projects: []string{"shop"}, Secret: "test-session-secret"})

	router := chi.NewRouter()
	router.Post("/{session}/resource", h.ReceiveResourceHandler)

	cases := []struct {
		name string
		body map[string]any
	}{
		{"app in an unaccepted project's namespace", map[string]any{"kind": "App", "name": "web", "namespace": "payments-prod", "spec": map[string]any{}}},
		{"app in a namespace that does not exist", map[string]any{"kind": "App", "name": "web", "namespace": "ghost", "spec": map[string]any{}}},
		{"creating an unaccepted project", map[string]any{"kind": "Project", "name": "payments", "spec": map[string]any{}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(tc.body)
			req := httptest.NewRequest(http.MethodPost, "/s1/resource", bytes.NewReader(body))
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", rec.Code)
			}
		})
	}
}

func TestReceiveSecretHandler_ScopeGate(t *testing.T) {
	h := &Handler{
		Sessions: NewSessionStore(),
		Client:   fake.NewSimpleClientset(projectNamespace("shop-test", "shop")),
		CRClient: migrationOwners(t),
	}
	h.Sessions.Put(&Session{ID: "s1", Projects: []string{"shop"}, Secret: "test-session-secret"})

	router := chi.NewRouter()
	router.Post("/{session}/secret", h.ReceiveSecretHandler)

	post := func(namespace string) int {
		body, _ := json.Marshal(map[string]any{
			"name":      "app-secrets",
			"namespace": namespace,
			"data":      map[string]string{"TOKEN": "dmFsdWU="}, //nolint:gosec // base64 of "value", a test fixture, not a credential
		})
		req := httptest.NewRequest(http.MethodPost, "/s1/secret", bytes.NewReader(body))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := post("payments-prod"); code != http.StatusForbidden {
		t.Fatalf("out-of-scope secret status = %d, want 403", code)
	}
	if code := post("shop-test"); code == http.StatusForbidden {
		t.Fatalf("in-scope secret was refused with 403")
	}
}

// A Secret's type is immutable in Kubernetes: replaying a transfer over an
// existing Secret of a different type must replace it, not fail an update
// or leave the stale type behind.
func TestReceiveSecretHandler_ReplacesSecretOnTypeChange(t *testing.T) {
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "site-tls", Namespace: "shop-test",
			Annotations: map[string]string{"cert-manager.io/issuer": "letsencrypt"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "kipper.run/v1alpha1", Kind: "App", Name: "web", UID: "uid-1",
			}},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"stale": []byte("x")},
	}
	h := &Handler{
		Sessions: NewSessionStore(),
		Client:   fake.NewSimpleClientset(projectNamespace("shop-test", "shop"), existing),
		CRClient: migrationOwners(t),
	}
	h.Sessions.Put(&Session{ID: "s1", Projects: []string{"shop"}, Secret: "test-session-secret"})

	router := chi.NewRouter()
	router.Post("/{session}/secret", h.ReceiveSecretHandler)

	body, _ := json.Marshal(map[string]any{
		"name":      "site-tls",
		"namespace": "shop-test",
		"type":      "kubernetes.io/tls",
		"data":      map[string]string{"tls.crt": "Y2VydA==", "tls.key": "a2V5"},
	})
	req := httptest.NewRequest(http.MethodPost, "/s1/secret", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	got, err := h.Client.CoreV1().Secrets("shop-test").Get(context.Background(), "site-tls", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting replaced secret: %v", err)
	}
	if got.Type != corev1.SecretTypeTLS {
		t.Fatalf("type = %s, want kubernetes.io/tls", got.Type)
	}
	if _, stale := got.Data["stale"]; stale {
		t.Fatal("replaced secret must not keep stale data")
	}
	if got.Annotations["cert-manager.io/issuer"] != "letsencrypt" {
		t.Fatal("replacement must carry the annotations forward")
	}
	if len(got.OwnerReferences) != 1 || got.OwnerReferences[0].Name != "web" {
		t.Fatal("replacement must carry the owner references forward")
	}
}

// A type change deletes then recreates the Secret; if the recreate fails,
// the original must be restored rather than left missing on the target.
func TestReceiveSecretHandler_RestoresOnFailedTypeChange(t *testing.T) {
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "site-tls", Namespace: "shop-test"},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"original": []byte("keep-me")},
	}
	client := fake.NewSimpleClientset(projectNamespace("shop-test", "shop"), existing)
	// Fail every create of the replacement so the recreate cannot succeed.
	client.PrependReactor("create", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		s := action.(k8stesting.CreateAction).GetObject().(*corev1.Secret)
		if s.Name == "site-tls" && s.Type == corev1.SecretTypeTLS {
			return true, nil, fmt.Errorf("simulated create failure")
		}
		return false, nil, nil
	})
	h := &Handler{Sessions: NewSessionStore(), Client: client, CRClient: migrationOwners(t)}
	h.Sessions.Put(&Session{ID: "s1", Projects: []string{"shop"}, Secret: "test-session-secret"})

	router := chi.NewRouter()
	router.Post("/{session}/secret", h.ReceiveSecretHandler)

	body, _ := json.Marshal(map[string]any{
		"name": "site-tls", "namespace": "shop-test", "type": "kubernetes.io/tls",
		"data": map[string]string{"tls.crt": "Y2VydA==", "tls.key": "a2V5"},
	})
	req := httptest.NewRequest(http.MethodPost, "/s1/secret", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 on a failed replace", rec.Code)
	}
	// The original Secret must be back, with its original type and data.
	got, err := h.Client.CoreV1().Secrets("shop-test").Get(context.Background(), "site-tls", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("original secret must be restored after a failed replace: %v", err)
	}
	if got.Type != corev1.SecretTypeOpaque || string(got.Data["original"]) != "keep-me" {
		t.Fatalf("restored secret must match the original: type=%s data=%v", got.Type, got.Data)
	}
}

// An aborted migration removes only the unadopted secrets it created:
// pre-existing secrets and adopted ones stay.
func TestAbortHandler_RemovesOnlyUnadoptedCreatedSecrets(t *testing.T) {
	preexisting := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "kept-existing", Namespace: "shop-test"},
		Data:       map[string][]byte{"k": []byte("old")},
	}
	h := &Handler{
		Sessions: NewSessionStore(),
		Client:   fake.NewSimpleClientset(projectNamespace("shop-test", "shop"), preexisting),
		CRClient: migrationOwners(t),
	}
	h.Sessions.Put(&Session{ID: "s1", Projects: []string{"shop"}, Secret: "test-session-secret"})

	router := chi.NewRouter()
	router.Post("/{session}/secret", h.ReceiveSecretHandler)
	router.Post("/{session}/abort", h.AbortHandler)

	send := func(name string) {
		body, _ := json.Marshal(map[string]any{
			"name": name, "namespace": "shop-test",
			"data": map[string]string{"k": "dg=="},
		})
		req := httptest.NewRequest(http.MethodPost, "/s1/secret", bytes.NewReader(body))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("sending %s: status = %d: %s", name, rec.Code, rec.Body.String())
		}
	}
	send("fresh-orphan")
	send("fresh-adopted")
	send("kept-existing")

	// The reconciler adopts one of the fresh secrets before the abort.
	adopted, err := h.Client.CoreV1().Secrets("shop-test").Get(context.Background(), "fresh-adopted", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting fresh-adopted: %v", err)
	}
	controller := true
	adopted.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "kipper.run/v1alpha1", Kind: "App", Name: "web", UID: "uid-1", Controller: &controller,
	}}
	if _, err := h.Client.CoreV1().Secrets("shop-test").Update(context.Background(), adopted, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("adopting fresh-adopted: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/s1/abort", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("abort status = %d: %s", rec.Code, rec.Body.String())
	}

	if _, err := h.Client.CoreV1().Secrets("shop-test").Get(context.Background(), "fresh-orphan", metav1.GetOptions{}); err == nil {
		t.Fatal("abort must remove the unadopted secret this session created")
	}
	if _, err := h.Client.CoreV1().Secrets("shop-test").Get(context.Background(), "fresh-adopted", metav1.GetOptions{}); err != nil {
		t.Fatal("abort must keep adopted secrets")
	}
	if _, err := h.Client.CoreV1().Secrets("shop-test").Get(context.Background(), "kept-existing", metav1.GetOptions{}); err != nil {
		t.Fatal("abort must keep secrets that predated the transfer")
	}
}
