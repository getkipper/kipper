package installer

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
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
	assert.Equal(t, "do-nothing", settings["node-down-pod-deletion-policy"],
		"only kip node fence deletes the pods of a dead node")
	assert.Equal(t, false, settings["rwx-volume-fast-failover"],
		"fast RWX failover is disabled while normal share-manager failover remains available")
	assert.Equal(t, "longhorn-critical", settings["priority-class"], "the stock settings stay")
}

func TestLonghornSettingsFilter_RefusesAManifestItCannotPlaceTheSettingsIn(t *testing.T) {
	_, err := runSettingsFilter(t, strings.Replace(stockLonghornSettings, "  default-setting.yaml: |-", "  default-setting.yaml: |", 1))

	assert.Error(t, err, "applying Longhorn with its defaults would report to upstream")
}

func TestInstallLonghorn_AppliesTheFilteredManifest(t *testing.T) {
	r := pinHost(map[string]string{"get storageclass longhorn-single": "", "get storageclass longhorn-replicated": "", "get nodes.longhorn.io": ""})
	require.NoError(t, installLonghorn(r, HostOS{ID: "debian", Family: FamilyDebian}, nil))

	all := strings.Join(r.commands, "\n")
	install := slices.Index(r.commands, "DEBIAN_FRONTEND=noninteractive apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq open-iscsi nfs-common fio")
	require.Positive(t, install, "storage packages are installed")
	assert.Contains(t, r.commands[install-1], "open-iscsi", "storage package installation follows the package check")
	assert.Contains(t, all, longhornSettingsFuncName+` "$dir/stock.yaml" "$dir/longhorn.yaml"`)
	assert.Contains(t, all, `kubectl apply -f "$dir/longhorn.yaml"`)
	assert.NotContains(t, all, "kubectl apply -f https://", "the stock manifest must not be applied unfiltered")
	assert.Contains(t, all, `{"spec":{"tags":["kipper-server","kipper-replica"]}}`, "a fresh install tags the server")
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

// A kip whose target is several minors ahead must re-apply what the cluster
// already runs: applying the target would skip minors, which Longhorn refuses.
func TestInstallLonghornReappliesTheInstalledReleaseOnAnExistingCluster(t *testing.T) {
	r := pinHost(map[string]string{
		"get storageclass longhorn-single":      "",
		"get storageclass longhorn-replicated":  "",
		"get nodes.longhorn.io":                 "",
		"daemonset longhorn-manager":            "longhornio/longhorn-manager:v1.7.3",
		"current-longhorn-version":              "v1.7.3",
		"get crd --request-timeout=30s -o name": longhornCRDs,
	})
	require.NoError(t, installLonghorn(r, HostOS{ID: "debian", Family: FamilyDebian}, nil))

	all := strings.Join(r.commands, "\n")
	assert.Contains(t, all, "longhorn/longhorn/v1.7.3/deploy/longhorn.yaml")
	assert.NotContains(t, all, "longhorn/longhorn/"+longhornVersion+"/")
}

func TestInstallLonghornGivesAFreshHostTheTarget(t *testing.T) {
	r := pinHost(map[string]string{"get storageclass longhorn-single": "", "get storageclass longhorn-replicated": "", "get nodes.longhorn.io": ""})
	require.NoError(t, installLonghorn(r, HostOS{ID: "debian", Family: FamilyDebian}, nil))

	assert.Contains(t, strings.Join(r.commands, "\n"), "longhorn/longhorn/"+longhornVersion+"/deploy/longhorn.yaml")
}

func TestInstallLonghornFinishesAnUnfinishedHopInsteadOfGoingBack(t *testing.T) {
	r := pinHost(map[string]string{
		"get storageclass longhorn-single":      "",
		"get storageclass longhorn-replicated":  "",
		"get nodes.longhorn.io":                 "",
		"configmap " + LonghornHopRecordName:    "v1.8.2\tv1.9.2\t\tb\te",
		"daemonset longhorn-manager":            "longhornio/longhorn-manager:v1.9.2",
		"current-longhorn-version":              "v1.8.2",
		"get crd --request-timeout=30s -o name": longhornCRDs,
	})
	require.NoError(t, installLonghorn(r, HostOS{ID: "debian", Family: FamilyDebian}, nil))

	all := strings.Join(r.commands, "\n")
	assert.Contains(t, all, "longhorn/longhorn/v1.9.2/deploy/longhorn.yaml")
	assert.NotContains(t, all, "longhorn/longhorn/v1.8.2/deploy")
}

// A prepared hop has no version marker at its target, so a plain upgrade
// keeps the old release.
func TestInstallLonghornKeepsTheOldReleaseForAHopThatWasNeverApplied(t *testing.T) {
	r := pinHost(map[string]string{
		"get storageclass":                      "",
		"get nodes.longhorn.io":                 "",
		"configmap " + LonghornHopRecordName:    "v1.8.2\tv1.9.2\t\tb\te",
		"daemonset longhorn-manager":            "longhornio/longhorn-manager:v1.8.2",
		"current-longhorn-version":              "v1.8.2",
		"get crd --request-timeout=30s -o name": longhornCRDs,
	})
	require.NoError(t, installLonghorn(r, HostOS{ID: "debian", Family: FamilyDebian}, nil))

	all := strings.Join(r.commands, "\n")
	assert.Contains(t, all, "longhorn/longhorn/v1.8.2/deploy/longhorn.yaml")
	assert.NotContains(t, all, "longhorn/longhorn/v1.9.2/deploy")
}

func TestInstallLonghornRefusesAReleaseNewerThanItsTarget(t *testing.T) {
	r := pinHost(map[string]string{
		"daemonset longhorn-manager":            "longhornio/longhorn-manager:v1.13.0",
		"current-longhorn-version":              "v1.13.0",
		"get crd --request-timeout=30s -o name": longhornCRDs,
	})
	err := installLonghorn(r, HostOS{ID: "debian", Family: FamilyDebian}, nil)

	assert.ErrorContains(t, err, "newer than")
	assert.Empty(t, r.ranWith("kubectl apply"), "nothing is applied")
}

func longhornSettingsFromFilter(t *testing.T) map[string]interface{} {
	t.Helper()
	filtered, err := runSettingsFilter(t, stockLonghornSettings)
	require.NoError(t, err, filtered)
	first := strings.SplitN(strings.TrimPrefix(filtered, "---\n"), "\n---\n", 2)[0]
	var cm struct {
		Data map[string]string `json:"data"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(first), &cm))
	var settings map[string]interface{}
	require.NoError(t, yaml.Unmarshal([]byte(cm.Data["default-setting.yaml"]), &settings))
	return settings
}

// Fast rebuild with fast-check can rebuild a replica with missing data
// (Longhorn issue 14096). No released Longhorn carries the fix.
func TestLonghornSettingsFilter_TurnsOffFastReplicaRebuild(t *testing.T) {
	assert.Equal(t, false, longhornSettingsFromFilter(t)["fast-replica-rebuild-enabled"])
}

// These equal Longhorn's defaults today and are written so a hop that changes
// a default cannot change them.
func TestLonghornSettingsFilter_PinsTheStorageDefaultsKipperRelies(t *testing.T) {
	s := longhornSettingsFromFilter(t)
	assert.Equal(t, "disabled", s["default-data-locality"])
	assert.Equal(t, "disabled", s["replica-auto-balance"])
	assert.Equal(t, false, s["rwx-volume-fast-failover"])
	assert.Equal(t, true, s["allow-volume-creation-with-degraded-availability"])
}

// kip node fence controls pod deletion after checking that the node is gone,
// so Longhorn must leave pods on a down node in place.
func TestLonghornSettingsFilter_LeavesPodsOnADownNodeAlone(t *testing.T) {
	assert.Equal(t, "do-nothing", longhornSettingsFromFilter(t)["node-down-pod-deletion-policy"])
}

// longhorn173Settings are the setting names in longhorn-manager v1.7.3
// types/setting.go that kip writes. A key outside it would make Longhorn
// v1.7.3 to v1.9.2 drop every default setting kip writes.
var longhorn173Settings = map[string]bool{
	"upgrade-checker": true, "allow-collecting-longhorn-usage-metrics": true, "node-drain-policy": true,
	"fast-replica-rebuild-enabled": true, "default-data-locality": true, "replica-auto-balance": true,
	"rwx-volume-fast-failover": true, "node-down-pod-deletion-policy": true,
	"allow-volume-creation-with-degraded-availability": true,
}

func TestLonghornSettingsExistOnTheOldestReleaseOfThePath(t *testing.T) {
	require.Equal(t, "v1.7.3", longhornPath[0].Version, "check every key against the new oldest release, then update longhorn173Settings")
	for _, setting := range longhornSettings {
		key, _, _ := strings.Cut(setting, ":")
		assert.True(t, longhorn173Settings[key], "%s is not a Longhorn v1.7.3 setting", key)
	}
}

// Engine upgrades run only inside kip upgrade --longhorn, after a backup, and
// a Longhorn backup target would bring the 14121 memory leak.
func TestLonghornSettingsFilter_NeverEnablesEngineUpgradesOrABackupTarget(t *testing.T) {
	s := longhornSettingsFromFilter(t)
	for _, key := range []string{"concurrent-automatic-engine-upgrade-per-node-limit", "backup-target", "backup-target-credential-secret"} {
		_, ok := s[key]
		assert.False(t, ok, key)
	}
}
