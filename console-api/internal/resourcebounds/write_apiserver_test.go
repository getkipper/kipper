package resourcebounds

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func crclientWithOwner(c client.Client, owner string) client.Client {
	return client.WithFieldOwner(c, owner)
}

func memoryEdit(req, lim string) *PairEdit { return &PairEdit{Request: req, Limit: lim} }

func TestWriteQuantitiesOnTheAPIServer(t *testing.T) {
	cfg := startAPIServer(t)
	consoleAPI := clientAs(t, cfg, consoleAPIUserAgent)
	kip := clientAs(t, cfg, kipUserAgent)
	console := clientAs(t, cfg, "kipper-console-test/v0.0.0")
	ctx := context.Background()
	const manager = "kipper-console"

	write := func(t *testing.T, name string, edits Edits) {
		t.Helper()
		if err := WriteQuantities(ctx, console, &kipperv1.App{}, "default", name, manager, edits); err != nil {
			t.Fatalf("WriteQuantities(%s): %v", name, err)
		}
	}
	classify := func(t *testing.T, name, field string) Source {
		t.Helper()
		app := getApp(t, kip, name)
		spec, err := AppSpec(app)
		if err != nil {
			t.Fatal(err)
		}
		switch field {
		case "cpuRequest":
			return spec.CPURequest.Source
		case "memoryRequest":
			return spec.MemoryRequest.Source
		default:
			return spec.MemoryLimit.Source
		}
	}

	t.Run("setting values makes them the user's", func(t *testing.T) {
		createApp(t, consoleAPI, "set", kipperv1.AppResources{})
		write(t, "set", Edits{Memory: memoryEdit("512Mi", "2Gi")})
		app := getApp(t, kip, "set")
		if app.Spec.Resources.MemoryRequest != "512Mi" || app.Spec.Resources.MemoryLimit != "2Gi" {
			t.Fatalf("memory = %s/%s, want 512Mi/2Gi", app.Spec.Resources.MemoryRequest, app.Spec.Resources.MemoryLimit)
		}
		assertOwners(t, app, "memoryRequest", manager)
		if got := classify(t, "set", "memoryRequest"); got != User {
			t.Fatalf("memoryRequest is %v, want user", got)
		}
	})

	t.Run("saving the value the old auto-sizer left makes it the user's", func(t *testing.T) {
		createApp(t, consoleAPI, "confirm", kipperv1.AppResources{MemoryRequest: "256Mi", MemoryLimit: "256Mi"})
		if got := classify(t, "confirm", "memoryRequest"); got != Automatic {
			t.Fatalf("before the save memoryRequest is %v, want automatic", got)
		}
		write(t, "confirm", Edits{Memory: memoryEdit("256Mi", "256Mi")})
		if got := classify(t, "confirm", "memoryRequest"); got != User {
			t.Fatalf("after a same-value save memoryRequest is %v, want user", got)
		}
	})

	t.Run("a memory edit keeps the CPU this manager set earlier", func(t *testing.T) {
		createApp(t, consoleAPI, "partial", kipperv1.AppResources{})
		write(t, "partial", Edits{CPU: &PairEdit{Request: "250m", Limit: "1"}})
		write(t, "partial", Edits{Memory: memoryEdit("512Mi", "1Gi")})
		app := getApp(t, kip, "partial")
		if app.Spec.Resources.CPURequest != "250m" || app.Spec.Resources.CPULimit != "1" {
			t.Fatalf("cpu = %s/%s, want the earlier 250m/1", app.Spec.Resources.CPURequest, app.Spec.Resources.CPULimit)
		}
		assertOwners(t, app, "cpuRequest", manager)
	})

	t.Run("a memory edit claims no CPU the user never chose", func(t *testing.T) {
		createApp(t, consoleAPI, "unclaimed", kipperv1.AppResources{CPURequest: "100m", CPULimit: "100m"})
		write(t, "unclaimed", Edits{Memory: memoryEdit("512Mi", "1Gi")})
		if got := classify(t, "unclaimed", "cpuRequest"); got != Automatic {
			t.Fatalf("cpuRequest is %v, want it still automatic", got)
		}
	})

	t.Run("changing a value kip set takes it over", func(t *testing.T) {
		createApp(t, kip, "takeover", kipperv1.AppResources{MemoryRequest: "512Mi", MemoryLimit: "512Mi"})
		write(t, "takeover", Edits{Memory: memoryEdit("1Gi", "2Gi")})
		app := getApp(t, kip, "takeover")
		if app.Spec.Resources.MemoryLimit != "2Gi" {
			t.Fatalf("memoryLimit = %s, want 2Gi", app.Spec.Resources.MemoryLimit)
		}
		assertOwners(t, app, "memoryLimit", manager)
	})

	t.Run("clearing hands a value back to automatic whoever set it", func(t *testing.T) {
		createApp(t, kip, "clear", kipperv1.AppResources{MemoryRequest: "512Mi", MemoryLimit: "2Gi", CPURequest: "100m"})
		write(t, "clear", Edits{Memory: &PairEdit{Clear: true}})
		app := getApp(t, kip, "clear")
		if app.Spec.Resources.MemoryRequest != "" || app.Spec.Resources.MemoryLimit != "" {
			t.Fatalf("memory = %s/%s, want both cleared", app.Spec.Resources.MemoryRequest, app.Spec.Resources.MemoryLimit)
		}
		if app.Spec.Resources.CPURequest != "100m" {
			t.Fatalf("cpuRequest = %q, want it untouched", app.Spec.Resources.CPURequest)
		}
	})

	t.Run("clearing values this manager set and setting others in one save", func(t *testing.T) {
		createApp(t, consoleAPI, "mixed", kipperv1.AppResources{})
		write(t, "mixed", Edits{CPU: &PairEdit{Request: "250m", Limit: "250m"}})
		write(t, "mixed", Edits{CPU: &PairEdit{Clear: true}, Memory: memoryEdit("512Mi", "512Mi")})
		app := getApp(t, kip, "mixed")
		if app.Spec.Resources.CPURequest != "" || app.Spec.Resources.MemoryRequest != "512Mi" {
			t.Fatalf("cpu %q memory %q, want cpu cleared and memory 512Mi", app.Spec.Resources.CPURequest, app.Spec.Resources.MemoryRequest)
		}
	})
}

// A copy creates the target with every carried value held, then claims the
// user's. Values the claim does not reach stay held, and a claim that finds the
// target changed leaves the user's edit alone.
func TestCarriedValuesAreHeldUntilClaimedOnTheAPIServer(t *testing.T) {
	cfg := startAPIServer(t)
	ctx := context.Background()
	kip := clientAs(t, cfg, kipUserAgent)
	copier := clientAs(t, cfg, "kipper-copy-test/v0.0.0")

	create := func(t *testing.T, name string) *kipperv1.App {
		t.Helper()
		app := &kipperv1.App{}
		app.Name, app.Namespace = name, "default"
		app.Spec.Image = "registry.example.com/web:1"
		app.Spec.Resources = kipperv1.AppResources{CPURequest: "300m", MemoryRequest: "512Mi", MemoryLimit: "2Gi"}
		if err := crclientWithOwner(copier, HeldManager).Create(ctx, app); err != nil {
			t.Fatal(err)
		}
		return app
	}
	sources := func(t *testing.T, name string) Spec {
		t.Helper()
		spec, err := AppSpec(getApp(t, kip, name))
		if err != nil {
			t.Fatal(err)
		}
		return spec
	}

	t.Run("claimed values are the user's, the rest stay held", func(t *testing.T) {
		app := create(t, "claimed")
		if s := sources(t, "claimed"); s.MemoryRequest.Source != Held || s.CPURequest.Source != Held {
			t.Fatalf("before the claim: memory %v, cpu %v; want both held", s.MemoryRequest.Source, s.CPURequest.Source)
		}
		if err := ClaimQuantities(ctx, copier, &kipperv1.App{}, "default", "claimed", CopyManager, app.ResourceVersion,
			map[string]string{"memoryRequest": "512Mi", "memoryLimit": "2Gi"}); err != nil {
			t.Fatal(err)
		}
		s := sources(t, "claimed")
		if s.MemoryRequest.Source != User || s.MemoryLimit.Source != User || s.CPURequest.Source != Held {
			t.Fatalf("after the claim: memory %v/%v, cpu %v; want user, user, held", s.MemoryRequest.Source, s.MemoryLimit.Source, s.CPURequest.Source)
		}
	})

	t.Run("a claim that finds the target changed keeps the user's edit", func(t *testing.T) {
		app := create(t, "edited")
		// The user sets memory on the new App before the copy's claim lands.
		edited := getApp(t, kip, "edited")
		edited.Spec.Resources.MemoryLimit = "4Gi"
		if err := kip.Update(ctx, edited); err != nil {
			t.Fatal(err)
		}
		err := ClaimQuantities(ctx, copier, &kipperv1.App{}, "default", "edited", CopyManager, app.ResourceVersion,
			map[string]string{"memoryRequest": "512Mi", "memoryLimit": "2Gi"})
		if !apierrors.IsConflict(err) {
			t.Fatalf("claim with a stale version: err = %v, want a conflict", err)
		}
		if got := getApp(t, kip, "edited").Spec.Resources.MemoryLimit; got != "4Gi" {
			t.Fatalf("memoryLimit = %s, want the user's 4Gi kept", got)
		}
	})
}
