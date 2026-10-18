package installer

import (
	"bytes"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// repositoryKeySecret is the Secret Velero reads the password of its Kopia
	// repositories from. Velero creates it with a published default password
	// when it is missing at startup, so kip creates it first.
	repositoryKeySecret = "velero-repo-credentials" //nolint:gosec // G101: Kubernetes Secret object name, not a credential value
	repositoryKeyField  = "repository-password"

	excludeFromBackupLabel = "velero.io/exclude-from-backup=true"

	bucketCheckJob    = "kipper-bucket-check"
	bucketCheckSecret = "kipper-bucket-check" //nolint:gosec // G101: Kubernetes Secret object name, not a credential value

	bucketPopulatedMarker = "KIPPER_BUCKET_POPULATED"
	bucketEmptyMarker     = "KIPPER_BUCKET_EMPTY"

	maxRepositoryKeyBytes = 1024
	bucketCheckTimeout    = 5 * time.Minute
)

var (
	bucketNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{1,61}[A-Za-z0-9]$`)
	regionPattern     = regexp.MustCompile(`^[a-z0-9-]+$`)
)

// ReadRepositoryKeyFile reads a regular file of at most 1 KiB and validates its
// contents with CheckRepositoryKey. It removes one trailing LF, CRLF or CR.
// On systems other than Windows, it rejects any group or other permissions,
// as SSH does for private keys.
func ReadRepositoryKeyFile(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("--backup-repository-key-file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("--backup-repository-key-file %s is not a regular file", path)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("--backup-repository-key-file %s grants permissions to other users. Run chmod 600 %s", path, path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("--backup-repository-key-file: %w", err)
	}
	defer func() { _ = f.Close() }()
	key, err := io.ReadAll(io.LimitReader(f, maxRepositoryKeyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("--backup-repository-key-file: %w", err)
	}
	if len(key) > maxRepositoryKeyBytes {
		return nil, fmt.Errorf("--backup-repository-key-file %s is larger than %d bytes", path, maxRepositoryKeyBytes)
	}
	key = []byte(strings.TrimSuffix(strings.TrimSuffix(string(key), "\n"), "\r"))
	if err := CheckRepositoryKey(key); err != nil {
		return nil, fmt.Errorf("--backup-repository-key-file %s: %w", path, err)
	}
	return key, nil
}

// CheckRepositoryKey validates that key can pass unchanged through a key file
// and a JSON install request. It returns an error for an empty key, more than
// 1 KiB, invalid UTF-8, ASCII control characters (including line breaks), or
// leading or trailing whitespace.
func CheckRepositoryKey(key []byte) error {
	if len(key) == 0 {
		return errors.New("the key is empty")
	}
	if len(key) > maxRepositoryKeyBytes {
		return fmt.Errorf("the key is longer than %d bytes", maxRepositoryKeyBytes)
	}
	for _, b := range key {
		if b < 0x20 || b == 0x7f {
			return errors.New("the key contains a line break or another control character")
		}
	}
	if !utf8.Valid(key) {
		return errors.New("the key is not valid UTF-8")
	}
	if !bytes.Equal(bytes.TrimSpace(key), key) {
		return errors.New("the key starts or ends with whitespace, which Velero would drop")
	}
	return nil
}

// WriteRepositoryKeyFile writes key unchanged to a new file at path with mode
// 0600. On Windows, the file inherits its folder's permissions. An existing
// file or symlink causes an error. A failed write attempts to remove the partial file.
// Call CheckRepositoryKey first when the key needs validation.
func WriteRepositoryKeyFile(path string, key []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("--to: %w", err)
	}
	if _, err := f.Write(key); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return fmt.Errorf("--to: %w", err)
	}
	return f.Close()
}

// IsDefaultRepositoryKey reports whether key is the password Velero publishes
// and uses when it finds no key, ignoring surrounding whitespace.
func IsDefaultRepositoryKey(key []byte) bool {
	return strings.TrimSpace(string(key)) == "static-passw0rd"
}

// backupTarget identifies Velero's backup storage location and the MC_HOST
// value used to list it. An empty mcHost means listing is unavailable, either
// because credentials are absent (as on external-storage upgrades) or because
// mc cannot use the endpoint or credentials. noList explains those restrictions.
type backupTarget struct {
	bucket string
	s3URL  string
	mcHost string
	noList string
}

// normalizeEndpoint drops a trailing slash, so https://s3.example.com/ and
// https://s3.example.com name the same location.
func normalizeEndpoint(s3URL string) string { return strings.TrimRight(s3URL, "/") }

func describeEndpoint(s3URL string) string {
	if s3URL == "" {
		return "AWS S3"
	}
	return normalizeEndpoint(s3URL)
}

// newBackupTarget describes the location cfg configures, or in-cluster MinIO
// when cfg is nil. mc splits MC_HOST with a regular expression and does not
// URL-decode it, so credentials containing ":" or "@" prevent listing.
// Endpoint and credential restrictions are deferred until a listing is needed.
func newBackupTarget(cfg *BackupStorageConfig, minioPassword string) (backupTarget, error) {
	if cfg == nil {
		t := backupTarget{bucket: "velero", s3URL: inClusterMinIOURL}
		if minioPassword != "" && !strings.ContainsAny(minioPassword, ":@") {
			t.mcHost = "http://" + minioRootUser + ":" + minioPassword + "@" + strings.TrimPrefix(inClusterMinIOURL, "http://")
		}
		return t, nil
	}
	if !bucketNamePattern.MatchString(cfg.Bucket) {
		return backupTarget{}, fmt.Errorf("backup storage bucket %q is not a valid bucket name", cfg.Bucket)
	}
	t := backupTarget{bucket: cfg.Bucket, s3URL: cfg.Endpoint}
	scheme, host := "https", ""
	if cfg.Endpoint == "" {
		if !regionPattern.MatchString(cfg.Region) {
			return backupTarget{}, fmt.Errorf("backup storage region %q is not a valid AWS region", cfg.Region)
		}
		host = "s3." + cfg.Region + ".amazonaws.com"
	} else {
		u, ok := plainEndpoint(cfg.Endpoint)
		if !ok {
			// Defer listing restrictions so upgrades can reuse an endpoint
			// that Velero already accepts. A move or a missing key may need a listing.
			t.noList = fmt.Sprintf("listing bucket contents requires an HTTP or HTTPS endpoint with no credentials, path, query or fragment. Got %q", cfg.Endpoint)
			return t, nil
		}
		scheme, host = u.Scheme, u.Host
	}
	if cfg.AccessKeyID == "" && cfg.SecretAccessKey == "" {
		return t, nil
	}
	if strings.ContainsAny(cfg.AccessKeyID+cfg.SecretAccessKey, ":@") {
		t.noList = "kip cannot list a bucket with storage credentials containing ':' or '@'"
		return t, nil
	}
	t.mcHost = scheme + "://" + cfg.AccessKeyID + ":" + cfg.SecretAccessKey + "@" + host
	return t, nil
}

// plainEndpoint accepts an HTTP or HTTPS endpoint with an optional root slash
// and no credentials, query or fragment. It returns false on failure.
func plainEndpoint(endpoint string) (*url.URL, bool) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, false
	}
	ok := (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" &&
		(u.Path == "" || u.Path == "/") && u.User == nil && u.RawQuery == "" && u.Fragment == ""
	return u, ok
}

// prepareBackupRepository runs before Velero starts. It refuses to point an
// existing installation at a bucket that already holds backups, keeps an
// existing repository key unchanged, and otherwise creates the key: from key
// when given, or freshly generated on the server for a bucket with no
// backups. A populated bucket without a key is refused, because only the key
// its backups were written with can read them.
func prepareBackupRepository(client CommandRunner, target backupTarget, check *bucketCheck, key []byte) error {
	if err := refuseMoveToPopulatedBucket(client, check); err != nil {
		return err
	}

	out, err := client.Run("kubectl -n velero get secret " + repositoryKeySecret + " --ignore-not-found -o name 2>/dev/null")
	if err != nil {
		return fmt.Errorf("checking for the backup repository key: %w", err)
	}
	if strings.TrimSpace(out) == "secret/"+repositoryKeySecret {
		if key != nil {
			if err := requireSameRepositoryKey(client, key); err != nil {
				return err
			}
		}
		if _, err := client.Run("kubectl -n velero label secret " + repositoryKeySecret + " " + excludeFromBackupLabel + " --overwrite"); err != nil {
			return fmt.Errorf("labeling the backup repository key: %w", err)
		}
		return nil
	}

	if key != nil {
		if _, err := client.RunStdin(createRepositoryKeyCmd, strings.NewReader(string(key))); err != nil {
			return fmt.Errorf("creating the backup repository key: %w", err)
		}
		return nil
	}
	populated, err := check.populated()
	if err != nil {
		return fmt.Errorf("the %s Secret is missing, so kip has to check bucket %s for existing backups before it creates a key: %w. "+
			"Re-run kip install with the storage credentials, or pass --backup-repository-key-file with the key the existing backups use",
			repositoryKeySecret, target.bucket, err)
	}
	if populated {
		return fmt.Errorf("bucket %s already holds backups. Pass --backup-repository-key-file with the key used to encrypt them. "+
			"kip creates a new key only when the bucket has no existing backups", target.bucket)
	}
	if _, err := client.Run(generateRepositoryKeyCmd); err != nil {
		return fmt.Errorf("creating the backup repository key: %w", err)
	}
	return nil
}

// createRepositoryKeyCmd reads the key from stdin, so it never appears in a
// command line. kubectl create, unlike apply, fails if the Secret appeared in
// the meantime instead of replacing it.
var createRepositoryKeyCmd = "kubectl -n velero create secret generic " + repositoryKeySecret +
	" --from-file=" + repositoryKeyField + "=/dev/stdin --dry-run=client -o yaml" +
	" | kubectl label --local -f - " + excludeFromBackupLabel + " -o yaml | kubectl create -f -"

// generateRepositoryKeyCmd creates a key from 32 random bytes on the server,
// so the key never crosses the network. The length check stops a failed read
// from producing an empty key.
var generateRepositoryKeyCmd = `k=$(head -c 32 /dev/urandom | base64 | tr -d '\n') && [ ${#k} -eq 44 ] && printf '%s' "$k" | ` + createRepositoryKeyCmd

// requireSameRepositoryKey compares key with the live one after trimming both
// as Velero does. The live key travels base64-encoded over SSH and is compared
// in constant time. Neither key is printed.
func requireSameRepositoryKey(client CommandRunner, key []byte) error {
	// Use fixed errors to keep captured key material out of diagnostics.
	out, err := client.Run("kubectl -n velero get secret " + repositoryKeySecret +
		" -o jsonpath='{.data." + repositoryKeyField + "}' 2>/dev/null")
	if err != nil {
		return errors.New("reading the backup repository key failed")
	}
	live, err := base64.StdEncoding.DecodeString(strings.TrimSpace(out))
	if err != nil {
		return errors.New("the backup repository key is not valid base64")
	}
	if subtle.ConstantTimeCompare(bytes.TrimSpace(live), bytes.TrimSpace(key)) != 1 {
		return errors.New("--backup-repository-key-file differs from this cluster's backup repository key. " +
			"Changing the key of existing repositories would make their backups unreadable, so kip keeps the current one")
	}
	return nil
}

// refuseMoveToPopulatedBucket stops a re-run of install from pointing a
// cluster's backups at a bucket that already holds backups, such as another
// cluster's: Velero would adopt those backups and later expire them.
func refuseMoveToPopulatedBucket(client CommandRunner, check *bucketCheck) error {
	live, err := liveBackupLocation(client)
	if err != nil {
		return err
	}
	liveBucket, liveURL := live.bucket, live.s3URL
	if liveBucket == "" || (liveBucket == check.target.bucket && normalizeEndpoint(liveURL) == normalizeEndpoint(check.target.s3URL)) {
		return nil
	}
	populated, err := check.populated()
	if err != nil {
		return fmt.Errorf("backups move from bucket %s at %s to bucket %s at %s, which kip must check for existing backups first: %w. Re-run kip install with the storage credentials",
			liveBucket, describeEndpoint(liveURL), check.target.bucket, describeEndpoint(check.target.s3URL), err)
	}
	if populated {
		return fmt.Errorf("backups move from bucket %s at %s to bucket %s at %s, which already holds backups, possibly another cluster's. Point this cluster at an empty bucket",
			liveBucket, describeEndpoint(liveURL), check.target.bucket, describeEndpoint(check.target.s3URL))
	}
	return nil
}

// liveLocation is Velero's default backup location as the cluster runs it.
// An empty bucket means Velero or the location is absent.
type liveLocation struct {
	bucket string
	s3URL  string
	sseKey string
}

func liveBackupLocation(client CommandRunner) (liveLocation, error) {
	crd, err := client.Run("kubectl get crd backupstoragelocations.velero.io --ignore-not-found -o name 2>/dev/null")
	if err != nil {
		return liveLocation{}, fmt.Errorf("checking for Velero: %w", err)
	}
	if strings.TrimSpace(crd) == "" {
		return liveLocation{}, nil
	}
	out, err := client.Run(`kubectl -n velero get backupstoragelocation default --ignore-not-found -o jsonpath='{.spec.objectStorage.bucket}|{.spec.config.s3Url}|{.spec.config.customerKeyEncryptionSecret}' 2>/dev/null`)
	if err != nil {
		return liveLocation{}, fmt.Errorf("reading the current backup location: %w", err)
	}
	fields := strings.SplitN(strings.TrimSpace(out), "|", 3)
	for len(fields) < 3 {
		fields = append(fields, "")
	}
	return liveLocation{bucket: fields[0], s3URL: fields[1], sseKey: fields[2]}, nil
}

// changesFrom reports whether target differs from the live location,
// counting a missing location as a change.
func (l liveLocation) changesFrom(target backupTarget) bool {
	return l.bucket != target.bucket || normalizeEndpoint(l.s3URL) != normalizeEndpoint(target.s3URL)
}

// bucketCheck caches a successful listing result from a Job inside the
// cluster. Credentials reach the Job through a temporary Secret over stdin.
// Failed checks can be retried.
type bucketCheck struct {
	client CommandRunner
	target backupTarget
	done   bool
	result bool
}

func (c *bucketCheck) populated() (bool, error) {
	if c.done {
		return c.result, nil
	}
	if c.target.noList != "" {
		return false, errors.New(c.target.noList)
	}
	if c.target.mcHost == "" {
		return false, errors.New("kip has no credentials for this bucket")
	}
	result, err := runBucketCheck(c.client, c.target)
	if err != nil {
		return false, err
	}
	c.done, c.result = true, result
	return result, nil
}

func runBucketCheck(client CommandRunner, target backupTarget) (bool, error) {
	logs, err := runCheckJob(client, target, bucketCheckJob, "the bucket check", bucketCheckManifest(target.bucket))
	if err != nil {
		return false, err
	}
	switch {
	case strings.Contains(logs, bucketPopulatedMarker):
		return true, nil
	case strings.Contains(logs, bucketEmptyMarker):
		return false, nil
	}
	return false, fmt.Errorf("listing bucket %s failed: %s", target.bucket, strings.TrimSpace(logs))
}

// runCheckJob applies manifest for the named Job and returns its last 20 log
// lines after success or failure. It waits up to bucketCheckTimeout. Credentials
// travel over stdin into a temporary Secret. Cleanup attempts to delete both
// the Job and Secret. The what argument identifies the check in errors.
func runCheckJob(client CommandRunner, target backupTarget, job, what, manifest string) (string, error) {
	cleanup := fmt.Sprintf("kubectl -n velero delete job/%s secret/%s --ignore-not-found", job, bucketCheckSecret)
	_, _ = client.Run(cleanup)
	defer func() { _, _ = client.Run(cleanup) }()

	secretCmd := "kubectl -n velero create secret generic " + bucketCheckSecret +
		" --from-file=host=/dev/stdin --dry-run=client -o yaml" +
		" | kubectl label --local -f - " + excludeFromBackupLabel + " -o yaml | kubectl apply -f -"
	if _, err := client.RunStdin(secretCmd, strings.NewReader(target.mcHost)); err != nil {
		return "", fmt.Errorf("storing the credentials for %s: %w", what, err)
	}
	if _, err := client.Run(fmt.Sprintf("cat <<'KIPEOF' | kubectl apply -f -\n%sKIPEOF", manifest)); err != nil {
		return "", fmt.Errorf("starting %s: %w", what, err)
	}

	deadline := time.Now().Add(bucketCheckTimeout)
	for {
		status, err := client.Run(fmt.Sprintf("kubectl -n velero get job %s -o jsonpath='{.status.succeeded}/{.status.failed}' 2>/dev/null", job))
		if err != nil {
			return "", fmt.Errorf("waiting for %s: %w", what, err)
		}
		if succeeded, failed, _ := strings.Cut(strings.TrimSpace(status), "/"); succeeded == "1" || failed == "1" {
			break
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("%s did not finish within %s", what, bucketCheckTimeout)
		}
		time.Sleep(2 * time.Second)
	}

	logs, err := client.Run(fmt.Sprintf("kubectl -n velero logs job/%s --tail=20", job))
	if err != nil {
		return "", fmt.Errorf("reading the result of %s: %w", what, err)
	}
	return logs, nil
}

// bucketCheckManifest builds a Job that checks for top-level backups, kopia
// or restic directories.
func bucketCheckManifest(bucket string) string {
	return checkJobManifest(bucketCheckJob, bucket,
		fmt.Sprintf(`mc ls "kipper/$BUCKET/" > /tmp/ls && if grep -qE ' (backups|kopia|restic)/$' /tmp/ls; then echo %s; else echo %s; fi`, bucketPopulatedMarker, bucketEmptyMarker),
		"")
}

// checkJobManifest builds a named Job running script with mc. The kipper alias
// points to the storage endpoint. The bucket name travels in an environment
// variable to keep it separate from shell syntax. A non-empty keySecret is
// mounted read-only at /sse.
func checkJobManifest(job, bucket, script, keySecret string) string {
	mount, volume := "", ""
	if keySecret != "" {
		mount = `
            - name: sse
              mountPath: /sse
              readOnly: true`
		volume = fmt.Sprintf(`
        - name: sse
          secret:
            secretName: %s`, keySecret)
	}
	return fmt.Sprintf(`apiVersion: batch/v1
kind: Job
metadata:
  name: %s
  namespace: velero
spec:
  backoffLimit: 0
  activeDeadlineSeconds: 300
  ttlSecondsAfterFinished: 600
  template:
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
            - name: HOME
              value: /tmp
            - name: MC_HOST_kipper
              valueFrom:
                secretKeyRef:
                  name: %s
                  key: host
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop: [ALL]
          volumeMounts:
            - name: tmp
              mountPath: /tmp%s
      volumes:
        - name: tmp
          emptyDir: {}%s
`, job, minioClientImage, yamlQuote(script), bucket, bucketCheckSecret, mount, volume)
}

// yamlQuote renders s as a single-quoted YAML scalar.
func yamlQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
