package handlers

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

// Functions provides handlers for serverless function management.
type Functions struct {
	Client   kubernetes.Interface
	Dynamic  dynamic.Interface
	CRClient crclient.Client
}

type functionResponse struct {
	Name    string `json:"name"`
	Trigger string `json:"trigger"`
	Image   string `json:"image"`
	Ready   int32  `json:"ready"`
	URL     string `json:"url"`
	Status  string `json:"status"`
}

type createFunctionRequest struct {
	Name        string `json:"name"`
	Image       string `json:"image"`
	Port        int32  `json:"port"`
	Trigger     string `json:"trigger"`
	Schedule    string `json:"schedule"`
	IdleTimeout int    `json:"idle_timeout"`
	Source      string `json:"source"`
	Query       string `json:"query"`
	MarkDone    string `json:"mark_done"`
	RedisList   string `json:"redis_list"`
	Bucket      string `json:"bucket"`
}

// List returns all functions in a project namespace.
// GET /api/v1/projects/{name}/functions
func (f *Functions) List(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var fnList kipperv1.FunctionList
	if err := f.CRClient.List(ctx, &fnList, crclient.InNamespace(project)); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list functions")
		return
	}

	result := make([]functionResponse, 0, len(fnList.Items))
	for _, fn := range fnList.Items {

		status := strings.ToLower(fn.Status.Phase)
		if status == "" {
			status = "idle"
		}

		trigger := "http"
		if len(fn.Spec.Triggers) > 0 {
			trigger = fn.Spec.Triggers[0].Type
		}

		image := fn.Spec.Image
		if fn.Spec.Runtime != "" && fn.Spec.Runtime != "custom" {
			switch fn.Spec.Runtime {
			case "node":
				image = "inline (Node.js)"
			case "python":
				image = "inline (Python)"
			default:
				image = "inline (" + fn.Spec.Runtime + ")"
			}
		}

		fnURL := fn.Status.Endpoint

		result = append(result, functionResponse{
			Name:    fn.Name,
			Trigger: trigger,
			Image:   image,
			Ready:   fn.Status.Replicas,
			Status:  status,
			URL:     fnURL,
		})
	}

	respondJSON(w, http.StatusOK, result)
}

// Create creates a new function with a KEDA ScaledObject.
// POST /api/v1/projects/{name}/functions
func (f *Functions) Create(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")

	var req createFunctionRequest
	if err := decodeJSON(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Name == "" || req.Image == "" {
		respondError(w, http.StatusBadRequest, "name and image are required")
		return
	}
	if req.Port == 0 {
		req.Port = 8080
	}
	if req.Trigger == "" {
		req.Trigger = "http"
	}
	if req.IdleTimeout == 0 {
		req.IdleTimeout = 300
	}
	if req.Trigger == "cron" && req.Schedule == "" {
		respondError(w, http.StatusBadRequest, "schedule is required for cron triggers (e.g. \"0 2 * * *\")")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	triggers := []kipperv1.FunctionTrigger{
		{Type: req.Trigger, Schedule: req.Schedule},
	}

	// Populate trigger config from request fields
	triggerConfig := make(map[string]string)
	if req.IdleTimeout != 300 {
		triggerConfig["idleTimeout"] = fmt.Sprintf("%d", req.IdleTimeout)
	}
	if req.Source != "" {
		triggerConfig["source"] = req.Source
	}
	if req.Query != "" {
		triggerConfig["query"] = req.Query
	}
	if req.MarkDone != "" {
		triggerConfig["markDone"] = req.MarkDone
	}
	if req.RedisList != "" {
		triggerConfig["redisList"] = req.RedisList
	}
	if req.Bucket != "" {
		triggerConfig["bucket"] = req.Bucket
	}
	if len(triggerConfig) > 0 {
		triggers[0].Config = triggerConfig
	}

	fn := &kipperv1.Function{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: project,
			Labels: map[string]string{
				"app":                      req.Name,
				kipperLabel:                kipperValue,
				"kipper.run/resource-type": "function",
				"kipper.run/trigger":       req.Trigger,
			},
		},
		Spec: kipperv1.FunctionSpec{
			Image:    req.Image,
			Port:     req.Port,
			Triggers: triggers,
		},
	}

	release, ok := reserveWorkloadName(ctx, w, f.CRClient, project, req.Name, "function")
	if !ok {
		return
	}

	if err := f.CRClient.Create(ctx, fn); err != nil {
		// See Apps.Create: AlreadyExists proves the workload is there, so the
		// reservation just made is its own first claim and must stand.
		if !errors.IsAlreadyExists(err) {
			release()
		}
		if errors.IsAlreadyExists(err) {
			respondError(w, http.StatusConflict, fmt.Sprintf("function %q already exists", req.Name))
			return
		}
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to create function: %v", err))
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "created", "name": req.Name})
}

// Delete removes a function and its associated resources.
// DELETE /api/v1/projects/{name}/functions/{fn}
func (f *Functions) Delete(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	fnName := chi.URLParam(r, "fn")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	fn := &kipperv1.Function{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fnName,
			Namespace: project,
		},
	}

	if err := f.CRClient.Delete(ctx, fn); err != nil {
		if errors.IsNotFound(err) {
			respondError(w, http.StatusNotFound, fmt.Sprintf("function %q not found", fnName))
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to delete function")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
