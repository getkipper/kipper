package v1alpha1

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/getkipper/kipper/console-api/internal/apiservertest"
	"github.com/getkipper/kipper/controller/pkg/platform"
)

const sharedVolumesPolicy = "../../../deploy/policies/kipper-shared-volumes.yaml"

func applyPolicy(ctx context.Context, t *testing.T, c crclient.Client) {
	t.Helper()
	data, err := os.ReadFile(sharedVolumesPolicy)
	if err != nil {
		t.Fatal(err)
	}
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	for {
		obj := &unstructured.Unstructured{}
		if err := dec.Decode(&obj.Object); err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			t.Fatal(err)
		}
		if len(obj.Object) == 0 {
			continue
		}
		if err := c.Create(ctx, obj); err != nil {
			t.Fatalf("creating %s: %v", obj.GetKind(), err)
		}
	}
}

func TestSharedVolumesPolicy(t *testing.T) {
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
	applyPolicy(ctx, t, c)

	volume := func(name string) *Volume {
		return &Volume{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}, Spec: VolumeSpec{Size: "1Gi"}}
	}
	mount := AppVolumeMount{Name: "uploads", MountPath: "/data"}
	app := func(name string, mounts ...AppVolumeMount) *App {
		return &App{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec:       AppSpec{Image: "registry.example.com/web:1", Port: 8080, Volumes: mounts},
		}
	}
	setLocal := func() {
		t.Helper()
		pc := &PlatformConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "platform"},
			Spec:       PlatformConfigSpec{Profile: "nano", Storage: "local"},
		}
		if err := c.Create(ctx, pc); err != nil {
			t.Fatal(err)
		}
	}
	// The policy and its parameter reach the admission chain asynchronously,
	// so wait until a fresh volume gets the expected answer.
	probe := 0
	waitFor := func(refused bool) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			probe++
			err := c.Create(ctx, volume(fmt.Sprintf("probe-%d", probe)))
			if (err != nil) == refused {
				return
			}
			time.Sleep(250 * time.Millisecond)
		}
		t.Fatalf("volume creation never became refused=%v", refused)
	}

	setLocal()
	waitFor(true)

	t.Run("a shared volume is refused with the message the console and kip show", func(t *testing.T) {
		err := c.Create(ctx, volume("uploads"))
		if err == nil || !strings.Contains(err.Error(), platform.SharedVolumesUnsupported) {
			t.Fatalf("want the shared-volumes refusal, got %v", err)
		}
	})

	t.Run("an app or function that mounts a shared volume is refused", func(t *testing.T) {
		if err := c.Create(ctx, app("mounting", mount)); err == nil {
			t.Fatal("an app mounting a shared volume was accepted")
		}
		fn := &Function{
			ObjectMeta: metav1.ObjectMeta{Name: "mounting", Namespace: "default"},
			Spec:       FunctionSpec{Image: "registry.example.com/fn:1", Port: 8080, Volumes: []AppVolumeMount{mount}},
		}
		if err := c.Create(ctx, fn); err == nil {
			t.Fatal("a function mounting a shared volume was accepted")
		}
	})

	t.Run("an app without shared volumes is accepted and can change", func(t *testing.T) {
		a := app("plain")
		if err := c.Create(ctx, a); err != nil {
			t.Fatalf("creating a plain app: %v", err)
		}
		a.Spec.Port = 9090
		if err := c.Update(ctx, a); err != nil {
			t.Fatalf("updating a plain app: %v", err)
		}
		a.Spec.Volumes = []AppVolumeMount{mount}
		if err := c.Update(ctx, a); err == nil {
			t.Fatal("adding a shared volume to an app was accepted")
		}
	})

	// Allow requests when PlatformConfig is missing for compatibility with older clusters.
	pc := &PlatformConfig{ObjectMeta: metav1.ObjectMeta{Name: "platform"}}
	if err := c.Delete(ctx, pc); err != nil {
		t.Fatal(err)
	}
	waitFor(false)
	if err := c.Create(ctx, volume("kept")); err != nil {
		t.Fatalf("creating a volume without a PlatformConfig: %v", err)
	}

	setLocal()
	waitFor(true)

	t.Run("an existing volume can still be updated, so deletion is never blocked", func(t *testing.T) {
		v := &Volume{}
		if err := c.Get(ctx, crclient.ObjectKey{Namespace: "default", Name: "kept"}, v); err != nil {
			t.Fatal(err)
		}
		v.Finalizers = []string{"kipper.run/volume-cleanup"}
		if err := c.Update(ctx, v); err != nil {
			t.Fatalf("updating an existing volume: %v", err)
		}
		v.Finalizers = nil
		if err := c.Update(ctx, v); err != nil {
			t.Fatalf("removing the finalizer: %v", err)
		}
		if err := c.Delete(ctx, v); err != nil && !apierrors.IsNotFound(err) {
			t.Fatalf("deleting the volume: %v", err)
		}
	})
}
