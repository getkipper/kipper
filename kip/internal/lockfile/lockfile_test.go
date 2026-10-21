package lockfile

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTryExclusiveReportsAHeldLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "l")
	release, err := TryExclusive(path)
	require.NoError(t, err)

	_, err = TryExclusive(path)
	assert.ErrorIs(t, err, ErrHeld)

	release()
	again, err := TryExclusive(path)
	require.NoError(t, err)
	again()
}

func TestTryExclusiveRefusesWhileExclusiveHolds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "l")
	release, err := Exclusive(path)
	require.NoError(t, err)
	defer release()
	_, err = TryExclusive(path)
	assert.ErrorIs(t, err, ErrHeld)
}
