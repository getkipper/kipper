package deployer

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/util/retry"
)

// StopOptions describes a stop. By, At and ForMigration are recorded only
// when the app was running; stopping a stopped app changes the reason alone,
// and only when one is given.
type StopOptions struct {
	Reason       *string
	By           string
	At           time.Time
	ForMigration bool
}

// StopResult says what a stop found.
type StopResult struct {
	// Already is true when the app was stopped before.
	Already bool
	// Freeze is true when the stored stop is a migration write freeze, which
	// a migration leaves behind.
	Freeze bool
}

// StartResult says what a start did.
type StartResult struct {
	// WasStopped is false when the app was already running.
	WasStopped bool
	// RunsNoPods is true for an app without autoscaling whose replica count
	// is zero, which therefore runs no pods after the start either.
	RunsNoPods bool
}

// Stop records a request to scale the app to zero, preserving the rest of its
// spec. It returns the previous stop state and the stored migration flag.
func (d *Deployer) Stop(ctx context.Context, namespace, name string, opts StopOptions) (StopResult, error) {
	var res StopResult
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		app, err := d.getApp(ctx, namespace, name)
		if err != nil {
			return err
		}
		stopped, found, _ := unstructured.NestedMap(app.Object, "spec", "stopped")
		res.Already = found
		if !found {
			stopped = map[string]interface{}{"at": opts.At.UTC().Format(time.RFC3339)}
			if opts.By != "" {
				stopped["by"] = opts.By
			}
			// Only a new stop can be the migration freeze. Marking an operator's
			// stop as one would have the migration leave it behind and start
			// the app on the target.
			if opts.ForMigration {
				stopped["forMigration"] = true
			}
		}
		if opts.Reason != nil {
			delete(stopped, "reason")
			if *opts.Reason != "" {
				stopped["reason"] = *opts.Reason
			}
		}
		if err := unstructured.SetNestedMap(app.Object, stopped, "spec", "stopped"); err != nil {
			return fmt.Errorf("setting the stop: %w", err)
		}
		written, err := d.Dynamic.Resource(AppGVR).Namespace(namespace).Update(ctx, app, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("updating app: %w", err)
		}
		res.Freeze, _, _ = unstructured.NestedBool(written.Object, "spec", "stopped", "forMigration")
		return RequireStopStored(written, true)
	})
	return res, err
}

// Start removes an app's stop, so it runs with its replica count and
// autoscaling again. An app at zero replicas without a stop is refused rather
// than started at a guessed count.
func (d *Deployer) Start(ctx context.Context, namespace, name string) (StartResult, error) {
	var res StartResult
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		app, err := d.getApp(ctx, namespace, name)
		if err != nil {
			return err
		}
		_, res.WasStopped, _ = unstructured.NestedMap(app.Object, "spec", "stopped")
		autoscaled, _, _ := unstructured.NestedBool(app.Object, "spec", "autoscale", "enabled")
		replicas, hasReplicas, _ := unstructured.NestedInt64(app.Object, "spec", "replicas")
		atZero := hasReplicas && replicas == 0 && !autoscaled
		if !res.WasStopped {
			if atZero {
				return fmt.Errorf("%s is scaled to zero, not stopped. Run `kip app scale %s --replicas N`", name, name)
			}
			return nil
		}
		res.RunsNoPods = atZero
		unstructured.RemoveNestedField(app.Object, "spec", "stopped")
		if _, err := d.Dynamic.Resource(AppGVR).Namespace(namespace).Update(ctx, app, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("updating app: %w", err)
		}
		return nil
	})
	return res, err
}

// Stopped reports whether the app is stopped.
func (d *Deployer) Stopped(ctx context.Context, namespace, name string) (bool, error) {
	app, err := d.getApp(ctx, namespace, name)
	if err != nil {
		return false, err
	}
	_, stopped, _ := unstructured.NestedMap(app.Object, "spec", "stopped")
	return stopped, nil
}

func (d *Deployer) getApp(ctx context.Context, namespace, name string) (*unstructured.Unstructured, error) {
	app, err := d.Dynamic.Resource(AppGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return nil, fmt.Errorf("app %q not found", name)
	}
	if err != nil {
		return nil, fmt.Errorf("getting app: %w", err)
	}
	return app, nil
}

// RequireStopStored detects an older App schema that silently drops
// spec.stopped even when the write succeeds, which leaves the app running.
func RequireStopStored(written *unstructured.Unstructured, sent bool) error {
	if !sent || written == nil {
		return nil
	}
	if _, found, _ := unstructured.NestedMap(written.Object, "spec", "stopped"); found {
		return nil
	}
	return fmt.Errorf("the cluster did not keep the stop: its App schema predates stopping apps, so %s is running. Run kip upgrade, then try again", written.GetName())
}
