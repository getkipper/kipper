package cmd

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"

	"github.com/getkipper/kipper/kip/internal/deployer"
)

// kip apply claims the resource values the manifest declares, exactly as
// declared, so a value it repeats unchanged still becomes the user's.
func TestApplyClaimsTheDeclaredResourceValues(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seeded := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1", "kind": "App",
		"metadata": map[string]interface{}{"name": "web", "namespace": "default"},
		"spec": map[string]interface{}{
			"image":     "nginx:1",
			"resources": map[string]interface{}{"memoryRequest": "512Mi", "memoryLimit": "512Mi"},
		},
	}}
	_, err := dyn.Resource(deployer.AppGVR).Namespace("default").Create(context.Background(), seeded, metav1.CreateOptions{})
	require.NoError(t, err)

	var applies []k8stesting.PatchAction
	dyn.PrependReactor("patch", "apps", func(a k8stesting.Action) (bool, runtime.Object, error) {
		p := a.(k8stesting.PatchAction)
		if p.GetPatchType() == types.ApplyPatchType {
			applies = append(applies, p)
			return true, seeded.DeepCopy(), nil
		}
		return false, nil, nil
	})

	res := appResource("web", map[string]interface{}{
		"image":     "nginx:2",
		"resources": map[string]interface{}{"memoryRequest": "512Mi", "memoryLimit": "2Gi"},
	})
	action, err := applyResource(context.Background(), dyn, "default", res, false, nil, false)
	require.NoError(t, err)
	assert.Equal(t, "updated", action)

	require.Len(t, applies, 1)
	var cfg map[string]interface{}
	require.NoError(t, json.Unmarshal(applies[0].GetPatch(), &cfg))
	claimed, _, _ := unstructured.NestedStringMap(cfg, "spec", "resources")
	assert.Equal(t, map[string]string{"memoryRequest": "512Mi", "memoryLimit": "2Gi"}, claimed,
		"the unchanged 512Mi request is claimed too, and nothing the manifest did not declare")
	assert.Equal(t, deployer.FieldManager, applies[0].(k8stesting.PatchActionImpl).PatchOptions.FieldManager)
}

// A manifest without resources claims nothing, so an automatic app stays automatic.
func TestApplyWithoutResourcesClaimsNothing(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seeded := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1", "kind": "App",
		"metadata": map[string]interface{}{"name": "web", "namespace": "default"},
		"spec":     map[string]interface{}{"image": "nginx:1"},
	}}
	_, err := dyn.Resource(deployer.AppGVR).Namespace("default").Create(context.Background(), seeded, metav1.CreateOptions{})
	require.NoError(t, err)
	applied := false
	dyn.PrependReactor("patch", "apps", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.(k8stesting.PatchAction).GetPatchType() == types.ApplyPatchType {
			applied = true
		}
		return false, nil, nil
	})

	_, err = applyResource(context.Background(), dyn, "default", appResource("web", map[string]interface{}{"image": "nginx:2"}), false, nil, false)
	require.NoError(t, err)
	assert.False(t, applied, "no resource values declared, so none are claimed")
}
