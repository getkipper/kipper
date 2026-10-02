package resourcebounds

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/internal/apiservertest"
	"github.com/getkipper/kipper/controller/pkg/fieldowners"
)

// These tests pin the API server's field-management behaviour that resource
// bounds rely on to tell a user's values from automatic ones.

const (
	consoleAPIUserAgent = "console-api/v0.0.0 (linux/amd64) kubernetes/unknown"
	kipUserAgent        = "kip/v0.0.0 (darwin/arm64) kubernetes/unknown"
)

func clientAs(t *testing.T, cfg *rest.Config, userAgent string) crclient.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := kipperv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	withAgent := rest.CopyConfig(cfg)
	withAgent.UserAgent = userAgent
	c, err := crclient.New(withAgent, crclient.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func createApp(t *testing.T, c crclient.Client, name string, res kipperv1.AppResources) {
	t.Helper()
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       kipperv1.AppSpec{Image: "registry.example.com/web:1", Resources: res},
	}
	if err := c.Create(context.Background(), app); err != nil {
		t.Fatalf("creating app %s: %v", name, err)
	}
}

func getApp(t *testing.T, c crclient.Client, name string) *kipperv1.App {
	t.Helper()
	var app kipperv1.App
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, &app); err != nil {
		t.Fatalf("getting app %s: %v", name, err)
	}
	return &app
}

func applyResources(t *testing.T, c crclient.Client, name, manager string, resources map[string]any) {
	t.Helper()
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": kipperv1.GroupVersion.String(),
		"kind":       "App",
		"metadata":   map[string]any{"name": name, "namespace": "default"},
		"spec":       map[string]any{"resources": resources},
	}}
	err := c.Apply(context.Background(), crclient.ApplyConfigurationFromUnstructured(obj), crclient.FieldOwner(manager), crclient.ForceOwnership)
	if err != nil {
		t.Fatalf("applying resources to %s as %s: %v", name, manager, err)
	}
}

// removeMemoryRequest is a JSON patch that removes memoryRequest only if the
// object is still at resourceVersion.
func removeMemoryRequest(resourceVersion string) crclient.Patch {
	return crclient.RawPatch(types.JSONPatchType, []byte(fmt.Sprintf(
		`[{"op":"test","path":"/metadata/resourceVersion","value":%q},`+
			`{"op":"remove","path":"/spec/resources/memoryRequest"}]`, resourceVersion)))
}

func assertOwners(t *testing.T, app *kipperv1.App, field string, want ...string) {
	t.Helper()
	got := fieldowners.Owners(app.ManagedFields, "spec", "resources", field)
	if len(want) == 0 {
		want = nil
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("owners of %s = %v, want %v", field, got, want)
	}
}

func TestFieldOwnershipOnTheAPIServer(t *testing.T) {
	cfg := apiservertest.Start(t)
	consoleAPI := clientAs(t, cfg, consoleAPIUserAgent)
	kip := clientAs(t, cfg, kipUserAgent)
	ctx := context.Background()

	t.Run("an update that leaves a quantity unchanged does not take it", func(t *testing.T) {
		createApp(t, kip, "unchanged", kipperv1.AppResources{MemoryRequest: "512Mi", MemoryLimit: "2Gi"})
		assertOwners(t, getApp(t, kip, "unchanged"), "memoryRequest", "kip")

		app := getApp(t, consoleAPI, "unchanged")
		app.Spec.Image = "registry.example.com/web:2"
		if err := consoleAPI.Update(ctx, app); err != nil {
			t.Fatal(err)
		}
		after := getApp(t, kip, "unchanged")
		assertOwners(t, after, "memoryRequest", "kip")
		assertOwners(t, after, "memoryLimit", "kip")
	})

	t.Run("an update that changes a quantity takes it", func(t *testing.T) {
		createApp(t, kip, "changed", kipperv1.AppResources{MemoryRequest: "512Mi", MemoryLimit: "2Gi"})
		app := getApp(t, consoleAPI, "changed")
		app.Spec.Resources.MemoryRequest = "256Mi"
		if err := consoleAPI.Update(ctx, app); err != nil {
			t.Fatal(err)
		}
		after := getApp(t, kip, "changed")
		assertOwners(t, after, "memoryRequest", "console-api")
		assertOwners(t, after, "memoryLimit", "kip")
	})

	t.Run("a same-value apply shares the quantity", func(t *testing.T) {
		createApp(t, consoleAPI, "shared", kipperv1.AppResources{MemoryRequest: "256Mi", MemoryLimit: "256Mi"})
		applyResources(t, kip, "shared", "kipper-console", map[string]any{"memoryRequest": "256Mi"})
		assertOwners(t, getApp(t, kip, "shared"), "memoryRequest", "console-api", "kipper-console")
	})

	t.Run("a forced apply of a new value takes the quantity alone", func(t *testing.T) {
		createApp(t, consoleAPI, "forced", kipperv1.AppResources{MemoryRequest: "256Mi", MemoryLimit: "256Mi"})
		applyResources(t, kip, "forced", "kipper-console", map[string]any{"memoryRequest": "512Mi"})
		after := getApp(t, kip, "forced")
		if after.Spec.Resources.MemoryRequest != "512Mi" {
			t.Fatalf("memoryRequest = %q, want 512Mi", after.Spec.Resources.MemoryRequest)
		}
		assertOwners(t, after, "memoryRequest", "kipper-console")
	})

	t.Run("a remove patch clears the quantity for every owner", func(t *testing.T) {
		createApp(t, consoleAPI, "cleared", kipperv1.AppResources{MemoryRequest: "256Mi", MemoryLimit: "256Mi"})
		applyResources(t, kip, "cleared", "kipper-console", map[string]any{"memoryRequest": "256Mi"})
		app := getApp(t, kip, "cleared")
		assertOwners(t, app, "memoryRequest", "console-api", "kipper-console")
		if err := kip.Patch(ctx, app, removeMemoryRequest(app.ResourceVersion), crclient.FieldOwner("kipper-console")); err != nil {
			t.Fatal(err)
		}
		after := getApp(t, kip, "cleared")
		if after.Spec.Resources.MemoryRequest != "" {
			t.Fatalf("memoryRequest = %q, want it removed", after.Spec.Resources.MemoryRequest)
		}
		assertOwners(t, after, "memoryRequest")
		assertOwners(t, after, "memoryLimit", "console-api")
	})

	t.Run("a remove patch built from a stale read is refused", func(t *testing.T) {
		createApp(t, consoleAPI, "stale", kipperv1.AppResources{MemoryRequest: "256Mi"})
		stale := getApp(t, kip, "stale")
		applyResources(t, kip, "stale", "kip", map[string]any{"memoryRequest": "512Mi"})

		err := kip.Patch(ctx, stale, removeMemoryRequest(stale.ResourceVersion))
		if !apierrors.IsInvalid(err) {
			t.Fatalf("remove patch with a stale resourceVersion: err = %v, want the JSON patch test to fail as Invalid", err)
		}
		fresh := getApp(t, kip, "stale")
		if fresh.Spec.Resources.MemoryRequest != "512Mi" {
			t.Fatalf("memoryRequest = %q, want the concurrent 512Mi to survive", fresh.Spec.Resources.MemoryRequest)
		}

		// The same patch built from a fresh read succeeds, so the resourceVersion test caused the refusal.
		if err := kip.Patch(ctx, fresh, removeMemoryRequest(fresh.ResourceVersion)); err != nil {
			t.Fatalf("remove patch with a fresh resourceVersion: %v", err)
		}
		if got := getApp(t, kip, "stale").Spec.Resources.MemoryRequest; got != "" {
			t.Fatalf("memoryRequest = %q, want it removed", got)
		}
	})

	t.Run("the first apply to an object without managedFields assigns before-first-apply", func(t *testing.T) {
		createApp(t, consoleAPI, "unowned", kipperv1.AppResources{CPURequest: "100m", MemoryRequest: "256Mi"})
		app := getApp(t, consoleAPI, "unowned")
		app.ManagedFields = []metav1.ManagedFieldsEntry{{}}
		if err := consoleAPI.Update(ctx, app); err != nil {
			t.Fatal(err)
		}
		if mf := getApp(t, kip, "unowned").ManagedFields; len(mf) != 0 {
			t.Fatalf("managedFields = %v, want them stripped", mf)
		}

		applyResources(t, kip, "unowned", "kipper-console", map[string]any{"memoryRequest": "512Mi"})
		after := getApp(t, kip, "unowned")
		assertOwners(t, after, "cpuRequest", "before-first-apply")
		assertOwners(t, after, "memoryRequest", "kipper-console")
	})
}
