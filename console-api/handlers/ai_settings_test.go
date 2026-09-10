package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestAISettingsGetRawWithFake(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      aiSecretName,
			Namespace: aiSecretNamespace,
		},
		Data: map[string][]byte{
			"provider":   []byte("claude"),
			"api_key":    []byte("sk-ant-test-key-12345"),
			"model":      []byte("claude-sonnet-4-5-20250514"),
			"ollama_url": []byte(""),
		},
	}

	client := fake.NewClientset(secret)
	ctx := context.Background()

	// Simulate what GetRaw does
	s, err := client.CoreV1().Secrets(aiSecretNamespace).Get(ctx, aiSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cfg := &aiConfig{
		Provider:  string(s.Data["provider"]),
		APIKey:    string(s.Data["api_key"]),
		Model:     string(s.Data["model"]),
		OllamaURL: string(s.Data["ollama_url"]),
	}

	if cfg.Provider != "claude" {
		t.Errorf("expected provider 'claude', got %q", cfg.Provider)
	}
	if cfg.APIKey != "sk-ant-test-key-12345" {
		t.Errorf("expected unmasked API key, got %q", cfg.APIKey)
	}
	if cfg.Model != "claude-sonnet-4-5-20250514" {
		t.Errorf("expected model 'claude-sonnet-4-5-20250514', got %q", cfg.Model)
	}
}

func TestAISettingsGetRawNotFound(t *testing.T) {
	client := fake.NewClientset()
	ctx := context.Background()

	_, err := client.CoreV1().Secrets(aiSecretNamespace).Get(ctx, aiSecretName, metav1.GetOptions{})
	if err == nil {
		t.Fatal("expected error for missing secret")
	}
}

func TestAISettingsAPIKeyMasking(t *testing.T) {
	tests := []struct {
		name     string
		key      string
		expected string
	}{
		{
			name:     "long key is partially shown",
			key:      "sk-ant-1234567890abcdef",
			expected: "sk-ant-1...cdef",
		},
		{
			name:     "short key is fully masked",
			key:      "short",
			expected: "••••••••",
		},
		{
			name:     "empty key stays empty",
			key:      "",
			expected: "",
		},
		{
			name:     "exactly 12 chars is fully masked",
			key:      "123456789012",
			expected: "••••••••",
		},
		{
			name:     "13 chars is partially shown",
			key:      "1234567890abc",
			expected: "12345678...0abc",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Replicates the masking logic from AISettings.Get
			var masked string
			if len(tt.key) > 12 {
				masked = tt.key[:8] + "..." + tt.key[len(tt.key)-4:]
			} else if len(tt.key) > 0 {
				masked = "••••••••"
			}

			if masked != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, masked)
			}
		})
	}
}

func TestAISettingsMaskedKeyDetection(t *testing.T) {
	tests := []struct {
		name     string
		key      string
		isMasked bool
	}{
		{
			name:     "dots mask pattern",
			key:      "sk-ant-1...cdef",
			isMasked: true,
		},
		{
			name:     "bullet mask pattern",
			key:      "••••••••",
			isMasked: true,
		},
		{
			name:     "real key",
			key:      "sk-ant-1234567890abcdef",
			isMasked: false,
		},
		{
			name:     "empty is not masked",
			key:      "",
			isMasked: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Replicates the detection logic from AISettings.Update
			isMasked := tt.key == "••••••••" || (len(tt.key) > 11 && tt.key[8:11] == "...")
			if isMasked != tt.isMasked {
				t.Errorf("expected isMasked=%v, got %v for key %q", tt.isMasked, isMasked, tt.key)
			}
		})
	}
}

func TestAISettingsSecretCRUD(t *testing.T) {
	client := fake.NewClientset()
	ctx := context.Background()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      aiSecretName,
			Namespace: aiSecretNamespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "kipper",
			},
		},
		Data: map[string][]byte{
			"provider":   []byte("claude"),
			"api_key":    []byte("sk-ant-test-key"),
			"model":      []byte("claude-sonnet-4-5-20250514"),
			"ollama_url": []byte(""),
		},
	}

	// Create
	_, err := client.CoreV1().Secrets(aiSecretNamespace).Create(ctx, secret, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("unexpected error creating secret: %v", err)
	}

	// Read
	stored, err := client.CoreV1().Secrets(aiSecretNamespace).Get(ctx, aiSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("unexpected error getting secret: %v", err)
	}
	if string(stored.Data["provider"]) != "claude" {
		t.Errorf("expected provider 'claude', got %q", string(stored.Data["provider"]))
	}

	// Update
	stored.Data["model"] = []byte("claude-opus-4-20250514")
	_, err = client.CoreV1().Secrets(aiSecretNamespace).Update(ctx, stored, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("unexpected error updating secret: %v", err)
	}

	updated, _ := client.CoreV1().Secrets(aiSecretNamespace).Get(ctx, aiSecretName, metav1.GetOptions{})
	if string(updated.Data["model"]) != "claude-opus-4-20250514" {
		t.Errorf("expected updated model, got %q", string(updated.Data["model"]))
	}
}

func TestHealthEndpoint(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/health", nil)

	Health(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp["status"] != "ok" {
		t.Errorf("expected status 'ok', got %q", resp["status"])
	}
}

// TestAISettingsUpdateSavesTwice pins the shape of the write. A handler that
// posts a freshly built Secret creates it the first time and is refused every
// time after, because an update carrying no resourceVersion is invalid. The
// reactor below makes the fake client refuse what an API server refuses.
func TestAISettingsUpdateSavesTwice(t *testing.T) {
	client := fake.NewClientset()
	enforceResourceVersions(client, "secrets")
	h := &AISettings{Client: client}

	save := func(model string) int {
		rec := httptest.NewRecorder()
		body := bytes.NewBufferString(`{"provider":"claude","api_key":"sk-ant-secret","model":"` + model + `","ollama_url":""}`)
		h.Update(rec, httptest.NewRequest(http.MethodPut, "/api/v1/settings/ai", body))
		return rec.Code
	}

	if code := save("claude-opus-5"); code != http.StatusOK {
		t.Fatalf("first save: got %d, want %d", code, http.StatusOK)
	}
	if code := save("claude-sonnet-5"); code != http.StatusOK {
		t.Fatalf("second save: got %d, want %d; a settings page that saves once is not a settings page", code, http.StatusOK)
	}

	secret, err := client.CoreV1().Secrets(aiSecretNamespace).Get(context.Background(), aiSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading the secret back: %v", err)
	}
	if got := string(secret.Data["model"]); got != "claude-sonnet-5" {
		t.Errorf("model = %q, want the second save to have landed", got)
	}
	if got := string(secret.Data["api_key"]); got != "sk-ant-secret" {
		t.Errorf("api_key = %q, want it carried through both saves", got)
	}
}

// TestAISettingsUpdateKeepsKeysItDoesNotOwn guards the read-modify-write: the
// Secret is fetched and mutated, so anything else stored alongside survives.
func TestAISettingsUpdateKeepsKeysItDoesNotOwn(t *testing.T) {
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: aiSecretName, Namespace: aiSecretNamespace, ResourceVersion: "9"},
		Data:       map[string][]byte{"provider": []byte("claude"), "somethingElse": []byte("left alone")},
	}
	client := fake.NewClientset(existing)
	h := &AISettings{Client: client}

	rec := httptest.NewRecorder()
	body := bytes.NewBufferString(`{"provider":"ollama","api_key":"","model":"llama3","ollama_url":"http://localhost:11434"}`)
	h.Update(rec, httptest.NewRequest(http.MethodPut, "/api/v1/settings/ai", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want %d", rec.Code, http.StatusOK)
	}

	secret, err := client.CoreV1().Secrets(aiSecretNamespace).Get(context.Background(), aiSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading the secret back: %v", err)
	}
	if got := string(secret.Data["provider"]); got != "ollama" {
		t.Errorf("provider = %q, want ollama", got)
	}
	if got := string(secret.Data["somethingElse"]); got != "left alone" {
		t.Errorf("somethingElse = %q, want it untouched", got)
	}
}
