// Package handlers — function_config.go provides REST endpoints for the
// function detail / edit view: env vars, secrets, dependencies, and
// service bindings. The CLI manipulates the same surfaces through the
// dynamic client; this file is purely the HTTP face of the same model.
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
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/secretname"
)

// FunctionConfig provides handlers for the function detail / edit view.
type FunctionConfig struct {
	Client   kubernetes.Interface
	CRClient crclient.Client
}

func (f *FunctionConfig) getFunction(ctx context.Context, namespace, name string) (*kipperv1.Function, error) {
	var fn kipperv1.Function
	if err := f.CRClient.Get(ctx, crclient.ObjectKey{Namespace: namespace, Name: name}, &fn); err != nil {
		return nil, err
	}
	return &fn, nil
}

// GetEnv returns the function's plain env vars from FunctionSpec.Env.
// GET /api/v1/projects/{name}/functions/{fn}/env
func (f *FunctionConfig) GetEnv(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	fnName := chi.URLParam(r, "fn")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	fn, err := f.getFunction(ctx, project, fnName)
	if err != nil {
		if errors.IsNotFound(err) {
			respondJSON(w, http.StatusOK, map[string]string{})
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to get function")
		return
	}

	env := fn.Spec.Env
	if env == nil {
		env = map[string]string{}
	}
	respondJSON(w, http.StatusOK, env)
}

// UpdateEnv replaces the function's env vars. The reconciler picks up the
// change and rolls the deployment / cronjob / pollset so new pods see the
// new values.
// PUT /api/v1/projects/{name}/functions/{fn}/env
func (f *FunctionConfig) UpdateEnv(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	fnName := chi.URLParam(r, "fn")

	var env map[string]string
	if err := decodeJSON(r, &env); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	fn, err := f.getFunction(ctx, project, fnName)
	if err != nil {
		if errors.IsNotFound(err) {
			respondError(w, http.StatusNotFound, fmt.Sprintf("function %q not found", fnName))
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to get function")
		return
	}

	fn.Spec.Env = env
	if err := f.CRClient.Update(ctx, fn); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to update env vars")
		return
	}
	respondJSON(w, http.StatusOK, env)
}

// secretKeyInfo mirrors the apps secrets handler so the console can use a
// single component for both surfaces.
type fnSecretKeyInfo struct {
	Key         string `json:"key"`
	HasPrevious bool   `json:"has_previous"`
}

// ListSecretKeys returns the keys of the function's secrets Secret. Values
// are never returned through this endpoint.
// GET /api/v1/projects/{name}/functions/{fn}/secrets
func (f *FunctionConfig) ListSecretKeys(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	fnName := chi.URLParam(r, "fn")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	secret, err := f.Client.CoreV1().Secrets(project).Get(ctx, secretname.Secrets(secretname.KindFunction, fnName), metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			respondJSON(w, http.StatusOK, []fnSecretKeyInfo{})
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to get secrets")
		return
	}

	keys := make([]fnSecretKeyInfo, 0, len(secret.Data))
	for k := range secret.Data {
		if strings.HasSuffix(k, ".__previous") {
			continue
		}
		_, hasPrev := secret.Data[k+".__previous"]
		keys = append(keys, fnSecretKeyInfo{Key: k, HasPrevious: hasPrev})
	}
	respondJSON(w, http.StatusOK, keys)
}

// RevealSecret returns a single secret value in plaintext, so the route is
// registered behind env.reveal. Listing the keys beside it takes env.read;
// reading one takes more. Use sparingly — the preferred
// pattern is to rotate, not to read.
// GET /api/v1/projects/{name}/functions/{fn}/secrets/{key}
func (f *FunctionConfig) RevealSecret(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	fnName := chi.URLParam(r, "fn")
	key := chi.URLParam(r, "key")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	secret, err := f.Client.CoreV1().Secrets(project).Get(ctx, secretname.Secrets(secretname.KindFunction, fnName), metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			respondError(w, http.StatusNotFound, fmt.Sprintf("no secrets for function %q", fnName))
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to get secrets")
		return
	}

	val, ok := secret.Data[key]
	if !ok {
		respondError(w, http.StatusNotFound, fmt.Sprintf("secret %q not found", key))
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"key": key, "value": string(val)})
}

// SetSecrets creates or updates one or more secret values. Body is a flat
// map of key/value pairs. Existing keys are overwritten and the previous
// value is preserved under "<key>.__previous" so rotation is reversible.
// PUT /api/v1/projects/{name}/functions/{fn}/secrets
func (f *FunctionConfig) SetSecrets(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	fnName := chi.URLParam(r, "fn")

	var req map[string]string
	if err := decodeJSON(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	secret, err := f.Client.CoreV1().Secrets(project).Get(ctx, secretname.Secrets(secretname.KindFunction, fnName), metav1.GetOptions{})
	if err != nil && !errors.IsNotFound(err) {
		respondError(w, http.StatusInternalServerError, "failed to get secrets")
		return
	}

	if errors.IsNotFound(err) {
		newSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      secretname.Secrets(secretname.KindFunction, fnName),
				Namespace: project,
				Labels: map[string]string{
					kipperLabel: kipperValue,
					"app":       fnName,
				},
			},
			Data: make(map[string][]byte, len(req)),
		}
		for k, v := range req {
			newSecret.Data[k] = []byte(v)
		}
		if _, err := f.Client.CoreV1().Secrets(project).Create(ctx, newSecret, metav1.CreateOptions{}); err != nil {
			respondError(w, http.StatusInternalServerError, "failed to create secrets")
			return
		}
		respondJSON(w, http.StatusOK, map[string]string{"status": "created"})
		return
	}

	if secret.Data == nil {
		secret.Data = make(map[string][]byte)
	}
	for k, v := range req {
		if current, exists := secret.Data[k]; exists && string(current) != v {
			secret.Data[k+".__previous"] = current
		}
		secret.Data[k] = []byte(v)
	}
	if _, err := f.Client.CoreV1().Secrets(project).Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to update secrets")
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// DeleteSecret removes a single secret key.
// DELETE /api/v1/projects/{name}/functions/{fn}/secrets/{key}
func (f *FunctionConfig) DeleteSecret(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	fnName := chi.URLParam(r, "fn")
	key := chi.URLParam(r, "key")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	secret, err := f.Client.CoreV1().Secrets(project).Get(ctx, secretname.Secrets(secretname.KindFunction, fnName), metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			respondError(w, http.StatusNotFound, fmt.Sprintf("no secrets for function %q", fnName))
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
	delete(secret.Data, key+".__previous")
	if _, err := f.Client.CoreV1().Secrets(project).Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to update secrets")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// GetDependencies returns the function's inline-source dependency map.
// GET /api/v1/projects/{name}/functions/{fn}/dependencies
func (f *FunctionConfig) GetDependencies(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	fnName := chi.URLParam(r, "fn")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	fn, err := f.getFunction(ctx, project, fnName)
	if err != nil {
		if errors.IsNotFound(err) {
			respondJSON(w, http.StatusOK, map[string]string{})
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to get function")
		return
	}

	deps := map[string]string{}
	if fn.Spec.Source != nil && fn.Spec.Source.Dependencies != nil {
		deps = fn.Spec.Source.Dependencies
	}
	respondJSON(w, http.StatusOK, deps)
}

// UpdateDependencies replaces the function's inline-source dependency map.
// PUT /api/v1/projects/{name}/functions/{fn}/dependencies
func (f *FunctionConfig) UpdateDependencies(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	fnName := chi.URLParam(r, "fn")

	var deps map[string]string
	if err := decodeJSON(r, &deps); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	fn, err := f.getFunction(ctx, project, fnName)
	if err != nil {
		if errors.IsNotFound(err) {
			respondError(w, http.StatusNotFound, fmt.Sprintf("function %q not found", fnName))
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to get function")
		return
	}

	if fn.Spec.Source == nil {
		fn.Spec.Source = &kipperv1.FunctionSource{}
	}
	fn.Spec.Source.Dependencies = deps
	if err := f.CRClient.Update(ctx, fn); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to update dependencies")
		return
	}
	respondJSON(w, http.StatusOK, deps)
}

// bindingResponse is what the bindings list endpoint returns for each
// bound service. injected_env is the canonical answer to "what will I
// see in process.env when this binding is active" — derived from the
// service type and the chosen prefix via kipperv1.InjectedEnvNames.
type bindingResponse struct {
	Service     string   `json:"service"`
	Type        string   `json:"type"`
	Prefix      string   `json:"prefix"`
	Database    string   `json:"database,omitempty"`
	InjectedEnv []string `json:"injected_env"`
}

// ListBindings returns the function's service bindings, each enriched
// with the env var names that the binding will inject.
// GET /api/v1/projects/{name}/functions/{fn}/bindings
func (f *FunctionConfig) ListBindings(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	fnName := chi.URLParam(r, "fn")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	fn, err := f.getFunction(ctx, project, fnName)
	if err != nil {
		if errors.IsNotFound(err) {
			respondJSON(w, http.StatusOK, []bindingResponse{})
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to get function")
		return
	}

	out := make([]bindingResponse, 0, len(fn.Spec.ServiceBindings))
	for _, b := range fn.Spec.ServiceBindings {
		svcType := f.lookupServiceType(ctx, project, b.Name)
		prefix := b.Prefix
		if prefix == "" {
			prefix = kipperv1.DefaultBindingPrefix(svcType)
		}
		out = append(out, bindingResponse{
			Service:     b.Name,
			Type:        svcType,
			Prefix:      prefix,
			Database:    b.Database,
			InjectedEnv: kipperv1.InjectedEnvNames(svcType, prefix),
		})
	}
	respondJSON(w, http.StatusOK, out)
}

func (f *FunctionConfig) lookupServiceType(ctx context.Context, namespace, name string) string {
	var svc kipperv1.Service
	if err := f.CRClient.Get(ctx, crclient.ObjectKey{Namespace: namespace, Name: name}, &svc); err == nil {
		return svc.Spec.Type
	}
	// Fall back to a StatefulSet in the function's own namespace only, matching
	// the same-namespace binding rule and avoiding cross-project disclosure.
	ssList, err := f.Client.AppsV1().StatefulSets(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "kipper.run/service-type",
	})
	if err != nil {
		return ""
	}
	for _, ss := range ssList.Items {
		if ss.Name == name {
			return ss.Labels["kipper.run/service-type"]
		}
	}
	return ""
}
