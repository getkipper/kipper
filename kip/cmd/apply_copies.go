package cmd

import (
	"context"
	"fmt"
	"io"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/getkipper/kipper/kip/internal/manifest"
)

func carriesCopies(kind string) bool {
	return kind == "Service" || kind == "Volume"
}

// withNewCopies writes one copy on a new Service or Volume whose manifest
// omits copies, as kip service add and kip volume create do. It returns a
// shallow copy when adding the field and the original spec otherwise.
func withNewCopies(kind string, spec map[string]interface{}) map[string]interface{} {
	if !carriesCopies(kind) || spec == nil {
		return spec
	}
	if _, set := spec["copies"]; set {
		return spec
	}
	return withField(spec, "copies", int64(1))
}

// withLiveCopies keeps an existing object's copies when the manifest omits
// them, so leaving copies out of a manifest never unsets them.
func withLiveCopies(kind string, spec, live map[string]interface{}) map[string]interface{} {
	if !carriesCopies(kind) || spec == nil {
		return spec
	}
	if _, set := spec["copies"]; set {
		return spec
	}
	current, found := live["copies"]
	if !found {
		return spec
	}
	return withField(spec, "copies", current)
}

// refuseCopiesChange refuses a manifest that changes the copies of an
// existing object while they are being converted. A manifest without copies
// changes nothing.
func refuseCopiesChange(kind string, spec map[string]interface{}, existing *unstructured.Unstructured) error {
	if !carriesCopies(kind) {
		return nil
	}
	wanted, set := spec["copies"]
	if !set {
		return nil
	}
	want, ok := wanted.(int64)
	if !ok {
		return fmt.Errorf("copies must be a whole number, got %v", wanted)
	}
	_, err := checkCopiesChange(existing, specCopies, want, nil, kind == "Volume")
	return err
}

// refuseCopiesInPlans checks every copies setting in the manifests before
// apply writes anything: a new object's copies against what the cluster
// offers and can place, an existing object's change against a running
// conversion and, for two copies, against what the cluster can place.
// Admission and the controllers still refuse what changes after this check.
func refuseCopiesInPlans(ctx context.Context, out io.Writer, dyn dynamic.Interface, plans []applyPlan) error {
	var cluster *copiesCluster
	for _, p := range plans {
		for _, res := range p.resources {
			kind, name := res.Object.GetKind(), res.Object.GetName()
			copies, set, _ := unstructured.NestedInt64(res.Object.Object, "spec", "copies")
			if !carriesCopies(kind) || !set {
				continue
			}
			live, err := dyn.Resource(res.GVR).Namespace(p.namespace).Get(ctx, name, metav1.GetOptions{})
			switch {
			case errors.IsNotFound(err):
				if copies < 2 {
					continue
				}
				c, err := checkCopiesOnCreate(ctx, out, dyn, int32(copies), res.GVR == manifest.VolumeGVR) //nolint:gosec // G115: Validate bounds copies to 1 or 2.
				if err != nil {
					return fmt.Errorf("%s/%s: %w", kind, name, err)
				}
				if c.offered == 0 {
					if err := askAdmission(ctx, dyn, res.GVR, p.namespace, name, res.Object); err != nil {
						return fmt.Errorf("%s/%s: %w", kind, name, err)
					}
				}
			case err != nil:
				return fmt.Errorf("reading %s/%s: %w", kind, name, err)
			default:
				if copies == 2 && cluster == nil {
					c := readCopiesCluster(ctx, dyn)
					cluster = &c
				}
				unchanged, err := checkCopiesChange(live, specCopies, copies, cluster, res.GVR == manifest.VolumeGVR)
				if err != nil {
					return fmt.Errorf("%s/%s: %w", kind, name, err)
				}
				if !unchanged && cluster != nil {
					warnAboutCopies(out, int32(copies), *cluster) //nolint:gosec // G115: Validate bounds copies to 1 or 2.
				}
			}
		}
	}
	return nil
}

// askAdmission checks a new object with a dry-run create when the caller cannot
// read the copies offer, so admission can reject it before apply writes anything.
// NotFound errors are ignored to allow namespaces that have yet to be created.
// Admission checks the real create as well.
func askAdmission(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, namespace, name string, obj *unstructured.Unstructured) error {
	_, err := dyn.Resource(gvr).Namespace(namespace).Create(ctx, objectForCreate(name, obj), metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
	if err == nil || errors.IsNotFound(err) {
		return nil
	}
	return err
}

func withField(spec map[string]interface{}, key string, value interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(spec)+1)
	for k, v := range spec {
		out[k] = v
	}
	out[key] = value
	return out
}
