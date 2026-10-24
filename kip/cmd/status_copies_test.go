package cmd

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/getkipper/kipper/kip/internal/manifest"
)

func copiesStatusObject(kind, namespace, name string, live int64, status, reason, message string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1",
		"kind":       kind,
		"metadata":   map[string]interface{}{"name": name},
		"spec":       map[string]interface{}{},
	}}
	if namespace != "" {
		obj.SetNamespace(namespace)
	}
	if live > 0 {
		_ = unstructured.SetNestedField(obj.Object, live, "status", "storage", "copies")
	}
	if status != "" {
		_ = unstructured.SetNestedSlice(obj.Object, []interface{}{map[string]interface{}{
			"type": "StorageCopies", "status": status, "reason": reason, "message": message,
		}}, "status", "conditions")
	}
	return obj
}

func copiesStatusClient(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		manifest.ServiceGVR: "ServiceList", manifest.VolumeGVR: "VolumeList", manifest.ProjectGVR: "ProjectList",
	}, objs...)
}

func TestStatusListsUnsetCopiesAndCopiesNeedingAttention(t *testing.T) {
	dyn := copiesStatusClient(
		copiesStatusObject("Service", "shop", "db", 3, "Unknown", "NotSet", "Copies are not set"),
		copiesStatusObject("Service", "shop", "cache", 2, "True", "Healthy", "2 of 2 copies are healthy."),
		copiesStatusObject("Service", "shop", "queue", 2, "False", "Degraded", "1 of 2 copies are healthy."),
		copiesStatusObject("Service", "shop", "search", 2, "Unknown", "Unknown", "1 of 2 copies are healthy, and Kipper could not read node information for 1 copy."),
		copiesStatusObject("Service", "shop", "new", 0, "Unknown", "Pending", "The volume is not provisioned yet."),
		copiesStatusObject("Volume", "shop", "files", 0, "Unknown", "NotSet", "Copies are not set"),
		copiesStatusObject("Volume", "blog", "media", 0, "False", "RWXNotQualified", "Shared volumes cannot have two copies."),
		copiesStatusObject("Project", "", "shop", 0, "Unknown", "NotSet", "Copies are not set. Set them with 'kip project update shop --copies 1'."),
		copiesStatusObject("Service", "old", "legacy", 0, "", "", ""),
	)

	var out bytes.Buffer
	reportStorageCopies(context.Background(), &out, dyn)
	got := out.String()

	assert.Contains(t, got, "Storage copies not set, so Kipper leaves them as they are:")
	assert.Contains(t, got, "shop/db (service, 3 copies)")
	assert.Contains(t, got, "shop/files (volume)")
	assert.Contains(t, got, "shared storage of project shop")
	assert.Contains(t, got, "'kip project update shop --copies 1'", "a project shows the controller's message with its command")
	assert.Contains(t, got, "'kip project update <project> --copies <n>'")
	assert.NotContains(t, got, "kubectl")
	assert.Contains(t, got, "Storage copies needing attention:")
	assert.Contains(t, got, "✗  shop/queue (service)\n       1 of 2 copies are healthy.")
	assert.Contains(t, got, "✗  shop/search (service)\n       1 of 2 copies are healthy, and Kipper could not read", "a copy Kipper cannot vouch for is not reported as healthy")
	assert.Contains(t, got, "✗  blog/media (volume)\n       Shared volumes cannot have two copies.")
	for _, quiet := range []string{"cache", "shop/new", "legacy"} {
		assert.NotContains(t, got, quiet, "healthy, provisioning and pre-copies objects need nothing")
	}
}

func TestStatusPrintsNothingAboutCopiesWhenAllIsWell(t *testing.T) {
	var out bytes.Buffer
	reportStorageCopies(context.Background(), &out, copiesStatusClient(
		copiesStatusObject("Service", "shop", "cache", 2, "True", "Healthy", "2 of 2 copies are healthy."),
	))
	assert.Empty(t, out.String())
}

func TestStatusListsConversionsApartFromProblems(t *testing.T) {
	var out bytes.Buffer
	reportStorageCopies(context.Background(), &out, copiesStatusClient(
		copiesStatusObject("Service", "shop", "db", 1, "False", "Converting", "Longhorn is building a copy of the volume (1 of 2 copies healthy)."),
		copiesStatusObject("Service", "shop", "queue", 1, "False", "ConversionPending", "Another volume's copies are being converted."),
		copiesStatusObject("Volume", "shop", "files", 2, "False", "Blocked", "The server has no room for a copy of this volume (5.0 GiB)."),
	))
	got := out.String()
	assert.Contains(t, got, "Storage copies being converted:\n    …  shop/db (service)\n       Longhorn is building a copy")
	assert.Contains(t, got, "…  shop/queue (service)")
	assert.Contains(t, got, "Storage copies needing attention:\n    ✗  shop/files (volume)\n       The server has no room")
}
