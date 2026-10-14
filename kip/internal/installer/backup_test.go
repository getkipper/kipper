package installer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestVeleroBSLBlock_InClusterMinIO(t *testing.T) {
	bsl := veleroBSLBlock(nil)
	assert.Contains(t, bsl, "bucket: velero")
	assert.Contains(t, bsl, "s3Url: http://minio.velero.svc:9000")
	assert.Contains(t, bsl, `s3ForcePathStyle: "true"`)
	assert.Contains(t, bsl, "region: minio")
	// The in-cluster MinIO default is not SSE-S3 (it has no KMS); SSE applies
	// to the external stores only.
	assert.NotContains(t, bsl, "serverSideEncryption")
}

func TestVeleroBSLBlock_ExternalAWSNative(t *testing.T) {
	bsl := veleroBSLBlock(&BackupStorageConfig{
		Bucket:          "acme-kipper-backups",
		Region:          "eu-west-1",
		Endpoint:        "", // native AWS S3 — no endpoint
		AccessKeyID:     "AKIAEXAMPLE",
		SecretAccessKey: "secretvalue",
	})
	assert.Contains(t, bsl, "bucket: acme-kipper-backups")
	assert.Contains(t, bsl, "region: eu-west-1")
	assert.NotContains(t, bsl, "s3Url", "native AWS S3 should not set s3Url")
	assert.NotContains(t, bsl, "s3ForcePathStyle", "native AWS S3 should not set s3ForcePathStyle")
	assert.Contains(t, bsl, "serverSideEncryption: AES256", "external backups must request SSE-S3 encryption")
}

func TestVeleroBSLBlock_ExternalS3Compatible(t *testing.T) {
	// Cloudflare R2 / MinIO / B2-S3 etc. all use the same shape:
	// bucket + region + endpoint + force-path-style.
	bsl := veleroBSLBlock(&BackupStorageConfig{
		Bucket:   "r2-backups",
		Region:   "auto",
		Endpoint: "https://abcd1234.r2.cloudflarestorage.com",
	})
	assert.Contains(t, bsl, "bucket: r2-backups")
	assert.Contains(t, bsl, "region: auto")
	assert.Contains(t, bsl, "s3Url: https://abcd1234.r2.cloudflarestorage.com")
	assert.Contains(t, bsl, `s3ForcePathStyle: "true"`)
	// A custom S3-compatible endpoint is not forced into SSE-S3 (would break a
	// KMS-less MinIO); its encryption is the operator's responsibility.
	assert.NotContains(t, bsl, "serverSideEncryption")
}

func TestVeleroBSLBlock_ExternalDoesNotLeakKipperPlaceholder(t *testing.T) {
	// Regression guard: the external branch should not include any of
	// the in-cluster MinIO placeholder strings (which would tell us the
	// branch fell through to the wrong path).
	bsl := veleroBSLBlock(&BackupStorageConfig{
		Bucket: "external-bucket",
		Region: "eu-west-1",
	})
	assert.NotContains(t, bsl, "minio.velero.svc")
	assert.NotContains(t, bsl, "bucket: velero")
}

func TestDefaultBackupExcludedNamespaces(t *testing.T) {
	got := DefaultBackupExcludedNamespaces()

	// velero is the load-bearing one: omitting it makes manual backups
	// recurse into the MinIO PVC that hosts the BSL bucket and hang
	// (storefront migration 2026-05-16).
	assert.Contains(t, got, "velero")

	// Mirror schedule template's exclusion list verbatim — they must
	// stay in sync or manual and scheduled backups capture different
	// content.
	assert.Equal(t, []string{
		"kube-system", "kube-public", "kube-node-lease",
		"monitoring", "keda", "longhorn-system", "traefik", "velero",
	}, got)

	// Independent calls return distinct slices so callers can mutate.
	a := DefaultBackupExcludedNamespaces()
	b := DefaultBackupExcludedNamespaces()
	a[0] = "mutated"
	assert.NotEqual(t, a[0], b[0])
}

func TestDefaultBackupExcludedResources(t *testing.T) {
	got := DefaultBackupExcludedResources()

	// These three transient cert-manager kinds are the load-bearing
	// exclusion: capturing them wedges certificate renewal after a
	// restore. The restored CertificateRequests collide with the names
	// cert-manager wants to reissue, so certificates silently fail to
	// renew and expire ~90 days later.
	assert.Equal(t, []string{
		"certificaterequests.cert-manager.io",
		"orders.acme.cert-manager.io",
		"challenges.acme.cert-manager.io",
	}, got)

	// The Certificate and its Secret must NOT be excluded — those are the
	// desired state a restore needs to bring back.
	assert.NotContains(t, got, "certificates.cert-manager.io")
	assert.NotContains(t, got, "secrets")

	// Independent calls return distinct slices so callers can mutate.
	a := DefaultBackupExcludedResources()
	b := DefaultBackupExcludedResources()
	a[0] = "mutated"
	assert.NotEqual(t, a[0], b[0])
}

func TestRenderExcludedNamespacesYAML(t *testing.T) {
	out := renderYAMLList([]string{"kube-system", "velero"}, "            ")
	assert.Equal(t, "            - kube-system\n            - velero", out)
}

func TestVeleroManifestSchedulesExcludeAllSystemNamespaces(t *testing.T) {
	// The chart's daily-apps schedule must exclude the same namespaces
	// the manual backup path excludes. The template is built from
	// DefaultBackupExcludedNamespaces() now, but a drift between
	// rendering and helper output (wrong indent, wrong newline, wrong
	// order) would silently break scheduled backups.
	excluded := DefaultBackupExcludedNamespaces()
	block := renderYAMLList(excluded, "            ")
	for _, ns := range excluded {
		assert.Contains(t, block, "- "+ns)
	}
	// First line carries the indent — proves we didn't accidentally
	// strip it during refactor.
	assert.True(t, strings.HasPrefix(block, "            -"), "indent must be preserved on first line")
}

func TestRenderExcludedNamespacesYAML_Empty(t *testing.T) {
	// Empty input should produce empty string, not a stray newline that
	// would break YAML indentation in the heredoc template.
	assert.Equal(t, "", renderYAMLList(nil, "    "))
	assert.Equal(t, "", renderYAMLList([]string{}, "    "))
}

func TestGenerateMinIOPassword(t *testing.T) {
	p1, err := generateMinIOPassword()
	assert.NoError(t, err)

	// MinIO requires a root password of at least 8 characters.
	assert.GreaterOrEqual(t, len(p1), 8)

	// base64.RawURLEncoding keeps the value to [A-Za-z0-9_-], so it drops
	// into the Secret stringData and the mc shell invocation without
	// quoting, escaping, or risk of terminating the apply heredoc.
	assert.Regexp(t, `^[A-Za-z0-9_-]+$`, p1)

	// Every install must get a distinct password — that is the whole point
	// of moving off the shared hardcoded value.
	p2, err := generateMinIOPassword()
	assert.NoError(t, err)
	assert.NotEqual(t, p1, p2)
}

func TestVeleroBSLBlock_CredentialsNotEmbedded(t *testing.T) {
	// Regression guard: ensure the BSL block does NOT include
	// any credential material. Credentials live in the Secret installed
	// by installVeleroCredentials; the BSL block is referenced by name
	// only.
	bsl := veleroBSLBlock(&BackupStorageConfig{
		Bucket:          "external-bucket",
		Region:          "eu-west-1",
		AccessKeyID:     "SHOULD-NOT-APPEAR",
		SecretAccessKey: "ALSO-SHOULD-NOT-APPEAR",
	})
	assert.NotContains(t, bsl, "SHOULD-NOT-APPEAR")
	assert.NotContains(t, bsl, "ALSO-SHOULD-NOT-APPEAR")
	assert.NotContains(t, bsl, "aws_access_key_id")
	assert.NotContains(t, bsl, "aws_secret_access_key")
}

func veleroValues(t *testing.T, selinux bool) map[string]any {
	t.Helper()
	var chart struct {
		Spec struct {
			ValuesContent string `yaml:"valuesContent"`
		} `yaml:"spec"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(veleroManifest(nil, selinux)), &chart))
	values := map[string]any{}
	require.NoError(t, yaml.Unmarshal([]byte(chart.Spec.ValuesContent), &values))
	return values
}

// Preserve the existing manifests for hosts where Kipper leaves k3s SELinux support disabled.
func TestVeleroManifestWithoutSELinuxMatchesTheGoldenFiles(t *testing.T) {
	cases := map[string]*BackupStorageConfig{
		"velero-in-cluster.golden": nil,
		"velero-external.golden":   {Bucket: "kipper-backups", Region: "auto", Endpoint: "https://s3.example.com", AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "example-secret"},
	}
	for name, cfg := range cases {
		want, err := os.ReadFile(filepath.Join("testdata", name))
		require.NoError(t, err)
		assert.Equal(t, string(want), veleroManifest(cfg, false), name)
	}
}

func TestVeleroManifestLeavesFsBackupUnprivilegedWithoutSELinux(t *testing.T) {
	values := veleroValues(t, false)

	assert.NotContains(t, values, "configMaps")
	assert.NotContains(t, values, "podAnnotations")
	assert.NotContains(t, values["nodeAgent"], "extraArgs")
}

func TestVeleroManifestRunsFsBackupPrivilegedOnSELinux(t *testing.T) {
	values := veleroValues(t, true)

	configMaps, ok := values["configMaps"].(map[string]any)
	require.True(t, ok)
	require.Len(t, configMaps, 1)
	nodeAgentConfig, ok := configMaps["node-agent-config"].(map[string]any)
	require.True(t, ok, "the chart names this ConfigMap velero-node-agent-config")
	data, ok := nodeAgentConfig["data"].(map[string]any)
	require.True(t, ok)
	require.Len(t, data, 1, "node-agent refuses a ConfigMap with more than one key")
	var raw string
	for _, v := range data {
		raw = v.(string)
	}
	var config map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &config))
	assert.Equal(t, map[string]any{"privilegedFsBackup": true}, config)

	nodeAgent := values["nodeAgent"].(map[string]any)
	assert.Equal(t, []any{"--node-agent-configmap=velero-node-agent-config"}, nodeAgent["extraArgs"])
	assert.Contains(t, nodeAgent, "resources")
	assert.NotContains(t, nodeAgent, "containerSecurityContext", "the node agent itself stays confined")
	assert.NotContains(t, nodeAgent, "podSecurityContext", "the node agent itself stays confined")

	annotations, ok := values["podAnnotations"].(map[string]any)
	require.True(t, ok, "node-agent reads its ConfigMap only at start, so a change must roll the pods")
	sum := sha256.Sum256([]byte(raw))
	assert.Equal(t, hex.EncodeToString(sum[:]), annotations["checksum/node-agent-config"])
}

type fakeVeleroHost struct {
	osRelease string
	cmds      []string
}

func (h *fakeVeleroHost) Run(cmd string) (string, error) {
	h.cmds = append(h.cmds, cmd)
	if cmd == "cat /etc/os-release" {
		return h.osRelease, nil
	}
	return "", nil
}

func (h *fakeVeleroHost) RunStdin(cmd string, _ io.Reader) (string, error) { return h.Run(cmd) }

func (h *fakeVeleroHost) Upload(io.Reader, string, os.FileMode) error { return nil }

func (h *fakeVeleroHost) applied(t *testing.T) string {
	t.Helper()
	for _, cmd := range h.cmds {
		if strings.Contains(cmd, "kind: HelmChart") {
			return cmd
		}
	}
	t.Fatal("no Velero HelmChart was applied")
	return ""
}

func TestInstallVeleroPrivilegesFsBackupOnlyWhereK3sRunsSELinux(t *testing.T) {
	rocky := &fakeVeleroHost{osRelease: "ID=rocky\nID_LIKE=\"rhel centos fedora\"\nVERSION_ID=\"10.2\"\n"}
	require.NoError(t, installVelero(rocky, nil, "pw"))
	assert.Contains(t, rocky.applied(t), "privilegedFsBackup")

	ubuntu := &fakeVeleroHost{osRelease: "ID=ubuntu\nID_LIKE=debian\nVERSION_ID=\"24.04\"\n"}
	require.NoError(t, installVelero(ubuntu, nil, "pw"))
	assert.NotContains(t, ubuntu.applied(t), "privilegedFsBackup")
}
