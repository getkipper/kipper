package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/getkipper/kipper/console-api/handlers"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	quotapkg "github.com/getkipper/kipper/console-api/quota"
	"github.com/getkipper/kipper/controller/pkg/labels"
)

const (
	modeConfigMapName      = "kipper-mode"
	modeConfigMapNamespace = "kipper-system"
	resourceLogConfigMap   = "kipper-resource-log"
	maxLogEntries          = 50
	checkInterval          = 60 * time.Second

	modeAuto   = "auto"
	modeExpert = "expert"
)

// usageObservation tracks a single metrics observation for a workload.
type usageObservation struct {
	CPUMillis    int64
	MemoryBytes  int64
	OOMKilled    bool
	StuckPodName string
}

// workloadKey uniquely identifies a workload for history tracking.
type workloadKey struct {
	Namespace string
	Name      string
}

// pendingOOMMark records an OOM event evaluateAndAdjust acted on. The caller
// commits it to oomHandledAt only after the workload update succeeds, so a
// failed update leaves the OOM eligible for retry rather than permanently
// suppressed.
type pendingOOMMark struct {
	key string
	at  time.Time
}

// ResourceLogEntry represents a single auto-mode resource change.
type ResourceLogEntry struct {
	Time      string `json:"time"`
	App       string `json:"app"`
	Namespace string `json:"namespace"`
	Action    string `json:"action"`
	From      string `json:"from"`
	To        string `json:"to"`
	Reason    string `json:"reason"`
	// Severity overrides the action-derived severity when the alert is
	// created. Empty falls back to alertSeverity(Action).
	Severity string `json:"severity,omitempty"`
}

const (
	// defaultOOMCapBytes is used when node capacity cannot be determined.
	defaultOOMCapBytes = int64(8 * 1024 * 1024 * 1024) // 8Gi
	// oomCapFraction limits OOM doubling to this fraction of total node memory.
	oomCapFraction = 0.5
	// oomActionableWindow bounds how recent an OOM must be to act on. A
	// LastTerminationState lingers on a container until its pod is replaced, so
	// after a controller restart (which empties the in-memory handled-OOM map)
	// an old OOM would otherwise look new and re-double memory. Only a fresh
	// OOM within this window drives a memory bump.
	oomActionableWindow = 30 * time.Minute

	// Alert cooldowns keep a persistent failure from re-firing every tick.
	nodeAlertCooldown    = 30 * time.Minute
	rolloutAlertCooldown = 30 * time.Minute
	jobAlertCooldown     = 6 * time.Hour

	// alertStateTTL bounds how long a cooldown/dedup entry is kept. Anything
	// older is for a resource that has almost certainly been deleted or long
	// recovered, so pruning it caps map growth; a live problem re-adds its
	// key on the next tick.
	alertStateTTL = 24 * time.Hour
)

// ResourceController runs a background loop that automatically adjusts
// resource requests and limits for kipper-managed workloads.
type ResourceController struct {
	client           kubernetes.Interface
	crClient         crclient.Client
	history          map[workloadKey][]usageObservation
	hpaReplicas      map[string]int32        // namespace/name → last seen replica count
	changeTimestamps map[string][]time.Time  // namespace/name → recent resource or HPA change times
	imagePullAlerted map[string]time.Time    // namespace/pod/container → last ImagePullBackOff alert time
	crashLoopAlerted map[string]time.Time    // namespace/pod/container → last CrashLoopBackOff alert time
	oomHandledAt     map[string]time.Time    // namespace/app/container → finish time of the last OOM acted on
	rolloutAlerted   map[string]time.Time    // namespace/name → last stuck-rollout alert time
	jobFailAlerted   map[string]time.Time    // job UID → last failed-job alert time
	nodeReadyAlerted map[string]time.Time    // node name → last NotReady alert time
	quotaBlockAlert  map[string]time.Time    // namespace/app → last quota-blocked-increase alert time
	updateFailAlert  map[string]time.Time    // namespace/app → last failed-workload-update alert time
	cpuPinned        map[string]pinnedWindow // namespace/app → CPU-saturation observation window
	mu               sync.Mutex
	oomCapBytes      int64
	nodePressureAt   time.Time // last node-pressure alert time
	nodePressureSev  string    // severity of the last node-pressure alert
}

// NewResourceController creates a controller that manages resources automatically.
func NewResourceController(client kubernetes.Interface, crClient crclient.Client) *ResourceController {
	return &ResourceController{
		hpaReplicas:      make(map[string]int32),
		changeTimestamps: make(map[string][]time.Time),
		imagePullAlerted: make(map[string]time.Time),
		crashLoopAlerted: make(map[string]time.Time),
		oomHandledAt:     make(map[string]time.Time),
		rolloutAlerted:   make(map[string]time.Time),
		jobFailAlerted:   make(map[string]time.Time),
		nodeReadyAlerted: make(map[string]time.Time),
		quotaBlockAlert:  make(map[string]time.Time),
		updateFailAlert:  make(map[string]time.Time),
		cpuPinned:        make(map[string]pinnedWindow),
		client:           client,
		crClient:         crClient,
		history:          make(map[workloadKey][]usageObservation),
	}
}

// Run starts the controller loop. It blocks until the context is cancelled.
func (rc *ResourceController) Run(ctx context.Context) {
	log.Printf("resource controller started (interval: %s)", checkInterval)
	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Printf("resource controller stopped")
			return
		case <-ticker.C:
			rc.tick(ctx)
		}
	}
}

func (rc *ResourceController) tick(ctx context.Context) {
	// Drop cooldown/dedup entries for resources that are long gone so the
	// state maps stay bounded on a long-running controller.
	rc.pruneAlertState()

	// List nodes once per tick and reuse the result for every node-based
	// check below instead of listing them again in each.
	nodes := rc.listNodes(ctx)

	// Failure alerts run regardless of auto/expert mode — an operator needs
	// to know about pod, node, job, and rollout problems either way. Each check
	// stages its cooldown marks rather than setting them; the marks commit only
	// once the alert write lands, so a failed write re-fires next tick instead
	// of being suppressed for the cooldown window.
	alertEntries, alertMarks := rc.checkPodProblems(ctx)
	nodeEntries, nodeMarks := rc.checkNodeReady(nodes)
	alertEntries = append(alertEntries, nodeEntries...)
	alertMarks = append(alertMarks, nodeMarks...)
	jobEntries, jobMarks := rc.checkFailedJobs(ctx)
	alertEntries = append(alertEntries, jobEntries...)
	alertMarks = append(alertMarks, jobMarks...)

	deployments, deployErr := rc.listManagedDeployments(ctx)
	if deployErr != nil {
		log.Printf("resource controller: failed to list deployments: %v", deployErr)
	} else {
		rolloutEntries, rolloutMarks := rc.checkStuckRollouts(deployments)
		alertEntries = append(alertEntries, rolloutEntries...)
		alertMarks = append(alertMarks, rolloutMarks...)
	}

	if len(alertEntries) > 0 {
		// Bound this tick's batch to the same per-write cap the alert store
		// applies, dropping the oldest overflow so entries and their staged
		// marks stay aligned. Committing a mark for an alert the store never
		// wrote would suppress it for its cooldown though the operator never
		// saw it; the deferred alerts stay eligible on the next tick. Entries
		// and marks are appended together per problem, so they share an order.
		if len(alertEntries) > handlers.MaxAlertsPerWrite {
			drop := len(alertEntries) - handlers.MaxAlertsPerWrite
			log.Printf("resource controller: %d failure alerts this tick exceeds the per-write cap, deferring the %d oldest", len(alertEntries), drop)
			alertEntries = alertEntries[drop:]
			alertMarks = alertMarks[drop:]
		}
		if err := rc.createAlerts(ctx, alertEntries); err != nil {
			log.Printf("resource controller: failed to persist failure alerts, will retry next tick: %v", err)
		} else {
			rc.commitMarks(alertMarks)
		}
	}

	mode := rc.getMode(ctx)
	if mode != modeAuto || deployErr != nil {
		return
	}

	rc.refreshOOMCap(nodes)

	statefulsets, err := rc.listManagedStatefulSets(ctx)
	if err != nil {
		log.Printf("resource controller: failed to list statefulsets: %v", err)
		return
	}

	podMetrics, err := rc.fetchPodMetrics(ctx)
	if err != nil {
		log.Printf("resource controller: failed to fetch metrics: %v", err)
		return
	}

	// Resolve which apps are scaled out once, rather than per deployment.
	scaledOut := rc.scaledOutApps(ctx)

	var logEntries []ResourceLogEntry

	for i := range deployments {
		entries := rc.processDeployment(ctx, &deployments[i], podMetrics, scaledOut)
		logEntries = append(logEntries, entries...)
	}

	for i := range statefulsets {
		entries := rc.processStatefulSet(ctx, &statefulsets[i], podMetrics)
		logEntries = append(logEntries, entries...)
	}

	// Check HPA scaling events
	hpaEntries := rc.checkHPAScaling(ctx)
	logEntries = append(logEntries, hpaEntries...)

	// Check node pressure and generate alerts
	pressureEntries := rc.checkNodePressure(ctx, podMetrics, nodes)
	logEntries = append(logEntries, pressureEntries...)

	if len(logEntries) > 0 {
		rc.appendLogEntries(ctx, logEntries)
		if err := rc.createAlerts(ctx, logEntries); err != nil {
			log.Printf("resource controller: failed to persist tuning alerts: %v", err)
		}
	}

	rc.detectAndRecommend(ctx)
}

// tuningPaused reports whether the workload carries an unexpired
// tuning-pause annotation. Bulk operations (kip service import/export) set
// it so a resource update cannot roll the pod mid-operation; the deadline
// bounds the pause when the client never cleans up. A malformed timestamp
// counts as not paused so a bad write cannot switch tuning off silently.
func tuningPaused(annotations map[string]string) bool {
	raw, ok := annotations[labels.AnnoTuningPausedUntil]
	if !ok {
		return false
	}
	until, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return false
	}
	return time.Now().Before(until)
}

// listNodes lists all nodes once so a single tick can share the result across
// every node-based check instead of listing them repeatedly.
func (rc *ResourceController) listNodes(ctx context.Context) []corev1.Node {
	list, err := rc.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Printf("resource controller: failed to list nodes: %v", err)
		return nil
	}
	return list.Items
}

func (rc *ResourceController) refreshOOMCap(nodes []corev1.Node) {
	if len(nodes) == 0 {
		if rc.oomCapBytes == 0 {
			rc.oomCapBytes = defaultOOMCapBytes
		}
		return
	}

	// Sum allocatable memory across all nodes
	var totalBytes int64
	for _, node := range nodes {
		if mem, ok := node.Status.Allocatable[corev1.ResourceMemory]; ok {
			totalBytes += mem.Value()
		}
	}

	cap := int64(float64(totalBytes) * oomCapFraction)
	if cap < defaultOOMCapBytes {
		cap = defaultOOMCapBytes
	}
	rc.oomCapBytes = cap
}

func (rc *ResourceController) getMode(ctx context.Context) string {
	cm, err := rc.client.CoreV1().ConfigMaps(modeConfigMapNamespace).Get(ctx, modeConfigMapName, metav1.GetOptions{})
	if err != nil {
		return modeAuto
	}
	mode, ok := cm.Data["mode"]
	if !ok || (mode != modeAuto && mode != modeExpert) {
		return modeAuto
	}
	return mode
}

func (rc *ResourceController) listManagedDeployments(ctx context.Context) ([]appsv1.Deployment, error) {
	selector := labels.KipperManagedSelector
	list, err := rc.client.AppsV1().Deployments("").List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

func (rc *ResourceController) listManagedStatefulSets(ctx context.Context) ([]appsv1.StatefulSet, error) {
	selector := labels.KipperManagedSelector
	list, err := rc.client.AppsV1().StatefulSets("").List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

// startupGracePeriod is how long to ignore pod metrics after creation.
// JVM apps can spike to 100% CPU during class loading and JIT compilation
// for several minutes — this prevents the controller from reacting to
// startup transients.
const startupGracePeriod = 5 * time.Minute

// podMetricsEntry holds parsed metrics for a single pod.
type podMetricsEntry struct {
	Namespace    string
	PodName      string
	CPUMillis    int64
	MemoryBytes  int64
	Labels       map[string]string
	OOMKilled    bool
	OOMAt        time.Time
	StuckMinutes float64
	Age          time.Duration
	// Synthetic marks a zero-usage entry built only to carry a crash-looping
	// pod's OOM state (the pod reports no metrics). It drives the OOM bump but
	// must never feed the usage averaging, or its zeros would scale a still
	// crash-looping workload down.
	Synthetic bool
}

func (rc *ResourceController) fetchPodMetrics(ctx context.Context) (map[string][]podMetricsEntry, error) {
	result := make(map[string][]podMetricsEntry)

	// Fetch pod metrics from metrics-server via the raw API
	data, err := rc.client.Discovery().RESTClient().Get().
		AbsPath("/apis/metrics.k8s.io/v1beta1/pods").
		DoRaw(ctx)
	if err != nil {
		return nil, fmt.Errorf("querying metrics-server: %w", err)
	}

	var metricsResponse struct {
		Items []struct {
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
			Containers []struct {
				Name  string `json:"name"`
				Usage struct {
					CPU    string `json:"cpu"`
					Memory string `json:"memory"`
				} `json:"usage"`
			} `json:"containers"`
		} `json:"items"`
	}

	if err := json.Unmarshal(data, &metricsResponse); err != nil {
		return nil, fmt.Errorf("parsing metrics response: %w", err)
	}

	// Fetch all pods to get labels, OOMKill status, and stuck status
	pods, err := rc.client.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		LabelSelector: labels.KipperManagedSelector,
	})
	if err != nil {
		return nil, fmt.Errorf("listing pods: %w", err)
	}

	podInfo := make(map[string]*corev1.Pod)
	for i := range pods.Items {
		p := &pods.Items[i]
		podInfo[p.Namespace+"/"+p.Name] = p
	}

	covered := make(map[string]bool, len(metricsResponse.Items))
	for _, item := range metricsResponse.Items {
		pod, ok := podInfo[item.Metadata.Namespace+"/"+item.Metadata.Name]
		if !ok {
			continue
		}
		covered[pod.Namespace+"/"+pod.Name] = true

		var totalCPU, totalMem int64
		for _, c := range item.Containers {
			cpuQty := resource.MustParse(c.Usage.CPU)
			memQty := resource.MustParse(c.Usage.Memory)
			totalCPU += cpuQty.MilliValue()
			totalMem += memQty.Value()
		}

		oomAt, oomKilled := latestOOMAt(pod)
		stuckMinutes := stuckDuration(pod)

		appName := pod.Labels["app"]
		if appName == "" {
			continue
		}

		podAge := time.Since(pod.CreationTimestamp.Time)

		key := item.Metadata.Namespace + "/" + appName
		result[key] = append(result[key], podMetricsEntry{
			Namespace:    item.Metadata.Namespace,
			PodName:      item.Metadata.Name,
			CPUMillis:    totalCPU,
			MemoryBytes:  totalMem,
			Labels:       pod.Labels,
			OOMKilled:    oomKilled,
			OOMAt:        oomAt,
			StuckMinutes: stuckMinutes,
			Age:          podAge,
		})
	}

	for key, entries := range oomEntriesForUncovered(podInfo, covered) {
		result[key] = append(result[key], entries...)
	}

	return result, nil
}

// oomEntriesForUncovered builds zero-usage entries carrying the OOM state of
// managed pods that reported no metrics (crash-looping pods), keyed by
// "namespace/app". Crash-looping pods are absent from metrics-server, so
// surfacing their OOM here lets a memory bump still fire without a per-app pod
// list. Only OOM-carrying pods are added, and each is flagged Synthetic so
// evaluateAndAdjust drops it before the CPU/memory averaging — its zero usage
// must never skew an adjustment.
func oomEntriesForUncovered(podInfo map[string]*corev1.Pod, covered map[string]bool) map[string][]podMetricsEntry {
	out := make(map[string][]podMetricsEntry)
	for name, pod := range podInfo {
		if covered[name] {
			continue
		}
		oomAt, oomKilled := latestOOMAt(pod)
		if !oomKilled {
			continue
		}
		appName := pod.Labels["app"]
		if appName == "" {
			continue
		}
		key := pod.Namespace + "/" + appName
		out[key] = append(out[key], podMetricsEntry{
			Namespace: pod.Namespace,
			PodName:   pod.Name,
			Labels:    pod.Labels,
			OOMKilled: oomKilled,
			OOMAt:     oomAt,
			Age:       time.Since(pod.CreationTimestamp.Time),
			Synthetic: true,
		})
	}
	return out
}

// withoutSynthetic returns the entries that carry real metrics, dropping the
// zero-usage synthetic entries built only to convey a crash-looping pod's OOM
// state so they never enter the usage averaging.
func withoutSynthetic(entries []podMetricsEntry) []podMetricsEntry {
	var out []podMetricsEntry
	for _, e := range entries {
		if !e.Synthetic {
			out = append(out, e)
		}
	}
	return out
}

// latestOOMAt returns the finish time of the pod's most recent OOMKilled
// termination, across current and previous container states. The bool is
// false when no OOM termination is recorded. Tracking the time lets the
// controller act once per OOM event: the kubelet's LastTerminationState
// lingers after the pod restarts, so a plain bool would re-double memory
// and re-alert on every tick.
func latestOOMAt(pod *corev1.Pod) (time.Time, bool) {
	var latest time.Time
	found := false
	for _, cs := range pod.Status.ContainerStatuses {
		for _, term := range []*corev1.ContainerStateTerminated{cs.State.Terminated, cs.LastTerminationState.Terminated} {
			if term == nil || term.Reason != "OOMKilled" {
				continue
			}
			found = true
			if term.FinishedAt.After(latest) {
				latest = term.FinishedAt.Time
			}
		}
	}
	return latest, found
}

func isOOMKilled(pod *corev1.Pod) bool {
	_, ok := latestOOMAt(pod)
	return ok
}

func stuckDuration(pod *corev1.Pod) float64 {
	if pod.Status.Phase != corev1.PodPending {
		return 0
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Waiting != nil && cs.State.Waiting.Reason == "ContainerCreating" {
			if !pod.CreationTimestamp.IsZero() {
				return time.Since(pod.CreationTimestamp.Time).Minutes()
			}
		}
	}
	return 0
}

func (rc *ResourceController) processDeployment(ctx context.Context, deploy *appsv1.Deployment, metrics map[string][]podMetricsEntry, scaledOut map[string]bool) []ResourceLogEntry {
	if len(deploy.Spec.Template.Spec.Containers) == 0 || tuningPaused(deploy.Annotations) {
		return nil
	}

	appName := deploy.Labels["app"]
	if appName == "" {
		appName = deploy.Name
	}

	key := deploy.Namespace + "/" + appName
	podEntries := metrics[key]

	container := &deploy.Spec.Template.Spec.Containers[0]
	var replicas int32 = 1
	if deploy.Spec.Replicas != nil {
		replicas = *deploy.Spec.Replicas
	}
	blockDecrease := scaledOut[deploy.Namespace+"/"+appName]
	before := container.Resources.DeepCopy()
	entries, oomMark := rc.evaluateAndAdjust(deploy.Namespace, appName, container, podEntries, deploy.Labels, replicas, blockDecrease, 0)
	entries, quotaBlocked := rc.applyQuotaCeiling(ctx, deploy.Namespace, appName, container, *before,
		replicas, quotapkg.DeploymentSurgePods(deploy, replicas), &deploy.Spec.Template.Spec, entries)

	if len(entries) > 0 && !quotaBlocked {
		if _, err := rc.client.AppsV1().Deployments(deploy.Namespace).Update(ctx, deploy, metav1.UpdateOptions{}); err != nil {
			log.Printf("resource controller: failed to update deployment %s/%s: %v", deploy.Namespace, deploy.Name, err)
			return rc.workloadUpdateFailed(deploy.Namespace, appName, err)
		}
		rc.commitOOMMark(oomMark)
		log.Printf("resource controller: updated deployment %s/%s", deploy.Namespace, deploy.Name)

		// Sync the new resources back to the App CR so the app reconciler
		// doesn't overwrite them on the next reconcile.
		rc.syncAppCRResources(ctx, deploy.Namespace, appName, container)

		rc.recordChange(deploy.Namespace, appName)
	}

	// Handle stuck pods
	for _, pe := range podEntries {
		if pe.StuckMinutes > 5 {
			log.Printf("resource controller: deleting stuck pod %s/%s (stuck %.0f minutes)", pe.Namespace, pe.PodName, pe.StuckMinutes)
			_ = rc.client.CoreV1().Pods(pe.Namespace).Delete(ctx, pe.PodName, metav1.DeleteOptions{})
			entries = append(entries, ResourceLogEntry{
				Time:      time.Now().UTC().Format(time.RFC3339),
				App:       appName,
				Namespace: deploy.Namespace,
				Action:    "deleted stuck pod",
				From:      pe.PodName,
				To:        "recreating",
				Reason:    fmt.Sprintf("stuck in ContainerCreating for %.0f minutes", pe.StuckMinutes),
			})
		}
	}

	return entries
}

func (rc *ResourceController) processStatefulSet(ctx context.Context, sts *appsv1.StatefulSet, metrics map[string][]podMetricsEntry) []ResourceLogEntry {
	if len(sts.Spec.Template.Spec.Containers) == 0 || tuningPaused(sts.Annotations) {
		return nil
	}

	appName := sts.Labels["app"]
	if appName == "" {
		appName = sts.Name
	}

	key := sts.Namespace + "/" + appName
	podEntries := metrics[key]

	container := &sts.Spec.Template.Spec.Containers[0]
	var replicas int32 = 1
	if sts.Spec.Replicas != nil {
		replicas = *sts.Spec.Replicas
	}
	// StatefulSets replace pods one at a time with no surge, so the
	// steady-state projection is also the admission peak.
	before := container.Resources.DeepCopy()
	entries, oomMark := rc.evaluateAndAdjust(sts.Namespace, appName, container, podEntries, sts.Labels, replicas, false, statefulSetSaturationWindow)
	entries, quotaBlocked := rc.applyQuotaCeiling(ctx, sts.Namespace, appName, container, *before, replicas, 0, nil, entries)

	if len(entries) > 0 && !quotaBlocked {
		if _, err := rc.client.AppsV1().StatefulSets(sts.Namespace).Update(ctx, sts, metav1.UpdateOptions{}); err != nil {
			log.Printf("resource controller: failed to update statefulset %s/%s: %v", sts.Namespace, sts.Name, err)
			return rc.workloadUpdateFailed(sts.Namespace, appName, err)
		}
		rc.commitOOMMark(oomMark)
		log.Printf("resource controller: updated statefulset %s/%s", sts.Namespace, sts.Name)
	}

	return entries
}

// quotaBlockCooldown keeps a persistently blocked increase from re-alerting
// every tick while the workload stays over its ceiling.
const quotaBlockCooldown = 30 * time.Minute

// updateFailCooldown throttles the alert raised when a workload's resource
// update keeps failing, so a persistent failure surfaces on a heartbeat rather
// than every tick.
const updateFailCooldown = 30 * time.Minute

// workloadUpdateFailed turns a failed resource-update apply into a throttled
// critical alert instead of a silent dropped tick. The controller retries the
// change on the next tick (the OOM mark is only committed on success), but
// without this the operator would never learn the remediation isn't landing.
func (rc *ResourceController) workloadUpdateFailed(namespace, appName string, err error) []ResourceLogEntry {
	key := namespace + "/" + appName
	now := time.Now()
	rc.mu.Lock()
	suppressed := now.Sub(rc.updateFailAlert[key]) < updateFailCooldown
	if !suppressed {
		rc.updateFailAlert[key] = now
	}
	rc.mu.Unlock()
	if suppressed {
		return nil
	}
	return []ResourceLogEntry{{
		Time:      now.UTC().Format(time.RFC3339),
		App:       appName,
		Namespace: namespace,
		Action:    "resource update failed",
		Severity:  "critical",
		Reason:    fmt.Sprintf("could not apply resource changes: %v", err),
	}}
}

// applyQuotaCeiling reverts an automatic resource increase that would push
// the namespace over its project ResourceQuota. Committing an increase the
// quota cannot admit leaves the workload stuck: the Deployment update is
// accepted but every replacement pod is rejected at admission. Instead the
// increase is dropped and a critical alert points at the project quota. The
// entry is deduplicated per workload with a cooldown so a persistently
// blocked workload alerts on a heartbeat, never per tick.
func (rc *ResourceController) applyQuotaCeiling(ctx context.Context, namespace, appName string, container *corev1.Container, before corev1.ResourceRequirements, replicas, surgePods int32, podSpec *corev1.PodSpec, entries []ResourceLogEntry) (out []ResourceLogEntry, blocked bool) {
	if len(entries) == 0 || !resourcesIncreased(before, container.Resources) {
		return entries, false
	}

	quota, err := rc.client.CoreV1().ResourceQuotas(namespace).Get(ctx, kipperv1.ProjectQuotaName, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		// No quota on the namespace: nothing to enforce here.
		return entries, false
	}
	if err != nil {
		// The quota exists but is unreadable (transient API error). Don't
		// commit an increase we can't prove fits, or admission will wedge the
		// rollout. Revert it and let a later reconcile retry once the quota
		// reads again.
		container.Resources = *before.DeepCopy()
		return nil, true
	}

	dimension, projected, hard, fits := quotapkg.Fits(quota, before, container.Resources, replicas, surgePods, podSpec)
	if fits {
		return entries, false
	}

	// Deep copy so later mutations of the container cannot alias the
	// caller's snapshot.
	container.Resources = *before.DeepCopy()

	key := namespace + "/" + appName
	now := time.Now()
	rc.mu.Lock()
	suppressed := now.Sub(rc.quotaBlockAlert[key]) < quotaBlockCooldown
	if !suppressed {
		rc.quotaBlockAlert[key] = now
	}
	rc.mu.Unlock()
	if suppressed {
		return nil, true
	}

	return []ResourceLogEntry{{
		Time:      now.UTC().Format(time.RFC3339),
		App:       appName,
		Namespace: namespace,
		Action:    "resource increase blocked by project quota",
		From:      hard,
		To:        projected,
		Severity:  "critical",
		Reason: fmt.Sprintf("raising resources would need %s of %s but the namespace quota caps at %s. Raise the project tier or environment quota, or reduce other workloads",
			projected, dimension, hard),
	}}, true
}

// resourcesIncreased reports whether any request or limit grew.
func resourcesIncreased(before, after corev1.ResourceRequirements) bool {
	grew := func(old, updated corev1.ResourceList) bool {
		for name, v := range updated {
			prev, ok := old[name]
			if !ok || v.Cmp(prev) > 0 {
				return true
			}
		}
		return false
	}
	return grew(before.Requests, after.Requests) || grew(before.Limits, after.Limits)
}

// commitOOMMark records that the controller successfully acted on an OOM
// event. Called only after the workload update lands so a failed update
// leaves the OOM eligible for retry on the next tick.
func (rc *ResourceController) commitOOMMark(mark *pendingOOMMark) {
	if mark == nil {
		return
	}
	rc.mu.Lock()
	rc.oomHandledAt[mark.key] = mark.at
	rc.mu.Unlock()
}

// pendingMark is an alert cooldown mark staged while building a tick's alerts.
// The tick commits it only after the alert write succeeds, so a failed write
// leaves the alert eligible to fire on the next tick instead of being silently
// suppressed for the cooldown window.
type pendingMark struct {
	dst map[string]time.Time
	key string
	at  time.Time
}

// commitMarks records staged cooldown marks under the state lock.
func (rc *ResourceController) commitMarks(marks []pendingMark) {
	if len(marks) == 0 {
		return
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	for _, m := range marks {
		m.dst[m.key] = m.at
	}
}

// pruneAlertState drops cooldown/dedup entries older than alertStateTTL,
// keeping the churny per-pod and per-job maps bounded over a long-running
// controller. Each stored time is the last alert time, so an entry older than
// the TTL is for a pod/job/deployment/node that is long gone; a live problem
// re-adds its key on the next tick.
//
// oomHandledAt is deliberately excluded: its value is the OOM event's finish
// time, not an activity time, so a TTL sweep would resurrect an already
// handled OOM whenever a pod keeps a >TTL-old OOM in its lingering
// LastTerminationState. Its keys are per-workload/container, a small stable
// set, so it does not need TTL pruning.
func (rc *ResourceController) pruneAlertState() {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	cutoff := time.Now().Add(-alertStateTTL)
	for _, m := range []map[string]time.Time{
		rc.imagePullAlerted, rc.crashLoopAlerted,
		rc.rolloutAlerted, rc.jobFailAlerted, rc.nodeReadyAlerted,
		rc.quotaBlockAlert, rc.updateFailAlert,
	} {
		for k, t := range m {
			if t.Before(cutoff) {
				delete(m, k)
			}
		}
	}

	// A saturation window whose last hot observation is older than the TTL
	// belongs to a deleted workload — a live one either clears its key on a
	// cool tick or keeps refreshing lastHot.
	for k, w := range rc.cpuPinned {
		if w.lastHot.Before(cutoff) {
			delete(rc.cpuPinned, k)
		}
	}
}

// syncAppCRResources updates the App CR's resource fields to match what the
// resource controller set on the Deployment. This prevents the app reconciler
// from overwriting the adjusted resources on its next reconcile.
func (rc *ResourceController) syncAppCRResources(ctx context.Context, namespace, appName string, container *corev1.Container) {
	if rc.crClient == nil {
		return
	}

	var app kipperv1.App
	if err := rc.crClient.Get(ctx, crclient.ObjectKey{Namespace: namespace, Name: appName}, &app); err != nil {
		return
	}

	cpuReq := stringFromResource(container.Resources.Requests, corev1.ResourceCPU)
	cpuLim := stringFromResource(container.Resources.Limits, corev1.ResourceCPU)
	memReq := stringFromResource(container.Resources.Requests, corev1.ResourceMemory)
	memLim := stringFromResource(container.Resources.Limits, corev1.ResourceMemory)

	if app.Spec.Resources.CPURequest == cpuReq && app.Spec.Resources.CPULimit == cpuLim &&
		app.Spec.Resources.MemoryRequest == memReq && app.Spec.Resources.MemoryLimit == memLim {
		return
	}

	app.Spec.Resources.CPURequest = cpuReq
	app.Spec.Resources.CPULimit = cpuLim
	app.Spec.Resources.MemoryRequest = memReq
	app.Spec.Resources.MemoryLimit = memLim
	if app.Spec.Resources.Profile == "" {
		app.Spec.Resources.Profile = "custom"
	}

	if err := rc.crClient.Update(ctx, &app); err != nil {
		log.Printf("resource controller: failed to sync App CR resources for %s/%s: %v", namespace, appName, err)
	}
}

func stringFromResource(list corev1.ResourceList, name corev1.ResourceName) string {
	if list == nil {
		return ""
	}
	if v, ok := list[name]; ok {
		return v.String()
	}
	return ""
}

// scaledOutApps returns the set of "namespace/name" apps whose autoscaling is
// enabled and whose HPA has scaled above minReplicas — pods under genuine load
// whose resources should not be decreased. Computing it once from a single App
// list and a single HPA list avoids a per-deployment App+HPA get on every tick.
func (rc *ResourceController) scaledOutApps(ctx context.Context) map[string]bool {
	out := make(map[string]bool)
	if rc.crClient == nil {
		return out
	}

	var apps kipperv1.AppList
	if err := rc.crClient.List(ctx, &apps); err != nil {
		return out
	}
	enabled := make(map[string]bool)
	for i := range apps.Items {
		a := &apps.Items[i]
		if a.Spec.Autoscale != nil && a.Spec.Autoscale.Enabled {
			enabled[a.Namespace+"/"+a.Name] = true
		}
	}
	if len(enabled) == 0 {
		return out
	}

	hpas, err := rc.client.AutoscalingV2().HorizontalPodAutoscalers("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return out
	}
	for i := range hpas.Items {
		hpa := &hpas.Items[i]
		key := hpa.Namespace + "/" + hpa.Name
		if enabled[key] && hpa.Spec.MinReplicas != nil && hpa.Status.CurrentReplicas > *hpa.Spec.MinReplicas {
			out[key] = true
		}
	}
	return out
}

const (
	thrashingWindow    = 10 * time.Minute
	thrashingThreshold = 3
	stabilityWindow    = 30 * time.Minute
	recommendationType = "ResourceRecommendation"
	dismissAnnotation  = "kipper.run/recommendation-dismissed-at"
)

// recordChange adds a timestamp to the sliding window for a workload.
func (rc *ResourceController) recordChange(namespace, appName string) {
	key := namespace + "/" + appName
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.changeTimestamps[key] = append(rc.changeTimestamps[key], time.Now())
}

// detectAndRecommend checks all tracked workloads for thrashing patterns
// and sets or clears recommendation conditions on their App CRs.
func (rc *ResourceController) detectAndRecommend(ctx context.Context) {
	if rc.crClient == nil {
		return
	}

	rc.mu.Lock()
	// Build a snapshot so we don't hold the lock during API calls
	snapshot := make(map[string][]time.Time, len(rc.changeTimestamps))
	now := time.Now()
	for key, timestamps := range rc.changeTimestamps {
		var recent []time.Time
		for _, t := range timestamps {
			if now.Sub(t) < stabilityWindow {
				recent = append(recent, t)
			}
		}
		rc.changeTimestamps[key] = recent
		snapshot[key] = recent
	}
	rc.mu.Unlock()

	for key, timestamps := range snapshot {
		parts := strings.SplitN(key, "/", 2)
		if len(parts) != 2 {
			continue
		}
		namespace, appName := parts[0], parts[1]

		// Count changes within the thrashing window
		recentCount := 0
		for _, t := range timestamps {
			if now.Sub(t) < thrashingWindow {
				recentCount++
			}
		}

		var app kipperv1.App
		if err := rc.crClient.Get(ctx, crclient.ObjectKey{Namespace: namespace, Name: appName}, &app); err != nil {
			continue
		}

		if recentCount >= thrashingThreshold {
			rc.setRecommendation(ctx, &app)
		} else if recentCount == 0 {
			rc.clearRecommendation(ctx, &app)
		}
	}
}

func (rc *ResourceController) setRecommendation(ctx context.Context, app *kipperv1.App) {
	// Check if dismissed recently
	if dismissed, ok := app.Annotations[dismissAnnotation]; ok {
		if t, err := time.Parse(time.RFC3339, dismissed); err == nil {
			if time.Since(t) < stabilityWindow {
				return
			}
		}
	}

	profile := rc.recommendProfile(app)
	message := fmt.Sprintf(
		"This app has been frequently adjusting resources and scaling. Consider switching to the %s profile to stabilise.",
		profile,
	)

	condition := metav1.Condition{
		Type:               recommendationType,
		Status:             metav1.ConditionTrue,
		Reason:             "Recommend" + strings.ToUpper(profile[:1]) + profile[1:],
		Message:            message,
		LastTransitionTime: metav1.Now(),
	}

	// Only update if the condition doesn't already exist
	for _, c := range app.Status.Conditions {
		if c.Type == recommendationType && c.Status == metav1.ConditionTrue {
			return
		}
	}

	meta.SetStatusCondition(&app.Status.Conditions, condition)
	if err := rc.crClient.Status().Update(ctx, app); err != nil {
		log.Printf("resource controller: failed to set recommendation for %s/%s: %v", app.Namespace, app.Name, err)
	}
}

func (rc *ResourceController) clearRecommendation(ctx context.Context, app *kipperv1.App) {
	found := false
	for _, c := range app.Status.Conditions {
		if c.Type == recommendationType {
			found = true
			break
		}
	}
	if !found {
		return
	}

	meta.RemoveStatusCondition(&app.Status.Conditions, recommendationType)
	if err := rc.crClient.Status().Update(ctx, app); err != nil {
		log.Printf("resource controller: failed to clear recommendation for %s/%s: %v", app.Namespace, app.Name, err)
	}
}

// recommendProfile suggests a resource profile based on the app's image and usage pattern.
func (rc *ResourceController) recommendProfile(app *kipperv1.App) string {
	image := strings.ToLower(app.Spec.Image)
	jvmIndicators := []string{"java", "jdk", "jre", "spring", "quarkus", "gradle", "maven", "tomcat", "wildfly", "openjdk"}
	for _, indicator := range jvmIndicators {
		if strings.Contains(image, indicator) {
			return "jvm"
		}
	}

	currentProfile := app.Spec.Resources.Profile
	switch currentProfile {
	case "lightweight", "standard", "":
		return "compute-heavy"
	default:
		return currentProfile
	}
}

// checkHPAScaling lists all HPAs managed by Kipper and generates alerts
// when the replica count changes.
func (rc *ResourceController) checkHPAScaling(ctx context.Context) []ResourceLogEntry {
	hpaList, err := rc.client.AutoscalingV2().HorizontalPodAutoscalers("").List(ctx, metav1.ListOptions{
		LabelSelector: labels.KipperManagedSelector,
	})
	if err != nil {
		return nil
	}

	var entries []ResourceLogEntry
	now := time.Now().UTC().Format(time.RFC3339)

	rc.mu.Lock()
	defer rc.mu.Unlock()

	for _, hpa := range hpaList.Items {
		key := hpa.Namespace + "/" + hpa.Name
		current := hpa.Status.CurrentReplicas
		prev, seen := rc.hpaReplicas[key]
		rc.hpaReplicas[key] = current

		if !seen || current == prev {
			continue
		}

		var action string
		if current > prev {
			action = fmt.Sprintf("scaled out (%d → %d pods)", prev, current)
		} else {
			action = fmt.Sprintf("scaled in (%d → %d pods)", prev, current)
		}

		entries = append(entries, ResourceLogEntry{
			Time:      now,
			App:       hpa.Name,
			Namespace: hpa.Namespace,
			Action:    action,
			From:      fmt.Sprintf("%d", prev),
			To:        fmt.Sprintf("%d", current),
			Reason:    "HPA autoscaling",
		})

		log.Printf("resource controller: HPA %s %s", key, action)
		rc.recordChange(hpa.Namespace, hpa.Name)
	}

	return entries
}

func (rc *ResourceController) evaluateAndAdjust(
	namespace, appName string,
	container *corev1.Container,
	podEntries []podMetricsEntry,
	objectLabels map[string]string,
	replicas int32,
	blockDecrease bool,
	sustainSaturationFor time.Duration,
) (entries []ResourceLogEntry, oomMark *pendingOOMMark) {
	now := time.Now().UTC().Format(time.RFC3339)

	// Apply defaults if no resource requests are set
	if container.Resources.Requests == nil && container.Resources.Limits == nil {
		defaults := profileDefaults(objectLabels[labels.ResourceProfile])
		container.Resources = corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(defaults.cpu),
				corev1.ResourceMemory: resource.MustParse(defaults.memory),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(defaults.cpu),
				corev1.ResourceMemory: resource.MustParse(defaults.memory),
			},
		}
		entries = append(entries, ResourceLogEntry{
			Time:      now,
			App:       appName,
			Namespace: namespace,
			Action:    "applied default resources",
			From:      "none",
			To:        fmt.Sprintf("%s CPU, %s memory", defaults.cpu, defaults.memory),
			Reason:    fmt.Sprintf("no resources set, using %s profile", defaults.name),
		})
		return
	}

	// Find the most recent OOMKilled event for this workload. Tracking the
	// finish time (not just a bool) lets us act once per OOM: the kubelet's
	// LastTerminationState lingers after the pod restarts, so a bool would
	// re-double memory and re-alert on every 60s tick.
	// fetchPodMetrics already folds crash-looping (no-metrics) pods into
	// podEntries with their OOM time, so the most recent OOM is the newest
	// OOMAt across the entries.
	var oomAt time.Time
	for _, pe := range podEntries {
		if pe.OOMAt.After(oomAt) {
			oomAt = pe.OOMAt
		}
	}

	// Act on an OOM only if it is both new (not already handled this lifetime)
	// and recent. An already-handled OOM whose LastTerminationState still
	// lingers, or one old enough to have survived a controller restart, must
	// not re-double memory or re-alert — fall through to usage-based tuning
	// instead of returning, so a scarred workload still gets tuned.
	oomKey := namespace + "/" + appName + "/" + container.Name
	rc.mu.Lock()
	isNewOOM := !oomAt.IsZero() && oomAt.After(rc.oomHandledAt[oomKey])
	rc.mu.Unlock()
	if isNewOOM && time.Since(oomAt) < oomActionableWindow {
		// Stage the mark; the caller commits it once the workload update
		// succeeds so a failed update is retried instead of suppressed.
		oomMark = &pendingOOMMark{key: oomKey, at: oomAt}
		if container.Resources.Requests == nil {
			container.Resources.Requests = corev1.ResourceList{}
		}
		if container.Resources.Limits == nil {
			container.Resources.Limits = corev1.ResourceList{}
		}

		memReq := container.Resources.Requests[corev1.ResourceMemory]
		memLimit := container.Resources.Limits[corev1.ResourceMemory]
		// Use the larger of request or limit as the base
		base := memReq.Value()
		if memLimit.Value() > base {
			base = memLimit.Value()
		}
		if base == 0 {
			base = 128 * 1024 * 1024 // default 128Mi
		}

		cap := rc.oomCapBytes
		if cap == 0 {
			cap = defaultOOMCapBytes
		}

		newBytes := base * 2
		if newBytes > cap {
			newBytes = cap
		}

		oldVal := memReq.String()
		newQty := roundMemory(newBytes)

		// If already at cap, don't update — just alert
		if newQty.Value() <= base {
			entries = append(entries, ResourceLogEntry{
				Time:      now,
				App:       appName,
				Namespace: namespace,
				Action:    "OOMKilled at memory cap",
				From:      oldVal,
				To:        oldVal,
				Reason:    fmt.Sprintf("container was OOMKilled but already at cap (%s) — increase manually or check for memory leaks", newQty.String()),
			})
			return
		}

		container.Resources.Requests[corev1.ResourceMemory] = newQty
		container.Resources.Limits[corev1.ResourceMemory] = newQty
		entries = append(entries, ResourceLogEntry{
			Time:      now,
			App:       appName,
			Namespace: namespace,
			Action:    "doubled memory (OOMKilled)",
			From:      oldVal,
			To:        newQty.String(),
			Reason:    "container was OOMKilled — doubling memory",
		})
		return
	}

	// Past the OOM handling, drop synthetic OOM-only entries. They exist only to
	// drive the bump above; a non-actionable OOM (already handled or stale) falls
	// through to here, and their zero usage would otherwise pull a still
	// crash-looping workload down through the averaging below.
	podEntries = withoutSynthetic(podEntries)

	if len(podEntries) == 0 {
		return nil, nil
	}

	// Saturation override: if any pod is sitting at >= cpuSaturationPct of its
	// CPU limit, that's not transient warmup noise — the cgroup is the
	// bottleneck. Bump CPU now, before the 5-minute grace period and 3-tick
	// hysteresis would otherwise delay action by 8+ minutes (long enough that
	// HPA reacts first and never lets a single pod mature past grace).
	if entry := rc.maybeBumpForSaturation(namespace, appName, container, podEntries, sustainSaturationFor); entry != nil {
		entries = append(entries, *entry)
		return
	}

	// Filter out pods still in the startup grace period to avoid reacting
	// to transient CPU spikes during JVM class loading, JIT compilation, etc.
	var maturePods []podMetricsEntry
	for _, pe := range podEntries {
		if pe.Age >= startupGracePeriod {
			maturePods = append(maturePods, pe)
		}
	}
	if len(maturePods) == 0 {
		return nil, nil
	}

	// Calculate average usage across mature pods only
	var totalCPU, totalMem int64
	for _, pe := range maturePods {
		totalCPU += pe.CPUMillis
		totalMem += pe.MemoryBytes
	}
	avgCPU := totalCPU / int64(len(maturePods))
	avgMem := totalMem / int64(len(maturePods))

	wk := workloadKey{Namespace: namespace, Name: appName}
	rc.mu.Lock()
	rc.history[wk] = append(rc.history[wk], usageObservation{
		CPUMillis:   avgCPU,
		MemoryBytes: avgMem,
		// An actionable OOM returns earlier, so this path is always non-OOM.
		OOMKilled: false,
	})
	// Keep only last 3 observations
	if len(rc.history[wk]) > 3 {
		rc.history[wk] = rc.history[wk][len(rc.history[wk])-3:]
	}
	history := make([]usageObservation, len(rc.history[wk]))
	copy(history, rc.history[wk])
	rc.mu.Unlock()

	// Ensure resource maps are initialized
	if container.Resources.Requests == nil {
		container.Resources.Requests = corev1.ResourceList{}
	}
	if container.Resources.Limits == nil {
		container.Resources.Limits = corev1.ResourceList{}
	}

	// consistentHigh* checks every history entry, so once the window is full
	// an increase already needs three hot ticks. With fresh history (new
	// workload, controller restart) a single hot sample is enough — fine for
	// a stateless Deployment, but rolling a StatefulSet on one hot sample
	// kills whatever the database is doing (a restore, a bulk load), so those
	// need a second observation.
	minHotObservations := 1
	if sustainSaturationFor > 0 {
		minHotObservations = 2
	}

	// Memory adjustments (with hysteresis)
	if memReq, ok := container.Resources.Requests[corev1.ResourceMemory]; ok {
		reqBytes := memReq.Value()
		if reqBytes > 0 {
			usagePct := float64(avgMem) / float64(reqBytes) * 100

			if usagePct > 80 && len(history) >= minHotObservations && consistentHighMemory(history, reqBytes) {
				oldVal := memReq.String()
				newBytes := int64(math.Ceil(float64(reqBytes) * 1.5))
				newQty := roundMemory(newBytes)
				container.Resources.Requests[corev1.ResourceMemory] = newQty
				// Keep a burstable profile's higher limit; only raise the limit
				// when the request would otherwise exceed it. Collapsing the
				// limit to the request would strip a jvm workload of its
				// cold-start headroom.
				if curLim, ok := container.Resources.Limits[corev1.ResourceMemory]; !ok || newQty.Cmp(curLim) > 0 {
					container.Resources.Limits[corev1.ResourceMemory] = newQty
				}
				entries = append(entries, ResourceLogEntry{
					Time:      now,
					App:       appName,
					Namespace: namespace,
					Action:    "increased memory",
					From:      oldVal,
					To:        newQty.String(),
					Reason:    fmt.Sprintf("usage at %.0f%%", usagePct),
				})
			} else if usagePct < 20 && len(history) >= 3 && consistentLowMemory(history, reqBytes) && replicas >= 2 && !blockDecrease {
				profileMin := profileDefaults(objectLabels[labels.ResourceProfile])
				minQty := resource.MustParse(profileMin.memory)
				minBytes := minQty.Value()
				newBytes := int64(math.Ceil(float64(reqBytes) * 0.5))
				if newBytes < minBytes {
					newBytes = minBytes
				}
				newQty := roundMemory(newBytes)
				if newQty.Value() < reqBytes {
					oldVal := memReq.String()
					container.Resources.Requests[corev1.ResourceMemory] = newQty
					container.Resources.Limits[corev1.ResourceMemory] = newQty
					entries = append(entries, ResourceLogEntry{
						Time:      now,
						App:       appName,
						Namespace: namespace,
						Action:    "decreased memory",
						From:      oldVal,
						To:        newQty.String(),
						Reason:    fmt.Sprintf("usage at %.0f%% for 3+ checks", usagePct),
					})
				}
			}
		}
	}

	// CPU adjustments (with hysteresis)
	if cpuReq, ok := container.Resources.Requests[corev1.ResourceCPU]; ok {
		reqMillis := cpuReq.MilliValue()
		if reqMillis > 0 {
			usagePct := float64(avgCPU) / float64(reqMillis) * 100

			if usagePct > 80 && len(history) >= minHotObservations && consistentHighCPU(history, reqMillis) {
				oldVal := cpuReq.String()
				newMillis := int64(math.Ceil(float64(reqMillis) * 1.5))
				newQty := roundCPU(newMillis)
				container.Resources.Requests[corev1.ResourceCPU] = newQty
				// Preserve a burstable profile's higher CPU limit (e.g. jvm's
				// 1000m ceiling for JIT warm-up); only raise the limit when the
				// request would otherwise exceed it.
				if curLim, ok := container.Resources.Limits[corev1.ResourceCPU]; !ok || newQty.Cmp(curLim) > 0 {
					container.Resources.Limits[corev1.ResourceCPU] = newQty
				}
				entries = append(entries, ResourceLogEntry{
					Time:      now,
					App:       appName,
					Namespace: namespace,
					Action:    "increased CPU",
					From:      oldVal,
					To:        newQty.String(),
					Reason:    fmt.Sprintf("usage at %.0f%%", usagePct),
				})
			} else if usagePct < 20 && len(history) >= 3 && consistentLowCPU(history, reqMillis) && replicas >= 2 && !blockDecrease {
				profileMin := profileDefaults(objectLabels[labels.ResourceProfile])
				minQty := resource.MustParse(profileMin.cpu)
				minMillis := minQty.MilliValue()
				newMillis := int64(math.Ceil(float64(reqMillis) * 0.5))
				if newMillis < minMillis {
					newMillis = minMillis
				}
				newQty := roundCPU(newMillis)
				if newQty.MilliValue() < reqMillis {
					oldVal := cpuReq.String()
					container.Resources.Requests[corev1.ResourceCPU] = newQty
					container.Resources.Limits[corev1.ResourceCPU] = newQty
					entries = append(entries, ResourceLogEntry{
						Time:      now,
						App:       appName,
						Namespace: namespace,
						Action:    "decreased CPU",
						From:      oldVal,
						To:        newQty.String(),
						Reason:    fmt.Sprintf("usage at %.0f%% for 3+ checks", usagePct),
					})
				}
			}
		}
	}

	return
}

// cpuSaturationPct is the threshold at which CPU usage is treated as a hard
// signal that the cgroup is the bottleneck — not transient startup noise.
// At this level the controller bypasses the startup grace period and the
// 3-tick hysteresis and bumps CPU immediately.
const cpuSaturationPct = 95

// minTimeBetweenSaturationBumps prevents thrashing: a pod that just got
// bumped is given time to settle (and either drop below saturation or come
// back asking for more). Slightly less than the 60s tick interval so we can
// react on the next tick if the pod is still pinned.
const minTimeBetweenSaturationBumps = 50 * time.Second

// saturationWarmup excludes freshly started pods from the saturation peak.
// A booting workload (JVM class loading, mongod cache warmup) legitimately
// pins its CPU limit; bumping on that rolls the pod, whose replacement boots
// pinned again — a restart loop. A pod still pinned past this age is genuinely
// bottlenecked and the bump fires (still well inside the 5-minute
// startupGracePeriod the averaging path uses).
const saturationWarmup = 2 * time.Minute

// statefulSetSaturationWindow is how long a StatefulSet must stay pinned
// before the saturation bump fires. Rolling a StatefulSet kills whatever the
// database is doing (a restore, a bulk load), so one hot tick is never enough
// evidence — the window spans at least two 60s ticks.
const statefulSetSaturationWindow = 90 * time.Second

// maxSaturationGap bounds the time between two hot observations that still
// count as one continuous window. A larger gap means ticks went unobserved
// (metrics fetch failed, an OOM intervened), so "sustained" can't be
// affirmed and the window restarts. Slightly over two 60s ticks tolerates
// one missed tick.
const maxSaturationGap = 150 * time.Second

// pinnedWindow tracks a workload's continuous CPU-saturation observations.
type pinnedWindow struct {
	since   time.Time // first hot observation of the current window
	lastHot time.Time // most recent hot observation
}

func (rc *ResourceController) maybeBumpForSaturation(
	namespace, appName string,
	container *corev1.Container,
	podEntries []podMetricsEntry,
	sustainFor time.Duration,
) *ResourceLogEntry {
	if container.Resources.Limits == nil {
		return nil
	}
	cpuLim, ok := container.Resources.Limits[corev1.ResourceCPU]
	if !ok {
		return nil
	}
	limMillis := cpuLim.MilliValue()
	if limMillis == 0 {
		return nil
	}

	// Use the highest-pressure pod, not the average. If even one replica is
	// pinned, the per-pod ceiling is too low. Pods still inside the warmup
	// window don't count: their boot pinning is expected and transient.
	var peakMillis int64
	for _, pe := range podEntries {
		if pe.Age < saturationWarmup {
			continue
		}
		if pe.CPUMillis > peakMillis {
			peakMillis = pe.CPUMillis
		}
	}

	key := namespace + "/" + appName
	usagePct := float64(peakMillis) / float64(limMillis) * 100
	if peakMillis == 0 || usagePct < cpuSaturationPct {
		rc.mu.Lock()
		delete(rc.cpuPinned, key)
		rc.mu.Unlock()
		return nil
	}

	// Workloads that must sustain saturation (StatefulSets) only bump once
	// they have stayed pinned for the whole window. The window must be
	// gap-free: a stretch with no hot observation (metrics outage, an early
	// return elsewhere in the tick) breaks continuity and restarts it.
	now := time.Now()
	rc.mu.Lock()
	w, pinned := rc.cpuPinned[key]
	if !pinned || now.Sub(w.lastHot) > maxSaturationGap {
		w.since = now
	}
	w.lastHot = now
	rc.cpuPinned[key] = w
	sustained := now.Sub(w.since) >= sustainFor
	rc.mu.Unlock()
	if !sustained {
		return nil
	}

	// Don't double-bump in quick succession.
	if rc.recentlyChanged(namespace, appName, minTimeBetweenSaturationBumps) {
		return nil
	}

	oldVal := cpuLim.String()
	newMillis := int64(math.Ceil(float64(limMillis) * 1.5))
	newQty := roundCPU(newMillis)
	if newQty.MilliValue() <= limMillis {
		return nil
	}

	if container.Resources.Requests == nil {
		container.Resources.Requests = corev1.ResourceList{}
	}
	// Bump the limit, but keep request unchanged so we don't suddenly
	// reserve a full core on the node. This is the burstable mode the
	// jvm profile uses by default.
	container.Resources.Limits[corev1.ResourceCPU] = newQty
	if reqQty, ok := container.Resources.Requests[corev1.ResourceCPU]; !ok || reqQty.MilliValue() == 0 {
		// No request set — fall back to mirroring limit.
		container.Resources.Requests[corev1.ResourceCPU] = newQty
	}

	// The bump restarts the pinned window: a further bump needs fresh
	// sustained evidence at the new, higher limit.
	rc.mu.Lock()
	delete(rc.cpuPinned, key)
	rc.mu.Unlock()

	return &ResourceLogEntry{
		Time:      time.Now().UTC().Format(time.RFC3339),
		App:       appName,
		Namespace: namespace,
		Action:    "increased CPU (saturated)",
		From:      oldVal,
		To:        newQty.String(),
		Reason:    fmt.Sprintf("pod at %.0f%% of CPU limit — bypassing startup grace", usagePct),
	}
}

// recentlyChanged returns true if this app had a resource change in the last
// `within` window. Re-uses the existing changeTimestamps map.
func (rc *ResourceController) recentlyChanged(namespace, appName string, within time.Duration) bool {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	key := namespace + "/" + appName
	stamps := rc.changeTimestamps[key]
	if len(stamps) == 0 {
		return false
	}
	last := stamps[len(stamps)-1]
	return time.Since(last) < within
}

func consistentHighMemory(history []usageObservation, reqBytes int64) bool {
	for _, obs := range history {
		if float64(obs.MemoryBytes)/float64(reqBytes)*100 <= 80 {
			return false
		}
	}
	return true
}

func consistentLowMemory(history []usageObservation, reqBytes int64) bool {
	for _, obs := range history {
		if float64(obs.MemoryBytes)/float64(reqBytes)*100 >= 20 {
			return false
		}
	}
	return true
}

func consistentHighCPU(history []usageObservation, reqMillis int64) bool {
	for _, obs := range history {
		if float64(obs.CPUMillis)/float64(reqMillis)*100 <= 80 {
			return false
		}
	}
	return true
}

func consistentLowCPU(history []usageObservation, reqMillis int64) bool {
	for _, obs := range history {
		if float64(obs.CPUMillis)/float64(reqMillis)*100 >= 20 {
			return false
		}
	}
	return true
}

// roundMemory rounds bytes up to the nearest 64Mi.
func roundMemory(bytes int64) resource.Quantity {
	mib := int64(64 * 1024 * 1024)
	rounded := ((bytes + mib - 1) / mib) * mib
	return *resource.NewQuantity(rounded, resource.BinarySI)
}

// roundCPU rounds millicores up to the nearest 50m.
func roundCPU(millis int64) resource.Quantity {
	rounded := ((millis + 49) / 50) * 50
	return *resource.NewMilliQuantity(rounded, resource.DecimalSI)
}

type profileDefaults_ struct {
	name   string
	cpu    string
	memory string
}

func profileDefaults(profile string) profileDefaults_ {
	switch strings.ToLower(profile) {
	case "lightweight":
		return profileDefaults_{name: "lightweight", cpu: "50m", memory: "64Mi"}
	case "standard":
		return profileDefaults_{name: "standard", cpu: "100m", memory: "128Mi"}
	case "compute-heavy":
		return profileDefaults_{name: "compute-heavy", cpu: "500m", memory: "256Mi"}
	case "memory-heavy":
		return profileDefaults_{name: "memory-heavy", cpu: "100m", memory: "512Mi"}
	case "jvm":
		return profileDefaults_{name: "jvm", cpu: "500m", memory: "2Gi"}
	case "database":
		return profileDefaults_{name: "database", cpu: "500m", memory: "1Gi"}
	default:
		return profileDefaults_{name: "standard", cpu: "100m", memory: "128Mi"}
	}
}

func (rc *ResourceController) appendLogEntries(ctx context.Context, newEntries []ResourceLogEntry) {
	cm, err := rc.client.CoreV1().ConfigMaps(modeConfigMapNamespace).Get(ctx, resourceLogConfigMap, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		data, _ := json.Marshal(newEntries)
		cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      resourceLogConfigMap,
				Namespace: modeConfigMapNamespace,
				Labels: map[string]string{
					"app.kubernetes.io/managed-by": "kipper",
				},
			},
			Data: map[string]string{"entries": string(data)},
		}
		if _, err := rc.client.CoreV1().ConfigMaps(modeConfigMapNamespace).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
			log.Printf("resource controller: failed to create resource log: %v", err)
		}
		return
	}
	if err != nil {
		log.Printf("resource controller: failed to read resource log: %v", err)
		return
	}

	var existing []ResourceLogEntry
	if logData, ok := cm.Data["entries"]; ok {
		_ = json.Unmarshal([]byte(logData), &existing)
	}

	existing = append(existing, newEntries...)
	if len(existing) > maxLogEntries {
		existing = existing[len(existing)-maxLogEntries:]
	}

	data, _ := json.Marshal(existing)
	if cm.Data == nil {
		cm.Data = make(map[string]string)
	}
	cm.Data["entries"] = string(data)

	if _, err := rc.client.CoreV1().ConfigMaps(modeConfigMapNamespace).Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		log.Printf("resource controller: failed to update resource log: %v", err)
	}
}

// checkPodProblems lists all Kipper-managed pods and fires a warning alert
// for any container stuck in ImagePullBackOff/ErrImagePull or crash-looping
// for a non-OOM reason. OOM crash loops are left to the OOM path, which
// raises the memory limit. Each pod/container is alerted at most once per
// hour to avoid alert storms.
func (rc *ResourceController) checkPodProblems(ctx context.Context) ([]ResourceLogEntry, []pendingMark) {
	pods, err := rc.client.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		LabelSelector: labels.KipperManagedSelector,
	})
	if err != nil {
		return nil, nil
	}

	rc.mu.Lock()
	defer rc.mu.Unlock()

	cooldown := 60 * time.Minute
	now := time.Now()
	nowStr := now.UTC().Format(time.RFC3339)
	var entries []ResourceLogEntry
	var marks []pendingMark

	for i := range pods.Items {
		pod := &pods.Items[i]
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.State.Waiting == nil {
				continue
			}
			key := pod.Namespace + "/" + pod.Name + "/" + cs.Name

			switch cs.State.Waiting.Reason {
			case "ImagePullBackOff", "ErrImagePull":
				if last, seen := rc.imagePullAlerted[key]; seen && now.Sub(last) < cooldown {
					continue
				}
				marks = append(marks, pendingMark{dst: rc.imagePullAlerted, key: key, at: now})
				entries = append(entries, ResourceLogEntry{
					Time:      nowStr,
					App:       pod.Labels["app"],
					Namespace: pod.Namespace,
					Action:    "ImagePullBackOff",
					Reason:    fmt.Sprintf("container %q cannot pull image — registry credentials may be expired", cs.Name),
				})
			case "CrashLoopBackOff":
				// The OOM path already bumps memory for OOM crash loops;
				// alerting here too would double-count them.
				if term := cs.LastTerminationState.Terminated; term != nil && term.Reason == "OOMKilled" {
					continue
				}
				if last, seen := rc.crashLoopAlerted[key]; seen && now.Sub(last) < cooldown {
					continue
				}
				marks = append(marks, pendingMark{dst: rc.crashLoopAlerted, key: key, at: now})
				reason := fmt.Sprintf("container %q is crash-looping", cs.Name)
				if term := cs.LastTerminationState.Terminated; term != nil {
					reason += fmt.Sprintf(" (last exit code %d)", term.ExitCode)
				}
				entries = append(entries, ResourceLogEntry{
					Time:      nowStr,
					App:       pod.Labels["app"],
					Namespace: pod.Namespace,
					Action:    "CrashLoopBackOff",
					Reason:    reason,
				})
			}
		}
	}

	return entries, marks
}

// checkNodeReady alerts when a node's Ready condition is not True. Each node
// is alerted at most once per cooldown so a node that stays down does not
// storm; the marker resets on recovery so the next outage alerts again.
func (rc *ResourceController) checkNodeReady(nodes []corev1.Node) ([]ResourceLogEntry, []pendingMark) {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	now := time.Now()
	var entries []ResourceLogEntry
	var marks []pendingMark
	for i := range nodes {
		node := &nodes[i]
		ready := false
		for _, c := range node.Status.Conditions {
			if c.Type == corev1.NodeReady {
				ready = c.Status == corev1.ConditionTrue
				break
			}
		}
		if ready {
			delete(rc.nodeReadyAlerted, node.Name)
			continue
		}
		if last, seen := rc.nodeReadyAlerted[node.Name]; seen && now.Sub(last) < nodeAlertCooldown {
			continue
		}
		marks = append(marks, pendingMark{dst: rc.nodeReadyAlerted, key: node.Name, at: now})
		entries = append(entries, ResourceLogEntry{
			Time:      now.UTC().Format(time.RFC3339),
			App:       "cluster",
			Namespace: "system",
			Action:    "node NotReady",
			Reason:    fmt.Sprintf("node %q is not Ready — workloads on it may be disrupted", node.Name),
			Severity:  "critical",
		})
	}
	return entries, marks
}

// checkFailedJobs alerts on Kipper-managed Jobs that ended in failure. This
// covers app builds, one-off Jobs, and CronJob runs, since all of them are
// batch Jobs. Each Job is alerted at most once per cooldown.
func (rc *ResourceController) checkFailedJobs(ctx context.Context) ([]ResourceLogEntry, []pendingMark) {
	jobs, err := rc.client.BatchV1().Jobs("").List(ctx, metav1.ListOptions{
		LabelSelector: labels.KipperManagedSelector,
	})
	if err != nil {
		return nil, nil
	}

	rc.mu.Lock()
	defer rc.mu.Unlock()

	now := time.Now()
	var entries []ResourceLogEntry
	var marks []pendingMark
	for i := range jobs.Items {
		job := &jobs.Items[i]
		failed := false
		for _, c := range job.Status.Conditions {
			if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
				failed = true
				break
			}
		}
		if !failed {
			continue
		}
		// Key on the Job UID, not its name: a build Job is deleted and
		// recreated with the same name per commit, and keying on name would
		// suppress the alert for a genuinely new failed run.
		key := string(job.UID)
		if last, seen := rc.jobFailAlerted[key]; seen && now.Sub(last) < jobAlertCooldown {
			continue
		}
		marks = append(marks, pendingMark{dst: rc.jobFailAlerted, key: key, at: now})
		entries = append(entries, ResourceLogEntry{
			Time:      now.UTC().Format(time.RFC3339),
			App:       job.Labels["app"],
			Namespace: job.Namespace,
			Action:    "job failed",
			Reason:    fmt.Sprintf("job %q failed — check its logs", job.Name),
			Severity:  "warning",
		})
	}
	return entries, marks
}

// checkStuckRollouts alerts when a Deployment's rollout has stalled
// (ProgressDeadlineExceeded). The deployment slice is the one already listed
// for this tick, so no extra API call is made. The marker resets once the
// rollout recovers.
func (rc *ResourceController) checkStuckRollouts(deployments []appsv1.Deployment) ([]ResourceLogEntry, []pendingMark) {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	now := time.Now()
	var entries []ResourceLogEntry
	var marks []pendingMark
	for i := range deployments {
		d := &deployments[i]
		stuck := false
		for _, c := range d.Status.Conditions {
			if c.Type == appsv1.DeploymentProgressing && c.Status == corev1.ConditionFalse && c.Reason == "ProgressDeadlineExceeded" {
				stuck = true
				break
			}
		}
		key := d.Namespace + "/" + d.Name
		if !stuck {
			delete(rc.rolloutAlerted, key)
			continue
		}
		if last, seen := rc.rolloutAlerted[key]; seen && now.Sub(last) < rolloutAlertCooldown {
			continue
		}
		marks = append(marks, pendingMark{dst: rc.rolloutAlerted, key: key, at: now})
		entries = append(entries, ResourceLogEntry{
			Time:      now.UTC().Format(time.RFC3339),
			App:       d.Labels["app"],
			Namespace: d.Namespace,
			Action:    "rollout stuck",
			Reason:    fmt.Sprintf("deployment %q did not roll out within its progress deadline", d.Name),
			Severity:  "warning",
		})
	}
	return entries, marks
}

func (rc *ResourceController) createAlerts(ctx context.Context, entries []ResourceLogEntry) error {
	if len(entries) == 0 {
		return nil
	}
	alerts := make([]handlers.Alert, 0, len(entries))
	for i, entry := range entries {
		severity := entry.Severity
		if severity == "" {
			severity = alertSeverity(entry.Action)
		}
		// Only append the "(from → to)" transition when there is one. Failure
		// alerts (image pull, crash loop, node not ready, job failed, stuck
		// rollout) carry no from/to and would otherwise render "( → )".
		action := entry.Action
		if !strings.Contains(entry.Action, "→") && (entry.From != "" || entry.To != "") {
			action = fmt.Sprintf("%s (%s → %s)", entry.Action, entry.From, entry.To)
		}
		alerts = append(alerts, handlers.Alert{
			ID:        fmt.Sprintf("%s-%s-%d-%d", entry.Namespace, entry.App, time.Now().UnixNano(), i),
			Time:      entry.Time,
			App:       entry.App,
			Namespace: entry.Namespace,
			Action:    action,
			Severity:  severity,
			Reason:    entry.Reason,
		})
	}
	// One conflict-retried write for the whole tick instead of one per alert.
	return handlers.AddAlerts(ctx, rc.client, alerts)
}

func alertSeverity(action string) string {
	switch {
	case strings.Contains(action, "OOM"), strings.Contains(action, "doubled memory"):
		return "critical"
	case strings.Contains(action, "stuck"), strings.Contains(action, "increased"), strings.Contains(action, "scaled out"), strings.Contains(action, "ImagePullBackOff"), strings.Contains(action, "CrashLoopBackOff"):
		return "warning"
	default:
		return "info"
	}
}
