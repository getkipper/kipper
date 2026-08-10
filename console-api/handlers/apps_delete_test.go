package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func TestAppsDelete_DeletesAppCR(t *testing.T) {
	ns := "default"
	appName := "myapp"

	appCR := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{
			Name:      appName,
			Namespace: ns,
			Labels:    kipperLabels(appName),
		},
		Spec: kipperv1.AppSpec{
			Image: "myapp:v1",
			Port:  8080,
		},
	}

	client := fake.NewClientset()
	handler := &Apps{Client: client, CRClient: testCRClient(appCR)}

	r := chi.NewRouter()
	r.Delete("/projects/{name}/apps/{app}", handler.Delete)

	req := httptest.NewRequest("DELETE", "/projects/default/apps/myapp", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected status %d, got %d; body: %s", http.StatusNoContent, rec.Code, rec.Body.String())
	}
}

func TestAppsDelete_NonexistentAppReturns404(t *testing.T) {
	client := fake.NewClientset()
	handler := &Apps{Client: client, CRClient: testCRClient()}

	r := chi.NewRouter()
	r.Delete("/projects/{name}/apps/{app}", handler.Delete)

	req := httptest.NewRequest("DELETE", "/projects/default/apps/nonexistent", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected status %d, got %d; body: %s", http.StatusNotFound, rec.Code, rec.Body.String())
	}
}
