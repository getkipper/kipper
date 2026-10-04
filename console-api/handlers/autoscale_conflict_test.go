package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

// racedClient stores app and, just before the handler's first App update,
// applies change as another writer would, so that update carries a stale
// resource version.
func racedClient(t *testing.T, app *kipperv1.App, change func(*kipperv1.App)) crclient.Client {
	t.Helper()
	raced := false
	return crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(app).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.UpdateOption) error {
				if _, ok := obj.(*kipperv1.App); ok && !raced {
					raced = true
					var live kipperv1.App
					if err := c.Get(ctx, crclient.ObjectKeyFromObject(obj), &live); err != nil {
						t.Fatal(err)
					}
					change(&live)
					if err := c.Update(ctx, &live); err != nil {
						t.Fatal(err)
					}
				}
				return c.Update(ctx, obj, opts...)
			},
		}).Build()
}

// conflictingClient refuses every App update with a conflict.
func conflictingClient(app *kipperv1.App) crclient.Client {
	return crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(app).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.UpdateOption) error {
				return apierrors.NewConflict(schema.GroupResource{Group: "kipper.run", Resource: "apps"}, obj.GetName(), nil)
			},
		}).Build()
}

func serveScale(t *testing.T, h *Apps, body string) (int, map[string]any) {
	t.Helper()
	r := chi.NewRouter()
	r.Put("/projects/{name}/apps/{app}/scale", h.Scale)
	req := httptest.NewRequest("PUT", "/projects/staging/apps/web/scale", strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding %s: %v", rec.Body.String(), err)
	}
	return rec.Code, resp
}

func TestAutoscaleSet_RetriesOnAConcurrentWrite(t *testing.T) {
	c := racedClient(t, capacityApp(int32Ptr(3), nil), func(app *kipperv1.App) {
		app.Spec.Replicas = int32Ptr(9)
	})

	code, resp := serveAutoscale(t, &Autoscale{Client: fake.NewClientset(), CRClient: c}, "PUT", `{"min_replicas":2,"max_replicas":6,"cpu_target":70}`)

	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %v", code, resp)
	}
	got := storedStagingApp(t, c)
	if got.Spec.Autoscale == nil || !got.Spec.Autoscale.Enabled {
		t.Errorf("expected autoscaling stored on, got %s", blockString(got.Spec.Autoscale))
	}
	if ptr.Deref(got.Spec.Replicas, 0) != 6 {
		t.Errorf("expected the concurrently stored 9 moved to 6, got %v", ptr.Deref(got.Spec.Replicas, 0))
	}
	if moved, _ := resp["replicas_moved"].(map[string]any); moved["from"] != float64(9) {
		t.Errorf("expected the move to start from the re-read count, got %v", resp)
	}
}

func TestAutoscaleSet_SwitchOffRetriesOnAConcurrentWrite(t *testing.T) {
	c := racedClient(t, capacityApp(int32Ptr(3), nil), func(app *kipperv1.App) {
		app.Spec.Replicas = int32Ptr(9)
	})

	code, resp := serveAutoscale(t, &Autoscale{Client: fake.NewClientset(), CRClient: c}, "PUT", `{"enabled":false,"min_replicas":2,"max_replicas":6}`)

	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %v", code, resp)
	}
	got := storedStagingApp(t, c)
	if ptr.Deref(got.Spec.Replicas, 0) != 6 {
		t.Errorf("expected the concurrently stored 9 moved to 6, got %v", ptr.Deref(got.Spec.Replicas, 0))
	}
	if resp["replicas"] != float64(6) {
		t.Errorf("expected the response to report 6, got %v", resp)
	}
}

func TestAutoscaleDelete_RetriesOnAConcurrentWrite(t *testing.T) {
	c := racedClient(t, capacityApp(int32Ptr(3), onPolicy(2, 6)), func(app *kipperv1.App) {
		app.Spec.Autoscale.MaxReplicas = ptr.To[int32](8)
	})

	code, resp := serveAutoscale(t, &Autoscale{Client: fake.NewClientset(), CRClient: c}, "DELETE", "")

	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %v", code, resp)
	}
	got := storedStagingApp(t, c)
	if got.Spec.Autoscale == nil || got.Spec.Autoscale.Enabled || ptr.Deref(got.Spec.Autoscale.MaxReplicas, 0) != 8 {
		t.Errorf("expected autoscaling off over the concurrent maximum of 8, got %s", blockString(got.Spec.Autoscale))
	}
}

func TestScale_RetriesOnAConcurrentWrite(t *testing.T) {
	t.Run("the write lands on the re-read app", func(t *testing.T) {
		c := racedClient(t, capacityApp(int32Ptr(3), nil), func(app *kipperv1.App) {
			app.Spec.Image = "nginx:1.27"
		})

		code, resp := serveScale(t, &Apps{Client: fake.NewClientset(), CRClient: c}, `{"replicas":5}`)

		if code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %v", code, resp)
		}
		got := storedStagingApp(t, c)
		if ptr.Deref(got.Spec.Replicas, 0) != 5 || got.Spec.Image != "nginx:1.27" {
			t.Errorf("expected 5 replicas beside the concurrent image, got %v and %s", ptr.Deref(got.Spec.Replicas, 0), got.Spec.Image)
		}
	})

	t.Run("autoscaling switched on meanwhile refuses the scale", func(t *testing.T) {
		c := racedClient(t, capacityApp(int32Ptr(3), nil), func(app *kipperv1.App) {
			app.Spec.Autoscale = onPolicy(2, 6)
		})

		code, resp := serveScale(t, &Apps{Client: fake.NewClientset(), CRClient: c}, `{"replicas":4}`)

		if code != http.StatusConflict {
			t.Fatalf("expected 409, got %d: %v", code, resp)
		}
		if msg, _ := resp["error"].(string); !strings.Contains(msg, "disable autoscaling first") {
			t.Errorf("expected the autoscaling refusal, got %v", resp)
		}
		if ptr.Deref(storedStagingApp(t, c).Spec.Replicas, 0) != 3 {
			t.Error("expected the stored count left at 3")
		}
	})
}

func TestAppWrites_AConflictThatPersistsAsksForARetry(t *testing.T) {
	tests := []struct {
		name  string
		app   *kipperv1.App
		serve func(crclient.Client) (int, map[string]any)
	}{
		{
			name: "PUT /autoscale on", app: capacityApp(int32Ptr(3), nil),
			serve: func(c crclient.Client) (int, map[string]any) {
				return serveAutoscale(t, &Autoscale{Client: fake.NewClientset(), CRClient: c}, "PUT", `{"min_replicas":2,"max_replicas":6,"cpu_target":70}`)
			},
		},
		{
			name: "PUT /autoscale off", app: capacityApp(int32Ptr(3), nil),
			serve: func(c crclient.Client) (int, map[string]any) {
				return serveAutoscale(t, &Autoscale{Client: fake.NewClientset(), CRClient: c}, "PUT", `{"enabled":false,"min_replicas":2,"max_replicas":6}`)
			},
		},
		{
			name: "DELETE /autoscale", app: capacityApp(int32Ptr(3), onPolicy(2, 6)),
			serve: func(c crclient.Client) (int, map[string]any) {
				return serveAutoscale(t, &Autoscale{Client: fake.NewClientset(), CRClient: c}, "DELETE", "")
			},
		},
		{
			name: "PUT /scale", app: capacityApp(int32Ptr(3), nil),
			serve: func(c crclient.Client) (int, map[string]any) {
				return serveScale(t, &Apps{Client: fake.NewClientset(), CRClient: c}, `{"replicas":4}`)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, resp := tt.serve(conflictingClient(tt.app))
			if code != http.StatusConflict {
				t.Fatalf("expected 409, got %d: %v", code, resp)
			}
			if msg, _ := resp["error"].(string); !strings.Contains(msg, "try again") {
				t.Errorf("expected a retry hint, got %v", resp)
			}
		})
	}
}
