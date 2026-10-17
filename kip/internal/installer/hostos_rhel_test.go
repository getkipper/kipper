package installer

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const rocky10OSRelease = `NAME="Rocky Linux"
VERSION="10.0 (Red Quartz)"
ID="rocky"
ID_LIKE="rhel centos fedora"
VERSION_ID="10.0"
`

var (
	rhel10Host = parseOSRelease(rocky10OSRelease)
	rhel9Host  = HostOS{ID: "almalinux", VersionID: "9.6", IDLike: []string{"rhel", "centos", "fedora"}, Family: FamilyRHEL}
)

func TestParseOSReleaseRHELFamily(t *testing.T) {
	assert.Equal(t, FamilyRHEL, rhel10Host.Family)
	assert.Equal(t, "10", rhel10Host.MajorVersion())
	for _, id := range []string{"rocky", "almalinux", "centos", "rhel"} {
		assert.Equal(t, FamilyRHEL, parseOSRelease("ID="+id+"\nVERSION_ID=9\n").Family, id)
	}
	assert.Equal(t, FamilyUnknown, parseOSRelease("ID=fedora\nVERSION_ID=44\n").Family, "Fedora is not part of the supported family")
}

func TestRHELPackageCommands(t *testing.T) {
	assert.Contains(t, rhel10Host.InstallPackagesCommand(true, rhel10Host.StoragePackages()...), "dnf install -y -q --setopt=exit_on_lock=True --refresh iscsi-initiator-utils nfs-utils 2>")
	assert.Contains(t, rhel10Host.InstallPackagesCommand(false, "firewalld"), "dnf install -y -q --setopt=exit_on_lock=True firewalld 2>")
	assert.Equal(t, []string{"iscsid"}, rhel10Host.StorageServices())
	assert.Empty(t, debianHost.StorageServices(), "open-iscsi starts iscsid itself on Debian")
}

func TestHostTraits(t *testing.T) {
	assert.True(t, debianHost.UsesNeedrestart())
	assert.False(t, rhel10Host.UsesNeedrestart())
	assert.True(t, rhel10Host.K3sSELinux())
	assert.False(t, debianHost.K3sSELinux())
	assert.Equal(t, []string{"kernel-modules-extra"}, rhel10Host.KernelModulePackages())
	assert.Empty(t, rhel9Host.KernelModulePackages())
	assert.Equal(t, []string{"br_netfilter", "overlay", "iscsi_tcp", "wireguard"}, rhel10Host.KernelModules(true))
	assert.Equal(t, []string{"wireguard"}, debianHost.KernelModules(true), "host preparation requires WireGuard on Debian too")
}

// moduleHost simulates module availability before and after package installation.
type moduleHost struct {
	storageHostRunner
	available map[string]bool
	providing map[string][]string
}

func (h *moduleHost) Run(cmd string) (string, error) {
	h.commands = append(h.commands, cmd)
	switch {
	case strings.HasPrefix(cmd, "for m in"):
		var out []string
		for _, m := range strings.Fields(strings.TrimPrefix(strings.SplitN(cmd, ";", 2)[0], "for m in")) {
			if !h.available[m] {
				out = append(out, "MISSING:"+m)
			}
		}
		return strings.Join(out, "\n"), nil
	case strings.Contains(cmd, "dnf install"):
		for pkg, mods := range h.providing {
			if strings.Contains(cmd, pkg) {
				for _, m := range mods {
					h.available[m] = true
				}
			}
		}
		return "", nil
	case cmd == "uname -r":
		return "6.12.0-55.el10.x86_64\n", nil
	}
	return "", nil
}

func TestEnsureKernelModulesInstallsWhatEL10Lacks(t *testing.T) {
	h := &moduleHost{available: map[string]bool{"overlay": true, "iscsi_tcp": true, "wireguard": true}, providing: map[string][]string{"kernel-modules-extra": {"br_netfilter"}}}

	require.NoError(t, ensureKernelModules(h, rhel10Host, false, true))

	assert.Contains(t, strings.Join(h.commands, "\n"), "dnf install -y -q --setopt=exit_on_lock=True kernel-modules-extra-6.12.0-55.el10.x86_64",
		"the package must match the running kernel, or dnf installs modules for a kernel that is not booted")
}

func TestEnsureKernelModulesStopsWhenTheRunningKernelLacksOne(t *testing.T) {
	h := &moduleHost{available: map[string]bool{"overlay": true, "iscsi_tcp": true, "wireguard": true}, providing: map[string][]string{}}

	err := ensureKernelModules(h, rhel10Host, false, true)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "br_netfilter")
	assert.Contains(t, err.Error(), "reboot")
}

func TestEnsureKernelModulesReportsAFailedInstall(t *testing.T) {
	h := &failingInstallHost{moduleHost{available: map[string]bool{"overlay": true, "iscsi_tcp": true, "wireguard": true}}}

	err := ensureKernelModules(h, rhel10Host, false, true)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "installing kernel-modules-extra-6.12.0-55.el10.x86_64")
	assert.Contains(t, err.Error(), "repository unreachable")
}

type failingInstallHost struct{ moduleHost }

func (h *failingInstallHost) Run(cmd string) (string, error) {
	if strings.Contains(cmd, "dnf install") {
		return "", errors.New("repository unreachable")
	}
	return h.moduleHost.Run(cmd)
}

func TestEnsureKernelModulesLoadsWireGuardOnDebian(t *testing.T) {
	h := &moduleHost{available: map[string]bool{"wireguard": true}}
	require.NoError(t, ensureKernelModules(h, debianHost, false, true))
	assert.Contains(t, h.commands[0], `modprobe -q "$m"`, "loading verifies compatibility with the running kernel")
	assert.NotContains(t, h.commands[0], "modprobe -n")
}

func TestEnsureKernelModulesRefusesDebianWithoutWireGuard(t *testing.T) {
	h := &moduleHost{available: map[string]bool{}}
	err := ensureKernelModules(h, debianHost, false, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "wireguard")
	assert.Contains(t, err.Error(), "apt-get", "the update hint must use the host's package manager")
	assert.NotContains(t, strings.Join(h.commands, "\n"), "dnf install")
}

func TestK3sSELinuxConfigOnlyOnRHEL(t *testing.T) {
	r := &recordingRunner{}
	require.NoError(t, writeK3sHostConfig(r, rhel10Host))
	all := strings.Join(r.commands, "\n")
	assert.Contains(t, all, "/etc/rancher/k3s/config.yaml.d/kipper-selinux.yaml")
	assert.Contains(t, all, "selinux: true")

	r = &recordingRunner{}
	require.NoError(t, writeK3sHostConfig(r, debianHost))
	assert.Empty(t, r.commands)
}

func TestLonghornEnablesIscsidOnRHEL(t *testing.T) {
	r := pinHost(nil)
	require.NoError(t, installLonghorn(r, rhel10Host, nil))
	assert.Contains(t, r.commands[0], "rpm -q --quiet iscsi-initiator-utils nfs-utils")
	assert.Contains(t, r.commands[1], "dnf install -y -q --setopt=exit_on_lock=True --refresh iscsi-initiator-utils nfs-utils")
	assert.Equal(t, "systemctl enable --now iscsid", r.commands[2])
}

func TestStorageRestartsSkipRHEL(t *testing.T) {
	r := &storageHostRunner{replies: map[string]string{"/etc/os-release": rocky10OSRelease}}
	require.NoError(t, ConfigureStorageRestarts(r))
	for _, c := range r.commands {
		assert.NotContains(t, c, "needrestart", c)
	}
}

func TestPendingRestartsOnRHEL(t *testing.T) {
	r := &storageHostRunner{replies: map[string]string{
		"/etc/os-release":  rocky10OSRelease,
		"needs-restarting": "SVC:iscsid.service\nSVC:sshd.service\nREBOOT:yes\n",
	}}

	pending, err := PendingRestarts(r)

	require.NoError(t, err)
	assert.Equal(t, []string{"iscsid.service"}, pending.Deferred, "storage units are named apart")
	assert.Equal(t, []string{"sshd.service"}, pending.Other)
	assert.True(t, pending.RebootRequired)
	assert.False(t, pending.KernelOutdated, "dnf's hint also covers glibc and systemd, so it does not claim a new kernel")
	assert.True(t, pending.Any())
}

func TestPendingRestartsOnRHELWithoutTheTool(t *testing.T) {
	r := &storageHostRunner{replies: map[string]string{
		"/etc/os-release":  rocky10OSRelease,
		"needs-restarting": "NEEDS-RESTARTING-ABSENT\n",
	}}
	_, err := PendingRestarts(r)
	assert.ErrorContains(t, err, "needs-restarting")

	r.replies["needs-restarting"] = "NEEDS-RESTARTING-FAILED\n"
	_, err = PendingRestarts(r)
	assert.ErrorContains(t, err, "exited with an error")
}

func TestK3sEntryPointsPrepareTheHost(t *testing.T) {
	unreadable := map[string]error{"/etc/os-release": errors.New("connection reset")}

	err := InstallK3s(&storageHostRunner{fail: unreadable}, "203.0.113.10", nil, false, false, StorageLonghorn)
	assert.ErrorContains(t, err, "preparing the host for k3s")

	_, err = PrepareWorker(&storageHostRunner{}, &storageHostRunner{fail: unreadable}, "")
	assert.ErrorContains(t, err, "preparing the host for k3s")
}

// Exercise the shell's handling of exit code 1 for both a reboot hint and an error.
func TestDNFNeedsRestartingShell(t *testing.T) {
	cases := []struct {
		name, rebootOut, rebootRC, want string
	}{
		{"reboot needed", "Reboot is required to fully utilize these updates.", "1", "SVC:sshd.service\nREBOOT:yes\n"},
		{"no reboot", "Reboot should not be necessary.", "0", "SVC:sshd.service\n"},
		{"dnf error exits 1", "Error: rpmdb open failed", "1", "SVC:sshd.service\nNEEDS-RESTARTING-FAILED\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			fake := "#!/bin/sh\n" +
				"case \"$*\" in\n" +
				"  *--help*) exit 0 ;;\n" +
				"  *-s*) echo sshd.service ;;\n" +
				"  *-r*) echo '" + tc.rebootOut + "'; exit " + tc.rebootRC + " ;;\n" +
				"esac\n"
			//nolint:gosec // the fake dnf has to be executable
			require.NoError(t, os.WriteFile(filepath.Join(dir, "dnf"), []byte(fake), 0o755))

			cmd := exec.Command("bash", "-c", dnfNeedsRestartingCommand)
			cmd.Env = []string{"PATH=" + dir + ":/usr/bin:/bin"}
			out, err := cmd.Output()

			require.NoError(t, err)
			assert.Equal(t, tc.want, string(out))
		})
	}
}

func TestEnsureKernelModulesToleratesOnlyAnOptionalWireGuard(t *testing.T) {
	h := &failingInstallHost{moduleHost{available: map[string]bool{"br_netfilter": true, "overlay": true, "iscsi_tcp": true}}}
	require.NoError(t, ensureKernelModules(h, rhel10Host, true, true), "a failed install that only concerned wireguard must not stop a VXLAN server")

	h = &failingInstallHost{moduleHost{available: map[string]bool{"overlay": true, "iscsi_tcp": true}}}
	err := ensureKernelModules(h, rhel10Host, true, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "br_netfilter")
}

// Local storage has no iSCSI volumes, so a RHEL host that lacks only the iSCSI
// module installs without fetching a storage-only module package.
func TestEnsureKernelModulesLeavesISCSIToLonghorn(t *testing.T) {
	h := &moduleHost{available: map[string]bool{"br_netfilter": true, "overlay": true, "wireguard": true}, providing: map[string][]string{}}
	require.NoError(t, ensureKernelModules(h, rhel10Host, false, false))
	assert.NotContains(t, strings.Join(h.commands, "\n"), "dnf install", "no package is installed for a module local storage never uses")

	h = &moduleHost{available: map[string]bool{"br_netfilter": true, "overlay": true, "wireguard": true}, providing: map[string][]string{}}
	assert.ErrorContains(t, ensureKernelModules(h, rhel10Host, false, true), "iscsi_tcp", "Longhorn still needs it")

	assert.Equal(t, []string{"br_netfilter", "overlay", "wireguard"}, rhel10Host.KernelModules(false))
}
