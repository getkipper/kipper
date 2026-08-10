package ai

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// VeleroNamespace is where Velero CRs live (chart default).
const VeleroNamespace = "velero"

// SystemNamespace is where Kipper installs cross-cutting Secrets and
// ConfigMaps. The AI bundle's cluster-side Secret (kipper-ai-config)
// lives here; everything else AI-related is in Namespace.
const SystemNamespace = "kipper-system"

// AIConfigSecretName is the Secret in SystemNamespace where kip's AI
// client config (provider, api_key, model, ollama_url) is mirrored
// for the Console to read.
const AIConfigSecretName = "kipper-ai-config" //nolint:gosec // kubernetes Secret object name, not a credential value

// configBackupSuffix tags the second Velero Backup that holds the
// cross-namespace AIConfigSecret. CreateBackup makes two Backups per
// logical kip operation; ListBackups hides the suffix so the user sees
// one entry per snapshot.
const configBackupSuffix = "-aiconfig"

// configSiblingAnnotation records on the primary whether a config
// sibling was created at backup time. List and restore consult it to
// distinguish "no sibling expected" from "sibling lost/orphaned".
const configSiblingAnnotation = "kipper.run/expects-config-sibling"

var (
	veleroBackupGVR        = schema.GroupVersionResource{Group: "velero.io", Version: "v1", Resource: "backups"}
	veleroRestoreGVR       = schema.GroupVersionResource{Group: "velero.io", Version: "v1", Resource: "restores"}
	veleroDeleteRequestGVR = schema.GroupVersionResource{Group: "velero.io", Version: "v1", Resource: "deletebackuprequests"}
)

// BackupSummary is a flattened view of the primary Velero Backup for
// CLI output. Sibling-config-Backup status is folded into Errors so the
// user sees a single combined result.
type BackupSummary struct {
	Name          string
	Phase         string
	CreatedAt     time.Time
	StartedAt     time.Time
	FinishedAt    time.Time
	ItemsBackedUp int64
	TotalItems    int64
	Errors        int64
	Warnings      int64
}

// StartedBackup describes the just-created Velero CRs that callers
// pass to WaitBackup or WatchBackupForWindow. The HasSibling flag
// drives whether the wait covers one or two physical backups.
type StartedBackup struct {
	Name       string
	HasSibling bool
}

// BackupRunState reports whether a backup snapshot has finished
// (cleanly or otherwise) within the caller-supplied window.
type BackupRunState int

const (
	// BackupStillRunning means at least one of the physical backups
	// is still in flight.
	BackupStillRunning BackupRunState = iota
	// BackupCompleted means every physical backup reached a clean
	// terminal phase within the window.
	BackupCompleted
	// BackupFailed means at least one physical backup ended in
	// Failed or PartiallyFailed within the window.
	BackupFailed
)

// StartBackup creates the Velero Backup CRs and returns immediately.
// All preflight (namespace ownership, name pre-flight) happens here so
// callers see immediate-validation failures synchronously. The caller decides whether to block on completion
// (via WaitBackup) or fire-and-forget with a short watch window (via
// WatchBackupForWindow).
//
// Splitting the snapshot across two Backups is the simplest way to
// back up a populated namespace plus a single cross-namespace
// resource: Velero label selectors apply to every object in the
// include scope, so a single Backup spanning both namespaces would
// force every chart-rendered resource in kipper-ai to carry a label.
func (i *Installer) StartBackup(ctx context.Context, name string) (*StartedBackup, error) {
	if name == "" {
		name = fmt.Sprintf("kipper-ai-%s", time.Now().UTC().Format("20060102-150405"))
	}
	if strings.HasSuffix(name, configBackupSuffix) {
		return nil, fmt.Errorf("backup name must not end in %q (reserved for the cross-namespace config backup)", configBackupSuffix)
	}

	if err := i.requireOwnedNamespace(ctx); err != nil {
		return nil, err
	}
	// Refuse if either physical name is taken before creating
	// anything. Otherwise a stale orphan sibling from a partially
	// failed earlier delete could end up paired with a fresh primary,
	// silently mixing snapshot generations.
	if err := i.refuseIfBackupNameTaken(ctx, name); err != nil {
		return nil, err
	}
	if err := i.refuseIfBackupNameTaken(ctx, name+configBackupSuffix); err != nil {
		return nil, err
	}

	// Sibling backup catches every cross-namespace AI artefact:
	// kipper-ai-config Secret in kipper-system, plus the
	// ollama/librechat HelmChart CRs in kube-system. Create it
	// whenever any of these exist, not only when the Secret is
	// present — a user who declined install-time auto-config still
	// has the HelmCharts to back up.
	hasSiblingArtefacts, err := i.crossNamespaceArtefactsExist(ctx)
	if err != nil {
		return nil, err
	}
	annotations := map[string]string{
		configSiblingAnnotation: "false",
	}
	if hasSiblingArtefacts {
		annotations[configSiblingAnnotation] = "true"
	}

	if err := i.createVeleroBackup(ctx, name, primaryBackupSpec(), annotations); err != nil {
		return nil, err
	}
	if hasSiblingArtefacts {
		if err := i.createVeleroBackup(ctx, name+configBackupSuffix, configBackupSpec(), nil); err != nil {
			return nil, err
		}
	}
	return &StartedBackup{Name: name, HasSibling: hasSiblingArtefacts}, nil
}

// WaitBackup blocks until the started backup pair reaches a terminal
// phase, returning the aggregated summary. Times out per the
// installer's PollTimeout (30 minutes by default). Use this for
// scripted/CI flows that need to know the backup actually completed
// before chaining the next operation.
func (i *Installer) WaitBackup(ctx context.Context, started *StartedBackup) (*BackupSummary, error) {
	primary, err := i.waitForBackup(ctx, started.Name)
	if err != nil {
		return primary, err
	}
	if started.HasSibling {
		sibling, err := i.waitForBackup(ctx, started.Name+configBackupSuffix)
		if err != nil {
			return primary, err
		}
		primary.Errors += sibling.Errors
		primary.Warnings += sibling.Warnings
		primary.ItemsBackedUp += sibling.ItemsBackedUp
	}
	return primary, nil
}

// WatchBackupForWindow polls the started backup pair for the given
// window and returns the run state plus the most recent aggregated
// summary. Used by the default (async) `kip ai backup` flow so the
// CLI can surface immediate-validation failures (bad name, BSL down,
// RBAC) before exiting and pointing the user at `kip ai backup show`.
//
// Returns BackupCompleted only when every expected component reached
// Completed AND each is healthy by `backupUnhealthyReason` (non-zero
// items, no recorded errors). BackupFailed if any reached a hard
// terminal failure or a Completed-but-unhealthy state in the window.
// BackupStillRunning otherwise — the caller should treat that as
// "submitted, check progress later". The same predicate as the
// blocking path (`waitForBackup`) and the restore precondition
// (`requireCleanBackup`) is used here so the warmup window cannot
// declare success on a snapshot that those paths would later refuse.
func (i *Installer) WatchBackupForWindow(ctx context.Context, started *StartedBackup, window time.Duration) (BackupRunState, *BackupSummary, error) {
	deadline := time.Now().Add(window)
	for {
		primary, sibling, err := i.fetchBackupSummaries(ctx, started)
		if err != nil {
			return BackupStillRunning, nil, err
		}
		summary := mergeBackupSummaries(primary, sibling)

		// Hard terminal failure (Failed/PartiallyFailed/FailedValidation)
		// on either component.
		if isTerminalFailed(primary) || isTerminalFailed(sibling) {
			return BackupFailed, summary, nil
		}

		// Per-component healthiness: as soon as one component reaches
		// Completed, check its items/errors against the same predicate
		// as the blocking path. Without this early check, a primary
		// that finished Completed-but-unhealthy at 5s while the sibling
		// is still uploading would only be evaluated after both are
		// done — and if the sibling outlasts the warmup, the watcher
		// would exit BackupStillRunning over an already-broken snapshot.
		if isCompleted(primary) {
			if reason := backupUnhealthyReason(primary); reason != "" {
				return BackupFailed, summary, nil
			}
		}
		siblingExpected := started.HasSibling
		if siblingExpected && isCompleted(sibling) {
			if reason := backupUnhealthyReason(sibling); reason != "" {
				return BackupFailed, summary, nil
			}
		}

		// Both components present and Completed and healthy.
		primaryDone := isCompleted(primary)
		siblingDone := !siblingExpected || isCompleted(sibling)
		if primaryDone && siblingDone {
			return BackupCompleted, summary, nil
		}

		if time.Now().After(deadline) {
			return BackupStillRunning, summary, nil
		}
		select {
		case <-ctx.Done():
			return BackupStillRunning, summary, ctx.Err()
		case <-time.After(i.interval()):
		}
	}
}

// BackupStatus is the detailed view returned by `kip ai backup show`.
// Tracks the two physical backups separately so the CLI can render
// each component, and carries ExpectsSibling so a missing config
// sibling can be flagged as incomplete rather than presented as
// "intentionally absent".
type BackupStatus struct {
	Name           string
	Primary        *BackupSummary
	Sibling        *BackupSummary
	ExpectsSibling bool
}

// Restorable returns the empty string if the snapshot is safe to
// restore from (and therefore safe to delete the bundle's source PVCs
// against), or a human-readable reason otherwise. Used by `kip ai
// backup show` so its verdict matches restore/list — without this,
// an operator could see "Completed" in show, uninstall, then have
// restore refuse the snapshot as unhealthy.
//
// A present sibling is validated unconditionally: the
// expects-config-sibling annotation only governs the *missing* case
// (so an older snapshot without the annotation can't sneak past with
// a broken sibling, and restore/list/show all agree).
func (s *BackupStatus) Restorable() string {
	if s.Primary == nil {
		return "primary backup missing: namespace data was not preserved"
	}
	if s.Primary.Phase != "Completed" {
		return fmt.Sprintf("primary in phase %s", s.Primary.Phase)
	}
	if reason := backupUnhealthyReason(s.Primary); reason != "" {
		return "primary " + reason
	}
	if s.Sibling != nil {
		if s.Sibling.Phase != "Completed" {
			return fmt.Sprintf("config sibling in phase %s", s.Sibling.Phase)
		}
		if reason := backupUnhealthyReason(s.Sibling); reason != "" {
			return "config sibling " + reason
		}
	} else if s.ExpectsSibling {
		return "config sibling expected but missing: cluster Secret + HelmCharts will not be restored"
	}
	return ""
}

// GetBackupStatus returns the current primary + (optional) sibling
// summary for a logical snapshot name. Powers `kip ai backup show`.
// Both components are validated through findOwnedBackup, so a
// Kipper-managed Velero backup that isn't part of the AI bundle (or
// a foreign backup with a colliding name) is rejected the same way
// list/delete/restore reject it. ExpectsSibling reflects the
// `kipper.run/expects-config-sibling` annotation on the primary so
// the CLI can show a missing-but-expected sibling as incomplete.
func (i *Installer) GetBackupStatus(ctx context.Context, name string) (*BackupStatus, error) {
	if strings.HasSuffix(name, configBackupSuffix) {
		return nil, fmt.Errorf("%q is the physical sibling of a snapshot; pass the logical primary name (without the %q suffix)", name, configBackupSuffix)
	}
	primaryObj, err := i.findOwnedBackup(ctx, name)
	if err != nil {
		return nil, err
	}
	siblingObj, err := i.findOwnedBackup(ctx, name+configBackupSuffix)
	if err != nil {
		return nil, err
	}
	if primaryObj == nil && siblingObj == nil {
		return nil, fmt.Errorf("backup %q not found", name)
	}
	out := &BackupStatus{Name: name}
	if primaryObj != nil {
		out.Primary = summariseBackup(primaryObj)
		out.ExpectsSibling = primaryObj.GetAnnotations()[configSiblingAnnotation] == "true"
	}
	if siblingObj != nil {
		out.Sibling = summariseBackup(siblingObj)
	}
	return out, nil
}

// InFlightBackupNames returns the logical names of Kipper AI backups
// that have not yet reached a terminal phase. Used by `kip ai
// uninstall` to refuse a destructive operation while Velero is still
// copying the source PVCs — the user-visible backup → uninstall
// sequence under the new async default would otherwise race and
// leave an unrestorable snapshot. Both physical components (primary
// and `-aiconfig` sibling) are inspected so an in-flight sibling is
// also caught.
func (i *Installer) InFlightBackupNames(ctx context.Context) ([]string, error) {
	list, err := i.Dynamic.Resource(veleroBackupGVR).Namespace(VeleroNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: managedByLabel + "=" + managedByValue + ",app.kubernetes.io/part-of=kipper-ai",
	})
	if err != nil {
		// Velero genuinely not installed → nothing to wait for.
		// `isCRDNotFound` would also swallow `apierrors.IsServerTimeout`,
		// but a transient apiserver hiccup must NOT make this guard
		// fail open: returning empty would let `kip ai uninstall`
		// race an in-flight async backup. Fail closed instead.
		if isNoKindMatch(err) || apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("listing backups: %w", err)
	}
	seen := map[string]struct{}{}
	var inFlight []string
	for idx := range list.Items {
		summary := summariseBackup(&list.Items[idx])
		if summary.Phase == "Completed" || isTerminalFailed(summary) {
			continue
		}
		// Map the physical backup back to its logical name (drop the
		// -aiconfig suffix) so the caller sees one entry per logical
		// snapshot.
		logical := strings.TrimSuffix(summary.Name, configBackupSuffix)
		if _, ok := seen[logical]; ok {
			continue
		}
		seen[logical] = struct{}{}
		inFlight = append(inFlight, logical)
	}
	return inFlight, nil
}

// CreateBackup is the synchronous convenience wrapper: start the
// snapshot, then block until it finishes. Used by `kip ai backup
// --wait` and existing tests that drive the full lifecycle in one
// call.
func (i *Installer) CreateBackup(ctx context.Context, name string) (*BackupSummary, error) {
	started, err := i.StartBackup(ctx, name)
	if err != nil {
		return nil, err
	}
	return i.WaitBackup(ctx, started)
}

// fetchBackupSummaries reads the primary + (optional) sibling Backup
// CRs and returns their summaries. A missing CR for either is treated
// as nil so the caller can distinguish "not yet visible to the
// apiserver" from "no sibling expected".
func (i *Installer) fetchBackupSummaries(ctx context.Context, started *StartedBackup) (primary, sibling *BackupSummary, err error) {
	primaryObj, getErr := i.Dynamic.Resource(veleroBackupGVR).Namespace(VeleroNamespace).Get(ctx, started.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(getErr):
		// New backup, controller hasn't materialised status yet — caller polls again.
	case getErr != nil:
		return nil, nil, fmt.Errorf("reading backup %q: %w", started.Name, getErr)
	default:
		primary = summariseBackup(primaryObj)
	}
	if !started.HasSibling {
		return primary, nil, nil
	}
	siblingObj, getErr := i.Dynamic.Resource(veleroBackupGVR).Namespace(VeleroNamespace).Get(ctx, started.Name+configBackupSuffix, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(getErr):
		// fall through — sibling not yet visible
	case getErr != nil:
		return primary, nil, fmt.Errorf("reading backup %q: %w", started.Name+configBackupSuffix, getErr)
	default:
		sibling = summariseBackup(siblingObj)
	}
	return primary, sibling, nil
}

func mergeBackupSummaries(primary, sibling *BackupSummary) *BackupSummary {
	if primary == nil {
		return sibling
	}
	out := *primary
	if sibling != nil {
		out.Errors += sibling.Errors
		out.Warnings += sibling.Warnings
		out.ItemsBackedUp += sibling.ItemsBackedUp
	}
	return &out
}

func isCompleted(s *BackupSummary) bool {
	return s != nil && s.Phase == "Completed"
}

// isTerminalFailed reports whether the Velero Backup phase is a hard
// terminal failure. FailedValidation lives here too: Velero uses it
// for issues that prevent the backup from running at all (missing
// BackupStorageLocation, RBAC, malformed spec). Without classifying
// it as terminal, the async warmup would treat a permanently broken
// backup as "still running" and the blocking --wait path would burn
// the full 30-minute timeout.
func isTerminalFailed(s *BackupSummary) bool {
	if s == nil {
		return false
	}
	switch s.Phase {
	case "Failed", "PartiallyFailed", "FailedValidation":
		return true
	}
	return false
}

func primaryBackupSpec() map[string]interface{} {
	// defaultVolumesToFsBackup must be true so Velero captures the
	// model cache, MongoDB, and Meilisearch PVC contents via
	// filesystem backup. The Kipper Velero install already sets this
	// cluster-wide, but pinning it on the Backup itself protects
	// against drift if someone reconfigures Velero — the AI backup
	// promise is only meaningful with PVC data, not just the K8s
	// objects.
	//
	// Limitation: this is a live filesystem snapshot, not an
	// application-consistent dump. MongoDB / Meilisearch may have
	// in-flight writes when Velero walks the volume, so the very last
	// chat messages can be inconsistent on restore. Documented in
	// docs/en/ai.md; application-consistent backup via Velero
	// pre/post hooks is tracked as a follow-up.
	return map[string]interface{}{
		"includedNamespaces":       []interface{}{Namespace},
		"defaultVolumesToFsBackup": true,
	}
}

func configBackupSpec() map[string]interface{} {
	// The sibling backup catches every cross-namespace AI artefact:
	//   - kipper-system/kipper-ai-config Secret (kip's AI client config)
	//   - kube-system/{ollama,librechat} HelmChart CRs (helm-controller
	//     inputs that reconcile the bundle's workloads)
	// Both label selectors are required so a Secret or HelmChart merely
	// labelled `part-of=kipper-ai` (no Kipper ownership) is excluded.
	// Without the HelmChart CRs, a restored bundle works but is invisible
	// to helm-controller, breaking later upgrade/uninstall flows.
	return map[string]interface{}{
		"includedNamespaces": []interface{}{SystemNamespace, "kube-system"},
		"includedResources":  []interface{}{"secrets", "helmcharts.helm.cattle.io"},
		"labelSelector": map[string]interface{}{
			"matchLabels": map[string]interface{}{
				managedByLabel:              managedByValue,
				"app.kubernetes.io/part-of": "kipper-ai",
			},
		},
	}
}

// refuseIfBackupNameTaken errors when a Backup CR with the given name
// already exists in the velero namespace. Used as a pre-flight on
// CreateBackup so a stale orphan sibling from a partially failed
// earlier delete cannot be paired with a fresh primary.
func (i *Installer) refuseIfBackupNameTaken(ctx context.Context, name string) error {
	_, err := i.Dynamic.Resource(veleroBackupGVR).Namespace(VeleroNamespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("checking backup name %q: %w", name, err)
	}
	logical := strings.TrimSuffix(name, configBackupSuffix)
	return fmt.Errorf("backup %q already exists; choose a different name or run 'kip ai backup delete --name %s' first", name, logical)
}

func (i *Installer) createVeleroBackup(ctx context.Context, name string, spec map[string]interface{}, annotations map[string]string) error {
	meta := map[string]interface{}{
		"name":      name,
		"namespace": VeleroNamespace,
		"labels": map[string]interface{}{
			managedByLabel:              managedByValue,
			"app.kubernetes.io/part-of": "kipper-ai",
		},
	}
	if len(annotations) > 0 {
		ann := make(map[string]interface{}, len(annotations))
		for k, v := range annotations {
			ann[k] = v
		}
		meta["annotations"] = ann
	}
	backup := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "velero.io/v1",
		"kind":       "Backup",
		"metadata":   meta,
		"spec":       spec,
	}}
	if _, err := i.Dynamic.Resource(veleroBackupGVR).Namespace(VeleroNamespace).Create(ctx, backup, metav1.CreateOptions{}); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("backup %q already exists; choose a different name or run 'kip ai backup delete --name %s' first", name, strings.TrimSuffix(name, configBackupSuffix))
		}
		if isCRDNotFound(err) {
			return fmt.Errorf("velero CRDs not present on this cluster; install Kipper system components first")
		}
		return fmt.Errorf("creating backup %s: %w", name, err)
	}
	return nil
}

// crossNamespaceArtefactsExist returns true when at least one
// Kipper-owned cross-namespace AI artefact is on the cluster: the
// kipper-ai-config Secret in kipper-system, or one of the
// ollama/librechat HelmChart CRs in kube-system. Used to decide
// whether the sibling backup should be created.
func (i *Installer) crossNamespaceArtefactsExist(ctx context.Context) (bool, error) {
	secret, err := i.Clientset.CoreV1().Secrets(SystemNamespace).Get(ctx, AIConfigSecretName, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		// fall through to the HelmChart check
	case err != nil:
		return false, fmt.Errorf("checking %s/%s: %w", SystemNamespace, AIConfigSecretName, err)
	default:
		if isManagedByKipper(secret.Labels) {
			return true, nil
		}
	}

	for _, name := range []string{"ollama", "librechat"} {
		got, err := i.Dynamic.Resource(helmChartGVR).Namespace("kube-system").Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("checking helmchart kube-system/%s: %w", name, err)
		}
		if isManagedByKipper(got.GetLabels()) {
			return true, nil
		}
	}
	return false, nil
}

// waitForBackup polls until the named Backup reaches a terminal phase.
// Returns the summary on Completed; an error on Failed / PartiallyFailed
// that includes the recorded error count.
func (i *Installer) waitForBackup(ctx context.Context, name string) (*BackupSummary, error) {
	deadline := time.Now().Add(i.timeout())
	for {
		got, err := i.Dynamic.Resource(veleroBackupGVR).Namespace(VeleroNamespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("reading backup %q: %w", name, err)
		}
		summary := summariseBackup(got)
		switch {
		case summary.Phase == "Completed":
			// Apply the shared healthiness predicate (zero items, or
			// non-zero errors → unhealthy) so the blocking --wait
			// path agrees with WatchBackupForWindow, ListBackups, and
			// requireCleanBackup. Any of those surfaces would later
			// reject a Completed-but-unhealthy snapshot, so
			// pretending it succeeded here is a dangerous split for
			// scripts that use --wait as their safety gate.
			if reason := backupUnhealthyReason(summary); reason != "" {
				return summary, fmt.Errorf("backup %q is not restorable: %s", name, reason)
			}
			return summary, nil
		case isTerminalFailed(summary):
			return summary, fmt.Errorf("backup %q ended in phase %s with %d errors", name, summary.Phase, summary.Errors)
		}
		if time.Now().After(deadline) {
			return summary, fmt.Errorf("backup %q did not finish within %s (current phase: %s)", name, i.timeout(), summary.Phase)
		}
		select {
		case <-ctx.Done():
			return summary, ctx.Err()
		case <-time.After(i.interval()):
		}
	}
}

// ListBackups returns the primary Kipper-owned Velero Backups in the
// velero namespace, sorted newest first. Each entry folds the
// matching `-aiconfig` sibling's status into the logical view, so a
// failed config backup never shows up as healthy because the primary
// happened to complete.
func (i *Installer) ListBackups(ctx context.Context) ([]BackupSummary, error) {
	list, err := i.Dynamic.Resource(veleroBackupGVR).Namespace(VeleroNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: managedByLabel + "=" + managedByValue + ",app.kubernetes.io/part-of=kipper-ai",
	})
	if err != nil {
		if isCRDNotFound(err) {
			return nil, fmt.Errorf("velero CRDs not present on this cluster")
		}
		return nil, fmt.Errorf("listing backups: %w", err)
	}

	primaries := map[string]*unstructured.Unstructured{}
	siblings := map[string]*BackupSummary{}
	siblingObjs := map[string]*unstructured.Unstructured{}
	for idx := range list.Items {
		item := &list.Items[idx]
		name := item.GetName()
		if strings.HasSuffix(name, configBackupSuffix) {
			logical := strings.TrimSuffix(name, configBackupSuffix)
			siblings[logical] = summariseBackup(item)
			siblingObjs[logical] = item
		} else {
			primaries[name] = item
		}
	}

	out := make([]BackupSummary, 0, len(list.Items))
	for name, primaryObj := range primaries {
		primarySummary := summariseBackup(primaryObj)
		summary := *primarySummary
		expectsSibling := primaryObj.GetAnnotations()[configSiblingAnnotation] == "true"
		sib, hasSibling := siblings[name]
		switch {
		case hasSibling:
			summary.Errors += sib.Errors
			summary.Warnings += sib.Warnings
			summary.ItemsBackedUp += sib.ItemsBackedUp
			summary.Phase = listingPhase(primarySummary, sib)
		case expectsSibling:
			// Sibling expected but missing: a partial snapshot the
			// user cannot restore from cleanly.
			summary.Phase = "PartiallyFailed"
		default:
			summary.Phase = listingPhase(primarySummary, nil)
		}
		out = append(out, summary)
	}
	// Surface orphan siblings (sibling present without a matching
	// primary) so a partially failed delete cannot hide a snapshot
	// that still contains credentials in object storage. The orphan is
	// presented under its physical name and flagged PartiallyFailed.
	for logical, sib := range siblings {
		if _, ok := primaries[logical]; ok {
			continue
		}
		summary := *sib
		summary.Name = siblingObjs[logical].GetName()
		summary.Phase = "PartiallyFailed"
		out = append(out, summary)
	}
	for a := 0; a < len(out); a++ {
		for b := a + 1; b < len(out); b++ {
			if out[b].CreatedAt.After(out[a].CreatedAt) {
				out[a], out[b] = out[b], out[a]
			}
		}
	}
	return out, nil
}

// backupUnhealthyReason returns an empty string when a Backup summary
// passes every health check (phase Completed, no errors, at least one
// item captured), or a human-readable reason otherwise. Used by both
// `kip ai restore` (to refuse unhealthy sources) and `kip ai backup
// list` (to surface unhealthy snapshots) so the two views never
// disagree.
func backupUnhealthyReason(s *BackupSummary) string {
	switch s.Phase {
	case "Completed":
		if s.Errors > 0 {
			return fmt.Sprintf("completed with %d errors", s.Errors)
		}
		if s.ItemsBackedUp == 0 {
			return "completed but captured zero items"
		}
		return ""
	default:
		return fmt.Sprintf("in phase %s", s.Phase)
	}
}

// requireCleanBackup refuses a restore source that did not reach a
// healthy Completed state. Restoring from a Failed/PartiallyFailed/
// empty backup leaves the AI bundle half-rehydrated; from an
// InProgress one it races the still-running snapshot.
func requireCleanBackup(u *unstructured.Unstructured, name string) error {
	if reason := backupUnhealthyReason(summariseBackup(u)); reason != "" {
		return fmt.Errorf("backup %q is not restorable: %s; refusing to restore an unhealthy snapshot", name, reason)
	}
	return nil
}

// listingPhase returns the phase to surface for a logical snapshot in
// the table output. It folds the sibling's phase into the primary's
// (Failed beats PartiallyFailed beats InProgress beats Completed), and
// downgrades a Completed-but-unhealthy snapshot (zero items, or with
// errors) to PartiallyFailed so the user cannot mistake an empty
// backup for a healthy one.
func listingPhase(primary, sibling *BackupSummary) string {
	phase := primary.Phase
	if sibling != nil {
		phase = worsePhase(phase, sibling.Phase)
	}
	if phase == "Completed" {
		if backupUnhealthyReason(primary) != "" {
			return "PartiallyFailed"
		}
		if sibling != nil && backupUnhealthyReason(sibling) != "" {
			return "PartiallyFailed"
		}
	}
	return phase
}

// worsePhase picks the phase that should win in a logical view.
// Worst-first: Failed > PartiallyFailed > InProgress/New > Completed.
func worsePhase(a, b string) string {
	rank := func(p string) int {
		switch p {
		case "Failed":
			return 4
		case "PartiallyFailed":
			return 3
		case "Completed":
			return 1
		case "":
			return 0
		default: // New, InProgress, FinalizingPartiallyFailed, etc.
			return 2
		}
	}
	if rank(b) > rank(a) {
		return b
	}
	return a
}

// StartedDelete describes the just-created DeleteBackupRequest CRs
// that callers pass to WaitDelete or WatchDeleteForWindow. The
// Targets list is the set of Backup CR names that the caller expects
// to disappear (primary, sibling, or both).
type StartedDelete struct {
	Name     string
	Targets  []string
	Requests []string // names of the DeleteBackupRequest CRs we created
}

// DeleteRunState reports whether the deletion for a logical snapshot
// has finished within the caller-supplied window.
type DeleteRunState int

const (
	// DeleteStillRunning means at least one target Backup CR is still
	// on the cluster; Velero's DeleteBackupRequest controller has not
	// finished freeing the Kopia repo data yet.
	DeleteStillRunning DeleteRunState = iota
	// DeleteCompleted means every target Backup CR is gone.
	DeleteCompleted
	// DeleteFailed means the Velero DeleteBackupRequest reported
	// errors, so the deletion will not progress without intervention.
	DeleteFailed
)

// StartDelete validates ownership of the named primary + sibling
// Backups and creates a Velero DeleteBackupRequest for each existing
// component. Returns immediately. Callers either block via WaitDelete
// or hand the StartedDelete to WatchDeleteForWindow for the async
// warmup pattern.
//
// `kubectl delete backup <name>` only removes the CR; the underlying
// snapshot data stays in object storage and the volume backend, which
// is wrong for AI backups carrying credentials and chat history. The
// DeleteBackupRequest controller deletes both the data and the CR, so
// we go through it.
//
// Each existing component is discovered and ownership-checked
// independently. That keeps a retry after a partially failed delete
// (primary gone, sibling still there) idempotent — and prevents a
// foreign backup that happens to be named `<owned>-aiconfig` from
// being deleted by name-convention alone.
func (i *Installer) StartDelete(ctx context.Context, name string) (*StartedDelete, error) {
	if strings.HasSuffix(name, configBackupSuffix) {
		return nil, fmt.Errorf("%q is the physical sibling of a snapshot; pass the logical primary name (without the %q suffix) to delete the whole snapshot", name, configBackupSuffix)
	}
	primary, err := i.findOwnedBackup(ctx, name)
	if err != nil {
		return nil, err
	}
	sibling, err := i.findOwnedBackup(ctx, name+configBackupSuffix)
	if err != nil {
		return nil, err
	}
	if primary == nil && sibling == nil {
		return nil, fmt.Errorf("backup %q not found", name)
	}
	out := &StartedDelete{Name: name}
	if primary != nil {
		reqName, err := i.adoptOrCreateDeleteRequest(ctx, name, primary.GetUID())
		if err != nil {
			return nil, err
		}
		out.Targets = append(out.Targets, name)
		out.Requests = append(out.Requests, reqName)
	}
	if sibling != nil {
		reqName, err := i.adoptOrCreateDeleteRequest(ctx, name+configBackupSuffix, sibling.GetUID())
		if err != nil {
			return nil, err
		}
		out.Targets = append(out.Targets, name+configBackupSuffix)
		out.Requests = append(out.Requests, reqName)
	}
	return out, nil
}

// adoptOrCreateDeleteRequest returns the name of the
// DeleteBackupRequest to watch. On a retry while a previous request
// is still in flight, Velero's deletion controller dedupes by
// `velero.io/backup-name` label and removes any duplicate requests
// it finds. If we always created a fresh request and watched it, the
// new request would be GC'd by Velero, our watcher would see
// "request gone + target still present" and (correctly under that
// rule) flag it as orphaned — even though the older request is
// progressing fine. Adopting the existing request instead avoids
// that false negative.
func (i *Installer) adoptOrCreateDeleteRequest(ctx context.Context, backupName string, backupUID types.UID) (string, error) {
	existing, err := i.findActiveDeleteRequest(ctx, backupName, backupUID)
	if err != nil {
		return "", err
	}
	if existing != "" {
		return existing, nil
	}
	return i.createDeleteBackupRequest(ctx, backupName, backupUID)
}

// findErroringDeleteRequest returns the name of any
// DeleteBackupRequest for the given backup name with non-empty
// status.errors. Used when our watched request is gone (likely
// deduped by Velero) — if a surviving rival has actually failed,
// the operator needs to be pointed at THAT request, not the deleted
// one whose name no longer resolves.
func (i *Installer) findErroringDeleteRequest(ctx context.Context, backupName string) (string, error) {
	selector := "velero.io/backup-name=" + getValidVeleroLabel(backupName)
	list, err := i.Dynamic.Resource(veleroDeleteRequestGVR).Namespace(VeleroNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		if isCRDNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("listing delete requests: %w", err)
	}
	for idx := range list.Items {
		item := &list.Items[idx]
		spec, _, _ := unstructured.NestedString(item.Object, "spec", "backupName")
		if spec != backupName {
			continue
		}
		errs, _, _ := unstructured.NestedStringSlice(item.Object, "status", "errors")
		if len(errs) > 0 {
			return item.GetName(), nil
		}
	}
	return "", nil
}

// findInProgressDeleteRequest returns the name of any
// DeleteBackupRequest for the given backup name whose phase is
// InProgress. Stricter than findActiveDeleteRequest: a rival that is
// also stuck in New/empty doesn't count, because two stalled
// requests are no more useful than one.
func (i *Installer) findInProgressDeleteRequest(ctx context.Context, backupName string) (string, error) {
	selector := "velero.io/backup-name=" + getValidVeleroLabel(backupName)
	list, err := i.Dynamic.Resource(veleroDeleteRequestGVR).Namespace(VeleroNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		if isCRDNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("listing delete requests: %w", err)
	}
	for idx := range list.Items {
		item := &list.Items[idx]
		spec, _, _ := unstructured.NestedString(item.Object, "spec", "backupName")
		if spec != backupName {
			continue
		}
		phase, _, _ := unstructured.NestedString(item.Object, "status", "phase")
		if phase == "InProgress" {
			return item.GetName(), nil
		}
	}
	return "", nil
}

// findActiveDeleteRequest returns the name of an already-existing
// DeleteBackupRequest for the same backup that is still making
// progress (phase empty/New/InProgress, no errors). Returns "" when
// no candidate exists.
//
// We select by velero.io/backup-name only, not backup-uid. Velero's
// own controller writes the UID label after it starts processing, so
// a New (still-queued) request from a previous run can be missing
// it. Filtering on spec.backupName provides the same safety as a
// UID match without losing legitimate adoption candidates.
func (i *Installer) findActiveDeleteRequest(ctx context.Context, backupName string, backupUID types.UID) (string, error) {
	selector := "velero.io/backup-name=" + getValidVeleroLabel(backupName)
	list, err := i.Dynamic.Resource(veleroDeleteRequestGVR).Namespace(VeleroNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		// CRD not found means Velero isn't installed; treat as
		// "nothing to adopt" and let createDeleteBackupRequest
		// surface the real error on Create.
		if isCRDNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("listing existing delete requests: %w", err)
	}
	for idx := range list.Items {
		item := &list.Items[idx]
		// Defence against a hash-collision on getValidVeleroLabel:
		// require spec.backupName to match exactly. Without this an
		// unrelated backup that hashes to the same label value could
		// be adopted.
		spec, _, _ := unstructured.NestedString(item.Object, "spec", "backupName")
		if spec != backupName {
			continue
		}
		// If the existing request carries a UID label and we know
		// the current backup's UID, they must match — otherwise the
		// request is for a previous incarnation of the same name.
		if backupUID != "" {
			if labelUID, ok := item.GetLabels()["velero.io/backup-uid"]; ok && labelUID != string(backupUID) {
				continue
			}
		}
		phase, _, _ := unstructured.NestedString(item.Object, "status", "phase")
		// Already Processed: Velero may not have GC'd yet, but the
		// work is done. Don't adopt — we want the next retry to
		// create a fresh request if needed.
		if phase == "Processed" {
			continue
		}
		errs, _, _ := unstructured.NestedStringSlice(item.Object, "status", "errors")
		if len(errs) > 0 {
			// Errored request is unrecoverable. Skip so the new
			// path can surface the failure cleanly.
			continue
		}
		return item.GetName(), nil
	}
	return "", nil
}

// deleteAckWindow is how long WaitDelete waits for the Velero
// controller to acknowledge a DeleteBackupRequest (move it past
// New/empty phase) before treating the request as stuck. Matches the
// async-default warmup window so the two paths agree on what
// "controller is healthy" means.
const deleteAckWindow = 60 * time.Second

// WaitDelete blocks until every target Backup CR disappears or the
// installer's poll timeout fires. Fails fast on Velero validation
// errors, and after the deleteAckWindow also fails on requests the
// controller never picked up (orphaned, or stuck in empty/New phase).
// CI scripts using `--wait` get a useful error within ~60 seconds
// instead of hanging for the full 30-minute timeout.
func (i *Installer) WaitDelete(ctx context.Context, started *StartedDelete) error {
	waitStart := time.Now()
	ackThreshold := waitStart.Add(i.ackWindow())
	deadline := waitStart.Add(i.timeout())
	for {
		// Fail fast on Velero validation errors.
		failed, err := i.failedDeleteRequests(ctx, started)
		if err != nil {
			return err
		}
		if len(failed) > 0 {
			return fmt.Errorf("delete request(s) for %q reported errors: %s; run 'kubectl describe deletebackuprequest -n %s %s' for details",
				started.Name, strings.Join(failed, ", "), VeleroNamespace, strings.Join(failed, " "))
		}

		gone, err := i.allTargetsGone(ctx, started)
		if err != nil {
			return err
		}
		if gone {
			return nil
		}

		// Past the ack window, surface unacknowledged/orphaned
		// requests so a stuck Velero doesn't burn the full poll
		// timeout silently.
		if time.Now().After(ackThreshold) {
			stuck, err := i.stuckDeleteRequests(ctx, started)
			if err != nil {
				return err
			}
			if len(stuck) > 0 {
				return fmt.Errorf("delete request(s) for %q failed or were not picked up by Velero: %s. "+
					"Run 'kubectl describe deletebackuprequest -n %s %s' for details, "+
					"and 'kubectl -n %s get pods,deployment/velero' to confirm the controller is healthy",
					started.Name, strings.Join(stuck, ", "), VeleroNamespace, strings.Join(stuck, " "), VeleroNamespace)
			}
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("backup %q still present after %s; check 'kubectl get deletebackuprequests -n %s' for the Velero error", started.Name, i.timeout(), VeleroNamespace)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(i.interval()):
		}
	}
}

// WatchDeleteForWindow polls the started deletion for the given
// window and returns the run state plus, on DeleteFailed, the names
// of the DeleteBackupRequest CRs that are responsible for the
// failure (status.errors non-empty, or still unacknowledged after
// the window).
//
// "Unacknowledged" is a distinct failure mode worth catching: if the
// Velero controller is down, stuck, or unable to patch status, the
// request sits with phase="" / "New" forever and the target Backup
// CRs are never deleted. Without this check the CLI would map that
// silent stall to "running in the background", and the user would
// never see a failure signal — leaving credentials and chat history
// retained in object storage indefinitely.
func (i *Installer) WatchDeleteForWindow(ctx context.Context, started *StartedDelete, window time.Duration) (DeleteRunState, []string, error) {
	deadline := time.Now().Add(window)
	for {
		// Per-request validation failures: any Kipper-owned
		// DeleteBackupRequest that reports errors during the warmup
		// is unrecoverable without operator action.
		failed, err := i.failedDeleteRequests(ctx, started)
		if err != nil {
			return DeleteStillRunning, nil, err
		}
		if len(failed) > 0 {
			return DeleteFailed, failed, nil
		}

		gone, err := i.allTargetsGone(ctx, started)
		if err != nil {
			return DeleteStillRunning, nil, err
		}
		if gone {
			return DeleteCompleted, nil, nil
		}

		if time.Now().After(deadline) {
			// At deadline expiry, any request stuck in empty/New
			// phase OR already gone while its target Backup remains
			// indicates Velero will not finish the deletion without
			// help. Surface that as DeleteFailed so the CLI errors
			// rather than silently exiting "running in the background".
			stuck, err := i.stuckDeleteRequests(ctx, started)
			if err != nil {
				return DeleteStillRunning, nil, err
			}
			if len(stuck) > 0 {
				return DeleteFailed, stuck, nil
			}
			return DeleteStillRunning, nil, nil
		}
		select {
		case <-ctx.Done():
			return DeleteStillRunning, nil, ctx.Err()
		case <-time.After(i.interval()):
		}
	}
}

// DeleteBackup is the synchronous convenience wrapper: start the
// deletion, then block until every target Backup CR is gone. Used by
// `kip ai backup delete --wait` and existing tests.
func (i *Installer) DeleteBackup(ctx context.Context, name string) error {
	started, err := i.StartDelete(ctx, name)
	if err != nil {
		return err
	}
	return i.WaitDelete(ctx, started)
}

// allTargetsGone returns true once every target Backup CR is no
// longer on the cluster.
func (i *Installer) allTargetsGone(ctx context.Context, started *StartedDelete) (bool, error) {
	for _, target := range started.Targets {
		_, err := i.Dynamic.Resource(veleroBackupGVR).Namespace(VeleroNamespace).Get(ctx, target, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("checking backup %s: %w", target, err)
		}
		return false, nil
	}
	return true, nil
}

// stuckDeleteRequests returns the names of DeleteBackupRequest CRs
// that cannot make further progress for their paired target Backup:
//
//   - phase=="" or "New" with no other related request InProgress →
//     Velero controller never picked any of our requests up.
//   - request gone but target still present and no rival active
//     request exists → request was deleted (by a human, or stripped
//     of progress with no replacement) leaving no controller work.
//
// A New/empty request whose sibling is currently InProgress is not
// flagged: Velero may be processing requests serially, so the
// sibling is queued behind active work, not actually stuck. Pairing
// is by parallel index in started.Requests/Targets, matching the
// order StartDelete creates them.
//
// Limitation: a request whose status is patched to InProgress and
// then never progresses (e.g. Velero crashes after acknowledging
// the request) is NOT classified as stuck by this function — its
// presence makes any related New/empty siblings count as "queued
// behind active work" too. `kip ai backup list` shows snapshots by
// Backup CR phase, not DeleteBackupRequest phase, so it does not
// expose this case directly. Operators investigate with
// `kubectl describe deletebackuprequests -n velero -l velero.io/backup-name=<name>`
// when a delete is taking unusually long. Building CLI-level
// liveness probing for the Velero controller is out of scope.
//
// Per Velero's API (https://github.com/vmware-tanzu/velero/blob/main/pkg/apis/velero/v1/delete_backup_request_types.go),
// DeleteBackupRequestPhase is one of: "" / "New" / "InProgress" / "Processed".
func (i *Installer) stuckDeleteRequests(ctx context.Context, started *StartedDelete) ([]string, error) {
	type info struct {
		target  string
		reqName string
		phase   string
		gone    bool
	}
	infos := make([]info, 0, len(started.Requests))
	anyInProgress := false
	for idx, reqName := range started.Requests {
		target := started.Targets[idx]
		_, err := i.Dynamic.Resource(veleroBackupGVR).Namespace(VeleroNamespace).Get(ctx, target, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			// Target gone → success for this pair, ignore.
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("checking backup %s: %w", target, err)
		}
		got, err := i.Dynamic.Resource(veleroDeleteRequestGVR).Namespace(VeleroNamespace).Get(ctx, reqName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			infos = append(infos, info{target: target, reqName: reqName, gone: true})
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("checking delete request %s: %w", reqName, err)
		}
		phase, _, _ := unstructured.NestedString(got.Object, "status", "phase")
		if phase == "InProgress" {
			anyInProgress = true
		}
		infos = append(infos, info{target: target, reqName: reqName, phase: phase})
	}

	var stuck []string
	for _, in := range infos {
		if in.gone {
			// Request gone, target still present. Could be Velero
			// dedupe of a concurrent retry. First: a rival with
			// status.errors should surface as the failed name —
			// otherwise the operator is sent to describe a CR that
			// no longer exists, hiding the real Velero error.
			erroring, err := i.findErroringDeleteRequest(ctx, in.target)
			if err != nil {
				return nil, err
			}
			if erroring != "" {
				stuck = append(stuck, erroring)
				continue
			}
			// Then: only suppress stuck when a rival is InProgress
			// (actually doing work). Two stalled requests (deduped
			// + rival also New) are both dead from the operator's
			// perspective.
			progressing, err := i.findInProgressDeleteRequest(ctx, in.target)
			if err != nil {
				return nil, err
			}
			if progressing != "" && progressing != in.reqName {
				continue
			}
			stuck = append(stuck, in.reqName)
			continue
		}
		if in.phase == "" || in.phase == "New" {
			// Queued behind another active request: not stuck.
			if anyInProgress {
				continue
			}
			stuck = append(stuck, in.reqName)
		}
	}
	return stuck, nil
}

// failedDeleteRequests returns the names of any DeleteBackupRequest
// CRs we created whose status.errors is non-empty. Velero writes
// errors there for requests it cannot process — e.g. the backup is
// missing in object storage, or the BackupStorageLocation is
// unreachable. Surfacing each failing CR name lets the CLI point the
// operator at the exact request rather than guessing the logical
// label, which is wrong when only the sibling failed.
func (i *Installer) failedDeleteRequests(ctx context.Context, started *StartedDelete) ([]string, error) {
	var failed []string
	for _, reqName := range started.Requests {
		got, err := i.Dynamic.Resource(veleroDeleteRequestGVR).Namespace(VeleroNamespace).Get(ctx, reqName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			// Velero already deleted the request after Processing.
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("checking delete request %s: %w", reqName, err)
		}
		errs, found, _ := unstructured.NestedStringSlice(got.Object, "status", "errors")
		if found && len(errs) > 0 {
			failed = append(failed, reqName)
		}
	}
	return failed, nil
}

// findOwnedBackup returns the named Backup if it exists and carries
// both Kipper's managed-by label and the AI bundle's part-of label.
// Returns (nil, nil) when not found, and an errNotOwnedByKipper when
// the backup is present but not ours. The part-of check matters: a
// `kip cluster backup` (some other Kipper subsystem) carries
// managed-by=kipper too, and `kip ai restore` / `kip ai backup
// delete` must not be able to act on that.
func (i *Installer) findOwnedBackup(ctx context.Context, name string) (*unstructured.Unstructured, error) {
	got, err := i.Dynamic.Resource(veleroBackupGVR).Namespace(VeleroNamespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading backup %q: %w", name, err)
	}
	labels := got.GetLabels()
	if !isManagedByKipper(labels) || labels["app.kubernetes.io/part-of"] != "kipper-ai" {
		return nil, &errNotOwnedByKipper{kind: "Backup", name: name}
	}
	return got, nil
}

// createDeleteBackupRequest creates a DeleteBackupRequest CR for the
// given Backup and returns the name of the created request.
//
// Explicit name (not GenerateName) is used so this works against the
// dynamic fake in tests, which does not synthesise names.
//
// We set Velero's standard correlation labels so the deletion
// controller's existing-request lookup
// (https://github.com/vmware-tanzu/velero/blob/main/pkg/controller/backup_deletion_controller.go,
// `label.NewSelectorForBackup`) finds our request and dedupes against
// any other in-flight request for the same backup. The backup-name
// label uses Velero's own truncate+hash algorithm so the value
// always fits the 63-char Kubernetes label-value limit even for
// long backup names.
func (i *Installer) createDeleteBackupRequest(ctx context.Context, backupName string, backupUID types.UID) (string, error) {
	reqName := boundedDeleteRequestName(backupName, time.Now().UTC().UnixNano())
	labels := map[string]interface{}{
		managedByLabel:              managedByValue,
		"app.kubernetes.io/part-of": "kipper-ai",
		"velero.io/backup-name":     getValidVeleroLabel(backupName),
	}
	if backupUID != "" {
		labels["velero.io/backup-uid"] = string(backupUID)
	}
	req := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "velero.io/v1",
		"kind":       "DeleteBackupRequest",
		"metadata": map[string]interface{}{
			"name":      reqName,
			"namespace": VeleroNamespace,
			"labels":    labels,
		},
		"spec": map[string]interface{}{
			"backupName": backupName,
		},
	}}
	if _, err := i.Dynamic.Resource(veleroDeleteRequestGVR).Namespace(VeleroNamespace).Create(ctx, req, metav1.CreateOptions{}); err != nil {
		return "", fmt.Errorf("creating delete request for %s: %w", backupName, err)
	}
	return reqName, nil
}

// getValidVeleroLabel mirrors Velero's pkg/label.GetValidName so the
// `velero.io/backup-name` label values we emit match the ones
// Velero's deletion controller computes when building its existing-
// request selector. Names ≤ 63 chars pass through; longer names are
// truncated to 57 chars and concatenated with the first 6 hex chars
// of the SHA-256 hash (no separator) for a final length of 63.
//
// Verified against
// https://raw.githubusercontent.com/vmware-tanzu/velero/main/pkg/label/label.go.
func getValidVeleroLabel(name string) string {
	const dns1035Max = 63
	if len(name) <= dns1035Max {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	const charsFromLabel = dns1035Max - 6
	return name[:charsFromLabel] + hex.EncodeToString(sum[:])[:6]
}

// boundedDeleteRequestName produces a DeleteBackupRequest name that
// embeds the backup name (so `kubectl get deletebackuprequests` is
// readable) plus a nanosecond timestamp for uniqueness across retries,
// while staying a valid Kubernetes DNS-1123 subdomain (lowercase
// alphanumerics, hyphens, dots; ≤ 253 chars; each label between dots
// non-empty and starting/ending alphanumeric).
//
// For typical names (well under the limit) the result is just
// `<backupName>-delete-<ts>`. For pathological 200+ char backup names
// it falls back to a truncated prefix + a SHA-256 fragment. The
// truncated prefix is right-trimmed of non-alphanumeric characters
// so a backup name with a `.` at byte 32 doesn't produce an invalid
// `<prefix>.-<hash>` label boundary. Velero matches by
// spec.backupName, so this naming choice has no functional effect —
// it only affects what the operator sees in `kubectl`.
func boundedDeleteRequestName(backupName string, nanos int64) string {
	const maxLen = 253
	suffix := fmt.Sprintf("-delete-%d", nanos)
	if len(backupName)+len(suffix) <= maxLen {
		return backupName + suffix
	}
	const prefixKeep = 32
	h := sha256.Sum256([]byte(backupName))
	prefix := strings.TrimRightFunc(backupName[:prefixKeep], func(r rune) bool {
		// DNS-1123 subdomain labels must start/end with [a-z0-9].
		// Trimming the right-hand side before the `-<hash>` boundary
		// keeps that invariant.
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	})
	if prefix == "" {
		// First 32 chars were entirely non-alphanumeric. Fall back to
		// a fully synthetic name so we never produce an empty label.
		return "dbr-" + hex.EncodeToString(h[:8]) + suffix
	}
	return prefix + "-" + hex.EncodeToString(h[:8]) + suffix
}

// CreateRestore restores a primary backup, then (if it existed) the
// cross-namespace sibling. Refuses if kipper-ai already exists, since
// restoring on top of a populated namespace corrupts state.
func (i *Installer) CreateRestore(ctx context.Context, backupName string) (*BackupSummary, error) {
	if backupName == "" {
		return nil, fmt.Errorf("backup name is required")
	}
	if strings.HasSuffix(backupName, configBackupSuffix) {
		return nil, fmt.Errorf("specify the primary backup name, not the %q sibling", configBackupSuffix)
	}
	if err := i.refuseIfNamespaceExists(ctx); err != nil {
		return nil, err
	}
	// HelmChart CRs in kube-system would cause helm-controller to
	// reconcile a fresh install in parallel with the restore, racing
	// the snapshotted resources. Refuse until those are also gone.
	if err := i.refuseIfHelmChartsPresent(ctx); err != nil {
		return nil, err
	}

	// Validate ownership for both physical backups before creating any
	// Restore CRs. A foreign Backup that happens to be named
	// `<owned>-aiconfig` would otherwise be replayed into
	// kipper-system unchecked.
	primaryBackup, err := i.findOwnedBackup(ctx, backupName)
	if err != nil {
		return nil, err
	}
	if primaryBackup == nil {
		return nil, fmt.Errorf("backup %q not found", backupName)
	}
	if err := requireCleanBackup(primaryBackup, backupName); err != nil {
		return nil, err
	}
	siblingName := backupName + configBackupSuffix
	siblingBackup, err := i.findOwnedBackup(ctx, siblingName)
	if err != nil {
		return nil, err
	}
	expectsSibling := primaryBackup.GetAnnotations()[configSiblingAnnotation] == "true"
	if expectsSibling && siblingBackup == nil {
		return nil, fmt.Errorf("backup %q is incomplete: config sibling %q is missing; refusing to restore a partial snapshot", backupName, siblingName)
	}
	if siblingBackup != nil {
		if err := requireCleanBackup(siblingBackup, siblingName); err != nil {
			return nil, err
		}
	}

	stamp := time.Now().UTC().Format("20060102-150405")
	primary, err := i.runRestore(ctx, backupName, primaryBackup.GetUID(), fmt.Sprintf("%s-restore-%s", backupName, stamp))
	if err != nil {
		return primary, err
	}
	if siblingBackup == nil {
		return primary, nil
	}
	sibling, err := i.runRestore(ctx, siblingName, siblingBackup.GetUID(), fmt.Sprintf("%s-restore-%s", siblingName, stamp))
	if err != nil {
		// Primary restore already succeeded: kipper-ai is back with
		// chat history, model cache, MongoDB. Only the cross-namespace
		// AI config and HelmChart CRs failed to replay. Recovery is
		// uninstall + retry: `kip ai install` is NOT safe here because
		// it generates fresh LibreChat credentials on every run, which
		// would overwrite the restored librechat-credentials Secret
		// and break LibreChat's ability to decrypt restored MongoDB
		// session/auth state.
		return primary, fmt.Errorf("primary restore for %q succeeded but the cross-namespace sibling restore failed: %w; "+
			"chat history and model cache are intact, but kip's AI client config and HelmChart records are missing. "+
			"Recover with: kip ai uninstall && kip ai restore --name %s. "+
			"Do NOT run 'kip ai install' to repair: it regenerates LibreChat credentials and would break access to the restored chat data",
			backupName, err, backupName)
	}
	primary.Errors += sibling.Errors
	primary.Warnings += sibling.Warnings
	primary.ItemsBackedUp += sibling.ItemsBackedUp
	return primary, nil
}

func (i *Installer) runRestore(ctx context.Context, backupName string, backupUID types.UID, restoreName string) (*BackupSummary, error) {
	spec := map[string]interface{}{
		"backupName":         backupName,
		"includedNamespaces": []interface{}{Namespace},
		// `update` overwrites resources Velero finds already in the
		// target namespace. The kipper-ai namespace is required to be
		// empty before restore (refuseIfNamespaceExists), so the only
		// thing that can collide is the kube-root-ca.crt ConfigMap that
		// the cluster's RootCAConfigMap controller auto-injects between
		// namespace creation and the Velero restore loop. Default
		// `none` produces a "ConfigMap kube-root-ca.crt already exists"
		// warning on every restore (cosmetic, but noisy and inflates the
		// CLI's warning count). Velero's RestoreSpec has no per-name
		// exclusion field (only resource-type filters), so update is the
		// cleanest fix: kube-root-ca.crt content is byte-identical to
		// what the controller injects, so update is a no-op semantically
		// and silences the warning.
		"existingResourcePolicy": "update",
	}
	// The sibling restore replays into kipper-system (AI config Secret)
	// and kube-system (HelmChart CRs). `kip ai uninstall` deletes the
	// HelmCharts but not the cluster Secret, so the existing config
	// would be skipped under the default `none` policy — that is exactly
	// the failure mode this command exists to prevent. The same `update`
	// policy that silences the kube-root-ca.crt warning above also makes
	// snapshot values win for the cross-namespace config Secret.
	if strings.HasSuffix(backupName, configBackupSuffix) {
		spec["includedNamespaces"] = []interface{}{SystemNamespace, "kube-system"}
	}
	// Match the labels Velero's own tooling expects: `velero.io/backup-name`
	// is what `velero restore describe`, the operator UI, and label
	// selectors look up. backup-uid disambiguates collisions when a
	// backup is recreated with the same name. The backup-name value goes
	// through getValidVeleroLabel because Velero's own controllers
	// truncate long names the same way; if we emit the raw name and
	// Velero emits the truncated one, label selectors miss.
	labels := map[string]interface{}{
		managedByLabel:              managedByValue,
		"app.kubernetes.io/part-of": "kipper-ai",
		"velero.io/backup-name":     getValidVeleroLabel(backupName),
	}
	if backupUID != "" {
		labels["velero.io/backup-uid"] = string(backupUID)
	}
	restore := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "velero.io/v1",
		"kind":       "Restore",
		"metadata": map[string]interface{}{
			"name":      restoreName,
			"namespace": VeleroNamespace,
			"labels":    labels,
		},
		"spec": spec,
	}}
	if _, err := i.Dynamic.Resource(veleroRestoreGVR).Namespace(VeleroNamespace).Create(ctx, restore, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Errorf("creating restore for %s: %w", backupName, err)
	}
	return i.waitForRestore(ctx, restoreName)
}

// waitForRestore polls until the named Restore reaches a terminal
// phase. Unlike Backup, a Velero Restore can finish in `Completed`
// phase while still carrying per-resource errors (per
// https://velero.io/docs/main/debugging-restores/), which means parts
// of the restore did not apply. Treat any non-zero error count as a
// failure so the CLI does not print a success checkmark over a
// half-restored bundle.
func (i *Installer) waitForRestore(ctx context.Context, name string) (*BackupSummary, error) {
	deadline := time.Now().Add(i.timeout())
	for {
		got, err := i.Dynamic.Resource(veleroRestoreGVR).Namespace(VeleroNamespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("reading restore %q: %w", name, err)
		}
		summary := summariseBackup(got)
		switch {
		case summary.Phase == "Completed":
			if summary.Errors > 0 {
				return summary, fmt.Errorf("restore %q completed with %d errors; run 'kubectl -n %s describe restore %s' for details",
					name, summary.Errors, VeleroNamespace, name)
			}
			return summary, nil
		case isTerminalFailed(summary):
			return summary, fmt.Errorf("restore %q ended in phase %s with %d errors", name, summary.Phase, summary.Errors)
		}
		if time.Now().After(deadline) {
			return summary, fmt.Errorf("restore %q did not finish within %s (current phase: %s)", name, i.timeout(), summary.Phase)
		}
		select {
		case <-ctx.Done():
			return summary, ctx.Err()
		case <-time.After(i.interval()):
		}
	}
}

// requireOwnedNamespace errors when the kipper-ai namespace is missing
// or is present but not Kipper-owned. A backup taken when the bundle
// was never installed would Complete with zero items and look healthy;
// a backup taken against an unrelated namespace someone happened to
// name `kipper-ai` would silently grab their data and label the
// resulting Backup CRs as ours. Both are unsafe.
func (i *Installer) requireOwnedNamespace(ctx context.Context) error {
	got, err := i.Clientset.CoreV1().Namespaces().Get(ctx, Namespace, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("namespace %s not found; install the AI bundle with 'kip ai install' before taking a backup", Namespace)
	}
	if err != nil {
		return fmt.Errorf("checking namespace: %w", err)
	}
	if !isManagedByKipper(got.Labels) {
		return fmt.Errorf("namespace %s exists but is not managed by Kipper; refusing to back up unrelated content", Namespace)
	}
	return nil
}

// refuseIfHelmChartsPresent waits until the Kipper-owned ollama and
// librechat HelmChart CRs in kube-system are gone. helm-controller
// would otherwise see them after the namespace is restored and start
// a parallel install that races the restore. Treats a Kipper-owned
// HelmChart as a hard refusal; a cluster left half-uninstalled (e.g.
// the user deleted the namespace by hand) is not a safe restore
// target until `kip ai uninstall` finishes the cleanup.
func (i *Installer) refuseIfHelmChartsPresent(ctx context.Context) error {
	for _, name := range []string{"ollama", "librechat"} {
		got, err := i.Dynamic.Resource(helmChartGVR).Namespace("kube-system").Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("checking helmchart kube-system/%s: %w", name, err)
		}
		if isManagedByKipper(got.GetLabels()) {
			return fmt.Errorf("HelmChart kube-system/%s still present; run 'kip ai uninstall' to clear it before restoring", name)
		}
	}
	return nil
}

// refuseIfNamespaceExists ensures the kipper-ai namespace is gone
// before restore creates Velero Restore CRs. A namespace mid-deletion
// (Phase=Terminating) is acceptable: we wait until it actually
// disappears, since `kip ai uninstall` returns immediately after
// issuing the Delete and users will commonly chain it with `kip ai
// restore`.
func (i *Installer) refuseIfNamespaceExists(ctx context.Context) error {
	deadline := time.Now().Add(i.timeout())
	for {
		got, err := i.Clientset.CoreV1().Namespaces().Get(ctx, Namespace, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("checking namespace: %w", err)
		}
		if got.Status.Phase != "Terminating" {
			return fmt.Errorf("namespace %s already exists; run 'kip ai uninstall' before restoring", Namespace)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("namespace %s is still Terminating after %s; cannot start restore", Namespace, i.timeout())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(i.interval()):
		}
	}
}

func summariseBackup(u *unstructured.Unstructured) *BackupSummary {
	s := &BackupSummary{
		Name:      u.GetName(),
		CreatedAt: u.GetCreationTimestamp().Time,
	}
	s.Phase, _, _ = unstructured.NestedString(u.Object, "status", "phase")
	if started, found, _ := unstructured.NestedString(u.Object, "status", "startTimestamp"); found {
		if t, err := time.Parse(time.RFC3339, started); err == nil {
			s.StartedAt = t
		}
	}
	if finished, found, _ := unstructured.NestedString(u.Object, "status", "completionTimestamp"); found {
		if t, err := time.Parse(time.RFC3339, finished); err == nil {
			s.FinishedAt = t
		}
	}
	// Backup CRs and Restore CRs share `status.progress.totalItems` but
	// use different counters for the processed count: Backup writes
	// `itemsBackedUp`, Restore writes `itemsRestored`. Reading only
	// `itemsBackedUp` makes Restore CRs report 0 items even when the
	// restore actually succeeded with hundreds of items, which is what
	// the smoke test surfaced as a misleading CLI summary. Pick the
	// field by kind so a single helper covers both. Older Velero
	// versions that left these unset would simply read 0, which matches
	// the previous behaviour.
	switch u.GetKind() {
	case "Restore":
		s.ItemsBackedUp, _, _ = unstructured.NestedInt64(u.Object, "status", "progress", "itemsRestored")
	default:
		s.ItemsBackedUp, _, _ = unstructured.NestedInt64(u.Object, "status", "progress", "itemsBackedUp")
	}
	s.TotalItems, _, _ = unstructured.NestedInt64(u.Object, "status", "progress", "totalItems")
	s.Errors, _, _ = unstructured.NestedInt64(u.Object, "status", "errors")
	s.Warnings, _, _ = unstructured.NestedInt64(u.Object, "status", "warnings")
	if s.Phase == "" {
		s.Phase = "New"
	}
	return s
}

// isCRDNotFound matches the apierrors variants Kubernetes returns when
// a CRD is missing.
func isCRDNotFound(err error) bool {
	if err == nil {
		return false
	}
	return apierrors.IsNotFound(err) || apierrors.IsServerTimeout(err) || isNoKindMatch(err)
}

func isNoKindMatch(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "no matches for kind") || strings.Contains(msg, "the server could not find the requested resource")
}
