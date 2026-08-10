package migration

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/controllers"
)

// clusterCapacity sums a cluster's allocatable node resources, the resource
// requests of its non-terminal pods, and the storage its PersistentVolumeClaims
// request. All CPU is in millicores, memory and storage in bytes.
type clusterCapacity struct {
	AllocatableCPU     int64 `json:"allocatable_cpu_millis"`
	AllocatableMemory  int64 `json:"allocatable_memory_bytes"`
	AllocatableStorage int64 `json:"allocatable_storage_bytes"`
	RequestedCPU       int64 `json:"requested_cpu_millis"`
	RequestedMemory    int64 `json:"requested_memory_bytes"`
	RequestedStorage   int64 `json:"requested_storage_bytes"`
}

// FreeCPU returns the unrequested CPU headroom.
func (c clusterCapacity) FreeCPU() int64 { return c.AllocatableCPU - c.RequestedCPU }

// FreeMemory returns the unrequested memory headroom.
func (c clusterCapacity) FreeMemory() int64 { return c.AllocatableMemory - c.RequestedMemory }

// FreeStorage returns the storage headroom: node ephemeral-storage capacity
// minus what existing PVCs already request. Longhorn provisions volumes on
// the node filesystem, so this is an approximation of what a new PVC can
// still claim, and deliberately errs on the honest side of "unknown".
func (c clusterCapacity) FreeStorage() int64 { return c.AllocatableStorage - c.RequestedStorage }

// TargetCapacityHandler reports this cluster's headroom so the source can
// refuse a migration that cannot fit before anything is transferred. Runs
// before a session exists, so it authenticates against the migration token
// like the projects listing does. The optional projects query parameter
// names the projects the migration will overwrite: their existing workloads
// are replaced by the incoming ones, so counting them as consumed would
// double-count and refuse an overwrite that actually fits.
// GET /api/v1/migrate-target/capacity?projects=shop,blog
func (h *Handler) TargetCapacityHandler(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	if err := ValidateToken(ctx, h.Client, r.Header.Get("X-Migration-Secret")); err != nil {
		respondError(w, http.StatusUnauthorized, "invalid migration secret")
		return
	}

	var excludeProjects []string
	if raw := r.URL.Query().Get("projects"); raw != "" {
		excludeProjects = strings.Split(raw, ",")
	}

	capacity, err := h.capacity(ctx, excludeProjects)
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("computing capacity: %v", err))
		return
	}

	// The version rides along so the source can run its major-version check
	// at plan time; the accept handshake otherwise only reveals it on the
	// call that consumes the token.
	respondJSON(w, http.StatusOK, struct {
		clusterCapacity
		TargetVersion string `json:"target_version"`
	}{capacity, BuildVersion})
}

// capacity computes this cluster's allocatable resources and current
// requests, leaving out namespaces owned by the given projects.
func (h *Handler) capacity(ctx context.Context, excludeProjects []string) (clusterCapacity, error) {
	var c clusterCapacity

	excluded, err := h.projectNamespaceSet(ctx, excludeProjects)
	if err != nil {
		return c, err
	}

	nodes, err := h.Client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return c, fmt.Errorf("listing nodes: %w", err)
	}
	for _, node := range nodes.Items {
		c.AllocatableCPU += node.Status.Allocatable.Cpu().MilliValue()
		c.AllocatableMemory += node.Status.Allocatable.Memory().Value()
		c.AllocatableStorage += node.Status.Allocatable.StorageEphemeral().Value()
	}

	pods, err := h.Client.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return c, fmt.Errorf("listing pods: %w", err)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		if excluded[pod.Namespace] {
			continue
		}
		cpu, mem := podRequests(pod)
		c.RequestedCPU += cpu
		c.RequestedMemory += mem
	}

	pvcs, err := h.Client.CoreV1().PersistentVolumeClaims("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return c, fmt.Errorf("listing volume claims: %w", err)
	}
	for i := range pvcs.Items {
		if excluded[pvcs.Items[i].Namespace] {
			continue
		}
		c.RequestedStorage += pvcStorageRequest(&pvcs.Items[i])
	}

	return c, nil
}

// projectNamespaceSet resolves project names to the set of namespaces they
// own on this cluster. Unknown projects resolve to nothing.
func (h *Handler) projectNamespaceSet(ctx context.Context, projects []string) (map[string]bool, error) {
	set := make(map[string]bool)
	if len(projects) == 0 {
		return set, nil
	}
	names := make(map[string]bool, len(projects))
	for _, p := range projects {
		if p != "" {
			names[p] = true
		}
	}
	nsList, err := h.Client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{
		LabelSelector: "kipper.run/project",
	})
	if err != nil {
		return nil, fmt.Errorf("listing namespaces: %w", err)
	}
	for _, ns := range nsList.Items {
		if names[ns.Labels["kipper.run/project"]] {
			set[ns.Name] = true
		}
	}
	return set, nil
}

// pvcStorageRequest returns the storage one claim requests.
func pvcStorageRequest(pvc *corev1.PersistentVolumeClaim) int64 {
	if req, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
		return req.Value()
	}
	return 0
}

// namespacesResourceRequests sums what the given namespaces need on the
// target. App demand comes from the App specs, not from live pods: the guide
// tells operators to freeze writes (scale to zero) before migrating, and
// live-pod measurement would zero the demand exactly when the check matters
// most. Everything else — services, functions, running jobs — is measured
// from its live pods, and PVC storage from the claims.
func (h *Handler) namespacesResourceRequests(ctx context.Context, namespaces []string) (cpu, mem, storage int64, err error) {
	for _, ns := range namespaces {
		var appList kipperv1.AppList
		if err := h.CRClient.List(ctx, &appList, crclient.InNamespace(ns)); err != nil {
			return 0, 0, 0, fmt.Errorf("listing apps in %s: %w", ns, err)
		}

		// Pods are matched to Apps by the generic "app" label, which Function
		// and Service pods carry too. A name shared across kinds keeps its
		// live pods in the measurement AND its spec demand: the shared pods
		// count twice, which over-reserves — the safe direction for a
		// refusal check — while dropping either side could admit an
		// undersized target for a frozen app.
		ambiguous, err := h.crossKindNames(ctx, ns)
		if err != nil {
			return 0, 0, 0, err
		}

		appNames := make(map[string]bool, len(appList.Items))
		for i := range appList.Items {
			app := &appList.Items[i]
			if !ambiguous[app.Name] {
				appNames[app.Name] = true
			}
			cpuOne, memOne := controllers.AppRequests(app.Spec.Resources)
			replicas := int64(plannedReplicas(app))
			cpu += replicas * cpuOne
			mem += replicas * memOne
		}

		pods, err := h.Client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return 0, 0, 0, fmt.Errorf("listing pods in %s: %w", ns, err)
		}
		for i := range pods.Items {
			pod := &pods.Items[i]
			if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
				continue
			}
			// App pods are already counted from their specs above.
			if appNames[pod.Labels["app"]] {
				continue
			}
			c, m := podRequests(pod)
			cpu += c
			mem += m
		}

		pvcs, err := h.Client.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return 0, 0, 0, fmt.Errorf("listing volume claims in %s: %w", ns, err)
		}
		for i := range pvcs.Items {
			storage += pvcStorageRequest(&pvcs.Items[i])
		}
	}
	return cpu, mem, storage, nil
}

// crossKindNames returns the names in a namespace that a Function or Service
// also uses, where the "app" pod label cannot distinguish the workloads.
func (h *Handler) crossKindNames(ctx context.Context, ns string) (map[string]bool, error) {
	names := make(map[string]bool)
	var fnList kipperv1.FunctionList
	if err := h.CRClient.List(ctx, &fnList, crclient.InNamespace(ns)); err != nil {
		return nil, fmt.Errorf("listing functions in %s: %w", ns, err)
	}
	for i := range fnList.Items {
		names[fnList.Items[i].Name] = true
	}
	var svcList kipperv1.ServiceList
	if err := h.CRClient.List(ctx, &svcList, crclient.InNamespace(ns)); err != nil {
		return nil, fmt.Errorf("listing services in %s: %w", ns, err)
	}
	for i := range svcList.Items {
		names[svcList.Items[i].Name] = true
	}
	return names, nil
}

// plannedReplicas returns the replica count an app needs room for on the
// target. A frozen or scale-to-zero app still needs space for at least one
// replica when it wakes after cutover; an autoscaled app needs at least its
// HPA floor.
func plannedReplicas(app *kipperv1.App) int32 {
	if app.Spec.Autoscale != nil && app.Spec.Autoscale.Enabled {
		if app.Spec.Autoscale.MinReplicas > 1 {
			return app.Spec.Autoscale.MinReplicas
		}
		return 1
	}
	if app.Spec.Replicas != nil && *app.Spec.Replicas > 1 {
		return *app.Spec.Replicas
	}
	return 1
}

// podRequests returns the pod's effective scheduling requests, the way the
// scheduler counts them. Init containers run in order, and a one-shot init
// stage runs alongside every restartable (sidecar) init container already
// started, so each stage's request is the init's own plus the cumulative
// sidecars before it. The pod's request is the largest of those stages and
// the running state (containers plus all sidecars), plus pod overhead.
// Counting only regular containers understates pods like build Jobs, whose
// clone init container reserves real resources.
func podRequests(pod *corev1.Pod) (cpu, mem int64) {
	var sidecarCPU, sidecarMem int64
	var initCPUMax, initMemMax int64
	for i := range pod.Spec.InitContainers {
		c := &pod.Spec.InitContainers[i]
		reqCPU := c.Resources.Requests.Cpu().MilliValue()
		reqMem := c.Resources.Requests.Memory().Value()
		if c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			sidecarCPU += reqCPU
			sidecarMem += reqMem
			continue
		}
		initCPUMax = max(initCPUMax, sidecarCPU+reqCPU)
		initMemMax = max(initMemMax, sidecarMem+reqMem)
	}
	for _, c := range pod.Spec.Containers {
		cpu += c.Resources.Requests.Cpu().MilliValue()
		mem += c.Resources.Requests.Memory().Value()
	}
	cpu = max(cpu+sidecarCPU, initCPUMax)
	mem = max(mem+sidecarMem, initMemMax)
	if pod.Spec.Overhead != nil {
		cpu += pod.Spec.Overhead.Cpu().MilliValue()
		mem += pod.Spec.Overhead.Memory().Value()
	}
	return cpu, mem
}

// migrationDemand is what the selected projects need on the target.
type migrationDemand struct {
	CPU     int64
	Memory  int64
	Storage int64
}

// computeMigrationDemand sums the selected projects' resource needs.
func (h *Handler) computeMigrationDemand(ctx context.Context, projects []string) (migrationDemand, error) {
	var namespaces []string
	for _, project := range projects {
		ns, err := h.getProjectNamespaces(ctx, project)
		if err != nil {
			return migrationDemand{}, fmt.Errorf("resolving namespaces for %s: %w", project, err)
		}
		namespaces = append(namespaces, ns...)
	}
	cpu, mem, storage, err := h.namespacesResourceRequests(ctx, namespaces)
	if err != nil {
		return migrationDemand{}, err
	}
	return migrationDemand{CPU: cpu, Memory: mem, Storage: storage}, nil
}

// fetchTargetCapacity queries the target's headroom and version. The target
// leaves the selected projects' own workloads out of its consumption figures:
// an overwrite or retry replaces them with the incoming demand, so counting
// both would refuse migrations that fit.
func (h *Handler) fetchTargetCapacity(token *Token, projects []string) (clusterCapacity, string, error) {
	resp, err := h.callTarget(token, "GET",
		"/api/v1/migrate-target/capacity?projects="+url.QueryEscape(strings.Join(projects, ",")), nil)
	if err != nil {
		return clusterCapacity{}, "", fmt.Errorf("querying target capacity: %w", err)
	}

	var target clusterCapacity
	fields := []struct {
		key  string
		dest *int64
	}{
		{"allocatable_cpu_millis", &target.AllocatableCPU},
		{"allocatable_memory_bytes", &target.AllocatableMemory},
		{"allocatable_storage_bytes", &target.AllocatableStorage},
		{"requested_cpu_millis", &target.RequestedCPU},
		{"requested_memory_bytes", &target.RequestedMemory},
		{"requested_storage_bytes", &target.RequestedStorage},
	}
	for _, f := range fields {
		// A missing or non-numeric field errors instead of reading as zero:
		// zero allocatable would fail every check, but zero *requested* would
		// silently wave demand through a full cluster.
		v, ok := resp[f.key].(float64)
		if !ok {
			return clusterCapacity{}, "", fmt.Errorf("target capacity response is missing %s — upgrade the target cluster", f.key)
		}
		*f.dest = int64(v)
	}
	version, _ := resp["target_version"].(string)
	return target, version, nil
}

// capacityShortfall renders the refusal message when the demand does not fit,
// or "" when it does. Shared by the plan screen and the start precheck so the
// two can never disagree.
func capacityShortfall(need migrationDemand, target clusterCapacity) string {
	if need.CPU > target.FreeCPU() || need.Memory > target.FreeMemory() {
		return fmt.Sprintf(
			"target cluster is too small for this migration: the selected projects request %dm CPU and %dMi memory, the target has %dm CPU and %dMi memory free; free up capacity or use a bigger box",
			need.CPU, need.Memory/(1024*1024), target.FreeCPU(), target.FreeMemory()/(1024*1024))
	}
	// Storage is compared only when the target reports its disk capacity —
	// a zero allocatable figure means the target could not measure it, and
	// refusing on an unknown would block every migration.
	if target.AllocatableStorage > 0 && need.Storage > target.FreeStorage() {
		return fmt.Sprintf(
			"target cluster has too little disk for this migration: the selected projects' volumes request %dMi, the target has %dMi of storage headroom; use a bigger disk or free up volumes",
			need.Storage/(1024*1024), target.FreeStorage()/(1024*1024))
	}
	return ""
}

// checkTargetCapacity fails fast when the target cannot take the projects'
// current resource requests. Called with the decoded token before the accept
// handshake, so nothing has been consumed or created when it refuses.
func (h *Handler) checkTargetCapacity(ctx context.Context, token *Token, projects []string) error {
	need, err := h.computeMigrationDemand(ctx, projects)
	if err != nil {
		return err
	}
	target, _, err := h.fetchTargetCapacity(token, projects)
	if err != nil {
		return err
	}
	if msg := capacityShortfall(need, target); msg != "" {
		return fmt.Errorf("%s", msg)
	}
	return nil
}
