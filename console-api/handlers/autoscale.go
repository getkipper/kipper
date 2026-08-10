package handlers

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

// Autoscale provides handlers for HPA management.
type Autoscale struct {
	Client   kubernetes.Interface
	CRClient crclient.Client
}

type autoscaleResponse struct {
	Enabled         bool   `json:"enabled"`
	MinReplicas     int32  `json:"min_replicas"`
	MaxReplicas     int32  `json:"max_replicas"`
	CPUTarget       int32  `json:"cpu_target"`
	MemoryTarget    int32  `json:"memory_target"`
	CurrentReplicas int32  `json:"current_replicas"`
	CurrentCPU      string `json:"current_cpu"`
	CurrentMemory   string `json:"current_memory"`
}

type autoscaleRequest struct {
	MinReplicas  int32 `json:"min_replicas"`
	MaxReplicas  int32 `json:"max_replicas"`
	CPUTarget    int32 `json:"cpu_target"`
	MemoryTarget int32 `json:"memory_target"`
}

// Get returns the current autoscaling config for an app.
// GET /api/v1/projects/{name}/apps/{app}/autoscale
func (a *Autoscale) Get(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	app := chi.URLParam(r, "app")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var appCR kipperv1.App
	if err := a.CRClient.Get(ctx, crclient.ObjectKey{Namespace: project, Name: app}, &appCR); err != nil {
		if errors.IsNotFound(err) {
			respondJSON(w, http.StatusOK, autoscaleResponse{Enabled: false})
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to get app")
		return
	}

	if appCR.Spec.Autoscale == nil || !appCR.Spec.Autoscale.Enabled {
		respondJSON(w, http.StatusOK, autoscaleResponse{Enabled: false})
		return
	}

	as := appCR.Spec.Autoscale
	resp := autoscaleResponse{
		Enabled:      true,
		MinReplicas:  as.MinReplicas,
		MaxReplicas:  as.MaxReplicas,
		CPUTarget:    as.CPUTarget,
		MemoryTarget: as.MemoryTarget,
	}

	// Read current metrics from the HPA status (created by the reconciler)
	hpa, err := a.Client.AutoscalingV2().HorizontalPodAutoscalers(project).Get(ctx, app, metav1.GetOptions{})
	if err == nil {
		resp.CurrentReplicas = hpa.Status.CurrentReplicas
		for _, status := range hpa.Status.CurrentMetrics {
			if status.Resource == nil || status.Resource.Current.AverageUtilization == nil {
				continue
			}
			switch status.Resource.Name {
			case "cpu":
				resp.CurrentCPU = fmt.Sprintf("%d%%", *status.Resource.Current.AverageUtilization)
			case "memory":
				resp.CurrentMemory = fmt.Sprintf("%d%%", *status.Resource.Current.AverageUtilization)
			}
		}
	}

	respondJSON(w, http.StatusOK, resp)
}

// Set creates or updates autoscaling for an app.
// PUT /api/v1/projects/{name}/apps/{app}/autoscale
func (a *Autoscale) Set(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	app := chi.URLParam(r, "app")

	var req autoscaleRequest
	if err := decodeJSON(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.MaxReplicas < 1 {
		respondError(w, http.StatusBadRequest, "max_replicas must be at least 1")
		return
	}

	if req.CPUTarget == 0 && req.MemoryTarget == 0 {
		respondError(w, http.StatusBadRequest, "specify at least cpu_target or memory_target")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var appCR kipperv1.App
	if err := a.CRClient.Get(ctx, crclient.ObjectKey{Namespace: project, Name: app}, &appCR); err != nil {
		respondError(w, http.StatusNotFound, fmt.Sprintf("app %q not found", app))
		return
	}

	appCR.Spec.Autoscale = &kipperv1.AppAutoscale{
		Enabled:      true,
		MinReplicas:  req.MinReplicas,
		MaxReplicas:  req.MaxReplicas,
		CPUTarget:    req.CPUTarget,
		MemoryTarget: req.MemoryTarget,
	}

	if err := a.CRClient.Update(ctx, &appCR); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to configure autoscaling")
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "enabled"})
}

// Delete removes autoscaling for an app.
// DELETE /api/v1/projects/{name}/apps/{app}/autoscale
func (a *Autoscale) Delete(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	app := chi.URLParam(r, "app")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var appCR kipperv1.App
	if err := a.CRClient.Get(ctx, crclient.ObjectKey{Namespace: project, Name: app}, &appCR); err != nil {
		if errors.IsNotFound(err) {
			respondJSON(w, http.StatusOK, map[string]string{"status": "disabled"})
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to get app")
		return
	}

	if appCR.Spec.Autoscale != nil {
		appCR.Spec.Autoscale.Enabled = false
	}

	if err := a.CRClient.Update(ctx, &appCR); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to disable autoscaling")
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "disabled"})
}
