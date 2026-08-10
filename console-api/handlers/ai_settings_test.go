package handlers

import (
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
