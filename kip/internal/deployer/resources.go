package deployer

import (
	"context"
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/util/retry"

	"github.com/getkipper/kipper/controller/pkg/fieldowners"
)

// FieldManager identifies resource values explicitly chosen through kip.
const FieldManager = "kip"

// PairEdit is one resource's change: a new request and limit, or Clear to
// hand the resource back to automatic sizing.
type PairEdit struct {
	Request string
	Limit   string
	Clear   bool
}

// ResourceEdits is a change to an App's CPU and memory. A nil resource is
// left as it is.
type ResourceEdits struct {
	CPU    *PairEdit
	Memory *PairEdit
}

var resourceFields = []string{"cpuRequest", "cpuLimit", "memoryRequest", "memoryLimit", "profile"}

// UpdateResources claims edited CPU and memory values as user bounds, even
// when unchanged. It preserves fields kip already owns and removes cleared
// resources for all owners. Each write checks resourceVersion; conflicts
// retry the edits against a fresh read.
func (d *Deployer) UpdateResources(ctx context.Context, namespace, name string, edits ResourceEdits) error {
	return retry.OnError(retry.DefaultRetry, changedMeanwhile, func() error {
		return d.updateResourcesOnce(ctx, namespace, name, edits)
	})
}

// changedMeanwhile accepts apply conflicts and invalid-patch errors for retry.
// The API server reports a failed JSON patch resourceVersion test as invalid.
func changedMeanwhile(err error) bool {
	return errors.IsConflict(err) || errors.IsInvalid(err)
}

func (d *Deployer) updateResourcesOnce(ctx context.Context, namespace, name string, edits ResourceEdits) error {
	apps := d.Dynamic.Resource(AppGVR).Namespace(namespace)
	app, err := apps.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return fmt.Errorf("app %q not found", name)
		}
		return fmt.Errorf("getting app: %w", err)
	}
	current, _, _ := unstructured.NestedStringMap(app.Object, "spec", "resources")

	apply := map[string]interface{}{}
	for _, f := range resourceFields {
		if current[f] != "" && ownedBy(app.GetManagedFields(), f, FieldManager) {
			apply[f] = current[f]
		}
	}
	var remove []string
	sets := false
	edit := func(e *PairEdit, reqField, limField string) {
		if e == nil {
			return
		}
		if e.Clear {
			delete(apply, reqField)
			delete(apply, limField)
			for _, f := range []string{reqField, limField} {
				if current[f] != "" {
					remove = append(remove, f)
				}
			}
			return
		}
		apply[reqField], apply[limField] = e.Request, e.Limit
		sets = true
	}
	edit(edits.CPU, "cpuRequest", "cpuLimit")
	edit(edits.Memory, "memoryRequest", "memoryLimit")
	if sets {
		apply["profile"] = "custom"
	}

	resourceVersion := app.GetResourceVersion()
	if len(remove) > 0 {
		ops := []map[string]interface{}{{"op": "test", "path": "/metadata/resourceVersion", "value": resourceVersion}}
		for _, f := range remove {
			ops = append(ops, map[string]interface{}{"op": "remove", "path": "/spec/resources/" + f})
		}
		patch, err := json.Marshal(ops)
		if err != nil {
			return fmt.Errorf("building the patch: %w", err)
		}
		patched, err := apps.Patch(ctx, name, types.JSONPatchType, patch, metav1.PatchOptions{FieldManager: FieldManager})
		if err != nil {
			return changedWhileSaving(name, err)
		}
		resourceVersion = patched.GetResourceVersion()
	}
	return applyResources(ctx, apps, namespace, name, resourceVersion, apply)
}

// ClaimResources claims the manifest's exact resource values as user-owned,
// including unchanged values. Conflicts retry against a fresh read.
func (d *Deployer) ClaimResources(ctx context.Context, namespace, name string, declared map[string]string) error {
	if len(declared) == 0 {
		return nil
	}
	return retry.OnError(retry.DefaultRetry, changedMeanwhile, func() error {
		return d.claimResourcesOnce(ctx, namespace, name, declared)
	})
}

func (d *Deployer) claimResourcesOnce(ctx context.Context, namespace, name string, declared map[string]string) error {
	apps := d.Dynamic.Resource(AppGVR).Namespace(namespace)
	app, err := apps.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("getting app: %w", err)
	}
	current, _, _ := unstructured.NestedStringMap(app.Object, "spec", "resources")
	apply := map[string]interface{}{}
	for _, f := range resourceFields {
		if current[f] != "" && ownedBy(app.GetManagedFields(), f, FieldManager) {
			apply[f] = current[f]
		}
	}
	for f, v := range declared {
		apply[f] = v
	}
	return applyResources(ctx, apps, namespace, name, app.GetResourceVersion(), apply)
}

// applyResources applies spec.resources fields to an App as kip, taking them
// over from other writers, only if the App is still at resourceVersion.
func applyResources(ctx context.Context, apps dynamic.ResourceInterface, namespace, name, resourceVersion string, apply map[string]interface{}) error {
	if len(apply) == 0 {
		return nil
	}
	cfg := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": AppGVR.GroupVersion().String(),
		"kind":       "App",
		"metadata": map[string]interface{}{
			"name": name, "namespace": namespace, "resourceVersion": resourceVersion,
		},
		"spec": map[string]interface{}{"resources": apply},
	}}
	if _, err := apps.Apply(ctx, name, cfg, metav1.ApplyOptions{FieldManager: FieldManager, Force: true}); err != nil {
		return changedWhileSaving(name, err)
	}
	return nil
}

func ownedBy(entries []metav1.ManagedFieldsEntry, field, manager string) bool {
	for _, owner := range fieldowners.Owners(entries, "spec", "resources", field) {
		if owner == manager {
			return true
		}
	}
	return false
}

func changedWhileSaving(name string, err error) error {
	if errors.IsConflict(err) || errors.IsInvalid(err) {
		return fmt.Errorf("app %q changed while its resources were being saved; run the command again: %w", name, err)
	}
	return fmt.Errorf("updating resources of app %q: %w", name, err)
}
