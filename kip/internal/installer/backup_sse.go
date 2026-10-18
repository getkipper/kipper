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
)

const (
	// sseKeySecret names the Secret holding 32 raw key bytes in sseKeyField
	// for direct object reads and writes through Velero's AWS plugin.
	sseKeySecret = "kipper-backup-sse-key" //nolint:gosec // G101: Kubernetes Secret object name, not a credential value
	sseKeyField  = "customer-key"

	sseCheckJob = "kipper-sse-check"

	sseWriteOKMarker   = "KIPPER_SSE_OK"
	sseKeyMatchMarker  = "KIPPER_SSE_KEY_MATCH"
	sseKeyWrongMarker  = "KIPPER_SSE_KEY_MISMATCH"
	ssePlainMarker     = "KIPPER_SSE_PLAIN"
	sseNoBackupsMarker = "KIPPER_SSE_NO_BACKUPS"
	sseEncryptedMarker = "KIPPER_SSE_ENCRYPTED"
	sseFailedMarker    = "KIPPER_SSE_FAILED"

	sseKeyBytes = 32
)

// ReadSSECKeyFile returns the 32 raw bytes of an exported SSE-C key. It reads
// the first 128 bytes of a regular file as standard base64, ignoring CR and LF.
// On systems other than Windows, it rejects group or other permissions.
func ReadSSECKeyFile(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("--backup-storage-sse-c-key-file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("--backup-storage-sse-c-key-file %s is not a regular file", path)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("--backup-storage-sse-c-key-file %s grants permissions to other users. Run chmod 600 %s", path, path)
	}
	f, err := os.Open(path) //nolint:gosec // path comes from a user flag at install time
	if err != nil {
		return nil, fmt.Errorf("--backup-storage-sse-c-key-file: %w", err)
	}
	defer func() { _ = f.Close() }()
	text, err := io.ReadAll(io.LimitReader(f, 128))
	if err != nil {
		return nil, fmt.Errorf("--backup-storage-sse-c-key-file: %w", err)
	}
	key, err := DecodeSSECKey(strings.TrimRight(string(text), "\r\n"))
	if err != nil {
		return nil, fmt.Errorf("--backup-storage-sse-c-key-file %s: %w", path, err)
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
// to enable SSE-C. An active key keeps SSE-C enabled across upgrades without
// a flag. A missing active key must be supplied again to prevent plain writes.
// New keys and keys left by interrupted installs require a bucket check and
// a write test. Existing backup metadata must accept the key. Moving an active
// key to another bucket also requires a write test. Failed activation attempts
// to remove the unused Secret, leaving any exported recovery key intact.
func prepareBackupSSEC(client CommandRunner, cfg *BackupStorageConfig, target backupTarget, check *bucketCheck) (bool, error) {
	out, err := client.Run("kubectl -n velero get secret " + sseKeySecret + " --ignore-not-found -o name 2>/dev/null")
	if err != nil {
		return false, fmt.Errorf("checking for the backup SSE-C key: %w", err)
	}
	exists := strings.TrimSpace(out) == "secret/"+sseKeySecret
	live, err := liveBackupLocation(client)
	if err != nil {
		return false, err
	}
	requested := cfg != nil && (cfg.SSEC || cfg.SSECKey != nil)
	if !exists && !requested {
		if live.sseKey != "" {
			return false, fmt.Errorf("this cluster's backups use SSE-C, but its key Secret velero/%s is missing. "+
				"Re-run kip install with --backup-storage-sse-c-key-file and the key saved by kip backup sse-key export", sseKeySecret)
		}
		return false, refusePlainOverSSECBackups(client, cfg, target, check, live)
	}
	if err := CheckSSECStorage(cfg); err != nil {
		if cfg == nil && exists {
			return false, fmt.Errorf("this cluster's backups use SSE-C (Secret velero/%s), which in-cluster MinIO cannot serve. "+
				"Keep the external backup storage", sseKeySecret)
		}
		return false, err
	}
	if cfg.SSECKey != nil && len(cfg.SSECKey) != sseKeyBytes {
		return false, fmt.Errorf("the SSE-C key must be %d bytes", sseKeyBytes)
	}

	if exists {
		if cfg.SSECKey != nil {
			if err := requireSameSSECKey(client, cfg.SSECKey); err != nil {
				return false, err
			}
		}
		if _, err := client.Run("kubectl -n velero label secret " + sseKeySecret + " " + excludeFromBackupLabel + " --overwrite"); err != nil {
			return false, fmt.Errorf("labeling the backup SSE-C key: %w", err)
		}
		if live.sseKey != "" {
			if live.changesFrom(target) {
				if err := runSSEWriteTest(client, target); err != nil {
					return false, err
				}
			}
			return true, nil
		}
		return activateSSEC(client, target, check, false)
	}

	if cfg.SSECKey != nil {
		if _, err := client.RunStdin(createSSEKeyCmd, bytes.NewReader(cfg.SSECKey)); err != nil {
			return false, errors.New("creating the backup SSE-C key failed")
		}
	} else {
		populated, err := check.populated()
		if err != nil {
			return false, fmt.Errorf("SSE-C can only be turned on for a bucket kip has checked for existing backups: %w", err)
		}
		if populated {
			return false, fmt.Errorf("bucket %s already holds backups, so SSE-C cannot be turned on for it. "+
				"Use an empty bucket, or pass --backup-storage-sse-c-key-file with the key its backups were written with", target.bucket)
		}
		if _, err := client.Run(generateSSEKeyCmd); err != nil {
			return false, fmt.Errorf("creating the backup SSE-C key: %w", err)
		}
	}
	return activateSSEC(client, target, check, cfg.SSECKey != nil)
}

// activateSSEC validates an unused key with a write test and, for a populated
// bucket, a check of one backup metadata object. Failure attempts to remove
// the unused Secret. The supplied flag identifies an imported key in errors.
func activateSSEC(client CommandRunner, target backupTarget, check *bucketCheck, supplied bool) (bool, error) {
	checkErr := func() error {
		populated, err := check.populated()
		if err != nil {
			return fmt.Errorf("SSE-C can only be turned on for a bucket kip has checked for existing backups: %w", err)
		}
		if err := runSSEWriteTest(client, target); err != nil {
			return err
		}
		if populated {
			return runSSEKeyCheck(client, target, supplied)
		}
		return nil
	}()
	if checkErr == nil {
		return true, nil
	}
	if _, err := client.Run("kubectl -n velero delete secret " + sseKeySecret + " --ignore-not-found"); err != nil {
		return false, fmt.Errorf("%w. Removing the unused SSE-C key also failed: %w", checkErr, err)
	}
	return false, checkErr
}

// refusePlainOverSSECBackups checks external storage when the cluster has no
// live backup location. It rejects backups detected as requiring SSE-C and
// failed checks, so a new cluster cannot mix plain objects into those backups.
func refusePlainOverSSECBackups(client CommandRunner, cfg *BackupStorageConfig, target backupTarget, check *bucketCheck, live liveLocation) error {
	if cfg == nil || live.bucket != "" {
		return nil
	}
	populated, err := check.populated()
	if err != nil {
		return fmt.Errorf("kip has to check bucket %s for backups written with SSE-C before Velero starts: %w", target.bucket, err)
	}
	if !populated {
		return nil
	}
	logs, err := runCheckJob(client, target, sseCheckJob, "the SSE-C check", checkJobManifest(sseCheckJob, target.bucket, ssePlainProbeScript, ""))
	if err != nil {
		return err
	}
	switch {
	case strings.Contains(logs, sseEncryptedMarker):
		return fmt.Errorf("the backups in bucket %s were written with SSE-C. Pass --backup-storage-sse-c-key-file with the key saved by kip backup sse-key export", target.bucket)
	case strings.Contains(logs, ssePlainMarker), strings.Contains(logs, sseNoBackupsMarker):
		return nil
	}
	return fmt.Errorf("checking whether the backups in bucket %s use SSE-C failed: %s", target.bucket, strings.TrimSpace(logs))
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
var createSSEKeyCmd = "kubectl -n velero create secret generic " + sseKeySecret +
	" --from-file=" + sseKeyField + "=/dev/stdin --dry-run=client -o yaml" +
	" | kubectl label --local -f - " + excludeFromBackupLabel + " -o yaml | kubectl create -f -"

// generateSSEKeyCmd creates the key from 32 random bytes on the server, so it
// never crosses the network. The length check stops a failed read from
// producing a short key.
var generateSSEKeyCmd = `k=$(head -c 32 /dev/urandom | base64 | tr -d '\n') && [ ${#k} -eq 44 ] && printf '%s' "$k" | base64 -d | ` + createSSEKeyCmd

// requireSameSSECKey compares key with the live one in constant time. Errors
// are fixed strings, so no key material reaches a diagnostic.
func requireSameSSECKey(client CommandRunner, key []byte) error {
	out, err := client.Run("kubectl -n velero get secret " + sseKeySecret + " -o jsonpath='{.data." + sseKeyField + "}' 2>/dev/null")
	if err != nil {
		return errors.New("reading the backup SSE-C key failed")
	}
	live, err := base64.StdEncoding.DecodeString(strings.TrimSpace(out))
	if err != nil {
		return errors.New("the backup SSE-C key is not valid base64")
	}
	if subtle.ConstantTimeCompare(live, key) != 1 {
		return errors.New("--backup-storage-sse-c-key-file differs from this cluster's SSE-C key. " +
			"Changing it would make the existing backups unreadable, so kip keeps the current one")
	}
	return nil
}

// runSSEWriteTest writes and reads a test object with the key, then requires
// an HTTP 400 or 403 response to a HEAD request without it. This checks that
// the store enforces key access. Cleanup attempts to delete the test object.
func runSSEWriteTest(client CommandRunner, target backupTarget) error {
	if target.mcHost == "" {
		if target.noList != "" {
			return fmt.Errorf("kip has to test SSE-C on bucket %s first: %s", target.bucket, target.noList)
		}
		return fmt.Errorf("kip has to test SSE-C on bucket %s first and has no credentials for it. Re-run kip install with the storage credentials", target.bucket)
	}
	logs, err := runCheckJob(client, target, sseCheckJob, "the SSE-C write test", sseJobManifest(target.bucket, sseWriteTestScript))
	if err != nil {
		return err
	}
	if strings.Contains(logs, sseWriteOKMarker) {
		return nil
	}
	return fmt.Errorf("bucket %s did not pass the SSE-C test: a test object written with the key must read back with it and fail to read without it. "+
		"The store may not support SSE-C. Output: %s", target.bucket, strings.TrimSpace(logs))
}

// runSSEKeyCheck checks one backup metadata object with HEAD requests. It
// succeeds when the keyless request is refused and the keyed request succeeds,
// or when no metadata is found. The supplied flag names the key file
// in errors.
func runSSEKeyCheck(client CommandRunner, target backupTarget, supplied bool) error {
	logs, err := runCheckJob(client, target, sseCheckJob, "the SSE-C key check", sseJobManifest(target.bucket, sseKeyCheckScript))
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
		return fmt.Errorf("the SSE-C key this cluster holds does not open the backups in bucket %s, so kip removed it", target.bucket)
	case strings.Contains(logs, ssePlainMarker) && supplied:
		return fmt.Errorf("the backups in bucket %s were written without SSE-C, so they cannot be read with --backup-storage-sse-c-key-file. "+
			"Install without the SSE-C flags to recover from them", target.bucket)
	case strings.Contains(logs, ssePlainMarker):
		return fmt.Errorf("the backups in bucket %s were written without SSE-C, so SSE-C cannot be turned on for it. "+
			"kip removed the unused SSE-C key, and the next run keeps backups as they are", target.bucket)
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

var sseWriteTestScript = sseStat + `; ` + sseKeyShell + ` && O="kipper/$BUCKET/kipper-sse-check-$(date +%s)" && echo kipper > /tmp/o` +
	` && mc cp -q --enc-c "$E" /tmp/o "$O" >/dev/null` +
	` && R=$(mc cat --enc-c "$E" "$O") && C=$(stat "$O") && if [ "$R" = kipper ] && [ "$C" = refused ]; then echo ` + sseWriteOKMarker + `; else echo "$C"; fi;` +
	` mc rm -q "$O" >/dev/null 2>&1 || true`

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

func sseJobManifest(bucket, script string) string {
	return checkJobManifest(sseCheckJob, bucket, script, sseKeySecret)
}
