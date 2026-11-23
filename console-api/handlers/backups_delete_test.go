package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/getkipper/kipper/controller/pkg/platform"
)

func veleroDeployment(replicas int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "velero", Namespace: "velero"},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
	}
}

func veleroBackup(name, phase string, labels map[string]interface{}) *unstructured.Unstructured {
	meta := map[string]interface{}{"name": name, "namespace": "velero", "uid": "uid-" + name}
	if labels != nil {
		meta["labels"] = labels
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "velero.io/v1", "kind": "Backup", "metadata": meta,
		"status": map[string]interface{}{"phase": phase},
	}}
}

func deletionRequest(backup, phase string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "velero.io/v1", "kind": "DeleteBackupRequest",
		"metadata": map[string]interface{}{"name": backup + "-abc12", "namespace": "velero", "creationTimestamp": "2026-11-22T09:00:00Z"},
		"spec":     map[string]interface{}{"backupName": backup},
		"status":   map[string]interface{}{"phase": phase},
	}}
}

// deleteBackup sends DELETE /backups/{name} to a fake cluster containing objects
// and a Velero Deployment with the given replica count. A negative count omits
// the Deployment.
func deleteBackup(t *testing.T, name string, replicas int32, objects ...*unstructured.Unstructured) (*httptest.ResponseRecorder, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	dyn := backupDynamic(platform.BackupExternal)
	for _, o := range objects {
		gvr := backupGVR
		switch o.GetKind() {
		case "DeleteBackupRequest":
			gvr = deleteBackupRequestGVR
		case "BackupStorageLocation":
			gvr = backupStorageLocationGVR
		}
		_, err := dyn.Resource(gvr).Namespace("velero").Create(context.Background(), o, metav1.CreateOptions{})
		require.NoError(t, err)
	}
	var typed []runtime.Object
	if replicas >= 0 {
		typed = append(typed, veleroDeployment(replicas))
	}
	h := &Backups{Client: fake.NewClientset(typed...), Dynamic: dyn}
	r := chi.NewRouter()
	r.Delete("/backups/{backup}", h.DeleteBackup)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("DELETE", "/backups/"+name, nil))
	return rec, dyn
}

func requests(t *testing.T, dyn *dynamicfake.FakeDynamicClient) []unstructured.Unstructured {
	t.Helper()
	list, err := dyn.Resource(deleteBackupRequestGVR).Namespace("velero").List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	return list.Items
}

func errorBody(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	return body["error"]
}

func TestDeleteBackupAsksVeleroToDeleteTheStoredData(t *testing.T) {
	rec, dyn := deleteBackup(t, "nightly-1", 1, veleroBackup("nightly-1", "Completed", nil), deletionRequest("other", "InProgress"))

	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	var created []unstructured.Unstructured
	for _, r := range requests(t, dyn) {
		if name, _, _ := unstructured.NestedString(r.Object, "spec", "backupName"); name == "nightly-1" {
			created = append(created, r)
		}
	}
	require.Len(t, created, 1)
	assert.Equal(t, "uid-nightly-1", created[0].GetLabels()["velero.io/backup-uid"])
	_, err := dyn.Resource(backupGVR).Namespace("velero").Get(context.Background(), "nightly-1", metav1.GetOptions{})
	assert.NoError(t, err, "Velero deletes the Backup once its data is gone")
}

func TestDeleteBackupRemovesABackupThatNeverStarted(t *testing.T) {
	rec, dyn := deleteBackup(t, "queued", 1, veleroBackup("queued", "New", nil))

	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	_, err := dyn.Resource(backupGVR).Namespace("velero").Get(context.Background(), "queued", metav1.GetOptions{})
	assert.Error(t, err)
	assert.Empty(t, requests(t, dyn))
}

func TestDeleteBackupRemovesAQueuedBackup(t *testing.T) {
	rec, dyn := deleteBackup(t, "queued", 1, veleroBackup("queued", "Queued", nil))

	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	_, err := dyn.Resource(backupGVR).Namespace("velero").Get(context.Background(), "queued", metav1.GetOptions{})
	assert.Error(t, err, "Velero has written nothing for a backup still waiting in its queue")
	assert.Empty(t, requests(t, dyn))
}

func TestDeleteBackupRefuses(t *testing.T) {
	readOnly := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "velero.io/v1", "kind": "BackupStorageLocation",
		"metadata": map[string]interface{}{"name": "default", "namespace": "velero"},
		"spec":     map[string]interface{}{"accessMode": "ReadOnly"},
	}}
	unavailable := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "velero.io/v1", "kind": "BackupStorageLocation",
		"metadata": map[string]interface{}{"name": "default", "namespace": "velero"},
		"spec":     map[string]interface{}{"accessMode": "ReadWrite"},
		"status":   map[string]interface{}{"phase": "Unavailable"},
	}}
	unchecked := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "velero.io/v1", "kind": "BackupStorageLocation",
		"metadata": map[string]interface{}{"name": "default", "namespace": "velero"},
		"spec":     map[string]interface{}{"accessMode": "ReadWrite"},
	}}
	ai := map[string]interface{}{"app.kubernetes.io/managed-by": "kipper", "app.kubernetes.io/part-of": "kipper-ai"}
	for name, tc := range map[string]struct {
		replicas int32
		objects  []*unstructured.Unstructured
		code     int
		want     string
	}{
		"a missing backup":        {replicas: 1, code: http.StatusNotFound, want: `backup "b1" not found`},
		"a running backup":        {replicas: 1, objects: []*unstructured.Unstructured{veleroBackup("b1", "InProgress", nil)}, code: http.StatusConflict, want: "still running backup"},
		"an AI bundle backup":     {replicas: 1, objects: []*unstructured.Unstructured{veleroBackup("b1", "Completed", ai)}, code: http.StatusConflict, want: "kip ai backup delete --name b1"},
		"a stopped Velero":        {replicas: 0, objects: []*unstructured.Unstructured{veleroBackup("b1", "Completed", nil)}, code: http.StatusConflict, want: "Velero is not running"},
		"no Velero":               {replicas: -1, objects: []*unstructured.Unstructured{veleroBackup("b1", "Completed", nil)}, code: http.StatusConflict, want: "Velero is not running"},
		"read-only storage":       {replicas: 1, objects: []*unstructured.Unstructured{veleroBackup("b1", "Completed", nil), readOnly}, code: http.StatusConflict, want: "read-only"},
		"a backup about to start": {replicas: 1, objects: []*unstructured.Unstructured{veleroBackup("b1", "ReadyToStart", nil)}, code: http.StatusConflict, want: "about to start"},
		"unavailable storage":     {replicas: 1, objects: []*unstructured.Unstructured{veleroBackup("b1", "Completed", nil), unavailable}, code: http.StatusConflict, want: "cannot reach"},
		"storage not checked yet": {replicas: 1, objects: []*unstructured.Unstructured{veleroBackup("b1", "Completed", nil), unchecked}, code: http.StatusConflict, want: "has not checked"},
		"a deletion under way":    {replicas: 1, objects: []*unstructured.Unstructured{veleroBackup("b1", "Deleting", nil), deletionRequest("b1", "New")}, code: http.StatusConflict, want: "to delete backup b1 and has not started"},
	} {
		t.Run(name, func(t *testing.T) {
			rec, dyn := deleteBackup(t, "b1", tc.replicas, tc.objects...)

			require.Equal(t, tc.code, rec.Code, rec.Body.String())
			assert.Contains(t, errorBody(t, rec), tc.want)
			for _, r := range requests(t, dyn) {
				assert.Equal(t, "b1-abc12", r.GetName(), "no deletion request is added")
			}
		})
	}
}

func TestDeleteBackupRetriesAFailedDeletion(t *testing.T) {
	failed := deletionRequest("b1", "Processed")
	require.NoError(t, unstructured.SetNestedStringSlice(failed.Object, []string{"error deleting backup from storage"}, "status", "errors"))

	rec, dyn := deleteBackup(t, "b1", 1, veleroBackup("b1", "Deleting", nil), failed)

	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	assert.Len(t, requests(t, dyn), 2)
}

// TestDeleteBackupRetriesADeletionAVeleroRestartLeftInProgress allows a retry
// because Velero skips requests left InProgress after a restart and removes them
// when processing a new request. Only pending requests should block a retry.
func TestDeleteBackupRetriesADeletionAVeleroRestartLeftInProgress(t *testing.T) {
	rec, dyn := deleteBackup(t, "b1", 1, veleroBackup("b1", "Deleting", nil), deletionRequest("b1", "InProgress"))

	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	assert.Len(t, requests(t, dyn), 2)
}

func TestDeleteBackupWaitsForARequestVeleroHasNotPickedUp(t *testing.T) {
	rec, _ := deleteBackup(t, "b1", 1, veleroBackup("b1", "Completed", nil), deletionRequest("b1", ""))

	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, errorBody(t, rec), "to delete backup b1 and has not started")
}
