package handlers

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"k8s.io/apimachinery/pkg/api/errors"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

type functionSettings struct {
	SecurityHeaders bool     `json:"security_headers"`
	CSPAllowlist    []string `json:"csp_allowlist"`
}

// GetFunctionSettings returns the security settings for a function.
// GET /api/v1/projects/{name}/functions/{fn}/settings
func (f *Functions) GetSettings(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	fnName := chi.URLParam(r, "fn")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	settings := functionSettings{SecurityHeaders: true, CSPAllowlist: []string{}}

	var fn kipperv1.Function
	if err := f.CRClient.Get(ctx, crclient.ObjectKey{Namespace: project, Name: fnName}, &fn); err != nil {
		if errors.IsNotFound(err) {
			respondJSON(w, http.StatusOK, settings)
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to get function")
		return
	}

	settings.SecurityHeaders = !fn.Spec.NoSecurityHeaders
	settings.CSPAllowlist = fn.Spec.CSPAllowlist
	if settings.CSPAllowlist == nil {
		settings.CSPAllowlist = []string{}
	}

	respondJSON(w, http.StatusOK, settings)
}

// UpdateFunctionSettings modifies the security settings for a function.
// PUT /api/v1/projects/{name}/functions/{fn}/settings
func (f *Functions) UpdateSettings(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	fnName := chi.URLParam(r, "fn")

	var req functionSettings
	if err := decodeJSON(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var fn kipperv1.Function
	if err := f.CRClient.Get(ctx, crclient.ObjectKey{Namespace: project, Name: fnName}, &fn); err != nil {
		if errors.IsNotFound(err) {
			respondError(w, http.StatusNotFound, "function not found")
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to get function")
		return
	}

	fn.Spec.NoSecurityHeaders = !req.SecurityHeaders
	fn.Spec.CSPAllowlist = req.CSPAllowlist

	if err := f.CRClient.Update(ctx, &fn); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to update settings")
		return
	}

	respondJSON(w, http.StatusOK, req)
}
