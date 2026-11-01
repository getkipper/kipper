package installer

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/getkipper/kipper/controller/pkg/backupscope"
	"github.com/getkipper/kipper/kip/internal/mirror"
)

const (
	bucketCleanupJob        = "kipper-bucket-cleanup"
	bucketObjectsLeftMarker = "KIPPER_OBJECTS_LEFT"
	bucketVersionsMarker    = "KIPPER_VERSIONS_CHECKED"
	bucketUploadsMarker     = "KIPPER_UPLOADS_CHECKED"
	bucketDeletingMarker    = "KIPPER_DELETING"
	bucketUntouchedMarker   = "KIPPER_NOTHING_DELETED"
	bucketCleanupLabel      = "kipper.run/bucket-cleanup"

	// veleroStoppedBy marks a Velero Deployment that a reset stopped, so an
	// install that moves to another bucket knows it may start Velero again.
	veleroStoppedBy      = "kipper.run/stopped-by"
	veleroStoppedByReset = "repository-key-reset"
	clearVeleroStopMark  = "kubectl -n velero annotate deploy/velero " + veleroStoppedBy + "-"

	bucketCleanupTimeout = 30 * time.Minute
)

var (
	resetPollInterval  = 5 * time.Second
	veleroStopTimeout  = 3 * time.Minute
	writerStopTimeout  = 30 * time.Minute
	resetBackupTimeout = 30 * time.Minute

	veleroApplyTimeout = 10 * time.Minute
)

// veleroRecordKinds are the Velero resources that describe backups, restores
// and repositories. A reset deletes all of them, restores first, because a
// restore's finalizer refers to its backup.
var veleroRecordKinds = []string{
	"restores", "podvolumerestores", "datadownloads",
	"backups", "podvolumebackups", "datauploads",
	"deletebackuprequests", "downloadrequests", "backuprepositories",
}

// volumeWorkKinds are the Velero resources whose data the node agents move.
// The node agents carry on with them while the Velero server is stopped.
var volumeWorkKinds = []string{"podvolumebackups", "podvolumerestores", "datauploads", "datadownloads"}

var kubernetesNamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)

// BackupMirror is a mirror whose source is the cluster's backup bucket. It
// keeps existing copies after a reset, locked for at least RetentionDays.
// MaxDelete is "off" or the most objects one run may delete.
type BackupMirror struct {
	Name          string
	Target        string
	RetentionDays string
	MaxDelete     string
}

// RepositoryKeyReset describes the scope of a backup repository key reset.
// Run deletes every backup in the bucket and every Velero record of one,
// creates a new key on the server and takes a full backup from each schedule
// unless OutdatedSchedule is set. OutdatedSchedule names a schedule missing
// transient-resource exclusions on a cluster stopped by an unfinished reset.
// In that case, Run applies the exclusions to kip's schedules before restarting
// Velero and skips new backups. Run kip upgrade to update the rest of the chart.
// Run sets VersionsUnchecked when older object versions may remain because
// the store did not list versions and did not report versioning as enabled or
// suspended, and UploadsUnchecked when the store did not list incomplete
// uploads.
type RepositoryKeyReset struct {
	client CommandRunner

	Bucket       string
	Endpoint     string
	Backups      int
	Repositories int
	Mirrors      []BackupMirror

	OutdatedSchedule string

	VersionsUnchecked bool
	UploadsUnchecked  bool
}

// ResetBackup is a backup taken after a reset and the phase Velero last
// reported for it. An empty phase means no phase value was available. An
// unfinished phase means completion was unconfirmed when the wait ended.
type ResetBackup struct {
	Name  string
	Phase string
}

// SurveyRepositoryKeyReset inspects the reset's scope without changing the
// cluster. It returns an error if backups are unconfigured, multiple backup
// locations exist, the default location has a prefix or invalid endpoint, or
// Velero is running with an unfinished backup or restore. Read errors are
// returned. Unsupported Velero chart inputs also return an error. A kip
// schedule missing transient-resource exclusions returns an error unless an
// unfinished reset has stopped Velero. In that case, it records OutdatedSchedule
// so Run applies the exclusions to kip's schedules and skips new backups.
// When Velero is stopped, unfinished backup and restore records may be stale
// and are ignored.
func SurveyRepositoryKeyReset(client CommandRunner) (*RepositoryKeyReset, error) {
	live, endpoint, err := resettableLocation(client)
	if err != nil {
		return nil, fmt.Errorf("%w. Nothing was changed", err)
	}
	schedule, err := unscopedSchedule(client)
	if err != nil {
		return nil, err
	}
	if schedule != "" {
		stoppedByReset, err := veleroStoppedByAReset(client)
		if err != nil {
			return nil, err
		}
		if !stoppedByReset {
			return nil, fmt.Errorf("schedule %s predates the exclusion of Dex's signing keys and sessions, so the backups the reset takes would include them. "+
				"Run kip upgrade first, which updates kip's schedules. Nothing was changed", schedule)
		}
	}
	if err := checkVeleroChartInputs(client); err != nil {
		return nil, fmt.Errorf("%w. Nothing was changed", err)
	}
	r := &RepositoryKeyReset{client: client, Bucket: live.bucket, Endpoint: endpoint, OutdatedSchedule: schedule}

	replicas, err := veleroReplicas(client)
	if err != nil {
		return nil, err
	}
	backups, running, err := veleroWork(client)
	if err != nil {
		return nil, err
	}
	if running != "" && replicas > 0 {
		return nil, fmt.Errorf("%s is still running. Wait for it to finish, then run the reset again", running)
	}
	r.Backups = backups
	out, err := client.Run("kubectl -n velero get backuprepositories.velero.io -o name 2>/dev/null")
	if err != nil {
		return nil, fmt.Errorf("listing Velero's backup repositories: %w", err)
	}
	r.Repositories = len(nonEmptyLines(out))

	if r.Mirrors, err = mirrorsOf(client, live.bucket, endpoint); err != nil {
		return nil, err
	}
	return r, nil
}

// resettableLocation reads the default backup location and its endpoint. It
// returns an error when backups are unconfigured, the location has a prefix or
// an invalid endpoint, or the cluster has more than one location.
func resettableLocation(client CommandRunner) (liveLocation, string, error) {
	live, err := liveBackupLocation(client)
	if err != nil {
		return liveLocation{}, "", err
	}
	if live.bucket == "" {
		return liveLocation{}, "", errors.New("backups are not configured on this cluster")
	}
	if live.prefix != "" {
		return liveLocation{}, "", fmt.Errorf("the backup location keeps its objects under the prefix %q, which kip does not set up", live.prefix)
	}
	endpoint, err := cleanupEndpoint(live)
	if err != nil {
		return liveLocation{}, "", err
	}
	out, err := client.Run("kubectl -n velero get backupstoragelocations.velero.io -o name 2>/dev/null")
	if err != nil {
		return liveLocation{}, "", fmt.Errorf("listing Velero's backup locations: %w", err)
	}
	if n := len(nonEmptyLines(out)); n > 1 {
		return liveLocation{}, "", fmt.Errorf("this cluster has %d backup locations. Every location shares the repository key, and the reset empties only the default one, "+
			"so it handles a cluster with one location", n)
	}
	return live, endpoint, nil
}

// unscopedSchedule names the first schedule of kip's Velero chart whose
// backups would include a transient resource, or returns an empty string when
// they all exclude them. Schedules kip did not create are not checked, because
// kip upgrade cannot change them and their own runs back up the same resources.
func unscopedSchedule(client CommandRunner) (string, error) {
	out, err := client.Run(`kubectl -n velero get schedules.velero.io -l app.kubernetes.io/instance=velero,app.kubernetes.io/managed-by=Helm -o jsonpath='{range .items[*]}{.metadata.name}={.spec.template.excludedNamespaceScopedResources}{"\n"}{end}' 2>/dev/null`)
	if err != nil {
		return "", fmt.Errorf("listing backup schedules: %w", err)
	}
	for _, line := range nonEmptyLines(out) {
		name, excluded, _ := strings.Cut(line, "=")
		for _, resource := range backupscope.TransientResources() {
			if !strings.Contains(excluded, `"`+resource+`"`) {
				return name, nil
			}
		}
	}
	return "", nil
}

// scopeSchedules applies the resource filters rendered by kip upgrade to every
// schedule in kip's Velero chart and verifies the transient-resource exclusions.
// It removes the older excludedResources filter because Velero rejects it when
// combined with scoped filters. Velero must be stopped to prevent outdated
// schedules from backing up Dex's signing keys and sessions.
func scopeSchedules(client CommandRunner) error {
	out, err := client.Run("kubectl -n velero get schedules.velero.io -l app.kubernetes.io/instance=velero,app.kubernetes.io/managed-by=Helm -o name 2>/dev/null")
	if err != nil {
		return fmt.Errorf("listing backup schedules: %w", err)
	}
	patch, err := json.Marshal(map[string]any{"spec": map[string]any{"template": map[string]any{
		"excludedNamespaceScopedResources": backupscope.TransientResources(),
		"includedClusterScopedResources":   backupscope.BackedUpClusterResources(),
		"excludedResources":                nil,
	}}})
	if err != nil {
		return err
	}
	for _, name := range nonEmptyLines(out) {
		name = strings.TrimPrefix(name, "schedule.velero.io/")
		if _, err := client.Run("kubectl -n velero patch schedules.velero.io/" + shellQuote(name) + " --type merge -p " + shellQuote(string(patch))); err != nil {
			return fmt.Errorf("adding the exclusions to schedule %s: %w", name, err)
		}
	}
	schedule, err := unscopedSchedule(client)
	if err != nil {
		return err
	}
	if schedule != "" {
		return fmt.Errorf("schedule %s still backs up Dex's signing keys and sessions after kip added the exclusions", schedule)
	}
	return nil
}

// sameLocation verifies that the surveyed bucket and endpoint are unchanged,
// the location is still eligible for a reset, and Velero remains stopped. It
// rejects unsupported chart inputs and pending chart changes, which could
// restart Velero during cleanup.
func (r *RepositoryKeyReset) sameLocation() error {
	replicas, err := veleroReplicas(r.client)
	if err != nil {
		return err
	}
	if replicas != 0 {
		return errors.New("the Velero server was started again after kip stopped it, likely by a Velero chart still being applied. Run the reset again in a few minutes")
	}
	if err := checkVeleroChartInputs(r.client); err != nil {
		return err
	}
	// A chart change still to be applied would start Velero again later.
	applied, err := veleroChartApplied(r.client)
	if err != nil {
		return err
	}
	if !applied {
		return errors.New("the Helm controller has not finished applying the Velero chart. Check 'kubectl -n kube-system get helmchart velero' and the helm-install-velero job, then run the reset again")
	}
	live, endpoint, err := resettableLocation(r.client)
	if err != nil {
		return err
	}
	if live.bucket != r.Bucket || endpoint != r.Endpoint {
		return fmt.Errorf("the backup location changed from bucket %s at %s to bucket %s at %s since kip asked for confirmation", r.Bucket, r.Endpoint, live.bucket, endpoint)
	}
	return nil
}

// cleanupEndpoint returns the bucket endpoint used by Velero, deriving the
// regional AWS S3 URL when no custom endpoint is configured.
func cleanupEndpoint(live liveLocation) (string, error) {
	if live.s3URL == "" {
		if !regionPattern.MatchString(live.region) {
			return "", fmt.Errorf("the backup location's region %q is not a valid AWS region", live.region)
		}
		return "https://s3." + live.region + ".amazonaws.com", nil
	}
	if _, ok := plainEndpoint(live.s3URL); !ok {
		return "", fmt.Errorf("the backup location's endpoint %q is not an HTTP or HTTPS URL without a path", live.s3URL)
	}
	return normalizeEndpoint(live.s3URL), nil
}

// veleroFinished reports whether a backup or restore in this phase is done.
// All other phases count as running, including empty, Queued, Deleting and
// unknown phases.
func veleroFinished(phase string) bool {
	switch phase {
	case "Completed", "PartiallyFailed", "Failed", "FailedValidation":
		return true
	}
	return false
}

// veleroWork counts the backups and names the first backup or restore that is
// still running. The name is empty when all backups and restores are
// finished.
func veleroWork(client CommandRunner) (backups int, running string, err error) {
	for _, kind := range []string{"backups", "restores"} {
		out, err := client.Run("kubectl -n velero get " + kind + `.velero.io -o jsonpath='{range .items[*]}{.metadata.name}={.status.phase}{"\n"}{end}' 2>/dev/null`)
		if err != nil {
			return 0, "", fmt.Errorf("listing Velero %s: %w", kind, err)
		}
		for _, line := range nonEmptyLines(out) {
			name, phase, _ := strings.Cut(line, "=")
			if running == "" && !veleroFinished(phase) {
				running = strings.TrimSuffix(kind, "s") + " " + name
			}
			if kind == "backups" {
				backups++
			}
		}
	}
	return backups, running, nil
}

func veleroReplicas(client CommandRunner) (int, error) {
	out, err := client.Run("kubectl -n velero get deploy velero -o jsonpath='{.spec.replicas}' 2>/dev/null")
	if err != nil {
		return 0, fmt.Errorf("reading the Velero Deployment: %w", err)
	}
	replicas, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, fmt.Errorf("reading the Velero Deployment: unexpected replica count %q", strings.TrimSpace(out))
	}
	return replicas, nil
}

// sameStore treats matching hosts and hosts ending in .amazonaws.com as the
// same store so regional AWS S3 endpoints can match a mirror's source.
func sameStore(a, b string) bool {
	return a == b || strings.HasSuffix(a, ".amazonaws.com") && strings.HasSuffix(b, ".amazonaws.com")
}

// mirrorsOf lists mirrors of the given bucket using CronJob labels and
// annotations, which provide the metadata without reading credentials.
func mirrorsOf(client CommandRunner, bucket, endpoint string) ([]BackupMirror, error) {
	out, err := client.Run(`kubectl -n velero get cronjobs -l ` + mirror.NameLabel + ` -o jsonpath='{range .items[*]}` +
		`{.metadata.labels.kipper\.run/backup-mirror}|{.metadata.annotations.kipper\.run/mirror-source}|` +
		`{.metadata.annotations.kipper\.run/mirror-target}|{.metadata.annotations.kipper\.run/mirror-retention-days}|` +
		`{.metadata.annotations.kipper\.run/mirror-max-delete}{"\n"}{end}' 2>/dev/null`)
	if err != nil {
		return nil, fmt.Errorf("listing backup mirrors: %w", err)
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	host := strings.TrimSuffix(strings.ToLower(u.Host), ":443")
	var mirrors []BackupMirror
	for _, line := range nonEmptyLines(out) {
		f := strings.SplitN(line, "|", 5)
		if len(f) < 5 {
			continue
		}
		srcHost, srcBucket, err := mirror.ParseURL(f[1])
		if err != nil || !sameStore(srcHost, host) || srcBucket != bucket {
			continue
		}
		mirrors = append(mirrors, BackupMirror{Name: f[0], Target: f[2], RetentionDays: f[3], MaxDelete: f[4]})
	}
	return mirrors, nil
}

// Run carries out a surveyed reset and reports each step through progress,
// which must be non-nil. Velero stays stopped from before the bucket is
// emptied until the new key exists, and the bucket is emptied only once the
// node agents and repository maintenance have stopped writing to it. The node
// agents restart before Velero to discard the old key before accepting new
// work. With OutdatedSchedule set, kip's schedules get their exclusions while
// Velero is still stopped.
//
// After Velero stops successfully, a failure confirmed to precede deletion
// restarts it with the old key if this run stopped it. Otherwise, a failure
// leaves Velero stopped or running with the new key. Rerunning can finish the
// reset once any deletion restrictions are resolved. Created backups are
// returned in schedule-list order, including partial results on error. After
// creating the backups, Run waits up to 30 minutes for completion and returns
// an error if any backup has a phase other than Completed. An empty result
// means no backups were created, including when no schedules exist or
// OutdatedSchedule is set. The caller must hold the node lifecycle lease
// throughout the reset to serialize it with backup setup and node operations.
func (r *RepositoryKeyReset) Run(progress func(string)) ([]ResetBackup, error) {
	replicas, wasRunning, err := r.stopVelero(progress)
	if err != nil {
		return nil, err
	}
	deleted := false
	stopped := func(err error) error {
		if !wasRunning && !deleted {
			return fmt.Errorf("%w. Velero stays stopped, because kip found it stopped and cannot tell whether an earlier run deleted part of the bucket. "+
				"Run kip backup repository-key reset again once the cause is fixed", err)
		}
		return fmt.Errorf("%w. Velero is stopped, so no backups run. Run kip backup repository-key reset again to finish", err)
	}
	// Only restart Velero with the old key if this run stopped it. An earlier
	// run may have left it stopped with a partially emptied bucket.
	untouched := func(err error) error {
		if !wasRunning {
			return stopped(err)
		}
		if serr := r.resumeVelero(replicas); serr != nil {
			return stopped(fmt.Errorf("%w. Nothing was deleted, but starting Velero again failed: %w", err, serr))
		}
		return fmt.Errorf("%w. Nothing was deleted, and Velero runs again with the old key", err)
	}

	if err := r.stopWriters(progress); err != nil {
		return nil, untouched(err)
	}
	if err := r.sameLocation(); err != nil {
		return nil, untouched(err)
	}
	progress(fmt.Sprintf("Emptying bucket %s", r.Bucket))
	deleting, err := r.emptyBucket()
	deleted = deleting || err == nil
	var refused *objectsLeftError
	switch {
	case errors.As(err, &refused):
		return nil, err
	case err != nil && !deleting:
		return nil, untouched(err)
	case err != nil:
		return nil, stopped(err)
	}
	progress("Deleting Velero's records of the old backups")
	if err := r.deleteRecords(); err != nil {
		return nil, stopped(err)
	}
	progress("Creating a new repository key on the server")
	if _, err := r.client.Run("kubectl -n velero delete secret " + repositoryKeySecret + " --ignore-not-found"); err != nil {
		return nil, stopped(fmt.Errorf("deleting the old repository key: %w", err))
	}
	if _, err := r.client.Run(generateRepositoryKeyCmd); err != nil {
		return nil, stopped(fmt.Errorf("creating the new repository key: %w", err))
	}

	if r.OutdatedSchedule != "" {
		progress("Excluding Dex's signing keys and sessions from kip's schedules")
		if err := scopeSchedules(r.client); err != nil {
			return nil, stopped(err)
		}
	}
	progress("Starting Velero")
	if err := r.startVelero(replicas); err != nil {
		return nil, fmt.Errorf("starting Velero with the new key: %w. Run kip backup repository-key reset again to finish", err)
	}
	if r.OutdatedSchedule != "" {
		return nil, nil
	}
	progress("Taking a full backup from each schedule")
	return r.backUpNow()
}

// stopVelero scales the Velero server to zero and waits for its pods to
// terminate. It returns the replica count to restore and whether Velero was
// running. The count defaults to one when Velero was already stopped. A
// running Velero that started a backup or restore since the survey is left
// running.
func (r *RepositoryKeyReset) stopVelero(progress func(string)) (int, bool, error) {
	replicas, err := veleroReplicas(r.client)
	if err != nil {
		return 0, false, err
	}
	pods, err := r.veleroPods()
	if err != nil {
		return 0, false, err
	}
	if replicas > 0 && pods == 0 {
		return 0, false, errors.New("no Velero pod carries the label name=velero, so kip cannot tell when Velero has stopped. Nothing was changed")
	}
	if replicas > 0 {
		_, running, err := veleroWork(r.client)
		if err != nil {
			return 0, false, err
		}
		if running != "" {
			return 0, false, fmt.Errorf("%s started after the survey. Nothing was changed. Run the reset again once it finishes", running)
		}
	}
	wasRunning := replicas > 0
	if !wasRunning {
		replicas = 1
	}

	progress("Stopping Velero")
	if _, err := r.client.Run("kubectl -n velero annotate deploy/velero " + veleroStoppedBy + "=" + veleroStoppedByReset + " --overwrite"); err != nil {
		return 0, false, fmt.Errorf("marking Velero as stopped by the reset: %w", err)
	}
	if _, err := r.client.Run("kubectl -n velero scale deploy/velero --replicas=0"); err != nil {
		return 0, false, fmt.Errorf("stopping Velero: %w", err)
	}
	deadline := time.Now().Add(veleroStopTimeout)
	for {
		pods, err := r.veleroPods()
		if err != nil {
			return 0, false, err
		}
		if pods == 0 {
			return replicas, wasRunning, nil
		}
		if time.Now().After(deadline) {
			return 0, false, fmt.Errorf("the Velero pod did not stop within %s. Velero is scaled to zero. Run the reset again to continue", veleroStopTimeout)
		}
		time.Sleep(resetPollInterval)
	}
}

func (r *RepositoryKeyReset) resumeVelero(replicas int) error {
	for _, cmd := range []string{
		fmt.Sprintf("kubectl -n velero scale deploy/velero --replicas=%d", replicas),
		"kubectl -n velero rollout status deploy/velero --timeout=5m",
		clearVeleroStopMark,
	} {
		if _, err := r.client.Run(cmd); err != nil {
			return err
		}
	}
	return nil
}

// stopWriters deletes the repository maintenance Jobs and waits for their
// pods to terminate, then waits until the node agents have finished every
// volume transfer. Velero must already be stopped to prevent new work.
func (r *RepositoryKeyReset) stopWriters(progress func(string)) error {
	progress("Waiting for repository maintenance and volume transfers to stop")
	if _, err := r.client.Run("kubectl -n velero delete jobs -l velero.io/repo-name --ignore-not-found --cascade=foreground --wait --timeout=2m"); err != nil {
		return fmt.Errorf("stopping repository maintenance: %w", err)
	}
	deadline := time.Now().Add(writerStopTimeout)
	for {
		busy, err := r.busyVolumeWork()
		if err != nil {
			return err
		}
		if busy == "" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not finish within %s", busy, writerStopTimeout)
		}
		time.Sleep(resetPollInterval)
	}
}

// busyVolumeWork returns the first unfinished volume transfer, or an empty
// string when all transfers have completed, failed or been canceled.
func (r *RepositoryKeyReset) busyVolumeWork() (string, error) {
	for _, kind := range volumeWorkKinds {
		out, err := r.client.Run("kubectl -n velero get " + kind + `.velero.io -o jsonpath='{range .items[*]}{.metadata.name}={.status.phase}{"\n"}{end}' 2>/dev/null`)
		if err != nil {
			return "", fmt.Errorf("listing Velero %s: %w", kind, err)
		}
		for _, line := range nonEmptyLines(out) {
			name, phase, _ := strings.Cut(line, "=")
			switch phase {
			case "Completed", "Failed", "Canceled":
			case "":
				return fmt.Sprintf("%s %s (New)", strings.TrimSuffix(kind, "s"), name), nil
			default:
				return fmt.Sprintf("%s %s (%s)", strings.TrimSuffix(kind, "s"), name, phase), nil
			}
		}
	}
	return "", nil
}

func (r *RepositoryKeyReset) veleroPods() (int, error) {
	out, err := r.client.Run("kubectl -n velero get pods -l name=velero -o name 2>/dev/null")
	if err != nil {
		return 0, fmt.Errorf("listing Velero's pods: %w", err)
	}
	return len(nonEmptyLines(out)), nil
}

// stopCleanup deletes a cleanup Job and verifies that no cleanup pods remain,
// including pods from earlier runs that could still delete objects.
func (r *RepositoryKeyReset) stopCleanup(job string) error {
	if _, err := r.client.Run("kubectl -n velero delete job/" + job + " --ignore-not-found --cascade=foreground --wait --timeout=2m"); err != nil {
		return fmt.Errorf("stopping the bucket cleanup: %w", err)
	}
	pods, err := r.cleanupPods()
	if err != nil {
		return fmt.Errorf("stopping the bucket cleanup: %w", err)
	}
	if pods > 0 {
		return fmt.Errorf("stopping the bucket cleanup: %d of its pods are still there", pods)
	}
	return nil
}

// scriptOutput removes internal markers and blank lines from cleanup logs
// before they appear in errors.
func scriptOutput(logs string) string {
	var lines []string
	for _, line := range nonEmptyLines(logs) {
		if !strings.HasPrefix(line, "KIPPER_") {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

func (r *RepositoryKeyReset) cleanupPods() (int, error) {
	out, err := r.client.Run("kubectl -n velero get pods -l " + bucketCleanupLabel + " -o name 2>/dev/null")
	if err != nil {
		return 0, fmt.Errorf("listing the bucket cleanup pods: %w", err)
	}
	return len(nonEmptyLines(out)), nil
}

// objectsLeftError reports old backup objects or incomplete uploads that
// remain after cleanup. Retrying requires the store to allow their deletion.
type objectsLeftError struct {
	bucket string
	left   int
}

func (e *objectsLeftError) Error() string {
	return fmt.Sprintf("bucket %s still holds %d objects or incomplete uploads of the old backups that the store refused to delete, for example under a retention lock. "+
		"Velero stays stopped because cleanup may have deleted part of the old backups, and replacing the key would make the remaining volume data unreadable. "+
		"Run kip backup repository-key reset again once the store allows their deletion", e.bucket, e.left)
}

// emptyBucket uses Velero's credentials to delete backup and restore objects,
// Kopia and Restic repositories, listed versions and incomplete uploads. It
// returns an *objectsLeftError if any listed object, version or upload
// remains. The deleting result is false only when a single pod's logs confirm
// it stopped before deletion and all cleanup pods have been removed.
func (r *RepositoryKeyReset) emptyBucket() (deleting bool, err error) {
	// Earlier Jobs may still be deleting, and their results belong to earlier
	// runs. Remove them and their pods before starting a new Job. Treat failures
	// here as possible deletion because earlier Jobs may have changed the bucket.
	if _, err := r.client.Run("kubectl -n velero delete jobs -l " + bucketCleanupLabel + " --ignore-not-found --cascade=foreground --wait --timeout=2m"); err != nil {
		return true, fmt.Errorf("removing an earlier bucket cleanup: %w", err)
	}
	pods, err := r.cleanupPods()
	if err != nil {
		return true, err
	}
	if pods > 0 {
		return true, errors.New("an earlier bucket cleanup is still stopping. Run the reset again in a minute")
	}

	job := bucketCleanupJob + "-" + time.Now().UTC().Format("20060102150405")
	manifest := bucketCleanupManifest(job, r.Bucket, r.Endpoint)
	// An apply that reports an error may still have created the Job.
	if _, err := r.client.Run(fmt.Sprintf("cat <<'KIPEOF' | kubectl apply -f -\n%sKIPEOF", manifest)); err != nil {
		_ = r.stopCleanup(job)
		return true, fmt.Errorf("starting the bucket cleanup: %w", err)
	}
	logs, logsErr := waitForJobLogs(r.client, job, "the bucket cleanup", bucketCleanupTimeout)
	// Logs cover only one pod. A replacement pod may have deleted objects too,
	// so proof that nothing was deleted requires exactly one pod and successful
	// removal of all cleanup pods.
	pods, podsErr := r.cleanupPods()
	if err := r.stopCleanup(job); err != nil {
		return true, err
	}
	if logsErr != nil {
		return true, logsErr
	}
	deleting = podsErr != nil || pods != 1 || !strings.Contains(logs, bucketUntouchedMarker) || strings.Contains(logs, bucketDeletingMarker)
	r.VersionsUnchecked = strings.Contains(logs, bucketVersionsMarker+" no")
	r.UploadsUnchecked = strings.Contains(logs, bucketUploadsMarker+" no")
	i := strings.LastIndex(logs, bucketObjectsLeftMarker)
	if i < 0 {
		return deleting, fmt.Errorf("emptying bucket %s failed: %s", r.Bucket, scriptOutput(logs))
	}
	left, err := strconv.Atoi(strings.Fields(logs[i+len(bucketObjectsLeftMarker):] + " x")[0])
	if err != nil {
		return deleting, fmt.Errorf("emptying bucket %s failed: %s", r.Bucket, scriptOutput(logs))
	}
	if left > 0 {
		return deleting, &objectsLeftError{bucket: r.Bucket, left: left}
	}
	return deleting, nil
}

// bucketCleanupScript empties the backup prefixes. Until right before its
// first delete, an exit triggers the trap that prints bucketUntouchedMarker.
// A bucket with default retention stops the script before deletion. It
// deletes and counts every version when the store lists versions. A store
// that refuses to list them for a bucket it says keeps versions stops the
// script, since plain deletes there would hide old versions behind delete
// markers. Any other store gets plain deletes and a report that versions went
// unchecked. Incomplete uploads are found in a listing of the whole bucket,
// because a listing under a prefix misses them on MinIO, and are removed and
// counted by key. A store that refuses to list them gets a report instead.
const bucketCleanupScript = `set -e
trap 'echo ` + bucketUntouchedMarker + `' EXIT
id=$(sed -n 's/^[[:space:]]*aws_access_key_id[[:space:]]*=[[:space:]]*//p' /creds/cloud | head -n 1 | tr -d '\r')
secret=$(sed -n 's/^[[:space:]]*aws_secret_access_key[[:space:]]*=[[:space:]]*//p' /creds/cloud | head -n 1 | tr -d '\r')
mc alias set kipper "$ENDPOINT" "$id" "$secret" >/dev/null
if mc retention info --default "kipper/$BUCKET" 2>/dev/null | grep -qE 'COMPLIANCE|GOVERNANCE'; then
  echo "bucket $BUCKET locks every object under a default retention, so the reset stops before deleting any backups"
  exit 1
fi
versions=--versions
checked=yes
if ! mc ls --versions "kipper/$BUCKET/" >/dev/null 2>&1; then
  if mc version info "kipper/$BUCKET" 2>/dev/null | grep -qE 'is (enabled|suspended)'; then
    echo "bucket $BUCKET keeps object versions, but the store refused to list them"
    exit 1
  fi
  versions=
  checked=no
fi
trap - EXIT
echo ` + bucketDeletingMarker + `
for p in backups restores kopia restic; do
  mc rm --recursive --force $versions "kipper/$BUCKET/$p/" >/dev/null 2>&1 || true
done
: > /tmp/ls
for p in backups restores kopia restic; do
  mc ls --recursive $versions "kipper/$BUCKET/$p/" >> /tmp/ls
done
uploads=yes
if mc ls --recursive --incomplete "kipper/$BUCKET" > /tmp/up 2>/dev/null; then
  sed -n 's/^\[[^]]*\] *[^ ]* //p' /tmp/up | grep -E '^(backups|restores|kopia|restic)/' > /tmp/keys || true
  while IFS= read -r key; do
    mc rm --recursive --force --incomplete "kipper/$BUCKET/$key" >/dev/null 2>&1 || true
  done < /tmp/keys
  mc ls --recursive --incomplete "kipper/$BUCKET" > /tmp/up
  sed -n 's/^\[[^]]*\] *[^ ]* //p' /tmp/up | grep -E '^(backups|restores|kopia|restic)/' >> /tmp/ls || true
else
  uploads=no
fi
echo ` + bucketVersionsMarker + ` $checked
echo ` + bucketUploadsMarker + ` $uploads
echo ` + bucketObjectsLeftMarker + ` $(grep -c . /tmp/ls || true)`

// bucketCleanupManifest builds the Job that runs bucketCleanupScript.
// Environment variables keep the bucket and endpoint separate from shell
// syntax.
func bucketCleanupManifest(job, bucket, endpoint string) string {
	return fmt.Sprintf(`apiVersion: batch/v1
kind: Job
metadata:
  name: %s
  namespace: velero
  labels:
    %s: "true"
spec:
  backoffLimit: 0
  activeDeadlineSeconds: %d
  ttlSecondsAfterFinished: 600
  template:
    metadata:
      labels:
        %s: "true"
    spec:
      restartPolicy: Never
      automountServiceAccountToken: false
      securityContext:
        runAsNonRoot: true
        runAsUser: 65534
        runAsGroup: 65534
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: mc
          image: %s
          command:
            - sh
            - -c
            - %s
          env:
            - name: BUCKET
              value: %q
            - name: ENDPOINT
              value: %q
            - name: HOME
              value: /tmp
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop: [ALL]
          volumeMounts:
            - name: tmp
              mountPath: /tmp
            - name: creds
              mountPath: /creds
              readOnly: true
      volumes:
        - name: tmp
          emptyDir: {}
        - name: creds
          secret:
            secretName: cloud-credentials
`, job, bucketCleanupLabel, int(bucketCleanupTimeout.Seconds()), bucketCleanupLabel, minioClientImage, yamlBlock(bucketCleanupScript, 14), bucket, endpoint)
}

// deleteRecords removes Velero's resources for the old backups. Velero is
// stopped, so the finalizers it would clear are cleared here first.
func (r *RepositoryKeyReset) deleteRecords() error {
	for _, kind := range veleroRecordKinds {
		resource := kind + ".velero.io"
		if _, err := r.client.Run("kubectl -n velero get " + resource + " -o name 2>/dev/null" +
			` | xargs -r -n1 kubectl -n velero patch --type=merge -p '{"metadata":{"finalizers":null}}'`); err != nil {
			return fmt.Errorf("clearing the finalizers of Velero %s: %w", kind, err)
		}
		if _, err := r.client.Run("kubectl -n velero delete " + resource + " --all --ignore-not-found --timeout=2m"); err != nil {
			return fmt.Errorf("deleting Velero %s: %w", kind, err)
		}
	}
	return nil
}

// startVelero restarts the node agents, then brings Velero back with the new
// key and waits until it runs and the backup location is available.
func (r *RepositoryKeyReset) startVelero(replicas int) error {
	var cmds []string
	out, err := r.client.Run("kubectl -n velero get ds node-agent --ignore-not-found -o name 2>/dev/null")
	if err != nil {
		return fmt.Errorf("checking for Velero's node agents: %w", err)
	}
	if strings.TrimSpace(out) != "" {
		// Node agents cache the old repositories and their key.
		cmds = append(cmds, "kubectl -n velero rollout restart ds/node-agent", "kubectl -n velero rollout status ds/node-agent --timeout=5m")
	}
	cmds = append(cmds,
		fmt.Sprintf("kubectl -n velero scale deploy/velero --replicas=%d", replicas),
		"kubectl -n velero rollout status deploy/velero --timeout=5m",
		clearVeleroStopMark,
	)
	for _, cmd := range cmds {
		if _, err := r.client.Run(cmd); err != nil {
			if strings.Contains(cmd, "ds/node-agent") {
				return fmt.Errorf("%w. A node agent cannot restart while its node is down. Recover that node's services with kip node fence", err)
			}
			return err
		}
	}
	deadline := time.Now().Add(veleroStopTimeout)
	reported := ""
	for {
		out, err := r.client.Run("kubectl -n velero get backupstoragelocation default -o jsonpath='{.status.phase}|{.status.message}' 2>/dev/null")
		phase, message, read := strings.Cut(strings.TrimSpace(out), "|")
		read = read && err == nil
		if read && phase == "Available" {
			return nil
		}
		// Preserve Velero's last message so later read failures leave a useful timeout reason.
		if read {
			reported = strings.TrimSuffix(message, ".")
		}
		if time.Now().After(deadline) {
			timeout := fmt.Errorf("the backup location did not become available within %s after Velero started with the new key", veleroStopTimeout)
			if reported != "" {
				return fmt.Errorf("%w. Velero reports: %s", timeout, reported)
			}
			return timeout
		}
		time.Sleep(resetPollInterval)
	}
}

// backUpNow starts a backup from every schedule and waits for all of them.
func (r *RepositoryKeyReset) backUpNow() ([]ResetBackup, error) {
	out, err := r.client.Run(`kubectl -n velero get schedules.velero.io -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null`)
	if err != nil {
		return nil, fmt.Errorf("listing backup schedules: %w", err)
	}
	// The Deployment's selector also matches the node agents, so exec through
	// deploy/velero can land in one of them. The server pod has its own label.
	running, err := r.client.Run("kubectl -n velero get pods -l name=velero --field-selector=status.phase=Running -o jsonpath='{.items[*].metadata.name}' 2>/dev/null")
	if err != nil {
		return nil, fmt.Errorf("finding the Velero pod: %w", err)
	}
	pods := strings.Fields(running)
	if len(pods) == 0 || !kubernetesNamePattern.MatchString(pods[0]) {
		return nil, errors.New("no running Velero pod carries the label name=velero, so kip cannot start the backups. The key is reset. Take a backup with kip backup create")
	}
	stamp := time.Now().UTC().Format("20060102150405")
	var results []ResetBackup
	for _, schedule := range nonEmptyLines(out) {
		if !kubernetesNamePattern.MatchString(schedule) {
			continue
		}
		name := schedule + "-" + stamp
		if _, err := r.client.Run(fmt.Sprintf("kubectl -n velero exec %s -c velero -- /velero backup create %s --from-schedule %s", pods[0], name, schedule)); err != nil {
			return results, fmt.Errorf("starting a backup from schedule %s: %w", schedule, err)
		}
		results = append(results, ResetBackup{Name: name})
	}

	deadline := time.Now().Add(resetBackupTimeout)
	for {
		running := 0
		for i := range results {
			if veleroFinished(results[i].Phase) {
				continue
			}
			phase, err := r.client.Run("kubectl -n velero get backups.velero.io/" + results[i].Name + " -o jsonpath='{.status.phase}' 2>/dev/null")
			if err == nil {
				results[i].Phase = strings.TrimSpace(phase)
			}
			if !veleroFinished(results[i].Phase) {
				running++
			}
		}
		if running == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(resetPollInterval)
	}
	var unfinished []string
	for i, b := range results {
		if b.Phase == "" {
			results[i].Phase = "New"
		}
		if b.Phase != "Completed" {
			unfinished = append(unfinished, b.Name+" "+results[i].Phase)
		}
	}
	if len(unfinished) > 0 {
		return results, fmt.Errorf("the repository key is reset, but these backups have not completed: %s. Follow them with kip backup list", strings.Join(unfinished, ", "))
	}
	return results, nil
}

// unfinishedResetError indicates that applying the Velero chart would restart
// Velero on the bucket an unfinished reset is emptying.
type unfinishedResetError struct{ bucket string }

func (e *unfinishedResetError) Error() string {
	return fmt.Sprintf("an unfinished repository key reset stopped Velero on bucket %s. Run kip backup repository-key reset to finish it, then run this command again. Velero was not changed", e.bucket)
}

// veleroStoppedByAReset reports whether Velero has the reset annotation and
// zero desired replicas. Both are required because a failed restart may leave
// the annotation on a running Deployment.
func veleroStoppedByAReset(client CommandRunner) (bool, error) {
	out, err := client.Run(`kubectl -n velero get deploy velero --ignore-not-found -o jsonpath='{.metadata.annotations.kipper\.run/stopped-by}|{.spec.replicas}' 2>/dev/null`)
	if err != nil {
		return false, fmt.Errorf("reading the Velero Deployment: %w", err)
	}
	return strings.TrimSpace(out) == veleroStoppedByReset+"|0", nil
}

// veleroChartCheck prints six whitespace-separated fields for veleroChartApplied:
//
//  1. The hash of the chart's current values.
//  2. The hash of the values stored by the Helm controller.
//  3. The expected Job signature: 1|version|repository|namespace|configHash.
//  4. The actual Job signature, with its succeeded count in place of 1.
//  5. The Job's arguments.
//  6. The content ConfigMap's data keys and the values Secret's data keys,
//     separated by "|".
//
// Reading the Secret once keeps its values, keys and config hash on the same
// revision. The config hash uses sorted keys followed by their stored values.
// The controller hashes ConfigMap data first, so the check requires that data
// to be empty. A deleting Job's signature includes its deletion timestamp so
// it cannot match. Older controllers use the values-01_HelmChart.yaml key.
const veleroChartCheck = `spec=$(kubectl -n kube-system get helmchart velero -o jsonpath='{.spec.valuesContent}' 2>/dev/null | sha256sum | cut -d' ' -f1); ` +
	`want=$(kubectl -n kube-system get helmchart velero -o jsonpath='{.spec.version}|{.spec.repo}|{.spec.targetNamespace}' 2>/dev/null); ` +
	`stored=$(kubectl -n kube-system get secret chart-values-velero -o go-template='{{range $k, $v := .data}}{{$k}} {{$v}}{{"\n"}}{{end}}' 2>/dev/null); ` +
	`applied=$(printf '%s\n' "$stored" | awk '$1 == "HelmChartValuesContent" || $1 == "values-01_HelmChart.yaml" {print $2}' | base64 -d 2>/dev/null | sha256sum | cut -d' ' -f1); ` +
	`config=$(printf '%s\n' "$stored" | tr -d ' \n' | sha256sum | cut -d' ' -f1 | tr a-f A-F); ` +
	`keys=$(kubectl -n kube-system get configmap chart-content-velero -o go-template='{{range $k, $v := .data}}{{$k}},{{end}}' 2>/dev/null)"|"$(printf '%s\n' "$stored" | awk 'NF {printf "%s,", $1}'); ` +
	`job=$(kubectl -n kube-system get job helm-install-velero --ignore-not-found -o jsonpath='{.metadata.deletionTimestamp}{.status.succeeded}` +
	`|{.spec.template.spec.containers[0].env[?(@.name=="VERSION")].value}` +
	`|{.spec.template.spec.containers[0].env[?(@.name=="REPO")].value}` +
	`|{.spec.template.spec.containers[0].env[?(@.name=="TARGET_NAMESPACE")].value}` +
	`|{.spec.template.metadata.annotations.helmcharts\.helm\.cattle\.io/configHash} {.spec.template.spec.containers[0].args}' 2>/dev/null); ` +
	`echo "$spec $applied 1|$want|SHA256=$config ${job:-none} $keys"`

// veleroChartApplied reports whether the stored values and successful install
// Job match the current chart's values, version, repository and namespace.
// The Job's config hash must also match the stored values. Because kip uses
// only valuesContent, extra stored data or Job arguments containing --set
// indicate an unsupported or earlier configuration and return false.
func veleroChartApplied(client CommandRunner) (bool, error) {
	out, err := client.Run(veleroChartCheck)
	if err != nil {
		return false, fmt.Errorf("checking whether the Velero chart is applied: %w", err)
	}
	f := strings.Fields(out)
	return len(f) == 6 && f[0] == f[1] && f[2] == f[3] && !strings.Contains(f[4], "--set") &&
		(f[5] == "|HelmChartValuesContent," || f[5] == "|values-01_HelmChart.yaml,"), nil
}

// veleroChartInputsCheck prints the Velero HelmChart's spec as JSON, then the
// name of a HelmChartConfig that adds to it, if there is one.
const veleroChartInputsCheck = `kubectl -n kube-system get helmchart velero -o jsonpath='{.spec}' 2>/dev/null; echo; ` +
	`kubectl -n kube-system get helmchartconfig velero --ignore-not-found -o name 2>/dev/null`

// veleroChartComparedInputs are the HelmChart fields veleroChartApplied
// compares with what the Helm controller last applied.
var veleroChartComparedInputs = []string{"repo", "targetNamespace", "valuesContent", "version"}

// veleroChartFixedInputs are the other HelmChart fields kip writes or leaves
// to the CRD's defaults, with the only value it accepts for each. The
// defaults are those of helm-controller v0.16 and v0.17.
var veleroChartFixedInputs = map[string]string{
	"chart":           `"velero"`,
	"createNamespace": "true",
	"driver":          `"secret"`,
	"failurePolicy":   `"reinstall"`,
}

// checkVeleroChartInputs returns an error naming the first Velero chart input
// that kip neither writes nor compares, such as a set override or a
// HelmChartConfig. veleroChartApplied cannot see a pending change to one.
func checkVeleroChartInputs(client CommandRunner) error {
	out, err := client.Run(veleroChartInputsCheck)
	if err != nil {
		return fmt.Errorf("reading the Velero HelmChart: %w", err)
	}
	spec, config, _ := strings.Cut(out, "\n")
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(spec), &fields); err != nil {
		return fmt.Errorf("reading the Velero HelmChart: %w", err)
	}
	unmanaged := func(input string) error {
		return fmt.Errorf("the Velero chart has %s, which kip does not set, so kip cannot tell when the Helm controller has applied it. "+
			"Remove it, then run this command again", input)
	}
	if strings.TrimSpace(config) != "" {
		return unmanaged("a HelmChartConfig")
	}
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if slices.Contains(veleroChartComparedInputs, name) {
			continue
		}
		if want, ok := veleroChartFixedInputs[name]; ok && string(fields[name]) == want {
			continue
		}
		return unmanaged("spec." + name)
	}
	return nil
}

// waitForVeleroChart retries chart checks until the current chart is applied
// or veleroApplyTimeout expires, including when a check returns an error.
func waitForVeleroChart(client CommandRunner) error {
	deadline := time.Now().Add(veleroApplyTimeout)
	for {
		applied, err := veleroChartApplied(client)
		if err == nil && applied {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the Helm controller did not apply the Velero chart within %s. Check 'kubectl -n kube-system get helmchart velero' and the helm-install-velero job, then run this command again", veleroApplyTimeout)
		}
		time.Sleep(resetPollInterval)
	}
}

// startVeleroStoppedByReset starts a Velero that an unfinished reset left
// stopped. Call it only once Velero's location has moved off the bucket the
// reset was emptying.
func startVeleroStoppedByReset(client CommandRunner) error {
	for _, cmd := range []string{"kubectl -n velero scale deploy/velero --replicas=1", clearVeleroStopMark} {
		if _, err := client.Run(cmd); err != nil {
			return fmt.Errorf("starting Velero, which an unfinished repository key reset stopped: %w", err)
		}
	}
	fmt.Printf("       Started Velero, which an unfinished repository key reset had stopped. It still uses the old repository key, so run kip backup repository-key reset again to replace it.\n")
	return nil
}

// waitForLocation waits for the default backup location's bucket and endpoint
// to match target after chart application. It retries read errors until
// veleroApplyTimeout expires.
func waitForLocation(client CommandRunner, target backupTarget) error {
	deadline := time.Now().Add(veleroApplyTimeout)
	for {
		live, err := liveBackupLocation(client)
		if err == nil && !live.changesFrom(target) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the backup location did not move to bucket %s within %s of applying the Velero chart. Run this command again once it has", target.bucket, veleroApplyTimeout)
		}
		time.Sleep(resetPollInterval)
	}
}

func nonEmptyLines(s string) []string {
	var lines []string
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}
