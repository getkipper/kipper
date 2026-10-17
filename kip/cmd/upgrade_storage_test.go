package cmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/getkipper/kipper/kip/internal/installer"
)

func platformConfigClient(storage string, objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(platformConfigGVR.GroupVersion().WithKind("PlatformConfig"), &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(platformConfigGVR.GroupVersion().WithKind("PlatformConfigList"), &unstructured.UnstructuredList{})
	scheme.AddKnownTypeWithName(backupStorageLocationGVR.GroupVersion().WithKind("BackupStorageLocation"), &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(backupStorageLocationGVR.GroupVersion().WithKind("BackupStorageLocationList"), &unstructured.UnstructuredList{})
	spec := map[string]interface{}{"profile": "small"}
	if storage != "" {
		spec["storage"] = storage
	}
	pc := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1", "kind": "PlatformConfig",
		"metadata": map[string]interface{}{"name": "platform"},
		"spec":     spec,
	}}
	return dynamicfake.NewSimpleDynamicClient(scheme, append([]runtime.Object{pc}, objects...)...)
}

func backupLocation(s3URL string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "velero.io/v1", "kind": "BackupStorageLocation",
		"metadata": map[string]interface{}{"name": "default", "namespace": "velero"},
		"spec":     map[string]interface{}{"config": map[string]interface{}{"s3Url": s3URL}},
	}}
}

// TestRecordLegacyBackupChecksWhereVeleroWrites checks that missing local storage
// settings cannot cause an upgrade to record in-cluster mode and redirect a
// legacy cluster's external backups to MinIO.
func TestRecordLegacyBackupChecksWhereVeleroWrites(t *testing.T) {
	dyn := platformConfigClient("", backupLocation("https://s3.example.com"))
	state := installer.PlatformState{}
	err := recordLegacyBackup(context.Background(), dyn, &state)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "external backup storage")
	assert.Empty(t, recordedBackup(t, dyn), "nothing is recorded")

	dyn = platformConfigClient("", backupLocation("http://minio.velero.svc:9000"))
	state = installer.PlatformState{}
	require.NoError(t, recordLegacyBackup(context.Background(), dyn, &state))
	assert.Equal(t, installer.BackupInCluster, recordedBackup(t, dyn))
}

func recordedStorage(t *testing.T, dyn *dynamicfake.FakeDynamicClient) string {
	t.Helper()
	pc, err := dyn.Resource(platformConfigGVR).Get(context.Background(), "platform", metav1.GetOptions{})
	require.NoError(t, err)
	storage, _, _ := unstructured.NestedString(pc.Object, "spec", "storage")
	return storage
}

func TestLoadPlatformStateRecordsLonghornOnALegacyCluster(t *testing.T) {
	dyn := platformConfigClient("")
	state, err := loadPlatformState(context.Background(), dyn)
	require.NoError(t, err)
	assert.Equal(t, installer.StorageLonghorn, state.Storage)
	assert.Equal(t, installer.StorageLonghorn, recordedStorage(t, dyn))
}

func TestLoadPlatformStateKeepsARecordedMode(t *testing.T) {
	dyn := platformConfigClient(installer.StorageLocal)
	state, err := loadPlatformState(context.Background(), dyn)
	require.NoError(t, err)
	assert.Equal(t, installer.StorageLocal, state.Storage)
	for _, a := range dyn.Actions() {
		assert.NotEqual(t, "patch", a.GetVerb(), "a recorded mode is never rewritten")
	}
}

func recordedBackup(t *testing.T, dyn *dynamicfake.FakeDynamicClient) string {
	t.Helper()
	pc, err := dyn.Resource(platformConfigGVR).Get(context.Background(), "platform", metav1.GetOptions{})
	require.NoError(t, err)
	backup, _, _ := unstructured.NestedString(pc.Object, "spec", "backup")
	return backup
}

// TestRecordLegacyBackup checks that saved storage settings determine the legacy mode.
func TestRecordLegacyBackup(t *testing.T) {
	dyn := platformConfigClient("")
	state := installer.PlatformState{}
	require.NoError(t, recordLegacyBackup(context.Background(), dyn, &state))
	assert.Equal(t, installer.BackupInCluster, state.Backup)
	assert.Equal(t, installer.BackupInCluster, recordedBackup(t, dyn))

	dyn = platformConfigClient("")
	state = installer.PlatformState{BackupStorage: &installer.BackupStorageConfig{Bucket: "b"}}
	require.NoError(t, recordLegacyBackup(context.Background(), dyn, &state))
	assert.Equal(t, installer.BackupExternal, recordedBackup(t, dyn))

	dyn = platformConfigClient("")
	state = installer.PlatformState{Backup: installer.BackupNone}
	require.NoError(t, recordLegacyBackup(context.Background(), dyn, &state))
	for _, a := range dyn.Actions() {
		assert.NotEqual(t, "patch", a.GetVerb(), "a recorded mode is left alone")
	}
}

func platformConfigWithBackup(backup string) *dynamicfake.FakeDynamicClient {
	dyn := platformConfigClient(installer.StorageLocal)
	if backup != "" {
		pc, _ := dyn.Resource(platformConfigGVR).Get(context.Background(), "platform", metav1.GetOptions{})
		_ = unstructured.SetNestedField(pc.Object, backup, "spec", "backup")
		_, _ = dyn.Resource(platformConfigGVR).Update(context.Background(), pc, metav1.UpdateOptions{})
	}
	return dyn
}

func TestBackupsConfiguredOnCluster(t *testing.T) {
	ctx := context.Background()
	assert.False(t, backupsConfiguredOn(ctx, platformConfigWithBackup(installer.BackupNone)))
	assert.True(t, backupsConfiguredOn(ctx, platformConfigWithBackup(installer.BackupExternal)))
	assert.True(t, backupsConfiguredOn(ctx, platformConfigWithBackup("")), "a legacy cluster runs Velero")

	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(platformConfigGVR.GroupVersion().WithKind("PlatformConfigList"), &unstructured.UnstructuredList{})
	assert.True(t, backupsConfiguredOn(ctx, dynamicfake.NewSimpleDynamicClient(scheme)), "an unreadable PlatformConfig keeps the old behavior")
}

// TestRequireBackupDestination checks that upgrades preserve external backup destinations.
func TestRequireBackupDestination(t *testing.T) {
	assert.ErrorContains(t, requireBackupDestination(installer.PlatformState{Backup: installer.BackupExternal}), "--backup-storage-bucket")
	assert.NoError(t, requireBackupDestination(installer.PlatformState{Backup: installer.BackupExternal, BackupStorage: &installer.BackupStorageConfig{Bucket: "b"}}))
	assert.NoError(t, requireBackupDestination(installer.PlatformState{Backup: installer.BackupInCluster}))
	assert.NoError(t, requireBackupDestination(installer.PlatformState{Backup: installer.BackupNone}))
}
