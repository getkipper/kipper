package installer

import (
	"bytes"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"
)

const (
	// sseKeySecret names the Secret holding 32 raw key bytes in sseKeyField
	// for direct object reads and writes through Velero's AWS plugin.
	sseKeySecret = "kipper-backup-sse-key" //nolint:gosec // G101: Kubernetes Secret object name, not a credential value
	sseKeyField  = "customer-key"
	// sseCandidateSecret holds a key file's key while kip checks it against
	// the bucket, before it replaces a key no backup location uses.
	sseCandidateSecret = "kipper-sse-key-candidate" //nolint:gosec // G101: Kubernetes Secret object name, not a credential value

	sseCheckJob = "kipper-sse-check"

	sseWriteOKMarker     = "KIPPER_SSE_OK"
	sseKeyMatchMarker    = "KIPPER_SSE_KEY_MATCH"
	sseKeyWrongMarker    = "KIPPER_SSE_KEY_MISMATCH"
	ssePlainMarker       = "KIPPER_SSE_PLAIN"
	sseNoBackupsMarker   = "KIPPER_SSE_NO_BACKUPS"
	sseEncryptedMarker   = "KIPPER_SSE_ENCRYPTED"
	sseFailedMarker      = "KIPPER_SSE_FAILED"
	sseUnsupportedMarker = "KIPPER_SSE_UNSUPPORTED"

	sseKeyBytes = 32
)

// ReadSSECKeyFile returns the 32 raw bytes of an exported SSE-C key. It reads
// the first 128 bytes of a regular file as standard base64, ignoring CR and LF.
// On systems other than Windows, it rejects group or other permissions.
func ReadSSECKeyFile(path string) ([]byte, error) {
	return readSSECKeyFile("--backup-storage-sse-c-key-file", path)
}

// readSSECKeyFile is ReadSSECKeyFile with errors naming flag.
func readSSECKeyFile(flag, path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", flag, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s %s is not a regular file", flag, path)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s %s grants permissions to other users. Run chmod 600 %s", flag, path, path)
	}
	f, err := os.Open(path) //nolint:gosec // path comes from a user flag at install time
	if err != nil {
		return nil, fmt.Errorf("%s: %w", flag, err)
	}
	defer func() { _ = f.Close() }()
	text, err := io.ReadAll(io.LimitReader(f, 128))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", flag, err)
	}
	key, err := DecodeSSECKey(strings.TrimRight(string(text), "\r\n"))
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", flag, path, err)
	}
	return key, nil
}

// DecodeSSECKey decodes the base64 text form of an SSE-C key and requires
// exactly 32 bytes. Errors never include the key.
func DecodeSSECKey(text string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(text)
	if err != nil || len(key) != sseKeyBytes {
		return nil, errors.New("the key must be the base64 encoding of 32 bytes, as openssl rand -base64 32 prints")
	}
	return key, nil
}

// EncodeSSECKey returns standard padded base64 without a trailing newline.
// A 32-byte key produces the format ReadSSECKeyFile accepts. It does not
// validate the key length.
func EncodeSSECKey(key []byte) string { return base64.StdEncoding.EncodeToString(key) }

// prepareBackupSSEC prepares the key before Velero starts and reports whether
// to enable SSE-C, returning an operator notice or an empty string. Active keys
// keep SSE-C enabled across upgrades without a flag. A missing active key must
// be supplied again to prevent plain writes. New and interrupted activations
// require a bucket check, a write test, and a key check against existing backup
// metadata. Moving an active key to another bucket requires a write test too.
// Failed activation attempts to remove keys created by this run and retains
// older keys because backups elsewhere may need them. Without an SSE-C setting,
// enableSSECWhereSupported decides whether to try an unused key.
func prepareBackupSSEC(client CommandRunner, cfg *BackupStorageConfig, target backupTarget, check *bucketCheck) (bool, string, error) {
	exists, err := sseKeyExists(client)
	if err != nil {
		return false, "", err
	}
	live, err := liveBackupLocation(client)
	if err != nil {
		return false, "", err
	}
	requested := cfg != nil && (cfg.SSEC || cfg.SSECKey != nil)
	if !exists && !requested && live.sseKey != "" {
		return false, "", fmt.Errorf("this cluster's backups use SSE-C, but its key Secret velero/%s is missing. "+
			"Re-run kip install with --backup-storage-sse-c-key-file and the key saved by kip backup sse-key export", sseKeySecret)
	}
	if !requested && live.sseKey == "" {
		held := exists
		unused := ""
		if exists && !triesSSECAutomatically(cfg, live, target) {
			// Keep the unused key because it may protect backups elsewhere.
			exists = false
			unused = sseWarning("Secret velero/%s holds an SSE-C key that no backup location uses. kip keeps it because backups "+
				"elsewhere may need it. Save it with kip backup sse-key export", sseKeySecret)
		}
		if err := refusePlainOverSSECBackups(client, cfg, target, check, live, held); err != nil {
			return false, "", err
		}
		on, notice, err := enableSSECWhereSupported(client, cfg, target, check, live, exists)
		return on, unused + notice, err
	}
	if err := CheckSSECStorage(cfg); err != nil {
		if cfg == nil && exists {
			return false, "", fmt.Errorf("this cluster's backups use SSE-C (Secret velero/%s), which in-cluster MinIO cannot serve. "+
				"Keep the external backup storage", sseKeySecret)
		}
		return false, "", err
	}
	if cfg.SSECKey != nil && len(cfg.SSECKey) != sseKeyBytes {
		return false, "", fmt.Errorf("the SSE-C key must be %d bytes", sseKeyBytes)
	}

	if exists {
		if cfg.SSECKey != nil {
			same, err := sameSSECKey(client, cfg.SSECKey)
			if err != nil {
				return false, "", err
			}
			if !same && live.sseKey != "" {
				return false, "", errors.New("--backup-storage-sse-c-key-file differs from the SSE-C key this cluster's backup location uses. " +
					"Changing it would make the existing backups unreadable, so kip keeps the current one")
			}
			if !same {
				notice, err := replaceUnusedSSECKey(client, cfg.SSECKey, target, check)
				return err == nil, notice, err
			}
		}
		if _, err := client.Run("kubectl -n velero label secret " + sseKeySecret + " " + excludeFromBackupLabel + " --overwrite"); err != nil {
			return false, "", fmt.Errorf("labeling the backup SSE-C key: %w", err)
		}
		if live.sseKey != "" {
			notice := ""
			if cfg.SSECOff {
				notice = sseWarning("This cluster's backups use SSE-C, which stays on despite --backup-storage-sse-c=false, because the backups already written with it need the key")
			}
			if live.changesFrom(target) {
				if err := runSSEWriteTest(client, target, sseKeySecret); err != nil {
					return false, "", err
				}
			}
			return true, notice, nil
		}
		if err := activateSSEC(client, target, check, false, sseKeySecret); err != nil {
			return false, "", err
		}
		if cfg.SSECKey == nil {
			return true, sseKeyExportNotice, nil
		}
		return true, "", nil
	}

	if cfg.SSECKey != nil {
		if _, err := client.RunStdin(createSSEKeyCmd, bytes.NewReader(cfg.SSECKey)); err != nil {
			return false, "", errors.New("creating the backup SSE-C key failed")
		}
		if err := activateSSEC(client, target, check, true, sseKeySecret); err != nil {
			return false, "", discardNewSSECKey(client, err)
		}
		return true, "", nil
	}
	populated, err := check.populated()
	if err != nil {
		return false, "", fmt.Errorf("SSE-C can only be turned on for a bucket kip has checked for existing backups: %w", err)
	}
	if populated {
		return false, "", fmt.Errorf("bucket %s already holds backups, so SSE-C cannot be turned on for it. "+
			"Use an empty bucket, or pass --backup-storage-sse-c-key-file with the key its backups were written with", target.bucket)
	}
	if _, err := client.Run(generateSSEKeyCmd); err != nil {
		return false, "", fmt.Errorf("creating the backup SSE-C key: %w", err)
	}
	if err := activateSSEC(client, target, check, false, sseKeySecret); err != nil {
		return false, "", discardNewSSECKey(client, err)
	}
	return true, sseKeyExportNotice, nil
}

// replaceUnusedSSECKey validates the imported key in sseCandidateSecret before
// replacing an unused key. It preserves the old key in a separate Secret and
// returns that Secret's name in the notice. Candidate cleanup failures name
// the candidate Secret in the notice or error.
func replaceUnusedSSECKey(client CommandRunner, key []byte, target backupTarget, check *bucketCheck) (string, error) {
	removeCandidate := "kubectl -n velero delete secret " + sseCandidateSecret + " --ignore-not-found"
	if _, err := client.Run(removeCandidate); err != nil {
		return "", fmt.Errorf("removing the SSE-C key an earlier run was checking: %w", err)
	}
	if _, err := client.RunStdin(createSSEKeyCmdFor(sseCandidateSecret), bytes.NewReader(key)); err != nil {
		return "", errors.New("storing the key file's SSE-C key for its check failed")
	}
	notice, err := moveUnusedSSECKeyAside(client, key, target, check)
	if _, rmErr := client.Run(removeCandidate); rmErr != nil {
		if err != nil {
			return "", fmt.Errorf("%w. Removing Secret velero/%s, which holds a copy of the key file's key, also failed: %w", err, sseCandidateSecret, rmErr)
		}
		notice += sseWarning("Removing Secret velero/%s, which holds a copy of the key file's key, failed: %v", sseCandidateSecret, rmErr)
	}
	return notice, err
}

// moveUnusedSSECKeyAside checks the key file's key held in sseCandidateSecret,
// then copies the key no backup location uses to a Secret of its own and puts
// the file's key in its place.
func moveUnusedSSECKeyAside(client CommandRunner, key []byte, target backupTarget, check *bucketCheck) (string, error) {
	if err := activateSSEC(client, target, check, true, sseCandidateSecret); err != nil {
		return "", err
	}
	previous := sseKeySecret + "-previous-" + time.Now().UTC().Format("20060102150405")
	copyCmd := `k=$(kubectl -n velero get secret ` + sseKeySecret + ` -o jsonpath='{.data.` + sseKeyField + `}') && [ ${#k} -eq 44 ] && ` +
		`printf '%s' "$k" | base64 -d | ` + createSSEKeyCmdFor(previous)
	// Errors from either Secret write can contain key material, so report only
	// the recovery instructions.
	if _, err := client.Run(copyCmd); err != nil {
		return "", fmt.Errorf("copying the unused SSE-C key to Secret velero/%s failed, so kip keeps it in place "+
			"and leaves the key file unused", previous)
	}
	if _, err := client.RunStdin(replaceSSEKeyCmd, bytes.NewReader(key)); err != nil {
		return "", fmt.Errorf("replacing the SSE-C key with the key from the file failed. Check Secret velero/%s to see which key it holds. "+
			"Secret velero/%s holds a copy of the old key", sseKeySecret, previous)
	}
	return sseWarning("The key file replaced an SSE-C key that no backup location used. kip saved the old key in Secret velero/%s "+
		"because backups elsewhere may need it. Save it with kip backup sse-key export --previous %s --to <file>", previous, previous), nil
}

// triesSSECAutomatically reports whether automatic activation can be attempted.
// It requires credentials for a writable HTTPS store and no live location at
// the target, where Velero could keep writing plain objects during the checks.
func triesSSECAutomatically(cfg *BackupStorageConfig, live liveLocation, target backupTarget) bool {
	return cfg != nil && !cfg.SSECOff && !cfg.ReadOnly && cfg.AccessKeyID != "" && CheckSSECStorage(cfg) == nil &&
		(live.bucket == "" || live.changesFrom(target))
}

// enableSSECWhereSupported handles a location without an explicit SSE-C request
// or active key. Automatic activation requires credentials, an HTTPS endpoint,
// an empty bucket, and a successful write test. It reuses the key when keyExists
// is true and generates one otherwise. Unsupported storage leaves SSE-C off
// with a warning, and a read-only location keeps its current setting.
// Failed checks and key operations stop the install.
// Native AWS S3 uses SSE-S3 instead. Upgrades without credentials keep their
// current settings. A live location at the target stays unchanged because
// Velero could write plain objects while kip tests and switches it.
func enableSSECWhereSupported(client CommandRunner, cfg *BackupStorageConfig, target backupTarget, check *bucketCheck, live liveLocation, keyExists bool) (bool, string, error) {
	switch {
	case cfg == nil:
		return false, sseWarning("Backups go to MinIO inside the cluster, which keeps Velero's own objects, including every Secret, " +
			"unencrypted on the server's disk. Use external storage with SSE-C, or native AWS S3, to encrypt them"), nil
	case cfg.Endpoint == "":
		return false, "", nil
	case cfg.SSECOff:
		return false, sseWarning("SSE-C is off (--backup-storage-sse-c=false), so bucket %s holds Velero's own objects, "+
			"including every Secret, unencrypted unless the store encrypts them itself", cfg.Bucket), nil
	case cfg.AccessKeyID == "", cfg.ReadOnly:
		return false, "", nil
	case live.bucket != "" && !live.changesFrom(target):
		return false, sseWarning("SSE-C stays off for bucket %s, because this cluster already writes backups to it without SSE-C. "+
			"To turn it on, move the backups to an empty bucket", cfg.Bucket), nil
	}
	if err := CheckSSECStorage(cfg); err != nil {
		return false, sseWarning("SSE-C stays off for bucket %s: %v", cfg.Bucket, err), nil
	}
	populated, err := check.populated()
	if err != nil {
		return false, "", err
	}
	if populated {
		return false, sseWarning("SSE-C stays off, because bucket %s already holds backup data written without it. "+
			"Use an empty bucket to turn it on", cfg.Bucket), nil
	}
	if !keyExists {
		if _, err := client.Run(generateSSEKeyCmd); err != nil {
			return false, "", fmt.Errorf("creating the backup SSE-C key: %w", err)
		}
	}
	activateErr := activateSSEC(client, target, check, false, sseKeySecret)
	if activateErr == nil {
		return true, sseKeyExportNotice, nil
	}
	stop := func(err error) error {
		return fmt.Errorf("%w. Retry, or pass --backup-storage-sse-c=false if the store refuses SSE-C", err)
	}
	if !keyExists {
		activateErr = discardNewSSECKey(client, activateErr)
		if left, err := sseKeyExists(client); err != nil || left {
			return false, "", stop(activateErr)
		}
	}
	// Fall back only when the test demonstrates incompatible SSE-C behavior.
	// Other failures leave support unknown and must stop the install.
	var unsupported *sseUnsupportedError
	if !errors.As(activateErr, &unsupported) {
		return false, "", stop(activateErr)
	}
	return false, sseWarning("SSE-C stays off: %v. To require it, pass --backup-storage-sse-c with an empty bucket", activateErr), nil
}

// sseKeyExportNotice prompts users to preserve a generated or reused key
// outside the cluster after activation.
const sseKeyExportNotice = "       Save your backup SSE-C key outside the cluster: kip backup sse-key export --to <file>\n"

func sseWarning(format string, args ...any) string {
	return "  ⚠  " + fmt.Sprintf(format, args...) + "\n"
}

func sseKeyExists(client CommandRunner) (bool, error) {
	out, err := client.Run("kubectl -n velero get secret " + sseKeySecret + " --ignore-not-found -o name 2>/dev/null")
	if err != nil {
		return false, fmt.Errorf("checking for the backup SSE-C key: %w", err)
	}
	return strings.TrimSpace(out) == "secret/"+sseKeySecret, nil
}

// activateSSEC validates an unused key in keySecret with a write test and, for
// a populated bucket, a check of one backup metadata object. Read-only
// locations skip the write test. It leaves the Secret in place either way. The
// supplied flag identifies an imported key in errors.
func activateSSEC(client CommandRunner, target backupTarget, check *bucketCheck, supplied bool, keySecret string) error {
	populated, err := check.populated()
	if err != nil {
		return fmt.Errorf("SSE-C can only be turned on for a bucket kip has checked for existing backups: %w", err)
	}
	if !target.readOnly {
		if err := runSSEWriteTest(client, target, keySecret); err != nil {
			return err
		}
	}
	if populated {
		return runSSEKeyCheck(client, target, supplied, keySecret)
	}
	return nil
}

// discardNewSSECKey removes the key Secret this run created after its
// activation failed, so the next run does not find an untested key. It returns
// checkErr itself when the removal succeeded.
func discardNewSSECKey(client CommandRunner, checkErr error) error {
	if _, err := client.Run("kubectl -n velero delete secret " + sseKeySecret + " --ignore-not-found"); err != nil {
		return fmt.Errorf("%w. Removing the unused SSE-C key also failed: %w", checkErr, err)
	}
	return checkErr
}

// refusePlainOverSSECBackups checks external storage when the cluster has no
// live backup location. It rejects backups detected as requiring SSE-C and
// failed checks, so a new cluster cannot mix plain objects into those backups.
// keyHeld adds that the cluster's own key Secret may be the one they need.
func refusePlainOverSSECBackups(client CommandRunner, cfg *BackupStorageConfig, target backupTarget, check *bucketCheck, live liveLocation, keyHeld bool) error {
	if cfg == nil || live.bucket != "" {
		return nil
	}
	encrypted, err := bucketHoldsSSECBackups(client, target, check)
	if err != nil {
		return err
	}
	if encrypted && keyHeld {
		return fmt.Errorf("the backups in bucket %s were written with SSE-C. This cluster holds an unused SSE-C key in Secret velero/%s. "+
			"If they were written with it, pass --backup-storage-sse-c and kip checks it against them. "+
			"Otherwise pass --backup-storage-sse-c-key-file with the key saved by kip backup sse-key export", target.bucket, sseKeySecret)
	}
	if encrypted {
		return fmt.Errorf("the backups in bucket %s were written with SSE-C. Pass --backup-storage-sse-c-key-file with the key saved by kip backup sse-key export", target.bucket)
	}
	return nil
}

// bucketHoldsSSECBackups treats HTTP 400 or 403 from a keyless metadata HEAD
// request as evidence of SSE-C. These codes can also indicate other refusals.
// Accessible or absent metadata returns false. Listing failures and other
// HEAD failures return an error.
func bucketHoldsSSECBackups(client CommandRunner, target backupTarget, check *bucketCheck) (bool, error) {
	populated, err := check.populated()
	if err != nil {
		return false, fmt.Errorf("kip has to check bucket %s for backups written with SSE-C before Velero starts: %w", target.bucket, err)
	}
	if !populated {
		return false, nil
	}
	logs, err := runCheckJob(client, target, sseCheckJob, "the SSE-C check", checkJobManifest(sseCheckJob, target.bucket, ssePlainProbeScript, ""))
	if err != nil {
		return false, err
	}
	switch {
	case strings.Contains(logs, sseEncryptedMarker):
		return true, nil
	case strings.Contains(logs, ssePlainMarker), strings.Contains(logs, sseNoBackupsMarker):
		return false, nil
	}
	return false, fmt.Errorf("checking whether the backups in bucket %s use SSE-C failed: %s", target.bucket, strings.TrimSpace(logs))
}

// CheckSSECStorage requires a non-nil config with an HTTPS endpoint. In-cluster
// MinIO uses HTTP, which cannot carry SSE-C keys. An empty endpoint selects
// native AWS S3, for which kip requests SSE-S3 and new buckets block SSE-C by
// default. This checks configuration only. The install tests the store separately.
func CheckSSECStorage(cfg *BackupStorageConfig) error {
	if cfg == nil {
		return errors.New("SSE-C needs external backup storage. Pass --backup-storage-bucket, --backup-storage-region and --backup-storage-endpoint")
	}
	if cfg.Endpoint == "" {
		return errors.New("SSE-C is for S3-compatible stores such as Hetzner Object Storage. " +
			"Native AWS S3 backups are already encrypted with SSE-S3, so leave --backup-storage-sse-c off")
	}
	if !strings.HasPrefix(strings.ToLower(cfg.Endpoint), "https://") {
		return fmt.Errorf("SSE-C needs an HTTPS endpoint, because S3 refuses customer keys sent over plain HTTP. Got %q", cfg.Endpoint)
	}
	return nil
}

// createSSEKeyCmd reads the 32 raw key bytes from stdin, so the key never
// appears in a command line. kubectl create fails if a Secret
// appeared in the meantime, preserving that Secret.
var createSSEKeyCmd = createSSEKeyCmdFor(sseKeySecret)

// replaceSSEKeyCmd puts the 32 raw key bytes from stdin into the existing key
// Secret, and fails if the Secret is gone.
var replaceSSEKeyCmd = sseKeySecretManifestCmd(sseKeySecret) + " | kubectl replace -f -"

func createSSEKeyCmdFor(name string) string {
	return sseKeySecretManifestCmd(name) + " | kubectl create -f -"
}

// sseKeySecretManifestCmd prints the manifest of a key Secret named name,
// labeled out of backups, from 32 raw key bytes on stdin.
func sseKeySecretManifestCmd(name string) string {
	return "kubectl -n velero create secret generic " + name +
		" --from-file=" + sseKeyField + "=/dev/stdin --dry-run=client -o yaml" +
		" | kubectl label --local -f - " + excludeFromBackupLabel + " -o yaml"
}

// generateSSEKeyCmd creates the key from 32 random bytes on the server, so it
// never crosses the network. The length check stops a failed read from
// producing a short key.
var generateSSEKeyCmd = `k=$(head -c 32 /dev/urandom | base64 | tr -d '\n') && [ ${#k} -eq 44 ] && printf '%s' "$k" | base64 -d | ` + createSSEKeyCmd

// sameSSECKey compares key with the one in the key Secret in constant time.
// Errors are fixed strings, so no key material reaches a diagnostic.
func sameSSECKey(client CommandRunner, key []byte) (bool, error) {
	out, err := client.Run("kubectl -n velero get secret " + sseKeySecret + " -o jsonpath='{.data." + sseKeyField + "}' 2>/dev/null")
	if err != nil {
		return false, errors.New("reading the backup SSE-C key failed")
	}
	held, err := base64.StdEncoding.DecodeString(strings.TrimSpace(out))
	if err != nil {
		return false, errors.New("the backup SSE-C key is not valid base64")
	}
	return subtle.ConstantTimeCompare(held, key) == 1, nil
}

// runSSEWriteTest writes and reads a test object with the key, then requires
// an HTTP 400 or 403 response to a HEAD request without it. This checks that
// the store enforces key access. Cleanup attempts to delete the test object.
func runSSEWriteTest(client CommandRunner, target backupTarget, keySecret string) error {
	if target.mcHost == "" {
		if target.noList != "" {
			return fmt.Errorf("kip has to test SSE-C on bucket %s first: %s", target.bucket, target.noList)
		}
		return fmt.Errorf("kip has to test SSE-C on bucket %s first and has no credentials for it. Re-run kip install with the storage credentials", target.bucket)
	}
	logs, err := runCheckJob(client, target, sseCheckJob, "the SSE-C write test", sseJobManifest(target.bucket, sseWriteTestScript, keySecret))
	if err != nil {
		return err
	}
	switch {
	case strings.Contains(logs, sseWriteOKMarker):
		return nil
	case strings.Contains(logs, sseUnsupportedMarker):
		_, reason, _ := strings.Cut(logs, sseUnsupportedMarker)
		return &sseUnsupportedError{fmt.Sprintf("bucket %s did not pass the SSE-C test, because %s. The store may not support SSE-C",
			target.bucket, strings.TrimSpace(reason))}
	}
	return fmt.Errorf("the SSE-C test of bucket %s could not run: %s", target.bucket, strings.TrimSpace(logs))
}

// sseUnsupportedError identifies incompatible SSE-C behavior demonstrated by
// the write test, allowing automatic activation to fall back to plain writes.
type sseUnsupportedError struct{ msg string }

func (e *sseUnsupportedError) Error() string { return e.msg }

// runSSEKeyCheck checks one backup metadata object with HEAD requests. It
// succeeds when the keyless request is refused and the keyed request succeeds,
// or when no metadata is found. The supplied flag names the key file
// in errors.
func runSSEKeyCheck(client CommandRunner, target backupTarget, supplied bool, keySecret string) error {
	logs, err := runCheckJob(client, target, sseCheckJob, "the SSE-C key check", sseJobManifest(target.bucket, sseKeyCheckScript, keySecret))
	if err != nil {
		return err
	}
	switch {
	case strings.Contains(logs, sseKeyMatchMarker), strings.Contains(logs, sseNoBackupsMarker):
		// Kopia data uses the repository key, so it alone cannot validate an SSE-C key.
		return nil
	case strings.Contains(logs, sseKeyWrongMarker) && supplied:
		return fmt.Errorf("--backup-storage-sse-c-key-file does not open the backups in bucket %s", target.bucket)
	case strings.Contains(logs, sseKeyWrongMarker):
		return fmt.Errorf("the SSE-C key this cluster holds does not open the backups in bucket %s. kip keeps it unused, because backups elsewhere may need it", target.bucket)
	case strings.Contains(logs, ssePlainMarker) && supplied:
		return fmt.Errorf("the backups in bucket %s were written without SSE-C, so they cannot be read with --backup-storage-sse-c-key-file. "+
			"Install without the SSE-C flags to recover from them", target.bucket)
	case strings.Contains(logs, ssePlainMarker):
		return fmt.Errorf("the backups in bucket %s were written without SSE-C, so SSE-C cannot be turned on for it. "+
			"kip keeps the unused SSE-C key, and the next run without --backup-storage-sse-c keeps backups as they are", target.bucket)
	}
	return fmt.Errorf("checking the SSE-C key against bucket %s failed: %s", target.bucket, strings.TrimSpace(logs))
}

// sseKeyShell uses unpadded base64 after the object prefix, as mc requires.
const sseKeyShell = `K=$(base64 < /sse/` + sseKeyField + ` | tr -d '\n=') && E="kipper/$BUCKET/=$K"`

// sseStat classifies an mc HEAD request as "ok" on success or "refused" on
// HTTP 400 or 403, which can indicate a missing or incorrect SSE-C key. Other
// failures return their last output line so callers can report a failed check.
// These status codes alone cannot distinguish key errors from other refusals.
const sseStat = `stat() { S=$(mc stat "$@" 2>&1) && { echo ok; return; }; if echo "$S" | grep -qE '400 Bad Request|403 Forbidden'; then echo refused; else echo "$S" | tail -n 1; fi; }`

// sseFindBackup sets F to the first velero-backup.json found under backups,
// or empty if none is found or the prefix is absent. Other listing failures
// print sseFailedMarker and stop the script.
const sseFindBackup = `if mc find "kipper/$BUCKET/backups" --name velero-backup.json > /tmp/f 2> /tmp/e; then F=$(head -n 1 /tmp/f);` +
	` elif grep -q 'Object does not exist' /tmp/e; then F=;` +
	` else echo ` + sseFailedMarker + `; cat /tmp/e; exit 0; fi`

// sseFailIfError stops with sseFailedMarker and the error when $C is
// neither ok nor refused.
const sseFailIfError = `case "$C" in ok|refused) ;; *) echo ` + sseFailedMarker + ` "$C"; exit 0;; esac`

// sseWriteTestScript prints sseWriteOKMarker when a test object written with
// the key reads back with it and is refused without it. It prints
// sseUnsupportedMarker and a reason when the store serves the object without
// the key, returns different content with the key, or answers the keyed write
// with Not Implemented while accepting a plain write. Other request failures
// print the last error line. Setup failures may produce no marker.
var sseWriteTestScript = sseStat + `; ` + sseKeyShell + ` && O="kipper/$BUCKET/kipper-sse-check-$(date +%s)" && echo kipper > /tmp/o || exit 0` +
	`; if mc cp -q --enc-c "$E" /tmp/o "$O" > /tmp/e 2>&1; then` +
	` if R=$(mc cat --enc-c "$E" "$O" 2> /tmp/e); then C=$(stat "$O");` +
	` if [ "$R" = kipper ] && [ "$C" = refused ]; then echo ` + sseWriteOKMarker + `;` +
	` elif [ "$C" = ok ]; then echo ` + sseUnsupportedMarker + ` it served the test object without the key;` +
	` elif [ "$R" != kipper ]; then echo ` + sseUnsupportedMarker + ` it returned other content for the test object read with the key;` +
	` else echo "$C"; fi;` +
	` elif [ "$(mc cat "$O" 2> /dev/null)" = kipper ]; then echo ` + sseUnsupportedMarker + ` it refused to read the test object with the key and served it without;` +
	` else tail -n 1 /tmp/e; fi;` +
	` elif grep -qi 'not implemented' /tmp/e && mc cp -q /tmp/o "$O" > /dev/null 2>&1; then` +
	` echo ` + sseUnsupportedMarker + ` it accepts the test object without the key and answers the key with: "$(tail -n 1 /tmp/e)";` +
	` else tail -n 1 /tmp/e; fi;` +
	` mc rm -q "$O" > /dev/null 2>&1 || true`

var sseKeyCheckScript = sseStat + `; ` + sseKeyShell + `; ` + sseFindBackup +
	`; if [ -z "$F" ]; then echo ` + sseNoBackupsMarker + `; exit 0; fi` +
	`; C=$(stat "$F"); ` + sseFailIfError +
	`; if [ "$C" = ok ]; then echo ` + ssePlainMarker + `; exit 0; fi` +
	`; C=$(stat --enc-c "$E" "$F"); ` + sseFailIfError +
	`; if [ "$C" = ok ]; then echo ` + sseKeyMatchMarker + `; else echo ` + sseKeyWrongMarker + `; fi`

var ssePlainProbeScript = sseStat + `; ` + sseFindBackup +
	`; if [ -z "$F" ]; then echo ` + sseNoBackupsMarker + `; exit 0; fi` +
	`; C=$(stat "$F"); ` + sseFailIfError +
	`; if [ "$C" = refused ]; then echo ` + sseEncryptedMarker + `; else echo ` + ssePlainMarker + `; fi`

func sseJobManifest(bucket, script, keySecret string) string {
	return checkJobManifest(sseCheckJob, bucket, script, keySecret)
}
