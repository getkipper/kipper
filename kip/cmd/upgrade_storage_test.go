package cmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/getkipper/kipper/kip/internal/installer"
)

func platformConfigClient(storage string) *dynamicfake.FakeDynamicClient {
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(platformConfigGVR.GroupVersion().WithKind("PlatformConfig"), &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(platformConfigGVR.GroupVersion().WithKind("PlatformConfigList"), &unstructured.UnstructuredList{})
	spec := map[string]interface{}{"profile": "small"}
	if storage != "" {
		spec["storage"] = storage
	}
	pc := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1", "kind": "PlatformConfig",
		"metadata": map[string]interface{}{"name": "platform"},
		"spec":     spec,
	}}
	return dynamicfake.NewSimpleDynamicClient(scheme, pc)
}

func recordedStorage(t *testing.T, dyn *dynamicfake.FakeDynamicClient) string {
	t.Helper()
	pc, err := dyn.Resource(platformConfigGVR).Get(context.Background(), "platform", metav1.GetOptions{})
	require.NoError(t, err)
	storage, _, _ := unstructured.NestedString(pc.Object, "spec", "storage")
	return storage
}

func TestLoadPlatformStateRecordsLonghornOnALegacyCluster(t *testing.T) {
	dyn := platformConfigClient("")
	state, err := loadPlatformState(context.Background(), dyn)
	require.NoError(t, err)
	assert.Equal(t, installer.StorageLonghorn, state.Storage)
	assert.Equal(t, installer.StorageLonghorn, recordedStorage(t, dyn))
}

func TestLoadPlatformStateKeepsARecordedMode(t *testing.T) {
	dyn := platformConfigClient(installer.StorageLocal)
	state, err := loadPlatformState(context.Background(), dyn)
	require.NoError(t, err)
	assert.Equal(t, installer.StorageLocal, state.Storage)
	for _, a := range dyn.Actions() {
		assert.NotEqual(t, "patch", a.GetVerb(), "a recorded mode is never rewritten")
	}
}
