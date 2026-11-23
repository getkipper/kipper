package handlers

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/getkipper/kipper/controller/pkg/backupscope"
	"github.com/getkipper/kipper/controller/pkg/platform"
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

var platformConfigsGVR = schema.GroupVersionResource{Group: "kipper.run", Version: "v1alpha1", Resource: "platformconfigs"}

// refuseWithoutBackups returns true after sending a bad-request response when
// backups are disabled. Read failures allow the request to continue for
// compatibility with clusters that lack a readable PlatformConfig.
func (b *Backups) refuseWithoutBackups(w http.ResponseWriter, r *http.Request) bool {
	pc, err := b.Dynamic.Resource(platformConfigsGVR).Get(r.Context(), platformConfigName, metav1.GetOptions{})
	if err != nil {
		return false
	}
	backup, _, _ := unstructured.NestedString(pc.Object, "spec", "backup")
	if platform.BackupsConfigured(backup) {
		return false
	}
	respondError(w, http.StatusBadRequest, platform.BackupsNotConfigured)
	return true
}

var backupStorageLocationGVR = schema.GroupVersionResource{Group: "velero.io", Version: "v1", Resource: "backupstoragelocations"}

const backupStorageReadOnly = "This cluster's backup storage is read-only, as it is on recovery installs, so new backups cannot be saved there. Restores from this storage work as usual."

// refuseOnReadOnlyStorage returns true after sending a conflict response when
// Velero's default location is read-only, preventing a backup validation failure.
// Read failures allow the request to continue.
func (b *Backups) refuseOnReadOnlyStorage(w http.ResponseWriter, r *http.Request) bool {
	location, err := b.Dynamic.Resource(backupStorageLocationGVR).Namespace("velero").Get(r.Context(), "default", metav1.GetOptions{})
	if err != nil {
		return false
	}
	if mode, _, _ := unstructured.NestedString(location.Object, "spec", "accessMode"); mode != "ReadOnly" {
		return false
	}
	respondError(w, http.StatusConflict, backupStorageReadOnly)
	return true
}

type backupResponse struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Namespaces string `json:"namespaces"`
	Created    string `json:"created"`
	TTL        string `json:"ttl"`
	Reason     string `json:"reason"`
}

// backupStarted returns the backup's start time in the local time zone. If the
// start timestamp is missing or invalid, it returns the object's creation time
// unchanged. Using the start time preserves the backup's age when syncing from
// a bucket gives the object a new creation time.
func backupStarted(u unstructured.Unstructured) time.Time {
	if started, found, _ := unstructured.NestedString(u.Object, "status", "startTimestamp"); found {
		if t, err := time.Parse(time.RFC3339, started); err == nil {
			return t.Local()
		}
	}
	return u.GetCreationTimestamp().Time
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
			Created:    backupStarted(item).Format("2006-01-02 15:04"),
			TTL:        ttl,
			Reason:     reason,
		})
	}

	respondJSON(w, http.StatusOK, result)
}

// Create creates a new backup.
// POST /api/v1/backups
func (b *Backups) Create(w http.ResponseWriter, r *http.Request) {
	if b.refuseWithoutBackups(w, r) || b.refuseOnReadOnlyStorage(w, r) {
		return
	}
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
	backupscope.Backup(spec)

	if req.Namespaces != "" {
		spec["includedNamespaces"] = anySlice(strings.Split(req.Namespaces, ","))
	} else {
		spec["excludedNamespaces"] = anySlice(backupscope.ExcludedNamespaces())
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
	if b.refuseWithoutBackups(w, r) {
		return
	}
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

	spec := map[string]interface{}{"backupName": backupName}
	backupscope.Restore(spec, false)

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

var deleteBackupRequestGVR = schema.GroupVersionResource{Group: "velero.io", Version: "v1", Resource: "deletebackuprequests"}

// DeleteBackup asks Velero to delete a backup and its stored data, returning 202
// when the request is created. Backups with an empty, New, or Queued phase are
// deleted directly because data writes have not started, returning 204. It returns
// 409 for AI bundle backups, other unfinished phases, or concurrent changes during
// direct deletion. Requests to Velero also return 409 if its Deployment is missing
// or scaled to zero, storage is read-only or has a phase other than Available,
// or a deletion request for the backup is waiting for Velero to start.
// DELETE /api/v1/backups/{backup}
func (b *Backups) DeleteBackup(w http.ResponseWriter, r *http.Request) {
	if b.refuseWithoutBackups(w, r) {
		return
	}
	name := chi.URLParam(r, "backup")
	if name == "" {
		respondError(w, http.StatusBadRequest, "backup name is required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	backups := b.Dynamic.Resource(backupGVR).Namespace("velero")
	backup, err := backups.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		respondError(w, http.StatusNotFound, fmt.Sprintf("backup %q not found", name))
		return
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("reading backup %q: %v", name, err))
		return
	}
	if labels := backup.GetLabels(); labels["app.kubernetes.io/managed-by"] == "kipper" && labels["app.kubernetes.io/part-of"] == "kipper-ai" {
		respondError(w, http.StatusConflict, fmt.Sprintf("Backup %s belongs to the AI bundle. Run 'kip ai backup delete --name %s', which deletes the bundle's paired backups together.", name, strings.TrimSuffix(name, "-aiconfig")))
		return
	}
	phase, _, _ := unstructured.NestedString(backup.Object, "status", "phase")
	switch {
	case phase == "" || phase == "New" || phase == "Queued":
		// Preconditions prevent deletion if Velero starts the backup after our read.
		uid, version := backup.GetUID(), backup.GetResourceVersion()
		err := backups.Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &version}})
		if apierrors.IsConflict(err) {
			respondError(w, http.StatusConflict, fmt.Sprintf("Backup %s changed while it was being deleted, so Velero may have started it. Try again.", name))
			return
		}
		if err != nil && !apierrors.IsNotFound(err) {
			respondError(w, http.StatusInternalServerError, fmt.Sprintf("deleting backup %q: %v", name, err))
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	case phase == "ReadyToStart":
		respondError(w, http.StatusConflict, fmt.Sprintf("Velero is about to start backup %s. Delete it once it completes or fails.", name))
		return
	case backupscope.Unfinished(phase):
		respondError(w, http.StatusConflict, fmt.Sprintf("Velero is still running backup %s (phase %s). Delete it once it completes or fails.", name, phase))
		return
	}
	if reason, err := b.deletionRefused(ctx, backup); err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	} else if reason != "" {
		respondError(w, http.StatusConflict, reason)
		return
	}

	requests := b.Dynamic.Resource(deleteBackupRequestGVR).Namespace("velero")
	list, err := requests.List(ctx, metav1.ListOptions{})
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("reading Velero's deletion requests: %v", err))
		return
	}
	for _, req := range list.Items {
		target, _, _ := unstructured.NestedString(req.Object, "spec", "backupName")
		reqPhase, _, _ := unstructured.NestedString(req.Object, "status", "phase")
		// Velero skips requests left InProgress after a restart and removes them
		// when processing a new request. Only pending requests should block a retry.
		if target == name && (reqPhase == "" || reqPhase == "New") {
			respondError(w, http.StatusConflict, fmt.Sprintf("Velero received a request at %s to delete backup %s and has not started it yet. Try again in a minute.", req.GetCreationTimestamp().UTC().Format("2006-01-02 15:04 MST"), name))
			return
		}
	}

	// Let Velero add and shorten the backup-name label to match its selectors.
	req := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "velero.io/v1",
		"kind":       "DeleteBackupRequest",
		"metadata": map[string]any{
			"generateName": name + "-",
			"namespace":    "velero",
			"labels":       map[string]any{"velero.io/backup-uid": string(backup.GetUID())},
		},
		"spec": map[string]any{"backupName": name},
	}}
	if _, err := requests.Create(ctx, req, metav1.CreateOptions{}); err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("asking Velero to delete backup %q: %v", name, err))
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// deletionRefused returns a reason if Velero's Deployment is missing or scaled to
// zero, or the backup's storage location is read-only or has a phase other than
// Available. An empty reason and nil error allow deletion. Storage location read
// errors are left for Velero to handle. Other Deployment read errors are returned.
func (b *Backups) deletionRefused(ctx context.Context, backup *unstructured.Unstructured) (string, error) {
	name := backup.GetName()
	deploy, err := b.Client.AppsV1().Deployments("velero").Get(ctx, "velero", metav1.GetOptions{})
	stopped := apierrors.IsNotFound(err)
	if err != nil && !stopped {
		return "", fmt.Errorf("reading the Velero Deployment: %w", err)
	}
	if !stopped && deploy.Spec.Replicas != nil {
		stopped = *deploy.Spec.Replicas == 0
	}
	if stopped {
		return fmt.Sprintf("Backup %s cannot be deleted now because Velero is not running. kip stops Velero while it moves the backup storage or resets the repository key. Try again once that finishes.", name), nil
	}
	location, _, _ := unstructured.NestedString(backup.Object, "spec", "storageLocation")
	if location == "" {
		location = "default"
	}
	bsl, err := b.Dynamic.Resource(backupStorageLocationGVR).Namespace("velero").Get(ctx, location, metav1.GetOptions{})
	if err == nil {
		if mode, _, _ := unstructured.NestedString(bsl.Object, "spec", "accessMode"); mode == "ReadOnly" {
			return fmt.Sprintf("Backup %s is in backup storage location %s, which is read-only. Recovery installs use this mode. Velero cannot delete data from read-only storage.", name, location), nil
		}
		// Velero deletes only from a location it has validated as Available.
		switch phase, _, _ := unstructured.NestedString(bsl.Object, "status", "phase"); phase {
		case "Available":
		case "":
			return fmt.Sprintf("Velero has not checked backup storage location %s yet, so it would refuse to delete backup %s. Try again in a minute.", location, name), nil
		default:
			return fmt.Sprintf("Velero cannot reach backup storage location %s right now, so it would refuse to delete backup %s. Check the storage's address and credentials, then try again.", location, name), nil
		}
	}
	return "", nil
}

// ToggleSchedule enables or disables a backup schedule.
// PUT /api/v1/backups/schedules/{schedule}
func (b *Backups) ToggleSchedule(w http.ResponseWriter, r *http.Request) {
	if b.refuseWithoutBackups(w, r) {
		return
	}
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

// anySlice converts a string slice into the []interface{} form unstructured
// objects need to deep-copy.
func anySlice(items []string) []interface{} {
	out := make([]interface{}, len(items))
	for i, item := range items {
		out[i] = item
	}
	return out
}
