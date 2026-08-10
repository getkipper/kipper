package handlers

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

var backupGVR = schema.GroupVersionResource{
	Group: "velero.io", Version: "v1", Resource: "backups",
}
var restoreGVR = schema.GroupVersionResource{
	Group: "velero.io", Version: "v1", Resource: "restores",
}
var scheduleGVR = schema.GroupVersionResource{
	Group: "velero.io", Version: "v1", Resource: "schedules",
}

// Backups provides handlers for backup management.
type Backups struct {
	Client  kubernetes.Interface
	Dynamic dynamic.Interface
}

type backupResponse struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Namespaces string `json:"namespaces"`
	Created    string `json:"created"`
	TTL        string `json:"ttl"`
	Reason     string `json:"reason"`
}

type scheduleResponse struct {
	Name       string `json:"name"`
	Schedule   string `json:"schedule"`
	Status     string `json:"status"`
	LastBackup string `json:"last_backup"`
}

// List returns all backups.
// GET /api/v1/backups
func (b *Backups) List(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	backups, err := b.Dynamic.Resource(backupGVR).Namespace("velero").List(ctx, metav1.ListOptions{})
	if err != nil {
		respondJSON(w, http.StatusOK, []backupResponse{})
		return
	}

	// Check Velero pod for OOM or crash info to enrich failure messages
	podTerminationReason := ""
	pods, podErr := b.Client.CoreV1().Pods("velero").List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=velero",
	})
	if podErr == nil {
		for _, pod := range pods.Items {
			for _, cs := range pod.Status.ContainerStatuses {
				if cs.LastTerminationState.Terminated != nil {
					podTerminationReason = cs.LastTerminationState.Terminated.Reason
				}
			}
		}
	}

	result := make([]backupResponse, 0)
	for _, item := range backups.Items {
		status := "Unknown"
		if s, ok, _ := unstructured.NestedString(item.Object, "status", "phase"); ok {
			status = s
		}

		namespaces := "all"
		if ns, ok, _ := unstructured.NestedStringSlice(item.Object, "spec", "includedNamespaces"); ok && len(ns) > 0 {
			namespaces = strings.Join(ns, ", ")
		}

		ttl := ""
		if t, ok, _ := unstructured.NestedString(item.Object, "spec", "ttl"); ok {
			ttl = t
		}

		reason := ""
		if r, ok, _ := unstructured.NestedString(item.Object, "status", "failureReason"); ok {
			reason = r
			if strings.Contains(reason, "InProgress") && strings.Contains(reason, "server starting") && podTerminationReason == "OOMKilled" {
				reason = "Backup failed: Velero ran out of memory (OOMKilled). Increase the memory limit in the Velero Helm chart values."
			} else if strings.Contains(reason, "InProgress") && strings.Contains(reason, "server starting") && podTerminationReason != "" {
				reason = fmt.Sprintf("%s (pod terminated: %s)", reason, podTerminationReason)
			}
		}

		result = append(result, backupResponse{
			Name:       item.GetName(),
			Status:     status,
			Namespaces: namespaces,
			Created:    item.GetCreationTimestamp().Format("2006-01-02 15:04"),
			TTL:        ttl,
			Reason:     reason,
		})
	}

	respondJSON(w, http.StatusOK, result)
}

// Create creates a new backup.
// POST /api/v1/backups
func (b *Backups) Create(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name       string `json:"name"`
		Namespaces string `json:"namespaces"`
		TTL        string `json:"ttl"`
	}
	if err := decodeJSON(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Name == "" {
		req.Name = "manual-" + time.Now().Format("20060102-150405")
	}
	if req.TTL == "" {
		req.TTL = "168h0m0s"
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	spec := map[string]interface{}{
		"ttl":                      req.TTL,
		"storageLocation":          "default",
		"defaultVolumesToFsBackup": true,
	}

	if req.Namespaces != "" {
		spec["includedNamespaces"] = strings.Split(req.Namespaces, ",")
	}

	backup := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "velero.io/v1",
			"kind":       "Backup",
			"metadata": map[string]interface{}{
				"name":      req.Name,
				"namespace": "velero",
			},
			"spec": spec,
		},
	}

	_, err := b.Dynamic.Resource(backupGVR).Namespace("velero").Create(ctx, backup, metav1.CreateOptions{})
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to create backup: %v", err))
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "created", "name": req.Name})
}

// Restore creates a restore from a backup.
// POST /api/v1/backups/{name}/restore
func (b *Backups) Restore(w http.ResponseWriter, r *http.Request) {
	backupName := r.PathValue("name")
	if backupName == "" {
		// Chi uses URLParam
		backupName = r.URL.Query().Get("name")
	}

	// Try to get backup name from chi
	// The route is /backups/{backup}/restore
	parts := strings.Split(r.URL.Path, "/")
	for i, p := range parts {
		if p == "backups" && i+1 < len(parts) {
			backupName = parts[i+1]
			break
		}
	}

	if backupName == "" || backupName == "restore" {
		respondError(w, http.StatusBadRequest, "backup name is required")
		return
	}

	var req struct {
		Namespace string `json:"namespace"`
		Resources string `json:"resources"`
	}
	// Body is optional — empty body means restore everything
	_ = decodeJSON(r, &req)

	restoreName := "restore-" + backupName + "-" + time.Now().Format("150405")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	spec := map[string]interface{}{
		"backupName": backupName,
	}

	if req.Namespace != "" {
		spec["includedNamespaces"] = []interface{}{req.Namespace}
	}
	if req.Resources != "" {
		var resList []interface{}
		for _, r := range strings.Split(req.Resources, ",") {
			resList = append(resList, strings.TrimSpace(r))
		}
		spec["includedResources"] = resList
	}

	restore := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "velero.io/v1",
			"kind":       "Restore",
			"metadata": map[string]interface{}{
				"name":      restoreName,
				"namespace": "velero",
			},
			"spec": spec,
		},
	}

	_, err := b.Dynamic.Resource(restoreGVR).Namespace("velero").Create(ctx, restore, metav1.CreateOptions{})
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to create restore")
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "restoring", "name": restoreName})
}

// Schedules returns all backup schedules.
// GET /api/v1/backups/schedules
func (b *Backups) Schedules(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	schedules, err := b.Dynamic.Resource(scheduleGVR).Namespace("velero").List(ctx, metav1.ListOptions{})
	if err != nil {
		respondJSON(w, http.StatusOK, []scheduleResponse{})
		return
	}

	result := make([]scheduleResponse, 0)
	for _, item := range schedules.Items {
		schedule, _, _ := unstructured.NestedString(item.Object, "spec", "schedule")
		phase := "Enabled"
		if paused, ok, _ := unstructured.NestedBool(item.Object, "spec", "paused"); ok && paused {
			phase = "Paused"
		}
		lastBackup := "never"
		if lb, ok, _ := unstructured.NestedString(item.Object, "status", "lastBackup"); ok {
			lastBackup = lb
		}

		result = append(result, scheduleResponse{
			Name:       item.GetName(),
			Schedule:   schedule,
			Status:     phase,
			LastBackup: lastBackup,
		})
	}

	respondJSON(w, http.StatusOK, result)
}

// DeleteBackup removes a backup.
// DELETE /api/v1/backups/{backup}
func (b *Backups) DeleteBackup(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(r.URL.Path, "/")
	backupName := ""
	for i, p := range parts {
		if p == "backups" && i+1 < len(parts) && parts[i+1] != "schedules" {
			backupName = parts[i+1]
			break
		}
	}

	if backupName == "" {
		respondError(w, http.StatusBadRequest, "backup name is required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	if err := b.Dynamic.Resource(backupGVR).Namespace("velero").Delete(ctx, backupName, metav1.DeleteOptions{}); err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to delete backup: %v", err))
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ToggleSchedule enables or disables a backup schedule.
// PUT /api/v1/backups/schedules/{schedule}
func (b *Backups) ToggleSchedule(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(r.URL.Path, "/")
	scheduleName := ""
	for i, p := range parts {
		if p == "schedules" && i+1 < len(parts) {
			scheduleName = parts[i+1]
			break
		}
	}

	if scheduleName == "" {
		respondError(w, http.StatusBadRequest, "schedule name is required")
		return
	}

	var req struct {
		Paused bool `json:"paused"`
	}
	if err := decodeJSON(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	schedule, err := b.Dynamic.Resource(scheduleGVR).Namespace("velero").Get(ctx, scheduleName, metav1.GetOptions{})
	if err != nil {
		respondError(w, http.StatusNotFound, fmt.Sprintf("schedule %q not found", scheduleName))
		return
	}

	_ = unstructured.SetNestedField(schedule.Object, req.Paused, "spec", "paused")

	if _, err = b.Dynamic.Resource(scheduleGVR).Namespace("velero").Update(ctx, schedule, metav1.UpdateOptions{}); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to update schedule")
		return
	}

	status := "enabled"
	if req.Paused {
		status = "paused"
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": status})
}
