package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/getkipper/kipper/controller/pkg/platform"
)

func backupDynamic(backup string) *dynamicfake.FakeDynamicClient {
	spec := map[string]interface{}{"profile": "nano", "storage": "local"}
	if backup != "" {
		spec["backup"] = backup
	}
	pc := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1", "kind": "PlatformConfig",
		"metadata": map[string]interface{}{"name": "platform"}, "spec": spec,
	}}
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		platformConfigsGVR: "PlatformConfigList", backupGVR: "BackupList", restoreGVR: "RestoreList", scheduleGVR: "ScheduleList",
	}, pc)
}

// TestBackupsRefuseWritesWithoutBackups checks that writes explain how to enable backups.
func TestBackupsRefuseWritesWithoutBackups(t *testing.T) {
	h := &Backups{Client: fake.NewClientset(), Dynamic: backupDynamic(platform.BackupNone)}
	r := chi.NewRouter()
	r.Post("/backups", h.Create)
	r.Post("/backups/{backup}/restore", h.Restore)
	r.Delete("/backups/{backup}", h.DeleteBackup)
	r.Put("/backups/schedules/{schedule}", h.ToggleSchedule)
	for _, req := range []*http.Request{
		httptest.NewRequest("POST", "/backups", strings.NewReader(`{}`)),
		httptest.NewRequest("POST", "/backups/b1/restore", strings.NewReader(`{}`)),
		httptest.NewRequest("DELETE", "/backups/b1", nil),
		httptest.NewRequest("PUT", "/backups/schedules/daily", strings.NewReader(`{"paused":true}`)),
	} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		require.Equal(t, http.StatusBadRequest, rec.Code, req.Method+" "+req.URL.Path)
		var body map[string]string
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.Equal(t, platform.BackupsNotConfigured, body["error"])
	}
}

func TestBackupsCreateOnALegacyCluster(t *testing.T) {
	h := &Backups{Client: fake.NewClientset(), Dynamic: backupDynamic("")}
	r := chi.NewRouter()
	r.Post("/backups", h.Create)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("POST", "/backups", strings.NewReader(`{}`)))
	require.Equal(t, http.StatusOK, rec.Code, "a cluster recorded before the backup mode runs Velero: %s", rec.Body.String())
	list, err := h.Dynamic.Resource(backupGVR).Namespace("velero").List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, list.Items, 1, "the Velero backup is created")
}

func createdSpec(t *testing.T, h *Backups, gvr schema.GroupVersionResource) map[string]interface{} {
	t.Helper()
	list, err := h.Dynamic.Resource(gvr).Namespace("velero").List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, list.Items, 1)
	return list.Items[0].Object["spec"].(map[string]interface{})
}

func TestBackupsCreateSharesTheBackupSelection(t *testing.T) {
	for body, wantExcludedNamespaces := range map[string]bool{`{}`: true, `{"namespaces":"shop,blog"}`: false} {
		h := &Backups{Client: fake.NewClientset(), Dynamic: backupDynamic(platform.BackupInCluster)}
		r := chi.NewRouter()
		r.Post("/backups", h.Create)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest("POST", "/backups", strings.NewReader(body)))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		spec := createdSpec(t, h, backupGVR)
		assert.Contains(t, spec["includedClusterScopedResources"], "projects.kipper.run", body)
		assert.Contains(t, spec["excludedNamespaceScopedResources"], "orders.acme.cert-manager.io", body)
		if wantExcludedNamespaces {
			// Excluding velero prevents backups from hanging while reading their own MinIO storage.
			assert.Contains(t, spec["excludedNamespaces"], "velero", body)
		} else {
			assert.Equal(t, []interface{}{"shop", "blog"}, spec["includedNamespaces"], body)
			assert.NotContains(t, spec, "excludedNamespaces", body)
		}
	}
}

func TestBackupsRestoreLeavesKipperClusterObjectsOut(t *testing.T) {
	h := &Backups{Client: fake.NewClientset(), Dynamic: backupDynamic(platform.BackupInCluster)}
	r := chi.NewRouter()
	r.Post("/backups/{backup}/restore", h.Restore)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("POST", "/backups/b1/restore", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	spec := createdSpec(t, h, restoreGVR)
	assert.Contains(t, spec["excludedResources"], "projects.kipper.run")
	assert.Contains(t, spec["excludedResources"], "clusteridentities.kipper.run")
	assert.Contains(t, spec["excludedResources"], "orders.acme.cert-manager.io")
	assert.NotContains(t, spec, "includeClusterResources")
}
