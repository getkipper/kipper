package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

const (
	aiSecretName      = "kipper-ai-config" //nolint:gosec // k8s Secret object name, not a credential value
	aiSecretNamespace = "kipper-system"
)

// AISettings handles cluster-wide AI configuration.
type AISettings struct {
	Client kubernetes.Interface
}

type aiConfig struct {
	Provider  string `json:"provider"`
	APIKey    string `json:"api_key"`
	Model     string `json:"model"`
	OllamaURL string `json:"ollama_url"`
}

type aiConfigResponse struct {
	Provider  string `json:"provider"`
	APIKey    string `json:"api_key"`
	Model     string `json:"model"`
	OllamaURL string `json:"ollama_url"`
}

// Get returns the current AI configuration with the API key masked.
// GET /api/v1/settings/ai
func (a *AISettings) Get(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	secret, err := a.Client.CoreV1().Secrets(aiSecretNamespace).Get(ctx, aiSecretName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			respondJSON(w, http.StatusOK, aiConfigResponse{})
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to read AI config")
		return
	}

	resp := aiConfigResponse{
		Provider:  string(secret.Data["provider"]),
		Model:     string(secret.Data["model"]),
		OllamaURL: string(secret.Data["ollama_url"]),
	}

	// Mask the API key — show first 8 and last 4 chars
	key := string(secret.Data["api_key"])
	if len(key) > 12 {
		resp.APIKey = key[:8] + "..." + key[len(key)-4:]
	} else if len(key) > 0 {
		resp.APIKey = "••••••••"
	}

	respondJSON(w, http.StatusOK, resp)
}

// Update saves the AI configuration.
// PUT /api/v1/settings/ai
func (a *AISettings) Update(w http.ResponseWriter, r *http.Request) {
	var req aiConfig
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	// If the API key looks masked, preserve the existing key
	if req.APIKey != "" && (req.APIKey == "••••••••" || len(req.APIKey) > 8 && req.APIKey[8:11] == "...") {
		existing, err := a.Client.CoreV1().Secrets(aiSecretNamespace).Get(ctx, aiSecretName, metav1.GetOptions{})
		if err == nil {
			req.APIKey = string(existing.Data["api_key"])
		}
	}

	if err := a.save(ctx, req); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to save AI config")
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

// save writes the settings onto the Secret that is there rather than posting a
// replacement, because Kubernetes refuses an update carrying no
// resourceVersion: building a fresh object means the first save creates it and
// every save after that fails. Two admins saving at once meet either a conflict
// or each other's create, and both are retried by reading back what won.
func (a *AISettings) save(ctx context.Context, cfg aiConfig) error {
	secrets := a.Client.CoreV1().Secrets(aiSecretNamespace)
	fields := map[string][]byte{
		"provider":   []byte(cfg.Provider),
		"api_key":    []byte(cfg.APIKey),
		"model":      []byte(cfg.Model),
		"ollama_url": []byte(cfg.OllamaURL),
	}
	retriable := func(err error) bool { return errors.IsConflict(err) || errors.IsAlreadyExists(err) }

	return retry.OnError(retry.DefaultRetry, retriable, func() error {
		existing, err := secrets.Get(ctx, aiSecretName, metav1.GetOptions{})
		if errors.IsNotFound(err) {
			_, err = secrets.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      aiSecretName,
					Namespace: aiSecretNamespace,
					Labels: map[string]string{
						"app.kubernetes.io/managed-by": "kipper",
					},
				},
				Data: fields,
			}, metav1.CreateOptions{})
			return err
		}
		if err != nil {
			return err
		}

		if existing.Data == nil {
			existing.Data = map[string][]byte{}
		}
		for name, value := range fields {
			existing.Data[name] = value
		}
		_, err = secrets.Update(ctx, existing, metav1.UpdateOptions{})
		return err
	})
}

// GetRaw returns the unmasked AI config for internal use by the chat handler.
func (a *AISettings) GetRaw(ctx context.Context) (*aiConfig, error) {
	secret, err := a.Client.CoreV1().Secrets(aiSecretNamespace).Get(ctx, aiSecretName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	return &aiConfig{
		Provider:  string(secret.Data["provider"]),
		APIKey:    string(secret.Data["api_key"]),
		Model:     string(secret.Data["model"]),
		OllamaURL: string(secret.Data["ollama_url"]),
	}, nil
}
