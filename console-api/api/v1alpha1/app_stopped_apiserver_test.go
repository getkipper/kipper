package v1alpha1

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/getkipper/kipper/console-api/internal/apiservertest"
)

func TestAppStoppedSchema(t *testing.T) {
	cfg := apiservertest.Start(t)
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c, err := crclient.New(cfg, crclient.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	app := func(name string, stopped *AppStopped) *App {
		return &App{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec:       AppSpec{Image: "registry.example.com/web:1", Port: 8080, Stopped: stopped},
		}
	}

	t.Run("a stop round-trips unchanged", func(t *testing.T) {
		at := metav1.NewTime(time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC))
		want := AppStopped{Reason: "freeing memory", By: "kip (alice)", At: &at, ForMigration: true}
		if err := c.Create(ctx, app("stopped-full", want.DeepCopy())); err != nil {
			t.Fatal(err)
		}
		got := &App{}
		if err := c.Get(ctx, crclient.ObjectKey{Namespace: "default", Name: "stopped-full"}, got); err != nil {
			t.Fatal(err)
		}
		if got.Spec.Stopped == nil || got.Spec.Stopped.Reason != want.Reason || got.Spec.Stopped.By != want.By ||
			!got.Spec.Stopped.At.Equal(want.At) || !got.Spec.Stopped.ForMigration {
			t.Fatalf("stored %+v, want %+v", got.Spec.Stopped, want)
		}
	})

	t.Run("an empty stop is kept and gets no defaults", func(t *testing.T) {
		if err := c.Create(ctx, app("stopped-empty", &AppStopped{})); err != nil {
			t.Fatal(err)
		}
		got := &App{}
		if err := c.Get(ctx, crclient.ObjectKey{Namespace: "default", Name: "stopped-empty"}, got); err != nil {
			t.Fatal(err)
		}
		if got.Spec.Stopped == nil || *got.Spec.Stopped != (AppStopped{}) {
			t.Fatalf("stored %+v, want an empty stop", got.Spec.Stopped)
		}
	})

	t.Run("an app without a stop gets none", func(t *testing.T) {
		if err := c.Create(ctx, app("running", nil)); err != nil {
			t.Fatal(err)
		}
		got := &App{}
		if err := c.Get(ctx, crclient.ObjectKey{Namespace: "default", Name: "running"}, got); err != nil {
			t.Fatal(err)
		}
		if got.Spec.Stopped != nil {
			t.Fatalf("stored %+v, want no stop", got.Spec.Stopped)
		}
	})

	for _, tc := range []struct {
		name, field string
		stopped     *AppStopped
	}{
		{"a reason over 500 characters", "spec.stopped.reason", &AppStopped{Reason: strings.Repeat("r", 501)}},
		{"a by over 256 characters", "spec.stopped.by", &AppStopped{By: strings.Repeat("b", 257)}},
	} {
		t.Run(tc.name+" is refused", func(t *testing.T) {
			err := c.Create(ctx, app("stopped-long-"+strings.Fields(tc.name)[1], tc.stopped))
			if err == nil || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("got %v, want an error naming %s", err, tc.field)
			}
		})
	}
}
