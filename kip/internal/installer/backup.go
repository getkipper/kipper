package installer

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/getkipper/kipper/kip/internal/ssh"
)

const veleroChartVersion = "12.0.0"

// In-cluster MinIO image pins. minio/minio is archived upstream as of
// 2026; these are the last standard (non-cpuv1) community RELEASE tags
// and are pinned so installs are reproducible instead of drifting with
// :latest.
const (
	minioServerImage = "minio/minio:RELEASE.2025-09-07T16-13-09Z"
	minioClientImage = "minio/mc:RELEASE.2025-08-13T08-35-41Z"
)

// MinIO credential Secret coordinates. The root password is generated
// per install (see generateMinIOPassword) and stored only in this
// Secret; the MinIO Deployment, the bucket-bootstrap Job, and `kip ai
// backup repair` all read it from here via secretKeyRef so nothing
// hardcodes the value or breaks when it changes.
const (
	minioRootUser           = "kipper"
	minioCredentialsSecret  = "minio-credentials" //nolint:gosec // G101: Kubernetes Secret object name, not a credential value
	minioUserSecretKey      = "root-user"
	minioPasswordSecretKey  = "root-password"
	minioBucketBootstrapJob = "minio-setup"
)

// generateMinIOPassword returns a random root password for the
// in-cluster MinIO. base64.RawURLEncoding keeps the value within the
// [A-Za-z0-9_-] set so it drops into YAML and shell without quoting or
// escaping surprises.
func generateMinIOPassword() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating minio password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// renderYAMLList renders a string slice as a block of YAML sequence
// items indented to fit inside a parent map. The indent argument carries
// the spaces before each `- item` line, so callers can drop the result
// into whichever level of the chart values block they need.
func renderYAMLList(items []string, indent string) string {
	if len(items) == 0 {
		return ""
	}
	lines := make([]string, len(items))
	for i, item := range items {
		lines[i] = indent + "- " + item
	}
	return strings.Join(lines, "\n")
}

// DefaultBackupExcludedNamespaces returns the namespaces every
// cluster-wide backup must skip. The list mirrors what the daily/weekly
// schedules already exclude: system namespaces with regenerable state
// (kube-*, traefik, longhorn-system, keda, monitoring) and Velero's own
// namespace — Velero must not back up the MinIO PVC that hosts its own
// BSL bucket, or the backup hangs in InProgress as it recurses into its
// own storage.
//
// `kip backup create` calls this so manual backups stay consistent with
// the schedules. Returns a fresh slice on every call so callers can
// mutate it safely.
func DefaultBackupExcludedNamespaces() []string {
	return []string{
		"kube-system",
		"kube-public",
		"kube-node-lease",
		"monitoring",
		"keda",
		"longhorn-system",
		"traefik",
		"velero",
	}
}

// DefaultBackupExcludedResources returns the cert-manager resource kinds
// every backup and restore must skip. These are the transient objects
// cert-manager creates while issuing a certificate (the CertificateRequest
// and the ACME Order/Challenge). They carry no desired state worth keeping
// and cert-manager recreates them on demand, so backing them up only
// causes harm.
//
// Capturing them wedges renewal after a restore: Velero recreates the old
// CertificateRequest objects while the Certificate's status.revision is
// lost (reset to none). cert-manager derives the next request name from
// the revision, collides with the restored name that already exists, and
// can never issue a fresh certificate. The certificate then silently
// expires ~90 days after the restore. Excluding these kinds lets the
// restored Certificate reconcile cleanly and renew on its own.
//
// The Certificate and its TLS Secret are deliberately kept — those are
// the desired state we want back. Returns a fresh slice on every call so
// callers can mutate it safely.
func DefaultBackupExcludedResources() []string {
	return []string{
		"certificaterequests.cert-manager.io",
		"orders.acme.cert-manager.io",
		"challenges.acme.cert-manager.io",
	}
}

// InstallBackup sets up Velero for cluster backups. When cfg is nil the
// installer deploys an in-cluster MinIO and points Velero at it (zero
// config out of the box). When cfg is non-nil the installer skips MinIO
// entirely and configures Velero to write to the user-provided
// S3-compatible bucket — backups then survive a cluster wipe.
func InstallBackup(client *ssh.Client, cfg *BackupStorageConfig) error {
	if cfg == nil {
		password, err := ensureMinIOPassword(client)
		if err != nil {
			return err
		}
		if err := installInClusterMinIO(client, password); err != nil {
			return err
		}
		return installVelero(client, cfg, password)
	}
	return installVelero(client, cfg, "")
}

// ensureMinIOPassword returns the in-cluster MinIO root password,
// reusing the value already stored in the minio-credentials Secret when
// it exists. Re-applies and upgrades (`kip upgrade` re-runs InstallBackup
// for in-cluster clusters) must not rotate the password: a Secret-only
// change does not restart the MinIO Deployment, so the running pod would
// keep the old value while the bucket Job and Velero credentials moved to
// the new one — wedging backups. A fresh password is generated only on
// first install, when the Secret is absent. This mirrors the external
// path, which likewise preserves the existing cloud-credentials Secret on
// upgrade.
func ensureMinIOPassword(client *ssh.Client) (string, error) {
	// Bracket notation because the key contains a hyphen, which kubectl's
	// jsonpath parser does not handle in dot notation.
	out, err := client.Run("kubectl -n velero get secret " + minioCredentialsSecret +
		" -o jsonpath=\"{.data['" + minioPasswordSecretKey + "']}\" --ignore-not-found")
	if err != nil {
		return "", fmt.Errorf("checking for existing minio credentials: %w", err)
	}
	if encoded := strings.TrimSpace(out); encoded != "" {
		decoded, derr := base64.StdEncoding.DecodeString(encoded)
		if derr != nil {
			return "", fmt.Errorf("decoding existing minio password: %w", derr)
		}
		return string(decoded), nil
	}
	return generateMinIOPassword()
}

// installInClusterMinIO deploys MinIO with a Longhorn-backed PVC and
// creates the velero bucket. Used as the default backup storage when
// the user has not configured external S3-compatible storage.
//
// Note: backups stored here die with the cluster (Longhorn data is on
// the cluster's own host). For durability across cluster wipes, use
// external storage via `kip install --backup-storage-*`.
func installInClusterMinIO(client *ssh.Client, password string) error {
	minioManifest := fmt.Sprintf(`apiVersion: v1
kind: Namespace
metadata:
  name: velero
---
apiVersion: v1
kind: Secret
metadata:
  name: %s
  namespace: velero
type: Opaque
stringData:
  %s: %s
  %s: %s
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: minio-storage
  namespace: velero
spec:
  storageClassName: longhorn-single
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 30Gi
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: minio
  namespace: velero
spec:
  selector:
    matchLabels:
      app: minio
  strategy:
    type: Recreate
  template:
    metadata:
      labels:
        app: minio
    spec:
      containers:
        - name: minio
          image: %s
          args: ["server", "/storage", "--console-address", ":9001"]
          env:
            - name: MINIO_ROOT_USER
              valueFrom:
                secretKeyRef:
                  name: %s
                  key: %s
            - name: MINIO_ROOT_PASSWORD
              valueFrom:
                secretKeyRef:
                  name: %s
                  key: %s
          ports:
            - containerPort: 9000
            - containerPort: 9001
          volumeMounts:
            - name: storage
              mountPath: /storage
          resources:
            requests:
              cpu: 25m
              memory: 64Mi
            limits:
              memory: 256Mi
      volumes:
        - name: storage
          persistentVolumeClaim:
            claimName: minio-storage
---
apiVersion: v1
kind: Service
metadata:
  name: minio
  namespace: velero
spec:
  selector:
    app: minio
  ports:
    - name: api
      port: 9000
      targetPort: 9000
    - name: console
      port: 9001
      targetPort: 9001
`,
		minioCredentialsSecret,
		minioUserSecretKey, minioRootUser,
		minioPasswordSecretKey, password,
		minioServerImage,
		minioCredentialsSecret, minioUserSecretKey,
		minioCredentialsSecret, minioPasswordSecretKey)

	applyCmd := fmt.Sprintf("cat <<'KIPEOF' | kubectl apply -f -\n%sKIPEOF", minioManifest)
	if _, err := client.Run(applyCmd); err != nil {
		return fmt.Errorf("applying MinIO manifest: %w", err)
	}

	waitCmd := "kubectl -n velero rollout status deployment/minio --timeout=120s"
	if _, err := client.Run(waitCmd); err != nil {
		return fmt.Errorf("waiting for MinIO: %w", err)
	}

	return createMinIOBucket(client)
}

// createMinIOBucket runs a one-shot Job that creates the velero bucket
// in the freshly-deployed MinIO. The Job reads the root credentials from
// the minio-credentials Secret via env, so the generated password never
// appears on a command line (where `ps` on the host could see it) or in
// the Job's pod spec as a literal.
func createMinIOBucket(client *ssh.Client) error {
	jobManifest := fmt.Sprintf(`apiVersion: batch/v1
kind: Job
metadata:
  name: %s
  namespace: velero
spec:
  backoffLimit: 3
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: mc
          image: %s
          command: ["sh", "-c", "mc alias set kipper http://minio.velero.svc:9000 \"$MINIO_ROOT_USER\" \"$MINIO_ROOT_PASSWORD\" && mc mb --ignore-existing kipper/velero"]
          env:
            - name: MINIO_ROOT_USER
              valueFrom:
                secretKeyRef:
                  name: %s
                  key: %s
            - name: MINIO_ROOT_PASSWORD
              valueFrom:
                secretKeyRef:
                  name: %s
                  key: %s
`,
		minioBucketBootstrapJob, minioClientImage,
		minioCredentialsSecret, minioUserSecretKey,
		minioCredentialsSecret, minioPasswordSecretKey)

	// Clear any Job left over from a previous failed bootstrap. A Job is
	// immutable and its name is fixed, so without this a prior failure
	// (e.g. MinIO not yet ready) would leave an exhausted Job that the
	// apply below can't replace, and the wait would block on a Job that
	// will never complete.
	_, _ = client.Run(fmt.Sprintf("kubectl -n velero delete job/%s --ignore-not-found", minioBucketBootstrapJob))

	applyCmd := fmt.Sprintf("cat <<'KIPEOF' | kubectl apply -f -\n%sKIPEOF", jobManifest)
	if _, err := client.Run(applyCmd); err != nil {
		return fmt.Errorf("applying MinIO bucket job: %w", err)
	}

	waitCmd := fmt.Sprintf("kubectl -n velero wait --for=condition=complete job/%s --timeout=120s", minioBucketBootstrapJob)
	if _, err := client.Run(waitCmd); err != nil {
		return fmt.Errorf("waiting for MinIO bucket job: %w", err)
	}

	// Best-effort cleanup; the bucket already exists at this point so a
	// leftover completed Job is harmless if the delete fails.
	_, _ = client.Run(fmt.Sprintf("kubectl -n velero delete job/%s --ignore-not-found", minioBucketBootstrapJob))
	return nil
}

// installVelero applies the Velero HelmChart, dispatching the BSL
// block depending on whether the operator has configured external
// object storage. The chart references a Secret/cloud-credentials in
// the velero namespace (created beforehand by installVeleroCredentials)
// rather than inlining the credentials in the chart values — embedding
// the access key in the HelmChart CR would expose it to anyone with
// read on kube-system HelmCharts, and it would leak into kubectl
// error output if apply ever fails.
func installVelero(client *ssh.Client, cfg *BackupStorageConfig, minioPassword string) error {
	if err := installVeleroCredentials(client, cfg, minioPassword); err != nil {
		return err
	}
	bslBlock := veleroBSLBlock(cfg)
	// 12-space indent puts each `- ns` two levels deeper than the
	// surrounding `excludedNamespaces:` key, matching the original
	// hand-written block so the chart YAML reads the same as before.
	excludedBlock := renderYAMLList(DefaultBackupExcludedNamespaces(), "            ")
	// cert-manager's transient issuance objects must be skipped in every
	// schedule so a later restore does not wedge certificate renewal (see
	// DefaultBackupExcludedResources).
	excludedResourcesBlock := renderYAMLList(DefaultBackupExcludedResources(), "            ")
	veleroManifest := fmt.Sprintf(`apiVersion: helm.cattle.io/v1
kind: HelmChart
metadata:
  name: velero
  namespace: kube-system
spec:
  repo: https://vmware-tanzu.github.io/helm-charts
  chart: velero
  version: %s
  targetNamespace: velero
  createNamespace: true
  valuesContent: |-
    initContainers:
      - name: velero-plugin-for-aws
        image: velero/velero-plugin-for-aws:v1.10.0
        volumeMounts:
          - mountPath: /target
            name: plugins
    configuration:
%s
      volumeSnapshotLocation: []
      uploaderType: kopia
      defaultVolumesToFsBackup: true
    deployNodeAgent: true
    credentials:
      useSecret: true
      existingSecret: cloud-credentials
    resources:
      requests:
        cpu: 100m
        memory: 256Mi
      limits:
        memory: 512Mi
    nodeAgent:
      resources:
        requests:
          cpu: 50m
          memory: 64Mi
        limits:
          memory: 512Mi
    schedules:
      daily-apps:
        disabled: false
        schedule: "0 3 * * *"
        useOwnerReferencesInBackup: false
        template:
          ttl: 168h0m0s
          storageLocation: default
          excludedNamespaces:
%s
          excludedResources:
%s
      weekly-system:
        disabled: false
        schedule: "0 4 * * 0"
        useOwnerReferencesInBackup: false
        template:
          ttl: 720h0m0s
          storageLocation: default
          includedNamespaces:
            - dex
            - cert-manager
            - kipper-system
          excludedResources:
%s
`, veleroChartVersion, bslBlock, excludedBlock, excludedResourcesBlock, excludedResourcesBlock)

	veleroCmd := fmt.Sprintf("cat <<'KIPEOF' | kubectl apply -f -\n%sKIPEOF", veleroManifest)
	if _, err := client.Run(veleroCmd); err != nil {
		return fmt.Errorf("applying Velero HelmChart: %w", err)
	}
	return nil
}

// installVeleroCredentials creates the velero namespace and the
// `cloud-credentials` Secret that the Velero HelmChart references via
// `existingSecret`. Done out-of-band so credentials never appear in
// HelmChart values or kubectl-apply error output. Re-runs replace the
// Secret in place (apply-style upsert), so re-installing with a fresh
// credentials file rotates the keys without leaving stale secrets
// behind.
//
// Upgrade path: when called with an external BackupStorageConfig that
// has no credentials (because kip config does not persist them), the
// function preserves the existing Secret instead of overwriting it.
// The upgrade can then re-apply the Velero chart without disturbing
// credentials the operator supplied at install time.
func installVeleroCredentials(client *ssh.Client, cfg *BackupStorageConfig, minioPassword string) error {
	// Ensure the namespace exists before we drop a Secret into it. The
	// Velero HelmChart will create it too, but that race is fine — apply
	// is idempotent and the namespace is harmless if it already exists.
	if _, err := client.Run("kubectl create namespace velero --dry-run=client -o yaml | kubectl apply -f -"); err != nil {
		return fmt.Errorf("creating velero namespace: %w", err)
	}

	// External mode without inline credentials means "preserve existing
	// Secret" (upgrade path). The chart references cloud-credentials by
	// name; as long as the Secret exists, the chart is happy.
	if cfg != nil && cfg.AccessKeyID == "" && cfg.SecretAccessKey == "" {
		out, err := client.Run("kubectl -n velero get secret cloud-credentials --ignore-not-found -o name")
		if err != nil {
			return fmt.Errorf("checking for existing velero credentials: %w", err)
		}
		if strings.TrimSpace(out) == "" {
			return fmt.Errorf("external backup storage configured but cloud-credentials Secret is missing from the velero namespace. Reinstall with --backup-storage-credentials to bootstrap")
		}
		return nil
	}

	var credsBlock string
	if cfg == nil {
		// In-cluster MinIO. The access key id is the fixed root user; the
		// secret key is the per-install password generated by InstallBackup
		// and also stored in the minio-credentials Secret the MinIO
		// Deployment reads. Both sides must agree, so the same value flows
		// into here and into that Secret.
		credsBlock = fmt.Sprintf("[default]\naws_access_key_id = %s\naws_secret_access_key = %s\n", minioRootUser, minioPassword)
	} else {
		credsBlock = (&AWSCredentials{
			AccessKeyID:     cfg.AccessKeyID,
			SecretAccessKey: cfg.SecretAccessKey,
		}).VeleroCredentialsBlock()
	}

	// Upload the credentials over the SSH stream (SFTP), then kubectl create
	// the Secret from the file. Upload pipes the bytes rather than embedding
	// them in a command, so the secret never lands in the remote shell's argv
	// (a here-doc through Run would put the whole body there, visible in `ps`).
	if err := client.Upload(strings.NewReader(credsBlock), "/tmp/velero-cloud-credentials", 0o600); err != nil {
		return fmt.Errorf("writing velero credentials to host: %w", err)
	}
	defer func() {
		_, _ = client.Run("shred -u /tmp/velero-cloud-credentials 2>/dev/null || rm -f /tmp/velero-cloud-credentials")
	}()

	applyCmd := "kubectl -n velero create secret generic cloud-credentials " +
		"--from-file=cloud=/tmp/velero-cloud-credentials " +
		"--dry-run=client -o yaml | kubectl apply -f -"
	if _, err := client.Run(applyCmd); err != nil {
		return fmt.Errorf("applying velero cloud-credentials secret: %w", err)
	}
	return nil
}

// veleroBSLBlock renders the BSL block of the Velero values, branching
// on whether external storage is configured. Returned string is pre-
// indented to slot into the HelmChart valuesContent at the right depth.
// Credentials are NOT rendered here — they live in the cloud-credentials
// Secret installed by installVeleroCredentials.
func veleroBSLBlock(cfg *BackupStorageConfig) string {
	if cfg == nil {
		return `      backupStorageLocation:
        - name: default
          provider: aws
          bucket: velero
          config:
            region: minio
            s3ForcePathStyle: "true"
            s3Url: http://minio.velero.svc:9000`
	}

	// External BSL. Endpoint is empty for native AWS S3 (Velero derives it from
	// the region); otherwise we set s3Url + force path style.
	configLines := []string{fmt.Sprintf("            region: %s", cfg.Region)}
	if cfg.Endpoint == "" {
		// Native AWS S3 always accepts SSE-S3, so request it: backups are then
		// encrypted at rest server-side. A custom S3-compatible endpoint is
		// left alone — forcing SSE-S3 breaks a MinIO store that has no KMS, and
		// the operator owns that store's encryption policy.
		configLines = append(configLines, `            serverSideEncryption: AES256`)
	} else {
		configLines = append(configLines,
			fmt.Sprintf(`            s3Url: %s`, cfg.Endpoint),
			`            s3ForcePathStyle: "true"`,
		)
	}
	return fmt.Sprintf(`      backupStorageLocation:
        - name: default
          provider: aws
          bucket: %s
          config:
%s`, cfg.Bucket, strings.Join(configLines, "\n"))
}
