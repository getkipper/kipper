package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"

	"github.com/getkipper/kipper/controller/pkg/dexsecrets"
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
	assert.True(t, strings.HasPrefix(connConfig["clientSecret"].(string), "$KIPPER_GITHUB_CLIENT_SECRET_"), "Dex expands the reference from its environment")
	assert.NotContains(t, cm.Data["config.yaml"], "my-secret")
	assert.Equal(t, "https://dex.test/dex/callback", connConfig["redirectURI"])

	assert.Equal(t, "my-secret", connectorSecretValue(t, client, "github"))
	assertDexReadsConnectorSecrets(t, client)
}

// connectorSecretValue is the value Dex finds for the connector's client
// secret: the Secret entry its config refers to.
func connectorSecretValue(t *testing.T, client *fake.Clientset, id string) string {
	t.Helper()
	cm, err := client.CoreV1().ConfigMaps("dex").Get(context.Background(), "dex-config", metav1.GetOptions{})
	require.NoError(t, err)
	var config map[string]interface{}
	require.NoError(t, yaml.Unmarshal([]byte(cm.Data["config.yaml"]), &config))
	connectors, _ := config["connectors"].([]interface{})
	for _, c := range connectors {
		conn, _ := c.(map[string]interface{})
		if conn["id"] != id {
			continue
		}
		ref, _ := conn["config"].(map[string]interface{})["clientSecret"].(string)
		require.True(t, strings.HasPrefix(ref, "$"), "the config holds a reference, not the secret")
		secret, err := client.CoreV1().Secrets("dex").Get(context.Background(), dexsecrets.SecretName, metav1.GetOptions{})
		require.NoError(t, err)
		value, ok := secret.Data[strings.TrimPrefix(ref, "$")]
		require.True(t, ok, "the Secret lacks %s", ref)
		return string(value)
	}
	t.Fatalf("no connector %s in the Dex config", id)
	return ""
}

// A save that fails after the new secret is stored must leave Dex with the
// client ID and secret that belong together.
func TestAuthConnectors_Update_AFailedSaveKeepsTheWorkingPair(t *testing.T) {
	client := fake.NewClientset(dexNamespace(), dexConfigMapForConnectors(baseDexConfig), dexDeploymentForConnectors())
	ac := &AuthConnectors{Client: client}
	r := chi.NewRouter()
	r.Put("/api/v1/settings/auth", ac.Update)
	put := func(body string) int {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest("PUT", "/api/v1/settings/auth", strings.NewReader(body)))
		return rec.Code
	}
	require.Equal(t, http.StatusOK, put(`{"type":"github","client_id":"first-id","client_secret":"first-secret","enabled":true}`))

	client.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("conflict")
	})
	assert.Equal(t, http.StatusInternalServerError, put(`{"type":"github","client_id":"second-id","client_secret":"second-secret","enabled":true}`))

	assert.Equal(t, "first-secret", connectorSecretValue(t, client, "github"))
}

// Two admins saving connectors at once both write the connector Secret, and
// the one that loses the race reads it again instead of failing.
func TestAuthConnectors_Update_RetriesASecretWriteThatLostARace(t *testing.T) {
	client := fake.NewClientset(dexNamespace(), dexConfigMapForConnectors(baseDexConfig), dexDeploymentForConnectors())
	ac := &AuthConnectors{Client: client}
	r := chi.NewRouter()
	r.Put("/api/v1/settings/auth", ac.Update)
	put := func(body string) int {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest("PUT", "/api/v1/settings/auth", strings.NewReader(body)))
		return rec.Code
	}
	require.Equal(t, http.StatusOK, put(`{"type":"github","client_id":"first-id","client_secret":"first-secret","enabled":true}`))

	conflicts := 1
	client.PrependReactor("update", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		if conflicts == 0 {
			return false, nil, nil
		}
		conflicts--
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, dexsecrets.SecretName, errors.New("modified"))
	})
	require.Equal(t, http.StatusOK, put(`{"type":"gitlab","client_id":"second-id","client_secret":"second-secret","enabled":true}`))

	assert.Equal(t, "second-secret", connectorSecretValue(t, client, "gitlab"))
	assert.Equal(t, "first-secret", connectorSecretValue(t, client, "github"))
}

func assertDexReadsConnectorSecrets(t *testing.T, client *fake.Clientset) {
	t.Helper()
	deploy, err := client.AppsV1().Deployments("dex").Get(context.Background(), "dex", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to read the Dex deployment: %v", err)
	}
	envFrom := deploy.Spec.Template.Spec.Containers[0].EnvFrom
	if assert.Len(t, envFrom, 1) && assert.NotNil(t, envFrom[0].SecretRef) {
		assert.Equal(t, dexsecrets.SecretName, envFrom[0].SecretRef.Name)
	}
}

// Dex expands the reference inside the connector's JSON, so a value that
// JSON would need to escape cannot pass through it intact.
func TestAuthConnectors_Update_RefusesASecretDexCannotExpand(t *testing.T) {
	for _, secret := range []string{`a"b`, `a\b`, "a\nb"} {
		client := fake.NewClientset(dexNamespace(), dexConfigMapForConnectors(baseDexConfig), dexDeploymentForConnectors())
		ac := &AuthConnectors{Client: client}
		r := chi.NewRouter()
		r.Put("/api/v1/settings/auth", ac.Update)

		body, err := json.Marshal(connectorConfig{Type: "github", ClientID: "my-id", ClientSecret: secret, Enabled: true}) //nolint:gosec // G117: the request body carries a test secret
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest("PUT", "/api/v1/settings/auth", strings.NewReader(string(body))))

		assert.Equal(t, http.StatusBadRequest, rec.Code, "secret %q: %s", secret, rec.Body.String())
	}
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

func TestAuthConnectors_Update_DisablingThenEnablingUsesTheNewSecret(t *testing.T) {
	client := fake.NewClientset(dexNamespace(), dexConfigMapForConnectors(baseDexConfig), dexDeploymentForConnectors())
	ac := &AuthConnectors{Client: client}
	r := chi.NewRouter()
	r.Put("/api/v1/settings/auth", ac.Update)
	put := func(body string) {
		t.Helper()
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest("PUT", "/api/v1/settings/auth", strings.NewReader(body)))
		assert.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	}

	put(`{"type":"github","client_id":"my-id","client_secret":"first-secret","enabled":true}`)
	put(`{"type":"github","enabled":false}`)
	cm, err := client.CoreV1().ConfigMaps("dex").Get(context.Background(), "dex-config", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to read dex-config: %v", err)
	}
	assert.NotContains(t, cm.Data["config.yaml"], "KIPPER_GITHUB_CLIENT_SECRET", "a disabled connector is gone from the config")

	put(`{"type":"github","client_id":"my-id","client_secret":"second-secret","enabled":true}`)
	assert.Equal(t, "second-secret", connectorSecretValue(t, client, "github"))
}

func TestMoveDexConnectorSecretsTakesInlineSecretsOutOfTheConfigMap(t *testing.T) {
	inline := `issuer: https://dex.test/dex
connectors:
- type: github
  id: github
  name: GitHub
  config:
    clientID: gh-id
    clientSecret: gh-secret
    redirectURI: https://dex.test/dex/callback
- type: oidc
  id: corp-sso
  name: Corp
  config:
    clientID: corp-id
    clientSecret: corp-secret
staticPasswords: []
`
	client := fake.NewClientset(dexNamespace(), dexConfigMapForConnectors(inline), dexDeploymentForConnectors())

	if err := MoveDexConnectorSecrets(context.Background(), client); err != nil {
		t.Fatalf("moving connector secrets: %v", err)
	}

	cm, _ := client.CoreV1().ConfigMaps("dex").Get(context.Background(), "dex-config", metav1.GetOptions{})
	assert.NotContains(t, cm.Data["config.yaml"], "gh-secret")
	assert.NotContains(t, cm.Data["config.yaml"], "corp-secret")
	assert.Contains(t, cm.Data["config.yaml"], "$KIPPER_GITHUB_CLIENT_SECRET")
	assert.Contains(t, cm.Data["config.yaml"], "$KIPPER_CORP_SSO_CLIENT_SECRET")
	assert.Equal(t, "gh-secret", connectorSecretValue(t, client, "github"))
	assert.Equal(t, "corp-secret", connectorSecretValue(t, client, "corp-sso"))
	assertDexReadsConnectorSecrets(t, client)
	deploy, _ := client.AppsV1().Deployments("dex").Get(context.Background(), "dex", metav1.GetOptions{})
	restarted := deploy.Spec.Template.Annotations["kipper.run/restartedAt"]
	assert.NotEmpty(t, restarted, "Dex reads its config only when it starts")

	if err := MoveDexConnectorSecrets(context.Background(), client); err != nil {
		t.Fatalf("second run: %v", err)
	}
	deploy, _ = client.AppsV1().Deployments("dex").Get(context.Background(), "dex", metav1.GetOptions{})
	assert.Equal(t, restarted, deploy.Spec.Template.Annotations["kipper.run/restartedAt"], "a second run has nothing to move and leaves Dex running")
}

func TestMoveDexConnectorSecretsLeavesAClusterWithoutConnectorsAlone(t *testing.T) {
	client := fake.NewClientset(dexNamespace(), dexConfigMapForConnectors(baseDexConfig), dexDeploymentForConnectors())

	if err := MoveDexConnectorSecrets(context.Background(), client); err != nil {
		t.Fatalf("moving connector secrets: %v", err)
	}

	_, err := client.CoreV1().Secrets("dex").Get(context.Background(), dexsecrets.SecretName, metav1.GetOptions{})
	assert.Error(t, err, "nothing to move creates no Secret")
	deploy, _ := client.AppsV1().Deployments("dex").Get(context.Background(), "dex", metav1.GetOptions{})
	assert.Empty(t, deploy.Spec.Template.Annotations["kipper.run/restartedAt"])
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

// raceDexConfig enforces resource versions on dex-config and adds a static-password
// user between the caller's first read and write to simulate an accepted invite.
func raceDexConfig(t *testing.T, client *fake.Clientset) {
	t.Helper()
	gvr := corev1.SchemeGroupVersion.WithResource("configmaps")
	version := 1
	reads := 0
	client.PrependReactor("get", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		reads++
		obj, err := client.Tracker().Get(gvr, "dex", "dex-config")
		if err != nil {
			return true, nil, err
		}
		stale := obj.(*corev1.ConfigMap).DeepCopy()
		stale.ResourceVersion = fmt.Sprint(version)
		if reads == 1 {
			fresh := stale.DeepCopy()
			fresh.Data["config.yaml"] += "- email: invited@example.com\n  username: invited\n"
			fresh.Data["config.yaml"] = strings.Replace(fresh.Data["config.yaml"], "staticPasswords: []\n", "staticPasswords:\n", 1)
			version++
			fresh.ResourceVersion = fmt.Sprint(version)
			require.NoError(t, client.Tracker().Update(gvr, fresh, "dex"))
		}
		return true, stale, nil
	})
	client.PrependReactor("update", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		cm := action.(k8stesting.UpdateAction).GetObject().(*corev1.ConfigMap).DeepCopy()
		if cm.ResourceVersion != fmt.Sprint(version) {
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, cm.Name, errors.New("modified"))
		}
		version++
		cm.ResourceVersion = fmt.Sprint(version)
		return true, cm, client.Tracker().Update(gvr, cm, "dex")
	})
}

func dexConfigText(t *testing.T, client *fake.Clientset) string {
	t.Helper()
	cm, err := client.CoreV1().ConfigMaps("dex").Get(context.Background(), "dex-config", metav1.GetOptions{})
	require.NoError(t, err)
	return cm.Data["config.yaml"]
}

func TestMoveDexConnectorSecretsKeepsAUserAddedWhileItRan(t *testing.T) {
	inline := `issuer: https://dex.test/dex
connectors:
- type: github
  id: github
  name: GitHub
  config:
    clientID: gh-id
    clientSecret: gh-secret
staticPasswords: []
`
	client := fake.NewClientset(dexNamespace(), dexConfigMapForConnectors(inline), dexDeploymentForConnectors())
	raceDexConfig(t, client)

	require.NoError(t, MoveDexConnectorSecrets(context.Background(), client))

	config := dexConfigText(t, client)
	assert.Contains(t, config, "invited@example.com")
	assert.NotContains(t, config, "gh-secret")
	var parsed struct {
		Connectors []struct {
			Config struct {
				ClientSecret string `json:"clientSecret"`
			} `json:"config"`
		} `json:"connectors"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(config), &parsed))
	require.Len(t, parsed.Connectors, 1)
	assert.Equal(t, "gh-secret", connectorSecretValue(t, client, "github"))
}

func TestAuthConnectors_Update_KeepsAUserAddedWhileItRan(t *testing.T) {
	client := fake.NewClientset(dexNamespace(), dexConfigMapForConnectors(baseDexConfig), dexDeploymentForConnectors())
	raceDexConfig(t, client)
	ac := &AuthConnectors{Client: client}
	r := chi.NewRouter()
	r.Put("/api/v1/settings/auth", ac.Update)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("PUT", "/api/v1/settings/auth", strings.NewReader(`{"type":"github","client_id":"gh-id","client_secret":"gh-secret","enabled":true}`)))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	config := dexConfigText(t, client)
	assert.Contains(t, config, "invited@example.com")
	assert.Contains(t, config, "gh-id")
	assert.Equal(t, "gh-secret", connectorSecretValue(t, client, "github"))
}

// A disabled connector's client secret is removed after the grace period
// measured from its first unused mark. An enabled connector keeps its secret.
func TestRetireConnectorSecretsRemovesADisabledConnectorsSecret(t *testing.T) {
	client := fake.NewClientset(dexNamespace(), dexConfigMapForConnectors(baseDexConfig), dexDeploymentForConnectors())
	ac := &AuthConnectors{Client: client}
	r := chi.NewRouter()
	r.Put("/api/v1/settings/auth", ac.Update)
	put := func(body string) {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest("PUT", "/api/v1/settings/auth", strings.NewReader(body)))
		require.Equal(t, http.StatusOK, rec.Code)
	}
	put(`{"type":"github","client_id":"gh-id","client_secret":"gh-secret","enabled":true}`)
	put(`{"type":"gitlab","client_id":"gl-id","client_secret":"gl-secret","enabled":true}`)
	put(`{"type":"github","enabled":false}`)

	start := time.Now()
	var passes atomic.Int64
	clock := func() time.Time { return start.Add(time.Duration(passes.Add(1)) * dexsecrets.UnusedGrace) }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { RetireConnectorSecrets(ctx, client, time.Millisecond, clock); close(done) }()
	require.Eventually(t, func() bool {
		secret, err := client.CoreV1().Secrets("dex").Get(context.Background(), dexsecrets.SecretName, metav1.GetOptions{})
		return err == nil && len(secret.Data) == 1
	}, 5*time.Second, 5*time.Millisecond)
	cancel()
	<-done

	assert.Equal(t, "gl-secret", connectorSecretValue(t, client, "gitlab"))
}
