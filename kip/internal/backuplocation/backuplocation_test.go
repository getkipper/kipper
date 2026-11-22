package backuplocation

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func location(name, accessMode string) *unstructured.Unstructured {
	spec := map[string]interface{}{"provider": "aws"}
	if accessMode != "" {
		spec["accessMode"] = accessMode
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "velero.io/v1", "kind": "BackupStorageLocation",
		"metadata": map[string]interface{}{"name": name, "namespace": "velero"},
		"spec":     spec,
	}}
}

func client(objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{GVR: "BackupStorageLocationList"}, objects...)
}

func TestReadOnlyReadsTheDefaultLocation(t *testing.T) {
	ctx := context.Background()

	assert.True(t, ReadOnly(ctx, client(location("default", "ReadOnly"))))
	assert.False(t, ReadOnly(ctx, client(location("default", "ReadWrite"))))
	assert.False(t, ReadOnly(ctx, client(location("default", ""))), "Velero's default access mode is ReadWrite")
	assert.False(t, ReadOnly(ctx, client(location("other", "ReadOnly"))), "only Velero's default location takes kip's backups")
	assert.False(t, ReadOnly(ctx, client()), "a location kip cannot read leaves the backup to report the problem")
}

func TestIsReadOnly(t *testing.T) {
	assert.True(t, IsReadOnly(location("default", "ReadOnly")))
	assert.False(t, IsReadOnly(location("default", "ReadWrite")))
	assert.False(t, IsReadOnly(location("default", "")))
}
