package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/controllers"
)

func postLink(t *testing.T, handler *Apps, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	r.Post("/api/v1/link", handler.Link)
	req := httptest.NewRequest("POST", "/api/v1/link", strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func postUnlink(t *testing.T, handler *Apps, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	r.Post("/api/v1/unlink", handler.Unlink)
	req := httptest.NewRequest("POST", "/api/v1/unlink", strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestLink_InjectsAppURL(t *testing.T) {
	targetApp := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "domain-service", Namespace: "default"},
		Spec:       kipperv1.AppSpec{Image: "domain:v1", Port: 8081, Replicas: int32Ptr(1)},
	}
	callerApp := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "api-gateway", Namespace: "default"},
		Spec:       kipperv1.AppSpec{Image: "gateway:v1", Port: 8080, Replicas: int32Ptr(1)},
	}
	crClient := testCRClient(targetApp, callerApp)
	handler := &Apps{Client: fake.NewClientset(), CRClient: crClient}

	rec := postLink(t, handler, `{"target":"domain-service","app":"api-gateway","namespace":"default"}`)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp linkResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	assert.Equal(t, "DOMAIN_SERVICE_URL", resp.EnvVar)
	assert.Equal(t, "http://domain-service.default.svc.cluster.local:8081", resp.URL,
		"the caller is told the address its app will see")

	var updatedApp kipperv1.App
	err := crClient.Get(context.Background(), crclient.ObjectKey{Namespace: "default", Name: "api-gateway"}, &updatedApp)
	if err != nil {
		t.Fatalf("expected app CR to exist: %v", err)
	}
	// The link is recorded; the address is not. The reconciler derives it from
	// this entry on every pass, so a target that moves takes its callers with
	// it instead of leaving them on an address that was true once.
	assert.Equal(t, []kipperv1.AppLink{{App: "domain-service", Namespace: "default"}}, updatedApp.Spec.Links)
	assert.NotContains(t, updatedApp.Spec.Env, "DOMAIN_SERVICE_URL",
		"a stored address is a second copy of something already derived, and only one of them can be right")
}

func TestLink_PreservesExistingEnv(t *testing.T) {
	targetApp := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "dns-service", Namespace: "default"},
		Spec:       kipperv1.AppSpec{Image: "dns:v1", Port: 8082, Replicas: int32Ptr(1)},
	}
	callerApp := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: kipperv1.AppSpec{
			Image: "api:v1", Port: 8080, Replicas: int32Ptr(1),
			Env: map[string]string{"LOG_LEVEL": "debug"},
		},
	}
	crClient := testCRClient(targetApp, callerApp)
	handler := &Apps{Client: fake.NewClientset(), CRClient: crClient}

	rec := postLink(t, handler, `{"target":"dns-service","app":"api","namespace":"default"}`)
	assert.Equal(t, http.StatusOK, rec.Code)

	var updatedApp kipperv1.App
	_ = crClient.Get(context.Background(), crclient.ObjectKey{Namespace: "default", Name: "api"}, &updatedApp)
	assert.Equal(t, "debug", updatedApp.Spec.Env["LOG_LEVEL"], "what the operator typed is left alone")
	assert.NotContains(t, updatedApp.Spec.Env, "DNS_SERVICE_URL")
	assert.Equal(t, []kipperv1.AppLink{{App: "dns-service", Namespace: "default"}}, updatedApp.Spec.Links)
}

func TestLink_TargetNotFound(t *testing.T) {
	callerApp := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec:       kipperv1.AppSpec{Image: "api:v1", Port: 8080, Replicas: int32Ptr(1)},
	}
	crClient := testCRClient(callerApp)
	handler := &Apps{Client: fake.NewClientset(), CRClient: crClient}

	rec := postLink(t, handler, `{"target":"nonexistent","app":"api","namespace":"default"}`)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestLink_CannotLinkToSelf(t *testing.T) {
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec:       kipperv1.AppSpec{Image: "api:v1", Port: 8080, Replicas: int32Ptr(1)},
	}
	crClient := testCRClient(app)
	handler := &Apps{Client: fake.NewClientset(), CRClient: crClient}

	rec := postLink(t, handler, `{"target":"api","app":"api","namespace":"default"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestUnlink_RemovesEnvVar(t *testing.T) {
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: kipperv1.AppSpec{
			Image: "api:v1", Port: 8080, Replicas: int32Ptr(1),
			Env: map[string]string{
				"DOMAIN_SERVICE_URL": "http://domain-service.default.svc.cluster.local:8081",
				"LOG_LEVEL":          "debug",
			},
		},
	}
	crClient := testCRClient(app)
	handler := &Apps{Client: fake.NewClientset(), CRClient: crClient}

	rec := postUnlink(t, handler, `{"target":"domain-service","app":"api","namespace":"default"}`)
	assert.Equal(t, http.StatusOK, rec.Code)

	var updatedApp kipperv1.App
	_ = crClient.Get(context.Background(), crclient.ObjectKey{Namespace: "default", Name: "api"}, &updatedApp)
	assert.Empty(t, updatedApp.Spec.Env["DOMAIN_SERVICE_URL"])
	assert.Equal(t, "debug", updatedApp.Spec.Env["LOG_LEVEL"])
}

func TestAppEnvKey(t *testing.T) {
	tests := []struct {
		name     string
		expected string
	}{
		{"domain-service", "DOMAIN_SERVICE_URL"},
		{"dns-service", "DNS_SERVICE_URL"},
		{"payments", "PAYMENTS_URL"},
		{"my-cool-thing", "MY_COOL_THING_URL"},
		{"api", "API_URL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, controllers.AppEnvKey(tt.name))
		})
	}
}

// The console derives the links it shows from the *_URL variables, so an unlink
// that drops only the variable takes the link off the screen while leaving the
// dependency in the spec — and the reconciler goes on rebuilding the egress it
// opened, on every pass and every sweep, with neither surface admitting it is
// there. A cross-project link is written by the CLI and unlinked here, so the
// entry to remove need not name this app's own namespace.
func TestUnlink_RemovesTheDeclaredLinkNotJustItsURL(t *testing.T) {
	caller := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "hrportal-backend", Namespace: "hrportal-test"},
		Spec: kipperv1.AppSpec{
			Image: "hrportal:v1", Port: 8080, Replicas: int32Ptr(1),
			Env: map[string]string{
				"DOCUSEAL_URL": "http://docuseal.docuseal-test.svc.cluster.local:3000",
				"LOG_LEVEL":    "debug",
			},
			Links: []kipperv1.AppLink{
				{App: "docuseal", Namespace: "docuseal-test"},
				{App: "billing", Namespace: "billing-test"},
			},
		},
	}
	crClient := testCRClient(caller)
	handler := &Apps{Client: fake.NewClientset(), CRClient: crClient}

	rec := postUnlink(t, handler, `{"target":"docuseal","app":"hrportal-backend","namespace":"hrportal-test"}`)
	assert.Equal(t, http.StatusOK, rec.Code)

	var updated kipperv1.App
	if err := crClient.Get(context.Background(),
		crclient.ObjectKey{Namespace: "hrportal-test", Name: "hrportal-backend"}, &updated); err != nil {
		t.Fatalf("expected app CR to exist: %v", err)
	}
	assert.NotContains(t, updated.Spec.Env, "DOCUSEAL_URL")
	assert.Equal(t, "debug", updated.Spec.Env["LOG_LEVEL"], "unrelated env must survive")
	assert.Equal(t, []kipperv1.AppLink{{App: "billing", Namespace: "billing-test"}}, updated.Spec.Links,
		"the unlinked dependency goes and the others stay")
}

// Linking records the dependency, so the two surfaces build the same state for
// the same concept and either can withdraw what the other opened.
func TestLink_RecordsTheDependencyItInjects(t *testing.T) {
	target := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "domain-service", Namespace: "default"},
		Spec:       kipperv1.AppSpec{Image: "domain:v1", Port: 8081, Replicas: int32Ptr(1)},
	}
	caller := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "api-gateway", Namespace: "default"},
		Spec:       kipperv1.AppSpec{Image: "gateway:v1", Port: 8080, Replicas: int32Ptr(1)},
	}
	crClient := testCRClient(target, caller)
	handler := &Apps{Client: fake.NewClientset(), CRClient: crClient}

	assert.Equal(t, http.StatusOK,
		postLink(t, handler, `{"target":"domain-service","app":"api-gateway","namespace":"default"}`).Code)

	var updated kipperv1.App
	if err := crClient.Get(context.Background(),
		crclient.ObjectKey{Namespace: "default", Name: "api-gateway"}, &updated); err != nil {
		t.Fatalf("expected app CR to exist: %v", err)
	}
	assert.Equal(t, []kipperv1.AppLink{{App: "domain-service", Namespace: "default"}}, updated.Spec.Links)
}

// A variable the operator set themselves is theirs. Linking would take the name
// — the derived value is an explicit container env entry and wins over the
// Secret — so the link is refused rather than quietly destroying or shadowing a
// value somebody chose.
func TestLinkingRefusesAVariableTheOperatorAlreadySet(t *testing.T) {
	target := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "domain-service", Namespace: "default"},
		Spec:       kipperv1.AppSpec{Image: "domain:v1", Port: 8081, Replicas: int32Ptr(1)},
	}
	caller := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "api-gateway", Namespace: "default"},
		Spec: kipperv1.AppSpec{
			Image: "gateway:v1", Port: 8080, Replicas: int32Ptr(1),
			Env: map[string]string{"DOMAIN_SERVICE_URL": "https://proxy.internal/domain"},
		},
	}
	crClient := testCRClient(target, caller)
	handler := &Apps{Client: fake.NewClientset(), CRClient: crClient}

	rec := postLink(t, handler, `{"target":"domain-service","app":"api-gateway","namespace":"default"}`)
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), "DOMAIN_SERVICE_URL")

	var after kipperv1.App
	require.NoError(t, crClient.Get(context.Background(),
		crclient.ObjectKey{Namespace: "default", Name: "api-gateway"}, &after))
	assert.Equal(t, "https://proxy.internal/domain", after.Spec.Env["DOMAIN_SERVICE_URL"],
		"the operator's value survives a refused link")
	assert.Empty(t, after.Spec.Links, "and no link was recorded")
}
