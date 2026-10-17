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
