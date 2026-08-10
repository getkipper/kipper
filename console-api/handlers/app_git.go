package handlers

import (
	"context"
	stderrors "errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/internal/giturl"
	"github.com/getkipper/kipper/console-api/middleware"
)

// gitResponse is what the UI reads when populating the Git source
// panel. The token field is never echoed back — the API treats it
// strictly as write-only so a compromised read endpoint can't exfil
// credentials.
type gitResponse struct {
	Configured     bool   `json:"configured"`
	URL            string `json:"url,omitempty"`
	Branch         string `json:"branch,omitempty"`
	DockerfilePath string `json:"dockerfile_path,omitempty"`
	Context        string `json:"context,omitempty"`
	HasToken       bool   `json:"has_token"`
}

type setGitRequest struct {
	URL            string `json:"url,omitempty"`
	Branch         string `json:"branch,omitempty"`
	DockerfilePath string `json:"dockerfile_path,omitempty"`
	Context        string `json:"context,omitempty"`
	// Token is optional. Empty string means "leave the existing
	// credentials Secret alone". Non-empty rotates the token in place.
	Token string `json:"token,omitempty"`
}

// GetGit returns the App's current Git source for the Settings UI. The
// token is never returned — only a `has_token` boolean indicating
// whether a credentials Secret is wired up.
// GET /api/v1/projects/{name}/apps/{app}/git
func (a *Apps) GetGit(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	appName := chi.URLParam(r, "app")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var appCR kipperv1.App
	if err := a.CRClient.Get(ctx, crclient.ObjectKey{Namespace: project, Name: appName}, &appCR); err != nil {
		if errors.IsNotFound(err) {
			respondJSON(w, http.StatusOK, gitResponse{})
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to get app")
		return
	}

	if appCR.Spec.Git == nil {
		respondJSON(w, http.StatusOK, gitResponse{})
		return
	}

	resp := gitResponse{
		Configured:     true,
		URL:            sanitizeGitURL(appCR.Spec.Git.URL),
		Branch:         appCR.Spec.Git.Branch,
		DockerfilePath: appCR.Spec.Git.DockerfilePath,
		Context:        appCR.Spec.Git.Context,
		HasToken:       appCR.Spec.Git.CredentialsSecret != "",
	}
	respondJSON(w, http.StatusOK, resp)
}

// SetGit updates the App's Git source. Empty string fields are NOT
// applied — the request acts as a partial update so the UI can rotate
// just the token, or just the branch, without re-supplying every field.
// A non-empty token replaces the data on the `<app>-git-credentials`
// Secret in place; the App CR's `credentialsSecret` always points at
// that fixed name so the reconciler picks the new value up on the next
// build.
//
// PUT /api/v1/projects/{name}/apps/{app}/git
func (a *Apps) SetGit(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	appName := chi.URLParam(r, "app")

	var req setGitRequest
	if err := decodeJSON(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var appCR kipperv1.App
	if err := a.CRClient.Get(ctx, crclient.ObjectKey{Namespace: project, Name: appName}, &appCR); err != nil {
		if errors.IsNotFound(err) {
			respondError(w, http.StatusNotFound, fmt.Sprintf("app %q not found", appName))
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to get app")
		return
	}

	if appCR.Spec.Git == nil {
		// Operators can wire a git source onto an existing image-based
		// App; the URL is the only field required to bootstrap that.
		if req.URL == "" {
			respondError(w, http.StatusBadRequest, "git.url is required to attach a git source to an app that does not have one")
			return
		}
		appCR.Spec.Git = &kipperv1.AppGitSource{}
	}

	// Reject a URL the build could not host-bind a credential to; a clean early
	// error rather than a build-time failure.
	if req.URL != "" {
		if _, err := giturl.CanonicalAuthority(req.URL); err != nil {
			respondError(w, http.StatusBadRequest, fmt.Sprintf("invalid git url: %v", err))
			return
		}
	}

	// Partial update: only overwrite fields the request actually set.
	// This lets the UI ship a token-only payload without clearing the
	// other fields.
	if req.URL != "" {
		appCR.Spec.Git.URL = req.URL
	}
	if req.Branch != "" {
		appCR.Spec.Git.Branch = req.Branch
	}
	if req.DockerfilePath != "" {
		appCR.Spec.Git.DockerfilePath = req.DockerfilePath
	}
	if req.Context != "" {
		appCR.Spec.Git.Context = req.Context
	}

	// Token rotation. The credentials Secret is named after the App so
	// the reconciler reads the new value on the next build without
	// needing the CR to know its name changed. We only touch it when a
	// non-empty token comes in.
	if req.Token != "" {
		secretName := appName + "-git-credentials"
		if err := a.createGitCredentialsSecret(ctx, project, secretName, appName, req.Token); err != nil {
			respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to store git credentials: %v", err))
			return
		}
		appCR.Spec.Git.CredentialsSecret = secretName
	}

	if err := a.CRClient.Update(ctx, &appCR); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to update git source")
		return
	}

	respondJSON(w, http.StatusOK, gitResponse{
		Configured:     true,
		URL:            sanitizeGitURL(appCR.Spec.Git.URL),
		Branch:         appCR.Spec.Git.Branch,
		DockerfilePath: appCR.Spec.Git.DockerfilePath,
		Context:        appCR.Spec.Git.Context,
		HasToken:       appCR.Spec.Git.CredentialsSecret != "",
	})
}

// RevealGitToken returns the plaintext token an app uses to clone its source
// repository, after re-verifying the caller's password against Dex. This
// breaks the write-only invariant of GetGit, so it sits behind two gates:
// the deployer role (enforced upstream by middleware.RequireRole) and the
// knowledge-factor password check here.
//
// Reveal is deployer-accessible, looser than the admin-only global credential
// reveal. A deployer already owns an app's git source and can rotate its token
// via SetGit, so recovering it stays within their existing scope; the password
// re-entry is the second factor. Operators who treat app tokens as broad,
// cross-repo PATs should scope them per repository.
// POST /api/v1/projects/{name}/apps/{app}/git/reveal
func (a *Apps) RevealGitToken(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	appName := chi.URLParam(r, "app")

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

	if err := VerifyUserPassword(ctx, a.Client, claims.Email, req.Password); err != nil {
		if stderrors.Is(err, ErrInvalidPassword) {
			log.Printf("reveal git-token app=%s/%s by user=%s: invalid password", project, appName, claims.Email)
			respondError(w, http.StatusUnauthorized, "invalid password")
			return
		}
		log.Printf("reveal git-token app=%s/%s by user=%s: %v", project, appName, claims.Email, err)
		respondError(w, http.StatusInternalServerError, "password verification failed")
		return
	}

	var appCR kipperv1.App
	if err := a.CRClient.Get(ctx, crclient.ObjectKey{Namespace: project, Name: appName}, &appCR); err != nil {
		if errors.IsNotFound(err) {
			respondError(w, http.StatusNotFound, fmt.Sprintf("app %q not found", appName))
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to get app")
		return
	}

	if appCR.Spec.Git == nil || appCR.Spec.Git.CredentialsSecret == "" {
		respondError(w, http.StatusNotFound, fmt.Sprintf("app %q has no git credential configured", appName))
		return
	}

	// Reveal only the app's OWN per-app credential. A shared credential (or a
	// leftover fan-out copy named after one) is administrator-managed and must
	// not be disclosed to a deployer through the per-app reveal — otherwise a
	// deployer could point CredentialsSecret at a shared token their project is
	// not allow-listed for and read it in plaintext, bypassing the builder's
	// classification. This applies the same contract the builder enforces.
	if appCR.Spec.Git.CredentialsSecret != appName+"-git-credentials" {
		respondError(w, http.StatusForbidden, "this app uses an administrator-managed shared git credential, which cannot be revealed here")
		return
	}

	secret, err := a.Client.CoreV1().Secrets(project).Get(ctx, appCR.Spec.Git.CredentialsSecret, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			respondError(w, http.StatusNotFound, "git credentials secret not found")
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to read git credentials")
		return
	}

	token, ok := secret.Data["token"]
	if !ok || len(token) == 0 {
		respondError(w, http.StatusNotFound, "git credentials secret has no token")
		return
	}

	log.Printf("reveal git-token app=%s/%s by user=%s: ok", project, appName, claims.Email)
	respondJSON(w, http.StatusOK, map[string]string{"token": string(token)})
}
