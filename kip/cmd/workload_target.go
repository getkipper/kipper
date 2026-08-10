package cmd

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/spf13/cobra"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/manifest"
)

// targetKind names a kind for the two commands that address a pod directly.
//
// It is not secretname.Kind, which cannot carry a service: that value flows
// into Env() and Secrets(), where a service has no derived Secret to name.
type targetKind string

const (
	targetKindApp      targetKind = "app"
	targetKindFunction targetKind = "function"
	targetKindService  targetKind = "service"
)

// targetKinds lists every kind these commands can address, in the order they
// are looked up and offered.
var targetKinds = []targetKind{targetKindApp, targetKindFunction, targetKindService}

func (k targetKind) gvr() schema.GroupVersionResource {
	switch k {
	case targetKindFunction:
		return manifest.FunctionGVR
	case targetKindService:
		return manifest.ServiceGVR
	default:
		return manifest.AppGVR
	}
}

// parseTargetKind reads the --kind flag. An empty string means the operator
// did not narrow by kind, which is the common case.
func parseTargetKind(value string) (targetKind, error) {
	if value == "" {
		return "", nil
	}
	for _, k := range targetKinds {
		if targetKind(value) == k {
			return k, nil
		}
	}
	names := make([]string, 0, len(targetKinds))
	for _, k := range targetKinds {
		names = append(names, string(k))
	}
	return "", fmt.Errorf("unknown kind %q, expected one of %s", value, strings.Join(names, ", "))
}

// workloadCandidate is one workload a name matched. A name can match several,
// which is the whole reason this file exists.
type workloadCandidate struct {
	kind      targetKind
	namespace string
}

func (c workloadCandidate) String() string { return string(c.kind) + "/" + c.namespace }

// workloadTargetNotFoundError reports a lookup that completed and matched
// nothing, as distinct from one that failed. Only the first may be reported as
// absence: an unavailable or forbidden API answers neither "it is here" nor
// "it is not".
type workloadTargetNotFoundError struct {
	name      string
	namespace string     // empty when the whole cluster was searched
	kind      targetKind // empty when any kind was acceptable
}

func (e *workloadTargetNotFoundError) Error() string {
	what := "no workload"
	if e.kind != "" {
		what = "no " + string(e.kind)
	}
	where := "on this cluster"
	if e.namespace != "" {
		where = "in " + e.namespace
	}
	return fmt.Sprintf("%s called %q %s", what, e.name, where)
}

// ambiguousTargetError reports a name that matched more than one workload, so
// no single one can be inferred from it. Which flag resolves it depends on how
// the candidates differ, so the message works that out rather than always
// recommending --project, which does nothing for two kinds in one namespace.
type ambiguousTargetError struct {
	name       string
	candidates []workloadCandidate
}

func (e *ambiguousTargetError) Error() string {
	namespaces := map[string]bool{}
	kinds := map[targetKind]bool{}
	for _, c := range e.candidates {
		namespaces[c.namespace] = true
		kinds[c.kind] = true
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%q matches more than one workload", e.name)
	if len(namespaces) == 1 {
		fmt.Fprintf(&b, " in %s", e.candidates[0].namespace)
	}
	b.WriteString(":\n")
	for _, c := range e.candidates {
		fmt.Fprintf(&b, "  %s\n", c)
	}

	switch {
	case len(namespaces) > 1 && len(kinds) > 1:
		b.WriteString("Name the one you mean with --project, plus --environment if the project has environments, and --kind.")
	case len(namespaces) > 1:
		b.WriteString("Name the one you mean with --project, plus --environment if the project has environments.")
	default:
		ordered := make([]string, 0, len(kinds))
		for _, k := range targetKinds {
			if kinds[k] {
				ordered = append(ordered, "--kind "+string(k))
			}
		}
		fmt.Fprintf(&b, "Name the one you mean with %s.", strings.Join(ordered, " or "))
	}
	return b.String()
}

// podPreference says what a command does when a workload is running but no
// replica is Ready.
type podPreference int

const (
	// preferReady refuses rather than hand back a pod that cannot serve.
	// Forwarding a port to one produces a failure that reads as a broken app.
	preferReady podPreference = iota
	// acceptUnready takes a Running-but-unready pod, because debugging one is a
	// legitimate reason to want a shell.
	acceptUnready
)

// workloadTargetRequest is everything the two commands know before they look
// anything up. It exists so the choosing happens in one tested place, with
// cobra's flag parsing on one side of it and the stream dial on the other.
type workloadTargetRequest struct {
	name        string
	project     string
	environment string
	kind        targetKind
	preference  podPreference
}

// workloadTargetFlags builds a request from the flags both commands share,
// refusing the two ways a scope can be given and then silently not applied.
//
// An explicit --project with an empty value is one of them: cobra reports the
// flag as changed, so resolveProjectAndEnvironment suppresses the saved project
// and returns nothing, and an unset shell variable in
// `kip exec api --project "$PROJECT"` would widen the search to every project
// rather than narrow it. An --environment with no project is the other: there
// is no namespace to build from an environment alone, so it would be dropped.
func workloadTargetFlags(cmd *cobra.Command, cluster *config.Cluster, name string, preference podPreference) (workloadTargetRequest, error) {
	if f := cmd.Flag("project"); f != nil && f.Changed && strings.TrimSpace(f.Value.String()) == "" {
		return workloadTargetRequest{}, fmt.Errorf("--project was given an empty value; name a project or leave the flag off to search every project")
	}

	project, environment := resolveProjectAndEnvironment(cmd, cluster)
	if project == "" && environment != "" {
		return workloadTargetRequest{}, fmt.Errorf("--environment %q needs a project; add --project, or set one with kip project use", environment)
	}

	kind, err := parseTargetKind(mustString(cmd, "kind"))
	if err != nil {
		return workloadTargetRequest{}, err
	}

	return workloadTargetRequest{
		name:        name,
		project:     project,
		environment: environment,
		kind:        kind,
		preference:  preference,
	}, nil
}

func mustString(cmd *cobra.Command, flag string) string {
	value, _ := cmd.Flags().GetString(flag)
	return value
}

// workloadTarget is the single workload and pod a request resolved to.
type workloadTarget struct {
	candidate workloadCandidate
	pod       *corev1.Pod
}

// containerPort reports the port the resolved pod's workload container
// declares, or 0 when it declares none.
func (t workloadTarget) containerPort() int32 {
	for _, c := range t.pod.Spec.Containers {
		if c.Name == kipperSidecarContainer {
			continue
		}
		if len(c.Ports) > 0 {
			return c.Ports[0].ContainerPort
		}
	}
	return 0
}

// resolve reduces the request to one workload and one pod, or refuses.
//
// A named project is authoritative: it confines the search, and a project that
// holds no such workload is an error rather than a reason to look in someone
// else's. Discovery runs inside that namespace too, because narrowing to a
// namespace says nothing about an App and a Service both called api inside it.
func (r workloadTargetRequest) resolve(ctx context.Context, clientset kubernetes.Interface, dyn dynamic.Interface, cluster *config.Cluster) (workloadTarget, error) {
	var namespace string
	if r.project != "" {
		namespace = cluster.ResolveNamespace(r.project, r.environment)
	}

	candidates, err := findWorkloadCandidates(ctx, clientset, dyn, namespace, r.name)
	if err != nil {
		return workloadTarget{}, err
	}
	if r.kind != "" {
		kept := candidates[:0]
		for _, c := range candidates {
			if c.kind == r.kind {
				kept = append(kept, c)
			}
		}
		candidates = kept
	}

	switch len(candidates) {
	case 0:
		return workloadTarget{}, &workloadTargetNotFoundError{name: r.name, namespace: namespace, kind: r.kind}
	case 1:
	default:
		return workloadTarget{}, &ambiguousTargetError{name: r.name, candidates: candidates}
	}
	candidate := candidates[0]

	pod, err := resolveWorkloadPod(ctx, clientset, candidate.kind, candidate.namespace, r.name, r.preference)
	if err != nil {
		return workloadTarget{}, err
	}
	return workloadTarget{candidate: candidate, pod: pod}, nil
}

// podSelector matches the pods belonging to one kind's workload of this name.
//
// app=<name> alone is not enough, and picking the candidate is not enough
// either: an App and a Service called api in one namespace have pods carrying
// the same app=api, so a kind-blind pod lookup hands back whichever sorts first
// and undoes the choice --kind just made. The kinds are told apart the same way
// their reconcilers label them — a function's pods carry
// kipper.run/resource-type=function, a service's carry kipper.run/service-type,
// and an app's carry neither. `!=` also matches a pod where the label is
// absent, which is what an app's pods are.
func podSelector(kind targetKind, name string) string {
	switch kind {
	case targetKindFunction:
		return fmt.Sprintf("app=%s,kipper.run/resource-type=function", name)
	case targetKindService:
		return fmt.Sprintf("app=%s,kipper.run/service-type", name)
	default:
		return fmt.Sprintf("app=%s,!kipper.run/service-type,kipper.run/resource-type!=function", name)
	}
}

// findWorkloadCandidates collects every workload named name. An empty namespace
// searches the whole cluster; otherwise the search is confined to that one.
//
// Every lookup here is authoritative, so any failure is the answer. A denied or
// missing GVR is not "no candidate of that kind" — treating it as absent would
// let the command select a different kind and act on the wrong workload. This
// holds during an upgrade, where one CRD can be briefly absent.
func findWorkloadCandidates(ctx context.Context, clientset kubernetes.Interface, dyn dynamic.Interface, namespace, name string) ([]workloadCandidate, error) {
	var candidates []workloadCandidate
	appNamespaces := map[string]bool{}

	for _, kind := range targetKinds {
		var lister dynamic.ResourceInterface = dyn.Resource(kind.gvr())
		if namespace != "" {
			lister = dyn.Resource(kind.gvr()).Namespace(namespace)
		}
		list, err := lister.List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("looking for a %s called %q: %w", kind, name, err)
		}
		for i := range list.Items {
			if list.Items[i].GetName() != name {
				continue
			}
			ns := list.Items[i].GetNamespace()
			candidates = append(candidates, workloadCandidate{kind: kind, namespace: ns})
			if kind == targetKindApp {
				appNamespaces[ns] = true
			}
		}
	}

	// `kip app promote` builds a Deployment with no App CR behind it, so an App
	// may have nothing to find above. Functions always have a CR, and their
	// Deployments carry the same app=<name> label, which is what the
	// resource-type clause excludes. Services are StatefulSets and never match.
	deployments, err := clientset.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=%s,app.kubernetes.io/managed-by=kipper,kipper.run/resource-type!=function", name),
	})
	if err != nil {
		return nil, fmt.Errorf("looking for a promoted app called %q: %w", name, err)
	}
	for i := range deployments.Items {
		// An ordinary App has both a CR and a Deployment, and is one candidate.
		if ns := deployments.Items[i].Namespace; !appNamespaces[ns] {
			candidates = append(candidates, workloadCandidate{kind: targetKindApp, namespace: ns})
			appNamespaces[ns] = true
		}
	}

	// The same two candidates must read the same way twice.
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].namespace != candidates[j].namespace {
			return candidates[i].namespace < candidates[j].namespace
		}
		return candidates[i].kind < candidates[j].kind
	})
	return candidates, nil
}

// resolveWorkloadPod picks the pod a command acts on.
//
// Ineligible for either command: a phase other than Running, or a deletion
// timestamp. Terminating is not a phase, so a pod on its way out stays
// status.phase=Running and a phase filter alone would hand back a pod that is
// going away.
func resolveWorkloadPod(ctx context.Context, clientset kubernetes.Interface, kind targetKind, namespace, name string, preference podPreference) (*corev1.Pod, error) {
	pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: podSelector(kind, name),
	})
	if err != nil {
		// Never "not found", and never a reason to look in another namespace.
		return nil, fmt.Errorf("finding a pod for %q in %s: %w", name, namespace, err)
	}

	var eligible, ready []*corev1.Pod
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Status.Phase != corev1.PodRunning || pod.DeletionTimestamp != nil {
			continue
		}
		eligible = append(eligible, pod)
		if podIsReady(pod) {
			ready = append(ready, pod)
		}
	}
	byName := func(pods []*corev1.Pod) {
		sort.Slice(pods, func(i, j int) bool { return pods[i].Name < pods[j].Name })
	}
	byName(eligible)
	byName(ready)

	if len(ready) > 0 {
		return ready[0], nil
	}
	if preference == acceptUnready && len(eligible) > 0 {
		return eligible[0], nil
	}
	if len(eligible) > 0 {
		return nil, fmt.Errorf("%q has %d running pod(s) in %s but none of them are ready, and forwarding a port to one that cannot serve would look like the app is broken", name, len(eligible), namespace)
	}
	return nil, fmt.Errorf("no running pod for %q in %s", name, namespace)
}

func podIsReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
