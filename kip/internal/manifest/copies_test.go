package manifest

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/utils/ptr"
)

func TestValidateRefusesCopiesOutsideOneAndTwo(t *testing.T) {
	m := &Manifest{Project: "shop", Services: map[string]SvcSpec{"db": {Type: "postgres", Copies: ptr.To[int32](3)}}}
	assert.ErrorContains(t, Validate(m), `service "db": copies must be 1 or 2`)

	m = &Manifest{Project: "shop", Volumes: map[string]VolSpec{"uploads": {Size: "1Gi", Copies: ptr.To[int32](0)}}}
	assert.ErrorContains(t, Validate(m), `volume "uploads": copies must be 1 or 2`)

	m = &Manifest{
		Project:  "shop",
		Services: map[string]SvcSpec{"db": {Type: "postgres", Copies: ptr.To[int32](2)}},
		Volumes:  map[string]VolSpec{"uploads": {Size: "1Gi", Copies: ptr.To[int32](1)}},
	}
	assert.NoError(t, Validate(m))
}

func TestConvertWritesCopiesOnlyWhenTheManifestSetsThem(t *testing.T) {
	svc := convertService("db", "shop", SvcSpec{Type: "postgres", Copies: ptr.To[int32](2)})
	copies, found, _ := unstructured.NestedInt64(svc.Object.Object, "spec", "copies")
	assert.True(t, found)
	assert.EqualValues(t, 2, copies)

	vol := convertVolume("uploads", "shop", VolSpec{Size: "1Gi"})
	_, found, _ = unstructured.NestedInt64(vol.Object.Object, "spec", "copies")
	assert.False(t, found, "an omitted value is resolved against the cluster by apply")
}

func TestExportKeepsCopies(t *testing.T) {
	obj := func(kind, name string, spec map[string]interface{}) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "kipper.run/v1alpha1", "kind": kind,
			"metadata": map[string]interface{}{"name": name, "namespace": "shop",
				"labels": map[string]interface{}{"app.kubernetes.io/managed-by": "kipper"}},
			"spec": spec,
		}}
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{ServiceGVR: "ServiceList", VolumeGVR: "VolumeList"},
		obj("Service", "db", map[string]interface{}{"type": "postgres", "copies": int64(2)}),
		obj("Volume", "uploads", map[string]interface{}{"size": "1Gi", "copies": int64(1)}),
		obj("Volume", "old", map[string]interface{}{"size": "1Gi"}),
	)
	m := &Manifest{}
	require.NoError(t, exportServices(context.Background(), dyn, "shop", m))
	require.NoError(t, exportVolumes(context.Background(), dyn, "shop", m))
	assert.Equal(t, ptr.To[int32](2), m.Services["db"].Copies)
	assert.Equal(t, ptr.To[int32](1), m.Volumes["uploads"].Copies)
	assert.Nil(t, m.Volumes["old"].Copies, "an unmanaged volume stays unmanaged")
}
