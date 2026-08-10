package handlers

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"k8s.io/client-go/kubernetes"

	"github.com/getkipper/kipper/console-api/internal/registrycred"
	"github.com/getkipper/kipper/console-api/middleware"
)

const (
	registryConfigName = registrycred.ConfigSecretName
	registryNamespace  = registrycred.Namespace
)

// Registry provides handlers for container registry credential management. The
// credentials live only in the kipper-system list Secret and are staged as a
// scoped, workload-owned pull Secret at reconcile time; they are never copied
// into tenant namespaces.
type Registry struct {
	Client kubernetes.Interface
}

type registryEntry = registrycred.Entry

type registryListResponse struct {
	Registries []registryEntry `json:"registries"`
}

// List returns all configured registries with masked passwords.
// GET /api/v1/settings/registries
func (reg *Registry) List(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	entries, err := registrycred.Load(ctx, reg.Client)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to read registry credentials")
		return
	}

	// Mask passwords in response
	for i := range entries {
		if entries[i].Password != "" {
			entries[i].Password = maskValue(entries[i].Password)
		}
	}

	respondJSON(w, http.StatusOK, registryListResponse{Registries: entries})
}

// Add creates or updates a registry credential.
// POST /api/v1/settings/registries
func (reg *Registry) Add(w http.ResponseWriter, r *http.Request) {
	var req registryEntry
	if err := decodeJSON(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Server == "" || req.Username == "" || req.Password == "" {
		respondError(w, http.StatusBadRequest, "server, username, and password are required")
		return
	}

	req.Server = registrycred.NormalizeServer(req.Server)

	if req.Name == "" {
		req.Name = sanitizeRegistryName(req.Server)
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	entries, err := registrycred.Load(ctx, reg.Client)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to read registry credentials")
		return
	}

	// Update existing or append new
	found := false
	for i := range entries {
		if entries[i].Name == req.Name {
			entries[i] = req
			found = true
			break
		}
	}
	if !found {
		entries = append(entries, req)
	}

	if err := registrycred.Save(ctx, reg.Client, entries); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to save registry credentials")
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

// Remove deletes a registry credential.
// DELETE /api/v1/settings/registries/{name}
func (reg *Registry) Remove(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	entries, err := registrycred.Load(ctx, reg.Client)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to read registry credentials")
		return
	}

	filtered := make([]registryEntry, 0, len(entries))
	for _, e := range entries {
		if e.Name != name {
			filtered = append(filtered, e)
		}
	}

	if len(filtered) == len(entries) {
		respondError(w, http.StatusNotFound, fmt.Sprintf("registry %q not found", name))
		return
	}

	if err := registrycred.Save(ctx, reg.Client, filtered); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to save registry credentials")
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

// Health probes all configured registries and returns validity and expiry info.
// GET /api/v1/settings/registries/health
func (reg *Registry) Health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	entries, err := registrycred.Load(ctx, reg.Client)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to read registry credentials")
		return
	}

	type result struct {
		name   string
		health tokenHealth
	}

	ch := make(chan result, len(entries))
	var wg sync.WaitGroup

	for _, entry := range entries {
		wg.Add(1)
		go func(e registryEntry) {
			defer wg.Done()
			probeCtx, probeCancel := context.WithTimeout(ctx, 5*time.Second)
			defer probeCancel()
			ch <- result{name: e.Name, health: probeRegistry(probeCtx, e)}
		}(entry)
	}

	wg.Wait()
	close(ch)

	health := make(map[string]tokenHealth, len(entries))
	for r := range ch {
		health[r.name] = r.health
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{"health": health})
}

// Reveal returns the plaintext password for a single registry credential after
// re-verifying the caller's password against Dex.
// POST /api/v1/settings/registries/{name}/reveal
func (reg *Registry) Reveal(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if name == "" {
		respondError(w, http.StatusBadRequest, "registry name required")
		return
	}

	var req struct {
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &req); err != nil || req.Password == "" {
		respondError(w, http.StatusBadRequest, "password is required")
		return
	}

	claims := middleware.UserFromContext(r.Context())
	if claims == nil {
		respondError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	if err := VerifyUserPassword(ctx, reg.Client, claims.Email, req.Password); err != nil {
		if errors.Is(err, ErrInvalidPassword) {
			log.Printf("reveal registry=%s by user=%s: invalid password", name, claims.Email)
			respondError(w, http.StatusUnauthorized, "invalid password")
			return
		}
		log.Printf("reveal registry=%s by user=%s: %v", name, claims.Email, err)
		respondError(w, http.StatusInternalServerError, "password verification failed")
		return
	}

	entries, err := registrycred.Load(ctx, reg.Client)
	if err != nil {
		log.Printf("reveal registry=%s by user=%s: %v", name, claims.Email, err)
		respondError(w, http.StatusInternalServerError, "failed to read registry credentials")
		return
	}
	for _, e := range entries {
		if e.Name == name {
			log.Printf("reveal registry=%s by user=%s: ok", name, claims.Email)
			respondJSON(w, http.StatusOK, map[string]string{"password": e.Password})
			return
		}
	}

	respondError(w, http.StatusNotFound, fmt.Sprintf("registry %q not found", name))
}

// sanitizeRegistryName derives a default credential name from a server's host,
// so the canonical Docker Hub key yields index-docker-io rather than a name
// carrying scheme and path debris. kip generates the same default, so both
// writers address the same entry for the same registry.
func sanitizeRegistryName(server string) string {
	host := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(server, "https://"), "http://"), "/")
	if i := strings.IndexByte(host, '/'); i >= 0 {
		host = host[:i]
	}
	name := strings.ReplaceAll(host, ".", "-")
	name = strings.ReplaceAll(name, ":", "-")
	return strings.ToLower(name)
}

func maskValue(s string) string {
	if len(s) <= 8 {
		return "••••••••"
	}
	return s[:4] + "••••••••"
}
