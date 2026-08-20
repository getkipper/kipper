package handlers

import (
	"context"
	"encoding/json"
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

// registryRequest is what this endpoint may set. It carries the allow-list
// because the published shape always has, and it is read as raw JSON so that a
// field left out can be told from one sent as null.
//
// What it may not do is change who may pull with an existing credential. A
// caller holding a copy read before somebody else granted a project would revoke
// that project by rotating a password, which is what this endpoint did until
// now, and there is no server-side way to validate the projects named, since the
// organisation prefix a grant is stored under lives in kip's own configuration.
type registryRequest struct {
	Name            string          `json:"name"`
	Server          string          `json:"server"`
	Username        string          `json:"username"`
	Password        string          `json:"password"`
	AllowedProjects json.RawMessage `json:"allowedProjects"`
}

func (r registryRequest) entry(allowed []string) registrycred.Entry {
	return registrycred.Entry{
		Name:            r.Name,
		Server:          r.Server,
		Username:        r.Username,
		Password:        r.Password,
		AllowedProjects: allowed,
	}
}

// requestedProjects is the allow-list a request carries, and whether it carried
// one at all. An absent field asks for no change; null and [] both ask for a
// credential nobody may pull with.
func (r registryRequest) requestedProjects() ([]string, bool, error) {
	if len(r.AllowedProjects) == 0 {
		return nil, false, nil
	}
	var projects []string
	if err := json.Unmarshal(r.AllowedProjects, &projects); err != nil {
		return nil, false, err
	}
	if projects == nil {
		projects = []string{}
	}
	return projects, true, nil
}

// Add creates or updates a registry credential.
// POST /api/v1/settings/registries
func (reg *Registry) Add(w http.ResponseWriter, r *http.Request) {
	var req registryRequest
	if err := decodeJSON(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Server == "" || req.Username == "" || req.Password == "" {
		respondError(w, http.StatusBadRequest, "server, username, and password are required")
		return
	}

	wanted, carried, err := req.requestedProjects()
	if err != nil {
		respondError(w, http.StatusBadRequest, "allowedProjects must be a list of project names")
		return
	}

	req.Server = registrycred.NormalizeServer(req.Server)

	if req.Name == "" {
		req.Name = sanitizeRegistryName(req.Server)
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	if err := registrycred.Update(ctx, reg.Client, func(entries []registrycred.Entry) ([]registrycred.Entry, error) {
		if live := registrycred.Find(entries, req.Name); live != nil {
			if carried && !sameProjects(wanted, live.AllowedProjects) {
				return nil, errChangesTheAllowList
			}
			// The stored list, or the carried one when it carries the same set:
			// either way this endpoint never changes who may pull.
			stored := live.AllowedProjects
			if carried {
				stored = uniqueProjects(wanted)
			}
			*live = req.entry(stored)
			return entries, nil
		}
		// Nothing exists to overwrite, so the first list may be set here, which
		// is how the published shape has always created a granted credential.
		if !carried {
			wanted = []string{}
		}
		return append(entries, req.entry(uniqueProjects(wanted))), nil
	}); err != nil {
		if errors.Is(err, errChangesTheAllowList) {
			respondError(w, http.StatusBadRequest,
				"this endpoint cannot change who may pull with an existing credential. Grant a project with 'kip registry allow <name> --project <project>' or take one away with 'kip registry revoke'. Send the allow-list unchanged, or leave it out, to edit the rest")
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to save registry credentials")
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "saved", "name": req.Name})
}

// Remove deletes a registry credential.
// DELETE /api/v1/settings/registries/{name}
func (reg *Registry) Remove(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	if err := registrycred.Update(ctx, reg.Client, func(entries []registrycred.Entry) ([]registrycred.Entry, error) {
		kept := make([]registrycred.Entry, 0, len(entries))
		for _, e := range entries {
			if e.Name != name {
				kept = append(kept, e)
			}
		}
		if len(kept) == len(entries) {
			return nil, &registrycred.UnknownRegistryError{Name: name}
		}
		return kept, nil
	}); err != nil {
		var unknown *registrycred.UnknownRegistryError
		if errors.As(err, &unknown) {
			respondError(w, http.StatusNotFound, fmt.Sprintf("registry %q not found", name))
			return
		}
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
