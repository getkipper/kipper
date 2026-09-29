package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/internal/resourcebounds"
)

func TestResourcesUpdateLeavesUnsentValuesAlone(t *testing.T) {
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "staging"},
		Spec: kipperv1.AppSpec{Image: "nginx:1.25", Port: 80, Resources: kipperv1.AppResources{
			CPURequest: "250m", CPULimit: "1",
		}},
	}
	cr := testCRClient(app)
	handler := &Resources{Client: fake.NewClientset(), CRClient: cr}
	r := chi.NewRouter()
	r.Put("/projects/{name}/apps/{app}/resources", handler.Update)

	req := httptest.NewRequest("PUT", "/projects/staging/apps/web/resources",
		strings.NewReader(`{"memory_request":"512Mi","memory_limit":"2Gi"}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var got kipperv1.App
	if err := cr.Get(context.Background(), crclient.ObjectKeyFromObject(app), &got); err != nil {
		t.Fatal(err)
	}
	res := got.Spec.Resources
	if res.MemoryRequest != "512Mi" || res.MemoryLimit != "2Gi" {
		t.Errorf("memory = %s/%s, want 512Mi/2Gi", res.MemoryRequest, res.MemoryLimit)
	}
	if res.CPURequest != "250m" || res.CPULimit != "1" {
		t.Errorf("cpu = %s/%s, want the untouched 250m/1", res.CPURequest, res.CPULimit)
	}
}

func TestServiceResourcesUpdateWritesTheServiceNotTheStatefulSet(t *testing.T) {
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "shop"},
		Spec: appsv1.StatefulSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:      "db",
			Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")}},
		}}}}},
	}
	svc := &kipperv1.Service{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "shop"}}
	client := fake.NewClientset(sts)
	cr := testCRClient(svc)
	s := &Services{Client: client, CRClient: cr}
	r := chi.NewRouter()
	r.Put("/api/v1/services/{name}/resources", s.UpdateResources)

	req := httptest.NewRequest("PUT", "/api/v1/services/db/resources?namespace=shop",
		strings.NewReader(`{"memory_request":"512Mi","memory_limit":"2Gi"}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var got kipperv1.Service
	if err := cr.Get(context.Background(), crclient.ObjectKeyFromObject(svc), &got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.Resources.MemoryRequest != "512Mi" || got.Spec.Resources.MemoryLimit != "2Gi" {
		t.Errorf("service memory = %s/%s, want 512Mi/2Gi", got.Spec.Resources.MemoryRequest, got.Spec.Resources.MemoryLimit)
	}
	for _, a := range client.Actions() {
		if a.GetResource().Resource == "statefulsets" && a.GetVerb() != "get" {
			t.Fatalf("the handler wrote the StatefulSet: %s", a.GetVerb())
		}
	}
}

// Values chosen when creating an App in the console are the user's.
func TestConsoleCreatedAppValuesAreTheUsers(t *testing.T) {
	cr := crfake.NewClientBuilder().WithScheme(testScheme()).WithReturnManagedFields().Build()
	// console-api's client writes as "console-api" when nothing names another
	// manager, which is the old auto-sizer's name.
	handler := &Apps{Client: fake.NewClientset(), CRClient: crclient.WithFieldOwner(cr, "console-api"), GitReach: gitAlwaysReachable}
	r := chi.NewRouter()
	r.Post("/projects/{name}/apps", handler.Create)

	body := `{"name":"web","image":"nginx:1.25","port":80,"memory_request":"512Mi","memory_limit":"2Gi"}`
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("POST", "/projects/staging/apps", strings.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var app kipperv1.App
	if err := cr.Get(context.Background(), crclient.ObjectKey{Namespace: "staging", Name: "web"}, &app); err != nil {
		t.Fatal(err)
	}
	spec, err := resourcebounds.AppSpec(&app)
	if err != nil {
		t.Fatal(err)
	}
	if spec.MemoryRequest.Source != resourcebounds.User || spec.MemoryLimit.Source != resourcebounds.User {
		t.Fatalf("memory sources = %v/%v, want the user's", spec.MemoryRequest.Source, spec.MemoryLimit.Source)
	}
}
