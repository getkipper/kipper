package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func makeMigrationJob(name, namespace, service string, createdAt metav1.Time) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         namespace,
			CreationTimestamp: createdAt,
			Labels: map[string]string{
				"kipper.run/migration": "true",
				"kipper.run/service":   service,
			},
		},
	}
}

func makeServiceCR(name, namespace, svcType, version string) *kipperv1.Service {
	return &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       kipperv1.ServiceSpec{Type: svcType, Version: version},
	}
}

func makeServiceCredentials(name, namespace string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-credentials", Namespace: namespace},
		Data: map[string][]byte{
			"HOST":     []byte(name + "." + namespace + ".svc.cluster.local"),
			"PORT":     []byte("5432"),
			"USERNAME": []byte("kipper"),
			"PASSWORD": []byte("hunter2"),
			"NAME":     []byte("app"),
		},
	}
}

func TestMigrateData_StartsPostgresJob(t *testing.T) {
	target := makeServiceCR("backend", "demo-prod", "postgres", "16-alpine")
	source := makeServiceCR("backend", "demo-test", "postgres", "16-alpine")
	srcCreds := makeServiceCredentials("backend", "demo-test")
	dstCreds := makeServiceCredentials("backend", "demo-prod")
	client := fake.NewClientset(srcCreds, dstCreds)
	handler := &Services{Client: client, CRClient: testCRClient(target, source)}

	r := chi.NewRouter()
	r.Post("/api/v1/services/{name}/migrate-data", handler.MigrateData)

	body := `{"source_namespace":"demo-test","confirm":"backend"}`
	req := httptest.NewRequest("POST", "/api/v1/services/backend/migrate-data?namespace=demo-prod", strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code, "body: %s", rec.Body.String())
	var resp map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Contains(t, resp["job_name"], "migrate-backend-")
	assert.Equal(t, "Pending", resp["phase"])

	jobs, _ := client.BatchV1().Jobs("demo-prod").List(req.Context(), metav1.ListOptions{})
	require.Len(t, jobs.Items, 1)
	job := jobs.Items[0]
	assert.Equal(t, "postgres:16-alpine", job.Spec.Template.Spec.Containers[0].Image)
	assert.Equal(t, "backend", job.Labels["kipper.run/service"])

	mirror, err := client.CoreV1().Secrets("demo-prod").Get(req.Context(), "backend-from-demo-test-credentials", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "hunter2", string(mirror.Data["PASSWORD"]))
}

func TestMigrateData_RejectsMissingConfirm(t *testing.T) {
	target := makeServiceCR("backend", "demo-prod", "postgres", "")
	handler := &Services{Client: fake.NewClientset(), CRClient: testCRClient(target)}

	r := chi.NewRouter()
	r.Post("/api/v1/services/{name}/migrate-data", handler.MigrateData)

	req := httptest.NewRequest("POST", "/api/v1/services/backend/migrate-data?namespace=demo-prod",
		strings.NewReader(`{"source_namespace":"demo-test","confirm":"wrong"}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestMigrateData_RejectsTypeMismatch(t *testing.T) {
	target := makeServiceCR("backend", "demo-prod", "postgres", "")
	source := makeServiceCR("backend", "demo-test", "mysql", "")
	handler := &Services{Client: fake.NewClientset(), CRClient: testCRClient(target, source)}

	r := chi.NewRouter()
	r.Post("/api/v1/services/{name}/migrate-data", handler.MigrateData)

	req := httptest.NewRequest("POST", "/api/v1/services/backend/migrate-data?namespace=demo-prod",
		strings.NewReader(`{"source_namespace":"demo-test","confirm":"backend"}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "type mismatch")
}

func TestMigrateData_RejectsUnsupportedType(t *testing.T) {
	target := makeServiceCR("cache", "demo-prod", "redis", "")
	source := makeServiceCR("cache", "demo-test", "redis", "")
	handler := &Services{Client: fake.NewClientset(), CRClient: testCRClient(target, source)}

	r := chi.NewRouter()
	r.Post("/api/v1/services/{name}/migrate-data", handler.MigrateData)

	req := httptest.NewRequest("POST", "/api/v1/services/cache/migrate-data?namespace=demo-prod",
		strings.NewReader(`{"source_namespace":"demo-test","confirm":"cache"}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
}

func TestMigrateData_RejectsSameSourceAndTarget(t *testing.T) {
	target := makeServiceCR("backend", "demo-prod", "postgres", "")
	handler := &Services{Client: fake.NewClientset(), CRClient: testCRClient(target)}

	r := chi.NewRouter()
	r.Post("/api/v1/services/{name}/migrate-data", handler.MigrateData)

	req := httptest.NewRequest("POST", "/api/v1/services/backend/migrate-data?namespace=demo-prod",
		strings.NewReader(`{"source_namespace":"demo-prod","confirm":"backend"}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestMigrateDataStatus_ReturnsLatestJob(t *testing.T) {
	now := metav1.Now()
	older := makeMigrationJob("migrate-backend-100", "demo-prod", "backend", metav1.NewTime(now.Add(-60*1e9)))
	newer := makeMigrationJob("migrate-backend-200", "demo-prod", "backend", now)
	client := fake.NewClientset(older, newer)
	handler := &Services{Client: client, CRClient: testCRClient()}

	r := chi.NewRouter()
	r.Get("/api/v1/services/{name}/migrate-data/status", handler.MigrateDataStatus)

	req := httptest.NewRequest("GET", "/api/v1/services/backend/migrate-data/status?namespace=demo-prod", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "migrate-backend-200", resp["job_name"], "must return the most recently created job")
}

func TestMigrateDataStatus_NoJobsReturnsEmptyPhase(t *testing.T) {
	handler := &Services{Client: fake.NewClientset(), CRClient: testCRClient()}
	r := chi.NewRouter()
	r.Get("/api/v1/services/{name}/migrate-data/status", handler.MigrateDataStatus)

	req := httptest.NewRequest("GET", "/api/v1/services/backend/migrate-data/status?namespace=demo-prod", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"phase":""`, "no jobs yet → empty phase")
}
