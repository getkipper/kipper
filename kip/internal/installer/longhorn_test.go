package installer

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

// stockLonghornSettings provides a settings block followed by another YAML document.
const stockLonghornSettings = `---
# Source: longhorn/templates/default-setting.yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: longhorn-default-setting
  namespace: longhorn-system
data:
  default-setting.yaml: |-
    priority-class: longhorn-critical
    disable-revision-counter: true
---
# Source: longhorn/templates/storageclass.yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: longhorn-storageclass
`

func runSettingsFilter(t *testing.T, manifest string) (string, error) {
	t.Helper()
	dir := t.TempDir()
	in, out := filepath.Join(dir, "stock.yaml"), filepath.Join(dir, "longhorn.yaml")
	require.NoError(t, os.WriteFile(in, []byte(manifest), 0o600))
	cmd := exec.Command("sh", "-c", longhornSettingsFunc+"\n"+longhornSettingsFuncName+` "$1" "$2"`, "sh", in, out)
	if combined, err := cmd.CombinedOutput(); err != nil {
		return string(combined), err
	}
	written, err := os.ReadFile(out)
	require.NoError(t, err)
	return string(written), nil
}

func TestLonghornSettingsFilter_SwitchesOffUpgradeChecksAndUsageMetrics(t *testing.T) {
	filtered, err := runSettingsFilter(t, stockLonghornSettings)
	require.NoError(t, err, filtered)

	first := strings.SplitN(strings.TrimPrefix(filtered, "---\n"), "\n---\n", 2)[0]
	var cm struct {
		Data map[string]string `json:"data"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(first), &cm))
	var settings map[string]interface{}
	require.NoError(t, yaml.Unmarshal([]byte(cm.Data["default-setting.yaml"]), &settings))

	assert.Equal(t, false, settings["upgrade-checker"])
	assert.Equal(t, false, settings["allow-collecting-longhorn-usage-metrics"])
	assert.Equal(t, "block-for-eviction-if-contains-last-replica", settings["node-drain-policy"],
		"draining must wait for eviction of a volume's last healthy replica")
	assert.Equal(t, "longhorn-critical", settings["priority-class"], "the stock settings stay")
}

func TestLonghornSettingsFilter_RefusesAManifestItCannotPlaceTheSettingsIn(t *testing.T) {
	_, err := runSettingsFilter(t, strings.Replace(stockLonghornSettings, "  default-setting.yaml: |-", "  default-setting.yaml: |", 1))

	assert.Error(t, err, "applying Longhorn with its defaults would report to upstream")
}

func TestInstallLonghorn_AppliesTheFilteredManifest(t *testing.T) {
	r := pinHost(map[string]string{"get storageclass": "", "get nodes.longhorn.io": ""})
	require.NoError(t, installLonghorn(r, HostOS{ID: "debian", Family: FamilyDebian}, nil))

	all := strings.Join(r.commands, "\n")
	assert.Equal(t, "DEBIAN_FRONTEND=noninteractive apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq open-iscsi nfs-common", r.commands[1],
		"storage package installation follows the package check")
	assert.Contains(t, all, longhornSettingsFuncName+` "$dir/stock.yaml" "$dir/longhorn.yaml"`)
	assert.Contains(t, all, `kubectl apply -f "$dir/longhorn.yaml"`)
	assert.NotContains(t, all, "kubectl apply -f https://", "the stock manifest must not be applied unfiltered")
	assert.Contains(t, all, `{"spec":{"tags":["kipper-server"]}}`, "a fresh install tags the server")
	assert.Contains(t, all, `nodeSelector: "kipper-server"`, "and pins the single-copy class to it")
	assert.Less(t, strings.Index(all, "patch nodes.longhorn.io"), strings.Index(all, `nodeSelector: "kipper-server"`),
		"the tag exists before any volume can select it")
}

func TestInstallLonghornRefusesAHostItCannotInstallPackagesOn(t *testing.T) {
	r := &recordingRunner{}

	err := installLonghorn(r, HostOS{ID: "arch"}, nil)

	assert.ErrorContains(t, err, "arch")
	assert.Empty(t, r.commands)
}

// Sharing the install step applies these settings during upgrades too.
func TestSystemComponents_UpgradeRunsTheLonghornInstall(t *testing.T) {
	for _, c := range SystemComponents("example.com", nil, nil, PlatformState{Profile: ProfileMedium}) {
		if c.Name == "longhorn" {
			assert.Equal(t, reflect.ValueOf(InstallLonghorn).Pointer(), reflect.ValueOf(c.Apply).Pointer())
			return
		}
	}
	t.Fatal("kip upgrade has no longhorn step")
}

// runOnly runs commands and cannot upload.
type runOnly struct{}

func (runOnly) Run(string) (string, error)                 { return "", nil }
func (runOnly) RunStdin(string, io.Reader) (string, error) { return "", nil }

func TestSystemComponents_VeleroNeedsARunnerThatUploads(t *testing.T) {
	for _, c := range SystemComponents("example.com", nil, nil, PlatformState{Profile: ProfileMedium}) {
		if c.Name == "velero" {
			assert.ErrorContains(t, c.Apply(runOnly{}), "cannot upload")
			return
		}
	}
	t.Fatal("kip upgrade has no velero step")
}
