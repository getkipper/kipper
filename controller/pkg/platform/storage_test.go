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
}
