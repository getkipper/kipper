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
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func rollbackApp(history []deployEntry) *kipperv1.App {
	data, _ := json.Marshal(history)
	return &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "api",
			Namespace:   "blog",
			Annotations: map[string]string{historyAnnotation: string(data)},
		},
		Spec: kipperv1.AppSpec{Image: "api:v2"},
	}
}

func doRollback(t *testing.T, h *Webhooks, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	r.Post("/projects/{name}/apps/{app}/rollback", h.Rollback)
	req := httptest.NewRequest(http.MethodPost, "/projects/blog/apps/api/rollback", strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestRollback_ToPreviousSetsImage(t *testing.T) {
	app := rollbackApp([]deployEntry{
		{Revision: 2, Image: "api:v2", Trigger: "webhook"},
		{Revision: 1, Image: "api:v1", Trigger: "manual"},
	})
	cr := testCRClient(app)
	h := &Webhooks{Client: fake.NewClientset(), CRClient: cr}

	rec := doRollback(t, h, `{}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var got kipperv1.App
	require.NoError(t, cr.Get(context.Background(), crclient.ObjectKey{Namespace: "blog", Name: "api"}, &got))
	assert.Equal(t, "api:v1", got.Spec.Image, "rollback must set spec.image to the previous revision")
}

func TestRollback_MissingExplicitRevisionErrors(t *testing.T) {
	app := rollbackApp([]deployEntry{
		{Revision: 2, Image: "api:v2"},
		{Revision: 1, Image: "api:v1"},
	})
	cr := testCRClient(app)
	h := &Webhooks{Client: fake.NewClientset(), CRClient: cr}

	rec := doRollback(t, h, `{"revision":9}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "9", "a missing explicit revision must be named, not silently fall back to previous")

	var got kipperv1.App
	require.NoError(t, cr.Get(context.Background(), crclient.ObjectKey{Namespace: "blog", Name: "api"}, &got))
	assert.Equal(t, "api:v2", got.Spec.Image, "a failed rollback must not change the image")
}

func TestRollback_DeletesPendingBuildJob(t *testing.T) {
	app := rollbackApp([]deployEntry{
		{Revision: 2, Image: "api:v2"},
		{Revision: 1, Image: "api:v1"},
	})
	// A build Job that has not started a pod yet (Active == 0). It must be
	// deleted so it can't succeed later and overwrite the rolled-back image.
	// Builds run in the shared build namespace, scoped by the source-namespace
	// label to the app's tenant namespace.
	pendingJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "api-build-abc",
			Namespace: "kipper-builds",
			Labels: map[string]string{
				"kipper.run/app":              "api",
				"kipper.run/build":            "true",
				"kipper.run/source-namespace": "blog",
			},
		},
	}
	client := fake.NewClientset(pendingJob)
	h := &Webhooks{Client: client, CRClient: testCRClient(app)}

	rec := doRollback(t, h, `{}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	_, err := client.BatchV1().Jobs("kipper-builds").Get(context.Background(), "api-build-abc", metav1.GetOptions{})
	assert.Error(t, err, "rollback must delete the pending build job so it can't overwrite the rolled-back image")
}

func TestRollback_NoPreviousVersion(t *testing.T) {
	app := rollbackApp([]deployEntry{{Revision: 1, Image: "api:v1"}})
	h := &Webhooks{Client: fake.NewClientset(), CRClient: testCRClient(app)}

	rec := doRollback(t, h, `{}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "no previous version")
}
