package deployer

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/util/retry"

	"github.com/getkipper/kipper/controller/pkg/healthcheck"
)

// HealthEdit is a change to an App's declared health check. Only the fields
// that are set change; Type auto removes the check so Kipper decides.
type HealthEdit struct {
	Type                  string
	Path                  *string
	Port                  *int32
	StartupTimeoutSeconds *int32
	TimeoutSeconds        *int32
}

// Any reports whether the edit includes any settings.
func (e HealthEdit) Any() bool {
	return e.Type != "" || e.Path != nil || e.Port != nil || e.StartupTimeoutSeconds != nil || e.TimeoutSeconds != nil
}

// UpdateHealth merges the supplied settings into the declared check.
// Changes to the rendered probe trigger a rollout.
func (d *Deployer) UpdateHealth(ctx context.Context, namespace, name string, edit HealthEdit) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		app, err := d.Dynamic.Resource(AppGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				return fmt.Errorf("app %q not found", name)
			}
			return fmt.Errorf("getting app: %w", err)
		}
		spec, _, _ := unstructured.NestedMap(app.Object, "spec")
		if err := applyHealthEdit(spec, edit); err != nil {
			return err
		}
		app.Object["spec"] = spec
		written, err := d.Dynamic.Resource(AppGVR).Namespace(namespace).Update(ctx, app, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("updating app: %w", err)
		}
		_, sent := spec["health"]
		return RequireHealthStored(written, sent)
	})
}

// RequireHealthStored detects older CRD schemas that silently discard
// spec.health even when the write succeeds.
func RequireHealthStored(written *unstructured.Unstructured, sent bool) error {
	if !sent || written == nil {
		return nil
	}
	if _, found, _ := unstructured.NestedMap(written.Object, "spec", "health"); found {
		return nil
	}
	return fmt.Errorf("the cluster did not keep the health check: its App schema predates health checks. Run kip upgrade, then try again")
}

// applyHealthEdit merges settings, removes fields unsupported by the
// selected type, and validates the result.
func applyHealthEdit(spec map[string]interface{}, edit HealthEdit) error {
	if edit.Type == healthcheck.Auto {
		delete(spec, "health")
		return nil
	}
	check := healthFromSpec(spec)
	if edit.Type != "" {
		check.Type = edit.Type
	}
	if edit.Path != nil {
		check.Path = *edit.Path
		if edit.Type == "" {
			check.Type = "http"
		}
	}
	for _, f := range []struct {
		from *int32
		to   **int32
	}{{edit.Port, &check.Port}, {edit.StartupTimeoutSeconds, &check.StartupTimeoutSeconds}, {edit.TimeoutSeconds, &check.TimeoutSeconds}} {
		if f.from != nil {
			v := *f.from
			*f.to = &v
		}
	}
	if check.Type == "" {
		return fmt.Errorf("choose a check with --health tcp or --health http --health-path <path>")
	}
	check.Normalize()
	port, _, _ := unstructured.NestedInt64(spec, "port")
	if err := check.Validate(int32(port)); err != nil { //nolint:gosec // the CRD bounds spec.port to a valid port
		return err
	}
	spec["health"] = healthToSpec(check)
	return nil
}

func healthFromSpec(spec map[string]interface{}) healthcheck.Check {
	h, _, _ := unstructured.NestedMap(spec, "health")
	num := func(key string) *int32 {
		v, found, _ := unstructured.NestedInt64(h, key)
		if !found {
			return nil
		}
		n := int32(v) //nolint:gosec // the CRD bounds every health number well inside int32
		return &n
	}
	typ, _, _ := unstructured.NestedString(h, "type")
	path, _, _ := unstructured.NestedString(h, "path")
	return healthcheck.Check{
		Type: typ, Path: path, Port: num("port"),
		StartupTimeoutSeconds: num("startupTimeoutSeconds"), TimeoutSeconds: num("timeoutSeconds"),
	}
}

func healthToSpec(c healthcheck.Check) map[string]interface{} {
	out := map[string]interface{}{"type": c.Type}
	if c.Path != "" {
		out["path"] = c.Path
	}
	for key, v := range map[string]*int32{"port": c.Port, "startupTimeoutSeconds": c.StartupTimeoutSeconds, "timeoutSeconds": c.TimeoutSeconds} {
		if v != nil {
			out[key] = int64(*v)
		}
	}
	return out
}
