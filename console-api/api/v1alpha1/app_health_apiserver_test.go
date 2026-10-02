package v1alpha1

import (
	"context"
	"fmt"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/getkipper/kipper/console-api/internal/apiservertest"
)

func TestAppHealthValidation(t *testing.T) {
	cfg := apiservertest.Start(t)
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c, err := crclient.New(cfg, crclient.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	i32 := func(v int32) *int32 { return &v }

	tests := []struct {
		name    string
		health  *AppHealth
		wantErr string
	}{
		{name: "no health block"},
		{name: "http with a path", health: &AppHealth{Type: "http", Path: "/healthz", Port: i32(9000), StartupTimeoutSeconds: i32(600), TimeoutSeconds: i32(5)}},
		{name: "tcp", health: &AppHealth{Type: "tcp"}},
		{name: "tcp on another port", health: &AppHealth{Type: "tcp", Port: i32(9090)}},
		{name: "none", health: &AppHealth{Type: "none"}},
		{name: "none with a startup timeout", health: &AppHealth{Type: "none", StartupTimeoutSeconds: i32(900)}},
		{name: "http without a path", health: &AppHealth{Type: "http"}, wantErr: "path is required for an http check"},
		{name: "tcp with a path", health: &AppHealth{Type: "tcp", Path: "/healthz"}, wantErr: "path is required for an http check"},
		{name: "none with a port", health: &AppHealth{Type: "none", Port: i32(8080)}, wantErr: "a check of type none takes no port or timeout"},
		{name: "none with a timeout", health: &AppHealth{Type: "none", TimeoutSeconds: i32(2)}, wantErr: "a check of type none takes no port or timeout"},
		{name: "unknown type", health: &AppHealth{Type: "grpc"}, wantErr: "Unsupported value"},
		{name: "relative path", health: &AppHealth{Type: "http", Path: "healthz"}, wantErr: "spec.health.path"},
		{name: "path with a space", health: &AppHealth{Type: "http", Path: "/health z"}, wantErr: "spec.health.path"},
		{name: "port zero", health: &AppHealth{Type: "tcp", Port: i32(0)}, wantErr: "spec.health.port"},
		{name: "port too high", health: &AppHealth{Type: "tcp", Port: i32(65536)}, wantErr: "spec.health.port"},
		{name: "startup timeout too short", health: &AppHealth{Type: "tcp", StartupTimeoutSeconds: i32(9)}, wantErr: "spec.health.startupTimeoutSeconds"},
		{name: "startup timeout too long", health: &AppHealth{Type: "tcp", StartupTimeoutSeconds: i32(3601)}, wantErr: "spec.health.startupTimeoutSeconds"},
		{name: "timeout zero", health: &AppHealth{Type: "tcp", TimeoutSeconds: i32(0)}, wantErr: "spec.health.timeoutSeconds"},
		{name: "timeout too long", health: &AppHealth{Type: "tcp", TimeoutSeconds: i32(61)}, wantErr: "spec.health.timeoutSeconds"},
		{name: "the instance proxy port", health: &AppHealth{Type: "tcp", Port: i32(18080)}, wantErr: "port + 10000"},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := &App{
				ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("health-%d", i), Namespace: "default"},
				Spec:       AppSpec{Image: "registry.example.com/web:1", Port: 8080, Health: tt.health},
			}
			err := c.Create(context.Background(), app)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("rejected: %v", err)
			case tt.wantErr != "" && err == nil:
				t.Fatalf("accepted, want an error mentioning %q", tt.wantErr)
			case tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr):
				t.Fatalf("error %q does not mention %q", err, tt.wantErr)
			}
		})
	}

	t.Run("the API server adds no defaults", func(t *testing.T) {
		app := &App{
			ObjectMeta: metav1.ObjectMeta{Name: "health-defaults", Namespace: "default"},
			Spec:       AppSpec{Image: "registry.example.com/web:1", Port: 8080, Health: &AppHealth{Type: "tcp"}},
		}
		if err := c.Create(context.Background(), app); err != nil {
			t.Fatal(err)
		}
		got := &App{}
		if err := c.Get(context.Background(), crclient.ObjectKeyFromObject(app), got); err != nil {
			t.Fatal(err)
		}
		if want := (AppHealth{Type: "tcp"}); got.Spec.Health == nil || *got.Spec.Health != want {
			t.Errorf("stored health = %+v, want exactly %+v so export and diff show what the user wrote", got.Spec.Health, want)
		}
	})
}
