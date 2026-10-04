package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func capacityApp(replicas *int32, as *kipperv1.AppAutoscale) *kipperv1.App {
	return &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "staging"},
		Spec:       kipperv1.AppSpec{Image: "nginx:1.25", Port: 80, Replicas: replicas, Autoscale: as},
	}
}

func liveDeployment(replicas int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "staging"},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
	}
}

func storedStagingApp(t *testing.T, c crclient.Client) kipperv1.App {
	t.Helper()
	var app kipperv1.App
	if err := c.Get(context.Background(), crclient.ObjectKey{Namespace: "staging", Name: "web"}, &app); err != nil {
		t.Fatal(err)
	}
	return app
}

func serveAutoscale(t *testing.T, h *Autoscale, method, body string) (int, map[string]any) {
	t.Helper()
	r := chi.NewRouter()
	r.Get("/projects/{name}/apps/{app}/autoscale", h.Get)
	r.Put("/projects/{name}/apps/{app}/autoscale", h.Set)
	r.Delete("/projects/{name}/apps/{app}/autoscale", h.Delete)
	req := httptest.NewRequest(method, "/projects/staging/apps/web/autoscale", strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding %s: %v", rec.Body.String(), err)
	}
	return rec.Code, resp
}

func onPolicy(minReplicas, maxReplicas int32) *kipperv1.AppAutoscale {
	return &kipperv1.AppAutoscale{Enabled: true, MinReplicas: ptr.To[int32](minReplicas), MaxReplicas: ptr.To[int32](maxReplicas), CPUTarget: ptr.To[int32](70)}
}

func TestAutoscaleDelete_KeepsTheRunningCount(t *testing.T) {
	tests := []struct {
		name        string
		app         *kipperv1.App
		deployment  *appsv1.Deployment
		wantStored  int32
		wantMoved   map[string]any
		wantWarning string
		wantNote    string
	}{
		{
			name:       "running count within the bounds",
			app:        capacityApp(int32Ptr(2), onPolicy(2, 6)),
			deployment: liveDeployment(4),
			wantStored: 4,
		},
		{
			name:       "running count above the maximum is clamped and reported",
			app:        capacityApp(int32Ptr(2), onPolicy(2, 6)),
			deployment: liveDeployment(9),
			wantStored: 6,
			wantMoved:  map[string]any{"from": float64(9), "to": float64(6)},
		},
		{
			name:        "an unreadable count keeps the stored count and says so",
			app:         capacityApp(int32Ptr(3), onPolicy(2, 6)),
			wantStored:  3,
			wantWarning: "could not be read",
		},
		{
			name:        "invalid bounds keep the running count",
			app:         capacityApp(int32Ptr(3), onPolicy(7, 3)),
			deployment:  liveDeployment(5),
			wantStored:  5,
			wantWarning: "bounds are invalid. Remove them, or save valid ones with autoscaling left off.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewClientset()
			if tt.deployment != nil {
				client = fake.NewClientset(tt.deployment)
			}
			crClient := testCRClient(tt.app)
			code, resp := serveAutoscale(t, &Autoscale{Client: client, CRClient: crClient}, "DELETE", "")

			if code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %v", code, resp)
			}
			stored := storedStagingApp(t, crClient)
			if stored.Spec.Autoscale == nil || stored.Spec.Autoscale.Enabled {
				t.Fatalf("expected the policy switched off with its bounds kept, got %+v", stored.Spec.Autoscale)
			}
			if !reflect.DeepEqual(stored.Spec.Autoscale.MinReplicas, tt.app.Spec.Autoscale.MinReplicas) || !reflect.DeepEqual(stored.Spec.Autoscale.MaxReplicas, tt.app.Spec.Autoscale.MaxReplicas) {
				t.Errorf("bounds changed: %+v", stored.Spec.Autoscale)
			}
			if got := *stored.Spec.Replicas; got != tt.wantStored {
				t.Errorf("stored replicas = %d, want %d", got, tt.wantStored)
			}
			if resp["replicas"] != float64(tt.wantStored) {
				t.Errorf("response replicas = %v, want %d", resp["replicas"], tt.wantStored)
			}
			if tt.wantMoved == nil {
				if _, ok := resp["replicas_moved"]; ok {
					t.Errorf("unexpected replicas_moved: %v", resp["replicas_moved"])
				}
			} else if moved, _ := resp["replicas_moved"].(map[string]any); moved["from"] != tt.wantMoved["from"] || moved["to"] != tt.wantMoved["to"] {
				t.Errorf("replicas_moved = %v, want %v", resp["replicas_moved"], tt.wantMoved)
			}
			warning, _ := resp["warning"].(string)
			if tt.wantWarning == "" && warning != "" || !strings.Contains(warning, tt.wantWarning) {
				t.Errorf("warning = %q, want it to contain %q", warning, tt.wantWarning)
			}
		})
	}
}

func TestAutoscaleDelete_StoppedAppKeepsTheStoredCount(t *testing.T) {
	app := capacityApp(int32Ptr(3), onPolicy(2, 6))
	app.Spec.Stopped = &kipperv1.AppStopped{Reason: "maintenance"}
	crClient := testCRClient(app)
	code, resp := serveAutoscale(t, &Autoscale{Client: fake.NewClientset(liveDeployment(0)), CRClient: crClient}, "DELETE", "")

	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %v", code, resp)
	}
	stored := storedStagingApp(t, crClient)
	if *stored.Spec.Replicas != 3 || stored.Spec.Autoscale.Enabled {
		t.Errorf("expected replicas 3 and the policy off, got %d and %+v", *stored.Spec.Replicas, stored.Spec.Autoscale)
	}
	if note, _ := resp["note"].(string); !strings.Contains(note, "stopped") {
		t.Errorf("expected a note about the stopped app, got %v", resp)
	}
}

func TestAutoscaleDelete_PassesOnTheAPIServersRefusal(t *testing.T) {
	crClient := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(capacityApp(int32Ptr(3), onPolicy(7, 3))).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.UpdateOption) error {
				return apierrors.NewInvalid(schema.GroupKind{Group: "kipper.run", Kind: "App"}, "web", field.ErrorList{
					field.Invalid(field.NewPath("spec", "autoscale"), nil, "minReplicas must not exceed maxReplicas"),
				})
			},
		}).Build()
	code, resp := serveAutoscale(t, &Autoscale{Client: fake.NewClientset(liveDeployment(5)), CRClient: crClient}, "DELETE", "")

	if code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %v", code, resp)
	}
	if msg, _ := resp["error"].(string); !strings.Contains(msg, "minReplicas must not exceed maxReplicas") {
		t.Errorf("expected the refusal's reason, got %v", resp)
	}
}

func TestAutoscaleDelete_AlreadyOffWritesNothing(t *testing.T) {
	for name, as := range map[string]*kipperv1.AppAutoscale{
		"disabled block": {Enabled: false, MinReplicas: ptr.To[int32](2), MaxReplicas: ptr.To[int32](6)},
		"no block":       nil,
	} {
		t.Run(name, func(t *testing.T) {
			crClient := testCRClient(capacityApp(int32Ptr(3), as))
			before := storedStagingApp(t, crClient).ResourceVersion
			code, resp := serveAutoscale(t, &Autoscale{Client: fake.NewClientset(liveDeployment(5)), CRClient: crClient}, "DELETE", "")

			if code != http.StatusOK || resp["status"] != "disabled" {
				t.Fatalf("expected 200 disabled, got %d: %v", code, resp)
			}
			stored := storedStagingApp(t, crClient)
			if stored.ResourceVersion != before {
				t.Error("a policy that is already off must not be written")
			}
			if as == nil && stored.Spec.Autoscale != nil {
				t.Errorf("a disable must not create a block, got %+v", stored.Spec.Autoscale)
			}
			if *stored.Spec.Replicas != 3 {
				t.Errorf("stored replicas = %d, want 3", *stored.Spec.Replicas)
			}
		})
	}
}

func TestAutoscaleGet_ReturnsStoredBoundsWhenOff(t *testing.T) {
	app := capacityApp(int32Ptr(3), &kipperv1.AppAutoscale{Enabled: false, MinReplicas: ptr.To[int32](2), MaxReplicas: ptr.To[int32](6), MemoryTarget: ptr.To[int32](80)})
	code, resp := serveAutoscale(t, &Autoscale{Client: fake.NewClientset(), CRClient: testCRClient(app)}, "GET", "")

	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if resp["enabled"] != false || resp["min_replicas"] != float64(2) || resp["max_replicas"] != float64(6) ||
		resp["cpu_target"] != float64(0) || resp["memory_target"] != float64(80) {
		t.Errorf("expected the stored bounds and targets with enabled false, got %v", resp)
	}
}

func TestAutoscaleSet_MovesStoredReplicasIntoNewBounds(t *testing.T) {
	tests := []struct {
		name       string
		replicas   *int32
		body       string
		wantStored int32
		wantFrom   float64
	}{
		{name: "raised minimum lifts the count", replicas: int32Ptr(1), body: `{"min_replicas":3,"max_replicas":6,"cpu_target":70}`, wantStored: 3, wantFrom: 1},
		{name: "lowered maximum lowers the count", replicas: int32Ptr(9), body: `{"min_replicas":1,"max_replicas":5,"cpu_target":70}`, wantStored: 5, wantFrom: 9},
		{name: "an absent count is 1", replicas: nil, body: `{"min_replicas":2,"max_replicas":5,"cpu_target":70}`, wantStored: 2, wantFrom: 1},
		{name: "a count within the bounds stays", replicas: int32Ptr(4), body: `{"min_replicas":2,"max_replicas":5,"cpu_target":70}`, wantStored: 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			crClient := testCRClient(capacityApp(tt.replicas, nil))
			code, resp := serveAutoscale(t, &Autoscale{Client: fake.NewClientset(), CRClient: crClient}, "PUT", tt.body)

			if code != http.StatusOK || resp["status"] != "enabled" {
				t.Fatalf("expected 200 enabled, got %d: %v", code, resp)
			}
			stored := storedStagingApp(t, crClient)
			if got := replicasOrDefault(stored.Spec.Replicas); stored.Spec.Replicas == nil || got != tt.wantStored {
				t.Errorf("stored replicas = %d (set: %v), want %d", got, stored.Spec.Replicas != nil, tt.wantStored)
			}
			moved, hasMoved := resp["replicas_moved"].(map[string]any)
			if tt.wantFrom == 0 {
				if hasMoved {
					t.Errorf("unexpected replicas_moved: %v", moved)
				}
				return
			}
			if moved["from"] != tt.wantFrom || moved["to"] != float64(tt.wantStored) {
				t.Errorf("replicas_moved = %v, want from %v to %d", moved, tt.wantFrom, tt.wantStored)
			}
		})
	}
}

func TestScale_HoldsStoredBounds(t *testing.T) {
	tests := []struct {
		name       string
		as         *kipperv1.AppAutoscale
		body       string
		wantStatus int
		wantError  string
		wantStored int32
	}{
		{name: "negative count", body: `{"replicas":-1}`, wantStatus: http.StatusBadRequest, wantError: "non-negative", wantStored: 3},
		{name: "above the stored maximum", as: &kipperv1.AppAutoscale{MinReplicas: ptr.To[int32](2), MaxReplicas: ptr.To[int32](6)}, body: `{"replicas":8}`, wantStatus: http.StatusBadRequest, wantError: "between 2 and 6", wantStored: 3},
		{name: "zero with bounds", as: &kipperv1.AppAutoscale{MinReplicas: ptr.To[int32](2), MaxReplicas: ptr.To[int32](6)}, body: `{"replicas":0}`, wantStatus: http.StatusBadRequest, wantError: "between 2 and 6", wantStored: 3},
		{name: "within the stored bounds", as: &kipperv1.AppAutoscale{MinReplicas: ptr.To[int32](2), MaxReplicas: ptr.To[int32](6)}, body: `{"replicas":5}`, wantStatus: http.StatusOK, wantStored: 5},
		{name: "stored zero bounds are refused", as: &kipperv1.AppAutoscale{MinReplicas: ptr.To[int32](0), MaxReplicas: ptr.To[int32](5)}, body: `{"replicas":4}`, wantStatus: http.StatusBadRequest, wantError: "minReplicas must be at least 1", wantStored: 3},
		{name: "policy on is refused", as: onPolicy(2, 6), body: `{"replicas":5}`, wantStatus: http.StatusConflict, wantError: "disable autoscaling first", wantStored: 3},
		{name: "no block, no bounds", body: `{"replicas":7}`, wantStatus: http.StatusOK, wantStored: 7},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			crClient := testCRClient(capacityApp(int32Ptr(3), tt.as))
			handler := &Apps{Client: fake.NewClientset(), CRClient: crClient, GitReach: gitAlwaysReachable}
			r := chi.NewRouter()
			r.Put("/projects/{name}/apps/{app}/scale", handler.Scale)
			req := httptest.NewRequest("PUT", "/projects/staging/apps/web/scale", strings.NewReader(tt.body))
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("expected %d, got %d: %s", tt.wantStatus, rec.Code, rec.Body.String())
			}
			if tt.wantError != "" && !strings.Contains(rec.Body.String(), tt.wantError) {
				t.Errorf("expected the response to say %q, got %s", tt.wantError, rec.Body.String())
			}
			stored := storedStagingApp(t, crClient)
			if *stored.Spec.Replicas != tt.wantStored {
				t.Errorf("stored replicas = %d, want %d", *stored.Spec.Replicas, tt.wantStored)
			}
			if tt.as == nil && stored.Spec.Autoscale != nil {
				t.Errorf("a scale must not create a block, got %+v", stored.Spec.Autoscale)
			}
		})
	}
}
