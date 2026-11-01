package installer

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/getkipper/kipper/controller/pkg/backupscope"
)

// veleroCluster models what a repository key reset reads and changes: the
// backup location, Velero's records, the bucket, the key Secret and the Velero
// Deployment.
type veleroCluster struct {
	bsl          string // Encodes bucket|s3Url|region|prefix, or is empty without a location.
	backups      map[string]string
	restores     map[string]string
	repos        int
	schedules    []string
	unscoped     []string // Schedules whose template predates the transient-resource exclusions.
	patchIgnored bool     // Simulates a successful patch response without applying the patch.
	custom       []string
	helmRaced    bool     // A chart apply brings Velero back while kip waits for the writers.
	chartBusy    bool     // A chart change waits for the Helm controller, whose last Job succeeded.
	mirrors      []string // Each entry encodes name|source|target|retention|maxDelete.
	replicas     int
	veleroPods   int
	nodeAgent    bool
	stoppedBy    string

	objects           int
	undeletable       int
	cleanupFails      bool
	failsDeleting     bool
	locations         int // Backup locations besides the default one.
	oldCleanupPods    int
	oldCleanupStuck   bool
	agentsStuck       bool
	applyLost         bool // The cleanup Job is created, but kubectl apply reports an error.
	logsLost          bool
	replacedPod       bool
	teardownFails     bool
	podSurvives       bool
	podGone           bool
	applied           bool
	tornDown          bool
	versionsHidden    bool
	uploadsHidden     bool
	key               string
	backupPhase       string
	volumeWork        map[string]string
	volumeWorkPolls   int    // Polls until the node agents finish them, or -1 for never.
	chartInput        string // A HelmChart spec field kip does not write, as JSON.
	chartConfig       bool
	locationProblem   string
	locationReadsFail bool
	locationReads     int

	cmds   []string
	stdins []string
	keys   int
	events []string
}

func resettableCluster() *veleroCluster {
	return &veleroCluster{
		bsl:        "backups|https://objects.example.com|hel1|",
		backups:    map[string]string{"daily-apps-20261031": "Completed", "weekly-system-20261026": "Completed"},
		restores:   map[string]string{"drill-1": "Completed"},
		repos:      3,
		schedules:  []string{"daily-apps", "weekly-system"},
		replicas:   1,
		veleroPods: 1,
		nodeAgent:  true,
		objects:    120,
		key:        "old-repository-key",
	}
}

func (c *veleroCluster) Run(cmd string) (string, error) {
	c.cmds = append(c.cmds, cmd)
	switch {
	case strings.Contains(cmd, "get crd backupstoragelocations.velero.io"):
		if c.bsl == "" {
			return "", nil
		}
		return "customresourcedefinition.apiextensions.k8s.io/backupstoragelocations.velero.io", nil
	case strings.Contains(cmd, "get backupstoragelocation default") && strings.Contains(cmd, "status.phase"):
		c.locationReads++
		if c.locationReadsFail && c.locationReads > 1 {
			// Even plausible status output must be ignored when the read fails.
			return "Available|transport diagnostic", errors.New("connection lost")
		}
		if c.locationProblem != "" {
			return "Unavailable|" + c.locationProblem, nil
		}
		return "Available|", nil
	case strings.Contains(cmd, "get backupstoragelocation default"):
		if c.bsl == "" {
			return "", nil
		}
		parts := strings.Split(c.bsl, "|")
		return parts[0] + "|" + parts[1] + "||" + parts[2] + "|" + parts[3], nil
	case strings.Contains(cmd, "get backups.velero.io") && strings.Contains(cmd, "range .items"):
		return phaseLines(c.backups), nil
	case strings.Contains(cmd, "get restores.velero.io") && strings.Contains(cmd, "range .items"):
		return phaseLines(c.restores), nil
	case strings.Contains(cmd, "get backuprepositories.velero.io"):
		return strings.Repeat("backuprepository.velero.io/repo\n", c.repos), nil
	case strings.Contains(cmd, "get cronjobs") && strings.Contains(cmd, "kipper.run/backup-mirror"):
		return strings.Join(c.mirrors, "\n"), nil
	case strings.Contains(cmd, "get deploy velero") && strings.Contains(cmd, "stopped-by}"):
		return fmt.Sprintf("%s|%d", c.stoppedBy, c.replicas), nil
	case strings.Contains(cmd, "get deploy velero") && strings.Contains(cmd, "spec.replicas"):
		return fmt.Sprint(c.replicas), nil
	case strings.Contains(cmd, "get pods -l name=velero") && strings.Contains(cmd, "metadata.name"):
		if c.veleroPods == 0 {
			return "", nil
		}
		return "velero-abc", nil
	case strings.Contains(cmd, "get pods -l name=velero"):
		return strings.Repeat("pod/velero-abc\n", c.veleroPods), nil
	case strings.Contains(cmd, "annotate deploy/velero kipper.run/stopped-by-"):
		c.stoppedBy = ""
	case strings.Contains(cmd, "annotate deploy/velero kipper.run/stopped-by="):
		c.stoppedBy = strings.Fields(cmd[strings.Index(cmd, "stopped-by=")+len("stopped-by="):])[0]
		c.events = append(c.events, "marked")
	case strings.Contains(cmd, "scale deploy/velero --replicas="):
		var n int
		_, _ = fmt.Sscanf(cmd[strings.Index(cmd, "--replicas=")+len("--replicas="):], "%d", &n)
		c.replicas, c.veleroPods = n, n
		c.events = append(c.events, fmt.Sprintf("velero=%d", n))
	case strings.Contains(cmd, "get backupstoragelocations.velero.io -o name"):
		return strings.Repeat("backupstoragelocation.velero.io/x\n", c.locations+1), nil
	case strings.Contains(cmd, "delete job/"+bucketCleanupJob+"-"):
		if c.teardownFails {
			return "", fmt.Errorf("connection reset by peer")
		}
		c.tornDown = true
		c.events = append(c.events, "cleanup stopped")
	case strings.Contains(cmd, "delete jobs -l kipper.run/bucket-cleanup"):
		if c.oldCleanupStuck {
			return "", fmt.Errorf("timed out waiting for the condition")
		}
		c.events = append(c.events, "old cleanup removed")
	case strings.Contains(cmd, "get pods -l kipper.run/bucket-cleanup"):
		if c.tornDown && !c.podSurvives {
			return "", nil
		}
		if c.applied {
			if c.podGone {
				return "", nil
			}
			if c.replacedPod {
				return "pod/kipper-bucket-cleanup-a\npod/kipper-bucket-cleanup-b\n", nil
			}
			return "pod/kipper-bucket-cleanup-a\n", nil
		}
		return strings.Repeat("pod/kipper-bucket-cleanup-old\n", c.oldCleanupPods), nil
	case strings.Contains(cmd, "rollout status ds/node-agent") && c.agentsStuck:
		return "", fmt.Errorf("timed out waiting for the condition")
	case strings.Contains(cmd, "kubectl apply -f -") && strings.Contains(cmd, "name: "+bucketCleanupJob):
		c.events = append(c.events, "cleanup")
		c.applied = true
		c.objects = c.undeletable
		if c.applyLost {
			return "", fmt.Errorf("connection reset by peer")
		}
	case strings.Contains(cmd, "get job "+bucketCleanupJob):
		if c.cleanupFails {
			return "/1", nil
		}
		return "1/", nil
	case strings.Contains(cmd, "logs job/"+bucketCleanupJob):
		if c.cleanupFails || c.replacedPod {
			return "mc: <ERROR> Unable to initialize new alias from the provided credentials. Access Denied.\n" + bucketUntouchedMarker + "\n", nil
		}
		if c.logsLost {
			return "", nil
		}
		if c.failsDeleting {
			return bucketDeletingMarker + "\nmc: <ERROR> Unable to list folder. Access Denied.\n", nil
		}
		checked := "yes"
		if c.versionsHidden {
			checked = "no"
		}
		uploads := "yes"
		if c.uploadsHidden {
			uploads = "no"
		}
		return fmt.Sprintf("%s\n%s %s\n%s %s\n%s %d\n", bucketDeletingMarker, bucketVersionsMarker, checked, bucketUploadsMarker, uploads, bucketObjectsLeftMarker, c.objects), nil
	case strings.Contains(cmd, "helmchartconfig velero"):
		spec := `{"chart":"velero","createNamespace":true,"driver":"secret","failurePolicy":"reinstall","repo":"r","targetNamespace":"velero","valuesContent":"x","version":"12.0.0"`
		if c.chartInput != "" {
			spec += "," + c.chartInput
		}
		out := spec + "}\n"
		if c.chartConfig {
			out += "helmchartconfig.helm.cattle.io/velero\n"
		}
		return out, nil
	case strings.Contains(cmd, "chart-values-velero"):
		if c.chartBusy {
			return "values values 1|12.1.0|r|velero|SHA256=H 1|12.0.0|r|velero|SHA256=H [\"install\"] |HelmChartValuesContent,", nil
		}
		return "values values 1|12.0.0|r|velero|SHA256=H 1|12.0.0|r|velero|SHA256=H [\"install\"] |HelmChartValuesContent,", nil
	case strings.Contains(cmd, "delete jobs -l velero.io/repo-name"):
		if c.helmRaced {
			c.replicas, c.veleroPods = 1, 1
		}
		c.events = append(c.events, "maintenance stopped")
	case strings.Contains(cmd, "get podvolumebackups.velero.io") && strings.Contains(cmd, "range .items"):
		if c.volumeWorkPolls == 0 {
			for name := range c.volumeWork {
				c.volumeWork[name] = "Completed"
			}
		} else if c.volumeWorkPolls > 0 {
			c.volumeWorkPolls--
		}
		c.events = append(c.events, "volume work "+phaseLines(c.volumeWork))
		return phaseLines(c.volumeWork), nil
	case strings.Contains(cmd, "delete backups.velero.io"):
		c.backups = map[string]string{}
		c.events = append(c.events, "records")
	case strings.Contains(cmd, "delete restores.velero.io"):
		c.restores = map[string]string{}
	case strings.Contains(cmd, "delete backuprepositories.velero.io"):
		c.repos = 0
	case strings.Contains(cmd, "delete secret "+repositoryKeySecret):
		c.key = ""
		c.events = append(c.events, "key deleted")
	case strings.Contains(cmd, "/dev/urandom") && strings.Contains(cmd, repositoryKeySecret):
		c.key = fmt.Sprintf("new-repository-key-%d", c.keys)
		c.keys++
		c.events = append(c.events, "key created")
	case strings.Contains(cmd, "get ds node-agent"):
		if c.nodeAgent {
			return "daemonset.apps/node-agent", nil
		}
		return "", nil
	case strings.Contains(cmd, "patch schedules.velero.io/"):
		name := strings.Trim(strings.Fields(cmd[strings.Index(cmd, "schedules.velero.io/")+len("schedules.velero.io/"):])[0], "'")
		for _, resource := range append(backupscope.TransientResources(), backupscope.BackedUpClusterResources()...) {
			if !strings.Contains(cmd, `"`+resource+`"`) {
				return "", fmt.Errorf("patch of %s misses %s", name, resource)
			}
		}
		// Velero refuses a backup that mixes the older resource filters with the scoped ones.
		if !strings.Contains(cmd, `"excludedResources":null`) {
			return "", fmt.Errorf("patch of %s keeps the older excludedResources filter", name)
		}
		if !c.patchIgnored {
			c.unscoped = slices.DeleteFunc(c.unscoped, func(s string) bool { return s == name })
		}
		c.events = append(c.events, "scoped "+name)
	case strings.Contains(cmd, "rollout restart ds/node-agent"):
		c.events = append(c.events, "node-agent restarted")
	case strings.Contains(cmd, "get schedules.velero.io") && strings.Contains(cmd, "excludedNamespaceScopedResources"):
		var lines []string
		for _, name := range c.schedules {
			scope := `["` + strings.Join(backupscope.TransientResources(), `","`) + `"]`
			if slices.Contains(c.unscoped, name) {
				scope = `["certificaterequests.cert-manager.io"]`
			}
			lines = append(lines, name+"="+scope)
		}
		if !strings.Contains(cmd, "-l app.kubernetes.io/instance=velero,app.kubernetes.io/managed-by=Helm") {
			for _, name := range c.custom {
				lines = append(lines, name+`=["certificaterequests.cert-manager.io"]`)
			}
		}
		return strings.Join(lines, "\n"), nil
	case strings.Contains(cmd, "get schedules.velero.io"):
		return strings.Join(c.schedules, "\n"), nil
	case strings.Contains(cmd, "backup create"):
		_, rest, _ := strings.Cut(cmd, "backup create ")
		fields := strings.Fields(rest)
		c.backups[fields[0]] = "New"
		c.events = append(c.events, "backup "+fields[0])
	case strings.Contains(cmd, "get backups.velero.io/"):
		name := strings.Fields(cmd[strings.Index(cmd, "get backups.velero.io/")+len("get backups.velero.io/"):])[0]
		if c.backupPhase != "" {
			return c.backupPhase, nil
		}
		if _, ok := c.backups[name]; ok {
			return "Completed", nil
		}
		return "", nil
	}
	return "", nil
}

func (c *veleroCluster) RunStdin(cmd string, in io.Reader) (string, error) {
	b, _ := io.ReadAll(in)
	c.stdins = append(c.stdins, string(b))
	return c.Run(cmd)
}

func phaseLines(items map[string]string) string {
	var lines []string
	for name, phase := range items {
		lines = append(lines, name+"="+phase)
	}
	slices.Sort(lines)
	return strings.Join(lines, "\n")
}

func (c *veleroCluster) changed() []string {
	var changes []string
	for _, cmd := range c.cmds {
		for _, verb := range []string{"scale ", "delete ", " create", "apply -f", "patch ", "rollout restart"} {
			if strings.Contains(cmd, verb) {
				changes = append(changes, cmd)
				break
			}
		}
	}
	return changes
}

func fastReset(t *testing.T) {
	t.Helper()
	poll, stop, writers, backups := resetPollInterval, veleroStopTimeout, writerStopTimeout, resetBackupTimeout
	resetPollInterval, veleroStopTimeout, writerStopTimeout, resetBackupTimeout = 0, 0, 0, 0
	t.Cleanup(func() {
		resetPollInterval, veleroStopTimeout, writerStopTimeout, resetBackupTimeout = poll, stop, writers, backups
	})
}

func TestRepositoryKeyResetSurveyChangesNothing(t *testing.T) {
	c := resettableCluster()
	c.mirrors = []string{
		"offsite|s3://objects.example.com/backups|s3://vault.example.net/backups-locked|30|500",
		"uploads|s3://objects.example.com/app-uploads|s3://vault.example.net/uploads|14|off",
	}

	reset, err := SurveyRepositoryKeyReset(c)

	require.NoError(t, err)
	assert.Equal(t, "backups", reset.Bucket)
	assert.Equal(t, 2, reset.Backups)
	assert.Equal(t, 3, reset.Repositories)
	require.Len(t, reset.Mirrors, 1, "only a mirror of the backup bucket keeps copies of these backups")
	assert.Equal(t, BackupMirror{Name: "offsite", Target: "s3://vault.example.net/backups-locked", RetentionDays: "30", MaxDelete: "500"}, reset.Mirrors[0])
	assert.Empty(t, c.changed())
}

func TestRepositoryKeyResetNeedsBackups(t *testing.T) {
	c := resettableCluster()
	c.bsl = ""

	_, err := SurveyRepositoryKeyReset(c)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "not configured")
}

func TestRepositoryKeyResetWaitsForRunningBackupsAndRestores(t *testing.T) {
	for _, tc := range []struct {
		name   string
		modify func(*veleroCluster)
		want   string
	}{
		{"backup in progress", func(c *veleroCluster) { c.backups["daily-apps-20261101"] = "InProgress" }, "daily-apps-20261101"},
		{"new backup", func(c *veleroCluster) { c.backups["manual"] = "" }, "manual"},
		{"backup finalizing", func(c *veleroCluster) { c.backups["weekly"] = "Finalizing" }, "weekly"},
		{"restore in progress", func(c *veleroCluster) { c.restores["drill-2"] = "InProgress" }, "drill-2"},
		{"backup queued", func(c *veleroCluster) { c.backups["queued"] = "Queued" }, "queued"},
		{"backup ready to start", func(c *veleroCluster) { c.backups["ready"] = "ReadyToStart" }, "ready"},
		{"backup being deleted", func(c *veleroCluster) { c.backups["old"] = "Deleting" }, "old"},
		{"phase kip does not know", func(c *veleroCluster) { c.backups["future"] = "SomethingNew" }, "future"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := resettableCluster()
			tc.modify(c)

			_, err := SurveyRepositoryKeyReset(c)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			assert.Empty(t, c.changed())
		})
	}
}

func TestRepositoryKeyResetRefusesALocationWithAPrefix(t *testing.T) {
	c := resettableCluster()
	c.bsl = "backups|https://objects.example.com|hel1|cluster-a"

	_, err := SurveyRepositoryKeyReset(c)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "cluster-a")
}

func TestRepositoryKeyResetReplacesTheKeyAndBacksUpAgain(t *testing.T) {
	fastReset(t)
	c := resettableCluster()
	reset, err := SurveyRepositoryKeyReset(c)
	require.NoError(t, err)

	results, err := reset.Run(func(string) {})

	require.NoError(t, err)
	assert.Equal(t, []string{"marked", "velero=0", "maintenance stopped", "volume work ", "old cleanup removed", "cleanup", "cleanup stopped", "records", "key deleted", "key created", "node-agent restarted", "velero=1"}, c.events[:12],
		"Velero stays stopped from before the bucket is emptied until the new key exists and the node agents dropped the old one")
	assert.False(t, reset.VersionsUnchecked)
	assert.False(t, reset.UploadsUnchecked)
	assert.Equal(t, "new-repository-key-0", c.key)
	assert.Empty(t, c.restores)
	assert.Zero(t, c.repos)
	require.Len(t, results, 2)
	for i, schedule := range c.schedules {
		assert.True(t, strings.HasPrefix(results[i].Name, schedule+"-"), results[i].Name)
		assert.Equal(t, "Completed", results[i].Phase)
	}
	for _, in := range c.stdins {
		assert.NotContains(t, in, "repository-key", "the new key is made on the server and never crosses SSH")
	}
	for _, cmd := range c.cmds {
		if strings.Contains(cmd, "backup create") {
			assert.Contains(t, cmd, "exec velero-abc -c velero", "the Deployment's selector also matches the node agents, so only the pod's own label finds the server")
		}
	}
}

func TestRepositoryKeyResetStopsWhileTheBucketStillHoldsBackups(t *testing.T) {
	fastReset(t)
	c := resettableCluster()
	c.undeletable = 7
	reset, err := SurveyRepositoryKeyReset(c)
	require.NoError(t, err)

	_, err = reset.Run(func(string) {})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "7 objects")
	assert.Contains(t, err.Error(), "Velero stays stopped")
	assert.NotContains(t, err.Error(), "again to finish", "a rerun meets the same refusal until the store lets the objects go")
	assert.Equal(t, "old-repository-key", c.key, "a new key over old repository data would leave both unreadable")
	assert.Zero(t, c.replicas)
}

func TestRepositoryKeyResetStopsWhenTheCleanupFails(t *testing.T) {
	fastReset(t)
	c := resettableCluster()
	c.failsDeleting = true
	reset, err := SurveyRepositoryKeyReset(c)
	require.NoError(t, err)

	_, err = reset.Run(func(string) {})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "Access Denied")
	assert.Contains(t, err.Error(), "Velero is stopped")
	assert.Equal(t, "old-repository-key", c.key)
	assert.Zero(t, c.replicas, "part of the old backups is gone, so Velero must not use them")
}

func TestRepositoryKeyResetStartsVeleroAgainWhenNothingWasDeleted(t *testing.T) {
	for _, tc := range []struct {
		name   string
		modify func(*veleroCluster)
		want   string
	}{
		{"cleanup fails before deleting", func(c *veleroCluster) { c.cleanupFails = true }, "Access Denied"},
		{"node agents do not finish", func(c *veleroCluster) {
			c.volumeWork = map[string]string{"daily-apps-20261101-x7k2p": "InProgress"}
			c.volumeWorkPolls = -1
		}, "daily-apps-20261101-x7k2p"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fastReset(t)
			c := resettableCluster()
			tc.modify(c)
			reset, err := SurveyRepositoryKeyReset(c)
			require.NoError(t, err)

			_, err = reset.Run(func(string) {})

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			assert.Contains(t, err.Error(), "Nothing was deleted")
			assert.Equal(t, 1, c.replicas, "Velero runs again with the old key")
			assert.Equal(t, "old-repository-key", c.key)
			assert.NotContains(t, c.events, "records")
		})
	}
}

func TestRepositoryKeyResetKeepsVeleroStoppedWhenACleanupMayHaveDeleted(t *testing.T) {
	for _, tc := range []struct {
		name   string
		modify func(*veleroCluster)
		want   string
	}{
		{"apply reports an error after creating the Job", func(c *veleroCluster) { c.applyLost = true }, "connection reset"},
		{"script ends without a marker", func(c *veleroCluster) { c.logsLost = true }, "emptying bucket"},
		{"Job replaced an evicted pod", func(c *veleroCluster) { c.replacedPod = true }, "Access Denied"},
		{"Job cannot be stopped after it emptied the bucket", func(c *veleroCluster) { c.teardownFails = true }, "stopping the bucket cleanup"},
		{"pod gone before kip counts it", func(c *veleroCluster) { c.cleanupFails = true; c.podGone = true }, "Access Denied"},
		{"a pod outlives its Job", func(c *veleroCluster) { c.podSurvives = true }, "stopping the bucket cleanup"},
		{"earlier cleanup still stopping", func(c *veleroCluster) { c.oldCleanupPods = 1 }, "earlier bucket cleanup"},
		{"earlier cleanup cannot be removed", func(c *veleroCluster) { c.oldCleanupStuck = true }, "timed out"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fastReset(t)
			c := resettableCluster()
			tc.modify(c)
			reset, err := SurveyRepositoryKeyReset(c)
			require.NoError(t, err)

			_, err = reset.Run(func(string) {})

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			assert.NotContains(t, err.Error(), "Nothing was deleted")
			assert.Contains(t, err.Error(), "Velero is stopped")
			assert.Zero(t, c.replicas, "a partly emptied repository is not safe to resume")
			assert.Equal(t, "old-repository-key", c.key)
		})
	}
}

func TestRepositoryKeyResetLeavesAVeleroItFoundStoppedStopped(t *testing.T) {
	fastReset(t)
	c := resettableCluster()
	c.replicas, c.veleroPods = 0, 0 // An earlier run may have emptied part of the bucket.
	c.cleanupFails = true
	reset, err := SurveyRepositoryKeyReset(c)
	require.NoError(t, err)

	_, err = reset.Run(func(string) {})

	require.Error(t, err)
	assert.Zero(t, c.replicas)
	assert.Contains(t, err.Error(), "cannot tell whether an earlier run deleted")
}

func TestRepositoryKeyResetRefusesAClusterWithMoreBackupLocations(t *testing.T) {
	c := resettableCluster()
	c.locations = 1

	_, err := SurveyRepositoryKeyReset(c)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "2 backup locations")
	assert.Empty(t, c.changed())
}

func TestRepositoryKeyResetRemovesEarlierCleanupJobsFirst(t *testing.T) {
	fastReset(t)
	c := resettableCluster()
	reset, err := SurveyRepositoryKeyReset(c)
	require.NoError(t, err)

	_, err = reset.Run(func(string) {})

	require.NoError(t, err)
	assert.Less(t, slices.Index(c.events, "old cleanup removed"), slices.Index(c.events, "cleanup"))
	var applied []string
	for _, cmd := range c.cmds {
		if strings.Contains(cmd, "kubectl apply -f -") {
			_, after, _ := strings.Cut(cmd, "name: ")
			applied = append(applied, strings.Fields(after)[0])
		}
	}
	require.Len(t, applied, 1)
	assert.NotEqual(t, bucketCleanupJob, applied[0], "each run reads the result of its own Job only")
}

func TestRepositoryKeyResetNamesFenceWhenTheNodeAgentsDoNotRestart(t *testing.T) {
	fastReset(t)
	c := resettableCluster()
	c.agentsStuck = true
	reset, err := SurveyRepositoryKeyReset(c)
	require.NoError(t, err)

	_, err = reset.Run(func(string) {})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "kip node fence")
}

func TestRepositoryKeyResetFindsVeleroBeforeStoppingIt(t *testing.T) {
	fastReset(t)
	c := resettableCluster()
	c.veleroPods = 0

	reset, err := SurveyRepositoryKeyReset(c)
	if err == nil {
		_, err = reset.Run(func(string) {})
	}

	require.Error(t, err, "without its pod in sight, kip cannot tell that Velero stopped")
	assert.Empty(t, c.changed())
}

func TestRepositoryKeyResetFinishesARunThatStoppedPartway(t *testing.T) {
	fastReset(t)
	c := resettableCluster()
	c.replicas, c.veleroPods, c.key = 0, 0, ""
	c.backups["daily-apps-20261101"] = "InProgress" // Velero stopped before it could finish this one.

	reset, err := SurveyRepositoryKeyReset(c)
	require.NoError(t, err)
	_, err = reset.Run(func(string) {})

	require.NoError(t, err)
	assert.Equal(t, 1, c.replicas, "a stopped Velero comes back with one replica")
	assert.Equal(t, "new-repository-key-0", c.key)
}

func TestRepositoryKeyResetReportsAFailedBackup(t *testing.T) {
	fastReset(t)
	c := resettableCluster()
	c.backupPhase = "PartiallyFailed"
	reset, err := SurveyRepositoryKeyReset(c)
	require.NoError(t, err)

	results, err := reset.Run(func(string) {})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "PartiallyFailed")
	assert.Equal(t, "new-repository-key-0", c.key, "the key was reset even though a backup failed")
	require.Len(t, results, 2)
}

func TestBucketCleanupJob(t *testing.T) {
	for _, tc := range []struct {
		name, bsl, endpoint string
	}{
		{"S3-compatible", "backups|https://objects.example.com|hel1|", "https://objects.example.com"},
		{"AWS", "backups||eu-central-1|", "https://s3.eu-central-1.amazonaws.com"},
		{"in-cluster MinIO", "velero|" + inClusterMinIOURL + "|minio|", inClusterMinIOURL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fastReset(t)
			c := resettableCluster()
			c.bsl = tc.bsl
			reset, err := SurveyRepositoryKeyReset(c)
			require.NoError(t, err)
			_, err = reset.Run(func(string) {})
			require.NoError(t, err)

			var manifest string
			for _, cmd := range c.cmds {
				if strings.Contains(cmd, "name: "+bucketCleanupJob) {
					manifest = cmd
				}
			}
			assert.Contains(t, manifest, "secretName: cloud-credentials", "the Job reads the credentials Velero already has")
			var job struct {
				Spec struct {
					Template struct {
						Spec struct {
							Containers []struct {
								Command []string `yaml:"command"`
							} `yaml:"containers"`
						} `yaml:"spec"`
					} `yaml:"template"`
				} `yaml:"spec"`
			}
			_, body, _ := strings.Cut(manifest, "\n")
			body, _, _ = strings.Cut(body, "KIPEOF")
			require.NoError(t, yaml.Unmarshal([]byte(body), &job))
			require.Len(t, job.Spec.Template.Spec.Containers, 1)
			assert.Equal(t, []string{"sh", "-c", bucketCleanupScript}, job.Spec.Template.Spec.Containers[0].Command,
				"the script reaches the shell line by line")
			assert.Contains(t, manifest, fmt.Sprintf("value: %q", tc.endpoint))
			assert.Contains(t, manifest, "--versions")
			for _, prefix := range []string{"backups", "restores", "kopia"} {
				assert.Contains(t, manifest, prefix)
			}
		})
	}
}

func TestRepositoryKeyResetRefusesWorkThatStartedAfterTheSurvey(t *testing.T) {
	fastReset(t)
	c := resettableCluster()
	reset, err := SurveyRepositoryKeyReset(c)
	require.NoError(t, err)
	c.backups["daily-apps-20261101"] = "New"

	_, err = reset.Run(func(string) {})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "daily-apps-20261101")
	assert.Empty(t, c.changed())
}

func TestRepositoryKeyResetWaitsForTheNodeAgents(t *testing.T) {
	fastReset(t)
	writerStopTimeout = time.Hour
	c := resettableCluster()
	c.volumeWork = map[string]string{"daily-apps-20261101-x7k2p": "InProgress"}
	c.volumeWorkPolls = 2
	reset, err := SurveyRepositoryKeyReset(c)
	require.NoError(t, err)

	_, err = reset.Run(func(string) {})

	require.NoError(t, err)
	cleanup := slices.Index(c.events, "cleanup")
	finished := slices.Index(c.events, "volume work daily-apps-20261101-x7k2p=Completed")
	require.NotEqual(t, -1, finished)
	assert.Less(t, finished, cleanup, "a node agent still uploading would write into the emptied bucket")
}

func TestRepositoryKeyResetStopsWhenTheNodeAgentsDoNotFinish(t *testing.T) {
	fastReset(t)
	c := resettableCluster()
	c.volumeWork = map[string]string{"daily-apps-20261101-x7k2p": "InProgress"}
	c.volumeWorkPolls = -1
	reset, err := SurveyRepositoryKeyReset(c)
	require.NoError(t, err)

	_, err = reset.Run(func(string) {})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "daily-apps-20261101-x7k2p")
	assert.NotContains(t, c.events, "cleanup")
	assert.Equal(t, "old-repository-key", c.key)
}

func TestRepositoryKeyResetSaysWhenVersionsWentUnchecked(t *testing.T) {
	fastReset(t)
	c := resettableCluster()
	c.versionsHidden = true
	reset, err := SurveyRepositoryKeyReset(c)
	require.NoError(t, err)

	_, err = reset.Run(func(string) {})

	require.NoError(t, err)
	assert.True(t, reset.VersionsUnchecked)
	assert.False(t, reset.UploadsUnchecked)
}

func TestRepositoryKeyResetSaysWhenIncompleteUploadsWentUnchecked(t *testing.T) {
	fastReset(t)
	c := resettableCluster()
	c.uploadsHidden = true
	reset, err := SurveyRepositoryKeyReset(c)
	require.NoError(t, err)

	_, err = reset.Run(func(string) {})

	require.NoError(t, err)
	assert.True(t, reset.UploadsUnchecked)
	assert.False(t, reset.VersionsUnchecked)
}

func TestRepositoryKeyResetFailsWhenBackupsOutlastTheWait(t *testing.T) {
	fastReset(t)
	c := resettableCluster()
	c.backupPhase = "InProgress"
	reset, err := SurveyRepositoryKeyReset(c)
	require.NoError(t, err)

	results, err := reset.Run(func(string) {})

	require.Error(t, err, "nothing confirms that a new backup exists")
	assert.Contains(t, err.Error(), "InProgress")
	require.Len(t, results, 2)
}

func TestRepositoryKeyResetFindsAnAWSMirrorByAnyRegionalHost(t *testing.T) {
	c := resettableCluster()
	c.bsl = "backups||eu-central-1|"
	c.mirrors = []string{"offsite|s3://s3.amazonaws.com/backups|s3://vault.example.net/backups-locked|30|500"}

	reset, err := SurveyRepositoryKeyReset(c)

	require.NoError(t, err)
	require.Len(t, reset.Mirrors, 1, "every AWS S3 host reaches the same bucket")
}

func TestRepositoryKeyResetSaysARunThatDeletedCanBeFinished(t *testing.T) {
	fastReset(t)
	c := resettableCluster()
	c.replicas, c.veleroPods = 0, 0
	c.failsDeleting = true
	reset, err := SurveyRepositoryKeyReset(c)
	require.NoError(t, err)

	_, err = reset.Run(func(string) {})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "again to finish")
	assert.NotContains(t, err.Error(), "earlier run", "this run did the deleting")
}

func TestRepositoryKeyResetKeepsScriptMarkersOutOfErrors(t *testing.T) {
	fastReset(t)
	for _, modify := range []func(*veleroCluster){
		func(c *veleroCluster) { c.cleanupFails = true },
		func(c *veleroCluster) { c.failsDeleting = true },
	} {
		c := resettableCluster()
		modify(c)
		reset, err := SurveyRepositoryKeyReset(c)
		require.NoError(t, err)

		_, err = reset.Run(func(string) {})

		require.Error(t, err)
		assert.NotContains(t, err.Error(), "KIPPER_")
	}
}

func TestRepositoryKeyResetRefusesASchedulePredatingTheDexExclusions(t *testing.T) {
	c := resettableCluster()
	c.unscoped = []string{"weekly-system"}

	_, err := SurveyRepositoryKeyReset(c)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "weekly-system")
	assert.Contains(t, err.Error(), "kip upgrade")
	assert.Empty(t, c.changed())
}

func TestRepositoryKeyResetFinishesAnUnfinishedResetWhoseSchedulesPredateTheDexExclusions(t *testing.T) {
	fastReset(t)
	c := resettableCluster()
	c.unscoped = []string{"weekly-system"}
	c.replicas, c.veleroPods, c.stoppedBy = 0, 0, veleroStoppedByReset // kip upgrade skips Velero until the reset finishes.

	reset, err := SurveyRepositoryKeyReset(c)
	require.NoError(t, err)
	assert.Equal(t, "weekly-system", reset.OutdatedSchedule)

	backups, err := reset.Run(func(string) {})

	require.NoError(t, err)
	assert.Empty(t, backups, "kip upgrade has not updated the chart yet, so the reset leaves new backups to the operator")
	assert.NotEqual(t, "old-repository-key", c.key)
	for _, e := range c.events {
		assert.NotContains(t, e, "backup ")
	}
	scoped, started := slices.Index(c.events, "scoped weekly-system"), slices.Index(c.events, "velero=1")
	require.NotEqual(t, -1, scoped, "Velero's scheduler would back up Dex's signing keys and sessions from the outdated schedule")
	require.NotEqual(t, -1, started)
	assert.Less(t, scoped, started, "the schedule must exclude Dex's keys before Velero's scheduler runs")
	assert.Empty(t, c.unscoped)
}

func TestRepositoryKeyResetKeepsVeleroStoppedWhenAScheduleStaysOutdated(t *testing.T) {
	fastReset(t)
	c := resettableCluster()
	c.unscoped = []string{"weekly-system"}
	c.replicas, c.veleroPods, c.stoppedBy = 0, 0, veleroStoppedByReset
	c.patchIgnored = true

	reset, err := SurveyRepositoryKeyReset(c)
	require.NoError(t, err)

	_, err = reset.Run(func(string) {})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "weekly-system")
	assert.Contains(t, err.Error(), "Velero is stopped")
	assert.Equal(t, 0, c.replicas, "an unscoped schedule must not run")
	assert.NotContains(t, c.events, "velero=1")
}

func TestRepositoryKeyResetStopsWhenTheLocationChangedSinceTheSurvey(t *testing.T) {
	for _, tc := range []struct {
		name   string
		modify func(*veleroCluster)
		want   string
	}{
		{"another bucket", func(c *veleroCluster) { c.bsl = "fresh|https://objects.example.com|hel1|" }, "fresh"},
		{"another store", func(c *veleroCluster) { c.bsl = "backups|https://vault.example.net|hel1|" }, "vault.example.net"},
		{"another location", func(c *veleroCluster) { c.locations = 1 }, "2 backup locations"},
		{"a prefix", func(c *veleroCluster) { c.bsl = "backups|https://objects.example.com|hel1|cluster-a" }, "prefix"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fastReset(t)
			c := resettableCluster()
			reset, err := SurveyRepositoryKeyReset(c)
			require.NoError(t, err)
			tc.modify(c)

			_, err = reset.Run(func(string) {})

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			assert.Contains(t, err.Error(), "Nothing was deleted")
			assert.NotContains(t, err.Error(), "Nothing was changed", "kip stopped and started Velero by then")
			assert.False(t, c.applied, "the bucket the operator confirmed is no longer the one Velero uses")
			assert.Equal(t, 1, c.replicas)
			assert.Equal(t, "old-repository-key", c.key)
		})
	}
}

func TestRepositoryKeyResetMarksTheVeleroItLeavesStopped(t *testing.T) {
	fastReset(t)
	c := resettableCluster()
	c.failsDeleting = true
	reset, err := SurveyRepositoryKeyReset(c)
	require.NoError(t, err)

	_, err = reset.Run(func(string) {})

	require.Error(t, err)
	assert.Equal(t, veleroStoppedByReset, c.stoppedBy, "an install that moves the bucket reads the mark to start Velero again")
	assert.Less(t, slices.Index(c.events, "marked"), slices.Index(c.events, "velero=0"), "the mark is in place before Velero stops")
}

func TestRepositoryKeyResetClearsTheMarkWhenVeleroRunsAgain(t *testing.T) {
	for _, tc := range []struct {
		name   string
		modify func(*veleroCluster)
	}{
		{"reset finished", func(*veleroCluster) {}},
		{"nothing was deleted", func(c *veleroCluster) { c.cleanupFails = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fastReset(t)
			c := resettableCluster()
			tc.modify(c)
			reset, err := SurveyRepositoryKeyReset(c)
			require.NoError(t, err)

			_, _ = reset.Run(func(string) {})

			assert.Equal(t, 1, c.replicas)
			assert.Empty(t, c.stoppedBy)
		})
	}
}

func TestRepositoryKeyResetLeavesSchedulesKipDidNotCreateAlone(t *testing.T) {
	c := resettableCluster()
	c.custom = []string{"app-only"}

	_, err := SurveyRepositoryKeyReset(c)

	require.NoError(t, err, "kip upgrade cannot change such a schedule, and its own runs include the same resources")
}

func TestRepositoryKeyResetStopsWhenAChartApplyStartedVeleroAgain(t *testing.T) {
	fastReset(t)
	c := resettableCluster()
	c.helmRaced = true
	reset, err := SurveyRepositoryKeyReset(c)
	require.NoError(t, err)

	_, err = reset.Run(func(string) {})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "Nothing was deleted")
	assert.False(t, c.applied)
	assert.Equal(t, "old-repository-key", c.key)
}

func TestRepositoryKeyResetWaitsForAChartStillBeingApplied(t *testing.T) {
	fastReset(t)
	c := resettableCluster()
	c.chartBusy = true
	reset, err := SurveyRepositoryKeyReset(c)
	require.NoError(t, err)

	_, err = reset.Run(func(string) {})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "Velero chart")
	assert.Contains(t, err.Error(), "Nothing was deleted")
	assert.False(t, c.applied)
}

func TestRepositoryKeyResetRefusesChartInputsKipDoesNotManage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		modify func(*veleroCluster)
		want   string
	}{
		{"a set override", func(c *veleroCluster) { c.chartInput = `"set":{"configuration.logLevel":"debug"}` }, "spec.set"},
		{"another job image", func(c *veleroCluster) { c.chartInput = `"jobImage":"example.com/helm:1"` }, "spec.jobImage"},
		{"another failure policy", func(c *veleroCluster) { c.chartInput = `"failurePolicy":"abort"` }, "spec.failurePolicy"},
		{"a HelmChartConfig", func(c *veleroCluster) { c.chartConfig = true }, "HelmChartConfig"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := resettableCluster()
			tc.modify(c)

			_, err := SurveyRepositoryKeyReset(c)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			assert.Contains(t, err.Error(), "Nothing was changed")
			assert.Equal(t, 1, c.replicas)
		})
	}
}

func TestRepositoryKeyResetStopsWhenTheChartGainsAnInputAfterTheSurvey(t *testing.T) {
	fastReset(t)
	c := resettableCluster()
	reset, err := SurveyRepositoryKeyReset(c)
	require.NoError(t, err)
	c.chartInput = `"set":{"configuration.logLevel":"debug"}`

	_, err = reset.Run(func(string) {})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "spec.set")
	assert.Contains(t, err.Error(), "Nothing was deleted")
	assert.False(t, c.applied)
	assert.Equal(t, "old-repository-key", c.key)
}

func TestRepositoryKeyResetSaysWhyTheBackupLocationStaysUnavailable(t *testing.T) {
	fastReset(t)
	c := resettableCluster()
	reset, err := SurveyRepositoryKeyReset(c)
	require.NoError(t, err)
	c.locationProblem = "Backup store contains invalid top-level directories: [other]"

	_, err = reset.Run(func(string) {})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "did not become available")
	assert.Contains(t, err.Error(), "invalid top-level directories: [other]")
}

func TestRepositoryKeyResetKeepsVelerosReasonWhenLaterReadsFail(t *testing.T) {
	fastReset(t)
	c := resettableCluster()
	reset, err := SurveyRepositoryKeyReset(c)
	require.NoError(t, err)
	c.locationProblem = "Access Denied."
	c.locationReadsFail = true
	veleroStopTimeout = 50 * time.Millisecond // Allow failed reads after the first successful read.

	_, err = reset.Run(func(string) {})

	require.Error(t, err, "a failed read that prints Available is no proof the location is available")
	assert.Contains(t, err.Error(), "Velero reports: Access Denied. Run", "one period between Velero's text and kip's")
	assert.NotContains(t, err.Error(), "transport diagnostic")
}
