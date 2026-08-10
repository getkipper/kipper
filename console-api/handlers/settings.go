package handlers

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/kubernetes"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

// Settings provides handlers for per-app security and configuration settings.
type Settings struct {
	Client   kubernetes.Interface
	CRClient crclient.Client
}

type appSettings struct {
	SecurityHeaders bool           `json:"security_headers"`
	InstanceHeader  bool           `json:"instance_header"`
	RateLimit       int            `json:"rate_limit"`
	RequireAPIKey   bool           `json:"require_api_key"`
	CSPAllowlist    []string       `json:"csp_allowlist"`
	Redirects       []redirectRule `json:"redirects"`
	BasicAuth       bool           `json:"basic_auth"`
	// APIKeyGatePending is true when RequireAPIKey is on but the forwardAuth
	// gate has not been confirmed in place, so the console can warn the route
	// may still be reachable without a key.
	APIKeyGatePending bool `json:"api_key_gate_pending"`
}

type redirectRule struct {
	Source    string `json:"source"`
	Target    string `json:"target"`
	Permanent bool   `json:"permanent"`
}

// Get returns the current settings for an app by reading the App CR's Route spec.
// GET /api/v1/projects/{name}/apps/{app}/settings
func (s *Settings) Get(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	app := chi.URLParam(r, "app")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	settings := appSettings{SecurityHeaders: true, InstanceHeader: true, RateLimit: 0}

	var appCR kipperv1.App
	if err := s.CRClient.Get(ctx, crclient.ObjectKey{Namespace: project, Name: app}, &appCR); err != nil {
		if errors.IsNotFound(err) {
			respondJSON(w, http.StatusOK, settings)
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to get app")
		return
	}

	if appCR.Spec.Route != nil {
		settings.SecurityHeaders = !appCR.Spec.Route.NoSecurityHeaders
		settings.InstanceHeader = !appCR.Spec.Route.NoInstanceHeader
		settings.RateLimit = appCR.Spec.Route.RateLimit
		settings.RequireAPIKey = appCR.Spec.Route.RequireAPIKey
		settings.CSPAllowlist = appCR.Spec.Route.CSPAllowlist
		settings.BasicAuth = appCR.Spec.Route.BasicAuth
		for _, r := range appCR.Spec.Route.Redirects {
			settings.Redirects = append(settings.Redirects, redirectRule{
				Source: r.Source, Target: r.Target, Permanent: r.Permanent,
			})
		}
		// The gate reconciles asynchronously, so a route can stay anonymous
		// after the toggle flips on. Flag it until the reconciler confirms the
		// forwardAuth middleware is in place.
		if settings.RequireAPIKey {
			settings.APIKeyGatePending = !apimeta.IsStatusConditionTrue(appCR.Status.Conditions, kipperv1.ConditionAPIKeyGateReady)
		}
	}
	if settings.CSPAllowlist == nil {
		settings.CSPAllowlist = []string{}
	}
	if settings.Redirects == nil {
		settings.Redirects = []redirectRule{}
	}

	respondJSON(w, http.StatusOK, settings)
}

// Update modifies the security settings for an app.
// PUT /api/v1/projects/{name}/apps/{app}/settings
func (s *Settings) Update(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	app := chi.URLParam(r, "app")

	var req appSettings
	if err := decodeJSON(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var appCR kipperv1.App
	if err := s.CRClient.Get(ctx, crclient.ObjectKey{Namespace: project, Name: app}, &appCR); err != nil {
		if errors.IsNotFound(err) {
			respondError(w, http.StatusNotFound, fmt.Sprintf("app %q not found", app))
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to get app")
		return
	}

	if appCR.Spec.Route == nil {
		appCR.Spec.Route = &kipperv1.AppRoute{}
	}

	appCR.Spec.Route.NoSecurityHeaders = !req.SecurityHeaders
	appCR.Spec.Route.NoInstanceHeader = !req.InstanceHeader
	appCR.Spec.Route.RateLimit = req.RateLimit
	appCR.Spec.Route.RequireAPIKey = req.RequireAPIKey
	appCR.Spec.Route.CSPAllowlist = req.CSPAllowlist
	appCR.Spec.Route.BasicAuth = req.BasicAuth

	appCR.Spec.Route.Redirects = nil
	for _, r := range req.Redirects {
		appCR.Spec.Route.Redirects = append(appCR.Spec.Route.Redirects, kipperv1.RedirectRule{
			Source: r.Source, Target: r.Target, Permanent: r.Permanent,
		})
	}

	if err := s.CRClient.Update(ctx, &appCR); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to update settings")
		return
	}

	respondJSON(w, http.StatusOK, req)
}

// ingressReferencesApp checks if an Ingress matches an app by label or backend service name.
func ingressReferencesApp(ing networkingv1.Ingress, app string) bool {
	if ing.Labels["app"] == app {
		return true
	}
	for _, rule := range ing.Spec.Rules {
		if rule.HTTP == nil {
			continue
		}
		for _, path := range rule.HTTP.Paths {
			if path.Backend.Service != nil && path.Backend.Service.Name == app {
				return true
			}
		}
	}
	return false
}
