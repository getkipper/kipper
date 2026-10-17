package installer

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLocalStorageNotes(t *testing.T) {
	notes := strings.Join(localStorageNotes(BackupNone), "\n")
	assert.Contains(t, notes, "one node")
	assert.Contains(t, notes, "--backup-storage-bucket", "a cluster without backups learns how to get them")
	assert.Contains(t, notes, "Cluster Migration", "and how to grow beyond one node")
	assert.NotContains(t, strings.ToLower(notes), "disposable")
	assert.NotContains(t, strings.ToLower(notes), "throwaway")

	assert.NotContains(t, strings.Join(localStorageNotes(BackupExternal), "\n"), "--backup-storage-bucket",
		"external backups need no hint")
}
