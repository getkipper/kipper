package doctor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

var backupNow = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

func dailySchedule(lastBackup time.Time) BackupSchedule {
	return BackupSchedule{Name: "velero-daily-apps", Cron: "0 3 * * *", Created: backupNow.AddDate(0, -1, 0), LastBackup: lastBackup}
}

func dailyRun(name, phase string, created time.Time) BackupRun {
	return BackupRun{Name: name, Schedule: "velero-daily-apps", Phase: phase, Created: created}
}

func TestCheckBackups(t *testing.T) {
	ran := backupNow.Add(-9 * time.Hour)
	partial := dailyRun("velero-daily-apps-20261021030000", "PartiallyFailed", ran)
	partial.Errors = 2
	for name, tc := range map[string]struct {
		state   BackupState
		status  Status
		message []string
	}{
		"latest scheduled backup completed": {
			state: BackupState{Configured: true, Schedules: []BackupSchedule{dailySchedule(ran)}, Runs: []BackupRun{
				dailyRun("velero-daily-apps-20261020030000", "PartiallyFailed", ran.Add(-24*time.Hour)),
				dailyRun("velero-daily-apps-20261021030000", "Completed", ran),
			}},
			status: Pass,
		},
		"latest partially failed": {
			state: BackupState{Configured: true, Schedules: []BackupSchedule{dailySchedule(ran)}, Runs: []BackupRun{
				dailyRun("velero-daily-apps-20261020030000", "Completed", ran.Add(-24*time.Hour)),
				partial,
			}},
			status:  Warn,
			message: []string{"velero-daily-apps-20261021030000", "partially failed", "2 errors", "kip backup describe velero-daily-apps-20261021030000"},
		},
		"latest failed": {
			state: BackupState{Configured: true, Schedules: []BackupSchedule{dailySchedule(ran)}, Runs: []BackupRun{
				dailyRun("velero-daily-apps-20261021030000", "Failed", ran),
			}},
			status:  Fail,
			message: []string{"velero-daily-apps-20261021030000", "failed"},
		},
		"a running backup does not hide the last finished one": {
			state: BackupState{Configured: true, Schedules: []BackupSchedule{dailySchedule(ran)}, Runs: []BackupRun{
				dailyRun("velero-daily-apps-20261020030000", "Failed", ran.Add(-24*time.Hour)),
				dailyRun("velero-daily-apps-20261021030000", "InProgress", ran),
			}},
			status:  Fail,
			message: []string{"velero-daily-apps-20261020030000"},
		},
		"a daily schedule that stopped running": {
			state: BackupState{Configured: true, Schedules: []BackupSchedule{dailySchedule(backupNow.Add(-72 * time.Hour))}, Runs: []BackupRun{
				dailyRun("velero-daily-apps-20261018030000", "Completed", backupNow.Add(-72*time.Hour)),
			}},
			status:  Warn,
			message: []string{"velero-daily-apps", "has not run since"},
		},
		"a weekly schedule five days after its run": {
			state: BackupState{Configured: true, Schedules: []BackupSchedule{
				{Name: "velero-weekly-system", Cron: "0 4 * * 0", Created: backupNow.AddDate(0, -1, 0), LastBackup: backupNow.Add(-5 * 24 * time.Hour)},
			}, Runs: []BackupRun{
				{Name: "velero-weekly-system-20261016040000", Schedule: "velero-weekly-system", Phase: "Completed", Created: backupNow.Add(-5 * 24 * time.Hour)},
			}},
			status: Pass,
		},
		"a new schedule that has not run yet": {
			state:  BackupState{Configured: true, Schedules: []BackupSchedule{{Name: "velero-daily-apps", Cron: "0 3 * * *", Created: backupNow.Add(-2 * time.Hour)}}},
			status: Pass,
		},
		"an old schedule that never ran": {
			state:   BackupState{Configured: true, Schedules: []BackupSchedule{{Name: "velero-daily-apps", Cron: "0 3 * * *", Created: backupNow.AddDate(0, 0, -5)}}},
			status:  Warn,
			message: []string{"velero-daily-apps", "has never run"},
		},
		"a paused schedule is left out": {
			state:  BackupState{Configured: true, Schedules: []BackupSchedule{{Name: "velero-daily-apps", Cron: "0 3 * * *", Paused: true, Created: backupNow.AddDate(0, 0, -5)}}},
			status: Pass,
		},
		"every schedule paused": {
			state:   BackupState{Configured: true, Schedules: []BackupSchedule{{Name: "velero-daily-apps", Cron: "0 3 * * *", Paused: true, Created: backupNow.AddDate(0, 0, -5)}}},
			status:  Pass,
			message: []string{"no active backup schedules"},
		},
		"a monthly schedule is not judged for staleness": {
			state: BackupState{Configured: true, Schedules: []BackupSchedule{
				{Name: "monthly", Cron: "0 3 1 * *", Created: backupNow.AddDate(0, -3, 0), LastBackup: backupNow.AddDate(0, 0, -20)},
			}},
			status: Pass,
		},
		"a @weekly schedule five days after its run": {
			state: BackupState{Configured: true, Schedules: []BackupSchedule{
				{Name: "weekly", Cron: "@weekly", Created: backupNow.AddDate(0, -1, 0), LastBackup: backupNow.Add(-5 * 24 * time.Hour)},
			}},
			status: Pass,
		},
		"backups not set up": {
			status: Skip,
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := CheckBackups(Snapshot{Backups: tc.state}, backupNow)
			assert.Equal(t, tc.status, r.Status, r.Message)
			for _, m := range tc.message {
				assert.Contains(t, r.Message, m)
			}
		})
	}

	unread := CheckBackups(Snapshot{Missing: map[Part]error{PartBackups: errors.New("forbidden")}}, backupNow)
	assert.Equal(t, Skip, unread.Status)
	assert.Contains(t, unread.Message, "not checked")
}

const backupsAndSchedulesJSON = `{"items":[
 {"kind":"Schedule","metadata":{"name":"velero-daily-apps","creationTimestamp":"2026-09-01T00:00:00Z"},"spec":{"schedule":"0 3 * * *","paused":false},"status":{"lastBackup":"2026-10-21T03:00:00Z"}},
 {"kind":"Backup","metadata":{"name":"velero-daily-apps-20261021030000","creationTimestamp":"2026-10-21T03:00:00Z","labels":{"velero.io/schedule-name":"velero-daily-apps"}},"status":{"phase":"PartiallyFailed","errors":3,"warnings":1}}]}`

func TestFetchOverSSHReadsBackups(t *testing.T) {
	r := (&routedRunner{}).
		on("get crd backups.velero.io", "customresourcedefinition.apiextensions.k8s.io/backups.velero.io\n", nil).
		on("get backups.velero.io,schedules.velero.io", backupsAndSchedulesJSON, nil)

	snap := FetchOverSSH(r)

	require.NotContains(t, snap.Missing, PartBackups)
	assert.True(t, snap.Backups.Configured)
	require.Len(t, snap.Backups.Schedules, 1)
	assert.Equal(t, "0 3 * * *", snap.Backups.Schedules[0].Cron)
	assert.Equal(t, time.Date(2026, 10, 21, 3, 0, 0, 0, time.UTC), snap.Backups.Schedules[0].LastBackup)
	require.Len(t, snap.Backups.Runs, 1)
	assert.Equal(t, BackupRun{Name: "velero-daily-apps-20261021030000", Schedule: "velero-daily-apps", Phase: "PartiallyFailed",
		Created: time.Date(2026, 10, 21, 3, 0, 0, 0, time.UTC), Errors: 3, Warnings: 1}, snap.Backups.Runs[0])
}

func TestFetchOverSSHWithoutVelero(t *testing.T) {
	snap := FetchOverSSH((&routedRunner{}).on("get crd backups.velero.io", "", nil))

	assert.False(t, snap.Backups.Configured)
	assert.NotContains(t, snap.Missing, PartBackups)
}

func TestReadBackupsViaAPI(t *testing.T) {
	backup := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "velero.io/v1", "kind": "Backup",
		"metadata": map[string]any{"name": "manual-1", "namespace": "velero", "creationTimestamp": "2026-10-21T08:00:00Z"},
		"status":   map[string]any{"phase": "Failed"},
	}}
	schedule := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "velero.io/v1", "kind": "Schedule",
		"metadata": map[string]any{"name": "velero-daily-apps", "namespace": "velero"},
		"spec":     map[string]any{"schedule": "0 3 * * *", "paused": true},
	}}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{veleroBackupGVR: "BackupList", veleroScheduleGVR: "ScheduleList"}, backup, schedule)

	state, err := ReadBackupsViaAPI(context.Background(), dyn)

	require.NoError(t, err)
	assert.True(t, state.Configured)
	require.Len(t, state.Runs, 1)
	assert.Equal(t, "Failed", state.Runs[0].Phase)
	assert.Equal(t, "", state.Runs[0].Schedule, "a manual backup belongs to no schedule")
	require.Len(t, state.Schedules, 1)
	assert.True(t, state.Schedules[0].Paused)
}

func TestReadBackupsViaAPIWithoutVelero(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{veleroBackupGVR: "BackupList", veleroScheduleGVR: "ScheduleList"})
	dyn.PrependReactor("list", "backups", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(veleroBackupGVR.GroupResource(), "")
	})

	state, err := ReadBackupsViaAPI(context.Background(), dyn)

	require.NoError(t, err)
	assert.False(t, state.Configured)
}
