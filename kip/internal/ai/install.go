package ai

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"
)

// Default sizes for the model cache PVC. The Ollama image plus a 14B Q4
// model fits comfortably in 60 GiB.
const (
	defaultPVCTier1 = "10Gi"
	defaultPVCTier2 = "30Gi"
	defaultPVCTier3 = "60Gi"
)

// Tier-appropriate memory requests for the Ollama container. Sized for
// the model plus runtime overhead so the scheduler refuses placement on
// a node that cannot host the chosen tier.
const (
	memoryRequestTier1 = "6Gi"
	memoryRequestTier2 = "10Gi"
	memoryRequestTier3 = "10Gi"
	memoryRequestTier4 = "20Gi"
)

// managedByLabel and managedByValue gate any update or delete on a
// resource that shares a name with the AI bundle. Refusing to mutate
// resources we do not own prevents us from clobbering an unrelated user
// HelmChart that happens to be called "ollama".
const (
	managedByLabel = "app.kubernetes.io/managed-by"
	managedByValue = "kipper"
)

// errNotOwnedByKipper is returned when an apply or delete would touch a
// resource lacking Kipper's ownership label.
type errNotOwnedByKipper struct {
	kind string
	name string
}

func (e *errNotOwnedByKipper) Error() string {
	return fmt.Sprintf("%s/%s exists but is not managed by kipper; refusing to overwrite", e.kind, e.name)
}

// Resource group/version/resource handles for everything the installer
// applies. Hardcoded because we own the manifest set.
var (
	namespaceGVR = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
	secretGVR    = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	configMapGVR = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	ingressGVR   = schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"}
	helmChartGVR = schema.GroupVersionResource{Group: "helm.cattle.io", Version: "v1", Resource: "helmcharts"}
)

// Options controls an Install or Uninstall run.
type Options struct {
	// Host is the external chat UI hostname, e.g. "chat.example.com".
	// Required for Install. Ignored by Uninstall.
	Host string
	// NodeName is the cluster node that passed tier preflight. Install
	// pins the Ollama Deployment here via nodeSelector so the scheduler
	// cannot place the pod on a smaller node. Required for Install.
	NodeName string
	// Model overrides the default model for the detected tier. Optional.
	Model string
	// PVCSize overrides the model-cache PVC size. Optional, defaults by
	// tier.
	PVCSize string
}

// Installer applies and removes the Kipper AI bundle against a cluster.
type Installer struct {
	Clientset kubernetes.Interface
	Dynamic   dynamic.Interface
	// Out receives step-status output. nil routes to io.Discard.
	Out io.Writer
	// PollInterval and PollTimeout govern rollout polling. Zero values
	// fall back to production defaults; tests typically override both.
	PollInterval time.Duration
	PollTimeout  time.Duration
	// AckWindow is the WaitDelete-only threshold for treating a
	// DeleteBackupRequest as stuck (controller never picked it up).
	// Zero falls back to deleteAckWindow.
	AckWindow time.Duration
}

func (i *Installer) out() io.Writer {
	if i.Out == nil {
		return io.Discard
	}
	return i.Out
}

func (i *Installer) interval() time.Duration {
	if i.PollInterval == 0 {
		return 5 * time.Second
	}
	return i.PollInterval
}

func (i *Installer) ackWindow() time.Duration {
	if i.AckWindow == 0 {
		return deleteAckWindow
	}
	return i.AckWindow
}

func (i *Installer) timeout() time.Duration {
	if i.PollTimeout == 0 {
		// 30 minutes covers cold-start cases that 10 minutes did not:
		// Longhorn PVC provisioning, MongoDB image pull (~270 MB),
		// MongoDB init, LibreChat boot waiting for Mongo, plus the
		// in-Job model probe (which itself has a 30-minute deadline).
		// Each step short-circuits when state is already healthy, so
		// the larger ceiling does not slow successful re-runs.
		return 30 * time.Minute
	}
	return i.PollTimeout
}

// Install applies the AI bundle. The cluster must already meet the tier-1
// minimum; the caller is expected to have run Detect first, refused
// below tier 1, and passed the BestNode through opts.NodeName so the
// pod is pinned to the node that actually has the headroom.
func (i *Installer) Install(ctx context.Context, tier Tier, opts Options) error {
	if !tier.MeetsMinimum() {
		return fmt.Errorf("cluster below tier 1 minimum, refusing to install")
	}
	if opts.Host == "" {
		return fmt.Errorf("host is required")
	}
	if opts.NodeName == "" {
		return fmt.Errorf("nodeName is required (run Detect and pass report.BestNode.Name)")
	}

	model := opts.Model
	if model == "" {
		model = tier.DefaultModel()
	}
	pvcSize := opts.PVCSize
	if pvcSize == "" {
		pvcSize = DefaultPVCFor(tier)
	}

	// Reuse existing LibreChat credentials across re-applies. The
	// credentials are referenced by chart-rendered Pods that already
	// loaded them at startup; rotating them on every `kip ai install`
	// would silently break MongoDB auth and JWT decoding for running
	// pods. Only generate fresh values on a true first install.
	creds, err := i.loadOrGenerateLibreChatCredentials(ctx)
	if err != nil {
		return fmt.Errorf("preparing librechat credentials: %w", err)
	}

	// Closed-over state from the apply step, threaded into the
	// matching wait step so both chart installs can be kicked off in
	// parallel before either Job wait blocks.
	var ollamaState, libreChatState helmApplyState

	steps := []struct {
		name string
		fn   func() error
	}{
		{"Creating namespace", func() error { return i.applyManifest(ctx, NamespaceManifest()) }},
		{"Installing Ollama", func() error {
			state, err := i.applyHelmChart(ctx, OllamaManifest(OllamaConfig{
				Model:         model,
				PVCSize:       pvcSize,
				EnableGPU:     tier >= TierThree,
				MemoryRequest: memoryRequestFor(tier),
				NodeName:      opts.NodeName,
			}), "ollama")
			ollamaState = state
			return err
		}},
		{"Installing LibreChat", func() error {
			state, err := i.applyHelmChart(ctx, LibreChatManifest(LibreChatConfig{
				Host:        opts.Host,
				Model:       model,
				Credentials: creds,
			}), "librechat")
			libreChatState = state
			return err
		}},
		{"Waiting for Ollama chart job", func() error { return i.waitForHelmChartJobFromState(ctx, ollamaState) }},
		{"Waiting for Ollama to be ready", func() error { return i.waitForDeployment(ctx, "ollama") }},
		{"Verifying Ollama loaded the model", func() error { return i.waitForModelLoaded(ctx, model) }},
		{"Waiting for LibreChat chart job", func() error { return i.waitForHelmChartJobFromState(ctx, libreChatState) }},
		{"Waiting for LibreChat to be ready", func() error { return i.waitForDeployment(ctx, LibreChatDeploymentName) }},
		{"Recording bundle state", func() error { return i.writeBundleState(ctx, model, opts.Host) }},
	}

	for _, s := range steps {
		_, _ = fmt.Fprintf(i.out(), "  ...  %s\n", s.name)
		if err := s.fn(); err != nil {
			return fmt.Errorf("%s: %w", s.name, err)
		}
		_, _ = fmt.Fprintf(i.out(), "  ✔   %s\n", s.name)
	}
	return nil
}

// Uninstall removes the AI bundle and its data. The order is:
//  1. Delete the HelmChart CRs so helm-controller stops reconciling.
//  2. Delete the namespace, which cascades through every remaining
//     resource (PVCs, Secrets, ConfigMaps, Ingress, leftover Pods).
//
// We tried preserving PVCs across uninstall via
// `helm.sh/resource-policy: keep`, but k3s helm-controller's bundled
// helm does not honour that annotation: chart-owned PVCs are deleted
// regardless. Rather than ship a half-working preservation promise, we
// document `kip ai uninstall` as fully destructive and defer
// data-preservation to a Phase 2 Velero-backed backup/restore command.
func (i *Installer) Uninstall(ctx context.Context, _ Options) error {
	// Refuse to delete the namespace while the Phase 2 RAG bundle is
	// still installed. Removing kipper-ai would cascade through Qdrant
	// and AnythingLLM PVCs, silently destroying the vector store and
	// document uploads. Surface the right next command instead.
	ragPresent, err := i.ragInstalled(ctx)
	if err != nil {
		return fmt.Errorf("checking for RAG bundle: %w", err)
	}
	if ragPresent {
		return fmt.Errorf(
			"RAG bundle is installed in namespace %s; run 'kip ai rag uninstall' first (this is destructive: back up Qdrant collections and AnythingLLM uploads with 'kip ai backup' first if you want to preserve them)",
			Namespace,
		)
	}

	steps := []struct {
		name string
		fn   func() error
	}{
		{"Removing LibreChat", func() error { return i.deleteHelmChart(ctx, "librechat") }},
		{"Removing Ollama", func() error { return i.deleteHelmChart(ctx, "ollama") }},
		{"Removing namespace and data", func() error { return i.deleteNamespace(ctx) }},
	}
	for _, s := range steps {
		_, _ = fmt.Fprintf(i.out(), "  ...  %s\n", s.name)
		if err := s.fn(); err != nil {
			return fmt.Errorf("%s: %w", s.name, err)
		}
		_, _ = fmt.Fprintf(i.out(), "  ✔   %s\n", s.name)
	}
	return nil
}

// applyManifest applies a multi-document YAML manifest. Each document is
// created or updated (resourceVersion preserved on update) via the dynamic
// client.
func (i *Installer) applyManifest(ctx context.Context, manifest string) error {
	for _, doc := range SplitYAMLDocuments(manifest) {
		if err := i.applyDocument(ctx, doc); err != nil {
			return err
		}
	}
	return nil
}

func (i *Installer) applyDocument(ctx context.Context, doc string) error {
	obj := &unstructured.Unstructured{}
	if err := yaml.Unmarshal([]byte(doc), &obj.Object); err != nil {
		return fmt.Errorf("parsing manifest yaml: %w", err)
	}
	gvr, err := gvrFor(obj)
	if err != nil {
		return err
	}

	resource := i.Dynamic.Resource(gvr)
	var ri dynamic.ResourceInterface = resource
	if ns := obj.GetNamespace(); ns != "" {
		ri = resource.Namespace(ns)
	}

	existing, err := ri.Get(ctx, obj.GetName(), metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		if _, err := ri.Create(ctx, obj, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("creating %s/%s: %w", obj.GetKind(), obj.GetName(), err)
		}
	case err != nil:
		return fmt.Errorf("reading existing %s/%s: %w", obj.GetKind(), obj.GetName(), err)
	default:
		if !isManagedByKipper(existing.GetLabels()) {
			return &errNotOwnedByKipper{kind: obj.GetKind(), name: obj.GetName()}
		}
		obj.SetResourceVersion(existing.GetResourceVersion())
		if _, err := ri.Update(ctx, obj, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("updating %s/%s: %w", obj.GetKind(), obj.GetName(), err)
		}
	}
	return nil
}

func isManagedByKipper(labels map[string]string) bool {
	return labels[managedByLabel] == managedByValue
}

func (i *Installer) deleteHelmChart(ctx context.Context, name string) error {
	// HelmChart resources live in kube-system; helm-controller uninstalls
	// the release and cleans up workload resources in the target namespace
	// when the CR is removed. Refuse to delete a chart we do not own so we
	// never wipe an unrelated user release that happens to share a name.
	// helm-controller cleanup is async, so block until the CR has actually
	// been finalised before returning. Otherwise a follow-up purge can race
	// the chart's still-running pods and PVC finalizers.
	chartClient := i.Dynamic.Resource(helmChartGVR).Namespace("kube-system")
	existing, err := chartClient.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading helmchart %s: %w", name, err)
	}
	if !isManagedByKipper(existing.GetLabels()) {
		return &errNotOwnedByKipper{kind: "HelmChart", name: name}
	}
	if err := chartClient.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting helmchart %s: %w", name, err)
	}
	return i.waitForHelmChartGone(ctx, name)
}

// helmInstallJobName is the well-known Job name pattern that
// helm-controller uses for chart installs and upgrades. The Job lives
// in kube-system alongside the HelmChart CR.
func helmInstallJobName(chartName string) string {
	return "helm-install-" + chartName
}

// jobCondition returns true if the Job has a condition of the given
// type with status True. Used to read terminal signals (Complete /
// Failed) without confusing per-pod retry counters with end state.
func jobCondition(job *batchv1.Job, condType batchv1.JobConditionType) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == condType && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// helmInstallJobUID returns the current UID of the helm-install Job
// for the given chart, or empty string if it does not exist. Used to
// detect when helm-controller has replaced the Job after a spec
// change (the controller deletes and recreates the Job rather than
// updating it in place, so the UID changes on every spec-driven
// rerun).
func (i *Installer) helmInstallJobUID(ctx context.Context, chartName string) (types.UID, error) {
	job, err := i.Clientset.BatchV1().Jobs("kube-system").Get(ctx, helmInstallJobName(chartName), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading helm-install job for %s: %w", chartName, err)
	}
	return job.UID, nil
}

// helmChartGeneration returns the metadata.generation of the named
// HelmChart, or 0 if the HelmChart does not exist. Comparing
// generation before and after applyManifest tells us whether the
// apply produced a real spec change (in which case helm-controller
// will spawn a new Job) or was a no-op.
func (i *Installer) helmChartGeneration(ctx context.Context, chartName string) (int64, error) {
	chart, err := i.Dynamic.Resource(helmChartGVR).Namespace("kube-system").Get(ctx, chartName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("reading helmchart %s: %w", chartName, err)
	}
	return chart.GetGeneration(), nil
}

// waitForHelmChartJob waits until the helm-install Job that
// helm-controller spawned for `chartName` reaches Succeeded > 0, or
// fails loudly on Failed > 0.
//
// k3s helm-controller's HelmChartStatus has no observedGeneration
// field, so there is no straightforward "ready when ObservedGeneration
// == Generation" check. Instead we anchor on the Job: helm-controller
// deletes and recreates `helm-install-<chartName>` whenever the chart
// spec changes (verified against k3s-io/helm-controller chart.go's
// reconcileJob → ErrReplace path). A new UID after our apply means
// helm-controller has reconciled the new spec; we then poll the new
// Job to terminal state. If the apply was a no-op (generation
// unchanged) the existing Succeeded Job satisfies us immediately.
//
// Without this gate, waitForDeployment can return success against the
// pre-upgrade Deployment that is still Available because helm-controller
// has not yet rolled out the new chart-rendered manifests.
func (i *Installer) waitForHelmChartJob(ctx context.Context, chartName string, prevJobUID types.UID, expectNewJob bool) error {
	deadline := time.Now().Add(i.timeout())
	jobName := helmInstallJobName(chartName)
	for {
		// Prefer status.jobName when set: helm-controller writes the
		// active job name there, and reading it confirms the
		// controller has at least observed the chart. Fall back to
		// the well-known name pattern.
		chart, err := i.Dynamic.Resource(helmChartGVR).Namespace("kube-system").Get(ctx, chartName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("reading helmchart %s: %w", chartName, err)
		}
		if statusJobName, found, _ := unstructured.NestedString(chart.Object, "status", "jobName"); found && statusJobName != "" {
			jobName = statusJobName
		}

		job, err := i.Clientset.BatchV1().Jobs("kube-system").Get(ctx, jobName, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			// helm-controller has deleted the previous Job and is
			// about to recreate it (template-change replace path), or
			// hasn't created the first Job yet. Wait.
		case err != nil:
			return fmt.Errorf("reading helm-install job %s: %w", jobName, err)
		case expectNewJob && job.UID == prevJobUID:
			// Stale Job from before the spec change. Wait for
			// helm-controller to replace it. A succeeded condition
			// here would be a false positive, since this Job ran
			// against the previous chart spec.
		default:
			// Use the Job's conditions for the terminal signal.
			// Status.Failed counts failed pods, but k3s helm-controller
			// Jobs have a high backoffLimit (~1000), so a transient
			// pod failure can have Failed>0 while the controller is
			// still retrying. Only Complete=True or Failed=True on the
			// Job's conditions list mean the Job has actually stopped.
			if jobCondition(job, batchv1.JobComplete) {
				return nil
			}
			if jobCondition(job, batchv1.JobFailed) {
				return fmt.Errorf("helm-controller job %s for chart %s failed; check 'kubectl logs -n kube-system jobs/%s'", jobName, chartName, jobName)
			}
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("helm-install job for %s did not finish within %s", chartName, i.timeout())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(i.interval()):
		}
	}
}

// helmApplyState records the pre-apply state needed to recognise the
// helm-install Job that helm-controller will spawn for the new chart
// spec. The wait step uses prevJobUID to skip the previous Succeeded
// Job (false positive on upgrade) and expectNewJob to short-circuit
// on no-op re-applies.
type helmApplyState struct {
	chartName    string
	prevJobUID   types.UID
	expectNewJob bool
}

// applyHelmChart captures pre-apply state, applies the manifest, then
// returns the state needed to drive a later waitForHelmChartJobFromState
// call. The split lets callers kick off multiple chart applies in
// parallel before waiting on any of them.
func (i *Installer) applyHelmChart(ctx context.Context, manifest, chartName string) (helmApplyState, error) {
	prevGen, err := i.helmChartGeneration(ctx, chartName)
	if err != nil {
		return helmApplyState{}, err
	}
	prevJobUID, err := i.helmInstallJobUID(ctx, chartName)
	if err != nil {
		return helmApplyState{}, err
	}
	if err := i.applyManifest(ctx, manifest); err != nil {
		return helmApplyState{}, err
	}
	newGen, err := i.helmChartGeneration(ctx, chartName)
	if err != nil {
		return helmApplyState{}, err
	}
	return helmApplyState{
		chartName:    chartName,
		prevJobUID:   prevJobUID,
		expectNewJob: newGen > prevGen,
	}, nil
}

func (i *Installer) waitForHelmChartJobFromState(ctx context.Context, state helmApplyState) error {
	return i.waitForHelmChartJob(ctx, state.chartName, state.prevJobUID, state.expectNewJob)
}

// waitForHelmChartGone polls until the named HelmChart is no longer
// retrievable. helm-controller uninstalls the release while the CR's
// finalizer keeps it visible; once cleanup finishes, the CR disappears.
func (i *Installer) waitForHelmChartGone(ctx context.Context, name string) error {
	chartClient := i.Dynamic.Resource(helmChartGVR).Namespace("kube-system")
	deadline := time.Now().Add(i.timeout())
	for {
		_, err := chartClient.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("waiting for helmchart %s cleanup: %w", name, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("helmchart %s still present after %s; helm-controller cleanup did not finish", name, i.timeout())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(i.interval()):
		}
	}
}

func (i *Installer) deleteNamespace(ctx context.Context) error {
	existing, err := i.Dynamic.Resource(namespaceGVR).Get(ctx, Namespace, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading namespace %s: %w", Namespace, err)
	}
	if !isManagedByKipper(existing.GetLabels()) {
		return &errNotOwnedByKipper{kind: "Namespace", name: Namespace}
	}
	if err := i.Dynamic.Resource(namespaceGVR).Delete(ctx, Namespace, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting namespace %s: %w", Namespace, err)
	}
	return nil
}

// waitForDeployment polls until the named Deployment exists and reports
// at least one ready replica with the Available condition true. It
// tolerates the deployment not existing yet, since helm-controller may
// still be downloading the chart.
func (i *Installer) waitForDeployment(ctx context.Context, name string) error {
	deadline := time.Now().Add(i.timeout())
	for {
		dep, err := i.Clientset.AppsV1().Deployments(Namespace).Get(ctx, name, metav1.GetOptions{})
		switch {
		case err == nil:
			if isDeploymentAvailable(dep) {
				return nil
			}
		case apierrors.IsNotFound(err):
			// fall through to wait
		default:
			return fmt.Errorf("reading deployment %s/%s: %w", Namespace, name, err)
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("deployment %s/%s not ready within %s", Namespace, name, i.timeout())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(i.interval()):
		}
	}
}

// isDeploymentAvailable returns true only when the Deployment has
// finished rolling out the spec the API server has observed. A bare
// Available=True check is not enough: a Deployment mid-rollout (old
// pods being replaced) can still report Available because at least one
// updated replica is up while the rest are pending. The matrix here
// matches kubectl's rollout-status logic.
func isDeploymentAvailable(dep *appsv1.Deployment) bool {
	// Deployment controller has not yet seen the latest spec.
	if dep.Status.ObservedGeneration < dep.Generation {
		return false
	}
	// Mid-rollout: not all pods are on the new template yet.
	if dep.Spec.Replicas != nil && dep.Status.UpdatedReplicas < *dep.Spec.Replicas {
		return false
	}
	// Old pods still pending termination.
	if dep.Status.Replicas > dep.Status.UpdatedReplicas {
		return false
	}
	// Updated pods not all ready.
	if dep.Status.AvailableReplicas < dep.Status.UpdatedReplicas {
		return false
	}
	if dep.Status.ReadyReplicas < 1 {
		return false
	}
	for _, cond := range dep.Status.Conditions {
		if cond.Type == appsv1.DeploymentAvailable && cond.Status == "True" {
			return true
		}
	}
	return false
}

// modelProbeNamePrefix is the GenerateName prefix used for the one-shot
// Job that confirms the configured Ollama model is loaded. Each install
// run gets a unique server-assigned suffix; this avoids the
// delete-then-create AlreadyExists race when retrying after a timeout
// (Kubernetes deletion is async, and TTL cleanup also has lag).
const modelProbeNamePrefix = "ollama-model-probe-"

// waitForModelLoaded confirms that the configured model has finished
// pulling and is reachable through the Ollama API. The otwld chart marks
// the Ollama Deployment Available before the model pull completes, so
// this is the gate the user actually cares about: a green install must
// mean the chat URL works.
//
// The probe runs as a Job inside the cluster so it does not depend on
// the operator's host being able to reach the in-cluster Service. It
// hits /api/tags and greps for the configured model, retrying on a tight
// loop until the model appears or the Job's deadline expires.
func (i *Installer) waitForModelLoaded(ctx context.Context, model string) error {
	if model == "" {
		return fmt.Errorf("model is required")
	}

	job := buildModelProbeJob(model)
	created, err := i.Clientset.BatchV1().Jobs(Namespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("creating model probe job: %w", err)
	}
	jobName := created.GetName()

	deadline := time.Now().Add(i.timeout())
	for {
		got, err := i.Clientset.BatchV1().Jobs(Namespace).Get(ctx, jobName, metav1.GetOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("reading model probe job: %w", err)
		}
		if got != nil {
			if got.Status.Succeeded > 0 {
				return nil
			}
			if got.Status.Failed > 0 {
				return fmt.Errorf("model %q did not load; check ollama logs with 'kubectl logs -n %s deployment/ollama' and the probe with 'kubectl logs -n %s job/%s'",
					model, Namespace, Namespace, jobName)
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("model %q not ready within %s; check 'kubectl logs -n %s deployment/ollama' for pull progress",
				model, i.timeout(), Namespace)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(i.interval()):
		}
	}
}

// buildModelProbeJob constructs the Job that polls Ollama's /api/tags
// endpoint until the configured model appears. GenerateName makes each
// install run produce a unique server-assigned name, so retries after
// timeout do not collide with a still-deleting predecessor.
func buildModelProbeJob(model string) *batchv1.Job {
	noBackoff := int32(0)
	ttl := int32(60)
	deadline := int64(1800) // 30 minutes upper bound on the in-pod loop.

	script := fmt.Sprintf(`set -eu
TARGET="%s"
URL="http://ollama.%s.svc.cluster.local:11434/api/tags"
for i in $(seq 1 360); do
  if wget -q -O- "$URL" 2>/dev/null | grep -q "$TARGET"; then
    echo "model $TARGET loaded"
    exit 0
  fi
  echo "waiting for $TARGET ($i/360)"
  sleep 5
done
echo "model $TARGET did not appear within 30 minutes" >&2
exit 1
`, model, Namespace)

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: modelProbeNamePrefix,
			Namespace:    Namespace,
			Labels: map[string]string{
				managedByLabel:                managedByValue,
				"app.kubernetes.io/part-of":   "kipper-ai",
				"app.kubernetes.io/component": "model-probe",
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &noBackoff,
			TTLSecondsAfterFinished: &ttl,
			ActiveDeadlineSeconds:   &deadline,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{managedByLabel: managedByValue},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:    "probe",
						Image:   "busybox:1.36",
						Command: []string{"sh", "-c", script},
					}},
				},
			},
		},
	}
}

func memoryRequestFor(t Tier) string {
	switch t {
	case TierOne:
		return memoryRequestTier1
	case TierTwo, TierThree:
		return memoryRequestTier2
	case TierFour:
		return memoryRequestTier4
	default:
		return memoryRequestTier1
	}
}

// DefaultPVCFor returns the model-cache PVC size that `kip ai install`
// uses when no `--pvc-size` override is supplied. Exported so callers
// outside the package (the CLI's storage preflight) can compute the
// projected backup size without re-implementing the tier table.
func DefaultPVCFor(t Tier) string {
	switch t {
	case TierOne:
		return defaultPVCTier1
	case TierTwo:
		return defaultPVCTier2
	default:
		return defaultPVCTier3
	}
}

// writeBundleState records the installed model and chat host in a
// Kipper-owned ConfigMap so `kip ai status` can report the actual
// installed model independently of the local AI client config.
func (i *Installer) writeBundleState(ctx context.Context, model, host string) error {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      BundleStateConfigMapName,
			Namespace: Namespace,
			Labels: map[string]string{
				managedByLabel:              managedByValue,
				"app.kubernetes.io/part-of": "kipper-ai",
			},
		},
		Data: map[string]string{
			"model": model,
			"host":  host,
		},
	}
	cms := i.Clientset.CoreV1().ConfigMaps(Namespace)
	existing, err := cms.Get(ctx, BundleStateConfigMapName, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		if _, err := cms.Create(ctx, cm, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("creating bundle state configmap: %w", err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("reading bundle state configmap: %w", err)
	}
	if !isManagedByKipper(existing.Labels) {
		return &errNotOwnedByKipper{kind: "ConfigMap", name: BundleStateConfigMapName}
	}
	cm.ResourceVersion = existing.ResourceVersion
	if _, err := cms.Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("updating bundle state configmap: %w", err)
	}
	return nil
}

// loadOrGenerateLibreChatCredentials returns the credentials carried
// by the existing librechat-credentials Secret if it is Kipper-owned
// and contains all five expected keys; otherwise generates a fresh
// set. This makes `kip ai install` idempotent on credential state — a
// re-apply preserves existing values rather than silently rotating
// them out from under running pods.
func (i *Installer) loadOrGenerateLibreChatCredentials(ctx context.Context) (LibreChatCredentials, error) {
	got, err := i.Clientset.CoreV1().Secrets(Namespace).Get(ctx, LibreChatCredentialsSecretName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return generateLibreChatCredentials()
	}
	if err != nil {
		return LibreChatCredentials{}, fmt.Errorf("reading existing librechat credentials: %w", err)
	}
	if !isManagedByKipper(got.Labels) {
		return LibreChatCredentials{}, &errNotOwnedByKipper{kind: "Secret", name: LibreChatCredentialsSecretName}
	}
	creds := LibreChatCredentials{
		CredsKey:         string(got.Data["CREDS_KEY"]),
		CredsIV:          string(got.Data["CREDS_IV"]),
		JWTSecret:        string(got.Data["JWT_SECRET"]),
		JWTRefreshSecret: string(got.Data["JWT_REFRESH_SECRET"]),
		MeiliMasterKey:   string(got.Data["MEILI_MASTER_KEY"]),
	}
	if creds.CredsKey == "" || creds.CredsIV == "" || creds.JWTSecret == "" || creds.JWTRefreshSecret == "" || creds.MeiliMasterKey == "" {
		// Existing Secret is missing one of the five values — treat it
		// as corrupt and start fresh. Better to roll the pods once
		// than to ship a half-populated credential set.
		return generateLibreChatCredentials()
	}
	return creds, nil
}

// generateLibreChatCredentials produces the five random values LibreChat
// needs at boot. Lengths: 32 bytes for CREDS_KEY/JWT pair/MEILI_MASTER_KEY,
// 16 bytes for CREDS_IV. Matches the chart's example values.yaml.
func generateLibreChatCredentials() (LibreChatCredentials, error) {
	credsKey, err := randomHex(32)
	if err != nil {
		return LibreChatCredentials{}, err
	}
	credsIV, err := randomHex(16)
	if err != nil {
		return LibreChatCredentials{}, err
	}
	jwtSecret, err := randomHex(32)
	if err != nil {
		return LibreChatCredentials{}, err
	}
	jwtRefresh, err := randomHex(32)
	if err != nil {
		return LibreChatCredentials{}, err
	}
	meili, err := randomHex(32)
	if err != nil {
		return LibreChatCredentials{}, err
	}
	return LibreChatCredentials{
		CredsKey:         credsKey,
		CredsIV:          credsIV,
		JWTSecret:        jwtSecret,
		JWTRefreshSecret: jwtRefresh,
		MeiliMasterKey:   meili,
	}, nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// gvrFor maps an unstructured object's apiVersion + kind to one of the
// hardcoded resource handles. Errors on anything we did not generate.
func gvrFor(obj *unstructured.Unstructured) (schema.GroupVersionResource, error) {
	gv, err := schema.ParseGroupVersion(obj.GetAPIVersion())
	if err != nil {
		return schema.GroupVersionResource{}, fmt.Errorf("parsing apiVersion %q: %w", obj.GetAPIVersion(), err)
	}
	switch {
	case gv.Group == "" && gv.Version == "v1" && obj.GetKind() == "Namespace":
		return namespaceGVR, nil
	case gv.Group == "" && gv.Version == "v1" && obj.GetKind() == "Secret":
		return secretGVR, nil
	case gv.Group == "" && gv.Version == "v1" && obj.GetKind() == "ConfigMap":
		return configMapGVR, nil
	case gv.Group == "networking.k8s.io" && gv.Version == "v1" && obj.GetKind() == "Ingress":
		return ingressGVR, nil
	case gv.Group == "helm.cattle.io" && gv.Version == "v1" && obj.GetKind() == "HelmChart":
		return helmChartGVR, nil
	default:
		return schema.GroupVersionResource{}, errors.New("unsupported resource kind " + obj.GetAPIVersion() + "/" + obj.GetKind())
	}
}
