package cmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/getkipper/kipper/kip/internal/manifest"
)

func applyCollisionScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	for gvr, kind := range map[schema.GroupVersionResource]string{
		manifest.AppGVR:      "App",
		manifest.FunctionGVR: "Function",
		manifest.JobGVR:      "Job",
	} {
		s.AddKnownTypeWithName(gvr.GroupVersion().WithKind(kind), &unstructured.Unstructured{})
		s.AddKnownTypeWithName(gvr.GroupVersion().WithKind(kind+"List"), &unstructured.UnstructuredList{})
	}
	return s
}

func workloadObject(gvr schema.GroupVersionResource, kind string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": gvr.GroupVersion().String(),
		"kind":       kind,
		"metadata":   map[string]any{"name": "checkout", "namespace": "shop-prod"},
	}}
}

// A manifest can declare any kind, so apply is the one path that reaches all
// three. Guarding the per-kind commands and leaving this open would let a
// kipper.yaml do what the commands refuse.
func TestApplyResourceRefusesANameAnotherKindHolds(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(applyCollisionScheme(),
		workloadObject(manifest.AppGVR, "App"))

	res := manifest.Resource{
		GVR:    manifest.FunctionGVR,
		Object: workloadObject(manifest.FunctionGVR, "Function"),
	}
	_, err := applyResource(context.Background(), dyn, "shop-prod", res, false, nil, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already used by an app")

	_, getErr := dyn.Resource(manifest.FunctionGVR).Namespace("shop-prod").
		Get(context.Background(), "checkout", metav1.GetOptions{})
	assert.Error(t, getErr, "the refused function was applied anyway")
}

// Apply is the re-apply path for everything Kipper manages, so a workload that
// already owns its name must keep applying in a collided namespace.
func TestApplyResourceStillUpdatesTheKindThatHoldsTheName(t *testing.T) {
	held := workloadObject(manifest.FunctionGVR, "Function")
	held.Object["spec"] = map[string]any{"image": "checkout:v1"}
	dyn := dynamicfake.NewSimpleDynamicClient(applyCollisionScheme(),
		held, workloadObject(manifest.AppGVR, "App"))

	updated := workloadObject(manifest.FunctionGVR, "Function")
	updated.Object["spec"] = map[string]any{"image": "checkout:v2"}

	action, err := applyResource(context.Background(), dyn, "shop-prod",
		manifest.Resource{GVR: manifest.FunctionGVR, Object: updated}, true, nil, false)
	require.NoError(t, err, "the function that owns this name was refused its own re-apply")
	assert.Equal(t, "updated", action)
}
