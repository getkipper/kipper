package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func functionLabels(fnName string) map[string]string {
	return map[string]string{
		kipperLabel:                kipperValue,
		"app":                      fnName,
		"kipper.run/resource-type": "function",
		"kipper.run/trigger":       "http",
	}
}

func TestFunctionsDelete_DeletesFunctionCR(t *testing.T) {
	ns := "default"
	fnName := "myfn"

	fnCR := &kipperv1.Function{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fnName,
			Namespace: ns,
			Labels:    functionLabels(fnName),
		},
		Spec: kipperv1.FunctionSpec{
			Image: "myfn:v1",
			Port:  8080,
		},
	}

	client := kubefake.NewClientset()
	scheme := runtime.NewScheme()
	dynClient := fake.NewSimpleDynamicClient(scheme)

	handler := &Functions{Client: client, Dynamic: dynClient, CRClient: testCRClient(fnCR)}

	r := chi.NewRouter()
	r.Delete("/projects/{name}/functions/{fn}", handler.Delete)

	req := httptest.NewRequest("DELETE", "/projects/default/functions/myfn", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected status %d, got %d; body: %s", http.StatusNoContent, rec.Code, rec.Body.String())
	}
}

func TestFunctionsDelete_NonexistentFunctionReturns404(t *testing.T) {
	client := kubefake.NewClientset()
	scheme := runtime.NewScheme()
	dynClient := fake.NewSimpleDynamicClient(scheme)

	handler := &Functions{Client: client, Dynamic: dynClient, CRClient: testCRClient()}

	r := chi.NewRouter()
	r.Delete("/projects/{name}/functions/{fn}", handler.Delete)

	req := httptest.NewRequest("DELETE", "/projects/default/functions/nonexistent", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected status %d, got %d; body: %s", http.StatusNotFound, rec.Code, rec.Body.String())
	}
}
