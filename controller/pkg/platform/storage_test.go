package platform

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestStorageModeReadsALegacyClusterAsLonghorn(t *testing.T) {
	assert.Equal(t, StorageLonghorn, StorageMode(""), "clusters recorded before the field existed run Longhorn")
	assert.Equal(t, StorageLonghorn, StorageMode(StorageLonghorn))
	assert.Equal(t, StorageLocal, StorageMode(StorageLocal))
}

func TestIsStorageMode(t *testing.T) {
	assert.True(t, IsStorageMode(StorageLocal))
	assert.True(t, IsStorageMode(StorageLonghorn))
	assert.False(t, IsStorageMode(""))
	assert.False(t, IsStorageMode("nfs"))
}

func TestSingleWriterClassFollowsTheMode(t *testing.T) {
	assert.Equal(t, "local-path", SingleWriterClass(StorageLocal))
	assert.Equal(t, "longhorn-single", SingleWriterClass(StorageLonghorn))
	assert.Equal(t, "longhorn-single", SingleWriterClass(""), "a legacy cluster runs Longhorn")
}

func TestSharedVolumesNeedLonghorn(t *testing.T) {
	assert.False(t, SupportsSharedVolumes(StorageLocal))
	assert.True(t, SupportsSharedVolumes(StorageLonghorn))
	assert.True(t, SupportsSharedVolumes(""))
	assert.Contains(t, SharedVolumesUnsupported, "local storage")
	assert.Contains(t, SharedVolumesUnsupported, "Longhorn")
	assert.Contains(t, SharedVolumesUnsupported, "kip cluster storage longhorn")
}

func TestInstalledFollowsProfileOverridesAndStorage(t *testing.T) {
	assert.False(t, Installed(ComponentLonghorn, nil, ProfileNano, StorageLocal, ""), "local storage runs no Longhorn")
	assert.True(t, Installed(ComponentLonghorn, nil, ProfileNano, "", ""), "a legacy nano runs Longhorn")
	assert.False(t, Installed(ComponentLoki, nil, ProfileNano, StorageLocal, ""), "nano turns monitoring off")
	assert.False(t, Installed(ComponentGrafana, nil, ProfileNano, StorageLocal, ""), "Grafana follows Prometheus")
	on := true
	assert.True(t, Installed(ComponentLoki, map[string]*bool{ComponentLoki: &on}, ProfileNano, StorageLocal, ""), "an explicit enable wins")
	assert.True(t, Installed(ComponentPromtail, map[string]*bool{ComponentLoki: &on}, ProfileNano, StorageLocal, ""), "Promtail follows Loki")
	assert.True(t, Installed(ComponentPrometheus, nil, ProfileSmall, StorageLonghorn, ""))
	assert.True(t, Installed("dex", nil, ProfileNano, StorageLocal, ""), "components outside the table are always there")
}

func TestInstalledLeavesOutVeleroWithoutBackups(t *testing.T) {
	assert.False(t, Installed(ComponentVelero, nil, ProfileNano, StorageLocal, BackupNone))
	assert.True(t, Installed(ComponentVelero, nil, ProfileNano, StorageLocal, BackupExternal))
	assert.True(t, Installed(ComponentVelero, nil, ProfileSmall, StorageLonghorn, ""), "a legacy cluster runs Velero")
}
