package function

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/getkipper/kipper/kip/internal/manifest"
	"github.com/getkipper/kipper/kip/internal/workload"
)

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

func workloadCR(gvr schema.GroupVersionResource, kind string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": gvr.GroupVersion().String(),
		"kind":       kind,
		"metadata":   map[string]any{"name": "checkout", "namespace": "shop-prod"},
	}}
}

// The demo hit this through the CLI, so the guard has to be wired into the
// CLI's own create path rather than only exist in a package it could call.
func TestCreateRefusesANameAnAppHolds(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(collisionScheme(),
		workloadCR(manifest.AppGVR, "App"))
	m := &Manager{Dynamic: dyn}

	err := m.Create(context.Background(), Options{
		Name: "checkout", Namespace: "shop-prod", Image: "checkout:v1", Port: 8080,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already used by an app")

	_, getErr := dyn.Resource(manifest.FunctionGVR).Namespace("shop-prod").
		Get(context.Background(), "checkout", metav1.GetOptions{})
	assert.Error(t, getErr, "the refused function was created anyway")
}

// Create upserts, so the function that holds the name keeps its updates even in
// a namespace where a collision exists. The holder is the older of the two,
// which is the same rule the controllers settle a contested name with.
func TestCreateStillUpdatesAFunctionThatAlreadyHoldsItsName(t *testing.T) {
	incumbent := workloadCR(manifest.FunctionGVR, "Function")
	incumbent.SetCreationTimestamp(metav1.NewTime(time.Now().Add(-24 * time.Hour)))
	intruder := workloadCR(manifest.AppGVR, "App")
	intruder.SetCreationTimestamp(metav1.NewTime(time.Now().Add(-time.Minute)))

	dyn := dynamicfake.NewSimpleDynamicClient(collisionScheme(), incumbent, intruder)
	m := &Manager{Dynamic: dyn}

	err := m.Create(context.Background(), Options{
		Name: "checkout", Namespace: "shop-prod", Image: "checkout:v2", Port: 8080,
	})
	require.NoError(t, err, "the function that owns this name was refused its own update")

	live, getErr := dyn.Resource(manifest.FunctionGVR).Namespace("shop-prod").
		Get(context.Background(), "checkout", metav1.GetOptions{})
	require.NoError(t, getErr)
	image, _, _ := unstructured.NestedString(live.Object, "spec", "image")
	assert.Equal(t, "checkout:v2", image, "the update did not reach the function")
}

// The other half of the same rule: a re-apply of the newer workload must not
// take a name the controllers would award to the older one. Client request
// order decided ownership before this, which contradicted the controllers and
// let an ordinary redeploy of an intruder freeze the incumbent.
func TestCreateRefusesTheNewerWorkloadsReapply(t *testing.T) {
	incumbent := workloadCR(manifest.AppGVR, "App")
	incumbent.SetCreationTimestamp(metav1.NewTime(time.Now().Add(-24 * time.Hour)))
	intruder := workloadCR(manifest.FunctionGVR, "Function")
	intruder.SetCreationTimestamp(metav1.NewTime(time.Now().Add(-time.Minute)))

	dyn := dynamicfake.NewSimpleDynamicClient(collisionScheme(), incumbent, intruder)
	m := &Manager{Dynamic: dyn}

	err := m.Create(context.Background(), Options{
		Name: "checkout", Namespace: "shop-prod", Image: "checkout:v2", Port: 8080,
	})
	require.Error(t, err, "the newer function re-applied over a name the older app holds")
	assert.Contains(t, err.Error(), "already used by an app")
}

// The upsert consumes the AlreadyExists that says a function of this name
// exists, so by the time Create returns, the error is the update's own. A
// rollback that reads the returned error cannot tell the two apart, and
// releasing here would free the name of a function that is running for another
// kind to take.
func TestCreateKeepsTheReservationWhenTheUpdateFailsAfterARace(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(collisionScheme())

	// The create loses a race to a concurrent writer, and the update then fails.
	raced := false
	dyn.PrependReactor("create", "functions", func(k8stesting.Action) (bool, runtime.Object, error) {
		raced = true
		return true, nil, apierrors.NewAlreadyExists(manifest.FunctionGVR.GroupResource(), "checkout")
	})
	dyn.PrependReactor("get", "functions", func(k8stesting.Action) (bool, runtime.Object, error) {
		if !raced {
			return true, nil, apierrors.NewNotFound(manifest.FunctionGVR.GroupResource(), "checkout")
		}
		return true, workloadCR(manifest.FunctionGVR, "Function"), nil
	})
	dyn.PrependReactor("update", "functions", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(context.DeadlineExceeded)
	})

	m := &Manager{Dynamic: dyn}
	err := m.Create(context.Background(), Options{
		Name: "checkout", Namespace: "shop-prod", Image: "checkout:v1", Port: 8080,
	})
	require.Error(t, err)

	_, getErr := dyn.Resource(workload.ClaimGVR).Namespace("shop-prod").
		Get(context.Background(), "checkout", metav1.GetOptions{})
	require.NoError(t, getErr,
		"the reservation was released over a function that exists, freeing its name for another kind")
}
