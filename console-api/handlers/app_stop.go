package handlers

import (
	"context"
	stderrors "errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

const maxStopReason = 500

// Stop records a request to scale the app to zero while preserving its configuration.
// For an existing stop, it replaces the reason and preserves the original record.
// POST /api/v1/projects/{name}/apps/{app}/stop
func (a *Apps) Stop(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	appName := chi.URLParam(r, "app")

	var req struct {
		Reason string `json:"reason"`
	}
	if err := decodeJSON(r, &req); err != nil && !stderrors.Is(err, io.EOF) {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.Reason) > maxStopReason {
		respondError(w, http.StatusBadRequest, fmt.Sprintf("the reason must be at most %d characters", maxStopReason))
		return
	}
	by := SubjectFromRequest(r)

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	stored, err := a.updateApp(ctx, project, appName, func(app *kipperv1.App) {
		if app.Spec.Stopped != nil {
			app.Spec.Stopped.Reason = req.Reason
			return
		}
		now := metav1.Now()
		app.Spec.Stopped = &kipperv1.AppStopped{Reason: req.Reason, By: by, At: &now}
	})
	if a.respondUpdateError(w, appName, "stop", err) {
		return
	}
	// Check the stored field: an older App schema can prune it from a successful write.
	if stored.Spec.Stopped == nil {
		respondError(w, http.StatusConflict, fmt.Sprintf(
			"this cluster's App schema predates stopping apps, so %s is still running. Upgrade Kipper, then try again", appName))
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
}

// Start removes an app's stop, so it runs with its replica count and
// autoscaling again. Starting a running app changes nothing.
// POST /api/v1/projects/{name}/apps/{app}/start
func (a *Apps) Start(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	appName := chi.URLParam(r, "app")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	wasStopped := false
	var started kipperv1.App
	_, err := a.updateApp(ctx, project, appName, func(app *kipperv1.App) {
		wasStopped = app.Spec.Stopped != nil
		app.Spec.Stopped = nil
		started = *app
	})
	if a.respondUpdateError(w, appName, "start", err) {
		return
	}
	if !wasStopped {
		respondJSON(w, http.StatusOK, map[string]string{"status": "running"})
		return
	}
	resp := map[string]string{"status": "starting"}
	autoscaled := started.Spec.Autoscale != nil && started.Spec.Autoscale.Enabled
	if !autoscaled && started.Spec.Replicas != nil && *started.Spec.Replicas == 0 {
		resp["note"] = fmt.Sprintf("%s is scaled to zero replicas, so it runs no pods until it is scaled up", appName)
	}
	respondJSON(w, http.StatusOK, resp)
}

// updateApp applies a spec change with conflict retries and returns the stored app.
// Unchanged specs skip the write.
func (a *Apps) updateApp(ctx context.Context, project, appName string, change func(*kipperv1.App)) (*kipperv1.App, error) {
	var stored *kipperv1.App
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		app := &kipperv1.App{}
		if err := a.CRClient.Get(ctx, crclient.ObjectKey{Namespace: project, Name: appName}, app); err != nil {
			return err
		}
		before := app.Spec.DeepCopy()
		change(app)
		if !equality.Semantic.DeepEqual(before, &app.Spec) {
			if err := a.CRClient.Update(ctx, app); err != nil {
				return err
			}
		}
		stored = app
		return nil
	})
	return stored, err
}

// respondUpdateError writes the response for a failed updateApp and reports
// whether there was one.
func (a *Apps) respondUpdateError(w http.ResponseWriter, appName, action string, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.IsNotFound(err):
		respondError(w, http.StatusNotFound, fmt.Sprintf("app %q not found", appName))
	default:
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to %s app", action))
	}
	return true
}
