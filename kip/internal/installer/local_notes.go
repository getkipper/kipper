package installer

// localStorageNotes returns installation guidance for local storage.
// It includes external backup setup guidance only when backup is BackupNone.
func localStorageNotes(backup string) []string {
	notes := []string{"This is a full Kubernetes cluster on one node, with every volume stored on this server's disk."}
	if backup == BackupNone {
		notes = append(notes, "Backups need external S3-compatible storage. Add it with --backup-storage-bucket and its related flags.")
	}
	return append(notes, "To add nodes later, switch to Longhorn with 'kip cluster storage longhorn'.")
}
