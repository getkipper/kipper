package installer

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRunPreflightChecks(t *testing.T) {
	tests := []struct {
		name                string
		system              SystemInfo
		profile             string
		externalBackup      bool
		wantError           string
		wantWarningContains string
	}{
		{
			name: "external backup storage leaves MinIO out of the volume budget",
			system: SystemInfo{
				OS: "ubuntu", OSVersion: "22.04",
				RAMMB: 8192, DiskMB: 22000, DiskSizeMB: 25000,
			},
			externalBackup: true,
		},
		{
			name: "fails at exactly 25% free, which Longhorn still refuses",
			system: SystemInfo{
				OS: "ubuntu", OSVersion: "22.04",
				RAMMB: 8192, DiskMB: 19216, DiskSizeMB: 40000,
			},
			wantError: "insufficient disk: 19216 MiB free on the filesystem holding /var/lib/longhorn. After about 9216 MiB of images and system data, Longhorn needs more than 25% of the filesystem (10000 MiB) kept free. Free up space or use a larger disk",
		},
		{
			name: "a re-run charges no image footprint, because the images are already on the disk",
			system: SystemInfo{
				OS: "ubuntu", OSVersion: "22.04",
				RAMMB: 8192, DiskMB: 12000, DiskSizeMB: 40000, ImagesPresentMB: 9500,
			},
		},
		{
			name: "a resumed install is charged only for the images still missing",
			system: SystemInfo{
				OS: "ubuntu", OSVersion: "22.04",
				RAMMB: 8192, DiskMB: 12000, DiskSizeMB: 40000, ImagesPresentMB: 3000,
			},
			wantError: "insufficient disk: 12000 MiB free on the filesystem holding /var/lib/longhorn. After about 6216 MiB of images and system data, Longhorn needs more than 25% of the filesystem (10000 MiB) kept free. Free up space or use a larger disk",
		},
		{
			name: "a separate Longhorn disk keeps the image footprint on the filesystem holding k3s's data",
			system: SystemInfo{
				OS: "ubuntu", OSVersion: "22.04",
				RAMMB: 8192, DiskMB: 30000, DiskSizeMB: 40000, SeparateImageDisk: true, ImageDiskMB: 5000,
			},
			wantError: "insufficient disk: 5000 MiB free on the filesystem holding k3s's data (/var/lib/rancher/k3s). Images and system data still need about 9216 MiB. Free up space or use a larger disk",
		},
		{
			name: "a separate Longhorn disk passes when both filesystems have their room",
			system: SystemInfo{
				OS: "ubuntu", OSVersion: "22.04",
				RAMMB: 8192, DiskMB: 11000, DiskSizeMB: 40000, SeparateImageDisk: true, ImageDiskMB: 12000,
			},
		},
		{
			name: "warns that a forced profile leaves no room for a default database service",
			system: SystemInfo{
				OS: "ubuntu", OSVersion: "22.04",
				RAMMB: 8192, DiskMB: 40960, CPUs: 2,
			},
			profile:             ProfileMedium,
			wantWarningContains: "500m",
		},
		{
			name: "warns on a single CPU where even nano leaves no room for a default database service",
			system: SystemInfo{
				OS: "ubuntu", OSVersion: "22.04",
				RAMMB: 8192, DiskMB: 40960, CPUs: 1,
			},
			wantWarningContains: "1 CPU:",
		},
		{
			name: "says nothing about CPU when the profile fits",
			system: SystemInfo{
				OS: "ubuntu", OSVersion: "22.04",
				RAMMB: 8192, DiskMB: 40960, CPUs: 2,
			},
			profile: ProfileSmall,
		},
		{
			name: "a forced larger profile on a small host warns about monitoring memory use",
			system: SystemInfo{
				OS: "ubuntu", OSVersion: "22.04",
				RAMMB: 3900, DiskMB: 40960, CPUs: 4,
			},
			profile:             ProfileMedium,
			wantWarningContains: "'medium' profile enables monitoring by default, leaving less memory for apps.",
		},
		{
			name: "a forced larger profile below 4 GB names the risk without calling it nano",
			system: SystemInfo{
				OS: "ubuntu", OSVersion: "22.04",
				RAMMB: 3000, DiskMB: 40960, CPUs: 4,
			},
			profile:             ProfileSmall,
			wantWarningContains: "The 'small' profile enables monitoring (Prometheus, Loki, Grafana) by default, which can push the host into swapping, and the install itself can fail at Zot.",
		},
		{
			name: "nano on a 6 GB host does not claim the RAM is below 4 GB",
			system: SystemInfo{
				OS: "ubuntu", OSVersion: "22.04",
				RAMMB: 6000, DiskMB: 40960, CPUs: 4,
			},
			profile:             ProfileNano,
			wantWarningContains: "RAM is below recommended 8GB (6000MB available): the 'nano' profile keeps monitoring",
		},
		{
			name: "the small profile warning suggests nano to reduce memory use",
			system: SystemInfo{
				OS: "ubuntu", OSVersion: "22.04",
				RAMMB: 3900, DiskMB: 40960, CPUs: 2,
			},
			wantWarningContains: "--profile nano",
		},
		{
			name: "passes with sufficient resources on ubuntu 22.04",
			system: SystemInfo{
				OS: "ubuntu", OSVersion: "22.04",
				RAMMB: 8192, DiskMB: 40960,
			},
		},
		{
			name: "passes with sufficient resources on debian 12",
			system: SystemInfo{
				OS: "debian", OSVersion: "12",
				RAMMB: 8192, DiskMB: 40960,
			},
		},
		{
			name: "passes at exact 2048 MB floor with nano warning",
			system: SystemInfo{
				OS: "ubuntu", OSVersion: "22.04",
				RAMMB: 2048, DiskMB: 40960,
			},
			wantWarningContains: "'nano' profile",
		},
		{
			name: "warns nano-style in the 3 GB range",
			system: SystemInfo{
				OS: "ubuntu", OSVersion: "22.04",
				RAMMB: 3000, DiskMB: 40960,
			},
			wantWarningContains: "RAM is below 4GB (3000MB available)",
		},
		{
			name: "warns small-style for a 4 GB host",
			system: SystemInfo{
				OS: "ubuntu", OSVersion: "20.04",
				RAMMB: 4096, DiskMB: 40960,
			},
			wantWarningContains: "'small' profile",
		},
		{
			name: "accepts debian 13",
			system: SystemInfo{
				OS: "debian", OSVersion: "13",
				RAMMB: 8192, DiskMB: 40960,
			},
		},
		{
			name: "accepts debian 11 with a warning that its standard support has ended",
			system: SystemInfo{
				OS: "debian", OSVersion: "11",
				RAMMB: 8192, DiskMB: 40960,
			},
			wantWarningContains: "Debian 11 is past the end of its standard support",
		},
		{
			name: "accepts ubuntu 20.04 with a warning that its standard support has ended",
			system: SystemInfo{
				OS: "ubuntu", OSVersion: "20.04",
				RAMMB: 8192, DiskMB: 40960,
			},
			wantWarningContains: "Ubuntu 20.04 is past the end of its standard support",
		},
		{
			name: "fails with unsupported OS",
			system: SystemInfo{
				OS: "fedora", OSVersion: "44",
				RAMMB: 8192, DiskMB: 40960,
			},
			wantError: "unsupported OS: fedora (supported: ubuntu, debian, rocky, almalinux, centos)",
		},
		{
			name: "fails with unsupported OS version",
			system: SystemInfo{
				OS: "ubuntu", OSVersion: "18.04",
				RAMMB: 8192, DiskMB: 40960,
			},
			wantError: "unsupported ubuntu version: 18.04 (supported: 20.04, 22.04, 24.04, 26.04)",
		},
		{
			name: "accepts ubuntu 26.04",
			system: SystemInfo{
				OS: "ubuntu", OSVersion: "26.04",
				RAMMB: 8192, DiskMB: 40960,
			},
		},
		{
			name: "fails when RAM is below the 2048 MB floor",
			system: SystemInfo{
				OS: "ubuntu", OSVersion: "22.04",
				RAMMB: 1024, DiskMB: 40960,
			},
			wantError: "insufficient RAM: 1024MB available, 2048MB required",
		},
		{
			name: "fails when 70% of the filesystem cannot hold the system volumes",
			system: SystemInfo{
				OS: "ubuntu", OSVersion: "22.04",
				RAMMB: 8192, DiskMB: 22000, DiskSizeMB: 25000,
			},
			wantError: "insufficient disk: the filesystem holding /var/lib/longhorn is 25000 MiB. Longhorn places Kipper's system volumes (20480 MiB on the 'medium' profile) in 70% of it, so it must be at least 29258 MiB. Use a server with a larger disk",
		},
		{
			name: "a 40 GB disk passes on the small profile",
			system: SystemInfo{
				OS: "ubuntu", OSVersion: "22.04",
				RAMMB: 8192, DiskMB: 36000, DiskSizeMB: 40000,
			},
		},
		{
			name: "a 25 GB disk passes on nano, whose system volumes are smaller",
			system: SystemInfo{
				OS: "ubuntu", OSVersion: "22.04",
				RAMMB: 8192, DiskMB: 22000, DiskSizeMB: 25000,
			},
			profile: ProfileNano,
		},
		{
			name: "fails when a large but full filesystem would drop below Longhorn's 25% free",
			system: SystemInfo{
				OS: "ubuntu", OSVersion: "22.04",
				RAMMB: 8192, DiskMB: 30000, DiskSizeMB: 200000,
			},
			wantError: "insufficient disk: 30000 MiB free on the filesystem holding /var/lib/longhorn. After about 9216 MiB of images and system data, Longhorn needs more than 25% of the filesystem (50000 MiB) kept free. Free up space or use a larger disk",
		},
		{
			name: "fails when another service holds a port",
			system: SystemInfo{
				OS: "ubuntu", OSVersion: "22.04",
				RAMMB: 8192, DiskMB: 40960,
				PortListeners: map[int][]string{443: {"nginx"}},
			},
			wantError: "required ports already in use: 443 (nginx). Stop the service holding them, or install on a clean server",
		},
		{
			name: "names every held port and its process",
			system: SystemInfo{
				OS: "ubuntu", OSVersion: "22.04",
				RAMMB: 8192, DiskMB: 40960,
				PortListeners: map[int][]string{80: {"nginx"}, 443: {"nginx"}, 6443: {""}},
			},
			wantError: "required ports already in use: 80 (nginx), 443 (nginx), 6443 (an unidentified process). Stop the service holding them, or install on a clean server",
		},
		{
			name: "passes when k3s already holds the API port",
			system: SystemInfo{
				OS: "ubuntu", OSVersion: "22.04",
				RAMMB: 8192, DiskMB: 40960,
				PortListeners: map[int][]string{6443: {"k3s-server"}},
			},
		},
		{
			name: "passes when an older k3s holds the API port",
			system: SystemInfo{
				OS: "ubuntu", OSVersion: "22.04",
				RAMMB: 8192, DiskMB: 40960,
				PortListeners: map[int][]string{6443: {"k3s"}},
			},
		},
		{
			name: "fails when a process only resembles k3s",
			system: SystemInfo{
				OS: "ubuntu", OSVersion: "22.04",
				RAMMB: 8192, DiskMB: 40960,
				PortListeners: map[int][]string{443: {"k3s-proxy"}},
			},
			wantError: "required ports already in use: 443 (k3s-proxy). Stop the service holding them, or install on a clean server",
		},
		{
			name: "fails when k3s shares a port with another process",
			system: SystemInfo{
				OS: "ubuntu", OSVersion: "22.04",
				RAMMB: 8192, DiskMB: 40960,
				PortListeners: map[int][]string{6443: {"k3s-server", "haproxy"}},
			},
			wantError: "required ports already in use: 6443 (haproxy). Stop the service holding them, or install on a clean server",
		},
		{
			name: "no warning at exactly the recommended floor",
			system: SystemInfo{
				OS: "ubuntu", OSVersion: "22.04",
				RAMMB: 8192, DiskMB: 40960,
			},
		},
		{
			name: "no warning on a 64 GB cluster node",
			system: SystemInfo{
				OS: "ubuntu", OSVersion: "22.04",
				RAMMB: 64253, DiskMB: 1024000,
			},
		},
		{
			name: "reports first failing check when multiple fail",
			system: SystemInfo{
				OS: "windows", OSVersion: "11",
				RAMMB: 512, DiskMB: 1024,
			},
			wantError: "unsupported OS: windows (supported: ubuntu, debian, rocky, almalinux, centos)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sys := tt.system
			if sys.CPUs == 0 {
				sys.CPUs = 4
			}
			if sys.DiskSizeMB == 0 {
				sys.DiskSizeMB = sys.DiskMB + 4096
			}
			profile := tt.profile
			if profile == "" {
				profile = pickProfile(sys.RAMMB, sys.CPUs)
			}
			result, err := RunPreflightChecks(sys, PlatformState{Profile: profile}, !tt.externalBackup, false)
			if tt.wantError != "" {
				assert.EqualError(t, err, tt.wantError)
				return
			}
			assert.NoError(t, err)
			if tt.wantWarningContains == "" {
				assert.Empty(t, result.Warnings, "expected no warnings")
				return
			}
			found := false
			for _, w := range result.Warnings {
				if strings.Contains(w, tt.wantWarningContains) {
					found = true
					break
				}
			}
			assert.Truef(t, found, "expected warning containing %q, got %v",
				tt.wantWarningContains, result.Warnings)
		})
	}
}

// Worker preflight budgets free space without reserving server system volumes.
func TestWorkerPreflightSkipsTheSystemVolumeRule(t *testing.T) {
	sys := SystemInfo{OS: "rocky", OSVersion: "10.2", RAMMB: 3000, DiskMB: 22000, DiskSizeMB: 25000, CPUs: 2}
	_, err := runPreflightChecks(sys, PlatformState{}, false, false, RoleWorker)
	assert.NoError(t, err)

	sys = SystemInfo{OS: "rocky", OSVersion: "10.2", RAMMB: 3000, DiskMB: 9000, DiskSizeMB: 40000, CPUs: 2}
	_, err = runPreflightChecks(sys, PlatformState{}, false, false, RoleWorker)
	assert.ErrorContains(t, err, "25% of the filesystem")
}

func TestSystemVolumesFollowTheManifestConstants(t *testing.T) {
	assert.Equal(t, 15360, systemVolumesMB(PlatformState{Profile: ProfileNano}, true), "MinIO 10Gi + Zot 5Gi")
	assert.Equal(t, 20480, systemVolumesMB(PlatformState{Profile: ProfileSmall}, true), "plus Loki 5Gi")
	assert.Equal(t, 20480, systemVolumesMB(PlatformState{Profile: ProfileXLarge}, true))
	assert.Equal(t, 10240, systemVolumesMB(PlatformState{Profile: ProfileSmall}, false), "external backup storage installs no MinIO")
}

// Saved component overrides determine which volumes installation needs.
func TestSystemVolumesFollowTheSavedLokiOverride(t *testing.T) {
	off := PlatformState{Profile: ProfileSmall, EnabledOverrides: map[string]bool{"loki": false}}
	assert.Equal(t, 15360, systemVolumesMB(off, true), "small with Loki disabled creates no Loki volume")
	on := PlatformState{Profile: ProfileNano, EnabledOverrides: map[string]bool{"loki": true}}
	assert.Equal(t, 20480, systemVolumesMB(on, true), "nano with Loki enabled creates one")

	sys := SystemInfo{OS: "ubuntu", OSVersion: "22.04", RAMMB: 8192, DiskMB: 22000, DiskSizeMB: 25000, CPUs: 4}
	_, err := RunPreflightChecks(sys, off, true, false)
	assert.NoError(t, err, "a 25 GB disk carries small without Loki")
	_, err = RunPreflightChecks(sys, PlatformState{Profile: ProfileSmall}, true, false)
	assert.ErrorContains(t, err, "20480 MiB")
}

func TestDiskShortageOnAnExistingClusterIsAWarning(t *testing.T) {
	sys := SystemInfo{OS: "ubuntu", OSVersion: "22.04", RAMMB: 8192, DiskMB: 8000, DiskSizeMB: 40000,
		CPUs: 4, ImagesPresentMB: 9216}
	state := PlatformState{Profile: ProfileSmall}

	_, err := RunPreflightChecks(sys, state, true, false)
	assert.ErrorContains(t, err, "insufficient disk", "a fresh install on this disk is refused")

	result, err := RunPreflightChecks(sys, state, true, true)
	if assert.NoError(t, err) && assert.NotEmpty(t, result.Warnings) {
		assert.Contains(t, result.Warnings[0], "insufficient disk")
		assert.Contains(t, result.Warnings[0], "new volumes")
	}
}

func TestWorkerPreflightChecksTheKubeletPortOnly(t *testing.T) {
	sys := SystemInfo{OS: "rocky", OSVersion: "10.2", RAMMB: 3000, DiskMB: 40960, DiskSizeMB: 45000,
		PortListeners: map[int][]string{80: {"nginx"}, 10250: {"k3s-agent"}}}
	result, err := runPreflightChecks(sys, PlatformState{}, false, false, RoleWorker)
	assert.NoError(t, err)
	assert.Empty(t, result.Warnings)

	sys.PortListeners = map[int][]string{10250: {"kubelet"}}
	_, err = runPreflightChecks(sys, PlatformState{}, false, false, RoleWorker)
	assert.ErrorContains(t, err, "10250 (kubelet)")

	sys.PortListeners = map[int][]string{80: {"nginx"}}
	_, err = runPreflightChecks(sys, PlatformState{Profile: ProfileNano}, true, false, RoleServer)
	assert.ErrorContains(t, err, "80 (nginx)")
}
