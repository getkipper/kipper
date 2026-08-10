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
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

// Volumes provides handlers for shared volume management.
type Volumes struct {
	Client   kubernetes.Interface
	CRClient crclient.Client
}

type volumeMount struct {
	App  string `json:"app"`
	Path string `json:"path"`
}

type volumeResponse struct {
	Name   string        `json:"name"`
	Size   string        `json:"size"`
	Status string        `json:"status"`
	Access string        `json:"access"`
	Mounts []volumeMount `json:"mounts"`
}

type createVolumeRequest struct {
	Name string `json:"name"`
	Size string `json:"size"`
}

type mountVolumeRequest struct {
	VolumeName string `json:"volume_name"`
	AppName    string `json:"app_name"`
	MountPath  string `json:"mount_path"`
}

// List returns all shared volumes in a project namespace.
// GET /api/v1/projects/{name}/volumes
func (v *Volumes) List(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var volList kipperv1.VolumeList
	if err := v.CRClient.List(ctx, &volList, crclient.InNamespace(project)); err != nil {
		respondJSON(w, http.StatusOK, []volumeResponse{})
		return
	}

	result := make([]volumeResponse, 0, len(volList.Items))
	for _, vol := range volList.Items {
		status := strings.ToLower(vol.Status.Phase)
		if status == "" {
			status = "pending"
		}

		mounts := make([]volumeMount, 0, len(vol.Spec.Mounts))
		for _, m := range vol.Spec.Mounts {
			mounts = append(mounts, volumeMount{
				App:  m.App,
				Path: m.MountPath,
			})
		}

		result = append(result, volumeResponse{
			Name:   vol.Name,
			Size:   vol.Spec.Size,
			Status: status,
			Access: "ReadWriteMany",
			Mounts: mounts,
		})
	}

	respondJSON(w, http.StatusOK, result)
}

// Create creates a new shared volume.
// POST /api/v1/projects/{name}/volumes
func (v *Volumes) Create(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")

	var req createVolumeRequest
	if err := decodeJSON(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Name == "" || req.Size == "" {
		respondError(w, http.StatusBadRequest, "name and size are required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	vol := &kipperv1.Volume{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: project,
			Labels: map[string]string{
				kipperLabel:                kipperValue,
				"kipper.run/resource-type": "shared-volume",
				"kipper.run/volume-name":   req.Name,
			},
		},
		Spec: kipperv1.VolumeSpec{
			Size: req.Size,
		},
	}

	if err := v.CRClient.Create(ctx, vol); err != nil {
		if errors.IsAlreadyExists(err) {
			respondError(w, http.StatusConflict, fmt.Sprintf("volume %q already exists", req.Name))
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to create volume")
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "created", "name": req.Name})
}

// Delete removes a shared volume.
// DELETE /api/v1/projects/{name}/volumes/{vol}
func (v *Volumes) Delete(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	volName := chi.URLParam(r, "vol")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	vol := &kipperv1.Volume{
		ObjectMeta: metav1.ObjectMeta{
			Name:      volName,
			Namespace: project,
		},
	}

	if err := v.CRClient.Delete(ctx, vol); err != nil {
		if errors.IsNotFound(err) {
			respondError(w, http.StatusNotFound, fmt.Sprintf("volume %q not found", volName))
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to delete volume")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// Mount attaches a shared volume to an app.
// POST /api/v1/projects/{name}/volumes/mount
func (v *Volumes) Mount(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")

	var req mountVolumeRequest
	if err := decodeJSON(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.VolumeName == "" || req.AppName == "" || req.MountPath == "" {
		respondError(w, http.StatusBadRequest, "volume_name, app_name, and mount_path are required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	// Only the Volume CR is written: spec.mounts is the authoritative mount
	// list, and the volume reconciler propagates it into App.spec.volumes.
	// Writing the App here too would make the handler a second owner of the
	// rendered field, racing both the reconciler and other App-spec writers.
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		vol := &kipperv1.Volume{}
		if err := v.CRClient.Get(ctx, crclient.ObjectKey{Namespace: project, Name: req.VolumeName}, vol); err != nil {
			return err
		}
		vol.Spec.Mounts = setMountTarget(vol.Spec.Mounts, req.AppName, req.MountPath)
		return v.CRClient.Update(ctx, vol)
	})
	if errors.IsNotFound(err) {
		respondError(w, http.StatusNotFound, fmt.Sprintf("volume %q not found", req.VolumeName))
		return
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to mount volume")
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "mounted"})
}

// setMountTarget returns mounts with app mounted at mountPath: an existing
// entry moves to the new path, duplicates collapse into one, and a missing
// entry is appended.
func setMountTarget(mounts []kipperv1.VolumeMountTarget, app, mountPath string) []kipperv1.VolumeMountTarget {
	out := make([]kipperv1.VolumeMountTarget, 0, len(mounts)+1)
	seen := false
	for _, m := range mounts {
		if m.App == app {
			if seen {
				continue
			}
			seen = true
			m.MountPath = mountPath
		}
		out = append(out, m)
	}
	if !seen {
		out = append(out, kipperv1.VolumeMountTarget{App: app, MountPath: mountPath})
	}
	return out
}

// Unmount removes a shared volume from an app.
// POST /api/v1/projects/{name}/volumes/unmount
func (v *Volumes) Unmount(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")

	var req mountVolumeRequest
	if err := decodeJSON(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	// Single-writer model: only spec.mounts changes here, the volume
	// reconciler removes the entry from App.spec.volumes.
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		vol := &kipperv1.Volume{}
		if err := v.CRClient.Get(ctx, crclient.ObjectKey{Namespace: project, Name: req.VolumeName}, vol); err != nil {
			return err
		}
		var filtered []kipperv1.VolumeMountTarget
		for _, m := range vol.Spec.Mounts {
			if m.App != req.AppName {
				filtered = append(filtered, m)
			}
		}
		vol.Spec.Mounts = filtered
		return v.CRClient.Update(ctx, vol)
	})
	if errors.IsNotFound(err) {
		respondError(w, http.StatusNotFound, fmt.Sprintf("volume %q not found", req.VolumeName))
		return
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to unmount volume")
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "unmounted"})
}
