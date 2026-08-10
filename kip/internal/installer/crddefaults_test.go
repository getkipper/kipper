package installer

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

func appGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "kipper.run", Version: "v1alpha1", Resource: "apps"}
}

func fakeCRDs(t *testing.T, crd *unstructured.Unstructured) *dynamicfake.FakeDynamicClient {
	t.Helper()
	s := runtime.NewScheme()
	s.AddKnownTypeWithName(schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"}, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinitionList"}, &unstructured.UnstructuredList{})
	dyn := dynamicfake.NewSimpleDynamicClient(s)
	if crd != nil {
		_, err := dyn.Resource(CRDGVR).Create(context.Background(), crd, metav1.CreateOptions{})
		require.NoError(t, err)
	}
	return dyn
}

func appCRD(specProps map[string]interface{}) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "apiextensions.k8s.io/v1",
		"kind":       "CustomResourceDefinition",
		"metadata":   map[string]interface{}{"name": "apps.kipper.run"},
		"spec": map[string]interface{}{
			"versions": []interface{}{map[string]interface{}{
				"name": "v1alpha1",
				"schema": map[string]interface{}{"openAPIV3Schema": map[string]interface{}{
					"properties": map[string]interface{}{"spec": map[string]interface{}{"properties": specProps}},
				}},
			}},
		},
	}}
}

// The cluster's own schema is what the cluster will apply, so that is what is
// read. A CLI newer than the cluster believing in a default the cluster does
// not have would suppress the warning for a field it is about to drop.
func TestSpecDefaults_ReadsTheClusterSchema(t *testing.T) {
	dyn := fakeCRDs(t, appCRD(map[string]interface{}{
		"image":    map[string]interface{}{"type": "string"},
		"replicas": map[string]interface{}{"type": "integer", "default": int64(1)},
		"git": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"branch": map[string]interface{}{"type": "string", "default": "main"},
			},
		},
	}))
	defaults, known, err := SpecDefaults(context.Background(), dyn, appGVR())
	require.NoError(t, err)
	assert.True(t, known)
	assert.Equal(t, map[string]interface{}{"replicas": int64(1), "git.branch": "main"}, defaults)
}

// A cluster can serve a kind whose schema this cannot see, and that must not
// stop an apply. No defaults means every omission is treated as a loss.
func TestSpecDefaults_AnAbsentSchemaIsNotAnError(t *testing.T) {
	defaults, known, err := SpecDefaults(context.Background(), fakeCRDs(t, nil), appGVR())
	require.NoError(t, err)
	assert.False(t, known, "the caller has to be able to say it could not tell")
	assert.Nil(t, defaults)
}

// Same for a caller not allowed to read one: noisy and safe beats guessing.
func TestSpecDefaults_NoPermissionIsNotAnError(t *testing.T) {
	dyn := fakeCRDs(t, nil)
	dyn.PrependReactor("get", "customresourcedefinitions", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "customresourcedefinitions"}, "apps.kipper.run", context.DeadlineExceeded)
	})
	defaults, known, err := SpecDefaults(context.Background(), dyn, appGVR())
	require.NoError(t, err)
	assert.False(t, known, "and to say so when it is a permission rather than a gap")
	assert.Nil(t, defaults)
}

// A default on an object is applied whole, so its members are not separately
// defaulted paths.
func TestSpecDefaults_StopsAtADefaultedObject(t *testing.T) {
	out := map[string]interface{}{}
	collectDefaults(out, "", map[string]interface{}{
		"properties": map[string]interface{}{
			"settings": map[string]interface{}{
				"default": map[string]interface{}{"mode": "safe"},
				"properties": map[string]interface{}{
					"mode": map[string]interface{}{"default": "unsafe"},
				},
			},
		},
	})
	assert.Equal(t, map[string]interface{}{"settings": map[string]interface{}{"mode": "safe"}}, out)
}

// A default below an array's items belongs to each element, not to a path.
func TestSpecDefaults_DoesNotDescendIntoArrayItems(t *testing.T) {
	out := map[string]interface{}{}
	collectDefaults(out, "", map[string]interface{}{
		"properties": map[string]interface{}{
			"ports": map[string]interface{}{
				"type": "array",
				"items": map[string]interface{}{
					"properties": map[string]interface{}{
						"protocol": map[string]interface{}{"default": "TCP"},
					},
				},
			},
		},
	})
	assert.Empty(t, out)
}
