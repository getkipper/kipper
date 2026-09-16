package installer

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// CRDGVR is where a cluster keeps the schemas its custom resources are
// validated and defaulted against.
var CRDGVR = schema.GroupVersionResource{
	Group:    "apiextensions.k8s.io",
	Version:  "v1",
	Resource: "customresourcedefinitions",
}

// SpecDefaults reads dotted-path defaults from the cluster's CRD version.
// Apply uses them to distinguish admission-restored defaults from removed fields.
// Use the live schema: a newer embedded default may not exist on the cluster.
// Missing or forbidden CRDs return known=false; other read errors propagate.
func SpecDefaults(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource) (defaults map[string]interface{}, known bool, err error) {
	crd, err := dyn.Resource(CRDGVR).Get(ctx, gvr.Resource+"."+gvr.Group, metav1.GetOptions{})
	switch {
	case err == nil:
	case errors.IsNotFound(err), errors.IsForbidden(err):
		// No schema to read, so nothing is known to be a default and every
		// omitted field is treated as a loss worth asking about.
		return nil, false, nil
	default:
		return nil, false, fmt.Errorf("reading schema for %s: %w", gvr.Resource, err)
	}

	versions, _, _ := unstructured.NestedSlice(crd.Object, "spec", "versions")
	for _, v := range versions {
		version, ok := v.(map[string]interface{})
		if !ok {
			continue
		}
		if name, _ := version["name"].(string); name != gvr.Version {
			continue
		}
		specSchema, found, _ := unstructured.NestedMap(version, "schema", "openAPIV3Schema", "properties", "spec")
		if !found {
			return nil, true, nil
		}
		out := map[string]interface{}{}
		collectDefaults(out, "", specSchema)
		return out, true, nil
	}
	return nil, false, nil
}

// collectDefaults walks an OpenAPI object schema, recording every default it
// declares against the dotted path that reaches it.
//
// It stops at a path that carries its own default and at an array. A default on
// an object is applied by admission as a whole value, so its members are not
// separately defaulted paths; a default below `items` belongs to each element
// of an array rather than to any path, and nothing that compares whole values
// could act on it. An array whose elements differ only by a defaulted member
// therefore still reads as changed, which is a diff line that overstates rather
// than a refusal.
func collectDefaults(out map[string]interface{}, prefix string, s map[string]interface{}) {
	props, found, _ := unstructured.NestedMap(s, "properties")
	if !found {
		return
	}
	for name, raw := range props {
		field, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		if def, has := field["default"]; has {
			out[path] = def
			continue
		}
		collectDefaults(out, path, field)
	}
}
