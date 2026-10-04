package deployer

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/util/retry"

	"github.com/getkipper/kipper/controller/pkg/capacity"
)

// Fallback bounds and CPU target when the merged policy leaves them unset.
const (
	defaultMinReplicas int32 = 1
	defaultMaxReplicas int32 = 5
	defaultCPUTarget   int32 = 70
)

// AutoscaleChange edits an app’s autoscaling policy. Nil fields preserve stored
// values before defaults are applied. Zero clears a target; if both targets are
// unset or zero, EnableAutoscale restores the default CPU target.
type AutoscaleChange struct {
	MinReplicas  *int32
	MaxReplicas  *int32
	CPUTarget    *int32
	MemoryTarget *int32
}

// EnableResult describes the policy and replica adjustment submitted to the App.
type EnableResult struct {
	Policy capacity.Policy
	// ReplicasMoved holds the stored count before and after clamping, if changed.
	ReplicasMoved *[2]int32
	Stopped       bool
}

// DisableResult describes the replica count retained when autoscaling is disabled.
type DisableResult = capacity.SwitchOff

// InvalidPolicyRefusal is the API server refusing a write to an App whose
// stored autoscaling block breaks the rules, such as one stored before they
// existed. Policy is that stored block.
type InvalidPolicyRefusal struct {
	Policy capacity.Policy
	Err    error
}

func (e *InvalidPolicyRefusal) Error() string { return e.Err.Error() }

func (e *InvalidPolicyRefusal) Unwrap() error { return e.Err }

func storedPolicy(app *unstructured.Unstructured) *capacity.Policy {
	spec, _, _ := unstructured.NestedMap(app.Object, "spec")
	return SpecPolicy(spec)
}

// SpecPolicy reads an App spec’s autoscale block, returning nil if absent.
// A stored zero stays set, so Validate refuses a zero bound as admission does.
func SpecPolicy(spec map[string]interface{}) *capacity.Policy {
	as, found, _ := unstructured.NestedMap(spec, "autoscale")
	if !found {
		return nil
	}
	enabled, _, _ := unstructured.NestedBool(as, "enabled")
	field := func(name string) *int32 {
		v, found, _ := unstructured.NestedInt64(as, name)
		if !found {
			return nil
		}
		n := int32(v) //nolint:gosec // replica counts and targets are bounded by K8s validation
		return &n
	}
	return &capacity.Policy{
		Enabled:      enabled,
		MinReplicas:  field("minReplicas"),
		MaxReplicas:  field("maxReplicas"),
		CPUTarget:    field("cpuTarget"),
		MemoryTarget: field("memoryTarget"),
	}
}

// storedReplicas reads spec.replicas, where an absent value is the default 1.
func storedReplicas(app *unstructured.Unstructured) int32 {
	v, found, _ := unstructured.NestedInt64(app.Object, "spec", "replicas")
	if !found {
		return 1
	}
	return int32(v) //nolint:gosec // replica counts are bounded by K8s validation
}

func writePolicy(app *unstructured.Unstructured, p capacity.Policy) error {
	as := map[string]interface{}{"enabled": p.Enabled}
	for name, v := range map[string]*int32{
		"minReplicas": p.MinReplicas, "maxReplicas": p.MaxReplicas,
		"cpuTarget": p.CPUTarget, "memoryTarget": p.MemoryTarget,
	} {
		if v != nil && *v > 0 {
			as[name] = int64(*v)
		}
	}
	return unstructured.SetNestedMap(app.Object, as, "spec", "autoscale")
}

// EnableAutoscale merges edits with the stored policy, fills defaults and validates
// the result. It clamps the stored replica count in the same App update so the
// count fits the new bounds. The reconciler creates or updates the HPA.
func (d *Deployer) EnableAutoscale(ctx context.Context, namespace, name string, c AutoscaleChange) (EnableResult, error) {
	var res EnableResult
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		res = EnableResult{}
		app, err := d.getApp(ctx, namespace, name)
		if err != nil {
			return err
		}
		p := capacity.Policy{Enabled: true}
		if stored := storedPolicy(app); stored != nil {
			p.MinReplicas, p.MaxReplicas = stored.MinReplicas, stored.MaxReplicas
			p.CPUTarget, p.MemoryTarget = stored.CPUTarget, stored.MemoryTarget
		}
		for _, f := range []struct{ dst, src **int32 }{
			{&p.MinReplicas, &c.MinReplicas}, {&p.MaxReplicas, &c.MaxReplicas},
			{&p.CPUTarget, &c.CPUTarget}, {&p.MemoryTarget, &c.MemoryTarget},
		} {
			if *f.src != nil {
				*f.dst = *f.src
			}
		}
		if p.MinReplicas == nil {
			p.MinReplicas = ptr(defaultMinReplicas)
		}
		if p.MaxReplicas == nil {
			p.MaxReplicas = ptr(defaultMaxReplicas)
		}
		// The default replaces only absent or zero targets; a negative one is
		// left for Validate to refuse.
		if unset(p.CPUTarget) && unset(p.MemoryTarget) {
			p.CPUTarget = ptr(defaultCPUTarget)
		}
		if err := capacity.Validate(&p, nil); err != nil {
			return err
		}

		n := storedReplicas(app)
		if moved, ok := p.IntoBounds(n); ok {
			if err := unstructured.SetNestedField(app.Object, int64(moved), "spec", "replicas"); err != nil {
				return fmt.Errorf("setting replicas: %w", err)
			}
			res.ReplicasMoved = &[2]int32{n, moved}
		}
		if err := writePolicy(app, p); err != nil {
			return fmt.Errorf("setting autoscale: %w", err)
		}
		_, res.Stopped, _ = unstructured.NestedMap(app.Object, "spec", "stopped")
		res.Policy = p
		_, err = d.Dynamic.Resource(AppGVR).Namespace(namespace).Update(ctx, app, metav1.UpdateOptions{})
		return err
	})
	return res, err
}

// DisableAutoscale retains the bounds and saves the Deployment’s desired count,
// clamped to valid bounds, to avoid reverting to a stale App count. Stopped apps
// and failed Deployment reads preserve the stored App count.
func (d *Deployer) DisableAutoscale(ctx context.Context, namespace, name string) (DisableResult, error) {
	var res DisableResult
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		app, err := d.getApp(ctx, namespace, name)
		if err != nil {
			return err
		}
		_, stopped, _ := unstructured.NestedMap(app.Object, "spec", "stopped")
		stored := storedPolicy(app)
		res = capacity.PlanSwitchOff(stored, storedReplicas(app), stopped, func() (int32, error) {
			live, err := d.Client.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return 0, err
			}
			if live.Spec.Replicas == nil {
				return 1, nil
			}
			return *live.Spec.Replicas, nil
		})
		if res.AlreadyOff {
			return nil
		}
		if res.WritesReplicas() {
			if err := unstructured.SetNestedField(app.Object, int64(res.Replicas), "spec", "replicas"); err != nil {
				return fmt.Errorf("setting replicas: %w", err)
			}
		}
		if err := unstructured.SetNestedField(app.Object, false, "spec", "autoscale", "enabled"); err != nil {
			return fmt.Errorf("setting autoscale: %w", err)
		}
		_, err = d.Dynamic.Resource(AppGVR).Namespace(namespace).Update(ctx, app, metav1.UpdateOptions{})
		if errors.IsInvalid(err) && capacity.Validate(stored, nil) != nil {
			return &InvalidPolicyRefusal{Policy: *stored, Err: err}
		}
		return err
	})
	return res, err
}

// RemoveAutoscale deletes the autoscaling block, and with it the bounds, from
// an app whose policy is off.
func (d *Deployer) RemoveAutoscale(ctx context.Context, namespace, name string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		app, err := d.getApp(ctx, namespace, name)
		if err != nil {
			return err
		}
		p := storedPolicy(app)
		if p == nil {
			return nil
		}
		if p.Enabled {
			return fmt.Errorf("autoscaling is on for %s; switch it off first with 'kip app autoscale %s --off', then remove the bounds", name, name)
		}
		unstructured.RemoveNestedField(app.Object, "spec", "autoscale")
		_, err = d.Dynamic.Resource(AppGVR).Namespace(namespace).Update(ctx, app, metav1.UpdateOptions{})
		return err
	})
}

// AutoscaleStatus combines stored App settings with observed Deployment and HPA data.
type AutoscaleStatus struct {
	Policy        *capacity.Policy
	Replicas      int32
	Stopped       bool
	LiveDesired   *int32
	Ready         *int32
	HPAExists     bool
	CurrentMetric map[string]int32
	// Condition is the App's AutoscalingReady condition, or nil when absent.
	Condition *AutoscalingCondition
}

// AutoscalingCondition is the console-api's report on the autoscaling policy
// and the count it holds.
type AutoscalingCondition struct {
	Status, Reason, Message string
}

// autoscalingCondition reads the AutoscalingReady condition from an App's status.
func autoscalingCondition(app *unstructured.Unstructured) *AutoscalingCondition {
	conditions, _, _ := unstructured.NestedSlice(app.Object, "status", "conditions")
	for _, c := range conditions {
		m, ok := c.(map[string]interface{})
		if !ok || m["type"] != "AutoscalingReady" {
			continue
		}
		str := func(key string) string { v, _ := m[key].(string); return v }
		return &AutoscalingCondition{Status: str("status"), Reason: str("reason"), Message: str("message")}
	}
	return nil
}

// ReadAutoscaleStatus combines the App policy with Deployment counts and HPA metrics.
// Deployment read failures leave counts nil; a missing HPA, or one the caller
// may not read, leaves metrics empty. App read failures and other HPA read
// errors are returned.
func (d *Deployer) ReadAutoscaleStatus(ctx context.Context, namespace, name string) (AutoscaleStatus, error) {
	app, err := d.getApp(ctx, namespace, name)
	if err != nil {
		return AutoscaleStatus{}, err
	}
	st := AutoscaleStatus{Policy: storedPolicy(app), Replicas: storedReplicas(app), CurrentMetric: map[string]int32{}, Condition: autoscalingCondition(app)}
	_, st.Stopped, _ = unstructured.NestedMap(app.Object, "spec", "stopped")
	if dep, err := d.Client.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{}); err == nil {
		n := int32(1)
		if dep.Spec.Replicas != nil {
			n = *dep.Spec.Replicas
		}
		st.LiveDesired = &n
		ready := dep.Status.ReadyReplicas
		st.Ready = &ready
	}
	hpa, err := d.Client.AutoscalingV2().HorizontalPodAutoscalers(namespace).Get(ctx, name, metav1.GetOptions{})
	switch {
	case err == nil:
		st.HPAExists = true
		for _, m := range hpa.Status.CurrentMetrics {
			if m.Resource != nil && m.Resource.Current.AverageUtilization != nil {
				st.CurrentMetric[string(m.Resource.Name)] = *m.Resource.Current.AverageUtilization
			}
		}
	case errors.IsNotFound(err), errors.IsForbidden(err):
		// A project operator may read Deployments but not autoscalers, so the metrics stay unknown.
	default:
		return st, fmt.Errorf("reading the autoscaler: %w", err)
	}
	return st, nil
}

func ptr(v int32) *int32 { return &v }

func unset(v *int32) bool { return v == nil || *v == 0 }

// Autoscaled reports whether the app's autoscaling policy is on.
func (d *Deployer) Autoscaled(ctx context.Context, namespace, name string) (bool, error) {
	app, err := d.getApp(ctx, namespace, name)
	if err != nil {
		return false, err
	}
	p := storedPolicy(app)
	return p != nil && p.Enabled, nil
}
