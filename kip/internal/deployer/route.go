package deployer

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/util/retry"
)

// SetRoute updates the host and path while preserving other route settings.
// Empty values preserve existing values. A new route requires a host and defaults
// to path "/". Adding a route may trigger instance proxy injection and a rollout.
func (d *Deployer) SetRoute(ctx context.Context, namespace, name, host, path string) error {
	return d.updateApp(ctx, namespace, name, func(app *unstructured.Unstructured) error {
		route, found, _ := unstructured.NestedMap(app.Object, "spec", "route")
		if !found {
			if host == "" {
				return fmt.Errorf("app %q has no route, so it needs a host", name)
			}
			if path == "" {
				path = "/"
			}
			route = map[string]interface{}{}
		}
		if host != "" {
			route["host"] = host
		}
		if path != "" {
			route["path"] = path
		}
		return unstructured.SetNestedMap(app.Object, route, "spec", "route")
	})
}

// RemoveRoute deletes the route and returns descriptions of the settings removed.
func (d *Deployer) RemoveRoute(ctx context.Context, namespace, name string) ([]string, error) {
	var dropped []string
	err := d.updateApp(ctx, namespace, name, func(app *unstructured.Unstructured) error {
		route, found, _ := unstructured.NestedMap(app.Object, "spec", "route")
		if !found {
			return fmt.Errorf("app %q has no route to remove", name)
		}
		dropped = RouteGates(route)
		unstructured.RemoveNestedField(app.Object, "spec", "route")
		return nil
	})
	return dropped, err
}

// RouteGates describes configured access controls, redirects, CSP entries and rate limits.
func RouteGates(route map[string]interface{}) []string {
	var gates []string
	if v, _ := route["basicAuth"].(bool); v {
		gates = append(gates, "basic auth")
	}
	if v, _ := route["requireApiKey"].(bool); v {
		gates = append(gates, "API key gate")
	}
	list := func(field, label string) {
		values, _, _ := unstructured.NestedStringSlice(route, field)
		if len(values) > 0 {
			gates = append(gates, label+" "+strings.Join(values, ", "))
		}
	}
	list("internalPaths", "internal paths")
	list("publicPaths", "public paths")
	list("redirectFrom", "redirect domains")
	list("cspAllowlist", "CSP allowlist")
	if redirects, _, _ := unstructured.NestedSlice(route, "redirects"); len(redirects) > 0 {
		gates = append(gates, fmt.Sprintf("%d redirect rules", len(redirects)))
	}
	if v, found, _ := unstructured.NestedInt64(route, "rateLimit"); found && v > 0 {
		gates = append(gates, fmt.Sprintf("rate limit %d/s", v))
	}
	return gates
}

// A conflict retries change against a fresh App, so change must be safe to repeat.
func (d *Deployer) updateApp(ctx context.Context, namespace, name string, change func(*unstructured.Unstructured) error) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		app, err := d.Dynamic.Resource(AppGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				return fmt.Errorf("app %q not found", name)
			}
			return fmt.Errorf("getting app: %w", err)
		}
		if err := change(app); err != nil {
			return err
		}
		if _, err := d.Dynamic.Resource(AppGVR).Namespace(namespace).Update(ctx, app, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("updating app: %w", err)
		}
		return nil
	})
}
