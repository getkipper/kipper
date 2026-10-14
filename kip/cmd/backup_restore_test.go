package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func backupInPhase(phase string) *unstructured.Unstructured {
	b := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "velero.io/v1", "kind": "Backup",
		"metadata": map[string]interface{}{"name": "before-reboot", "namespace": "velero"},
	}}
	if phase != "" {
		_ = unstructured.SetNestedField(b.Object, phase, "status", "phase")
	}
	return b
}

func podVolumeBackup(namespace, pod, volume, phase string) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{
			"volume": volume,
			"pod":    map[string]interface{}{"namespace": namespace, "name": pod},
		},
		"status": map[string]interface{}{"phase": phase},
	}}
}

func TestCheckRestorableAcceptsACompletedBackup(t *testing.T) {
	assert.NoError(t, checkRestorable(backupInPhase("Completed"), nil, false))
}

func TestCheckRestorableRefusesAPartiallyFailedBackupAndNamesTheMissingVolumes(t *testing.T) {
	volumes := []unstructured.Unstructured{
		podVolumeBackup("shop", "db-0", "data", "Failed"),
		podVolumeBackup("shop", "web-0", "uploads", "Completed"),
	}

	err := checkRestorable(backupInPhase("PartiallyFailed"), volumes, false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "PartiallyFailed")
	assert.Contains(t, err.Error(), "shop/db-0 volume data")
	assert.NotContains(t, err.Error(), "web-0")
	assert.Contains(t, err.Error(), "--allow-partial")
}

func TestCheckRestorableNamesCanceledVolumesToo(t *testing.T) {
	volumes := []unstructured.Unstructured{
		podVolumeBackup("shop", "db-0", "data", "Failed"),
		podVolumeBackup("blog", "cache-0", "data", "Canceled"),
		podVolumeBackup("shop", "web-0", "uploads", "Completed"),
	}

	err := checkRestorable(backupInPhase("PartiallyFailed"), volumes, false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "shop/db-0 volume data")
	assert.Contains(t, err.Error(), "blog/cache-0 volume data")
	assert.NotContains(t, err.Error(), "web-0")
}

func TestCheckRestorableRefusesAPartialBackupWithoutClaimingLostVolumes(t *testing.T) {
	for name, volumes := range map[string][]unstructured.Unstructured{
		"no volume backups":     nil,
		"all volumes backed up": {podVolumeBackup("shop", "db-0", "data", "Completed")},
	} {
		err := checkRestorable(backupInPhase("PartiallyFailed"), volumes, false)
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), "--allow-partial", name)
		assert.NotContains(t, err.Error(), "did not back up", name)
	}
}

func TestCheckRestorableAllowsAPartiallyFailedBackupWhenAsked(t *testing.T) {
	assert.NoError(t, checkRestorable(backupInPhase("PartiallyFailed"), nil, true))
}

func TestCheckRestorableRefusesABackupThatHasNotCompleted(t *testing.T) {
	for _, phase := range []string{"InProgress", "Failed", "FailedValidation", ""} {
		err := checkRestorable(backupInPhase(phase), nil, true)
		require.Error(t, err, phase)
		assert.Contains(t, err.Error(), "before-reboot", phase)
	}
}

func TestCheckRestorableSaysWhatToDoForEachPhase(t *testing.T) {
	for _, phase := range []string{"New", "InProgress", "WaitingForPluginOperations", "Finalizing"} {
		err := checkRestorable(backupInPhase(phase), nil, false)
		require.Error(t, err, phase)
		assert.Contains(t, err.Error(), "wait for it to finish", phase)
		assert.NotContains(t, err.Error(), "--allow-partial", phase)
	}
	for _, phase := range []string{"Failed", "FailedValidation", "Deleting"} {
		err := checkRestorable(backupInPhase(phase), nil, false)
		require.Error(t, err, phase)
		assert.Contains(t, err.Error(), "cannot be restored", phase)
		assert.NotContains(t, err.Error(), "--allow-partial", phase)
	}
}

func TestPartialRestoreWarningNamesTheMissingVolumes(t *testing.T) {
	volumes := []unstructured.Unstructured{
		podVolumeBackup("shop", "db-0", "data", "Failed"),
		podVolumeBackup("shop", "web-0", "uploads", "Completed"),
	}
	warning := partialRestoreWarning("before-reboot", volumes)
	assert.Contains(t, warning, "shop/db-0 volume data")
	assert.NotContains(t, warning, "web-0")
	assert.Contains(t, partialRestoreWarning("before-reboot", nil), "before-reboot")
}
