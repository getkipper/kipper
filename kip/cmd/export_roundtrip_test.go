package cmd

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/manifest"
)

func roundTripDynamic() *dynamicfake.FakeDynamicClient {
	scheme := runtime.NewScheme()
	for _, kind := range []string{"App", "Function", "Service", "Volume", "Job", "Project"} {
		scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "kipper.run", Version: "v1alpha1", Kind: kind}, &unstructured.Unstructured{})
		scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "kipper.run", Version: "v1alpha1", Kind: kind + "List"}, &unstructured.UnstructuredList{})
	}
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Version: "v1", Kind: "NamespaceList"}, &unstructured.UnstructuredList{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"}, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinitionList"}, &unstructured.UnstructuredList{})
	return dynamicfake.NewSimpleDynamicClient(scheme)
}

func managed(kind, name string, spec map[string]interface{}) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1", "kind": kind,
		"metadata": map[string]interface{}{
			"name": name, "namespace": "shop-test",
			"labels": map[string]interface{}{"app.kubernetes.io/managed-by": "kipper"},
		},
		"spec": spec,
	}}
}

func TestExportThenApplyChangesNothing(t *testing.T) {
	ctx := context.Background()
	dyn := roundTripDynamic()
	create := func(gvr schema.GroupVersionResource, obj *unstructured.Unstructured) {
		t.Helper()
		ns := obj.GetNamespace()
		var err error
		if ns == "" {
			_, err = dyn.Resource(gvr).Create(ctx, obj, metav1.CreateOptions{})
		} else {
			_, err = dyn.Resource(gvr).Namespace(ns).Create(ctx, obj, metav1.CreateOptions{})
		}
		require.NoError(t, err)
	}

	create(schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}, &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1", "kind": "Namespace",
		"metadata": map[string]interface{}{"name": "docuseal-test", "labels": map[string]interface{}{"kipper.run/project": "docuseal", "kipper.run/environment": "test"}},
	}})

	app := managed("App", "web", map[string]interface{}{
		"image": "web:1", "port": int64(8080), "replicas": int64(2),
		"serviceBindings": []interface{}{map[string]interface{}{"name": "db", "database": "web"}},
		"links": []interface{}{
			map[string]interface{}{"app": "api", "namespace": "shop-test"},
			map[string]interface{}{"app": "docuseal", "namespace": "docuseal-test"},
		},
		"autoscale": map[string]interface{}{"enabled": true, "minReplicas": int64(2), "maxReplicas": int64(5), "cpuTarget": int64(70)},
		"resources": map[string]interface{}{"cpuRequest": "150m", "cpuLimit": "2250m", "memoryRequest": "2Gi", "memoryLimit": "2Gi"},
		"route": map[string]interface{}{
			"host": "web.example.com", "path": "/", "basicAuth": true,
			"redirectFrom":  []interface{}{"www.example.com"},
			"internalPaths": []interface{}{"/actuator"},
			"publicPaths":   []interface{}{"/actuator/health"},
		},
	})
	app.SetManagedFields([]metav1.ManagedFieldsEntry{resourceOwner("console-api", "cpuRequest", "cpuLimit", "memoryRequest", "memoryLimit")})
	create(manifest.AppGVR, app)
	create(manifest.JobGVR, managed("Job", "nightly", map[string]interface{}{
		"image": "backup:1", "schedule": "0 3 * * *",
		"resources":    map[string]interface{}{"cpuRequest": "100m", "memoryLimit": "256Mi"},
		"secretRefs":   []interface{}{"backup-credentials"},
		"backoffLimit": int64(5),
	}))

	m, err := manifest.Export(ctx, dyn, "shop", "shop", "test", "shop-test")
	require.NoError(t, err)
	assert.Empty(t, m.Notes)
	resolve := func(project, environment string) string {
		if environment == "" {
			environment = "test"
		}
		return project + "-" + environment
	}

	changes, err := scanChanges(ctx, dyn, "shop-test", manifest.Convert(m, "shop-test", resolve))

	require.NoError(t, err)
	assert.Empty(t, changes, "applying an unchanged export must change nothing")
}

func TestApplyRefusesAnExportThatLeftALinkOut(t *testing.T) {
	ctx := context.Background()
	dyn := roundTripDynamic()
	_, err := dyn.Resource(schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}).Create(ctx, &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1", "kind": "Namespace",
		"metadata": map[string]interface{}{"name": "finance-staging", "labels": map[string]interface{}{"kipper.run/project": "finance"}},
	}}, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = dyn.Resource(manifest.AppGVR).Namespace("shop-test").Create(ctx, managed("App", "web", map[string]interface{}{
		"image": "web:1", "port": int64(8080),
		"links": []interface{}{
			map[string]interface{}{"app": "api", "namespace": "shop-test"},
			map[string]interface{}{"app": "ledger", "namespace": "finance-staging"},
		},
	}), metav1.CreateOptions{})
	require.NoError(t, err)

	m, err := manifest.Export(ctx, dyn, "shop", "shop", "test", "shop-test")
	require.NoError(t, err)
	require.Len(t, m.Notes, 1, "the export says it left the link out")

	changes, err := scanChanges(ctx, dyn, "shop-test", manifest.Convert(m, "shop-test", nil))
	require.NoError(t, err)
	assert.NotEmpty(t, clearsOf(changes), "removing the left-out link needs --force")
}

// TestExportReadsTheOrgPrefixedProjectMetadata checks that export reads metadata
// using the Project's organization-prefixed name, matching apply and --split.
func TestExportReadsTheOrgPrefixedProjectMetadata(t *testing.T) {
	ctx := context.Background()
	dyn := roundTripDynamic()
	_, err := dyn.Resource(manifest.ProjectGVR).Create(ctx, &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1", "kind": "Project",
		"metadata": map[string]interface{}{"name": "acme-shop"},
		"spec": map[string]interface{}{
			"displayName":  "Shop",
			"environments": []interface{}{map[string]interface{}{"name": "test"}},
		},
	}}, metav1.CreateOptions{})
	require.NoError(t, err)
	output := filepath.Join(t.TempDir(), "shop.yaml")

	require.NoError(t, exportSingle(ctx, dyn, &config.Cluster{Org: "acme"}, "shop", "test", output))

	data, err := os.ReadFile(output)
	require.NoError(t, err)
	assert.Contains(t, string(data), "displayName: Shop")
	assert.Contains(t, string(data), "- test")
}
