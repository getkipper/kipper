package handlers

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/middleware"
)

type createServiceRequest struct {
	Name          string `json:"name"`
	Type          string `json:"type"`
	Namespace     string `json:"namespace"`
	Storage       string `json:"storage"`
	CPURequest    string `json:"cpu_request"`
	CPULimit      string `json:"cpu_limit"`
	MemoryRequest string `json:"memory_request"`
	MemoryLimit   string `json:"memory_limit"`
}

// supportedServiceTypes lists valid service types for validation.
var supportedServiceTypes = map[string]bool{
	"postgres":   true,
	"mysql":      true,
	"redis":      true,
	"mongodb":    true,
	"rabbitmq":   true,
	"opensearch": true,
	"minio":      true,
	"mailhog":    true,
}

// Create deploys a new stateful service.
// POST /api/v1/services
func (s *Services) Create(w http.ResponseWriter, r *http.Request) {
	var req createServiceRequest
	if err := decodeJSON(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Name == "" || req.Type == "" {
		respondError(w, http.StatusBadRequest, "name and type are required")
		return
	}
	if req.Namespace == "" {
		req.Namespace = "default"
	}
	if !enforceProjectRole(w, r, req.Namespace, middleware.ProjectRoleDeployer) {
		return
	}

	if !supportedServiceTypes[req.Type] {
		respondError(w, http.StatusBadRequest, fmt.Sprintf("unsupported service type %q", req.Type))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	storage := req.Storage
	if storage == "" {
		storage = "1Gi"
	}

	svc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: req.Namespace,
			Labels: map[string]string{
				"app":                     req.Name,
				kipperLabel:               kipperValue,
				"kipper.run/service-type": req.Type,
			},
		},
		Spec: kipperv1.ServiceSpec{
			Type:    req.Type,
			Storage: storage,
			Resources: kipperv1.ServiceResources{
				CPURequest:    req.CPURequest,
				CPULimit:      req.CPULimit,
				MemoryRequest: req.MemoryRequest,
				MemoryLimit:   req.MemoryLimit,
			},
		},
	}

	if err := s.CRClient.Create(ctx, svc); err != nil {
		if errors.IsAlreadyExists(err) {
			respondError(w, http.StatusConflict, fmt.Sprintf("service %q already exists", req.Name))
			return
		}
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to create service: %v", err))
		return
	}

	svcHost := fmt.Sprintf("%s.%s.svc.cluster.local", req.Name, req.Namespace)
	respondJSON(w, http.StatusOK, map[string]string{
		"status": "created",
		"name":   req.Name,
		"type":   req.Type,
		"host":   svcHost,
	})
}

// Types returns the list of supported service types.
// GET /api/v1/service-types
func (s *Services) Types(w http.ResponseWriter, _ *http.Request) {
	types := make([]map[string]string, 0)
	for name := range supportedServiceTypes {
		types = append(types, map[string]string{
			"name": name,
		})
	}
	respondJSON(w, http.StatusOK, types)
}
