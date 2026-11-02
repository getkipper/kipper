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

	"github.com/getkipper/kipper/controller/pkg/backupscope"
	"github.com/getkipper/kipper/controller/pkg/platform"
	"github.com/getkipper/kipper/kip/internal/ssh"
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

func TestRenderExcludedNamespacesYAML(t *testing.T) {
	out := renderYAMLList([]string{"kube-system", "velero"}, "            ")
	assert.Equal(t, "            - kube-system\n            - velero", out)
}

func TestVeleroManifestSchedulesExcludeAllSystemNamespaces(t *testing.T) {
	// The daily-apps schedule and manual backups share these exclusions.
	// Rendering must preserve their order, indentation and line breaks
	// so scheduled backups receive the same valid namespace list.
	excluded := backupscope.ExcludedNamespaces()
	block := renderYAMLList(excluded, "            ")
	for _, ns := range excluded {
		assert.Contains(t, block, "- "+ns)
	}
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

func TestVeleroManifestSchedulesBackUpKipperClusterObjects(t *testing.T) {
	schedules := veleroValues(t, false)["schedules"].(map[string]any)
	require.Len(t, schedules, 2)
	for name, schedule := range schedules {
		template := schedule.(map[string]any)["template"].(map[string]any)
		assert.NotContains(t, template, "excludedResources", "%s: Velero rejects old filters next to scoped ones", name)
		assert.ElementsMatch(t, backupscope.BackedUpClusterResources(), template["includedClusterScopedResources"], name)
		assert.ElementsMatch(t, backupscope.TransientResources(), template["excludedNamespaceScopedResources"], name)
	}
}

func TestVeleroManifestForAReadOnlyLocationWritesNothing(t *testing.T) {
	values := func(readOnly bool) map[string]any {
		var chart struct {
			Spec struct {
				ValuesContent string `yaml:"valuesContent"`
			} `yaml:"spec"`
		}
		cfg := &BackupStorageConfig{Bucket: "old-backups", Region: "eu-west-1", ReadOnly: readOnly}
		require.NoError(t, yaml.Unmarshal([]byte(veleroManifest(cfg, false)), &chart))
		v := map[string]any{}
		require.NoError(t, yaml.Unmarshal([]byte(chart.Spec.ValuesContent), &v))
		return v
	}
	for _, readOnly := range []bool{false, true} {
		v := values(readOnly)
		bsl := v["configuration"].(map[string]any)["backupStorageLocation"].([]any)[0].(map[string]any)
		if readOnly {
			assert.Equal(t, "ReadOnly", bsl["accessMode"], "Velero refuses backups, deletion and maintenance only on a ReadOnly location")
		} else {
			assert.NotContains(t, bsl, "accessMode")
		}
		schedules := v["schedules"].(map[string]any)
		require.Len(t, schedules, 2)
		for name, schedule := range schedules {
			assert.Equal(t, readOnly, schedule.(map[string]any)["disabled"], name)
		}
	}
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

// TestVeleroRunsAReleaseThatFinishesCanceledVolumeBackups guards against the
// timeout and restore metadata loss caused by canceled volume backups in
// Velero 1.18.0 to 1.18.2.
func TestVeleroRunsAReleaseThatFinishesCanceledVolumeBackups(t *testing.T) {
	image, _ := veleroValues(t, false)["image"].(map[string]any)

	assert.Equal(t, "velero/velero", image["repository"])
	assert.Equal(t, "v1.18.4", image["tag"])
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
	liveBSL       string // The default backup location as "bucket|s3Url".
	bucket        string // The listing marker, or empty for a bucket with no backups.
	osRelease     string
	osImages      string
	chartValues   string
	helperConfigs string
	patches       []string
	stoppedBy     string
	movedBSL      string
	chartStuck    bool
	chartInput    string // A HelmChart spec field kip does not write, as JSON after a comma.
	fenced        int
	replicas      string
	cmds          []string
}

func (h *fakeVeleroHost) Run(cmd string) (string, error) {
	if _, inner, ok := strings.Cut(cmd, "kipper-node-ops.lock"); ok {
		h.fenced++
		_, inner, _ = strings.Cut(inner, "(\n")
		cmd = strings.TrimSuffix(inner, "\n)")
	}
	h.cmds = append(h.cmds, cmd)
	if cmd == "cat /etc/os-release" {
		return h.osRelease, nil
	}
	if strings.Contains(cmd, "helmchartconfig velero") {
		return `{"chart":"velero","repo":"r","version":"12.0.0","targetNamespace":"velero","createNamespace":true,"valuesContent":"x"` + h.chartInput + "}\n", nil
	}
	if strings.Contains(cmd, "chart-values-velero") {
		if h.chartStuck {
			return "new-values old-values 1|12.0.0|r|velero|SHA256=H 1|12.0.0|r|velero|SHA256=H [\"install\"] |HelmChartValuesContent,", nil
		}
		return "values values 1|12.0.0|r|velero|SHA256=H 1|12.0.0|r|velero|SHA256=H [\"install\"] |HelmChartValuesContent,", nil
	}
	if strings.Contains(cmd, "get configmap -l velero.io/plugin-config") {
		return h.helperConfigs, nil
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
	if strings.Contains(cmd, "get secret cloud-credentials") {
		return "secret/cloud-credentials", nil
	}
	if strings.Contains(cmd, "get crd backupstoragelocations.velero.io") && h.liveBSL != "" {
		return "customresourcedefinition.apiextensions.k8s.io/backupstoragelocations.velero.io", nil
	}
	if strings.Contains(cmd, "kind: HelmChart") {
		if !h.chartStuck && h.movedBSL != "" {
			h.liveBSL = h.movedBSL
		}
		return "helmchart.helm.cattle.io/velero configured", nil
	}
	if strings.Contains(cmd, "kipper\\.run/stopped-by") {
		if h.replicas != "" {
			return h.stoppedBy + "|" + h.replicas, nil
		}
		return h.stoppedBy + "|0", nil
	}
	if strings.Contains(cmd, "annotate deploy/velero kipper.run/stopped-by-") {
		h.stoppedBy = ""
	}
	if strings.Contains(cmd, "scale deploy/velero --replicas=") {
		h.replicas = cmd[strings.Index(cmd, "--replicas=")+len("--replicas="):]
	}
	if strings.Contains(cmd, "get backupstoragelocation default") {
		return h.liveBSL, nil
	}
	if strings.Contains(cmd, "get job "+bucketCheckJob) {
		return "1/", nil
	}
	if strings.Contains(cmd, "logs job/"+bucketCheckJob) {
		if h.bucket != "" {
			return h.bucket, nil
		}
		return bucketEmptyMarker, nil
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
	require.NoError(t, installVelero(rocky, nil, "pw", nil))
	assert.Contains(t, rocky.applied(t), "privilegedFsBackup")

	ubuntu := &fakeVeleroHost{osRelease: "ID=ubuntu\nID_LIKE=debian\nVERSION_ID=\"24.04\"\n"}
	require.NoError(t, installVelero(ubuntu, nil, "pw", nil))
	assert.NotContains(t, ubuntu.applied(t), "privilegedFsBackup")
}

func TestInstallVeleroPrivilegesFsBackupWhenAnyNodeIsRHEL(t *testing.T) {
	ubuntu := &fakeVeleroHost{
		osRelease: "ID=ubuntu\nID_LIKE=debian\nVERSION_ID=\"26.04\"\n",
		osImages:  "Ubuntu 26.04 LTS\nRocky Linux 10.2 (Red Quartz)\n",
	}
	require.NoError(t, installVelero(ubuntu, nil, "pw", nil))
	assert.Contains(t, ubuntu.applied(t), "privilegedFsBackup",
		"a Rocky worker needs privileged backups even with an Ubuntu server")

	debianOnly := &fakeVeleroHost{
		osRelease: "ID=ubuntu\nID_LIKE=debian\nVERSION_ID=\"26.04\"\n",
		osImages:  "Ubuntu 26.04 LTS\nDebian GNU/Linux 13 (trixie)\n",
	}
	require.NoError(t, installVelero(debianOnly, nil, "pw", nil))
	assert.NotContains(t, debianOnly.applied(t), "privilegedFsBackup")
}

func TestInstallVeleroCreatesTheRepositoryKeyBeforeVeleroStarts(t *testing.T) {
	h := &fakeVeleroHost{osRelease: "ID=ubuntu\nID_LIKE=debian\nVERSION_ID=\"24.04\"\n"}
	require.NoError(t, installVelero(h, nil, "pw", nil))

	key, chart := -1, -1
	for i, cmd := range h.cmds {
		if strings.Contains(cmd, "/dev/urandom") && key < 0 {
			key = i
		}
		if strings.Contains(cmd, "kind: HelmChart") && chart < 0 {
			chart = i
		}
	}
	require.GreaterOrEqual(t, key, 0, "the repository key is created")
	assert.Less(t, key, chart, "Velero writes its published default key into a missing Secret at startup")
}

func TestInstallVeleroGivesTheRestoreHelperSmallRequests(t *testing.T) {
	h := &fakeVeleroHost{osRelease: "ID=ubuntu\nID_LIKE=debian\nVERSION_ID=\"24.04\"\n"}
	require.NoError(t, installVelero(h, nil, "pw", nil))

	var applied string
	for _, cmd := range h.cmds {
		if strings.Contains(cmd, "velero.io/pod-volume-restore") {
			applied = cmd
		}
	}
	require.NotEmpty(t, applied, "Velero's restore-wait init container asks for 100m CPU by default, more than a full 4 GB server has left")
	_, manifest, found := strings.Cut(applied, "\n")
	require.True(t, found)
	manifest = strings.TrimSuffix(manifest, "KIPEOF")
	var cm struct {
		Kind     string `yaml:"kind"`
		Metadata struct {
			Namespace string            `yaml:"namespace"`
			Labels    map[string]string `yaml:"labels"`
		} `yaml:"metadata"`
		Data map[string]string `yaml:"data"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(manifest), &cm))
	assert.Equal(t, "ConfigMap", cm.Kind)
	assert.Equal(t, "velero", cm.Metadata.Namespace)
	assert.Contains(t, cm.Metadata.Labels, "velero.io/plugin-config", "Velero finds plugin settings by this label")
	assert.Equal(t, "RestoreItemAction", cm.Metadata.Labels["velero.io/pod-volume-restore"])
	assert.Equal(t, "10m", cm.Data["cpuRequest"])
	assert.Equal(t, "16Mi", cm.Data["memRequest"])
}

func TestInstallVeleroKeepsAnOperatorsOwnRestoreHelperConfig(t *testing.T) {
	h := &fakeVeleroHost{osRelease: "ID=ubuntu\nID_LIKE=debian\nVERSION_ID=\"24.04\"\n", helperConfigs: "custom-restore-helper"}
	require.NoError(t, installVelero(h, nil, "pw", nil))

	for _, cmd := range h.cmds {
		assert.NotContains(t, cmd, "name: fs-restore-action-config",
			"Velero fails every volume restore when two ConfigMaps match its restore helper selector")
	}

	ours := &fakeVeleroHost{osRelease: "ID=ubuntu\nID_LIKE=debian\nVERSION_ID=\"24.04\"\n", helperConfigs: "fs-restore-action-config"}
	require.NoError(t, installVelero(ours, nil, "pw", nil))
	applied := false
	for _, cmd := range ours.cmds {
		applied = applied || strings.Contains(cmd, "name: fs-restore-action-config")
	}
	assert.True(t, applied, "an upgrade reapplies Kipper's own config")
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

func TestStorageCredentialsStayOutOfBackups(t *testing.T) {
	host := &routedRunner{}
	require.NoError(t, installInClusterMinIO(host, "pw", platform.LonghornSingleClass))
	manifest := strings.Join(host.stdins, "")
	_, secret, found := strings.Cut(manifest, "kind: Secret")
	require.True(t, found)
	secret, _, _ = strings.Cut(secret, "---")
	assert.Contains(t, secret, "velero.io/exclude-from-backup: \"true\"")

	external := &fakeVeleroHost{}
	require.NoError(t, installVeleroCredentials(external, externalStorage, ""))
	assert.Contains(t, strings.Join(external.cmds, "\n"), "label secret cloud-credentials velero.io/exclude-from-backup=true")
}

// leasedVeleroHost is a server where another kip command holds the node lease.
type leasedVeleroHost struct {
	fakeVeleroHost
	holder string
}

type closedInput struct{ io.Writer }

func (closedInput) Close() error { return nil }

func (h *leasedVeleroHost) Start(string) (*ssh.Held, error) {
	return &ssh.Held{
		Stdin:  closedInput{io.Discard},
		Stdout: strings.NewReader(leaseHeld + " " + h.holder + "\n"),
		Wait:   func() error { return nil },
	}, nil
}

func TestInstallBackupRefusesWhileAKeyResetRuns(t *testing.T) {
	h := &leasedVeleroHost{holder: "kip backup repository-key reset"}

	err := InstallBackup(h, nil, "local", nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "kip backup repository-key reset")
	assert.Empty(t, h.cmds, "nothing touches Velero while another command holds the lease")
}

func TestInstallVeleroStartsAVeleroAResetLeftStoppedOnceTheBucketMoved(t *testing.T) {
	h := &fakeVeleroHost{osRelease: "ID=ubuntu\nID_LIKE=debian\n", liveBSL: "old-backups|https://objects.example.com",
		movedBSL: "velero|http://minio.velero.svc:9000", stoppedBy: veleroStoppedByReset}

	require.NoError(t, installVelero(h, nil, "pw", nil))

	assert.Equal(t, "1", h.replicas, "the half-emptied bucket is no longer Velero's, so nothing holds it back")
	assert.Empty(t, h.stoppedBy)
}

func TestInstallVeleroWaitsForTheLocationToMove(t *testing.T) {
	fastLocationMove(t)
	h := &fakeVeleroHost{osRelease: "ID=ubuntu\nID_LIKE=debian\n", liveBSL: "old-backups|https://objects.example.com", stoppedBy: veleroStoppedByReset}

	err := installVelero(h, nil, "pw", nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "bucket velero")
	assert.Empty(t, h.replicas, "Velero may still point at the half-emptied bucket")
	assert.Equal(t, veleroStoppedByReset, h.stoppedBy)
}

func TestInstallVeleroRefusesTheBucketAnUnfinishedResetStopped(t *testing.T) {
	h := &fakeVeleroHost{osRelease: "ID=ubuntu\nID_LIKE=debian\n", liveBSL: "velero|http://minio.velero.svc:9000", stoppedBy: veleroStoppedByReset}

	err := installVelero(h, nil, "pw", nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "kip backup repository-key reset")
	for _, cmd := range h.cmds {
		assert.NotContains(t, cmd, "kind: HelmChart", "the chart would bring Velero back with replicas: 1")
		assert.NotContains(t, cmd, "cloud-credentials --from-file", "the cleanup reads these credentials")
	}
}

func TestInstallBackupRefusesAConnectionThatCannotHoldTheLease(t *testing.T) {
	h := &fakeVeleroHost{}

	err := InstallBackup(h, nil, "local", nil)

	require.Error(t, err)
	assert.Empty(t, h.cmds)
}

func fastLocationMove(t *testing.T) {
	t.Helper()
	poll, timeout := resetPollInterval, veleroApplyTimeout
	resetPollInterval, veleroApplyTimeout = 0, 0
	t.Cleanup(func() { resetPollInterval, veleroApplyTimeout = poll, timeout })
}

// grantedVeleroHost is a server where kip gets the node lease.
type grantedVeleroHost struct {
	fakeVeleroHost
}

type closeFunc func() error

func (f closeFunc) Write(p []byte) (int, error) { return len(p), nil }
func (f closeFunc) Close() error                { return f() }

func (h *fakeVeleroHost) SurvivesSSHRateLimit() bool             { return true }
func (h *fakeVeleroHost) ConfirmFreshConnection() error          { return nil }
func (h *fakeVeleroHost) ObservedAddress(string) (string, error) { return "", nil }

func (h *grantedVeleroHost) Start(string) (*ssh.Held, error) {
	r, w := io.Pipe()
	go func() { _, _ = io.WriteString(w, leaseAcquired+" 7 0123456789abcdef0123456789abcdef\n") }()
	return &ssh.Held{Stdin: closeFunc(w.Close), Stdout: r, Wait: func() error { return nil }}, nil
}

func TestUpgradeSkipsVeleroWhileAResetIsUnfinishedOnItsBucket(t *testing.T) {
	h := &grantedVeleroHost{fakeVeleroHost{osRelease: "ID=ubuntu\nID_LIKE=debian\n", liveBSL: "velero|http://minio.velero.svc:9000", stoppedBy: veleroStoppedByReset}}

	zotUpgraded := false
	components := []SystemComponent{
		{Name: "velero", Apply: func(c CommandRunner) error { return upgradeVelero(c, nil, platform.StorageLocal) }},
		{Name: "zot", Apply: func(CommandRunner) error { zotUpgraded = true; return nil }},
	}

	var err error
	out := stdoutOf(t, func() { err = RunSystemUpgrade(h, components) })

	require.NoError(t, err)
	assert.True(t, zotUpgraded, "the components after Velero still get upgraded")
	assert.Contains(t, out, "kip backup repository-key reset")
	assert.Contains(t, out, "velero skipped")
	assert.NotContains(t, out, "✔  velero")
	assert.Contains(t, out, "✔  zot")
	assert.NotContains(t, strings.Join(h.cmds, "\n"), "kind: HelmChart")
}

func TestInstallVeleroSaysTheOldKeyStaysAfterAMove(t *testing.T) {
	h := &fakeVeleroHost{osRelease: "ID=ubuntu\nID_LIKE=debian\n", liveBSL: "old-backups|https://objects.example.com",
		movedBSL: "velero|http://minio.velero.svc:9000", stoppedBy: veleroStoppedByReset}

	out := stdoutOf(t, func() { require.NoError(t, installVelero(h, nil, "pw", nil)) })

	assert.Contains(t, out, "old repository key")
}

func TestInstallVeleroHoldsOnUntilTheChartIsApplied(t *testing.T) {
	fastLocationMove(t)
	h := &fakeVeleroHost{osRelease: "ID=ubuntu\nID_LIKE=debian\n", chartStuck: true}

	err := installVelero(h, nil, "pw", nil)

	require.Error(t, err, "a reset that takes the lease next would meet a chart apply that starts Velero again")
	assert.Contains(t, err.Error(), "Velero chart")
}

type chartCheckRunner struct{ out string }

func (r chartCheckRunner) Run(string) (string, error) { return r.out, nil }

func (r chartCheckRunner) RunStdin(cmd string, _ io.Reader) (string, error) { return r.Run(cmd) }

func TestVeleroChartAppliedNeedsTheCurrentChartAndASucceededJob(t *testing.T) {
	cases := map[string]struct {
		out     string
		applied bool
	}{
		"current chart, Job succeeded":           {"abc abc 1|12.0.0|r|velero|SHA256=H 1|12.0.0|r|velero|SHA256=H [\"install\"] |HelmChartValuesContent,\n", true},
		"values waiting, earlier Job done":       {"new old 1|12.0.0|r|velero|SHA256=H 1|12.0.0|r|velero|SHA256=H [\"install\"] |HelmChartValuesContent,\n", false},
		"version waiting, earlier Job done":      {"abc abc 1|12.1.0|r|velero|SHA256=H 1|12.0.0|r|velero|SHA256=H [\"install\"] |HelmChartValuesContent,\n", false},
		"repository waiting, earlier Job done":   {"abc abc 1|12.0.0|r2|velero|SHA256=H 1|12.0.0|r|velero|SHA256=H [\"install\"] |HelmChartValuesContent,\n", false},
		"current chart, Job still running":       {"abc abc 1|12.0.0|r|velero|SHA256=H |12.0.0|r|velero|SHA256=H [\"install\"] |HelmChartValuesContent,\n", false},
		"current chart, old Job being replaced":  {"abc abc 1|12.0.0|r|velero|SHA256=H 2026-11-01T12:00:00Z1|12.0.0|r|velero|SHA256=H [\"install\"] |HelmChartValuesContent,\n", false},
		"current chart, Job failed":              {"abc abc 1|12.0.0|r|velero|SHA256=H 0|12.0.0|r|velero|SHA256=H [\"install\"] |HelmChartValuesContent,\n", false},
		"values changed since the Job ran":       {"abc abc 1|12.0.0|r|velero|SHA256=NEW 1|12.0.0|r|velero|SHA256=OLD [\"install\"] |HelmChartValuesContent,\n", false},
		"Job ran with a removed override":        {"abc abc 1|12.0.0|r|velero|SHA256=H 1|12.0.0|r|velero|SHA256=H [\"install\",\"--set\",\"a=b\"] |HelmChartValuesContent,\n", false},
		"values from the older Secret layout":    {"abc abc 1|12.0.0|r|velero|SHA256=H 1|12.0.0|r|velero|SHA256=H [\"install\"] |values-01_HelmChart.yaml,\n", true},
		"removed structured values still stored": {"abc abc 1|12.0.0|r|velero|SHA256=H 1|12.0.0|r|velero|SHA256=H [\"install\"] |HelmChartValues,HelmChartValuesContent,\n", false},
		"removed HelmChartConfig still stored":   {"abc abc 1|12.0.0|r|velero|SHA256=H 1|12.0.0|r|velero|SHA256=H [\"install\"] |HelmChartConfigValuesContent,HelmChartValuesContent,\n", false},
		"removed chart content still stored":     {"abc abc 1|12.0.0|r|velero|SHA256=H 1|12.0.0|r|velero|SHA256=H [\"install\"] velero.tgz.base64,|HelmChartValuesContent,\n", false},
		"no install Job":                         {"abc abc 1|12.0.0|r|velero|SHA256=H none |HelmChartValuesContent,\n", false},
		"no chart values Secret yet":             {"abc e3b0 1|12.0.0|r|velero|SHA256=H 1|12.0.0|r|velero|SHA256=H [\"install\"] |HelmChartValuesContent,\n", false},
		"nothing printed":                        {"", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			applied, err := veleroChartApplied(chartCheckRunner{out: tc.out})
			require.NoError(t, err)
			assert.Equal(t, tc.applied, applied)
		})
	}
}

func TestInstallVeleroAcceptsAChartWhoseValuesAreAlreadyApplied(t *testing.T) {
	fastLocationMove(t)
	h := &fakeVeleroHost{osRelease: "ID=ubuntu\nID_LIKE=debian\n"}

	require.NoError(t, installVelero(h, nil, "pw", nil), "an apply that only touches metadata needs no new Job")
}

func TestInstallVeleroIgnoresAMarkOnARunningVelero(t *testing.T) {
	h := &fakeVeleroHost{osRelease: "ID=ubuntu\nID_LIKE=debian\n", liveBSL: "velero|http://minio.velero.svc:9000", stoppedBy: veleroStoppedByReset, replicas: "1"}

	require.NoError(t, installVelero(h, nil, "pw", nil), "a mark left by a failed start does not describe a stopped Velero")
}

func TestInstallBackupSendsEveryCommandThroughTheLease(t *testing.T) {
	h := &grantedVeleroHost{fakeVeleroHost{osRelease: "ID=ubuntu\nID_LIKE=debian\n"}}

	require.NoError(t, InstallBackup(h, nil, platform.StorageLocal, nil))

	assert.Equal(t, len(h.cmds), h.fenced, "a command after the lease ended must not run")
	assert.Contains(t, strings.Join(h.cmds, "\n"), "cat > ", "uploads go through the fence as well")
}

func TestEnsureVeleroCoversNodesLeavesTheChartOfAResetStoppedVelero(t *testing.T) {
	h := &fakeVeleroHost{
		osImages:    "Ubuntu 26.04 LTS\nRocky Linux 10.2 (Red Quartz)\n",
		chartValues: chartValuesContent(t, veleroManifest(externalStorage, false)),
		stoppedBy:   veleroStoppedByReset,
	}

	var changed bool
	out := stdoutOf(t, func() {
		var err error
		changed, err = EnsureVeleroCoversNodes(h)
		require.NoError(t, err)
	})

	assert.False(t, changed)
	assert.Empty(t, h.patches, "the chart apply would start Velero on the bucket the reset is emptying")
	assert.Contains(t, out, "kip upgrade")
}

func TestEnsureVeleroCoversNodesSaysNothingDuringAResetWhenNoNodeNeedsAChange(t *testing.T) {
	for name, h := range map[string]*fakeVeleroHost{
		"no RHEL node": {
			osImages:    "Ubuntu 26.04 LTS\nDebian GNU/Linux 13 (trixie)\n",
			chartValues: chartValuesContent(t, veleroManifest(externalStorage, false)),
			stoppedBy:   veleroStoppedByReset,
		},
		"chart already privileged": {
			osImages:    "Ubuntu 26.04 LTS\nRocky Linux 10.2 (Red Quartz)\n",
			chartValues: chartValuesContent(t, veleroManifest(externalStorage, true)),
			stoppedBy:   veleroStoppedByReset,
		},
	} {
		t.Run(name, func(t *testing.T) {
			out := stdoutOf(t, func() {
				changed, err := EnsureVeleroCoversNodes(h)
				require.NoError(t, err)
				assert.False(t, changed)
			})

			assert.Empty(t, out, "the chart would stay as it is for these nodes anyway")
		})
	}
}

func TestVeleroManifestSetsTheFieldsTheChartCheckCompares(t *testing.T) {
	manifest := veleroManifest(externalStorage, false)

	for _, field := range []string{"repo", "version", "targetNamespace"} {
		assert.Regexp(t, `(?m)^  `+field+`: \S`, manifest, "the Helm controller's Job reports %s, and an empty one never matches", field)
	}
}

func TestVeleroChartCheckReadsEachChartObjectOnce(t *testing.T) {
	assert.Equal(t, 1, strings.Count(veleroChartCheck, "get secret chart-values-velero"),
		"the values, their keys and the Job's config hash must come from one revision of the Secret")
	assert.Equal(t, 1, strings.Count(veleroChartCheck, "get configmap chart-content-velero"))
}

func TestInstallVeleroNamesAChartInputKipDoesNotManage(t *testing.T) {
	fastLocationMove(t)
	h := &fakeVeleroHost{osRelease: "ID=ubuntu\nID_LIKE=debian\n", chartInput: `,"set":{"configuration.logLevel":"debug"}`, chartStuck: true}

	err := installVelero(h, nil, "pw", nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "spec.set", "a timeout blaming the Helm controller would send the operator to the wrong place")
}
