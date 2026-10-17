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
