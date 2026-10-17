package v1alpha1

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/getkipper/kipper/console-api/internal/apiservertest"
)

func TestPlatformConfigStorageSchema(t *testing.T) {
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
	// Each case uses a separate object because schema validation applies to every name.
	create := func(t *testing.T, name, storage string) *PlatformConfig {
		t.Helper()
		pc := &PlatformConfig{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       PlatformConfigSpec{Profile: "nano", Storage: storage},
		}
		if err := c.Create(ctx, pc); err != nil {
			t.Fatalf("creating with storage %q: %v", storage, err)
		}
		return pc
	}
	update := func(pc *PlatformConfig, storage string) error {
		pc.Spec.Storage = storage
		return c.Update(ctx, pc)
	}

	t.Run("a new cluster records either mode", func(t *testing.T) {
		create(t, "new-local", "local")
		create(t, "new-longhorn", "longhorn")
	})

	t.Run("an unknown mode is refused", func(t *testing.T) {
		pc := &PlatformConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "unknown"},
			Spec:       PlatformConfigSpec{Profile: "nano", Storage: "nfs"},
		}
		if err := c.Create(ctx, pc); err == nil {
			t.Fatal("storage nfs was accepted")
		}
	})

	t.Run("a legacy cluster can record longhorn, which it already runs", func(t *testing.T) {
		pc := create(t, "legacy-adopted", "")
		if err := update(pc, "longhorn"); err != nil {
			t.Fatalf("adopting longhorn: %v", err)
		}
	})

	t.Run("a legacy cluster cannot switch to local", func(t *testing.T) {
		pc := create(t, "legacy-switch", "")
		if err := update(pc, "local"); err == nil {
			t.Fatal("a legacy Longhorn cluster was switched to local storage")
		}
	})

	t.Run("a recorded mode never changes or disappears", func(t *testing.T) {
		local := create(t, "recorded-local", "local")
		if err := update(local, "longhorn"); err == nil {
			t.Fatal("local storage was switched to longhorn")
		}
		local = &PlatformConfig{}
		if err := c.Get(ctx, crclient.ObjectKey{Name: "recorded-local"}, local); err != nil {
			t.Fatal(err)
		}
		if err := update(local, ""); err == nil {
			t.Fatal("the recorded mode was removed, which would read as longhorn")
		}
	})

	t.Run("removing the whole spec does not drop a recorded mode", func(t *testing.T) {
		pc := create(t, "spec-removed", "local")
		err := c.Patch(ctx, pc, crclient.RawPatch(types.MergePatchType, []byte(`{"spec":null}`)))
		if err == nil {
			t.Fatal("the spec, and with it the recorded mode, was removed")
		}
	})

	t.Run("the backup mode takes only the three named values", func(t *testing.T) {
		pc := &PlatformConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "backup-unknown"},
			Spec:       PlatformConfigSpec{Profile: "nano", Storage: "local", Backup: "tape"},
		}
		if err := c.Create(ctx, pc); err == nil {
			t.Fatal("backup tape was accepted")
		}
		pc = &PlatformConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "backup-none"},
			Spec:       PlatformConfigSpec{Profile: "nano", Storage: "local", Backup: "none"},
		}
		if err := c.Create(ctx, pc); err != nil {
			t.Fatalf("creating with backup none: %v", err)
		}
		pc.Spec.Backup = "external"
		if err := c.Update(ctx, pc); err != nil {
			t.Fatalf("adding external backup storage later: %v", err)
		}
	})

	t.Run("other spec changes keep working", func(t *testing.T) {
		pc := create(t, "profile-change", "local")
		pc.Spec.Profile = "small"
		if err := c.Update(ctx, pc); err != nil {
			t.Fatalf("changing the profile: %v", err)
		}
	})
}
