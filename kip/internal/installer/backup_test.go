package installer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/getkipper/kipper/controller/pkg/platform"
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

// TestVeleroAWSPluginMatchesTheChartsVelero guards compatibility because each
// Velero minor release requires a matching AWS plugin series.
func TestVeleroAWSPluginMatchesTheChartsVelero(t *testing.T) {
	pluginSeries := map[string]string{
		"12.0.0": "velero/velero-plugin-for-aws:v1.14.", // This chart installs Velero 1.18.
	}
	series, ok := pluginSeries[veleroChartVersion]
	require.True(t, ok, "add the AWS plugin series for chart %s", veleroChartVersion)

	values := veleroValues(t, false)
	initContainers, _ := values["initContainers"].([]any)
	require.Len(t, initContainers, 1)
	image, _ := initContainers[0].(map[string]any)["image"].(string)
	assert.True(t, strings.HasPrefix(image, series), "plugin %s is incompatible with chart %s", image, veleroChartVersion)
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
	osRelease   string
	osImages    string
	chartValues string
	patches     []string
	cmds        []string
}

func (h *fakeVeleroHost) Run(cmd string) (string, error) {
	h.cmds = append(h.cmds, cmd)
	if cmd == "cat /etc/os-release" {
		return h.osRelease, nil
	}
	if strings.Contains(cmd, ".status.nodeInfo.osImage") {
		return h.osImages, nil
	}
	if strings.Contains(cmd, "get helmchart velero") {
		return h.chartValues, nil
	}
	if strings.Contains(cmd, "checksum/node-agent-config}") {
		return "abc", nil
	}
	return "", nil
}

func (h *fakeVeleroHost) RunStdin(cmd string, in io.Reader) (string, error) {
	if strings.Contains(cmd, "patch helmchart velero") {
		b, _ := io.ReadAll(in)
		h.patches = append(h.patches, string(b))
	}
	return h.Run(cmd)
}

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

func TestInstallVeleroPrivilegesFsBackupWhenAnyNodeIsRHEL(t *testing.T) {
	ubuntu := &fakeVeleroHost{
		osRelease: "ID=ubuntu\nID_LIKE=debian\nVERSION_ID=\"26.04\"\n",
		osImages:  "Ubuntu 26.04 LTS\nRocky Linux 10.2 (Red Quartz)\n",
	}
	require.NoError(t, installVelero(ubuntu, nil, "pw"))
	assert.Contains(t, ubuntu.applied(t), "privilegedFsBackup",
		"a Rocky worker needs privileged backups even with an Ubuntu server")

	debianOnly := &fakeVeleroHost{
		osRelease: "ID=ubuntu\nID_LIKE=debian\nVERSION_ID=\"26.04\"\n",
		osImages:  "Ubuntu 26.04 LTS\nDebian GNU/Linux 13 (trixie)\n",
	}
	require.NoError(t, installVelero(debianOnly, nil, "pw"))
	assert.NotContains(t, debianOnly.applied(t), "privilegedFsBackup")
}

func chartValuesContent(t *testing.T, manifest string) string {
	t.Helper()
	var chart struct {
		Spec struct {
			ValuesContent string `yaml:"valuesContent"`
		} `yaml:"spec"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(manifest), &chart))
	return chart.Spec.ValuesContent
}

func parsedValues(t *testing.T, values string) map[string]any {
	t.Helper()
	doc := map[string]any{}
	require.NoError(t, yaml.Unmarshal([]byte(values), &doc))
	return doc
}

var externalStorage = &BackupStorageConfig{Bucket: "kipper-backups", Region: "auto", Endpoint: "https://s3.example.com"}

func TestEnsureVeleroCoversNodesKeepsTheBackupDestinationWhenTheFirstRHELWorkerJoins(t *testing.T) {
	h := &fakeVeleroHost{
		osImages:    "Ubuntu 26.04 LTS\nRocky Linux 10.2 (Red Quartz)\n",
		chartValues: chartValuesContent(t, veleroManifest(externalStorage, false)),
	}

	changed, err := EnsureVeleroCoversNodes(h)

	require.NoError(t, err)
	assert.True(t, changed)
	require.Len(t, h.patches, 1)
	var patch struct {
		Spec struct {
			ValuesContent string `json:"valuesContent"`
		} `json:"spec"`
	}
	require.NoError(t, json.Unmarshal([]byte(h.patches[0]), &patch))
	assert.Equal(t, parsedValues(t, chartValuesContent(t, veleroManifest(externalStorage, true))), parsedValues(t, patch.Spec.ValuesContent),
		"only the node agent settings change; the external bucket stays")
	joined := strings.Join(h.cmds, "\n")
	assert.NotContains(t, joined, "cloud-credentials", "the backup credentials are left alone")
	assert.Contains(t, joined, "rollout status ds/node-agent", "backups are covered only once the agents restart")
}

func TestEnsureVeleroCoversNodesStillWaitsForTheAgentsWhenTheChartIsAlreadyChanged(t *testing.T) {
	h := &fakeVeleroHost{
		osImages:    "Ubuntu 26.04 LTS\nRocky Linux 10.2 (Red Quartz)\n",
		chartValues: chartValuesContent(t, veleroManifest(nil, true)),
	}

	changed, err := EnsureVeleroCoversNodes(h)

	require.NoError(t, err)
	assert.False(t, changed)
	assert.Empty(t, h.patches)
	assert.Contains(t, strings.Join(h.cmds, "\n"), "rollout status ds/node-agent", "a retry after a failed rollout checks the agents again")
}

func TestEnsureVeleroCoversNodesLeavesANonRHELClusterAndOneWithoutVeleroAlone(t *testing.T) {
	debian := &fakeVeleroHost{
		osImages:    "Ubuntu 26.04 LTS\nDebian GNU/Linux 13 (trixie)\n",
		chartValues: chartValuesContent(t, veleroManifest(nil, false)),
	}
	changed, err := EnsureVeleroCoversNodes(debian)
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Empty(t, debian.patches)

	noVelero := &fakeVeleroHost{osImages: "Rocky Linux 10.2 (Red Quartz)\n"}
	changed, err = EnsureVeleroCoversNodes(noVelero)
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Empty(t, noVelero.patches)
}

// Preserve existing volume sizes because Kubernetes rejects shrinking storage.
func TestMinIOStorageRequest(t *testing.T) {
	tests := []struct {
		name    string
		out     string
		want    string
		wantErr bool
	}{
		{name: "fresh install", out: "", want: "10Gi"},
		{name: "claim from an install before the default shrank", out: "minio-size[30Gi|30Gi]", want: "30Gi"},
		{name: "claim at the default", out: "minio-size[10Gi|10Gi]", want: "10Gi"},
		{name: "capacity grown past the request", out: "minio-size[10Gi|25Gi]", want: "25Gi"},
		{name: "smaller legacy claim is raised to the default", out: "minio-size[5Gi|5Gi]", want: "10Gi"},
		{name: "unparseable values", out: "minio-size[lots|more]", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := minioStorageRequest(tt.out)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestInstallInClusterMinIORequestsThePreservedSize(t *testing.T) {
	host := &routedRunner{routes: []route{{contains: "get pvc minio-storage", out: "minio-size[30Gi|30Gi]"}}}
	require.NoError(t, installInClusterMinIO(host, "pw", platform.LonghornSingleClass))
	assert.Contains(t, strings.Join(host.stdins, ""), "storage: 30Gi\n", "an existing 30Gi claim is never shrunk")

	fresh := &routedRunner{}
	require.NoError(t, installInClusterMinIO(fresh, "pw", platform.LonghornSingleClass))
	assert.Contains(t, strings.Join(fresh.stdins, ""), "storage: 10Gi\n")
}

func TestInstallInClusterMinIOStopsWhenTheSizeLookupFails(t *testing.T) {
	host := &routedRunner{routes: []route{{contains: "get pvc minio-storage", err: errors.New("connection refused")}}}
	err := installInClusterMinIO(host, "pw", platform.LonghornSingleClass)
	assert.ErrorContains(t, err, "backup volume size")
	assert.Empty(t, host.stdins, "nothing is applied when the current size is unknown")
}

// Include storage conditions to help diagnose rollout failures.
func TestInstallInClusterMinIONamesTheLonghornConditionOnATimeout(t *testing.T) {
	host := &routedRunner{routes: []route{
		{contains: "rollout status deployment/minio", err: errors.New("exit status 1")},
		{contains: "volumes.longhorn.io", out: "lh-volume[pvc-1234|faulted|False|insufficient storage; precheck new replica failed]"},
	}}
	err := installInClusterMinIO(host, "pw", platform.LonghornSingleClass)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pvc-1234")
	assert.Contains(t, err.Error(), "insufficient storage; precheck new replica failed")
	assert.Contains(t, err.Error(), "disk")
}

func TestFormatLonghornVolumeDiagnosis(t *testing.T) {
	assert.Empty(t, formatLonghornVolumeDiagnosis(""), "no volume yet, nothing to add")
	assert.Empty(t, formatLonghornVolumeDiagnosis("Warning: something else\n"))

	unscheduled := formatLonghornVolumeDiagnosis("lh-volume[pvc-1|faulted|False|insufficient storage; precheck new replica failed]")
	assert.Contains(t, unscheduled, "Longhorn volume pvc-1 (faulted) is not scheduled: insufficient storage; precheck new replica failed")
	assert.Contains(t, unscheduled, "disk")

	healthy := formatLonghornVolumeDiagnosis("lh-volume[pvc-2|healthy|True|]")
	assert.Contains(t, healthy, "Longhorn volume pvc-2 is healthy and scheduled")
	assert.NotContains(t, healthy, "disk")
}

func TestInstallInClusterMinIOUsesTheGivenClass(t *testing.T) {
	host := &routedRunner{}
	require.NoError(t, installInClusterMinIO(host, "pw", platform.LocalPathClass))
	assert.Contains(t, strings.Join(host.stdins, ""), "storageClassName: local-path\n")
}
