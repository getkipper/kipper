package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func TestApplyingARecommendationKeepsTheUsersValues(t *testing.T) {
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "staging"},
		Spec: kipperv1.AppSpec{Image: "nginx:1.25", Port: 80, Resources: kipperv1.AppResources{
			Profile: "standard", MemoryRequest: "512Mi", MemoryLimit: "2Gi",
		}},
		Status: kipperv1.AppStatus{Conditions: []metav1.Condition{{
			Type: recommendationType, Status: metav1.ConditionTrue, Reason: "RecommendJvm",
			LastTransitionTime: metav1.Now(),
		}}},
	}
	cr := testCRClient(app)
	h := &Recommendations{CRClient: cr}
	r := chi.NewRouter()
	r.Post("/projects/{name}/apps/{app}/recommendation/apply", h.Apply)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("POST", "/projects/staging/apps/web/recommendation/apply", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var got kipperv1.App
	if err := cr.Get(context.Background(), crclient.ObjectKeyFromObject(app), &got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.Resources.Profile != "jvm" {
		t.Errorf("profile = %q, want jvm", got.Spec.Resources.Profile)
	}
	if got.Spec.Resources.MemoryRequest != "512Mi" || got.Spec.Resources.MemoryLimit != "2Gi" {
		t.Errorf("memory = %s/%s, want the user's 512Mi/2Gi kept", got.Spec.Resources.MemoryRequest, got.Spec.Resources.MemoryLimit)
	}
}
