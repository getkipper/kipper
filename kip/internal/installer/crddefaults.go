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

// SpecDefaults reports the spec fields a kind's CRD gives a default value, by
// dotted path.
//
// These are the reason a diff cannot be read off the manifest alone. Admission
// writes a default into the stored object, so a manifest that omits `replicas`
// produces a live App carrying `replicas: 1` that the manifest never mentioned.
// Comparing the two directly calls that a field the apply would remove, which
// it is not: assigning a spec without it makes admission put the same default
// straight back, and treating it as a loss made `kip apply` refuse manifests
// that are entirely ordinary.
//
// The schemas come from the cluster, because only the cluster's own copy says
// what the cluster will do. Reading the CLI's embedded copies instead looked
// cheaper and is unsafe in one direction: a binary newer than the cluster would
// believe in a default the cluster does not apply, suppress the warning for a
// field the operator had set, and let the replacement drop it.
//
// A caller who cannot read them gets no defaults rather than a guess, and the
// second return value says so, because "we could not tell" and "this field is
// being destroyed" are different things to put in front of someone deciding
// whether to pass --force.
//
// A project-scoped operator is in that position. A CRD is cluster-scoped and the
// binding the Project reconciler creates for project members is a namespaced
// RoleBinding, which cannot authorise one whatever role it names, and no shipped
// role grants it — so they are asked about fields that are not going anywhere,
// and told why. Widening that is a change to who may read what: see
// plans/apply-shows-what-it-clears-plan-2026-08-03.md.
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
