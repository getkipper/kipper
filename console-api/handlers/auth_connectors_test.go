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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/yaml"
)

func dexConfigMapForConnectors(configYAML string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dex-config",
			Namespace: "dex",
		},
		Data: map[string]string{
			"config.yaml": configYAML,
		},
	}
}

func dexDeploymentForConnectors() *appsv1.Deployment {
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

const baseDexConfig = `issuer: https://dex.test/dex
connectors: []
staticPasswords: []
`

func TestAuthConnectors_List_Empty(t *testing.T) {
	client := fake.NewClientset(
		dexNamespace(),
		dexConfigMapForConnectors(baseDexConfig),
		dexDeploymentForConnectors(),
	)
	ac := &AuthConnectors{Client: client}

	r := chi.NewRouter()
	r.Get("/api/v1/settings/auth", ac.List)

	req := httptest.NewRequest("GET", "/api/v1/settings/auth", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var connectors []connectorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &connectors); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	assert.Len(t, connectors, 3)
	for _, c := range connectors {
		assert.False(t, c.Enabled, "connector %s should be disabled", c.Type)
	}
}

func TestAuthConnectors_List_WithGitHub(t *testing.T) {
	configWithGitHub := `issuer: https://dex.test/dex
connectors:
- type: github
  id: github
  name: GitHub
  config:
    clientID: gh-id
    clientSecret: gh-secret
    redirectURI: https://dex.test/dex/callback
staticPasswords: []
`
	client := fake.NewClientset(
		dexNamespace(),
		dexConfigMapForConnectors(configWithGitHub),
		dexDeploymentForConnectors(),
	)
	ac := &AuthConnectors{Client: client}

	r := chi.NewRouter()
	r.Get("/api/v1/settings/auth", ac.List)

	req := httptest.NewRequest("GET", "/api/v1/settings/auth", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var connectors []connectorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &connectors); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	assert.Len(t, connectors, 3)

	var github *connectorResponse
	for i := range connectors {
		if connectors[i].Type == "github" {
			github = &connectors[i]
			break
		}
	}
	assert.NotNil(t, github)
	assert.True(t, github.Enabled)
	assert.True(t, github.HasKeys)
}

func TestAuthConnectors_Update_EnableGitHub(t *testing.T) {
	client := fake.NewClientset(
		dexNamespace(),
		dexConfigMapForConnectors(baseDexConfig),
		dexDeploymentForConnectors(),
	)
	ac := &AuthConnectors{Client: client}

	r := chi.NewRouter()
	r.Put("/api/v1/settings/auth", ac.Update)

	body := `{"type":"github","client_id":"my-id","client_secret":"my-secret","enabled":true}`
	req := httptest.NewRequest("PUT", "/api/v1/settings/auth", strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	// Verify the Dex ConfigMap was updated with the GitHub connector
	cm, err := client.CoreV1().ConfigMaps("dex").Get(context.Background(), "dex-config", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to read Dex config: %v", err)
	}

	var config map[string]interface{}
	if err := yaml.Unmarshal([]byte(cm.Data["config.yaml"]), &config); err != nil {
		t.Fatalf("failed to parse Dex config: %v", err)
	}

	connectors, ok := config["connectors"].([]interface{})
	assert.True(t, ok, "connectors should be a list")
	assert.Len(t, connectors, 1)

	conn, ok := connectors[0].(map[string]interface{})
	assert.True(t, ok)
	assert.Equal(t, "github", conn["type"])

	connConfig, ok := conn["config"].(map[string]interface{})
	assert.True(t, ok)
	assert.Equal(t, "my-id", connConfig["clientID"])
	assert.Equal(t, "my-secret", connConfig["clientSecret"])
	assert.Equal(t, "https://dex.test/dex/callback", connConfig["redirectURI"])
}

func TestAuthConnectors_Update_DisableGitHub(t *testing.T) {
	configWithGitHub := `issuer: https://dex.test/dex
connectors:
- type: github
  id: github
  name: GitHub
  config:
    clientID: gh-id
    clientSecret: gh-secret
    redirectURI: https://dex.test/dex/callback
staticPasswords: []
`
	client := fake.NewClientset(
		dexNamespace(),
		dexConfigMapForConnectors(configWithGitHub),
		dexDeploymentForConnectors(),
	)
	ac := &AuthConnectors{Client: client}

	r := chi.NewRouter()
	r.Put("/api/v1/settings/auth", ac.Update)

	body := `{"type":"github","enabled":false}`
	req := httptest.NewRequest("PUT", "/api/v1/settings/auth", strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	// Verify the GitHub connector was removed
	cm, err := client.CoreV1().ConfigMaps("dex").Get(context.Background(), "dex-config", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to read Dex config: %v", err)
	}

	var config map[string]interface{}
	if err := yaml.Unmarshal([]byte(cm.Data["config.yaml"]), &config); err != nil {
		t.Fatalf("failed to parse Dex config: %v", err)
	}

	connectors, _ := config["connectors"].([]interface{})
	assert.Empty(t, connectors, "connectors should be empty after disabling")
}

func TestAuthConnectors_Update_InvalidType(t *testing.T) {
	client := fake.NewClientset(
		dexNamespace(),
		dexConfigMapForConnectors(baseDexConfig),
		dexDeploymentForConnectors(),
	)
	ac := &AuthConnectors{Client: client}

	r := chi.NewRouter()
	r.Put("/api/v1/settings/auth", ac.Update)

	body := `{"type":"bitbucket","client_id":"id","client_secret":"secret","enabled":true}`
	req := httptest.NewRequest("PUT", "/api/v1/settings/auth", strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)

	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}
	assert.Equal(t, "type must be github, gitlab, or google", resp["error"])
}
