package handlers

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// A StatefulSet whose controller has not yet seen its newest template still
// reports the old rollout's counts, so it is not ready.
func TestRolloutStatusWaitsForTheNewestTemplate(t *testing.T) {
	cases := []struct {
		name     string
		observed int64
		want     bool
	}{
		{name: "template not yet observed", observed: 1, want: false},
		{name: "template observed and rolled out", observed: 2, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			one := int32(1)
			sts := &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "shop", Generation: 2},
				Spec:       appsv1.StatefulSetSpec{Replicas: &one},
				Status: appsv1.StatefulSetStatus{
					ObservedGeneration: tc.observed, ReadyReplicas: 1, UpdatedReplicas: 1,
					CurrentRevision: "db-1", UpdateRevision: "db-1",
				},
			}
			s := &Services{Client: fake.NewClientset(sts)}
			r := chi.NewRouter()
			r.Get("/api/v1/services/{name}/rollout", s.RolloutStatus)

			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/services/db/rollout?namespace=shop", nil))
			var resp struct {
				Ready bool `json:"ready"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if resp.Ready != tc.want {
				t.Fatalf("ready = %v, want %v", resp.Ready, tc.want)
			}
		})
	}
}
