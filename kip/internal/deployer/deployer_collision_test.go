package deployer

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
	"k8s.io/client-go/kubernetes/fake"

	"github.com/getkipper/kipper/kip/internal/manifest"
)

// collisionScheme knows all three workload kinds, because a cross-kind
// collision is invisible to a scheme carrying only one of them.
func collisionScheme() *runtime.Scheme {
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

func workloadCR(gvr schema.GroupVersionResource, kind, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": gvr.GroupVersion().String(),
		"kind":       kind,
		"metadata":   map[string]any{"name": name, "namespace": "shop-prod"},
	}}
}

func deployerWith(objs ...runtime.Object) (*Deployer, *dynamicfake.FakeDynamicClient) {
	dyn := dynamicfake.NewSimpleDynamicClient(collisionScheme(), objs...)
	return &Deployer{Client: fake.NewSimpleClientset(), Dynamic: dyn}, dyn //nolint:staticcheck
}

// Deploy is the CLI's own create path, so the guard has to be wired into it
// rather than only exist in the package the CLI could have called.
func TestDeployRefusesANameAFunctionHolds(t *testing.T) {
	d, dyn := deployerWith(workloadCR(manifest.FunctionGVR, "Function", "checkout"))

	err := d.Deploy(context.Background(), Options{
		Name:      "checkout",
		Namespace: "shop-prod",
		Image:     "checkout:v1",
		Port:      8080,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already used by a function")

	_, getErr := dyn.Resource(manifest.AppGVR).Namespace("shop-prod").
		Get(context.Background(), "checkout", metav1.GetOptions{})
	assert.Error(t, getErr, "the refused app was created anyway")
}

// The regression the guard itself introduced: Deploy upserts, so in a namespace
// where a collision already exists the App is the legitimate holder and its
// redeploy must keep working. Refusing it would break the healthy half over a
// name it owns, and it is the half with the running pipeline.
func TestDeployStillRedeploysAnAppThatAlreadyHoldsItsName(t *testing.T) {
	held := workloadCR(manifest.AppGVR, "App", "checkout")
	held.Object["spec"] = map[string]any{"image": "checkout:v1", "port": int64(8080)}
	d, dyn := deployerWith(held, workloadCR(manifest.FunctionGVR, "Function", "checkout"))

	err := d.Deploy(context.Background(), Options{
		Name:      "checkout",
		Namespace: "shop-prod",
		Image:     "checkout:v2",
		Port:      8080,
		Changed:   map[string]bool{"image": true},
	})
	require.NoError(t, err, "the app that owns this name was refused its own redeploy")

	live, getErr := dyn.Resource(manifest.AppGVR).Namespace("shop-prod").
		Get(context.Background(), "checkout", metav1.GetOptions{})
	require.NoError(t, getErr)
	image, _, _ := unstructured.NestedString(live.Object, "spec", "image")
	assert.Equal(t, "checkout:v2", image, "the redeploy did not reach the app")
}
