package migration

import (
	"context"
	goerrors "errors"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

// drainWait gives stopped apps time to finish shutting down before migration
// reports them as possible writers.
const drainWait = 30 * time.Second

// appFrozen checks that the Deployment and its owned ReplicaSets have observed
// scale-down and their pods are gone, including terminating pods. If the
// Deployment is absent, the App must be stopped and its labeled resources gone.
// Read failures leave the app classified as a possible writer.
func appFrozen(ctx context.Context, client kubernetes.Interface, app *kipperv1.App) bool {
	deploy, err := client.AppsV1().Deployments(app.Namespace).Get(ctx, app.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		// The reconciler recreates a missing Deployment from the App, so only a
		// stopped App stays at zero. A Deployment deleted in the background also
		// leaves ReplicaSets and pods behind for a while.
		return app.Spec.Stopped != nil && nothingLeftOf(ctx, client, app)
	}
	if err != nil || deploy.Spec.Replicas == nil || *deploy.Spec.Replicas != 0 ||
		deploy.Status.ObservedGeneration < deploy.Generation || deploy.Spec.Selector == nil {
		return false
	}
	selector := metav1.FormatLabelSelector(deploy.Spec.Selector)
	sets, err := client.AppsV1().ReplicaSets(app.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return false
	}
	owned := map[types.UID]bool{}
	for i := range sets.Items {
		rs := &sets.Items[i]
		if !metav1.IsControlledBy(rs, deploy) {
			continue
		}
		if !replicaSetAtZero(rs) {
			return false
		}
		owned[rs.UID] = true
	}
	pods, err := client.CoreV1().Pods(app.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return false
	}
	for i := range pods.Items {
		if ref := metav1.GetControllerOf(&pods.Items[i]); ref != nil && owned[ref.UID] {
			return false
		}
	}
	return true
}

// nothingLeftOf reports whether no ReplicaSet or pod carrying the app's label
// is left in its namespace. A failed read counts as something left.
func nothingLeftOf(ctx context.Context, client kubernetes.Interface, app *kipperv1.App) bool {
	selector := metav1.ListOptions{LabelSelector: "app=" + app.Name}
	sets, err := client.AppsV1().ReplicaSets(app.Namespace).List(ctx, selector)
	if err != nil || len(sets.Items) > 0 {
		return false
	}
	pods, err := client.CoreV1().Pods(app.Namespace).List(ctx, selector)
	return err == nil && len(pods.Items) == 0
}

// replicaSetAtZero reports whether a ReplicaSet wants and has no pods, judged
// on a status that has caught up with its spec. A status of zero from an
// earlier generation can still be followed by a pod.
func replicaSetAtZero(rs *appsv1.ReplicaSet) bool {
	return rs.Spec.Replicas != nil && *rs.Spec.Replicas == 0 &&
		rs.Status.Replicas == 0 && rs.Status.ObservedGeneration >= rs.Generation
}

// writeFreezeWarning lists possible source writers and commands to stop them.
// It waits up to wait for stopped apps to drain and reports incomplete checks.
func (h *Handler) writeFreezeWarning(ctx context.Context, projects []string, wait time.Duration) *Step {
	type serving struct{ scope, ns, name string }
	var live []serving
	var unchecked []string
	var stopping []*kipperv1.App
	stoppingScope := map[*kipperv1.App]string{}
	for _, project := range projects {
		namespaces, err := h.getProjectNamespaces(ctx, project)
		if goerrors.Is(err, errNoNamespaces) {
			continue
		}
		if err != nil {
			unchecked = append(unchecked, "project "+project)
			continue
		}
		for _, ns := range namespaces {
			scope := h.appScope(ctx, ns)
			if scope == "" {
				scope = " --project " + project
			}
			var appList kipperv1.AppList
			if err := h.CRClient.List(ctx, &appList, crclient.InNamespace(ns)); err != nil {
				unchecked = append(unchecked, "namespace "+ns)
				continue
			}
			for i := range appList.Items {
				app := &appList.Items[i]
				switch {
				case appFrozen(ctx, h.Client, app):
				case app.Spec.Stopped != nil:
					stopping = append(stopping, app)
					stoppingScope[app] = scope
				default:
					live = append(live, serving{scope, ns, app.Name})
				}
			}
		}
	}

	deadline := time.Now().Add(wait)
	for len(stopping) > 0 && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			deadline = time.Now()
		case <-time.After(time.Second):
		}
		still := stopping[:0]
		for _, app := range stopping {
			if !appFrozen(ctx, h.Client, app) {
				still = append(still, app)
			}
		}
		stopping = still
	}
	for _, app := range stopping {
		live = append(live, serving{stoppingScope[app], app.Namespace, app.Name})
	}

	if len(live) == 0 && len(unchecked) == 0 {
		return nil
	}
	if len(live) == 0 {
		return &Step{
			Name:   "Write freeze check",
			Phase:  "structure",
			Status: StepSkipped,
			Detail: fmt.Sprintf("could not check whether anything in %s keeps serving during the data copy, so this list may be incomplete", strings.Join(unchecked, ", ")),
		}
	}
	names := make([]string, len(live))
	steps := []string{"# Stop each app before the data copy, so nothing writes after its copy:"}
	for i, a := range live {
		names[i] = a.ns + "/" + a.name
		steps = append(steps, fmt.Sprintf("kip app stop %s --for-migration%s", a.name, a.scope))
	}
	steps = append(steps, "# The target receives the apps running. To undo the stop here after an aborted migration:")
	for _, a := range live {
		steps = append(steps, fmt.Sprintf("kip app start %s%s", a.name, a.scope))
	}
	detail := fmt.Sprintf("%s keep serving during the data copy; anything written there after its copy stays on this cluster", strings.Join(names, ", "))
	if len(unchecked) > 0 {
		detail += fmt.Sprintf(". %s could not be checked, so there may be more", strings.Join(unchecked, ", "))
	}
	return &Step{Name: "Write freeze check", Phase: "structure", Status: StepSkipped, Detail: detail, ManualSteps: steps}
}
