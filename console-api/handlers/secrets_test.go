package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestSecretsSetStampsChange(t *testing.T) {
	client := fake.NewClientset()
	s := &Secrets{Client: client}

	body, _ := json.Marshal(map[string]string{"API_KEY": "s3cr3t"})
	r := chi.NewRouter()
	r.Put("/projects/{name}/apps/{app}/secrets", s.Set)
	req := httptest.NewRequest(http.MethodPut, "/projects/blog/apps/api/secrets", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	secret, err := client.CoreV1().Secrets("blog").Get(t.Context(), "app-api-secrets", metav1.GetOptions{})
	require.NoError(t, err)
	assert.NotEmpty(t, secret.Annotations[dataUpdatedAtAnnotation], "setting a secret must stamp the change so the restart banner covers secret edits")
}

func TestSecretsDeleteStampsChange(t *testing.T) {
	seeded := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "app-api-secrets", Namespace: "blog"},
		Data:       map[string][]byte{"API_KEY": []byte("s3cr3t")},
	}
	client := fake.NewClientset(seeded)
	s := &Secrets{Client: client}

	r := chi.NewRouter()
	r.Delete("/projects/{name}/apps/{app}/secrets/{key}", s.Delete)
	req := httptest.NewRequest(http.MethodDelete, "/projects/blog/apps/api/secrets/API_KEY", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code)

	secret, err := client.CoreV1().Secrets("blog").Get(t.Context(), "app-api-secrets", metav1.GetOptions{})
	require.NoError(t, err)
	assert.NotEmpty(t, secret.Annotations[dataUpdatedAtAnnotation], "deleting a secret changes what pods read; it must stamp the change")
}
