package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	modeConfigMapName      = "kipper-mode"
	modeConfigMapNamespace = "kipper-system"
	resourceLogConfigMap   = "kipper-resource-log"

	ModeAuto   = "auto"
	ModeExpert = "expert"
)

// Mode handles the cluster-wide resource management mode setting.
type Mode struct {
	Client kubernetes.Interface
}

type modeResponse struct {
	Mode string `json:"mode"`
}

type modeRequest struct {
	Mode string `json:"mode"`
}

// Get returns the current resource management mode.
// GET /api/v1/settings/mode
func (m *Mode) Get(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	mode := m.GetMode(ctx)
	respondJSON(w, http.StatusOK, modeResponse{Mode: mode})
}

// GetMode reads the current mode from the ConfigMap, defaulting to "auto".
func (m *Mode) GetMode(ctx context.Context) string {
	cm, err := m.Client.CoreV1().ConfigMaps(modeConfigMapNamespace).Get(ctx, modeConfigMapName, metav1.GetOptions{})
	if err != nil {
		return ModeAuto
	}

	mode, ok := cm.Data["mode"]
	if !ok || (mode != ModeAuto && mode != ModeExpert) {
		return ModeAuto
	}
	return mode
}

// Update sets the resource management mode.
// PUT /api/v1/settings/mode
func (m *Mode) Update(w http.ResponseWriter, r *http.Request) {
	var req modeRequest
	if err := decodeJSON(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Mode != ModeAuto && req.Mode != ModeExpert {
		respondError(w, http.StatusBadRequest, "mode must be \"auto\" or \"expert\"")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	cm, err := m.Client.CoreV1().ConfigMaps(modeConfigMapNamespace).Get(ctx, modeConfigMapName, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      modeConfigMapName,
				Namespace: modeConfigMapNamespace,
				Labels: map[string]string{
					"app.kubernetes.io/managed-by": "kipper",
				},
			},
			Data: map[string]string{"mode": req.Mode},
		}
		if _, err := m.Client.CoreV1().ConfigMaps(modeConfigMapNamespace).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
			respondError(w, http.StatusInternalServerError, "failed to create mode config")
			return
		}
		respondJSON(w, http.StatusOK, modeResponse(req))
		return
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to read mode config")
		return
	}

	if cm.Data == nil {
		cm.Data = make(map[string]string)
	}
	cm.Data["mode"] = req.Mode

	if _, err := m.Client.CoreV1().ConfigMaps(modeConfigMapNamespace).Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to update mode config")
		return
	}

	respondJSON(w, http.StatusOK, modeResponse(req))
}

// ResourceLogEntry represents a single auto-mode resource change.
type ResourceLogEntry struct {
	Time      string `json:"time"`
	App       string `json:"app"`
	Namespace string `json:"namespace"`
	Action    string `json:"action"`
	From      string `json:"from"`
	To        string `json:"to"`
	Reason    string `json:"reason"`
}

type resourceLogResponse struct {
	Entries []ResourceLogEntry `json:"entries"`
}

// GetResourceLog returns the last 50 auto-mode resource changes.
// GET /api/v1/settings/resource-log
func (m *Mode) GetResourceLog(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	cm, err := m.Client.CoreV1().ConfigMaps(modeConfigMapNamespace).Get(ctx, resourceLogConfigMap, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			respondJSON(w, http.StatusOK, resourceLogResponse{Entries: []ResourceLogEntry{}})
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to read resource log")
		return
	}

	logData, ok := cm.Data["entries"]
	if !ok {
		respondJSON(w, http.StatusOK, resourceLogResponse{Entries: []ResourceLogEntry{}})
		return
	}

	var entries []ResourceLogEntry
	if err := json.Unmarshal([]byte(logData), &entries); err != nil {
		respondJSON(w, http.StatusOK, resourceLogResponse{Entries: []ResourceLogEntry{}})
		return
	}

	// Only surface entries in projects the caller belongs to.
	kept := make([]ResourceLogEntry, 0, len(entries))
	for _, e := range entries {
		if canAccessNamespace(r, e.Namespace) {
			kept = append(kept, e)
		}
	}

	respondJSON(w, http.StatusOK, resourceLogResponse{Entries: kept})
}
