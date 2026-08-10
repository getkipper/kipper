package handlers

import (
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

func buildPodInBuildsNS() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "api-build-abc123",
			Namespace: "kipper-builds",
			Labels: map[string]string{
				"kipper.run/build":            "true",
				"kipper.run/app":              "api",
				"kipper.run/source-namespace": "blog",
			},
		},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{Name: "build"}},
			Containers:     []corev1.Container{{Name: "push"}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodSucceeded},
	}
}

// The build pod lives in the shared kipper-builds namespace, not the tenant's
// project namespace, so BuildLogs must read logs from where GetBuildPod found
// the pod. This guards the namespace the GetLogs request targets.
func TestBuildLogs_ReadsFromBuildsNamespace(t *testing.T) {
	client := fake.NewClientset(buildPodInBuildsNS())
	h := &Webhooks{Client: client}

	r := chi.NewRouter()
	r.Get("/projects/{name}/apps/{app}/build/logs", h.BuildLogs)
	req := httptest.NewRequest(http.MethodGet, "/projects/blog/apps/api/build/logs", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var logNamespaces []string
	for _, action := range client.Actions() {
		if action.GetVerb() == "get" && action.GetResource().Resource == "pods" && action.GetSubresource() == "log" {
			logNamespaces = append(logNamespaces, action.GetNamespace())
		}
	}
	require.NotEmpty(t, logNamespaces, "expected at least one pod/log request")
	for _, ns := range logNamespaces {
		assert.Equal(t, "kipper-builds", ns, "logs must be read from the build namespace, not the project namespace")
	}
}

// A missing build pod is a 404, not a 500 — the tenant namespace holds no build
// pods now, so a lookup there would silently find nothing.
func TestBuildLogs_NoBuildPodIs404(t *testing.T) {
	client := fake.NewClientset()
	h := &Webhooks{Client: client}

	r := chi.NewRouter()
	r.Get("/projects/{name}/apps/{app}/build/logs", h.BuildLogs)
	req := httptest.NewRequest(http.MethodGet, "/projects/blog/apps/api/build/logs", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}
