package migration

import (
	"context"
	"testing"

	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/internal/resourcebounds"
)

func migratedMemorySources(t *testing.T, user *map[string]string) (request, limit resourcebounds.Source) {
	t.Helper()
	ctx := context.Background()
	h := &Handler{CRClient: crfake.NewClientBuilder().WithScheme(migrationScheme()).
		WithStatusSubresource(&kipperv1.App{}).WithReturnManagedFields().Build()}
	spec := map[string]interface{}{
		"image":     "registry.example.com/shop/web:v1",
		"port":      8080,
		"resources": map[string]interface{}{"memoryRequest": "512Mi", "memoryLimit": "2Gi"},
	}
	if err := h.createApp(ctx, "web", "shop-prod", spec, user); err != nil {
		t.Fatalf("createApp: %v", err)
	}
	var app kipperv1.App
	if err := h.CRClient.Get(ctx, crclient.ObjectKey{Namespace: "shop-prod", Name: "web"}, &app); err != nil {
		t.Fatal(err)
	}
	s, err := resourcebounds.AppSpec(&app)
	if err != nil {
		t.Fatal(err)
	}
	return s.MemoryRequest.Source, s.MemoryLimit.Source
}

func TestMigratedValuesTheSourceNamesAreTheUsers(t *testing.T) {
	user := map[string]string{"memoryRequest": "512Mi", "memoryLimit": "2Gi"}
	if req, lim := migratedMemorySources(t, &user); req != resourcebounds.User || lim != resourcebounds.User {
		t.Fatalf("memory sources = %v/%v, want the user's", req, lim)
	}
}

// An older source sends no list of the user's values, so the target cannot
// tell them from the old auto-sizer's: they arrive held.
func TestMigratedValuesFromAnOlderSourceAreHeld(t *testing.T) {
	if req, lim := migratedMemorySources(t, nil); req != resourcebounds.Held || lim != resourcebounds.Held {
		t.Fatalf("memory sources = %v/%v, want held", req, lim)
	}
}
