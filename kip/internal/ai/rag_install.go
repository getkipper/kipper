package ai

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RAGBundleStateConfigMapName records what the RAG bundle install
// settled on (embedding model, docs host, PVC sizes). Separate from
// Phase 1's BundleStateConfigMapName so the two bundles' states can be
// read and uninstalled independently.
const RAGBundleStateConfigMapName = "kipper-rag-bundle-state"

// Default sizes for the RAG bundle. Keep them modest at v1; the CLI in
// Step 5 will expose flags so users with bigger corpora can override.
//
//   - Qdrant PVC: 10 GiB. At 768-dim vectors and Qdrant's mmap mode,
//     this comfortably holds well over a million chunks.
//   - AnythingLLM PVC: 10 GiB. Holds raw uploaded documents, the SQLite
//     DB, and per-workspace state. Bumping is one CLI flag away.
//   - Qdrant memory: 512 MiB request / 1 GiB limit. Resident scales
//     gently with vector count.
//   - AnythingLLM memory: 2 GiB request / 3 GiB limit. The project's
//     documented minimum is 2 GiB; the limit gives headroom for agent
//     workloads without inviting OOM.
const (
	defaultRAGQdrantPVCSize          = "10Gi"
	defaultRAGAnythingLLMPVCSize     = "10Gi"
	defaultRAGQdrantMemoryRequest    = "512Mi"
	defaultRAGQdrantMemoryLimit      = "1Gi"
	defaultRAGAnythingLLMMemoryReq   = "2Gi"
	defaultRAGAnythingLLMMemoryLimit = "3Gi"
)

// RAGOptions controls an InstallRAG or UninstallRAG run.
type RAGOptions struct {
	// Host is the external hostname for the AnythingLLM Ask UI, e.g.
	// "ask.example.com". Required for InstallRAG. Ignored by
	// UninstallRAG.
	Host string
	// EmbeddingModel overrides DefaultEmbeddingModel. Optional.
	EmbeddingModel string
	// QdrantPVCSize overrides defaultRAGQdrantPVCSize. Optional.
	QdrantPVCSize string
	// AnythingLLMPVCSize overrides defaultRAGAnythingLLMPVCSize.
	// Optional.
	AnythingLLMPVCSize string
}

// embeddingPullJobNamePrefix is the GenerateName prefix used for the
// one-shot Job that pulls the embedding model into the running Ollama.
// Each install run gets a unique server-assigned suffix, matching the
// model-probe Job pattern: this avoids the AlreadyExists race when
// retrying after a timeout (Kubernetes deletion is async; TTL cleanup
// also has lag).
const embeddingPullJobNamePrefix = "ollama-embedding-pull-"

// waitForEmbeddingModelLoaded triggers a pull of the named embedding
// model on the running Ollama Deployment and waits for the model to
// appear in /api/tags. This is the gate the user actually cares about:
// AnythingLLM cannot embed documents until the model is reachable.
//
// Phase 1 preloads the chat model via the Ollama chart's models.pull
// values; Phase 2 cannot reuse that path because the Ollama HelmChart
// is already applied with a single chat model. We POST to /api/pull
// against the running Ollama instead, which streams the download and
// returns 200 when the model is ready.
func (i *Installer) waitForEmbeddingModelLoaded(ctx context.Context, model string) error {
	if model == "" {
		return fmt.Errorf("embedding model is required")
	}

	job := buildEmbeddingPullJob(model)
	created, err := i.Clientset.BatchV1().Jobs(Namespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("creating embedding pull job: %w", err)
	}
	jobName := created.GetName()

	deadline := time.Now().Add(i.timeout())
	for {
		got, err := i.Clientset.BatchV1().Jobs(Namespace).Get(ctx, jobName, metav1.GetOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("reading embedding pull job: %w", err)
		}
		if got != nil {
			if got.Status.Succeeded > 0 {
				return nil
			}
			if got.Status.Failed > 0 {
				return fmt.Errorf(
					"embedding model %q did not load; check 'kubectl logs -n %s job/%s' and 'kubectl logs -n %s deployment/ollama'",
					model, Namespace, jobName, Namespace,
				)
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf(
				"embedding model %q not ready within %s; check 'kubectl logs -n %s deployment/ollama' for pull progress",
				model, i.timeout(), Namespace,
			)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(i.interval()):
		}
	}
}

// buildEmbeddingPullJob constructs the Job that POSTs to Ollama's
// /api/pull endpoint to download the embedding model, then verifies
// the model appears in /api/tags. Same shape as buildModelProbeJob:
// busybox image, no chart-controller retry (script polls itself),
// 30-minute in-pod deadline so a stuck pull cannot hang the install
// indefinitely, and TTL cleanup so successful Jobs are reaped.
//
// The pull request body is `{"model": "<model>", "stream": false}`,
// per ollama/ollama/docs/api.md "Pull a Model": the parameter is
// `model`, not `name`, per the current Ollama spec.
// `stream: false` returns a single JSON object instead of NDJSON,
// which busybox wget handles cleanly.
func buildEmbeddingPullJob(model string) *batchv1.Job {
	noBackoff := int32(0)
	ttl := int32(60)
	deadline := int64(1800) // 30 minutes upper bound on the in-pod loop.

	script := fmt.Sprintf(`set -eu
TARGET="%s"
URL_BASE="http://ollama.%s.svc.cluster.local:11434"

echo "pulling $TARGET"
if ! wget -q -O- --post-data="{\"model\":\"$TARGET\",\"stream\":false}" --header="Content-Type: application/json" "$URL_BASE/api/pull" >/dev/null; then
  echo "pull request failed for $TARGET" >&2
  exit 1
fi

# /api/pull returns 200 once the model is downloaded and registered.
# Verify it appears in /api/tags as a belt-and-braces check; on a slow
# disk the index refresh can lag the response by a few seconds.
for i in $(seq 1 12); do
  if wget -q -O- "$URL_BASE/api/tags" 2>/dev/null | grep -q "$TARGET"; then
    echo "embedding model $TARGET ready"
    exit 0
  fi
  sleep 5
done
echo "pull reported success but $TARGET did not appear in /api/tags" >&2
exit 1
`, model, Namespace)

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: embeddingPullJobNamePrefix,
			Namespace:    Namespace,
			Labels: map[string]string{
				managedByLabel:                managedByValue,
				"app.kubernetes.io/part-of":   "kipper-ai",
				"app.kubernetes.io/component": componentValueRAG,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &noBackoff,
			TTLSecondsAfterFinished: &ttl,
			ActiveDeadlineSeconds:   &deadline,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						managedByLabel:                managedByValue,
						"app.kubernetes.io/component": componentValueRAG,
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:    "pull",
						Image:   "busybox:1.36",
						Command: []string{"sh", "-c", script},
					}},
				},
			},
		},
	}
}

// InstallRAG applies the Phase 2 RAG bundle (Qdrant + AnythingLLM)
// against a cluster that already runs Phase 1 (Ollama + LibreChat).
// The flow:
//
//  1. Preflight (DetectRAG): refuses if Phase 1 is missing or the
//     target node lacks RAG headroom.
//  2. Read Phase 1's chat model from the bundle state ConfigMap so
//     AnythingLLM defaults to the same model the operator already
//     trusts.
//  3. Load or generate the AnythingLLM credentials Secret. Re-installs
//     reuse existing values to avoid invalidating logged-in sessions
//     and rotating the bootstrap AUTH_TOKEN out from under the
//     operator.
//  4. Apply the Qdrant and AnythingLLM HelmCharts. Both kicked off
//     before either wait blocks so chart pulls run in parallel.
//  5. Wait for chart-controller Jobs, then for Qdrant (StatefulSet)
//     and AnythingLLM (Deployment) to be Ready.
//  6. Pull the embedding model into the running Ollama via the
//     waitForEmbeddingModelLoaded Job.
//  7. Record RAG bundle state.
func (i *Installer) InstallRAG(ctx context.Context, opts RAGOptions) error {
	if opts.Host == "" {
		return fmt.Errorf("host is required: pass --host ask.<your-domain>")
	}

	report, err := DetectRAG(ctx, i.Clientset)
	if err != nil {
		return fmt.Errorf("RAG preflight: %w", err)
	}
	if !report.Phase1Installed {
		return fmt.Errorf("phase 1 not ready: %s", report.Phase1Reason)
	}
	// Skip the headroom check on re-applies. DetectRAG counts every
	// running pod's memory request against the node's free memory,
	// including Qdrant and AnythingLLM themselves. On a tight node
	// that just barely passed the first install, an idempotent
	// re-apply would now fail with the bundle's own footprint
	// counted against itself. Mirrors the Phase 1 fix that skips the
	// tier-refusal gate when the kipper-ai namespace already
	// belongs to Kipper.
	alreadyInstalled, err := i.ragInstalled(ctx)
	if err != nil {
		return err
	}
	if !alreadyInstalled && !report.HeadroomSufficient {
		return fmt.Errorf(
			"insufficient memory headroom on node %q: have %d GiB free, need %d GiB",
			report.TargetNode,
			report.TargetFreeMemory/(1024*1024*1024),
			report.RequiredHeadroomBytes/(1024*1024*1024),
		)
	}

	chatModel, err := i.readPhase1ChatModel(ctx)
	if err != nil {
		return fmt.Errorf("reading Phase 1 chat model: %w", err)
	}
	if chatModel == "" {
		return fmt.Errorf(
			"phase 1 bundle state has no chat model recorded; re-run 'kip ai install' to refresh it before installing the RAG bundle",
		)
	}

	creds, err := i.loadOrGenerateAnythingLLMCredentials(ctx)
	if err != nil {
		return fmt.Errorf("preparing AnythingLLM credentials: %w", err)
	}

	embeddingModel := opts.EmbeddingModel
	if embeddingModel == "" {
		embeddingModel = DefaultEmbeddingModel
	}
	qdrantPVC := opts.QdrantPVCSize
	if qdrantPVC == "" {
		qdrantPVC = defaultRAGQdrantPVCSize
	}
	anyllmPVC := opts.AnythingLLMPVCSize
	if anyllmPVC == "" {
		anyllmPVC = defaultRAGAnythingLLMPVCSize
	}

	var qdrantState, anyllmState helmApplyState

	steps := []struct {
		name string
		fn   func() error
	}{
		{"Installing Qdrant", func() error {
			state, err := i.applyHelmChart(ctx, QdrantManifest(QdrantConfig{
				PVCSize:       qdrantPVC,
				MemoryRequest: defaultRAGQdrantMemoryRequest,
				MemoryLimit:   defaultRAGQdrantMemoryLimit,
				NodeName:      report.TargetNode,
			}), "qdrant")
			qdrantState = state
			return err
		}},
		{"Installing AnythingLLM", func() error {
			state, err := i.applyHelmChart(ctx, AnythingLLMManifest(AnythingLLMConfig{
				Host:           opts.Host,
				NodeName:       report.TargetNode,
				OllamaModel:    chatModel,
				EmbeddingModel: embeddingModel,
				PVCSize:        anyllmPVC,
				MemoryRequest:  defaultRAGAnythingLLMMemoryReq,
				MemoryLimit:    defaultRAGAnythingLLMMemoryLimit,
				Credentials:    creds,
			}), "anythingllm")
			anyllmState = state
			return err
		}},
		{"Waiting for Qdrant chart job", func() error { return i.waitForHelmChartJobFromState(ctx, qdrantState) }},
		{"Waiting for Qdrant to be ready", func() error { return i.waitForStatefulSet(ctx, Namespace, QdrantServiceName) }},
		{"Waiting for AnythingLLM chart job", func() error { return i.waitForHelmChartJobFromState(ctx, anyllmState) }},
		{"Waiting for AnythingLLM to be ready", func() error { return i.waitForDeployment(ctx, AnythingLLMDeploymentName) }},
		{"Pulling embedding model into Ollama", func() error { return i.waitForEmbeddingModelLoaded(ctx, embeddingModel) }},
		{"Recording RAG bundle state", func() error { return i.writeRAGBundleState(ctx, embeddingModel, opts.Host) }},
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

// UninstallRAG removes only Phase 2 resources, leaving Phase 1 (Ollama
// + LibreChat) running. Order matters: HelmChart CRs first so
// helm-controller stops reconciling and tears down chart-rendered
// resources, then the Kipper-applied Ingress and Secret. The
// embedding model in Ollama's PVC is intentionally left in place; a
// re-install reuses it without re-downloading.
func (i *Installer) UninstallRAG(ctx context.Context, _ RAGOptions) error {
	steps := []struct {
		name string
		fn   func() error
	}{
		{"Removing AnythingLLM", func() error { return i.deleteHelmChart(ctx, "anythingllm") }},
		{"Removing Qdrant", func() error { return i.deleteHelmChart(ctx, "qdrant") }},
		{"Removing AnythingLLM Ingress", func() error { return i.deleteOwnedIngress(ctx, Namespace, "anythingllm") }},
		{"Removing AnythingLLM credentials", func() error {
			return i.deleteOwnedSecret(ctx, AnythingLLMCredentialsSecretName)
		}},
		{"Removing RAG bundle state", func() error {
			return i.deleteOwnedConfigMap(ctx, Namespace, RAGBundleStateConfigMapName)
		}},
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

// ragInstalled reports whether the Kipper-owned RAG bundle has any
// visible HelmChart CR in kube-system. Used by Phase 1's Uninstall to
// refuse rather than silently destroy the namespace that Phase 2 still
// depends on, and by InstallRAG to skip the headroom check on
// re-applies.
//
// Charts named qdrant or anythingllm that lack the Kipper managed-by
// label do not count: a foreign chart with the same name belongs to
// someone else, and treating it as ours would block the user without
// giving them a path out (deleteHelmChart refuses foreign charts too).
func (i *Installer) ragInstalled(ctx context.Context) (bool, error) {
	chartClient := i.Dynamic.Resource(helmChartGVR).Namespace("kube-system")
	for _, name := range []string{"qdrant", "anythingllm"} {
		got, err := chartClient.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("checking for %s helmchart: %w", name, err)
		}
		if isManagedByKipper(got.GetLabels()) {
			return true, nil
		}
	}
	return false, nil
}

// readPhase1ChatModel reads the chat model recorded in Phase 1's
// bundle state ConfigMap. The empty string (no error) means the
// ConfigMap is missing; the caller treats that as "Phase 1 has not
// been fully installed".
func (i *Installer) readPhase1ChatModel(ctx context.Context) (string, error) {
	cm, err := i.Clientset.CoreV1().ConfigMaps(Namespace).Get(ctx, BundleStateConfigMapName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading bundle state configmap: %w", err)
	}
	return cm.Data["model"], nil
}

// writeRAGBundleState records the embedding model and docs host in a
// Kipper-owned ConfigMap separate from Phase 1's bundle state. Same
// idempotent semantics as writeBundleState: create if missing, update
// in place if owned, refuse if a foreign ConfigMap of the same name
// exists.
func (i *Installer) writeRAGBundleState(ctx context.Context, embeddingModel, host string) error {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      RAGBundleStateConfigMapName,
			Namespace: Namespace,
			Labels: map[string]string{
				managedByLabel:                managedByValue,
				"app.kubernetes.io/part-of":   "kipper-ai",
				"app.kubernetes.io/component": componentValueRAG,
			},
		},
		Data: map[string]string{
			"embedding_model": embeddingModel,
			"host":            host,
		},
	}
	cms := i.Clientset.CoreV1().ConfigMaps(Namespace)
	existing, err := cms.Get(ctx, RAGBundleStateConfigMapName, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		if _, err := cms.Create(ctx, cm, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("creating RAG bundle state configmap: %w", err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("reading RAG bundle state configmap: %w", err)
	}
	if !isManagedByKipper(existing.Labels) {
		return &errNotOwnedByKipper{kind: "ConfigMap", name: RAGBundleStateConfigMapName}
	}
	cm.ResourceVersion = existing.ResourceVersion
	if _, err := cms.Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("updating RAG bundle state configmap: %w", err)
	}
	return nil
}

// AnythingLLMCredentials reads the persisted credentials Secret and
// returns the four values. Pure read: never generates fresh values.
// Used by the CLI to display the AUTH_TOKEN after install so the
// operator can complete the bootstrap login on the Ask UI.
//
// Returns an error wrapped with the standard NotFound shape when the
// Secret is missing, and the ownership-refusal error when the Secret
// exists without Kipper's managed-by label.
func (i *Installer) AnythingLLMCredentials(ctx context.Context) (AnythingLLMCredentials, error) {
	got, err := i.Clientset.CoreV1().Secrets(Namespace).Get(ctx, AnythingLLMCredentialsSecretName, metav1.GetOptions{})
	if err != nil {
		return AnythingLLMCredentials{}, err
	}
	if !isManagedByKipper(got.Labels) {
		return AnythingLLMCredentials{}, &errNotOwnedByKipper{kind: "Secret", name: AnythingLLMCredentialsSecretName}
	}
	return AnythingLLMCredentials{
		JWTSecret: string(got.Data["JWT_SECRET"]),
		SigKey:    string(got.Data["SIG_KEY"]),
		SigSalt:   string(got.Data["SIG_SALT"]),
		AuthToken: string(got.Data["AUTH_TOKEN"]),
	}, nil
}

// loadOrGenerateAnythingLLMCredentials returns the four credentials
// (JWT_SECRET, SIG_KEY, SIG_SALT, AUTH_TOKEN) carried by the existing
// Secret if it is Kipper-owned and complete; otherwise generates a
// fresh set. Re-applies preserve existing values rather than rotating
// them out from under running pods, which would invalidate session
// JWTs and the bootstrap AUTH_TOKEN the operator is using.
func (i *Installer) loadOrGenerateAnythingLLMCredentials(ctx context.Context) (AnythingLLMCredentials, error) {
	got, err := i.Clientset.CoreV1().Secrets(Namespace).Get(ctx, AnythingLLMCredentialsSecretName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return generateAnythingLLMCredentials()
	}
	if err != nil {
		return AnythingLLMCredentials{}, fmt.Errorf("reading existing AnythingLLM credentials: %w", err)
	}
	if !isManagedByKipper(got.Labels) {
		return AnythingLLMCredentials{}, &errNotOwnedByKipper{kind: "Secret", name: AnythingLLMCredentialsSecretName}
	}
	creds := AnythingLLMCredentials{
		JWTSecret: string(got.Data["JWT_SECRET"]),
		SigKey:    string(got.Data["SIG_KEY"]),
		SigSalt:   string(got.Data["SIG_SALT"]),
		AuthToken: string(got.Data["AUTH_TOKEN"]),
	}
	if creds.JWTSecret == "" || creds.SigKey == "" || creds.SigSalt == "" || creds.AuthToken == "" {
		// Existing Secret is missing one of the four values — treat
		// it as corrupt and start fresh. Better to roll the pods once
		// than to ship a half-populated credential set.
		return generateAnythingLLMCredentials()
	}
	return creds, nil
}

// generateAnythingLLMCredentials produces the four random values
// AnythingLLM needs at boot. Lengths satisfy the documented minimums:
// JWT_SECRET >= 12 chars, SIG_KEY/SIG_SALT >= 32 chars. AUTH_TOKEN has
// no documented minimum; 32 hex chars (128 bits of entropy) is well
// outside any realistic brute-force window.
func generateAnythingLLMCredentials() (AnythingLLMCredentials, error) {
	jwtSecret, err := randomHex(16) // 32 hex chars, well above the 12-char minimum
	if err != nil {
		return AnythingLLMCredentials{}, err
	}
	sigKey, err := randomHex(16) // 32 hex chars
	if err != nil {
		return AnythingLLMCredentials{}, err
	}
	sigSalt, err := randomHex(16) // 32 hex chars
	if err != nil {
		return AnythingLLMCredentials{}, err
	}
	authToken, err := randomHex(16) // 32 hex chars / 128 bits of entropy
	if err != nil {
		return AnythingLLMCredentials{}, err
	}
	return AnythingLLMCredentials{
		JWTSecret: jwtSecret,
		SigKey:    sigKey,
		SigSalt:   sigSalt,
		AuthToken: authToken,
	}, nil
}

// waitForStatefulSet polls until the named StatefulSet exists and
// reports all desired replicas Ready. Mirrors waitForDeployment but
// against the StatefulSet API since the Qdrant chart renders one.
func (i *Installer) waitForStatefulSet(ctx context.Context, namespace, name string) error {
	deadline := time.Now().Add(i.timeout())
	for {
		ss, err := i.Clientset.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
		switch {
		case err == nil:
			if isStatefulSetReady(ss) {
				return nil
			}
		case apierrors.IsNotFound(err):
			// fall through to wait
		default:
			return fmt.Errorf("reading statefulset %s/%s: %w", namespace, name, err)
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("statefulset %s/%s not ready within %s", namespace, name, i.timeout())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(i.interval()):
		}
	}
}

// isStatefulSetReady returns true when the StatefulSet has rolled out
// the spec the API server has observed AND all desired replicas are
// Ready. Mirrors the rollout-aware logic in isDeploymentAvailable so a
// mid-rollout StatefulSet does not pass the wait.
func isStatefulSetReady(ss *appsv1.StatefulSet) bool {
	if ss.Status.ObservedGeneration < ss.Generation {
		return false
	}
	desired := int32(1)
	if ss.Spec.Replicas != nil {
		desired = *ss.Spec.Replicas
	}
	if ss.Status.UpdatedReplicas < desired {
		return false
	}
	if ss.Status.ReadyReplicas < desired {
		return false
	}
	return ss.Status.CurrentRevision == ss.Status.UpdateRevision
}

// deleteOwnedSecret deletes the named Secret iff it carries the
// Kipper managed-by label. Refuses to touch a foreign Secret with the
// same name; missing Secret is a no-op.
func (i *Installer) deleteOwnedSecret(ctx context.Context, name string) error {
	secrets := i.Clientset.CoreV1().Secrets(Namespace)
	existing, err := secrets.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading secret %s/%s: %w", Namespace, name, err)
	}
	if !isManagedByKipper(existing.Labels) {
		return &errNotOwnedByKipper{kind: "Secret", name: name}
	}
	if err := secrets.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting secret %s/%s: %w", Namespace, name, err)
	}
	return nil
}

// deleteOwnedIngress deletes the named Ingress iff it carries the
// Kipper managed-by label. Same ownership-gate as deleteOwnedSecret.
func (i *Installer) deleteOwnedIngress(ctx context.Context, namespace, name string) error {
	ingClient := i.Dynamic.Resource(ingressGVR).Namespace(namespace)
	existing, err := ingClient.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading ingress %s/%s: %w", namespace, name, err)
	}
	if !isManagedByKipper(existing.GetLabels()) {
		return &errNotOwnedByKipper{kind: "Ingress", name: name}
	}
	if err := ingClient.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting ingress %s/%s: %w", namespace, name, err)
	}
	return nil
}

// deleteOwnedConfigMap deletes the named ConfigMap iff it carries the
// Kipper managed-by label.
func (i *Installer) deleteOwnedConfigMap(ctx context.Context, namespace, name string) error {
	cms := i.Clientset.CoreV1().ConfigMaps(namespace)
	existing, err := cms.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading configmap %s/%s: %w", namespace, name, err)
	}
	if !isManagedByKipper(existing.Labels) {
		return &errNotOwnedByKipper{kind: "ConfigMap", name: name}
	}
	if err := cms.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting configmap %s/%s: %w", namespace, name, err)
	}
	return nil
}
