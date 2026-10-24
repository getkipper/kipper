package installer

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLonghornMemoryFloor(t *testing.T) {
	t.Run("a server at the floor passes", func(t *testing.T) {
		warning, err := CheckLonghornMemory(LonghornMinRAMMB, false)
		require.NoError(t, err)
		assert.Empty(t, warning)
	})

	t.Run("a 4 GB server is refused with resize and rerun", func(t *testing.T) {
		_, err := CheckLonghornMemory(3867, false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "3867 MiB")
		assert.Contains(t, err.Error(), "6000 MiB")
		assert.Contains(t, err.Error(), "Resize the server")
		assert.Contains(t, err.Error(), "run the command again")
		assert.NotContains(t, err.Error(), "profile", "the refusal must not suggest another profile")
	})

	t.Run("the override passes with a warning", func(t *testing.T) {
		warning, err := CheckLonghornMemory(3867, true)
		require.NoError(t, err)
		assert.Contains(t, warning, "3867 MiB")
		assert.Contains(t, warning, "below")
	})

	t.Run("the override above the floor says nothing", func(t *testing.T) {
		warning, err := CheckLonghornMemory(8192, true)
		require.NoError(t, err)
		assert.Empty(t, warning)
	})
}

func TestMemoryFloorOverrideAnnotation(t *testing.T) {
	r := &recordingRunner{}
	require.NoError(t, RecordMemoryFloorOverride(r, 3867))
	require.Len(t, r.commands, 1)
	assert.Contains(t, r.commands[0], "kubectl annotate --overwrite platformconfig platform")
	assert.Contains(t, r.commands[0], MemoryFloorOverrideAnnotation+"=")
	assert.Contains(t, r.commands[0], "ram=3867MiB floor=6000MiB")
}

func TestInstallAppliesTheFloorToNewLonghornClustersOnly(t *testing.T) {
	_, err := installMemoryFloor(StorageLonghorn, false, 3867, false)
	assert.Error(t, err, "a new Longhorn install below the floor is refused")

	_, err = installMemoryFloor(StorageLonghorn, true, 3867, false)
	assert.NoError(t, err, "an existing Longhorn cluster keeps installing and upgrading below it")

	_, err = installMemoryFloor(StorageLocal, false, 3867, false)
	assert.NoError(t, err, "a new local-storage install keeps its own floor")

	warning, err := installMemoryFloor(StorageLonghorn, false, 3867, true)
	assert.NoError(t, err)
	assert.NotEmpty(t, warning, "the override warns")
}
