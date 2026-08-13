package cmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/getkipper/kipper/kip/internal/function"
	"github.com/getkipper/kipper/kip/internal/manifest"
)

func namespace(name string, managed bool) *corev1.Namespace {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if managed {
		ns.Labels = map[string]string{"app.kubernetes.io/managed-by": "kipper"}
	}
	return ns
}

// `kip function create` without --project writes the function to the default
// namespace, so a list that sweeps only Kipper-labelled namespaces cannot see
// it. On an installed cluster there is always at least one labelled namespace,
// so the old fallback to default never ran and the function was invisible.
func TestFunctionListNamespaces_IncludesDefault(t *testing.T) {
	cs := k8sfake.NewSimpleClientset(
		namespace("kipper-builds", true),
		namespace("shop-prod", true),
		namespace("default", false),
	)

	got, err := functionListNamespaces(context.Background(), cs)
	require.NoError(t, err)

	assert.Contains(t, got, "default",
		"a function created without --project lives in default and would not be listed")
	assert.Contains(t, got, "shop-prod")
	assert.Contains(t, got, "kipper-builds")
}

// A cluster where default carries the Kipper label must not list it twice.
func TestFunctionListNamespaces_DoesNotRepeatDefault(t *testing.T) {
	cs := k8sfake.NewSimpleClientset(
		namespace("default", true),
		namespace("shop-prod", true),
	)

	got, err := functionListNamespaces(context.Background(), cs)
	require.NoError(t, err)

	seen := 0
	for _, ns := range got {
		if ns == "default" {
			seen++
		}
	}
	assert.Equal(t, 1, seen, "default was swept twice: %v", got)
}

// A sweep across a cluster printed "No functions in <ns>" once per empty
// namespace, so the one project that had a function was buried in the noise.
// Collecting first is what lets the command answer in one table.
func TestCollectFunctions_GathersAcrossNamespacesInOrder(t *testing.T) {
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(manifest.FunctionGVR.GroupVersion().WithKind("Function"), &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(manifest.FunctionGVR.GroupVersion().WithKind("FunctionList"), &unstructured.UnstructuredList{})

	fn := func(ns, name string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "kipper.run/v1alpha1",
			"kind":       "Function",
			"metadata":   map[string]any{"name": name, "namespace": ns},
			"spec":       map[string]any{"image": name + ":v1"},
		}}
	}
	dyn := dynamicfake.NewSimpleDynamicClient(scheme, fn("default", "hello"), fn("shop-prod", "checkout"))
	mgr := &function.Manager{Dynamic: dyn}

	got, err := collectFunctions(context.Background(), mgr, []string{"shop-prod", "kipper-builds", "default"})
	require.NoError(t, err)
	require.Len(t, got, 2, "an empty namespace in the sweep must contribute nothing rather than a message")

	assert.Equal(t, "shop-prod", got[0].namespace)
	assert.Equal(t, "checkout", got[0].fn.Name)
	assert.Equal(t, "default", got[1].namespace)
	assert.Equal(t, "hello", got[1].fn.Name,
		"a function created without --project lives in default and must appear in the sweep")
}
