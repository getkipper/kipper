package platform

// Backup modes are recorded in PlatformConfig.spec.backup.
const (
	BackupNone      = "none"
	BackupInCluster = "in-cluster"
	BackupExternal  = "external"
)

// BackupsConfigured reports whether the recorded mode enables Velero.
// Only BackupNone disables it. Empty and unknown values count as configured
// for compatibility with existing clusters.
func BackupsConfigured(recorded string) bool {
	return recorded != BackupNone
}

// BackupsNotConfigured explains how to enable backups in the CLI, API and console.
const BackupsNotConfigured = "Backups are not configured. This cluster uses local storage and needs external storage for backups. " +
	"Add external backup storage by running kip install again with --backup-storage-bucket and its related flags."
