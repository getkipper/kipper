package deployer

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

// patchesTo records the patches sent to an App, answering each with the
// seeded object so the call succeeds. The API server's handling of these
// requests is pinned against a real API server in console-api.
func patchesTo(dyn *dynamicfake.FakeDynamicClient, app *unstructured.Unstructured) *[]k8stesting.PatchAction {
	var got []k8stesting.PatchAction
	dyn.PrependReactor("patch", "apps", func(a k8stesting.Action) (bool, runtime.Object, error) {
		got = append(got, a.(k8stesting.PatchAction))
		return true, app.DeepCopy(), nil
	})
	return &got
}

func appOwnedBy(manager, fields string, resources map[string]interface{}) *unstructured.Unstructured {
	app := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1", "kind": "App",
		"metadata": map[string]interface{}{"name": "api", "namespace": "default", "resourceVersion": "7"},
		"spec":     map[string]interface{}{"image": "nginx", "resources": resources},
	}}
	app.SetManagedFields([]metav1.ManagedFieldsEntry{{
		Manager: manager, Operation: metav1.ManagedFieldsOperationUpdate, FieldsType: "FieldsV1",
		FieldsV1: &metav1.FieldsV1{Raw: []byte(fields)},
	}})
	return app
}

func applied(t *testing.T, p k8stesting.PatchAction) map[string]interface{} {
	t.Helper()
	var obj map[string]interface{}
	require.NoError(t, json.Unmarshal(p.GetPatch(), &obj))
	res, _, _ := unstructured.NestedMap(obj, "spec", "resources")
	rv, _, _ := unstructured.NestedString(obj, "metadata", "resourceVersion")
	assert.Equal(t, "7", rv, "the apply must be conditional on the version read")
	return res
}

func TestUpdateResourcesClaimsTheEditAndKeepsWhatKipSetBefore(t *testing.T) {
	d, dyn := testDeployer()
	app := appOwnedBy("kip", `{"f:spec":{"f:resources":{"f:cpuRequest":{},"f:cpuLimit":{}}}}`,
		map[string]interface{}{"cpuRequest": "250m", "cpuLimit": "1"})
	_, err := dyn.Resource(AppGVR).Namespace("default").Create(context.Background(), app, metav1.CreateOptions{})
	require.NoError(t, err)
	patches := patchesTo(dyn, app)

	require.NoError(t, d.UpdateResources(context.Background(), "default", "api",
		ResourceEdits{Memory: &PairEdit{Request: "512Mi", Limit: "2Gi"}}))

	require.Len(t, *patches, 1)
	p := (*patches)[0]
	assert.Equal(t, types.ApplyPatchType, p.GetPatchType())
	opts := p.(k8stesting.PatchActionImpl).PatchOptions
	assert.Equal(t, FieldManager, opts.FieldManager, "the values must be written as kip")
	require.NotNil(t, opts.Force)
	assert.True(t, *opts.Force, "a deliberate edit takes the value over from other writers")
	assert.Equal(t, map[string]interface{}{
		"cpuRequest": "250m", "cpuLimit": "1", "memoryRequest": "512Mi", "memoryLimit": "2Gi", "profile": "custom",
	}, applied(t, p))
}

func TestUpdateResourcesClaimsNoValueSomeoneElseSet(t *testing.T) {
	d, dyn := testDeployer()
	app := appOwnedBy("console-api", `{"f:spec":{"f:resources":{"f:cpuRequest":{},"f:cpuLimit":{}}}}`,
		map[string]interface{}{"cpuRequest": "100m", "cpuLimit": "100m"})
	_, err := dyn.Resource(AppGVR).Namespace("default").Create(context.Background(), app, metav1.CreateOptions{})
	require.NoError(t, err)
	patches := patchesTo(dyn, app)

	require.NoError(t, d.UpdateResources(context.Background(), "default", "api",
		ResourceEdits{Memory: &PairEdit{Request: "512Mi", Limit: "512Mi"}}))

	require.Len(t, *patches, 1)
	res := applied(t, (*patches)[0])
	assert.NotContains(t, res, "cpuRequest", "a CPU value kip never set is not kip's to claim")
}

func TestTuningAutoRemovesTheValuesConditionally(t *testing.T) {
	d, dyn := testDeployer()
	app := appOwnedBy("kip", `{"f:spec":{"f:resources":{"f:memoryRequest":{},"f:memoryLimit":{}}}}`,
		map[string]interface{}{"memoryRequest": "512Mi", "memoryLimit": "2Gi"})
	_, err := dyn.Resource(AppGVR).Namespace("default").Create(context.Background(), app, metav1.CreateOptions{})
	require.NoError(t, err)
	patches := patchesTo(dyn, app)

	require.NoError(t, d.UpdateResources(context.Background(), "default", "api",
		ResourceEdits{Memory: &PairEdit{Clear: true}}))

	require.Len(t, *patches, 1, "clearing everything kip set needs no apply")
	p := (*patches)[0]
	assert.Equal(t, types.JSONPatchType, p.GetPatchType())
	var ops []map[string]interface{}
	require.NoError(t, json.Unmarshal(p.GetPatch(), &ops))
	assert.Equal(t, []map[string]interface{}{
		{"op": "test", "path": "/metadata/resourceVersion", "value": "7"},
		{"op": "remove", "path": "/spec/resources/memoryRequest"},
		{"op": "remove", "path": "/spec/resources/memoryLimit"},
	}, ops)
}

func TestUpdateResourcesRetriesAConflict(t *testing.T) {
	d, dyn := testDeployer()
	app := appOwnedBy("kip", `{"f:spec":{"f:resources":{}}}`, map[string]interface{}{})
	_, err := dyn.Resource(AppGVR).Namespace("default").Create(context.Background(), app, metav1.CreateOptions{})
	require.NoError(t, err)
	attempts := 0
	dyn.PrependReactor("patch", "apps", func(a k8stesting.Action) (bool, runtime.Object, error) {
		attempts++
		if attempts == 1 {
			return true, nil, apierrors.NewConflict(AppGVR.GroupResource(), "api", nil)
		}
		return true, app.DeepCopy(), nil
	})

	require.NoError(t, d.UpdateResources(context.Background(), "default", "api",
		ResourceEdits{Memory: &PairEdit{Request: "512Mi", Limit: "2Gi"}}))
	assert.Equal(t, 2, attempts, "the conflicting apply is retried once")
}

func TestClaimResourcesRetriesAConflict(t *testing.T) {
	d, dyn := testDeployer()
	app := appOwnedBy("kip", `{"f:spec":{"f:resources":{}}}`, map[string]interface{}{"memoryRequest": "512Mi"})
	_, err := dyn.Resource(AppGVR).Namespace("default").Create(context.Background(), app, metav1.CreateOptions{})
	require.NoError(t, err)
	attempts := 0
	dyn.PrependReactor("patch", "apps", func(a k8stesting.Action) (bool, runtime.Object, error) {
		attempts++
		if attempts == 1 {
			return true, nil, apierrors.NewConflict(AppGVR.GroupResource(), "api", nil)
		}
		return true, app.DeepCopy(), nil
	})

	require.NoError(t, d.ClaimResources(context.Background(), "default", "api", map[string]string{"memoryRequest": "512Mi"}))
	assert.Equal(t, 2, attempts)
}
