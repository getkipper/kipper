package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// veleroBackupRepositoryGVR points at Velero's BackupRepository CRs.
// One repository per BackupStorageLocation per uploader (Kopia or
// Restic). When MinIO is wiped from underneath Velero, the
// BackupRepository's view of "what's in the bucket" diverges from
// reality and the next backup attempt fails with "repository not
// initialized in the provided storage". Detecting and replacing the
// CR forces Velero to re-initialise on the next backup.
var veleroBackupRepositoryGVR = schema.GroupVersionResource{
	Group:    "velero.io",
	Version:  "v1",
	Resource: "backuprepositories",
}

// RepairPlan summarises orphan states `kip ai backup repair` will fix.
type RepairPlan struct {
	// OrphanInCluster names Backup CRs Kipper owns that have no
	// matching Kopia repo data in MinIO. Recovery: delete the CR
	// (DeleteBackupRequest would loop trying to free non-existent
	// repo data).
	OrphanInCluster []string
	// OrphanInStorage names backup directories present in the MinIO
	// bucket without a matching Backup CR. Usually the result of
	// `kubectl delete backup` (which bypasses Velero's deletion
	// pipeline, so Kopia data is never freed).
	OrphanInStorage []string
	// BrokenRepos names BackupRepository CRs whose phase is anything
	// other than Ready. Replacing the CR forces Velero to re-init the
	// Kopia repo on the next backup.
	BrokenRepos []string
	// MinIOReachable is false when listing the bucket failed. The
	// caller can still act on cluster-side findings, but
	// OrphanInCluster and OrphanInStorage will be incomplete.
	MinIOReachable bool
}

// IsClean returns true when there is nothing for repair to do.
func (p *RepairPlan) IsClean() bool {
	return len(p.OrphanInCluster) == 0 && len(p.OrphanInStorage) == 0 && len(p.BrokenRepos) == 0
}

// minioListerImage is the mc client image used by the one-shot lister
// Pod. Pinned to match the bucket bootstrap step in
// kip/internal/installer/backup.go.
const minioListerImage = "minio/mc:RELEASE.2025-08-13T08-35-41Z"

// minioBackupBucket is the bucket Velero writes to. Matches the install
// path in kip/internal/installer/backup.go.
const minioBackupBucket = "velero"

// minioServiceURL points at the in-cluster MinIO Service. The Pod
// running mc lives in the velero namespace, so a short DNS name
// (minio:9000) suffices but using the FQDN protects against future
// namespace moves.
const minioServiceURL = "http://minio.velero.svc:9000"

// MinIO credential Secret coordinates, matching the install path in
// kip/internal/installer/backup.go. The root password is generated per
// install and lives only in this Secret, so the helper Pods read it via
// secretKeyRef rather than hardcoding a value that would break the
// moment MinIO is reinstalled with a fresh password.
const (
	minioCredentialsSecret = "minio-credentials" //nolint:gosec // G101: Kubernetes Secret object name, not a credential value
	minioUserSecretKey     = "root-user"
	minioPasswordSecretKey = "root-password"
)

// listerPodNamePrefix is the GenerateName prefix for the one-shot Pod
// that runs `mc ls` against MinIO. A unique server-assigned suffix
// avoids the AlreadyExists race when retrying after a timeout.
const listerPodNamePrefix = "kip-ai-backup-repair-list-"

// minioListEntry is the JSON shape mc emits for each top-level entry
// when invoked with `mc ls --json`. Only the Key field is needed.
type minioListEntry struct {
	Key string `json:"key"`
}

// DetectOrphans walks the Velero CRs in the cluster and the MinIO
// bucket, comparing the two to identify orphan states. Cluster-side
// findings (broken BackupRepository CRs) are returned even when MinIO
// is unreachable, so a torn-down storage layer does not hide an
// otherwise-detectable issue.
//
// MinIO-side orphan detection compares against every Backup CR, not
// just Kipper-owned ones. Velero's own scheduled snapshots
// (daily-apps, weekly-system) write to the same `velero/backups/`
// path; flagging those as orphans would have repair delete unrelated
// cluster-config backups. The cluster-side orphan list is
// Kipper-scoped because the only AI-bundle CRs we know how to
// recognise are the ones Kipper labels.
func (i *Installer) DetectOrphans(ctx context.Context) (*RepairPlan, error) {
	plan := &RepairPlan{}

	kipperCRs, err := i.listKipperBackupNames(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing backup CRs: %w", err)
	}
	allCRs, err := i.listAllBackupNames(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing all backup CRs: %w", err)
	}
	plan.BrokenRepos, err = i.listBrokenBackupRepositories(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing backup repositories: %w", err)
	}

	minioNames, listErr := i.listMinIOBackupDirs(ctx)
	if listErr != nil {
		// Cluster-side findings still useful; surface MinIO error to
		// the caller so the CLI prints a warning.
		return plan, fmt.Errorf("listing MinIO contents: %w", listErr)
	}
	plan.MinIOReachable = true

	allCRSet := stringSet(allCRs)
	minioSet := stringSet(minioNames)
	for _, n := range kipperCRs {
		if !minioSet[n] {
			plan.OrphanInCluster = append(plan.OrphanInCluster, n)
		}
	}
	for _, n := range minioNames {
		if !allCRSet[n] {
			plan.OrphanInStorage = append(plan.OrphanInStorage, n)
		}
	}
	sort.Strings(plan.OrphanInCluster)
	sort.Strings(plan.OrphanInStorage)
	return plan, nil
}

// ApplyRepair executes the cleanup actions in the plan against the
// cluster. The caller is expected to have prompted the operator for
// confirmation already.
func (i *Installer) ApplyRepair(ctx context.Context, plan *RepairPlan, out io.Writer) error {
	if out == nil {
		out = io.Discard
	}
	for _, name := range plan.BrokenRepos {
		_, _ = fmt.Fprintf(out, "  ...  Deleting broken BackupRepository %q\n", name)
		err := i.Dynamic.Resource(veleroBackupRepositoryGVR).Namespace(VeleroNamespace).Delete(ctx, name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting backuprepository %q: %w", name, err)
		}
		_, _ = fmt.Fprintf(out, "  ✔   Deleted BackupRepository %q (Velero will re-init on next backup)\n", name)
	}
	for _, name := range plan.OrphanInCluster {
		_, _ = fmt.Fprintf(out, "  ...  Deleting orphan Backup CR %q\n", name)
		err := i.Dynamic.Resource(veleroBackupGVR).Namespace(VeleroNamespace).Delete(ctx, name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting backup %q: %w", name, err)
		}
		_, _ = fmt.Fprintf(out, "  ✔   Deleted Backup %q\n", name)
	}
	if len(plan.OrphanInStorage) > 0 {
		_, _ = fmt.Fprintf(out, "  ...  Removing %d orphan path(s) from MinIO\n", len(plan.OrphanInStorage))
		if err := i.removeMinIOBackupDirs(ctx, plan.OrphanInStorage); err != nil {
			return fmt.Errorf("removing MinIO orphans: %w", err)
		}
		for _, name := range plan.OrphanInStorage {
			_, _ = fmt.Fprintf(out, "  ✔   Removed MinIO path backups/%s\n", name)
		}
	}
	return nil
}

// listKipperBackupNames returns the names of every Kipper-owned
// Backup CR in the velero namespace. Used to identify orphan CRs by
// set difference against the MinIO listing.
func (i *Installer) listKipperBackupNames(ctx context.Context) ([]string, error) {
	list, err := i.Dynamic.Resource(veleroBackupGVR).Namespace(VeleroNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: managedByLabel + "=" + managedByValue,
	})
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(list.Items))
	for _, item := range list.Items {
		names = append(names, item.GetName())
	}
	return names, nil
}

// listAllBackupNames returns every Backup CR in the velero namespace,
// regardless of owner. The MinIO bucket is shared between AI bundle
// backups and Velero's own scheduled snapshots (daily-apps,
// weekly-system); a MinIO directory only counts as an orphan when no
// Backup CR claims it, regardless of who created it.
func (i *Installer) listAllBackupNames(ctx context.Context) ([]string, error) {
	list, err := i.Dynamic.Resource(veleroBackupGVR).Namespace(VeleroNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(list.Items))
	for _, item := range list.Items {
		names = append(names, item.GetName())
	}
	return names, nil
}

// brokenRepoMinAge is the minimum time a BackupRepository CR must
// have spent in a non-Ready phase before repair flags it. New repos
// transit New → Ready (or New → NotReady) during normal init, which
// can take a minute on a slow cluster. Without this floor, repair
// would delete a still-initialising repo and interrupt the first
// backup of a fresh install.
const brokenRepoMinAge = 5 * time.Minute

// listBrokenBackupRepositories returns names of BackupRepository CRs
// stuck in a persistent non-Ready phase. Filters apply together:
//   - Phase is NotReady (the New phase is normal during init and is
//     skipped, so a freshly-created BR is never flagged).
//   - The CR has existed for at least brokenRepoMinAge so transient
//     init delays do not look like persistent failures.
//
// Kipper does not own BackupRepository CRs (Velero creates them), so
// the managed-by label is not used here.
func (i *Installer) listBrokenBackupRepositories(ctx context.Context) ([]string, error) {
	list, err := i.Dynamic.Resource(veleroBackupRepositoryGVR).Namespace(VeleroNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		// CRD not installed → no repair surface here, return clean.
		if apierrors.IsNotFound(err) || isCRDNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	now := time.Now()
	var broken []string
	for _, item := range list.Items {
		phase, _, _ := unstructured.NestedString(item.Object, "status", "phase")
		if phase != "NotReady" {
			continue
		}
		created := item.GetCreationTimestamp().Time
		if created.IsZero() || now.Sub(created) < brokenRepoMinAge {
			continue
		}
		broken = append(broken, item.GetName())
	}
	sort.Strings(broken)
	return broken, nil
}

// listMinIOBackupDirs runs a one-shot Pod with the mc client and
// returns the top-level entry names under `velero/backups/`. Each
// entry corresponds to a Kopia repo holding a single backup's data.
//
// The Pod prints `mc ls --json` output to stdout; the function reads
// the Pod logs after completion, parses each line as a minioListEntry,
// strips the trailing slash, and returns the names sorted.
//
// The shell script uses `set -e` and does not swallow errors from
// `mc alias set` or `mc ls`. If MinIO is unreachable, credentials are
// wrong, or the bucket is missing, the Pod ends in PodFailed and
// runMinIOLister returns a non-nil error. DetectOrphans then leaves
// MinIOReachable=false and the caller knows orphan-in-storage data is
// missing rather than treating "no entries listed" as "bucket empty".
// An empty bucket is fine: `mc ls` exits 0 and the Pod logs are empty.
func (i *Installer) listMinIOBackupDirs(ctx context.Context) ([]string, error) {
	pod, err := i.runMinIOLister(ctx, []string{
		"sh", "-c",
		fmt.Sprintf(
			"set -e\nmc alias set kipper %s \"$MINIO_ROOT_USER\" \"$MINIO_ROOT_PASSWORD\" >/dev/null\nmc ls --json kipper/%s/backups/\n",
			minioServiceURL, minioBackupBucket,
		),
	})
	if err != nil {
		return nil, err
	}
	defer i.deletePodBestEffort(context.Background(), pod.GetName())

	logs, err := i.readPodLogs(ctx, pod.GetName())
	if err != nil {
		return nil, fmt.Errorf("reading lister pod logs: %w", err)
	}
	return parseMinIOBackupNames(logs), nil
}

// removeMinIOBackupDirs runs a one-shot Pod that issues
// `mc rm --recursive --force` for each named directory.
func (i *Installer) removeMinIOBackupDirs(ctx context.Context, names []string) error {
	if len(names) == 0 {
		return nil
	}
	var script strings.Builder
	fmt.Fprintf(&script, "set -eu\nmc alias set kipper %s \"$MINIO_ROOT_USER\" \"$MINIO_ROOT_PASSWORD\" >/dev/null\n",
		minioServiceURL)
	for _, name := range names {
		// Pre-validated by the caller to be top-level entries we
		// listed ourselves; still strip slashes defensively before
		// embedding into the shell script.
		safe := strings.ReplaceAll(name, "/", "")
		fmt.Fprintf(&script, "mc rm --recursive --force kipper/%s/backups/%s >/dev/null\n",
			minioBackupBucket, safe)
	}
	pod, err := i.runMinIOLister(ctx, []string{"sh", "-c", script.String()})
	if err != nil {
		return err
	}
	defer i.deletePodBestEffort(context.Background(), pod.GetName())
	return nil
}

// runMinIOLister creates a one-shot Pod in the velero namespace and
// blocks until it reaches a terminal phase. Returns the Pod object so
// the caller can read logs and clean up.
func (i *Installer) runMinIOLister(ctx context.Context, command []string) (*corev1.Pod, error) {
	noBackoff := int64(60) // ActiveDeadlineSeconds upper bound.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: listerPodNamePrefix,
			Namespace:    VeleroNamespace,
			Labels: map[string]string{
				managedByLabel:                managedByValue,
				"app.kubernetes.io/part-of":   "kipper-ai",
				"app.kubernetes.io/component": "backup-repair",
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:         corev1.RestartPolicyNever,
			ActiveDeadlineSeconds: &noBackoff,
			Containers: []corev1.Container{{
				Name:    "mc",
				Image:   minioListerImage,
				Command: command,
				Env: []corev1.EnvVar{
					{
						Name: "MINIO_ROOT_USER",
						ValueFrom: &corev1.EnvVarSource{
							SecretKeyRef: &corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{Name: minioCredentialsSecret},
								Key:                  minioUserSecretKey,
							},
						},
					},
					{
						Name: "MINIO_ROOT_PASSWORD",
						ValueFrom: &corev1.EnvVarSource{
							SecretKeyRef: &corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{Name: minioCredentialsSecret},
								Key:                  minioPasswordSecretKey,
							},
						},
					},
				},
			}},
		},
	}
	created, err := i.Clientset.CoreV1().Pods(VeleroNamespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("creating MinIO helper pod: %w", err)
	}
	deadline := time.Now().Add(i.timeout())
	for {
		got, err := i.Clientset.CoreV1().Pods(VeleroNamespace).Get(ctx, created.GetName(), metav1.GetOptions{})
		if err != nil {
			return created, fmt.Errorf("watching MinIO helper pod: %w", err)
		}
		switch got.Status.Phase {
		case corev1.PodSucceeded:
			return got, nil
		case corev1.PodFailed:
			return got, fmt.Errorf("MinIO helper pod %q failed: %s", got.Name, got.Status.Message)
		}
		if time.Now().After(deadline) {
			return got, fmt.Errorf("MinIO helper pod %q did not finish within %s", got.Name, i.timeout())
		}
		select {
		case <-ctx.Done():
			return got, ctx.Err()
		case <-time.After(i.interval()):
		}
	}
}

// readPodLogs returns the full stdout/stderr stream of the named pod.
func (i *Installer) readPodLogs(ctx context.Context, name string) (string, error) {
	req := i.Clientset.CoreV1().Pods(VeleroNamespace).GetLogs(name, &corev1.PodLogOptions{})
	stream, err := req.Stream(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = stream.Close() }()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, stream); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// deletePodBestEffort deletes the named pod and ignores any error. Used
// from a defer when the pod's data has already been consumed.
func (i *Installer) deletePodBestEffort(ctx context.Context, name string) {
	_ = i.Clientset.CoreV1().Pods(VeleroNamespace).Delete(ctx, name, metav1.DeleteOptions{})
}

// parseMinIOBackupNames extracts directory names from the JSON-per-line
// output of `mc ls --json`. Trailing slashes are stripped; non-JSON
// lines (warnings, info) are silently skipped.
func parseMinIOBackupNames(logs string) []string {
	var names []string
	for _, line := range strings.Split(logs, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var entry minioListEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		key := strings.TrimSuffix(entry.Key, "/")
		if key == "" {
			continue
		}
		names = append(names, key)
	}
	sort.Strings(names)
	return names
}

// stringSet returns the input slice as a presence map for O(1) lookup.
func stringSet(items []string) map[string]bool {
	out := make(map[string]bool, len(items))
	for _, i := range items {
		out[i] = true
	}
	return out
}
