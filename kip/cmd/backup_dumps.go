package cmd

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/getkipper/kipper/kip/internal/velerolabel"
)

// dumpVolumeName is the pod volume a Postgres service with consistent backups
// writes its dump to.
const dumpVolumeName = "kipper-dump"

// backupDumps reports dump status from the backup phase, hook results and
// matching dump-volume PodVolumeBackups. "complete (n)" counts copied volumes
// in a Completed backup with recorded hook attempts and no recorded failures.
// "-" means validation failed or a Completed backup has no dump records or
// hook failures. "unknown" means a failed backup has no dump records or hook
// failures, since a copy that never started can leave no PodVolumeBackup. Other finished
// backups report "failed", including those with missing hook attempts.
// Unfinished backups report "pending". Per-database results are in the dump
// manifest and are not inspected here.
func backupDumps(backup unstructured.Unstructured, podVolumeBackups []unstructured.Unstructured) string {
	phase, _, _ := unstructured.NestedString(backup.Object, "status", "phase")
	switch phase {
	case "Completed", "PartiallyFailed", "Failed":
	case "FailedValidation":
		return "-"
	default:
		return "pending"
	}
	label := velerolabel.ValidName(backup.GetName())
	total, completed := 0, 0
	for _, pvb := range podVolumeBackups {
		if pvb.GetLabels()["velero.io/backup-name"] != label {
			continue
		}
		if volume, _, _ := unstructured.NestedString(pvb.Object, "spec", "volume"); volume != dumpVolumeName {
			continue
		}
		total++
		if phase, _, _ := unstructured.NestedString(pvb.Object, "status", "phase"); phase == "Completed" {
			completed++
		}
	}
	attempted, recorded, _ := unstructured.NestedInt64(backup.Object, "status", "hookStatus", "hooksAttempted")
	failed, _, _ := unstructured.NestedInt64(backup.Object, "status", "hookStatus", "hooksFailed")
	switch {
	case total == 0 && failed == 0 && phase == "Completed":
		return "-"
	case total == 0 && failed == 0:
		return "unknown"
	case phase == "Completed" && recorded && attempted > 0 && failed == 0 && completed == total:
		return fmt.Sprintf("complete (%d)", total)
	}
	return "failed"
}
