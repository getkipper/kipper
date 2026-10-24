package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/getkipper/kipper/kip/internal/installer"
)

func TestNodeAddRefusesALocalStorageCluster(t *testing.T) {
	err := refuseNodeAddOnLocalStorage(installer.StorageLocal)
	assert.ErrorContains(t, err, "local storage")
	assert.ErrorContains(t, err, "Longhorn")
	assert.ErrorContains(t, err, "kip cluster storage longhorn")
	assert.NoError(t, refuseNodeAddOnLocalStorage(installer.StorageLonghorn))
}

func TestRepairHostIsNotNeededOnLocalStorage(t *testing.T) {
	done, err := repairHostNotNeeded(&localStorageHost{})
	assert.NoError(t, err)
	assert.True(t, done)

	done, err = repairHostNotNeeded(brokenLonghorn{})
	assert.Error(t, err, "an unreadable mode changes no host")
	assert.False(t, done)
}
