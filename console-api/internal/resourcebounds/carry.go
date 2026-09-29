package resourcebounds

import (
	"context"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

// CopyManager and MigrationManager claim user values on the target.
const (
	CopyManager      = "kipper-copy"
	MigrationManager = "kipper-migration"
)

// CarriedQuantities returns the user and held values to copy or migrate, plus
// the subset to claim as user-owned after creating the target with held values.
// Automatic values are omitted so the target can size itself.
func CarriedQuantities(app *kipperv1.App) (carried kipperv1.AppResources, user map[string]string) {
	r := app.Spec.Resources
	carried.Profile = r.Profile
	user = map[string]string{}
	for _, f := range []struct {
		field string
		value string
		out   *string
	}{
		{"cpuRequest", r.CPURequest, &carried.CPURequest},
		{"cpuLimit", r.CPULimit, &carried.CPULimit},
		{"memoryRequest", r.MemoryRequest, &carried.MemoryRequest},
		{"memoryLimit", r.MemoryLimit, &carried.MemoryLimit},
	} {
		switch ClassifyAppQuantity(f.value, app.ManagedFields, f.field) {
		case User:
			*f.out = f.value
			user[f.field] = f.value
		case Held:
			*f.out = f.value
		}
	}
	return carried, user
}

// ClaimQuantities applies values under manager if the object still has the
// given resourceVersion. Copy and migration create the target with held values
// first, so a failed claim leaves them protected from automatic sizing.
// obj identifies the resource kind.
func ClaimQuantities(ctx context.Context, c client.Client, obj client.Object, namespace, name, manager, resourceVersion string, values map[string]string) error {
	if len(values) == 0 {
		return nil
	}
	gvk, err := apiutil.GVKForObject(obj, c.Scheme())
	if err != nil {
		return err
	}
	resources := map[string]any{}
	for k, v := range values {
		resources[k] = v
	}
	cfg := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"resources": resources}}}
	cfg.SetGroupVersionKind(gvk)
	cfg.SetNamespace(namespace)
	cfg.SetName(name)
	cfg.SetResourceVersion(resourceVersion)
	return c.Apply(ctx, client.ApplyConfigurationFromUnstructured(cfg), client.FieldOwner(manager), client.ForceOwnership)
}
