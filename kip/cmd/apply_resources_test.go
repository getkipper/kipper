package cmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/getkipper/kipper/kip/internal/deployer"
	"github.com/getkipper/kipper/kip/internal/manifest"
)

func resourceOwner(manager string, fields ...string) metav1.ManagedFieldsEntry {
	raw := `{"f:spec":{"f:resources":{`
	for i, f := range fields {
		if i > 0 {
			raw += ","
		}
		raw += `"f:` + f + `":{}`
	}
	raw += `}}}`
	return metav1.ManagedFieldsEntry{Manager: manager, Operation: metav1.ManagedFieldsOperationUpdate, FieldsType: "FieldsV1", FieldsV1: &metav1.FieldsV1{Raw: []byte(raw)}}
}

func seedSizedApp(t *testing.T, owner string) *unstructured.Unstructured {
	t.Helper()
	app := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1", "kind": "App",
		"metadata": map[string]interface{}{"name": "api", "namespace": "default"},
		"spec": map[string]interface{}{
			"image": "nginx",
			"resources": map[string]interface{}{
				"cpuRequest": "150m", "cpuLimit": "2250m", "memoryRequest": "2Gi", "memoryLimit": "2Gi",
			},
		},
	}}
	app.SetManagedFields([]metav1.ManagedFieldsEntry{resourceOwner(owner, "cpuRequest", "cpuLimit", "memoryRequest", "memoryLimit")})
	return app
}

func TestScanChanges_IgnoresAutomaticResourcesTheManifestOmits(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	_, err := dyn.Resource(deployer.AppGVR).Namespace("default").Create(context.Background(), seedSizedApp(t, "console-api"), metav1.CreateOptions{})
	require.NoError(t, err)

	changes, err := scanChanges(context.Background(), dyn, "default",
		[]manifest.Resource{appResource("api", map[string]interface{}{"image": "nginx"})})

	require.NoError(t, err)
	assert.Empty(t, changes, "automatic values are not the manifest's to clear")
}

func TestScanChanges_StillReportsAUserResourceTheManifestOmits(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	_, err := dyn.Resource(deployer.AppGVR).Namespace("default").Create(context.Background(), seedSizedApp(t, "kip"), metav1.CreateOptions{})
	require.NoError(t, err)

	changes, err := scanChanges(context.Background(), dyn, "default",
		[]manifest.Resource{appResource("api", map[string]interface{}{"image": "nginx"})})

	require.NoError(t, err)
	assert.Len(t, clearsOf(changes), 4)
}

func TestApplyResource_KeepsAutomaticResourcesWithoutClaimingThem(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	_, err := dyn.Resource(deployer.AppGVR).Namespace("default").Create(context.Background(), seedSizedApp(t, "console-api"), metav1.CreateOptions{})
	require.NoError(t, err)
	res := appResource("api", map[string]interface{}{"image": "nginx"})

	_, err = applyResource(context.Background(), dyn, "default", res, false, nil, false)
	require.NoError(t, err, "an unchanged export must apply without --force")

	written, err := dyn.Resource(deployer.AppGVR).Namespace("default").Get(context.Background(), "api", metav1.GetOptions{})
	require.NoError(t, err)
	cpu, _, _ := unstructured.NestedString(written.Object, "spec", "resources", "cpuRequest")
	assert.Equal(t, "150m", cpu, "the automatic value stays")
	assert.Empty(t, declaredResources(res.Object.Object), "carried-forward values are not declared, so nothing claims them")
}
