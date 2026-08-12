// Package function provides the kip CLI's view of serverless functions.
//
// Functions are modelled as Kipper Function CRs (functions.kipper.run).
// The CLI is a thin client: it builds a CR from the user's flags, applies
// it via the dynamic client, and lets the FunctionReconciler in the
// console-api do the work of creating Deployments, Services, KEDA
// scaling, Ingresses, and CronJobs as appropriate for the trigger type.
//
// HTTP triggers and cron triggers are fully supported by the controller.
// Event triggers (postgres, mysql, redis, minio) are recorded on the CR
// but their polling sidecars are not yet reconciled — that lands in a
// later phase of the function power-up work. The CLI warns the user when
// they create an event-triggered function so the gap is visible.
package function

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/getkipper/kipper/kip/internal/manifest"
	"github.com/getkipper/kipper/kip/internal/workload"
)

// TriggerType identifies what activates a function.
type TriggerType string

const (
	TriggerHTTP     TriggerType = "http"
	TriggerCron     TriggerType = "cron"
	TriggerPostgres TriggerType = "postgres"
	TriggerMySQL    TriggerType = "mysql"
	TriggerRedis    TriggerType = "redis"
	TriggerMinIO    TriggerType = "minio"
)

// Options describes a function to create.
type Options struct {
	Name      string
	Namespace string
	Image     string
	Port      int32
	Runtime   string

	// Inline source (when not using --image).
	Code         string
	Dependencies map[string]string

	// Volume mounts (each entry is a Volume CR name and a container path).
	Volumes []VolumeMount

	// Trigger
	Trigger  TriggerType
	Schedule string // cron trigger only

	// Event trigger config (postgres, mysql, redis, minio)
	SourceName  string
	Query       string
	MarkDone    string
	RedisList   string
	MinioBucket string
}

// VolumeMount is the Options-side mirror of api/v1alpha1.AppVolumeMount.
type VolumeMount struct {
	Name      string
	MountPath string
}

// Status holds summary info for a function as displayed in `kip function list`.
type Status struct {
	Name    string `json:"name"`
	Trigger string `json:"trigger"`
	Image   string `json:"image"`
	Ready   int32  `json:"ready"`
	URL     string `json:"url"`
	Status  string `json:"status"`
}

// Manager creates and manages serverless functions through the Function CR.
//
// All operations go through the dynamic client; the controller in the
// console-api handles all the rendering into Deployments, Services,
// CronJobs, etc.
type Manager struct {
	Dynamic dynamic.Interface
}

// Create writes a Function CR derived from opts. Existing CRs of the same
// name are updated in place rather than failing.
func (m *Manager) Create(ctx context.Context, opts Options) error {
	if m.Dynamic == nil {
		return fmt.Errorf("function manager is not configured with a dynamic client")
	}
	release, err := workload.Reserve(ctx, m.Dynamic, opts.Namespace, opts.Name, "function")
	if err != nil {
		return err
	}

	cr := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "kipper.run/v1alpha1",
			"kind":       "Function",
			"metadata": map[string]interface{}{
				"name":      opts.Name,
				"namespace": opts.Namespace,
			},
			"spec": buildFunctionSpec(opts),
		},
	}

	gvr := manifest.FunctionGVR
	// Whether a function of this name turned out to exist. The update below
	// consumes the AlreadyExists that says so, and by the time this function
	// returns the error is the update's own, so the fact has to be carried
	// rather than re-read from the error: rolling the reservation back over a
	// function that exists would free its name for another kind.
	existed := false
	_, err = m.Dynamic.Resource(gvr).Namespace(opts.Namespace).Create(ctx, cr, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		existed = true
		existing, getErr := m.Dynamic.Resource(gvr).Namespace(opts.Namespace).Get(ctx, opts.Name, metav1.GetOptions{})
		if getErr != nil {
			return fmt.Errorf("getting existing function: %w", getErr)
		}
		cr.SetResourceVersion(existing.GetResourceVersion())
		_, err = m.Dynamic.Resource(gvr).Namespace(opts.Namespace).Update(ctx, cr, metav1.UpdateOptions{})
	}
	if err != nil {
		if !existed {
			release()
		}
		return fmt.Errorf("applying function CR: %w", err)
	}
	return nil
}

// Delete removes the Function CR. The controller's finalizer cascades the
// deletion to all owned workloads (Deployment, Service, CronJob, etc.).
func (m *Manager) Delete(ctx context.Context, namespace, name string) error {
	if m.Dynamic == nil {
		return fmt.Errorf("function manager is not configured with a dynamic client")
	}

	err := m.Dynamic.Resource(manifest.FunctionGVR).Namespace(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if errors.IsNotFound(err) {
		return fmt.Errorf("function %q not found", name)
	}
	return err
}

// List returns all functions in a namespace by reading Function CRs.
func (m *Manager) List(ctx context.Context, namespace string) ([]Status, error) {
	if m.Dynamic == nil {
		return nil, fmt.Errorf("function manager is not configured with a dynamic client")
	}

	crList, err := m.Dynamic.Resource(manifest.FunctionGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing function CRs: %w", err)
	}

	result := make([]Status, 0, len(crList.Items))
	for i := range crList.Items {
		result = append(result, functionStatusFromCR(&crList.Items[i]))
	}
	return result, nil
}

// functionStatusFromCR derives the CLI's display status from a Function CR.
func functionStatusFromCR(cr *unstructured.Unstructured) Status {
	image, _, _ := unstructured.NestedString(cr.Object, "spec", "image")
	url, _, _ := unstructured.NestedString(cr.Object, "status", "endpoint")

	trigger := "http"
	if triggers, found, _ := unstructured.NestedSlice(cr.Object, "spec", "triggers"); found && len(triggers) > 0 {
		if first, ok := triggers[0].(map[string]interface{}); ok {
			if t, ok := first["type"].(string); ok && t != "" {
				trigger = t
			}
		}
	}

	var ready int32
	if r, found, _ := unstructured.NestedInt64(cr.Object, "status", "replicas"); found {
		ready = int32(r) //nolint:gosec // replica counts are well within int32
	}

	phase, _, _ := unstructured.NestedString(cr.Object, "status", "phase")
	if phase == "" {
		phase = "Idle"
	}

	return Status{
		Name:    cr.GetName(),
		Trigger: trigger,
		Image:   image,
		Ready:   ready,
		Status:  strings.ToLower(phase),
		URL:     url,
	}
}

// buildFunctionSpec maps Options to the FunctionSpec map. This is the
// inverse of FunctionSpec in console-api/api/v1alpha1/function_types.go;
// it must stay aligned with that schema.
func buildFunctionSpec(opts Options) map[string]interface{} {
	spec := map[string]interface{}{}
	if opts.Image != "" {
		spec["image"] = opts.Image
	}
	if opts.Port != 0 {
		spec["port"] = int64(opts.Port)
	}
	if opts.Runtime != "" {
		spec["runtime"] = opts.Runtime
	}

	if opts.Code != "" || len(opts.Dependencies) > 0 {
		source := map[string]interface{}{}
		if opts.Code != "" {
			source["code"] = opts.Code
		}
		if len(opts.Dependencies) > 0 {
			deps := make(map[string]interface{}, len(opts.Dependencies))
			for k, v := range opts.Dependencies {
				deps[k] = v
			}
			source["dependencies"] = deps
		}
		spec["source"] = source
	}

	if len(opts.Volumes) > 0 {
		volumes := make([]interface{}, 0, len(opts.Volumes))
		for _, vm := range opts.Volumes {
			volumes = append(volumes, map[string]interface{}{
				"name":      vm.Name,
				"mountPath": vm.MountPath,
			})
		}
		spec["volumes"] = volumes
	}

	trigger := buildTrigger(opts)
	if trigger != nil {
		spec["triggers"] = []interface{}{trigger}
	}

	return spec
}

// buildTrigger constructs the FunctionTrigger map for the function's
// trigger type. Returns nil when the trigger is empty (the CR will then
// default to HTTP via the controller's hasHTTPTrigger logic).
func buildTrigger(opts Options) map[string]interface{} {
	if opts.Trigger == "" {
		return nil
	}

	t := map[string]interface{}{"type": string(opts.Trigger)}

	if opts.Trigger == TriggerCron && opts.Schedule != "" {
		t["schedule"] = opts.Schedule
	}

	config := map[string]interface{}{}
	if opts.SourceName != "" {
		config["source"] = opts.SourceName
	}
	if opts.Query != "" {
		config["query"] = opts.Query
	}
	if opts.MarkDone != "" {
		config["markDone"] = opts.MarkDone
	}
	if opts.RedisList != "" {
		config["list"] = opts.RedisList
	}
	if opts.MinioBucket != "" {
		config["bucket"] = opts.MinioBucket
	}
	if len(config) > 0 {
		t["config"] = config
	}
	return t
}

// IsEventTrigger reports whether the trigger is one of the event-driven
// types (postgres, mysql, redis, minio). The CR records these but the
// controller's polling-sidecar reconciliation lands in a later phase.
func IsEventTrigger(t TriggerType) bool {
	switch t {
	case TriggerPostgres, TriggerMySQL, TriggerRedis, TriggerMinIO:
		return true
	}
	return false
}
