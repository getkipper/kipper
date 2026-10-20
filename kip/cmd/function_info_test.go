package cmd

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/getkipper/kipper/kip/internal/function"
	"github.com/getkipper/kipper/kip/internal/manifest"
)

func TestFunctionInfoShowsTheFunctionAndItsNodes(t *testing.T) {
	var out bytes.Buffer
	fn := function.Status{Name: "resize", Trigger: "http", Image: "registry.example.com/resize:v1", Ready: 2, Status: "ready", URL: "https://resize.example.com"}

	writeFunctionInfo(&out, fn, "server-1 (1 pod), worker-1 (1 pod)", nil)

	assert.Equal(t, "\n  Function: resize\n\n"+
		"  Status:  ready, 2 ready\n"+
		"  Trigger: http\n"+
		"  Image:   registry.example.com/resize:v1\n"+
		"  URL:     https://resize.example.com\n"+
		"  Nodes:   server-1 (1 pod), worker-1 (1 pod)\n\n", out.String())
}

func TestFunctionInfoLeavesOutWhatIsNotThere(t *testing.T) {
	var out bytes.Buffer
	writeFunctionInfo(&out, function.Status{Name: "nightly", Trigger: "cron", Image: "nightly:v1", Status: "idle"}, "", nil)

	assert.NotContains(t, out.String(), "URL:")
	assert.NotContains(t, out.String(), "Nodes:")
}

func TestFunctionManagerGet(t *testing.T) {
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(manifest.FunctionGVR.GroupVersion().WithKind("Function"), &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(manifest.FunctionGVR.GroupVersion().WithKind("FunctionList"), &unstructured.UnstructuredList{})
	dyn := dynamicfake.NewSimpleDynamicClient(scheme, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kipper.run/v1alpha1",
		"kind":       "Function",
		"metadata":   map[string]any{"name": "resize", "namespace": "shop"},
		"spec":       map[string]any{"image": "resize:v1"},
	}})
	mgr := &function.Manager{Dynamic: dyn}

	got, err := mgr.Get(context.Background(), "shop", "resize")
	require.NoError(t, err)
	assert.Equal(t, "resize:v1", got.Image)

	_, err = mgr.Get(context.Background(), "shop", "missing")
	assert.EqualError(t, err, `function "missing" not found`)
}
