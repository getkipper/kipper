package handlers

import (
	"context"
	stderrors "errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/retry"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/internal/resourcebounds"
	"github.com/getkipper/kipper/controller/pkg/healthcheck"
)

// HealthCheck reads and updates spec.health independently of route settings.
type HealthCheck struct {
	CRClient crclient.Client
}

type healthCheckBody struct {
	Type                  string `json:"type"`
	Path                  string `json:"path,omitempty"`
	Port                  *int32 `json:"port,omitempty"`
	StartupTimeoutSeconds *int32 `json:"startup_timeout_seconds,omitempty"`
	TimeoutSeconds        *int32 `json:"timeout_seconds,omitempty"`
}

// errHealthDropped reports that the API server discarded spec.health.
var errHealthDropped = stderrors.New("the cluster did not keep the health check: its App schema predates health checks. Upgrade the cluster with kip upgrade, then save again")

type invalidHealthCheck struct{ error }

type healthCheckResponse struct {
	Health healthCheckBody                `json:"health"`
	Status *kipperv1.AppHealthCheckStatus `json:"status"`
}

// Get returns the declared check (auto if omitted) and its observed status.
// GET /api/v1/projects/{name}/apps/{app}/health-check
func (h *HealthCheck) Get(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var app kipperv1.App
	if err := h.CRClient.Get(ctx, crclient.ObjectKey{Namespace: chi.URLParam(r, "name"), Name: chi.URLParam(r, "app")}, &app); err != nil {
		if errors.IsNotFound(err) {
			respondError(w, http.StatusNotFound, "app not found")
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to get app")
		return
	}
	resp := healthCheckResponse{Health: healthCheckBody{Type: healthcheck.Auto}, Status: app.Status.HealthCheck}
	if hc := app.Spec.Health; hc != nil {
		resp.Health = healthCheckBody{Type: hc.Type, Path: hc.Path, Port: hc.Port, StartupTimeoutSeconds: hc.StartupTimeoutSeconds, TimeoutSeconds: hc.TimeoutSeconds}
	}
	respondJSON(w, http.StatusOK, resp)
}

// Update replaces the declared check, or clears it with type auto. Changes
// to the rendered probe trigger a rollout; startup timeout changes do not.
// PUT /api/v1/projects/{name}/apps/{app}/health-check
func (h *HealthCheck) Update(w http.ResponseWriter, r *http.Request) {
	var body healthCheckBody
	if err := decodeJSON(r, &body); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	check := healthcheck.Check{
		Type: body.Type, Path: body.Path, Port: body.Port,
		StartupTimeoutSeconds: body.StartupTimeoutSeconds, TimeoutSeconds: body.TimeoutSeconds,
	}
	check.Normalize()

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	key := crclient.ObjectKey{Namespace: chi.URLParam(r, "name"), Name: chi.URLParam(r, "app")}
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var app kipperv1.App
		if err := h.CRClient.Get(ctx, key, &app); err != nil {
			return err
		}
		if check.Type == healthcheck.Auto {
			app.Spec.Health = nil
		} else {
			if err := check.Validate(app.Spec.Port); err != nil {
				return invalidHealthCheck{err}
			}
			app.Spec.Health = &kipperv1.AppHealth{
				Type: check.Type, Path: check.Path, Port: check.Port,
				StartupTimeoutSeconds: check.StartupTimeoutSeconds, TimeoutSeconds: check.TimeoutSeconds,
			}
		}
		if err := crclient.WithFieldOwner(h.CRClient, resourcebounds.ConsoleManager).Update(ctx, &app); err != nil {
			return err
		}
		if check.Type == healthcheck.Auto {
			return nil
		}
		// Read into a fresh object: decoding an update response into app can
		// retain fields that the server omitted.
		var stored kipperv1.App
		if err := h.CRClient.Get(ctx, key, &stored); err != nil {
			return err
		}
		if stored.Spec.Health == nil {
			return errHealthDropped
		}
		return nil
	})
	var invalid invalidHealthCheck
	switch {
	case stderrors.As(err, &invalid):
		respondError(w, http.StatusUnprocessableEntity, invalid.Error())
	case stderrors.Is(err, errHealthDropped):
		respondError(w, http.StatusConflict, err.Error())
	case errors.IsNotFound(err):
		respondError(w, http.StatusNotFound, "app not found")
	case errors.IsInvalid(err):
		respondError(w, http.StatusUnprocessableEntity, err.Error())
	case err != nil:
		respondError(w, http.StatusInternalServerError, "failed to update the health check")
	default:
		respondJSON(w, http.StatusOK, map[string]string{"status": "updated"})
	}
}
