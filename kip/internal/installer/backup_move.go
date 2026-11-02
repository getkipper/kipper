package installer

import (
	"bytes"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"
)

const (
	// StorageRecordConfigMap names the ConfigMap that records an unfinished move
	// and the current and previous locations. Keys and credentials are stored separately.
	StorageRecordConfigMap = "kipper-backup-storage"

	previousKeySecret = "kipper-previous-repository-key" //nolint:gosec // G101: Kubernetes Secret object name, not a credential value
	nextKeySecret     = "kipper-next-repository-key"     //nolint:gosec // G101: Kubernetes Secret object name, not a credential value

	movePreparing = "preparing"
	moveCutover   = "cutover"
	moveSwitched  = "switched"
)

var (
	moveWait       = 5 * time.Minute
	backupMovePoll = 2 * time.Second
)

// StorageLocation identifies a backup bucket and its SSE-C setting.
// Credentials are stored separately.
type StorageLocation struct {
	Bucket   string `json:"bucket"`
	Region   string `json:"region,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
	SSEC     bool   `json:"sseC,omitempty"`
}

// sameBucket compares the bucket and the normalized endpoint, which together
// identify where backups are written.
func (l StorageLocation) sameBucket(o StorageLocation) bool {
	return l.Bucket == o.Bucket && normalizeEndpoint(l.Endpoint) == normalizeEndpoint(o.Endpoint)
}

// String names the bucket and its store, such as "backups at AWS S3".
func (l StorageLocation) String() string {
	return l.Bucket + " at " + describeEndpoint(l.Endpoint)
}

// BackupStorageMove records an unfinished move so it can resume from its last phase.
type BackupStorageMove struct {
	Target       StorageLocation `json:"target"`
	Previous     StorageLocation `json:"previous"`
	Phase        string          `json:"phase"`
	StartedAt    string          `json:"startedAt"`
	SSEGenerated bool            `json:"sseGenerated,omitempty"`
}

// PreviousLocation is the location a cluster moved away from, kept until
// kip backup storage retire-previous.
type PreviousLocation struct {
	StorageLocation
	SwitchedAt string `json:"switchedAt"`
}

// StorageRecord holds the kipper-backup-storage ConfigMap entries.
// Nil fields represent absent entries.
type StorageRecord struct {
	Move     *BackupStorageMove
	Current  *StorageLocation
	Previous *PreviousLocation
}

// ParseStorageRecord decodes the move, current and previous entries in the
// storage ConfigMap. Missing, empty or JSON null entries yield nil fields.
// Invalid JSON or incompatible field types return an error.
func ParseStorageRecord(data map[string]string) (StorageRecord, error) {
	var rec StorageRecord
	fields := []struct {
		key string
		dst any
	}{{"move", &rec.Move}, {"current", &rec.Current}, {"previous", &rec.Previous}}
	for _, f := range fields {
		if v := data[f.key]; v != "" {
			if err := json.Unmarshal([]byte(v), f.dst); err != nil {
				return StorageRecord{}, fmt.Errorf("reading %s in velero/%s: %w", f.key, StorageRecordConfigMap, err)
			}
		}
	}
	return rec, nil
}

// UpgradeBackupStorage returns the storage settings for an upgrade. A recorded
// location replaces missing local settings or settings with a different bucket,
// region, or endpoint. It is saved to the local cluster entry named clusterName
// if that entry exists.
// A matching or absent recorded location returns local unchanged, even if nil.
// An unfinished move returns an error to prevent an upgrade from changing Velero's
// location mid-move. Saving the local configuration can also return an error.
func UpgradeBackupStorage(rec StorageRecord, local *BackupStorageConfig, clusterName string) (*BackupStorageConfig, error) {
	if rec.Move != nil {
		cancel := ""
		if rec.Move.Phase == movePreparing {
			cancel = ", or cancel it with --abort"
		}
		return nil, fmt.Errorf("backups are moving to bucket %s. Finish the move with kip backup storage set --bucket %s%s, then upgrade",
			rec.Move.Target, rec.Move.Target.Bucket, cancel)
	}
	cur := rec.Current
	if cur == nil || (local != nil && local.Bucket == cur.Bucket && local.Region == cur.Region && local.Endpoint == cur.Endpoint) {
		return local, nil
	}
	recorded := &BackupStorageConfig{Bucket: cur.Bucket, Region: cur.Region, Endpoint: cur.Endpoint}
	if err := recordBackupStorage(clusterName, backupStorageRef(recorded)); err != nil {
		return nil, fmt.Errorf("saving the cluster's backup location %s locally: %w", cur, err)
	}
	return recorded, nil
}

func readStorageRecord(client CommandRunner) (StorageRecord, error) {
	out, err := client.Run("kubectl -n velero get configmap " + StorageRecordConfigMap + " --ignore-not-found -o jsonpath='{.data}' 2>/dev/null")
	if err != nil {
		return StorageRecord{}, fmt.Errorf("reading the backup storage record: %w", err)
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return StorageRecord{}, nil
	}
	data := map[string]string{}
	if err := json.Unmarshal([]byte(out), &data); err != nil {
		return StorageRecord{}, fmt.Errorf("reading the backup storage record: %w", err)
	}
	return ParseStorageRecord(data)
}

// writeStorageRecord replaces the whole record, so a nil field removes its key.
func writeStorageRecord(client CommandRunner, rec StorageRecord) error {
	data := map[string]string{}
	add := func(key string, v any, present bool) error {
		if !present {
			return nil
		}
		b, err := json.Marshal(v)
		data[key] = string(b)
		return err
	}
	if err := errors.Join(add("move", rec.Move, rec.Move != nil), add("current", rec.Current, rec.Current != nil),
		add("previous", rec.Previous, rec.Previous != nil)); err != nil {
		return err
	}
	manifest, err := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      StorageRecordConfigMap,
			"namespace": "velero",
			"labels":    map[string]string{"velero.io/exclude-from-backup": "true"},
		},
		"data": data,
	})
	if err != nil {
		return err
	}
	if _, err := client.RunStdin("kubectl apply -f -", bytes.NewReader(manifest)); err != nil {
		return fmt.Errorf("writing the backup storage record: %w", err)
	}
	return nil
}

// refuseRepointAfterMove stops kip install from pointing Velero away from the
// location a finished move recorded, because kip upgrade follows that record.
// The record is updated by kip backup storage set only.
func refuseRepointAfterMove(client CommandRunner, target StorageLocation) error {
	rec, err := readStorageRecord(client)
	if err != nil {
		return err
	}
	if rec.Current != nil && !rec.Current.sameBucket(target) {
		return fmt.Errorf("this cluster's backups were moved to bucket %s with kip backup storage set, and kip upgrade follows that move. "+
			"To change the location again, run kip backup storage set --bucket %s", rec.Current, target.Bucket)
	}
	return nil
}

// refuseDuringStorageMove stops install and upgrade from rendering Velero for
// either location while a move is unfinished.
func refuseDuringStorageMove(client CommandRunner) error {
	rec, err := readStorageRecord(client)
	if err != nil {
		return err
	}
	if rec.Move != nil {
		cancel := ""
		if rec.Move.Phase == movePreparing {
			cancel = ", or cancel it with kip backup storage set --abort"
		}
		return fmt.Errorf("backups are moving to bucket %s. Finish the move first: run kip backup storage set with --bucket %s and the same flags again%s",
			rec.Move.Target, rec.Move.Target.Bucket, cancel)
	}
	return nil
}

// settledPhases identifies finished or queued work that permits a storage move.
// New and empty phases are safe because processing pauses while Velero is stopped.
// Deletion requests are checked separately and must be Processed, because the move
// would otherwise discard an unprocessed request.
var settledPhases = map[string]bool{
	"": true, "New": true, "Completed": true, "PartiallyFailed": true, "Failed": true,
	"FailedValidation": true, "Canceled": true, "Processed": true,
}

// backupWorkInFlight lists backups, restores, volume backups and restores,
// and deletion requests whose phases block a move, plus active repository
// maintenance Jobs. An empty result with no error means no work blocks the move.
func backupWorkInFlight(client CommandRunner) ([]string, error) {
	out, err := client.Run(`kubectl -n velero get backups.velero.io,restores.velero.io,podvolumebackups.velero.io,podvolumerestores.velero.io,deletebackuprequests.velero.io -o jsonpath='{range .items[*]}{.kind}/{.metadata.name} {.status.phase}{"\n"}{end}' 2>/dev/null`)
	if err != nil {
		return nil, fmt.Errorf("listing Velero's backups and restores: %w", err)
	}
	var busy []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		phase := ""
		if len(fields) > 1 {
			phase = fields[1]
		}
		settled := settledPhases[phase]
		if strings.HasPrefix(fields[0], "DeleteBackupRequest/") {
			settled = phase == "Processed"
		}
		if !settled {
			busy = append(busy, fields[0]+" ("+phase+")")
		}
	}
	jobs, err := client.Run(`kubectl -n velero get jobs -l velero.io/repo-name -o jsonpath='{range .items[*]}{.metadata.name} {.status.active}{"\n"}{end}' 2>/dev/null`)
	if err != nil {
		return nil, fmt.Errorf("listing repository maintenance jobs: %w", err)
	}
	for _, line := range strings.Split(jobs, "\n") {
		if fields := strings.Fields(line); len(fields) == 2 && fields[1] != "0" {
			busy = append(busy, "Job/"+fields[0]+" (repository maintenance)")
		}
	}
	return busy, nil
}

// refusePopulatedTarget returns an error when the target bucket already holds
// backups, or when kip cannot list it.
func refusePopulatedTarget(client CommandRunner, target StorageLocation, dest backupTarget) error {
	populated, err := (&bucketCheck{client: client, target: dest}).populated()
	if err != nil {
		return fmt.Errorf("kip has to check that bucket %s holds no backups before moving to it: %w", target, err)
	}
	if populated {
		return fmt.Errorf("bucket %s already holds backups (a top-level backups/, kopia/ or restic/ folder), possibly another cluster's. Move to a bucket without them", target)
	}
	return nil
}

// withResumeHint ends an error from a started move with the command that
// continues it, because Velero may be stopped or pointed at the new bucket.
func withResumeHint(err error, rec *StorageRecord) error {
	switch {
	case err == nil:
		return nil
	case rec.Move == nil:
		return fmt.Errorf("%w. Run the same kip backup storage set command again to finish", err)
	case rec.Move.Phase == movePreparing:
		return fmt.Errorf("%w. Run the same kip backup storage set command again to continue the move, or cancel it with --abort", err)
	default:
		return fmt.Errorf("%w. Run the same kip backup storage set command again to continue the move", err)
	}
}

func errBackupWorkInFlight(busy []string) error {
	return fmt.Errorf("a backup, restore, backup deletion or repository maintenance job is in progress (%s). Wait for it to finish", strings.Join(busy, ", "))
}

// stopVelero scales the Velero server to zero and waits up to moveWait for
// its pods to disappear, including terminating pods. Node agents keep running.
func stopVelero(client CommandRunner) error {
	if _, err := client.Run("kubectl -n velero scale deploy/velero --replicas=0"); err != nil {
		return fmt.Errorf("stopping Velero: %w", err)
	}
	deadline := time.Now().Add(moveWait)
	for {
		out, err := client.Run(`kubectl -n velero get pods -o jsonpath='{range .items[*]}{.metadata.name} {.metadata.ownerReferences[0].kind} {.metadata.ownerReferences[0].name}{"\n"}{end}' 2>/dev/null`)
		if err != nil {
			return fmt.Errorf("waiting for Velero to stop: %w", err)
		}
		running := false
		for _, line := range strings.Split(out, "\n") {
			if f := strings.Fields(line); len(f) == 3 && f[1] == "ReplicaSet" && strings.HasPrefix(f[2], "velero-") {
				running = true
			}
		}
		if !running {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the Velero server did not stop within %s", moveWait)
		}
		time.Sleep(backupMovePoll)
	}
}

// readKeySecret returns a key Secret's field, or nil when the Secret or the
// field is missing. Errors name only the Secret to keep keys out of diagnostics.
func readKeySecret(client CommandRunner, name, field string) ([]byte, error) {
	out, err := client.Run("kubectl -n velero get secret " + name + " --ignore-not-found -o jsonpath='{.data." + field + "}' 2>/dev/null")
	if err != nil {
		return nil, fmt.Errorf("reading Secret velero/%s failed", name)
	}
	value, err := base64.StdEncoding.DecodeString(strings.TrimSpace(out))
	if err != nil {
		return nil, fmt.Errorf("the key in Secret velero/%s is not valid base64", name)
	}
	if len(value) == 0 {
		return nil, nil
	}
	return value, nil
}

// readRepositoryKey reads a repository key Secret trimmed as Velero reads it.
func readRepositoryKey(client CommandRunner, name string) ([]byte, error) {
	key, err := readKeySecret(client, name, repositoryKeyField)
	return bytes.TrimSpace(key), err
}

func sameKey(a, b []byte) bool {
	return len(a) > 0 && subtle.ConstantTimeCompare(a, b) == 1
}

// exportKeyFile writes key to a new file at path with mode 0600. An existing
// empty regular file is replaced. Any other existing file must pass the same
// checks as a key file passed to kip install and hold the same key.
// The sse flag selects base64 encoding for SSE-C keys.
func exportKeyFile(flag, path string, key []byte, sse bool) error {
	// Replace an empty file so an export interrupted before writing can resume.
	if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() && info.Size() == 0 {
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("%s: %w", flag, err)
		}
	}
	if _, err := os.Lstat(path); err == nil {
		read := readRepositoryKeyFile
		if sse {
			read = readSSECKeyFile
		}
		have, err := read(flag, path)
		if err != nil {
			return err
		}
		if !sameKey(have, key) {
			return fmt.Errorf("%s %s holds a different key. Pass a new path, or the file this move wrote", flag, path)
		}
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s: %w", flag, err)
	}
	content := key
	if sse {
		content = []byte(EncodeSSECKey(key) + "\n")
	}
	return writeNewKeyFile(flag, path, content)
}

// applyRepositoryKeyCmd updates the live repository key in place from stdin.
var applyRepositoryKeyCmd = "kubectl -n velero create secret generic " + repositoryKeySecret +
	" --from-file=" + repositoryKeyField + "=/dev/stdin --dry-run=client -o yaml" +
	" | kubectl label --local -f - " + excludeFromBackupLabel + " -o yaml | kubectl apply -f -"

// applyCloudCredentialsCmd replaces cloud-credentials in place from stdin.
var applyCloudCredentialsCmd = "kubectl -n velero create secret generic cloud-credentials" +
	" --from-file=cloud=/dev/stdin --dry-run=client -o yaml" +
	" | kubectl label --local -f - " + excludeFromBackupLabel + " -o yaml | kubectl apply -f -"

// MoveOptions configures MoveBackupStorage. Target, Confirm, Out and Now must
// be non-nil. Target carries the new bucket's credentials, and Target.SSEC requests
// SSE-C there. NewKeyFile and PreviousKeyFile are required export paths, as is
// SSECKeyFile when SSE-C is enabled. Confirm receives a description of a fresh
// move and must return nil to proceed. Recorded moves resume without confirmation.
// ClusterName names the local cluster entry that records the new location.
type MoveOptions struct {
	Target          *BackupStorageConfig
	ClusterName     string
	NewKeyFile      string
	PreviousKeyFile string
	SSECKeyFile     string
	Confirm         func(summary []string) error
	Out             io.Writer
	Now             func() time.Time
}

// MoveBackupStorage redirects future backups to opts.Target with a new repository
// key. The target must be an external bucket without backups, and the current
// location must be writable external storage. Existing backups remain in the old
// bucket, where this cluster stops creating and deleting backups. Repository keys
// for both buckets and any SSE-C key are exported before Velero switches.
//
// Errors may leave a recorded move that resumes with the same bucket, endpoint
// and SSE-C setting. Credentials and region can be corrected when resuming.
// Active backup work and an unretired previous location prevent a fresh move.
// The caller must hold the node lifecycle lease and supply valid MoveOptions.
func MoveBackupStorage(client CommandRunner, opts MoveOptions) error {
	if err := CheckBackupEndpoint(opts.Target.Endpoint); err != nil {
		return err
	}
	target := StorageLocation{Bucket: opts.Target.Bucket, Region: opts.Target.Region, Endpoint: opts.Target.Endpoint, SSEC: opts.Target.SSEC}
	dest, err := newBackupTarget(opts.Target, "")
	if err != nil {
		return err
	}
	rec, err := readStorageRecord(client)
	if err != nil {
		return err
	}
	switch {
	case rec.Move != nil && !rec.Move.Target.sameBucket(target):
		cancel := ""
		if rec.Move.Phase == movePreparing {
			cancel = ", or cancel it with --abort"
		}
		return fmt.Errorf("backups are already moving to bucket %s. Finish that move first: run this command with --bucket %s%s",
			rec.Move.Target, rec.Move.Target.Bucket, cancel)
	case rec.Move != nil:
		if rec.Move.Target.SSEC != target.SSEC {
			return fmt.Errorf("the move to bucket %s was started with SSE-C %s. Run it again with the same --sse-c setting", rec.Move.Target, onOff(rec.Move.Target.SSEC))
		}
		// A corrected region is recorded first, so the record, the chart and
		// the local config keep naming the same location.
		if rec.Move.Target.Region != target.Region {
			rec.Move.Target.Region = target.Region
			if rec.Current != nil && rec.Current.sameBucket(target) {
				rec.Current.Region = target.Region
			}
			if err := writeStorageRecord(client, rec); err != nil {
				return err
			}
		}
		// Until Velero switches, another cluster can still claim the bucket.
		if rec.Move.Phase == movePreparing {
			if err := refusePopulatedTarget(client, target, dest); err != nil {
				return fmt.Errorf("%w. Cancel this move with --abort", err)
			}
		}
		_, _ = fmt.Fprintf(opts.Out, "\n  Continuing the move to bucket %s (phase %s).\n", target, rec.Move.Phase)
		return withResumeHint(runMove(client, opts, &rec, dest), &rec)
	case rec.Current != nil && rec.Current.sameBucket(target):
		if _, err := client.Run("kubectl -n velero delete secret " + nextKeySecret + " --ignore-not-found"); err != nil {
			return fmt.Errorf("removing a staged key left by an earlier move: %w", err)
		}
		_, _ = fmt.Fprintf(opts.Out, "\n  ✔  Backups already go to bucket %s.\n\n", target)
		return nil
	}

	live, err := liveBackupLocation(client)
	if err != nil {
		return err
	}
	stoppedByReset, err := veleroStoppedByAReset(client)
	if err != nil {
		return err
	}
	switch {
	case live.bucket == "":
		return errors.New("this cluster has no Velero backup location to move")
	case stoppedByReset:
		return fmt.Errorf("an unfinished repository key reset stopped Velero on bucket %s. Run kip backup repository-key reset to finish it first", live.bucket)
	case live.prefix != "":
		return fmt.Errorf("this cluster's backup location stores its backups under prefix %s in bucket %s, which kip does not set up. kip backup storage set moves only locations without a prefix", live.prefix, live.bucket)
	}
	if n, err := countBackupLocations(client); err != nil {
		return err
	} else if n > 1 {
		return fmt.Errorf("this cluster has %d backup locations. Every location shares the repository key, so kip backup storage set moves only a cluster with one location", n)
	}
	switch {
	case BackupsGoToMinIO(live.s3URL):
		return errors.New("this cluster's backups go to its in-cluster MinIO. kip backup storage set moves external backup storage only")
	case live.accessMode == "ReadOnly":
		return errors.New("this cluster's backup location is read-only, as on a recovery install. A move starts from a writable location")
	case rec.Previous != nil:
		return fmt.Errorf("this cluster still records its previous backup location, bucket %s. Run kip backup storage retire-previous first", rec.Previous.StorageLocation)
	case !live.changesFrom(dest):
		return fmt.Errorf("backups already go to bucket %s", target)
	}
	sseHeld, err := sseKeyExists(client)
	if err != nil {
		return err
	}
	if sseHeld && !target.SSEC {
		return fmt.Errorf("this cluster's backups use SSE-C (Secret velero/%s). Pass --sse-c to keep it for the new bucket", sseKeySecret)
	}
	if target.SSEC {
		if err := CheckSSECStorage(opts.Target); err != nil {
			return err
		}
	}
	if err := requireSSECKeyFlag(target.SSEC, opts.SSECKeyFile); err != nil {
		return err
	}
	if busy, err := backupWorkInFlight(client); err != nil {
		return err
	} else if len(busy) > 0 {
		return fmt.Errorf("%w, then run this command again", errBackupWorkInFlight(busy))
	}
	liveKey, err := readRepositoryKey(client, repositoryKeySecret)
	if err != nil {
		return err
	}
	if liveKey == nil {
		return fmt.Errorf("this cluster has no backup repository key in Secret velero/%s", repositoryKeySecret)
	}
	previous := StorageLocation{Bucket: live.bucket, Region: live.region, Endpoint: live.s3URL, SSEC: live.sseKey != ""}
	if err := opts.Confirm(moveSummary(previous, target, IsDefaultRepositoryKey(liveKey), opts)); err != nil {
		return err
	}

	if err := refusePopulatedTarget(client, target, dest); err != nil {
		return err
	}
	if _, err := client.Run("kubectl -n velero delete secret " + nextKeySecret + " --ignore-not-found"); err != nil {
		return fmt.Errorf("removing a staged key left by an earlier move: %w", err)
	}
	rec.Move = &BackupStorageMove{Target: target, Previous: previous, Phase: movePreparing,
		StartedAt: opts.Now().UTC().Format(time.RFC3339), SSEGenerated: target.SSEC && !sseHeld}
	if err := writeStorageRecord(client, rec); err != nil {
		return err
	}
	return withResumeHint(runMove(client, opts, &rec, dest), &rec)
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

func requireSSECKeyFlag(sseC bool, path string) error {
	switch {
	case sseC && path == "":
		return errors.New("--export-sse-c-key-to is required with SSE-C, because the new bucket's backups cannot be read without that key")
	case !sseC && path != "":
		return errors.New("--export-sse-c-key-to works only with --sse-c")
	}
	return nil
}

func moveSummary(from, to StorageLocation, defaultKey bool, opts MoveOptions) []string {
	key := "this cluster's own key"
	if defaultKey {
		key = "Velero's published default key"
	}
	lines := []string{
		fmt.Sprintf("From: bucket %s, volume data encrypted with %s", from, key),
		fmt.Sprintf("To:   bucket %s, a new repository key, SSE-C %s", to, onOff(to.SSEC)),
		"kip first checks that the new bucket holds no backups.",
		"Velero stops for a few minutes. A scheduled backup missed during the pause runs once in the new bucket afterward.",
		"Existing backups stay in the old bucket. This cluster stops creating and deleting backups there.",
		"Read the old backups with a read-only recovery install (kip install --backup-storage-read-only) and the previous key file.",
		"Key files: previous key " + opts.PreviousKeyFile + ", new key " + opts.NewKeyFile,
	}
	if to.SSEC {
		lines = append(lines, "SSE-C key file: "+opts.SSECKeyFile)
	}
	return lines
}

// runMove resumes the move from its recorded phase.
func runMove(client CommandRunner, opts MoveOptions, rec *StorageRecord, dest backupTarget) error {
	m := rec.Move
	if err := requireSSECKeyFlag(m.Target.SSEC, opts.SSECKeyFile); err != nil {
		return err
	}
	if m.Phase == moveSwitched {
		if err := checkSwitchedExports(client, opts, m); err != nil {
			return err
		}
		if busy, err := backupWorkInFlight(client); err != nil {
			return err
		} else if len(busy) > 0 {
			return errBackupWorkInFlight(busy)
		}
		return finishMove(client, opts, rec)
	}

	previousKey, err := stagePreviousKey(client, opts, m.Phase)
	if err != nil {
		return err
	}
	nextKey, err := readRepositoryKey(client, nextKeySecret)
	if err != nil {
		return err
	}
	if nextKey == nil {
		if m.Phase != movePreparing {
			return fmt.Errorf("the staged key Secret velero/%s is missing after the switch began, so kip cannot finish the move", nextKeySecret)
		}
		if _, err := client.Run(generateKeySecretCmd(nextKeySecret)); err != nil {
			return fmt.Errorf("creating the new backup repository key: %w", err)
		}
		if nextKey, err = readRepositoryKey(client, nextKeySecret); err != nil {
			return err
		}
	}
	if nextKey == nil || sameKey(nextKey, previousKey) || IsDefaultRepositoryKey(nextKey) {
		return errors.New("the staged repository key is missing, the published default, or the previous key, so kip stopped before switching")
	}
	if err := exportKeyFile("--export-new-key-to", opts.NewKeyFile, nextKey, false); err != nil {
		return err
	}
	if m.Target.SSEC {
		if err := stageSSECKey(client, opts, m, dest); err != nil {
			return err
		}
	}

	if err := stopVeleroWhenIdle(client, m.Phase == movePreparing); err != nil {
		return err
	}
	if m.Phase == movePreparing {
		m.Phase = moveCutover
		if err := writeStorageRecord(client, *rec); err != nil {
			return err
		}
	}
	_, _ = fmt.Fprintf(opts.Out, "  ✔  Velero stopped\n")
	if _, err := client.Run("kubectl -n velero delete backups.velero.io,backuprepositories.velero.io,podvolumebackups.velero.io,deletebackuprequests.velero.io --all --wait=false"); err != nil {
		return fmt.Errorf("removing the old location's backup records: %w", err)
	}
	liveKey, err := readRepositoryKey(client, repositoryKeySecret)
	if err != nil {
		return err
	}
	if !sameKey(liveKey, nextKey) {
		if !sameKey(liveKey, previousKey) {
			return fmt.Errorf("the key in Secret velero/%s is neither the previous nor the new key, so kip stopped. Its value changed outside this move", repositoryKeySecret)
		}
		if _, err := client.RunStdin(applyRepositoryKeyCmd, bytes.NewReader(nextKey)); err != nil {
			return errors.New("writing the new backup repository key failed")
		}
		if liveKey, err = readRepositoryKey(client, repositoryKeySecret); err != nil {
			return err
		}
		if !sameKey(liveKey, nextKey) {
			return errors.New("the backup repository key did not change to the new key")
		}
	}
	_, _ = fmt.Fprintf(opts.Out, "  ✔  New repository key in place\n")
	return finishMove(client, opts, rec)
}

// stagePreviousKey keeps a copy of the key the old location uses and exports
// it. While preparing, the copy is made from the live key and must match it.
// Once the switch begins, the live key may be new, so the saved copy is required.
func stagePreviousKey(client CommandRunner, opts MoveOptions, phase string) ([]byte, error) {
	previousKey, err := readRepositoryKey(client, previousKeySecret)
	if err != nil {
		return nil, err
	}
	if phase == movePreparing {
		liveKey, err := readRepositoryKey(client, repositoryKeySecret)
		if err != nil {
			return nil, err
		}
		if liveKey == nil {
			return nil, fmt.Errorf("this cluster has no backup repository key in Secret velero/%s", repositoryKeySecret)
		}
		if previousKey == nil {
			if _, err := client.RunStdin(createKeySecretCmd(previousKeySecret), bytes.NewReader(liveKey)); err != nil {
				return nil, errors.New("copying the current backup repository key failed")
			}
			previousKey = liveKey
		} else if !sameKey(previousKey, liveKey) {
			return nil, fmt.Errorf("the key in Secret velero/%s differs from the live backup repository key, so kip stopped. Run kip backup storage set --abort, then start the move again", previousKeySecret)
		}
	}
	if previousKey == nil {
		return nil, fmt.Errorf("the copy of the previous key, Secret velero/%s, is missing, so kip cannot finish the move", previousKeySecret)
	}
	if err := exportKeyFile("--export-previous-key-to", opts.PreviousKeyFile, previousKey, false); err != nil {
		return nil, err
	}
	if IsDefaultRepositoryKey(previousKey) {
		_, _ = fmt.Fprintf(opts.Out, "  ⚠  The previous key is Velero's published default, so the old bucket's volume data is protected only by access to the bucket\n")
	}
	_, _ = fmt.Fprintf(opts.Out, "  ✔  Previous repository key saved to %s\n", opts.PreviousKeyFile)
	return previousKey, nil
}

// stageSSECKey reuses the cluster's SSE-C key or generates one during
// preparation, then tests it on the new bucket and exports it.
func stageSSECKey(client CommandRunner, opts MoveOptions, m *BackupStorageMove, dest backupTarget) error {
	held, err := sseKeyExists(client)
	if err != nil {
		return err
	}
	if !held {
		if !m.SSEGenerated || m.Phase != movePreparing {
			return fmt.Errorf("the SSE-C key Secret velero/%s is missing, so kip cannot finish the move", sseKeySecret)
		}
		if _, err := client.Run(generateSSEKeyCmd); err != nil {
			return fmt.Errorf("creating the backup SSE-C key: %w", err)
		}
	}
	if err := runSSEWriteTest(client, dest, sseKeySecret); err != nil {
		return err
	}
	key, err := readKeySecret(client, sseKeySecret, sseKeyField)
	if err != nil {
		return err
	}
	if len(key) != sseKeyBytes {
		return fmt.Errorf("the SSE-C key Secret velero/%s does not hold %d bytes", sseKeySecret, sseKeyBytes)
	}
	return exportKeyFile("--export-sse-c-key-to", opts.SSECKeyFile, key, true)
}

// stopVeleroWhenIdle stops the Velero server between two checks for running
// work. If either check finds work and restart is true, it starts Velero again.
// If work is found and restart is false, it leaves Velero at its current replica
// count because the live key may already have changed.
func stopVeleroWhenIdle(client CommandRunner, restart bool) error {
	busy, err := backupWorkInFlight(client)
	if err != nil {
		return err
	}
	if len(busy) == 0 {
		if err := stopVelero(client); err != nil {
			return err
		}
		if busy, err = backupWorkInFlight(client); err != nil {
			return err
		}
	}
	if len(busy) == 0 {
		return nil
	}
	if restart {
		if _, err := client.Run("kubectl -n velero scale deploy/velero --replicas=1"); err != nil {
			return fmt.Errorf("%w. Starting Velero again also failed: %w", errBackupWorkInFlight(busy), err)
		}
	}
	return errBackupWorkInFlight(busy)
}

// checkSwitchedExports checks the export files once the live key is the new
// one. The staged copy may already be gone, so the live key is the reference.
func checkSwitchedExports(client CommandRunner, opts MoveOptions, m *BackupStorageMove) error {
	if previousKey, err := readRepositoryKey(client, previousKeySecret); err != nil {
		return err
	} else if previousKey != nil {
		if err := exportKeyFile("--export-previous-key-to", opts.PreviousKeyFile, previousKey, false); err != nil {
			return err
		}
	}
	liveKey, err := readRepositoryKey(client, repositoryKeySecret)
	if err != nil {
		return err
	}
	if liveKey == nil {
		return fmt.Errorf("this cluster has no backup repository key in Secret velero/%s", repositoryKeySecret)
	}
	if err := exportKeyFile("--export-new-key-to", opts.NewKeyFile, liveKey, false); err != nil {
		return err
	}
	if !m.Target.SSEC {
		return nil
	}
	key, err := readKeySecret(client, sseKeySecret, sseKeyField)
	if err != nil {
		return err
	}
	if len(key) != sseKeyBytes {
		return fmt.Errorf("the SSE-C key Secret velero/%s is missing, so kip cannot finish the move", sseKeySecret)
	}
	return exportKeyFile("--export-sse-c-key-to", opts.SSECKeyFile, key, true)
}

// finishMove points Velero at the new bucket, restarts it and the node agents
// so they read the new credentials, waits for the location to be available,
// and clears the move.
func finishMove(client CommandRunner, opts MoveOptions, rec *StorageRecord) error {
	m := rec.Move
	creds := (&AWSCredentials{AccessKeyID: opts.Target.AccessKeyID, SecretAccessKey: opts.Target.SecretAccessKey}).VeleroCredentialsBlock()
	if _, err := client.RunStdin(applyCloudCredentialsCmd, strings.NewReader(creds)); err != nil {
		return errors.New("writing the new bucket's credentials failed")
	}
	cfg := *opts.Target
	cfg.SSEC = m.Target.SSEC
	cfg.SSECKey = nil
	if err := applyVeleroChart(client, &cfg); err != nil {
		return err
	}
	if m.Phase != moveSwitched {
		m.Phase = moveSwitched
		current := m.Target
		rec.Current = &current
		rec.Previous = &PreviousLocation{StorageLocation: m.Previous, SwitchedAt: opts.Now().UTC().Format(time.RFC3339)}
		if err := writeStorageRecord(client, *rec); err != nil {
			return err
		}
	}
	if err := recordBackupStorage(opts.ClusterName, backupStorageRef(&cfg)); err != nil {
		return fmt.Errorf("saving the new backup location to the local kip configuration: %w", err)
	}

	dest, err := newBackupTarget(&cfg, "")
	if err != nil {
		return err
	}
	if err := waitForMove(func() (bool, string, error) {
		live, err := liveBackupLocation(client)
		return err == nil && !live.changesFrom(dest), "", err
	}, "Velero's backup location to name bucket "+m.Target.String()); err != nil {
		return err
	}
	for _, cmd := range []string{
		"kubectl -n velero scale deploy/velero --replicas=1",
		"kubectl -n velero rollout restart deploy/velero",
		"kubectl -n velero rollout restart ds/node-agent",
		"kubectl -n velero rollout status deploy/velero --timeout=" + moveWait.String(),
		"kubectl -n velero rollout status ds/node-agent --timeout=" + moveWait.String(),
	} {
		if _, err := client.Run(cmd); err != nil {
			return fmt.Errorf("restarting Velero with the new credentials: %w", err)
		}
	}
	_, _ = fmt.Fprintf(opts.Out, "  ✔  Velero restarted with the new bucket's credentials\n")
	if err := waitForMove(func() (bool, string, error) {
		out, err := client.Run("kubectl -n velero get backupstoragelocation default -o jsonpath='{.status.phase}|{.status.message}' 2>/dev/null")
		phase, message, _ := strings.Cut(strings.TrimSpace(out), "|")
		return err == nil && phase == "Available", message, err
	}, "bucket "+m.Target.String()+" to be available"); err != nil {
		return fmt.Errorf("%w. Fix the credentials or the bucket, then run this command again with --credentials", err)
	}

	rec.Move = nil
	if err := writeStorageRecord(client, *rec); err != nil {
		return err
	}
	if _, err := client.Run("kubectl -n velero delete secret " + nextKeySecret + " --ignore-not-found"); err != nil {
		return fmt.Errorf("removing the staged key: %w", err)
	}
	_, _ = fmt.Fprintf(opts.Out, "\n  ✔  Backups now go to bucket %s\n", m.Target)
	_, _ = fmt.Fprintf(opts.Out, "     Bucket %s keeps the earlier backups.\n", m.Previous)
	_, _ = fmt.Fprintf(opts.Out, "     Next: kip backup create, then kip backup drill <namespace>, then kip backup storage retire-previous.\n\n")
	return nil
}

// waitForMove polls check until it reports done, returns an error, or
// moveWait passes. A timeout includes the last message from check, if present.
func waitForMove(check func() (bool, string, error), what string) error {
	deadline := time.Now().Add(moveWait)
	for {
		done, message, err := check()
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		if time.Now().After(deadline) {
			if message != "" {
				return fmt.Errorf("timed out after %s waiting for %s: %s", moveWait, what, message)
			}
			return fmt.Errorf("timed out after %s waiting for %s", moveWait, what)
		}
		time.Sleep(backupMovePoll)
	}
}

// AbortBackupStorageMove removes a preparing move's staged keys and any SSE-C
// key it generated, restarts Velero at the old location, and clears the move entry.
// Exported key files and other storage record entries are preserved. It returns
// an error if no move is recorded or the switch has begun, or if cleanup fails.
// The caller must hold the node lifecycle lease and supply a non-nil writer.
func AbortBackupStorageMove(client CommandRunner, out io.Writer) error {
	rec, err := readStorageRecord(client)
	if err != nil {
		return err
	}
	if rec.Move == nil {
		return errors.New("no backup storage move is in progress")
	}
	if rec.Move.Phase != movePreparing {
		return fmt.Errorf("the switch to bucket %s has begun, so the move can only be finished: run kip backup storage set with --bucket %s again",
			rec.Move.Target, rec.Move.Target.Bucket)
	}
	secrets := nextKeySecret + " " + previousKeySecret
	if rec.Move.SSEGenerated {
		secrets += " " + sseKeySecret
	}
	if _, err := client.Run("kubectl -n velero delete secret " + secrets + " --ignore-not-found"); err != nil {
		return fmt.Errorf("removing the move's staged keys: %w", err)
	}
	if _, err := client.Run("kubectl -n velero scale deploy/velero --replicas=1"); err != nil {
		return fmt.Errorf("starting Velero again: %w", err)
	}
	target := rec.Move.Target
	rec.Move = nil
	if err := writeStorageRecord(client, rec); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "\n  ✔  The move to bucket %s was canceled. Backups keep going to the current bucket.\n\n", target)
	return nil
}

// RetirePreviousBackupStorage clears the previous location and deletes the
// cluster's copy of its repository key, leaving the exported file for recovery.
// The old bucket and its objects remain intact. A move in progress, a missing
// previous location, or lack of a completed backup created after the switch
// returns an error. Reading or parsing cluster state and cleanup can also fail.
//
// The caller must hold the node lifecycle lease and supply non-nil confirm and
// out values. The confirm callback receives a description and must return nil
// before cleanup proceeds.
func RetirePreviousBackupStorage(client CommandRunner, confirm func(summary []string) error, out io.Writer) error {
	rec, err := readStorageRecord(client)
	if err != nil {
		return err
	}
	switch {
	case rec.Move != nil:
		return fmt.Errorf("backups are moving to bucket %s. Finish the move first", rec.Move.Target)
	case rec.Previous == nil:
		return errors.New("this cluster records no previous backup location")
	}
	switchedAt, err := time.Parse(time.RFC3339, rec.Previous.SwitchedAt)
	if err != nil {
		return fmt.Errorf("reading when the move happened: %w", err)
	}
	list, err := client.Run(`kubectl -n velero get backups.velero.io -o jsonpath='{range .items[*]}{.status.phase} {.metadata.creationTimestamp}{"\n"}{end}' 2>/dev/null`)
	if err != nil {
		return fmt.Errorf("listing backups: %w", err)
	}
	completed := false
	for _, line := range strings.Split(list, "\n") {
		f := strings.Fields(line)
		if len(f) != 2 || f[0] != "Completed" {
			continue
		}
		if created, err := time.Parse(time.RFC3339, f[1]); err == nil && created.After(switchedAt) {
			completed = true
		}
	}
	if !completed {
		return errors.New("no backup has completed at the new location since the move. Run kip backup create and, ideally, kip backup drill <namespace> first")
	}
	if err := confirm([]string{
		"Previous location: bucket " + rec.Previous.String() + ", left at " + rec.Previous.SwitchedAt,
		"kip forgets this location and deletes the cluster's copy of its repository key. Keep the file exported during the move to read the old backups.",
		"The bucket and its contents stay intact. Delete the bucket and revoke its credentials at the provider when you no longer need its backups.",
	}); err != nil {
		return err
	}
	if _, err := client.Run("kubectl -n velero delete secret " + previousKeySecret + " --ignore-not-found"); err != nil {
		return fmt.Errorf("deleting the copy of the previous key: %w", err)
	}
	previous := rec.Previous
	rec.Previous = nil
	if err := writeStorageRecord(client, rec); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "\n  ✔  kip no longer tracks bucket %s.\n\n", previous.StorageLocation)
	return nil
}
