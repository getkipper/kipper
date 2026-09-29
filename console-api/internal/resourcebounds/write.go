package resourcebounds

import (
	"context"
	"encoding/json"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	"github.com/getkipper/kipper/controller/pkg/fieldowners"
)

// PairEdit is one resource's change: a new request and limit, or Clear to
// hand the resource back to automatic sizing.
type PairEdit struct {
	Request string
	Limit   string
	Clear   bool
}

// Edits is the change a user asked for. A nil resource is left as it is.
type Edits struct {
	CPU    *PairEdit
	Memory *PairEdit
	// Profile, when set, is written alongside the quantities.
	Profile string
}

// ConsoleManager distinguishes user edits from the old auto-sizer's writes.
const ConsoleManager = "kipper-console"

// ownedFields are the spec.resources fields a writer keeps when it applies.
var ownedFields = []string{"cpuRequest", "cpuLimit", "memoryRequest", "memoryLimit", "profile"}

// WriteQuantities applies resource edits under manager, preserving fields it
// already owns and claiming the edited values as the user's bounds. Each write
// checks resourceVersion. On a conflict, it reads again and replays the edits
// to preserve concurrent changes to other resources. obj identifies the kind
// and is left unchanged.
func WriteQuantities(ctx context.Context, c client.Client, obj client.Object, namespace, name, manager string, edits Edits) error {
	return retry.OnError(retry.DefaultRetry, changedMeanwhile, func() error {
		return writeQuantitiesOnce(ctx, c, obj, namespace, name, manager, edits)
	})
}

// changedMeanwhile accepts apply conflicts and invalid-patch errors for retry.
// The API server reports a failed JSON patch resourceVersion test as invalid.
func changedMeanwhile(err error) bool {
	return apierrors.IsConflict(err) || apierrors.IsInvalid(err)
}

func writeQuantitiesOnce(ctx context.Context, c client.Client, obj client.Object, namespace, name, manager string, edits Edits) error {
	gvk, err := apiutil.GVKForObject(obj, c.Scheme())
	if err != nil {
		return err
	}
	current := &unstructured.Unstructured{}
	current.SetGroupVersionKind(gvk)
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, current); err != nil {
		return err
	}
	resources, _, _ := unstructured.NestedStringMap(current.Object, "spec", "resources")

	// Start from what this manager already owns, so an apply that omits it
	// does not delete it.
	apply := map[string]any{}
	for _, field := range ownedFields {
		if contains(fieldowners.Owners(current.GetManagedFields(), "spec", "resources", field), manager) && resources[field] != "" {
			apply[field] = resources[field]
		}
	}
	var remove []string
	edit := func(e *PairEdit, reqField, limField string) {
		if e == nil {
			return
		}
		if e.Clear {
			delete(apply, reqField)
			delete(apply, limField)
			for _, f := range []string{reqField, limField} {
				if resources[f] != "" {
					remove = append(remove, f)
				}
			}
			return
		}
		apply[reqField], apply[limField] = e.Request, e.Limit
	}
	edit(edits.CPU, "cpuRequest", "cpuLimit")
	edit(edits.Memory, "memoryRequest", "memoryLimit")
	if edits.Profile != "" {
		apply["profile"] = edits.Profile
	}

	resourceVersion := current.GetResourceVersion()
	if len(remove) > 0 {
		if resourceVersion, err = removeFields(ctx, c, current, resourceVersion, remove); err != nil {
			return err
		}
	}
	if len(apply) == 0 {
		return nil
	}
	cfg := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"resources": apply},
	}}
	cfg.SetGroupVersionKind(gvk)
	cfg.SetNamespace(namespace)
	cfg.SetName(name)
	cfg.SetResourceVersion(resourceVersion)
	return c.Apply(ctx, client.ApplyConfigurationFromUnstructured(cfg), client.FieldOwner(manager), client.ForceOwnership)
}

// removeFields deletes quantities from spec.resources for every owner, only
// if the object is still at resourceVersion, and returns the new version.
func removeFields(ctx context.Context, c client.Client, obj *unstructured.Unstructured, resourceVersion string, fields []string) (string, error) {
	ops := []map[string]any{{"op": "test", "path": "/metadata/resourceVersion", "value": resourceVersion}}
	for _, f := range fields {
		ops = append(ops, map[string]any{"op": "remove", "path": "/spec/resources/" + f})
	}
	patch, err := json.Marshal(ops)
	if err != nil {
		return "", fmt.Errorf("building the remove patch: %w", err)
	}
	if err := c.Patch(ctx, obj, client.RawPatch(types.JSONPatchType, patch)); err != nil {
		return "", err
	}
	return obj.GetResourceVersion(), nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
