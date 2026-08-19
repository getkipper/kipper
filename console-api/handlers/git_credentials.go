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
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/internal/gitcred"
	"github.com/getkipper/kipper/console-api/middleware"
	"github.com/getkipper/kipper/controller/pkg/giturl"
)

// GitCredentials provides handlers for shared git credential management. Shared
// credentials live only in the kipper-system list Secret and are resolved at
// build time; they are never copied into tenant namespaces.
type GitCredentials struct {
	Client   kubernetes.Interface
	CRClient crclient.Client
}

type gitCredentialResponse struct {
	Name            string   `json:"name"`
	Server          string   `json:"server"`
	Username        string   `json:"username"`
	Token           string   `json:"token"`
	AllowedProjects []string `json:"allowedProjects"`
	AppCount        int      `json:"app_count"`
}

// List returns all configured git credentials with masked tokens.
// GET /api/v1/settings/git-credentials
func (gc *GitCredentials) List(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	entries, _ := gitcred.Load(ctx, gc.Client)

	resp := make([]gitCredentialResponse, len(entries))
	for i, e := range entries {
		resp[i] = gitCredentialResponse{
			Name:            e.Name,
			Server:          e.Server,
			Username:        e.Username,
			Token:           maskValue(e.Token),
			AllowedProjects: e.AllowedProjects,
			AppCount:        gc.countAppsUsingCredential(ctx, e.Name),
		}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{"credentials": resp})
}

// Add creates or updates a shared git credential.
// POST /api/v1/settings/git-credentials
func (gc *GitCredentials) Add(w http.ResponseWriter, r *http.Request) {
	var req gitcred.Entry
	if err := decodeJSON(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Server == "" || req.Token == "" {
		respondError(w, http.StatusBadRequest, "server and token are required")
		return
	}

	// Normalize and validate the server so it host-binds cleanly at build time.
	authority, err := giturl.CanonicalAuthority(req.Server)
	if err != nil {
		respondError(w, http.StatusBadRequest, fmt.Sprintf("invalid server: %v", err))
		return
	}
	req.Server = authority

	if req.Username == "" {
		req.Username = "oauth2"
	}

	if req.Name == "" {
		req.Name = sanitizeRegistryName(req.Server)
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	entries, err := gitcred.Load(ctx, gc.Client)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to read git credentials")
		return
	}

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

	if err := gitcred.Save(ctx, gc.Client, entries); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to save git credentials")
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "saved", "name": req.Name})
}

// Remove deletes a shared git credential.
// DELETE /api/v1/settings/git-credentials/{name}
func (gc *GitCredentials) Remove(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	// Check if any apps reference this credential
	appCount := gc.countAppsUsingCredential(ctx, name)
	if appCount > 0 {
		respondError(w, http.StatusConflict, fmt.Sprintf("credential %q is used by %d app(s). Remove the credential from those apps first", name, appCount))
		return
	}

	entries, err := gitcred.Load(ctx, gc.Client)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to read git credentials")
		return
	}

	filtered := make([]gitcred.Entry, 0, len(entries))
	for _, e := range entries {
		if e.Name != name {
			filtered = append(filtered, e)
		}
	}

	if len(filtered) == len(entries) {
		respondError(w, http.StatusNotFound, fmt.Sprintf("git credential %q not found", name))
		return
	}

	if err := gitcred.Save(ctx, gc.Client, filtered); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to save git credentials")
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

// Health probes all configured git credentials and returns validity and expiry info.
// GET /api/v1/settings/git-credentials/health
func (gc *GitCredentials) Health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	entries, _ := gitcred.Load(ctx, gc.Client)

	type result struct {
		name   string
		health tokenHealth
	}

	ch := make(chan result, len(entries))
	var wg sync.WaitGroup

	for _, entry := range entries {
		wg.Add(1)
		go func(e gitcred.Entry) {
			defer wg.Done()
			probeCtx, probeCancel := context.WithTimeout(ctx, 5*time.Second)
			defer probeCancel()
			ch <- result{name: e.Name, health: probeGitCredential(probeCtx, e.Server, e.Token)}
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

// Reveal returns the plaintext token for a single git credential after
// re-verifying the caller's password against Dex. The admin-role check is
// enforced upstream by middleware.RequireRole; this handler adds the
// knowledge-factor gate on top.
// POST /api/v1/settings/git-credentials/{name}/reveal
func (gc *GitCredentials) Reveal(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if name == "" {
		respondError(w, http.StatusBadRequest, "credential name required")
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

	if err := VerifyUserPassword(ctx, gc.Client, claims.Email, req.Password); err != nil {
		if errors.Is(err, ErrInvalidPassword) {
			log.Printf("reveal git-credential=%s by user=%s: invalid password", name, claims.Email)
			respondError(w, http.StatusUnauthorized, "invalid password")
			return
		}
		log.Printf("reveal git-credential=%s by user=%s: %v", name, claims.Email, err)
		respondError(w, http.StatusInternalServerError, "password verification failed")
		return
	}

	entries, _ := gitcred.Load(ctx, gc.Client)
	if e := gitcred.Find(entries, name); e != nil {
		log.Printf("reveal git-credential=%s by user=%s: ok", name, claims.Email)
		respondJSON(w, http.StatusOK, map[string]string{"token": e.Token})
		return
	}

	respondError(w, http.StatusNotFound, fmt.Sprintf("git credential %q not found", name))
}

func (gc *GitCredentials) countAppsUsingCredential(ctx context.Context, credentialName string) int {
	var apps kipperv1.AppList
	if err := gc.CRClient.List(ctx, &apps); err != nil {
		return 0
	}

	count := 0
	for _, app := range apps.Items {
		if app.Spec.Git != nil && app.Spec.Git.CredentialsSecret == credentialName {
			count++
		}
	}
	return count
}
