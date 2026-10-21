//go:build windows

package opstunnel

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestSecureDirLeavesOnlyTheOwnerOnWindows(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run")
	require.NoError(t, secureDir(dir))
	require.NoError(t, ownedByCurrentUser(dir))
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	require.NoError(t, err)
	control, _, err := sd.Control()
	require.NoError(t, err)
	require.NotZero(t, control&windows.SE_DACL_PROTECTED, "inherited entries are dropped")
}
