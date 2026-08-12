package job

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/getkipper/kipper/kip/internal/manifest"
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

func workloadCR(gvr schema.GroupVersionResource, kind, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": gvr.GroupVersion().String(),
		"kind":       kind,
		"metadata":   map[string]any{"name": name, "namespace": "shop-prod"},
	}}
}

func TestScheduleRefusesANameAnAppHolds(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(collisionScheme(),
		workloadCR(manifest.AppGVR, "App", "nightly-report"))
	m := &Manager{Dynamic: dyn}

	err := m.Schedule(context.Background(), Options{
		Name: "nightly-report", Namespace: "shop-prod", Image: "reporter:v1", Schedule: "0 2 * * *",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already used by an app")

	_, getErr := dyn.Resource(manifest.JobGVR).Namespace("shop-prod").
		Get(context.Background(), "nightly-report", metav1.GetOptions{})
	assert.Error(t, getErr, "the refused job was created anyway")
}

// Schedule upserts, so the job that holds the name keeps its updates. The
// holder is the older of the two, the rule the controllers settle a contested
// name with.
func TestScheduleStillUpdatesAJobThatAlreadyHoldsItsName(t *testing.T) {
	incumbent := workloadCR(manifest.JobGVR, "Job", "nightly-report")
	incumbent.SetCreationTimestamp(metav1.NewTime(time.Now().Add(-24 * time.Hour)))
	intruder := workloadCR(manifest.AppGVR, "App", "nightly-report")
	intruder.SetCreationTimestamp(metav1.NewTime(time.Now().Add(-time.Minute)))

	dyn := dynamicfake.NewSimpleDynamicClient(collisionScheme(), incumbent, intruder)
	m := &Manager{Dynamic: dyn}

	err := m.Schedule(context.Background(), Options{
		Name: "nightly-report", Namespace: "shop-prod", Image: "reporter:v2", Schedule: "0 3 * * *",
	})
	require.NoError(t, err, "the job that owns this name was refused its own update")

	live, getErr := dyn.Resource(manifest.JobGVR).Namespace("shop-prod").
		Get(context.Background(), "nightly-report", metav1.GetOptions{})
	require.NoError(t, getErr)
	schedule, _, _ := unstructured.NestedString(live.Object, "spec", "schedule")
	assert.Equal(t, "0 3 * * *", schedule, "the update did not reach the job")
}
