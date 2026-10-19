package cmd

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/getkipper/kipper/kip/internal/velerolabel"
)

func dumpBackup(name string, hooksFailed int64) unstructured.Unstructured {
	b := unstructured.Unstructured{Object: map[string]interface{}{}}
	b.SetName(name)
	_ = unstructured.SetNestedField(b.Object, "Completed", "status", "phase")
	_ = unstructured.SetNestedField(b.Object, int64(2), "status", "hookStatus", "hooksAttempted")
	_ = unstructured.SetNestedField(b.Object, hooksFailed, "status", "hookStatus", "hooksFailed")
	return b
}

func pvb(backup, volume, phase string) unstructured.Unstructured {
	p := unstructured.Unstructured{Object: map[string]interface{}{}}
	p.SetLabels(map[string]string{"velero.io/backup-name": backup})
	_ = unstructured.SetNestedField(p.Object, volume, "spec", "volume")
	_ = unstructured.SetNestedField(p.Object, phase, "status", "phase")
	return p
}

func TestBackupDumps(t *testing.T) {
	pvbs := []unstructured.Unstructured{
		pvb("daily", "kipper-dump", "Completed"), pvb("daily", "data", "Completed"), pvb("daily", "kipper-dump", "Completed"),
		pvb("partial", "kipper-dump", "Completed"), pvb("partial", "kipper-dump", "Failed"),
		pvb("other", "kipper-dump", "Completed"),
	}

	assert.Equal(t, "complete (2)", backupDumps(dumpBackup("daily", 0), pvbs))
	assert.Equal(t, "failed", backupDumps(dumpBackup("partial", 0), pvbs), "a dump volume that was not copied")
	assert.Equal(t, "failed", backupDumps(dumpBackup("other", 1), pvbs), "a failed pre-hook leaves no trustworthy dump")
	assert.Equal(t, "-", backupDumps(dumpBackup("plain", 0), pvbs))

	running := dumpBackup("daily", 0)
	_ = unstructured.SetNestedField(running.Object, "InProgress", "status", "phase")
	assert.Equal(t, "pending", backupDumps(running, pvbs), "Velero writes the hook status only at the end")

	noHookStatus := dumpBackup("daily", 0)
	unstructured.RemoveNestedField(noHookStatus.Object, "status", "hookStatus")
	assert.Equal(t, "failed", backupDumps(noHookStatus, pvbs), "no record of the hooks is no proof they ran")

	// A dump volume whose copy never started leaves no PodVolumeBackup, and
	// Velero marks the backup PartiallyFailed.
	partial := dumpBackup("daily", 0)
	_ = unstructured.SetNestedField(partial.Object, "PartiallyFailed", "status", "phase")
	assert.Equal(t, "failed", backupDumps(partial, pvbs), "the existing records do not prove every dump was copied")
	nothingCopied := dumpBackup("plain", 0)
	_ = unstructured.SetNestedField(nothingCopied.Object, "PartiallyFailed", "status", "phase")
	assert.Equal(t, "unknown", backupDumps(nothingCopied, pvbs))

	invalid := dumpBackup("plain", 0)
	_ = unstructured.SetNestedField(invalid.Object, "FailedValidation", "status", "phase")
	assert.Equal(t, "-", backupDumps(invalid, pvbs))

	long := strings.Repeat("a", 70)
	longPVBs := []unstructured.Unstructured{pvb(velerolabel.ValidName(long), "kipper-dump", "Completed")}
	assert.Equal(t, "complete (1)", backupDumps(dumpBackup(long, 0), longPVBs), "Velero shortens long names in labels")
}
