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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	fakedynamic "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func getSettings(t *testing.T, handler *Functions, project, fn string) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	r.Get("/projects/{name}/functions/{fn}/settings", handler.GetSettings)
	req := httptest.NewRequest("GET", "/projects/"+project+"/functions/"+fn+"/settings", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func putSettings(t *testing.T, handler *Functions, project, fn, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	r.Put("/projects/{name}/functions/{fn}/settings", handler.UpdateSettings)
	req := httptest.NewRequest("PUT", "/projects/"+project+"/functions/"+fn+"/settings", strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func testFunctionsHandler(objs ...crclient.Object) *Functions {
	scheme := runtime.NewScheme()
	_ = kipperv1.AddToScheme(scheme)
	dynClient := fakedynamic.NewSimpleDynamicClient(scheme)
	return &Functions{
		Client:   fake.NewClientset(),
		Dynamic:  dynClient,
		CRClient: testCRClient(objs...),
	}
}

func TestGetSettings_DefaultValues(t *testing.T) {
	fn := &kipperv1.Function{
		ObjectMeta: metav1.ObjectMeta{Name: "hello", Namespace: "staging"},
		Spec:       kipperv1.FunctionSpec{},
	}
	handler := testFunctionsHandler(fn)

	rec := getSettings(t, handler, "staging", "hello")
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp functionSettings
	err := json.Unmarshal(rec.Body.Bytes(), &resp)
	assert.NoError(t, err)
	assert.True(t, resp.SecurityHeaders)
	assert.Equal(t, []string{}, resp.CSPAllowlist)
}

func TestGetSettings_ConfiguredValues(t *testing.T) {
	fn := &kipperv1.Function{
		ObjectMeta: metav1.ObjectMeta{Name: "widget", Namespace: "production"},
		Spec: kipperv1.FunctionSpec{
			NoSecurityHeaders: true,
			CSPAllowlist:      []string{"https://cdn.example.com", "https://api.example.com"},
		},
	}
	handler := testFunctionsHandler(fn)

	rec := getSettings(t, handler, "production", "widget")
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp functionSettings
	err := json.Unmarshal(rec.Body.Bytes(), &resp)
	assert.NoError(t, err)
	assert.False(t, resp.SecurityHeaders)
	assert.Equal(t, []string{"https://cdn.example.com", "https://api.example.com"}, resp.CSPAllowlist)
}

func TestGetSettings_FunctionNotFound(t *testing.T) {
	handler := testFunctionsHandler()

	rec := getSettings(t, handler, "staging", "nonexistent")
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp functionSettings
	err := json.Unmarshal(rec.Body.Bytes(), &resp)
	assert.NoError(t, err)
	assert.True(t, resp.SecurityHeaders)
	assert.Equal(t, []string{}, resp.CSPAllowlist)
}

func TestUpdateSettings_SetsValues(t *testing.T) {
	fn := &kipperv1.Function{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "hello",
			Namespace:       "staging",
			ResourceVersion: "1",
		},
		Spec: kipperv1.FunctionSpec{},
	}
	handler := testFunctionsHandler(fn)

	body := `{"security_headers":false,"csp_allowlist":["https://fonts.googleapis.com"]}`
	rec := putSettings(t, handler, "staging", "hello", body)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp functionSettings
	err := json.Unmarshal(rec.Body.Bytes(), &resp)
	assert.NoError(t, err)
	assert.False(t, resp.SecurityHeaders)
	assert.Equal(t, []string{"https://fonts.googleapis.com"}, resp.CSPAllowlist)

	var updated kipperv1.Function
	err = handler.CRClient.Get(context.Background(), crclient.ObjectKey{Namespace: "staging", Name: "hello"}, &updated)
	assert.NoError(t, err)
	assert.True(t, updated.Spec.NoSecurityHeaders)
	assert.Equal(t, []string{"https://fonts.googleapis.com"}, updated.Spec.CSPAllowlist)
}

func TestUpdateSettings_FunctionNotFound(t *testing.T) {
	handler := testFunctionsHandler()

	body := `{"security_headers":true,"csp_allowlist":[]}`
	rec := putSettings(t, handler, "staging", "nonexistent", body)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}
