package installer

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRHEL97LevelHost(t *testing.T) {
	held := []struct{ id, version, kernel string }{
		{"rocky", "9.7", "5.14.0-611.5.1.el9_7.x86_64"},
		{"rocky", "9.8", "5.14.0-687.41.1.el9_8.x86_64"},
		{"almalinux", "9.7", ""},
		{"almalinux", "9.10", ""},
		{"centos", "9", "5.14.0-611.el9.x86_64"},
		{"centos", "9", "5.14.0-650.el9.aarch64"},
		// A 9.6 release running a 9.7 kernel is held too: the trigger is unknown.
		{"rocky", "9.6", "5.14.0-611.5.1.el9_7.x86_64"},
	}
	for _, h := range held {
		assert.True(t, RHEL97LevelHost(h.id, h.version, h.kernel), "%+v", h)
	}
	clear := []struct{ id, version, kernel string }{
		{"rocky", "9.6", "5.14.0-570.12.1.el9_6.x86_64"},
		{"almalinux", "9.5", ""},
		{"centos", "9", "5.14.0-570.el9.x86_64"},
		{"rocky", "10.0", "6.12.0-55.9.1.el10_0.x86_64"},
		{"ubuntu", "24.04", "6.8.0-45-generic"},
		{"debian", "13", "6.12.48+deb13-amd64"},
	}
	for _, h := range clear {
		assert.False(t, RHEL97LevelHost(h.id, h.version, h.kernel), "%+v", h)
	}
}

func TestRHEL97LevelNode(t *testing.T) {
	assert.True(t, RHEL97LevelNode(NodeFacts{OSImage: "Rocky Linux 9.7 (Blue Onyx)", Kernel: "5.14.0-611.5.1.el9_7.x86_64"}))
	assert.True(t, RHEL97LevelNode(NodeFacts{OSImage: "Rocky Linux 9.8 (Blue Onyx)", Kernel: "5.14.0-687.41.1.el9_8.x86_64"}))
	assert.True(t, RHEL97LevelNode(NodeFacts{OSImage: "AlmaLinux 9.7 (Moss Jungle Cat)", Kernel: "5.14.0-570.el9_6.x86_64"}))
	assert.True(t, RHEL97LevelNode(NodeFacts{OSImage: "CentOS Stream 9", Kernel: "5.14.0-620.el9.x86_64"}))
	assert.False(t, RHEL97LevelNode(NodeFacts{OSImage: "Rocky Linux 9.6 (Blue Onyx)", Kernel: "5.14.0-570.12.1.el9_6.x86_64"}))
	assert.False(t, RHEL97LevelNode(NodeFacts{OSImage: "Ubuntu 24.04.3 LTS", Kernel: "6.8.0-85-generic"}))
}

func TestPreflightRefusesALonghornHostAtTheRHEL97Level(t *testing.T) {
	sys := testSysInfo()
	sys.OS, sys.OSVersion, sys.Kernel = "rocky", "9.7", "5.14.0-611.5.1.el9_7.x86_64"

	_, err := RunPreflightChecks(sys, PlatformState{Profile: ProfileSmall, Storage: StorageLonghorn}, true, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "13606")

	_, err = runPreflightChecks(sys, PlatformState{}, false, false, RoleWorker)
	assert.ErrorContains(t, err, "13606", "every worker holds Longhorn volumes")
}

func TestPreflightAllowsTheRHEL97LevelOnLocalStorageAndWarnsOnAnExistingCluster(t *testing.T) {
	sys := testSysInfo()
	sys.OS, sys.OSVersion, sys.Kernel = "rocky", "9.7", "5.14.0-611.5.1.el9_7.x86_64"

	_, err := RunPreflightChecks(sys, PlatformState{Profile: ProfileNano, Storage: StorageLocal}, false, false)
	assert.NoError(t, err)

	res, err := RunPreflightChecks(sys, PlatformState{Profile: ProfileSmall, Storage: StorageLonghorn}, true, true)
	require.NoError(t, err, "a re-run on an existing cluster is how it gets repaired")
	assert.Contains(t, strings.Join(res.Warnings, "\n"), "13606")
}

func TestGatherSystemInfoReadsTheKernelOnEL9(t *testing.T) {
	server := &routedRunner{routes: []route{
		{contains: "cat /etc/os-release", out: "ID=\"centos\"\nVERSION_ID=\"9\"\n"},
		{contains: "MemTotal", out: "8000000\n"},
		{contains: "df ", out: "36000M 40000M /\n36000M 40000M /\n"},
		{contains: "nproc", out: "2\n"},
		{contains: "uname -r", out: "5.14.0-620.el9.x86_64\n"},
	}}
	sys, err := GatherSystemInfo(server)
	require.NoError(t, err)
	assert.Equal(t, "5.14.0-620.el9.x86_64", sys.Kernel)
}

func testSysInfo() SystemInfo {
	return SystemInfo{OS: "ubuntu", OSVersion: "24.04", RAMMB: 16000, DiskMB: 200000, DiskSizeMB: 250000, ImageDiskMB: 200000, CPUs: 4}
}
