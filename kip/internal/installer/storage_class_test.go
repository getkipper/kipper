package installer

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/controller/pkg/platform"
)

func TestLiveClaimClass(t *testing.T) {
	t.Run("a missing claim gets the fallback", func(t *testing.T) {
		got, err := liveClaimClass(&routedRunner{}, "velero", "minio-storage", platform.LonghornSingleClass)
		require.NoError(t, err)
		assert.Equal(t, platform.LonghornSingleClass, got)
	})

	t.Run("an existing claim keeps its class", func(t *testing.T) {
		host := &routedRunner{routes: []route{{contains: "get pvc minio-storage", out: "Warning: noise\nclaim-class[local-path]"}}}
		got, err := liveClaimClass(host, "velero", "minio-storage", platform.LonghornSingleClass)
		require.NoError(t, err)
		assert.Equal(t, platform.LocalPathClass, got)
	})

	t.Run("an existing claim without a class gets the fallback", func(t *testing.T) {
		host := &routedRunner{routes: []route{{contains: "get pvc minio-storage", out: "claim-class[]"}}}
		got, err := liveClaimClass(host, "velero", "minio-storage", platform.LonghornSingleClass)
		require.NoError(t, err)
		assert.Equal(t, platform.LonghornSingleClass, got)
	})

	t.Run("a failed lookup stops", func(t *testing.T) {
		host := &routedRunner{routes: []route{{contains: "get pvc minio-storage", err: errors.New("connection refused")}}}
		_, err := liveClaimClass(host, "velero", "minio-storage", platform.LonghornSingleClass)
		assert.Error(t, err)
	})
}

// After a local cluster grows onto Longhorn, the next upgrade must not ask a
// bound local-path claim to become longhorn-single: claim classes are immutable.
func TestInstallBackupKeepsTheLiveMinIOClassAfterGrowth(t *testing.T) {
	host := &routedRunner{routes: []route{
		{contains: "claim-class", out: "claim-class[local-path]"},
		{contains: "get pvc minio-storage", out: "minio-size[10Gi|10Gi]"},
	}}
	require.NoError(t, installInClusterMinIO(host, "pw", platform.LonghornSingleClass))
	applied := strings.Join(host.stdins, "") + strings.Join(host.commands, "")
	assert.Contains(t, applied, "storageClassName: local-path")
	assert.NotContains(t, applied, "storageClassName: longhorn-single")
}

func TestInstallZotKeepsTheLiveRegistryClassAfterGrowth(t *testing.T) {
	host := &routedRunner{routes: []route{
		{contains: "claim-class", out: "claim-class[local-path]"},
	}}
	_ = InstallZot(host, platform.StorageLonghorn)
	applied := strings.Join(host.commands, "")
	assert.Contains(t, applied, "storageClassName: local-path")
	assert.NotContains(t, applied, "storageClassName: longhorn-single")
}

func TestInstallZotGivesANewRegistryTheModesClass(t *testing.T) {
	host := &routedRunner{}
	_ = InstallZot(host, platform.StorageLonghorn)
	assert.Contains(t, strings.Join(host.commands, ""), "storageClassName: longhorn-single")
}
