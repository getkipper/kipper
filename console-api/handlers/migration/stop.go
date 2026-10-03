package migration

import (
	"context"
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/internal/nsowner"
)

// waitingForRestore is the reason an app arrives stopped when a database it
// is bound to stays behind for a manual restore. Started before the restore,
// it would run against an empty database, and an app that creates its schema
// on boot would collide with the restore.
const waitingForRestore = "waiting for a manual database restore"

// outgoingStop preserves operator stops and omits migration freezes. Apps
// without an operator stop receive a new stop if a bound database needs manual
// restore, protecting the empty target database from application writes.
func outgoingStop(app *kipperv1.App, dataLeftBehind func(service string) bool, now time.Time) *kipperv1.AppStopped {
	stop := app.Spec.Stopped
	if stop != nil && !stop.ForMigration {
		return stop.DeepCopy()
	}
	for _, b := range app.Spec.ServiceBindings {
		if dataLeftBehind(b.Name) {
			at := metav1.NewTime(now)
			return &kipperv1.AppStopped{Reason: waitingForRestore, By: "migration", At: &at}
		}
	}
	return nil
}

func stopSpec(stop *kipperv1.AppStopped) map[string]interface{} {
	out := map[string]interface{}{}
	if stop.Reason != "" {
		out["reason"] = stop.Reason
	}
	if stop.By != "" {
		out["by"] = stop.By
	}
	if stop.At != nil {
		out["at"] = stop.At.UTC().Format(time.RFC3339)
	}
	return out
}

// appScope returns the kip flags that select a namespace's project and
// environment, or "" when they cannot be worked out. The project comes from
// the project's claim on the namespace, the environment from its label.
func (h *Handler) appScope(ctx context.Context, namespace string) string {
	if h.Client == nil {
		return ""
	}
	ns, err := h.Client.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if err != nil {
		return ""
	}
	project, owned, err := nsowner.OfNamespace(ctx, h.CRClient, ns)
	if err != nil || !owned {
		return ""
	}
	scope := " --project " + project
	if env := ns.Labels["kipper.run/environment"]; env != "" {
		scope += " --environment " + env
	}
	return scope
}

// scopedCommand completes a kip command with the namespace's scope, or names
// the namespace when the scope is unknown.
func scopedCommand(command, scope, namespace string) string {
	if scope == "" {
		return command + "  # in " + namespace
	}
	return command + scope
}

// arrivesStoppedStep lists the apps in a namespace that arrive stopped on the
// target, with the command that starts each.
func arrivesStoppedStep(namespace, scope string, apps []string) Step {
	starts := make([]string, 0, len(apps)+1)
	starts = append(starts, "# Start each app once it can run on the target, after any manual database restore:")
	for _, app := range apps {
		starts = append(starts, scopedCommand("kip app start "+app, scope, namespace))
	}
	now := time.Now()
	return Step{
		Name:        fmt.Sprintf("Apps that arrive stopped (%s)", namespace),
		Phase:       "resources",
		Status:      StepCompleted,
		Detail:      strings.Join(apps, ", "),
		ManualSteps: starts,
		CompletedAt: &now,
	}
}

// keepsStops probes whether the installed App schema retains spec.stopped.
// The server-side dry run checks schema support independently of the running
// image version. A failed probe returns false.
func (h *Handler) keepsStops(ctx context.Context) bool {
	probe := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": kipperv1.GroupVersion.String(),
		"kind":       "App",
		"metadata":   map[string]interface{}{"generateName": "stop-probe-", "namespace": "kipper-system"},
		"spec": map[string]interface{}{
			"image": "registry.invalid/stop-probe:1", "port": int64(8080),
			"stopped": map[string]interface{}{},
		},
	}}
	if err := h.CRClient.Create(ctx, probe, crclient.DryRunAll); err != nil {
		return false
	}
	_, kept, _ := unstructured.NestedMap(probe.Object, "spec", "stopped")
	return kept
}
