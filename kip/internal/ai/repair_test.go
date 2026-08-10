package ai

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// brokenBackupRepository builds a BackupRepository CR in the given
// phase with a creationTimestamp old enough to clear the broken-repo
// age floor. Tests that need a transient-init repo override the time.
func brokenBackupRepository(name, phase string, age time.Duration) *unstructured.Unstructured {
	createdAt := metav1.NewTime(time.Now().Add(-age))
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "velero.io/v1",
		"kind":       "BackupRepository",
		"metadata": map[string]interface{}{
			"name":              name,
			"namespace":         VeleroNamespace,
			"creationTimestamp": createdAt.UTC().Format(time.RFC3339),
		},
		"status": map[string]interface{}{"phase": phase},
	}}
}

func TestParseMinIOBackupNamesStripsTrailingSlash(t *testing.T) {
	logs := `{"status":"success","type":"folder","key":"snapshot-a/","size":0}
{"status":"success","type":"folder","key":"snapshot-b/","size":0}
mc: <DEBUG> some non-JSON warning here
{"status":"success","type":"folder","key":"daily-apps-20260510/","size":0}
`
	got := parseMinIOBackupNames(logs)
	assert.Equal(t, []string{"daily-apps-20260510", "snapshot-a", "snapshot-b"}, got)
}

func TestParseMinIOBackupNamesIgnoresEmptyAndMalformed(t *testing.T) {
	logs := `

not-json
{"status":"success","type":"folder","key":"","size":0}
{"status":"success","type":"folder","key":"good/","size":0}
{}
`
	got := parseMinIOBackupNames(logs)
	assert.Equal(t, []string{"good"}, got)
}

func TestRepairPlanIsClean(t *testing.T) {
	clean := &RepairPlan{MinIOReachable: true}
	assert.True(t, clean.IsClean())

	withOrphan := &RepairPlan{OrphanInCluster: []string{"x"}, MinIOReachable: true}
	assert.False(t, withOrphan.IsClean())

	withRepo := &RepairPlan{BrokenRepos: []string{"r"}, MinIOReachable: true}
	assert.False(t, withRepo.IsClean())

	withStorage := &RepairPlan{OrphanInStorage: []string{"s"}, MinIOReachable: true}
	assert.False(t, withStorage.IsClean())
}

// TestDetectOrphansClusterSideOnly: when MinIO is unreachable
// (no helper Pod can run because the fake apiserver does not run a
// kubelet), DetectOrphans must still surface broken BackupRepository
// CRs and return a partial plan with MinIOReachable=false. Cluster-
// side findings are the operator's first signal that something is
// wrong, so suppressing them when MinIO is also down would be hostile.
func TestDetectOrphansClusterSideOnly(t *testing.T) {
	client := veleroDynamicFake()

	// Kipper-owned Backup CR — would be checked against MinIO
	// listing if we had one.
	kipperBackup := newBackupWithStatus("mine", "Completed", 0)
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, kipperBackup, VeleroNamespace))

	// Broken BackupRepository — the smoke-test bug. NotReady plus
	// an age beyond the brokenRepoMinAge floor flags it.
	br := brokenBackupRepository("default-velero-kopia-abc", "NotReady", 30*time.Minute)
	require.NoError(t, client.Tracker().Create(veleroBackupRepositoryGVR, br, VeleroNamespace))

	inst := fastInstaller(client)
	plan, err := inst.DetectOrphans(t.Context())

	// MinIO listing fails (the fake clientset's GetLogs doesn't
	// return a streamed body), so DetectOrphans returns the partial
	// plan plus an error. The plan must still carry the broken-repo
	// finding.
	require.Error(t, err)
	require.NotNil(t, plan)
	assert.False(t, plan.MinIOReachable)
	assert.Equal(t, []string{"default-velero-kopia-abc"}, plan.BrokenRepos)
}

// TestApplyRepairDeletesBrokenRepository: confirms the repair
// actually deletes the broken BackupRepository CR (Velero re-creates
// on next backup).
func TestApplyRepairDeletesBrokenRepository(t *testing.T) {
	client := veleroDynamicFake()
	br := brokenBackupRepository("default-velero-kopia-broken", "NotReady", 30*time.Minute)
	require.NoError(t, client.Tracker().Create(veleroBackupRepositoryGVR, br, VeleroNamespace))

	inst := fastInstaller(client)
	plan := &RepairPlan{BrokenRepos: []string{"default-velero-kopia-broken"}, MinIOReachable: true}

	var buf bytes.Buffer
	require.NoError(t, inst.ApplyRepair(t.Context(), plan, &buf))

	_, err := client.Tracker().Get(veleroBackupRepositoryGVR, VeleroNamespace, "default-velero-kopia-broken")
	require.Error(t, err, "BackupRepository should be deleted")
}

// TestListBrokenRepositoriesSkipsTransientInit: a freshly-created
// BackupRepository in NotReady is normal during init and must not
// be flagged. Repair would otherwise delete an actively-initialising
// repo and interrupt the first backup of a fresh install.
func TestListBrokenRepositoriesSkipsTransientInit(t *testing.T) {
	client := veleroDynamicFake()
	// 30 seconds old; well below the brokenRepoMinAge floor.
	fresh := brokenBackupRepository("default-velero-kopia-init", "NotReady", 30*time.Second)
	require.NoError(t, client.Tracker().Create(veleroBackupRepositoryGVR, fresh, VeleroNamespace))

	inst := fastInstaller(client)
	got, err := inst.listBrokenBackupRepositories(t.Context())
	require.NoError(t, err)
	assert.Empty(t, got, "fresh NotReady repos must not be flagged as broken")
}

// TestListBrokenRepositoriesSkipsNewPhase: BackupRepositories in the
// New phase are still bootstrapping; repair must never touch them.
func TestListBrokenRepositoriesSkipsNewPhase(t *testing.T) {
	client := veleroDynamicFake()
	newRepo := brokenBackupRepository("default-velero-kopia-new", "New", 30*time.Minute)
	require.NoError(t, client.Tracker().Create(veleroBackupRepositoryGVR, newRepo, VeleroNamespace))

	inst := fastInstaller(client)
	got, err := inst.listBrokenBackupRepositories(t.Context())
	require.NoError(t, err)
	assert.Empty(t, got, "BackupRepositories in phase New are bootstrapping and must not be flagged")
}

// TestApplyRepairDeletesOrphanCRs: Backup CRs whose Kopia data is
// gone are deleted via the dynamic client (DeleteBackupRequest would
// loop trying to free non-existent repo data; direct delete is
// correct here).
func TestApplyRepairDeletesOrphanCRs(t *testing.T) {
	client := veleroDynamicFake()
	orphan := newBackupWithStatus("orphan-cr", "Completed", 0)
	require.NoError(t, client.Tracker().Create(veleroBackupGVR, orphan, VeleroNamespace))

	inst := fastInstaller(client)
	plan := &RepairPlan{OrphanInCluster: []string{"orphan-cr"}, MinIOReachable: true}

	var buf bytes.Buffer
	require.NoError(t, inst.ApplyRepair(t.Context(), plan, &buf))

	_, err := client.Tracker().Get(veleroBackupGVR, VeleroNamespace, "orphan-cr")
	require.Error(t, err, "Backup CR should be deleted")
}

// TestApplyRepairTolerantOfMissingResources: re-running repair after
// a previous run, or against a CR that disappeared between detect
// and apply, must not error. Idempotent cleanup is the contract.
func TestApplyRepairTolerantOfMissingResources(t *testing.T) {
	client := veleroDynamicFake()
	inst := fastInstaller(client)

	plan := &RepairPlan{
		BrokenRepos:     []string{"already-gone-repo"},
		OrphanInCluster: []string{"already-gone-cr"},
		MinIOReachable:  true,
	}

	var buf bytes.Buffer
	require.NoError(t, inst.ApplyRepair(t.Context(), plan, &buf))
}
