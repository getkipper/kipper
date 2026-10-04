package v1alpha1

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/getkipper/kipper/console-api/internal/apiservertest"
)

func TestAppAutoscaleValidation(t *testing.T) {
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

	unstructuredApp := func(name string, autoscale map[string]any) *unstructured.Unstructured {
		u := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": GroupVersion.String(),
			"kind":       "App",
			"metadata":   map[string]any{"name": name, "namespace": "default"},
			"spec": map[string]any{
				"image":     "registry.example.com/web:1",
				"port":      int64(8080),
				"autoscale": autoscale,
			},
		}}
		return u
	}

	tests := []struct {
		name      string
		autoscale map[string]any
		wantErr   string
	}{
		{name: "enabled with cpu", autoscale: map[string]any{"enabled": true, "minReplicas": int64(2), "maxReplicas": int64(5), "cpuTarget": int64(70)}},
		{name: "enabled with memory only", autoscale: map[string]any{"enabled": true, "maxReplicas": int64(3), "memoryTarget": int64(80)}},
		{name: "enabled with min equal to max", autoscale: map[string]any{"enabled": true, "minReplicas": int64(4), "maxReplicas": int64(4), "cpuTarget": int64(50)}},
		{name: "enabled with one unused zero target", autoscale: map[string]any{"enabled": true, "maxReplicas": int64(3), "cpuTarget": int64(0), "memoryTarget": int64(80)}},
		{name: "enabled with a target above 100", autoscale: map[string]any{"enabled": true, "maxReplicas": int64(3), "cpuTarget": int64(250)}},
		{name: "disabled without bounds", autoscale: map[string]any{"enabled": false}},
		{name: "disabled with bounds", autoscale: map[string]any{"enabled": false, "minReplicas": int64(2), "maxReplicas": int64(6)}},
		{name: "disabled with the default min and no max", autoscale: map[string]any{"enabled": false, "minReplicas": int64(1)}},
		{name: "disabled with min and no max", autoscale: map[string]any{"enabled": false, "minReplicas": int64(3)}, wantErr: "set maxReplicas with minReplicas, or leave both out to remove the bounds"},
		{name: "disabled with stale targets", autoscale: map[string]any{"enabled": false, "maxReplicas": int64(3), "cpuTarget": int64(-5), "memoryTarget": int64(0)}},
		{name: "enabled without max", autoscale: map[string]any{"enabled": true, "cpuTarget": int64(70)}, wantErr: "maxReplicas is required"},
		{name: "enabled with max zero", autoscale: map[string]any{"enabled": true, "maxReplicas": int64(0), "cpuTarget": int64(70)}, wantErr: "maxReplicas must be at least 1"},
		{name: "disabled with max zero", autoscale: map[string]any{"enabled": false, "maxReplicas": int64(0)}, wantErr: "maxReplicas must be at least 1"},
		{name: "disabled with negative max", autoscale: map[string]any{"enabled": false, "maxReplicas": int64(-1)}, wantErr: "maxReplicas must be at least 1"},
		{name: "explicit min zero", autoscale: map[string]any{"enabled": false, "minReplicas": int64(0)}, wantErr: "minReplicas must be at least 1"},
		{name: "negative min", autoscale: map[string]any{"enabled": true, "minReplicas": int64(-2), "maxReplicas": int64(3), "cpuTarget": int64(70)}, wantErr: "minReplicas must be at least 1"},
		{name: "enabled with min above max", autoscale: map[string]any{"enabled": true, "minReplicas": int64(5), "maxReplicas": int64(3), "cpuTarget": int64(70)}, wantErr: "minReplicas must not exceed maxReplicas"},
		{name: "disabled with min above max", autoscale: map[string]any{"enabled": false, "minReplicas": int64(5), "maxReplicas": int64(3)}, wantErr: "minReplicas must not exceed maxReplicas"},
		{name: "enabled without targets", autoscale: map[string]any{"enabled": true, "maxReplicas": int64(3)}, wantErr: "set cpuTarget or memoryTarget"},
		{name: "enabled with zero targets", autoscale: map[string]any{"enabled": true, "maxReplicas": int64(3), "cpuTarget": int64(0), "memoryTarget": int64(0)}, wantErr: "set cpuTarget or memoryTarget"},
		{name: "enabled with a negative cpu target", autoscale: map[string]any{"enabled": true, "maxReplicas": int64(3), "cpuTarget": int64(-1), "memoryTarget": int64(80)}, wantErr: "targets must not be negative"},
		{name: "enabled with a negative memory target", autoscale: map[string]any{"enabled": true, "maxReplicas": int64(3), "cpuTarget": int64(70), "memoryTarget": int64(-1)}, wantErr: "targets must not be negative"},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := c.Create(ctx, unstructuredApp(fmt.Sprintf("autoscale-%d", i), tt.autoscale))
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

	t.Run("an app without a block is accepted", func(t *testing.T) {
		app := &App{
			ObjectMeta: metav1.ObjectMeta{Name: "autoscale-none", Namespace: "default"},
			Spec:       AppSpec{Image: "registry.example.com/web:1", Port: 8080},
		}
		if err := c.Create(ctx, app); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a stored invalid block is kept while other fields change", func(t *testing.T) {
		invalid := map[string]any{"enabled": true, "minReplicas": int64(5), "maxReplicas": int64(3)}
		withoutAutoscaleRules(t, ctx, c, func() {
			if err := c.Create(ctx, unstructuredApp("autoscale-stored", invalid)); err != nil {
				t.Fatalf("creating the stored invalid app: %v", err)
			}
		})

		app := &App{}
		key := crclient.ObjectKey{Namespace: "default", Name: "autoscale-stored"}
		if err := c.Get(ctx, key, app); err != nil {
			t.Fatal(err)
		}
		app.Spec.Image = "registry.example.com/web:2"
		if err := c.Update(ctx, app); err != nil {
			t.Fatalf("an image deploy with the stored block unchanged was refused: %v", err)
		}

		if err := c.Get(ctx, key, app); err != nil {
			t.Fatal(err)
		}
		app.Spec.Autoscale.MaxReplicas = ptr.To[int32](4)
		err := c.Update(ctx, app)
		if err == nil || !strings.Contains(err.Error(), "minReplicas must not exceed maxReplicas") {
			t.Fatalf("changing the block without repairing it gave %v, want the min and max refusal", err)
		}

		if err := c.Get(ctx, key, app); err != nil {
			t.Fatal(err)
		}
		app.Spec.Autoscale.MaxReplicas = ptr.To[int32](6)
		app.Spec.Autoscale.CPUTarget = ptr.To[int32](70)
		if err := c.Update(ctx, app); err != nil {
			t.Fatalf("repairing the block was refused: %v", err)
		}
	})

	t.Run("a stored invalid block with an explicit zero target is kept while other fields change", func(t *testing.T) {
		invalid := map[string]any{"enabled": true, "minReplicas": int64(5), "maxReplicas": int64(3), "cpuTarget": int64(70), "memoryTarget": int64(0)}
		withoutAutoscaleRules(t, ctx, c, func() {
			if err := c.Create(ctx, unstructuredApp("autoscale-stored-zero", invalid)); err != nil {
				t.Fatalf("creating the stored invalid app: %v", err)
			}
		})

		app := &App{}
		key := crclient.ObjectKey{Namespace: "default", Name: "autoscale-stored-zero"}
		if err := c.Get(ctx, key, app); err != nil {
			t.Fatal(err)
		}
		app.Spec.Image = "registry.example.com/web:2"
		if err := c.Update(ctx, app); err != nil {
			t.Fatalf("an image deploy with the stored block unchanged was refused: %v", err)
		}
	})
}

var crdGVK = schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"}

// withoutAutoscaleRules removes the autoscale CEL rules from the App CRD while
// fn runs, so a test can store a block that the rules would refuse today.
func withoutAutoscaleRules(t *testing.T, ctx context.Context, c crclient.Client, fn func()) {
	t.Helper()
	path := []string{"spec", "properties", "autoscale", "x-kubernetes-validations"}
	edit := func(change func(schema map[string]any) error) {
		t.Helper()
		crd := &unstructured.Unstructured{}
		crd.SetGroupVersionKind(crdGVK)
		if err := c.Get(ctx, crclient.ObjectKey{Name: "apps.kipper.run"}, crd); err != nil {
			t.Fatal(err)
		}
		versions, _, _ := unstructured.NestedSlice(crd.Object, "spec", "versions")
		for _, v := range versions {
			s, _, _ := unstructured.NestedMap(v.(map[string]any), "schema", "openAPIV3Schema")
			if err := change(s); err != nil {
				t.Fatal(err)
			}
			if err := unstructured.SetNestedMap(v.(map[string]any), s, "schema", "openAPIV3Schema"); err != nil {
				t.Fatal(err)
			}
		}
		if err := unstructured.SetNestedSlice(crd.Object, versions, "spec", "versions"); err != nil {
			t.Fatal(err)
		}
		if err := c.Update(ctx, crd); err != nil {
			t.Fatal(err)
		}
	}

	var saved []any
	edit(func(s map[string]any) error {
		rules, found, err := unstructured.NestedSlice(s, append([]string{"properties"}, path...)...)
		if err != nil || !found {
			return fmt.Errorf("the App CRD has no autoscale rules to remove (found %v, err %v)", found, err)
		}
		saved = rules
		unstructured.RemoveNestedField(s, append([]string{"properties"}, path...)...)
		return nil
	})
	probe := map[string]any{"enabled": true, "maxReplicas": int64(3)}
	waitForAutoscaleRules(t, ctx, c, probe, false)
	fn()
	edit(func(s map[string]any) error {
		return unstructured.SetNestedSlice(s, saved, append([]string{"properties"}, path...)...)
	})
	waitForAutoscaleRules(t, ctx, c, probe, true)
}

// waitForAutoscaleRules waits until the API server serves the CRD with or
// without the rules, by dry-running a create the rules refuse.
func waitForAutoscaleRules(t *testing.T, ctx context.Context, c crclient.Client, probe map[string]any, enforced bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		u := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": GroupVersion.String(),
			"kind":       "App",
			"metadata":   map[string]any{"name": "autoscale-probe", "namespace": "default"},
			"spec":       map[string]any{"image": "registry.example.com/web:1", "port": int64(8080), "autoscale": probe},
		}}
		err := c.Create(ctx, u, crclient.DryRunAll)
		if (err != nil) == enforced {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the App CRD did not switch to enforced=%v within 30s (last error %v)", enforced, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
