package cmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/getkipper/kipper/kip/internal/installer"
)

func TestUpgradeKeepsTheLocalBackupStorageWithoutARecord(t *testing.T) {
	local := &installer.BackupStorageConfig{Bucket: "backups", Region: "eu-west-1"}

	got, err := backupStorageForUpgrade(context.Background(), fake.NewClientset(), "prod", local)

	require.NoError(t, err)
	assert.Same(t, local, got)
}

func TestUpgradeRefusesWhileBackupStorageMoves(t *testing.T) {
	record := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "velero", Name: installer.StorageRecordConfigMap},
		Data:       map[string]string{"move": `{"target":{"bucket":"new-backups","endpoint":"https://objects.example.com"},"phase":"preparing"}`},
	}

	_, err := backupStorageForUpgrade(context.Background(), fake.NewClientset(record), "prod", &installer.BackupStorageConfig{Bucket: "backups", Region: "eu-west-1"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "new-backups")
}

func TestBackupListNamesThePreviousBucket(t *testing.T) {
	record := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "velero", Name: installer.StorageRecordConfigMap},
		Data:       map[string]string{"previous": `{"bucket":"old-backups","region":"eu-west-1","switchedAt":"2026-10-20T08:00:00Z"}`},
	}

	note := previousStorageNote(context.Background(), fake.NewClientset(record))

	assert.Contains(t, note, "old-backups")
	assert.Contains(t, note, "2026-10-20T08:00:00Z")
	assert.Empty(t, previousStorageNote(context.Background(), fake.NewClientset()))
}
