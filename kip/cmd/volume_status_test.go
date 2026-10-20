package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestVolumeListStatusShowsAnUnsupportedVolume(t *testing.T) {
	refused := &unstructured.Unstructured{Object: map[string]interface{}{
		"status": map[string]interface{}{
			"phase": "Pending",
			"conditions": []interface{}{map[string]interface{}{
				"type": "SharedStorageUnsupported", "status": "True",
			}},
		},
	}}
	assert.Equal(t, "unsupported", volumeListStatus(refused))
	assert.Equal(t, "bound", volumeListStatus(&unstructured.Unstructured{Object: map[string]interface{}{
		"status": map[string]interface{}{"phase": "Bound"},
	}}))
	assert.Equal(t, "pending", volumeListStatus(&unstructured.Unstructured{Object: map[string]interface{}{}}))
}

// A failed holder puts file backups at risk when no app mounts the volume,
// so the list surfaces the warning. Normal startup needs no warning.
func TestVolumeBackupWarningNamesAHolderThatCannotRun(t *testing.T) {
	withHolder := func(status, reason, message string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]interface{}{
			"status": map[string]interface{}{
				"phase": "Bound",
				"conditions": []interface{}{map[string]interface{}{
					"type": "BackupHolderReady", "status": status, "reason": reason, "message": message,
				}},
			},
		}}
	}
	assert.Equal(t, "the cluster refused it", volumeBackupWarning(withHolder("False", "CreateRejected", "the cluster refused it")))
	assert.Empty(t, volumeBackupWarning(withHolder("False", "Starting", "starting")))
	assert.Empty(t, volumeBackupWarning(withHolder("False", "Restoring", "restoring")))
	assert.Empty(t, volumeBackupWarning(withHolder("True", "Running", "running")))
	assert.Empty(t, volumeBackupWarning(&unstructured.Unstructured{Object: map[string]interface{}{}}))
}
