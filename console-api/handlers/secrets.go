package handlers

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/secretname"
)

// Secrets provides handlers for managing application secrets.
type Secrets struct {
	Client kubernetes.Interface
}

// ListKeys returns only the secret key names (values masked).
// GET /api/v1/projects/{name}/apps/{app}/secrets
func (s *Secrets) ListKeys(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	app := chi.URLParam(r, "app")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	secret, err := s.Client.CoreV1().Secrets(project).Get(ctx, secretname.Secrets(secretname.KindApp, app), metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			respondJSON(w, http.StatusOK, []string{})
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to get secrets")
		return
	}

	type secretKeyInfo struct {
		Key         string `json:"key"`
		HasPrevious bool   `json:"has_previous"`
	}

	keys := make([]secretKeyInfo, 0)
	for k := range secret.Data {
		if strings.HasSuffix(k, ".__previous") {
			continue
		}
		_, hasPrev := secret.Data[k+".__previous"]
		keys = append(keys, secretKeyInfo{Key: k, HasPrevious: hasPrev})
	}

	respondJSON(w, http.StatusOK, keys)
}

// Reveal returns the value of a single secret.
// GET /api/v1/projects/{name}/apps/{app}/secrets/{key}
func (s *Secrets) Reveal(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	app := chi.URLParam(r, "app")
	key := chi.URLParam(r, "key")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	secret, err := s.Client.CoreV1().Secrets(project).Get(ctx, secretname.Secrets(secretname.KindApp, app), metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			respondError(w, http.StatusNotFound, fmt.Sprintf("no secrets for app %q", app))
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to get secrets")
		return
	}

	value, ok := secret.Data[key]
	if !ok {
		respondError(w, http.StatusNotFound, fmt.Sprintf("secret %q not found", key))
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"key": key, "value": string(value)})
}

// Set creates or updates a secret value.
// PUT /api/v1/projects/{name}/apps/{app}/secrets
func (s *Secrets) Set(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	app := chi.URLParam(r, "app")

	var req map[string]string
	if err := decodeJSON(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	secret, err := s.Client.CoreV1().Secrets(project).Get(ctx, secretname.Secrets(secretname.KindApp, app), metav1.GetOptions{})
	if err != nil && !errors.IsNotFound(err) {
		respondError(w, http.StatusInternalServerError, "failed to get secrets")
		return
	}

	if errors.IsNotFound(err) {
		newSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      secretname.Secrets(secretname.KindApp, app),
				Namespace: project,
				Labels: map[string]string{
					kipperLabel: kipperValue,
					"app":       app,
				},
				// Stamp so a pod already running when the first secret is added is
				// reported as needing a restart to pick it up.
				Annotations: map[string]string{kipperv1.DataUpdatedAtAnnotation: time.Now().Format(time.RFC3339Nano)},
			},
			Data: make(map[string][]byte),
		}
		for k, v := range req {
			newSecret.Data[k] = []byte(v)
		}
		if _, err := s.Client.CoreV1().Secrets(project).Create(ctx, newSecret, metav1.CreateOptions{}); err != nil {
			respondError(w, http.StatusInternalServerError, "failed to create secrets")
			return
		}
	} else {
		if secret.Data == nil {
			secret.Data = make(map[string][]byte)
		}
		changed := false
		for k, v := range req {
			if current, exists := secret.Data[k]; !exists || string(current) != v {
				changed = true
			}
			// Preserve previous value before overwriting
			if current, exists := secret.Data[k]; exists && string(current) != v {
				secret.Data[k+".__previous"] = current
			}
			secret.Data[k] = []byte(v)
		}
		if changed {
			stampSecretDataUpdated(secret)
		}
		if _, err := s.Client.CoreV1().Secrets(project).Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
			respondError(w, http.StatusInternalServerError, "failed to update secrets")
			return
		}
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// Delete removes a single secret key.
// DELETE /api/v1/projects/{name}/apps/{app}/secrets/{key}
func (s *Secrets) Delete(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	app := chi.URLParam(r, "app")
	key := chi.URLParam(r, "key")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	secret, err := s.Client.CoreV1().Secrets(project).Get(ctx, secretname.Secrets(secretname.KindApp, app), metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			respondError(w, http.StatusNotFound, fmt.Sprintf("no secrets for app %q", app))
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to get secrets")
		return
	}

	if _, ok := secret.Data[key]; !ok {
		respondError(w, http.StatusNotFound, fmt.Sprintf("secret %q not found", key))
		return
	}

	delete(secret.Data, key)
	stampSecretDataUpdated(secret)
	if _, err := s.Client.CoreV1().Secrets(project).Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to update secrets")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// stampSecretDataUpdated records that the secret's data just changed, so the
// console's restart banner knows the running pods hold stale values until they
// restart.
func stampSecretDataUpdated(secret *corev1.Secret) {
	if secret.Annotations == nil {
		secret.Annotations = map[string]string{}
	}
	secret.Annotations[kipperv1.DataUpdatedAtAnnotation] = time.Now().Format(time.RFC3339Nano)
}
