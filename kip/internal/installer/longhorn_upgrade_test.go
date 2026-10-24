package installer

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeLonghorn simulates a Longhorn cluster that answers kip's kubectl calls:
// applying a manifest moves the manager at once, and an engine phase moves
// every volume Longhorn can upgrade onto the default engine.
type fakeLonghorn struct {
	applied, running string
	record           map[string]string
	engineLimit      string
	volumes          []fakeVolume
	nodes            []NodeFacts
	v1beta1          bool
	stuckAt          string
	applyErr         error
	depsErr          error
	commands         []string
	applies          []string
	limits           []string
	images           map[string]bool
	deleted          []string
}

type fakeVolume struct {
	name, state, robustness, engine string
	replicas                        int
}

func newFakeLonghorn(version string) *fakeLonghorn {
	return &fakeLonghorn{
		applied: version, running: version, engineLimit: "0",
		volumes: []fakeVolume{
			{name: "pvc-db", state: "attached", robustness: "healthy", engine: version, replicas: 1},
			{name: "pvc-idle", state: "detached", robustness: "unknown", engine: version, replicas: 1},
		},
		nodes: []NodeFacts{{Name: "server-1", KubeletVersion: "v1.36.2+k3s1", OSImage: "Ubuntu 24.04.3 LTS", Kernel: "6.8.0-85-generic"}},
	}
}

var manifestURL = regexp.MustCompile(`longhorn/longhorn/(v[^/]+)/deploy/longhorn.yaml`)

func (f *fakeLonghorn) Run(cmd string) (string, error) {
	f.commands = append(f.commands, cmd)
	switch {
	case strings.Contains(cmd, "cat /etc/os-release"):
		return "ID=ubuntu\nVERSION_ID=\"24.04\"\n", nil
	case strings.Contains(cmd, "dpkg") || strings.Contains(cmd, "open-iscsi"):
		return "present", f.depsErr
	case strings.Contains(cmd, "multipathd"):
		return "absent", nil
	case strings.Contains(cmd, "get configmap "+LonghornHopRecordName):
		if f.record == nil {
			return "", nil
		}
		r := f.record
		return strings.Join([]string{r["from"], r["to"], r["started"], r["backup"], r["export"]}, "\t"), nil
	case strings.Contains(cmd, "delete configmap "+LonghornHopRecordName):
		f.record = nil
		return "", nil
	case cmd == longhornImageQuery:
		return "longhornio/longhorn-manager:" + f.applied, nil
	case cmd == longhornRunningQuery:
		return f.running, nil
	case cmd == longhornCRDQuery:
		return longhornCRDs, nil
	case strings.Contains(cmd, "kubectl apply -f \"$dir/longhorn.yaml\""):
		v := manifestURL.FindStringSubmatch(cmd)[1]
		f.applies = append(f.applies, v)
		if f.applyErr != nil {
			return "", f.applyErr
		}
		f.applied = v
		if v != f.stuckAt {
			f.running = v
		}
		return "", nil
	case strings.Contains(cmd, "delete engineimages.longhorn.io "):
		name := regexp.MustCompile(`engineimages\.longhorn\.io (\S+)`).FindStringSubmatch(cmd)[1]
		f.deleted = append(f.deleted, name)
		delete(f.images, strings.TrimPrefix(name, "ei-"))
		return "", nil
	case strings.Contains(cmd, "patch settings.longhorn.io "+engineUpgradeSetting):
		f.engineLimit = regexp.MustCompile(`"value":"(\d+)"`).FindStringSubmatch(cmd)[1]
		f.limits = append(f.limits, f.engineLimit)
		return "", nil
	case cmd == longhornWorkloadQuery:
		return f.workloads(), nil
	case cmd == longhornSettingQuery:
		return fmt.Sprintf(`{"items":[{"metadata":{"name":"current-longhorn-version"},"value":%q},{"metadata":{"name":"default-engine-image"},"value":"longhornio/longhorn-engine:%s"},{"metadata":{"name":"default-instance-manager-image"},"value":"longhornio/longhorn-instance-manager:%s"}]}`, f.running, f.running, f.running), nil
	case cmd == longhornObjectQuery:
		return f.objects(), nil
	case cmd == kubeNodeQuery:
		return f.kubeNodes(), nil
	case strings.Contains(cmd, "kubectl get crd --request-timeout=30s -o json"):
		stored := `["v1beta2"]`
		if f.v1beta1 {
			stored = `["v1beta1","v1beta2"]`
		}
		return `{"items":[{"metadata":{"name":"volumes.longhorn.io"},"status":{"storedVersions":` + stored + `}}]}`, nil
	case strings.Contains(cmd, "-o yaml"):
		return "items: []\n", nil
	case strings.Contains(cmd, "get nodes -l"):
		return "node/server-1\n", nil
	case strings.Contains(cmd, "get nodes.longhorn.io"):
		return `["kipper-server","kipper-replica"]`, nil
	case strings.Contains(cmd, "get storageclass longhorn-replicated"):
		return replicatedClass, nil
	case strings.Contains(cmd, "get storageclass"):
		return pinnedClass, nil
	}
	return "", nil
}

func (f *fakeLonghorn) RunStdin(cmd string, stdin io.Reader) (string, error) {
	b, _ := io.ReadAll(stdin)
	if strings.Contains(string(b), "name: "+LonghornHopRecordName) {
		f.record = map[string]string{}
		for _, key := range []string{"from", "to", "started", "backup", "export"} {
			if m := regexp.MustCompile(`(?m)^  ` + key + `: "([^"]*)"`).FindStringSubmatch(string(b)); m != nil {
				f.record[key] = m[1]
			}
		}
	}
	return f.Run(cmd)
}

func (f *fakeLonghorn) workloads() string {
	var items []string
	for _, name := range longhornSettledWorkloads {
		kind, n, _ := strings.Cut(name, "/")
		image := "x"
		if n == "longhorn-manager" {
			image = "longhornio/longhorn-manager:" + f.applied
		}
		items = append(items, fmt.Sprintf(`{"kind":%q,"metadata":{"name":%q,"generation":1},"spec":{"replicas":1,"template":{"spec":{"containers":[{"name":%q,"image":%q}]}}},"status":{"observedGeneration":1,"replicas":1,"updatedReplicas":1,"availableReplicas":1,"desiredNumberScheduled":1,"updatedNumberScheduled":1,"numberAvailable":1}}`, kind, n, n, image))
	}
	return `{"items":[` + strings.Join(items, ",") + `]}`
}

func (f *fakeLonghorn) objects() string {
	if f.images == nil {
		f.images = map[string]bool{}
	}
	f.images[f.running] = true
	refs := map[string]int{}
	var items []string
	for i := range f.volumes {
		v := &f.volumes[i]
		byDesign := v.state == "attached" && v.robustness == "degraded"
		if f.engineLimit == "1" && !byDesign {
			v.engine = f.running
		}
		f.images[v.engine] = true
		refs[v.engine]++
		image := "longhornio/longhorn-engine:" + v.engine
		items = append(items,
			fmt.Sprintf(`{"kind":"Volume","metadata":{"name":%q},"spec":{"numberOfReplicas":%d,"image":%q,"dataEngine":"v1"},"status":{"state":%q,"robustness":%q,"currentImage":%q}}`, v.name, v.replicas, image, v.state, v.robustness, image),
			fmt.Sprintf(`{"kind":"Replica","metadata":{"name":"%s-r"},"spec":{"volumeName":%q,"nodeID":"server-1","healthyAt":"t"},"status":{"currentState":"running"}}`, v.name, v.name))
		modes := `{}`
		if v.state == "attached" {
			modes = fmt.Sprintf(`{"%s-r":"RW"}`, v.name)
		}
		items = append(items, fmt.Sprintf(`{"kind":"Engine","metadata":{"name":"%s-e"},"spec":{"volumeName":%q},"status":{"currentState":"running","replicaModeMap":%s}}`, v.name, v.name, modes))
	}
	for e := range f.images {
		items = append(items, fmt.Sprintf(`{"kind":"EngineImage","metadata":{"name":"ei-%s"},"spec":{"image":"longhornio/longhorn-engine:%s"},"status":{"state":"deployed","cliAPIVersion":10,"controllerAPIVersion":5,"refCount":%d}}`, e, e, refs[e]))
	}
	items = append(items,
		fmt.Sprintf(`{"kind":"InstanceManager","metadata":{"name":"im"},"spec":{"image":"longhornio/longhorn-instance-manager:%s","nodeID":"server-1","dataEngine":"v1"},"status":{"currentState":"running"}}`, f.running))
	for _, n := range f.nodes {
		items = append(items, fmt.Sprintf(`{"kind":"Node","metadata":{"name":%q}}`, n.Name))
	}
	return `{"items":[` + strings.Join(items, ",") + `]}`
}

func (f *fakeLonghorn) kubeNodes() string {
	b, _ := json.Marshal(map[string]any{"items": func() []any {
		var out []any
		for _, n := range f.nodes {
			var taints []any
			if n.OutOfService {
				taints = append(taints, map[string]any{"key": outOfServiceTaint, "effect": "NoExecute"})
			}
			out = append(out, map[string]any{"metadata": map[string]any{"name": n.Name}, "spec": map[string]any{"taints": taints},
				"status": map[string]any{"nodeInfo": map[string]any{"kubeletVersion": n.KubeletVersion, "osImage": n.OSImage, "kernelVersion": n.Kernel}}})
		}
		return out
	}()})
	return string(b)
}

func (f *fakeLonghorn) ran(fragment string) bool {
	for _, c := range f.commands {
		if strings.Contains(c, fragment) {
			return true
		}
	}
	return false
}

type upgradeRun struct {
	upgrade LonghornUpgrade
	backups []string
	out     *strings.Builder
}

func newUpgradeRun(t *testing.T) *upgradeRun {
	r := &upgradeRun{out: &strings.Builder{}}
	r.upgrade = LonghornUpgrade{
		Out:         r.out,
		ExportDir:   t.TempDir(),
		StepTimeout: time.Millisecond,
		Interval:    time.Microsecond,
		Backup: func(to string) (string, error) {
			name := "before-longhorn-" + to
			r.backups = append(r.backups, name)
			return name, nil
		},
	}
	return r
}

func TestLonghornUpgradeHopsOneMinorAtATimeWithABackupBeforeEach(t *testing.T) {
	f := newFakeLonghorn("v1.7.3")
	run := newUpgradeRun(t)

	require.NoError(t, run.upgrade.Apply(f), run.out.String())

	assert.Equal(t, []string{"v1.8.2", "v1.9.2", "v1.10.2", "v1.11.3"}, f.applies)
	assert.Equal(t, []string{"before-longhorn-v1.8.2", "before-longhorn-v1.9.2", "before-longhorn-v1.10.2", "before-longhorn-v1.11.3"}, run.backups)
	assert.Equal(t, "v1.11.3", f.running)
	assert.Nil(t, f.record, "the record is removed once the last hop settled")
	assert.Equal(t, "0", f.engineLimit, "engine upgrades are off when kip is done")
	for _, v := range f.volumes {
		assert.Equal(t, "v1.11.3", v.engine, v.name)
	}
	assert.True(t, f.ran("get crd --request-timeout=30s -o json"), "the stored versions are checked before the 1.10 hop")
}

func TestLonghornUpgradeSavesLonghornsObjectsOffTheClusterBeforeEachHop(t *testing.T) {
	f := newFakeLonghorn("v1.10.2")
	run := newUpgradeRun(t)
	require.NoError(t, run.upgrade.Apply(f), run.out.String())

	dirs, err := filepath.Glob(filepath.Join(run.upgrade.ExportDir, "*-v1.10.2-to-v1.11.3"))
	require.NoError(t, err)
	require.Len(t, dirs, 1)
	info, err := os.Stat(dirs[0])
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
	for _, kind := range longhornExportKinds {
		fi, err := os.Stat(filepath.Join(dirs[0], kind+".yaml"))
		require.NoError(t, err, kind)
		assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm(), kind)
	}
}

func TestLonghornUpgradeRecordsTheHopBeforeApplyingIt(t *testing.T) {
	f := newFakeLonghorn("v1.10.2")
	f.applyErr = errors.New("connection reset")
	run := newUpgradeRun(t)

	err := run.upgrade.Apply(f)

	assert.ErrorContains(t, err, "Run kip upgrade --longhorn again")
	require.NotNil(t, f.record, "an interrupted apply leaves the record")
	assert.Equal(t, "v1.10.2", f.record["from"])
	assert.Equal(t, "v1.11.3", f.record["to"])
	assert.Equal(t, "before-longhorn-v1.11.3", f.record["backup"])
}

// Once the new manifest reached the cluster, the next run finishes the hop
// under the backup taken when it started.
func TestLonghornUpgradeResumesAnInterruptedHopWithoutANewBackup(t *testing.T) {
	f := newFakeLonghorn("v1.10.2")
	f.applied = "v1.11.3"
	f.record = map[string]string{"from": "v1.10.2", "to": "v1.11.3", "backup": "before-longhorn-v1.11.3", "export": "/home/op/.kip/x"}
	run := newUpgradeRun(t)

	require.NoError(t, run.upgrade.Apply(f), run.out.String())

	assert.Equal(t, []string{"v1.11.3"}, f.applies)
	assert.Empty(t, run.backups, "the hop's backup was taken when it started")
	assert.Nil(t, f.record)
	assert.Contains(t, run.out.String(), "resuming the unfinished Longhorn step from v1.10.2 to v1.11.3")
}

// A later kip can carry another patch of the same minor. It finishes the
// hop an earlier kip started and continues along its own path.
func TestLonghornUpgradeFinishesAHopAnEarlierKipStarted(t *testing.T) {
	f := newFakeLonghorn("v1.9.2")
	f.applied = "v1.10.1"
	f.record = map[string]string{"from": "v1.9.2", "to": "v1.10.1", "backup": "before-longhorn-v1.10.1", "export": "/home/op/.kip/x"}
	run := newUpgradeRun(t)

	require.NoError(t, run.upgrade.Apply(f), run.out.String())

	assert.Equal(t, []string{"v1.10.1", "v1.10.2", "v1.11.3"}, f.applies)
	assert.Contains(t, run.out.String(), "Longhorn v1.9.2 → v1.10.1: settled")
}

func TestLonghornUpgradeFinishesAnInterruptedPatchStep(t *testing.T) {
	f := newFakeLonghorn("v1.10.1")
	f.applied = "v1.10.2"
	f.record = map[string]string{"from": "v1.10.1", "to": "v1.10.2", "backup": "before-longhorn-v1.10.2", "export": "/home/op/.kip/x"}
	run := newUpgradeRun(t)

	require.NoError(t, run.upgrade.Apply(f), run.out.String())

	assert.Equal(t, []string{"v1.10.2", "v1.11.3"}, f.applies)
	assert.Equal(t, []string{"before-longhorn-v1.11.3"}, run.backups)
}

// A hop recorded but never applied changed nothing in Longhorn, so the next
// run starts it again with every check and a fresh backup.
func TestLonghornUpgradeStartsAHopThatWasNeverAppliedAgain(t *testing.T) {
	f := newFakeLonghorn("v1.10.2")
	f.record = map[string]string{"from": "v1.10.2", "to": "v1.11.3", "backup": "before-longhorn-v1.11.3-old", "export": "/home/op/.kip/x"}
	run := newUpgradeRun(t)

	require.NoError(t, run.upgrade.Apply(f), run.out.String())

	assert.Equal(t, []string{"v1.11.3"}, f.applies)
	assert.Equal(t, []string{"before-longhorn-v1.11.3"}, run.backups)
	assert.Nil(t, f.record)
	assert.Contains(t, run.out.String(), "was recorded but never applied")
}

func TestLonghornUpgradeChecksAHopThatWasNeverAppliedLikeANewOne(t *testing.T) {
	f := newFakeLonghorn("v1.10.2")
	f.volumes[1].robustness = "faulted"
	f.record = map[string]string{"from": "v1.10.2", "to": "v1.11.3", "backup": "before-longhorn-v1.11.3", "export": "/home/op/.kip/x"}
	run := newUpgradeRun(t)

	err := run.upgrade.Apply(f)

	assert.ErrorContains(t, err, "pvc-idle is faulted")
	assert.Empty(t, f.applies)
	assert.Empty(t, run.backups)
}

// Components between releases cannot pass the old release's readiness checks.
// Resuming still requires healthy volumes and compatibility with the new release.
func TestLonghornUpgradeChecksAPartlyAppliedHopBeforeFinishingIt(t *testing.T) {
	f := newFakeLonghorn("v1.10.2")
	f.applied = "v1.11.3"
	f.volumes[1].robustness = "faulted"
	f.nodes[0].KubeletVersion = "v1.33.4+k3s1"
	f.record = map[string]string{"from": "v1.10.2", "to": "v1.11.3", "backup": "before-longhorn-v1.11.3", "export": "/home/op/.kip/x"}
	run := newUpgradeRun(t)

	err := run.upgrade.Apply(f)

	assert.ErrorContains(t, err, "pvc-idle is faulted")
	assert.ErrorContains(t, err, "v1.33.4")
	assert.Empty(t, f.applies)
	assert.NotNil(t, f.record, "the record stays until the hop is finished")
}

// TestLonghornUpgradeRefusesToResumeWhileANodeIsOutOfService verifies that a node
// fenced during a hop is reported before the manifest is reapplied.
func TestLonghornUpgradeRefusesToResumeWhileANodeIsOutOfService(t *testing.T) {
	f := newFakeLonghorn("v1.10.2")
	f.applied = "v1.11.3"
	f.nodes = append(f.nodes, NodeFacts{Name: "worker-1", KubeletVersion: "v1.36.2+k3s1", OSImage: "Ubuntu 24.04.3 LTS", Kernel: "6.8.0-85-generic", OutOfService: true})
	f.record = map[string]string{"from": "v1.10.2", "to": "v1.11.3", "backup": "before-longhorn-v1.11.3", "export": "/home/op/.kip/x"}
	run := newUpgradeRun(t)

	err := run.upgrade.Apply(f)

	assert.ErrorContains(t, err, "was not resumed")
	assert.ErrorContains(t, err, "node worker-1 is out of service")
	assert.ErrorContains(t, err, "kip node remove --force")
	assert.Empty(t, f.applies)
	assert.NotNil(t, f.record, "the record stays until the hop is finished")
}

func TestLonghornUpgradeStopsWhenAVolumeIsFaulted(t *testing.T) {
	f := newFakeLonghorn("v1.7.3")
	f.volumes[1].robustness = "faulted"
	run := newUpgradeRun(t)

	err := run.upgrade.Apply(f)

	assert.ErrorContains(t, err, "pvc-idle is faulted")
	assert.ErrorContains(t, err, "nothing in Longhorn was changed")
	assert.Empty(t, f.applies)
	assert.Empty(t, run.backups)
	assert.Nil(t, f.record)
}

func TestLonghornUpgradeStopsWhenTheBackupFails(t *testing.T) {
	f := newFakeLonghorn("v1.7.3")
	run := newUpgradeRun(t)
	run.upgrade.Backup = func(string) (string, error) {
		return "", errors.New("backup before-longhorn-v1.8.2 ended PartiallyFailed")
	}

	err := run.upgrade.Apply(f)

	assert.ErrorContains(t, err, "PartiallyFailed")
	assert.Empty(t, f.applies)
	assert.Nil(t, f.record)
}

func TestLonghornUpgradeRefusesThe110HopWhileACRDStillStoresV1beta1(t *testing.T) {
	f := newFakeLonghorn("v1.9.2")
	f.v1beta1 = true
	run := newUpgradeRun(t)

	err := run.upgrade.Apply(f)

	assert.ErrorContains(t, err, "volumes.longhorn.io still lists v1beta1")
	assert.Empty(t, f.applies)
}

func TestLonghornUpgradeRefusesAKubernetesBelowTheHopsFloor(t *testing.T) {
	f := newFakeLonghorn("v1.10.2")
	f.nodes[0].KubeletVersion = "v1.33.5+k3s1"
	run := newUpgradeRun(t)

	err := run.upgrade.Apply(f)

	assert.ErrorContains(t, err, "needs v1.34 or later. Run 'kip upgrade --k3s' first")
	assert.Empty(t, f.applies)
}

func TestLonghornUpgradeRefusesARHEL97LevelNode(t *testing.T) {
	f := newFakeLonghorn("v1.7.3")
	f.nodes[0].OSImage, f.nodes[0].Kernel = "Rocky Linux 9.8 (Blue Onyx)", "5.14.0-687.41.1.el9_8.x86_64"
	run := newUpgradeRun(t)

	err := run.upgrade.Apply(f)

	assert.ErrorContains(t, err, "13606")
	assert.Empty(t, f.applies)
}

func TestLonghornUpgradeCatchesUpLaggingEnginesUnderABackupFirst(t *testing.T) {
	f := newFakeLonghorn(longhornVersion)
	f.volumes[1].engine = "v1.10.2"
	run := newUpgradeRun(t)

	require.NoError(t, run.upgrade.Apply(f), run.out.String())

	assert.Equal(t, []string{longhornVersion}, f.applies)
	assert.Equal(t, []string{"before-longhorn-" + longhornVersion}, run.backups)
	assert.Equal(t, longhornVersion, f.volumes[1].engine)
	assert.Equal(t, []string{"0", "1", "0", "0"}, f.limits, "off at the start, on for the engine phase, off after it and on the way out")
}

// A shared volume that wants three copies on a one-node cluster is degraded
// by design and cannot be upgraded live, so it keeps its engine without
// blocking the hops.
func TestLonghornUpgradeLetsAByDesignDegradedVolumeKeepItsEngine(t *testing.T) {
	f := newFakeLonghorn("v1.10.2")
	f.volumes[0].robustness = "degraded"
	f.volumes[0].replicas = 3
	run := newUpgradeRun(t)

	require.NoError(t, run.upgrade.Apply(f), run.out.String())

	assert.Equal(t, "v1.10.2", f.volumes[0].engine)
	assert.Equal(t, longhornVersion, f.volumes[1].engine)
	assert.Contains(t, run.out.String(), "pvc-db wants more replicas than the cluster has nodes, so it keeps its older engine, which Longhorn "+longhornVersion+" still supports")
	assert.Contains(t, run.out.String(), "kip node add")
	assert.NotContains(t, f.deleted, "ei-v1.10.2", "an engine image in use stays")
}

// Each step leaves its engine image deployed on every node. Once nothing
// uses one, kip removes it.
func TestLonghornUpgradeRemovesEngineImagesNothingUses(t *testing.T) {
	f := newFakeLonghorn("v1.10.2")
	f.images = map[string]bool{"v1.8.2": true}
	run := newUpgradeRun(t)

	require.NoError(t, run.upgrade.Apply(f), run.out.String())

	assert.ElementsMatch(t, []string{"ei-v1.8.2", "ei-v1.10.2"}, f.deleted)
	assert.Contains(t, run.out.String(), "removed the unused engine image longhornio/longhorn-engine:v1.8.2")
}

// Cleanup requires Longhorn v1.10's admission check to protect images in use.
func TestLonghornEngineImagesStayBeforeLonghornGuardsThem(t *testing.T) {
	f := newFakeLonghorn("v1.9.2")
	f.images = map[string]bool{"v1.8.2": true}
	newUpgradeRun(t).upgrade.pruneEngineImages(f)
	assert.Empty(t, f.deleted)
}

func TestLonghornUpgradeAtTheTargetOnlyReappliesIt(t *testing.T) {
	f := newFakeLonghorn(longhornVersion)
	run := newUpgradeRun(t)

	require.NoError(t, run.upgrade.Apply(f), run.out.String())

	assert.Equal(t, []string{longhornVersion}, f.applies)
	assert.Empty(t, run.backups, "re-applying the running release changes no engine")
	assert.Equal(t, []string{"0", "0"}, f.limits)
}

func TestLonghornUpgradeNamesWhatDidNotSettleAndHowToResume(t *testing.T) {
	f := newFakeLonghorn("v1.10.2")
	f.stuckAt = "v1.11.3"
	run := newUpgradeRun(t)

	err := run.upgrade.Apply(f)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "has not finished upgrading to v1.11.3")
	assert.Contains(t, err.Error(), "Run kip upgrade --longhorn again to keep waiting")
	assert.NotNil(t, f.record, "the record stays for the resume")
	assert.True(t, f.ran("logs daemonset/longhorn-manager"), "the manager's log says why it refuses")
}

// A run interrupted during an engine phase leaves engine upgrades on, and the
// next run turns them off before host preparation, even if preparation fails.
func TestLonghornUpgradeTurnsInheritedEngineUpgradesOffOnAnEarlyFailure(t *testing.T) {
	f := newFakeLonghorn("v1.10.2")
	f.engineLimit = "1"
	f.depsErr = errors.New("package mirror unreachable")
	run := newUpgradeRun(t)

	require.Error(t, run.upgrade.Apply(f))
	assert.Equal(t, "0", f.engineLimit)
	stop, deps := -1, -1
	for i, c := range f.commands {
		if stop < 0 && strings.Contains(c, engineUpgradeSetting) {
			stop = i
		}
		if deps < 0 && strings.Contains(c, "cat /etc/os-release") {
			deps = i
		}
	}
	require.GreaterOrEqual(t, stop, 0)
	assert.Less(t, stop, deps, "engine upgrades are off before the slow host preparation")
}

func TestLonghornUpgradeTurnsInheritedEngineUpgradesOffBeforeTheFirstCheck(t *testing.T) {
	f := newFakeLonghorn("v1.10.2")
	f.engineLimit = "1"
	f.volumes[0].robustness = "faulted"
	f.volumes[1].engine = "v1.9.2"
	run := newUpgradeRun(t)

	require.Error(t, run.upgrade.Apply(f))
	require.NotEmpty(t, f.limits)
	assert.Equal(t, "0", f.limits[0])
	assert.Equal(t, "v1.9.2", f.volumes[1].engine, "no engine moved without a backup")
}

func TestLonghornUpgradeRefusesAFreshHost(t *testing.T) {
	r := &routedRunner{routes: []route{{contains: "cat /etc/os-release", out: "ID=ubuntu\nVERSION_ID=\"24.04\"\n"}, {contains: "dpkg", out: "present"}}}
	run := newUpgradeRun(t)
	err := run.upgrade.Apply(r)
	assert.ErrorContains(t, err, "not installed")
}

func TestParseCRDsStoringV1beta1IgnoresOtherGroups(t *testing.T) {
	stale, err := parseCRDsStoringV1beta1([]byte(`{"items":[
 {"metadata":{"name":"volumes.longhorn.io"},"status":{"storedVersions":["v1beta2"]}},
 {"metadata":{"name":"settings.longhorn.io"},"status":{"storedVersions":["v1beta1","v1beta2"]}},
 {"metadata":{"name":"widgets.example.com"},"status":{"storedVersions":["v1beta1"]}}]}`))
	require.NoError(t, err)
	assert.Equal(t, []string{"settings.longhorn.io"}, stale)
}

func TestKubernetesFloorReadsK3sVersions(t *testing.T) {
	r, _ := longhornReleaseFor("v1.11.3")
	assert.Empty(t, kubernetesFloorProblems([]NodeFacts{{Name: "a", KubeletVersion: "v1.36.2+k3s1"}, {Name: "b", KubeletVersion: "v1.34.0+k3s1"}}, r))
	assert.Len(t, kubernetesFloorProblems([]NodeFacts{{Name: "a", KubeletVersion: "v1.33.9+k3s1"}, {Name: "b", KubeletVersion: "garbage"}}, r), 2)
}

func TestLonghornPlanNamesTheFirstHopWhoseKubernetesFloorANodeMisses(t *testing.T) {
	plan, err := PlanLonghornUpgrade("v1.7.3")
	require.NoError(t, err)
	nodes := []NodeFacts{{Name: "server-1", KubeletVersion: "v1.33.5+k3s1"}}
	problems := plan.KubernetesProblems(nodes)
	require.Len(t, problems, 1)
	assert.Contains(t, problems[0], "Longhorn v1.11.3 needs v1.34")

	nodes[0].KubeletVersion = "v1.24.9+k3s1"
	assert.Contains(t, plan.KubernetesProblems(nodes)[0], "Longhorn v1.8.2 needs v1.25")
}

// A volume Longhorn cannot move while attached keeps its engine across hops,
// so a hop whose manager would refuse that engine must stop and name it.
func TestLonghornHopRefusesToStrandAVolumeOnAnEngineTheNextReleaseRefuses(t *testing.T) {
	s := byDesignSnapshot()
	s.Volumes[0].Image, s.Volumes[0].CurrentImage = engine173, engine173
	s.EngineImages = append(s.EngineImages, lhEngineImage{Image: engine173, State: "deployed", CLIAPI: 7, Controller: 5})
	release, _ := longhornReleaseFor("v1.9.2")

	problems := strings.Join(s.hopProblems(release), "\n")

	assert.Contains(t, problems, "shop/data-db-0 runs the engine image "+engine173)
	assert.Contains(t, problems, "Longhorn v1.9.2 accepts")
	assert.Contains(t, problems, "Detach it")
}
