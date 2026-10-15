package installer

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var debianHost = HostOS{ID: "debian", VersionID: "13", Family: FamilyDebian}

func TestParseOSRelease(t *testing.T) {
	h := parseOSRelease(`PRETTY_NAME="Ubuntu 26.04 LTS"
NAME="Ubuntu"
VERSION_ID="26.04"
ID=ubuntu
ID_LIKE=debian
`)
	assert.Equal(t, HostOS{ID: "ubuntu", VersionID: "26.04", IDLike: []string{"debian"}, Family: FamilyDebian}, h)

	h = parseOSRelease("ID=debian\nVERSION_ID=\"13\"\n")
	assert.Equal(t, FamilyDebian, h.Family)
	assert.Equal(t, "13", h.VersionID)

	h = parseOSRelease("ID=\"pop\"\nID_LIKE=\"ubuntu debian\"\nVERSION_ID=22.04\n")
	assert.Equal(t, FamilyDebian, h.Family, "ID_LIKE maps a derivative to its family")
	assert.Equal(t, []string{"ubuntu", "debian"}, h.IDLike)

	h = parseOSRelease("ID=arch\n")
	assert.Equal(t, FamilyUnknown, h.Family)

	assert.Equal(t, FamilyUnknown, parseOSRelease("").Family)
}

func TestDebianPackageCommands(t *testing.T) {
	h := HostOS{ID: "debian", VersionID: "13", Family: FamilyDebian}

	assert.Equal(t, "DEBIAN_FRONTEND=noninteractive apt-get install -y -qq ufw", h.InstallPackagesCommand(false, "ufw"))
	assert.Equal(t, "DEBIAN_FRONTEND=noninteractive apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq open-iscsi nfs-common",
		h.InstallPackagesCommand(true, h.StoragePackages()...))
}

func TestUnknownFamilyHasNoPackageCommand(t *testing.T) {
	h := HostOS{ID: "arch", Family: FamilyUnknown}
	assert.Empty(t, h.InstallPackagesCommand(true, "ufw"))
	assert.Empty(t, h.StoragePackages())
}

type osReleaseRunner struct{ out string }

func (r osReleaseRunner) Run(cmd string) (string, error) {
	if strings.Contains(cmd, "/etc/os-release") {
		return r.out, nil
	}
	return "", nil
}
func (r osReleaseRunner) RunStdin(cmd string, _ io.Reader) (string, error) { return r.Run(cmd) }

func TestDetectHostOS(t *testing.T) {
	h, err := DetectHostOS(osReleaseRunner{out: "ID=ubuntu\nVERSION_ID=\"24.04\"\n"})
	require.NoError(t, err)
	assert.Equal(t, FamilyDebian, h.Family)

	_, err = DetectHostOS(osReleaseRunner{out: "NAME=nothing\n"})
	assert.Error(t, err, "no ID, no way to tell what the host is")
}

func TestEntryPointsStopWhenTheHostCannotBeRead(t *testing.T) {
	unreadable := osReleaseRunner{out: "NAME=nothing\n"}

	assert.ErrorContains(t, InstallLonghorn(unreadable), "could not determine OS")
	assert.ErrorContains(t, configureFirewallWith(firewallOnly{unreadable}, false, FirewallConsent{}, RoleServer), "could not determine OS")
}

type firewallOnly struct{ osReleaseRunner }

func (firewallOnly) SurvivesSSHRateLimit() bool { return true }

func (firewallOnly) ConfirmFreshConnection() error { return nil }

func TestPackageInstallNoticeNamesThePackagesAndTheWait(t *testing.T) {
	notice := packageInstallNotice([]string{"iscsi-initiator-utils", "nfs-utils"})

	assert.Contains(t, notice, "iscsi-initiator-utils nfs-utils")
	assert.Contains(t, notice, "package mirrors")
	assert.Contains(t, notice, "few minutes")
	assert.True(t, strings.HasSuffix(notice, "\n"))
}

type packageHost struct {
	present bool
	cmds    []string
}

func (h *packageHost) Run(cmd string) (string, error) {
	h.cmds = append(h.cmds, cmd)
	if (strings.HasPrefix(cmd, "command -v ") || strings.HasPrefix(cmd, "rpm -q ")) && h.present {
		return "present\n", nil
	}
	return "", nil
}

func (h *packageHost) RunStdin(cmd string, _ io.Reader) (string, error) { return h.Run(cmd) }

func TestEnsureInstalledAnnouncesOnlyARealInstall(t *testing.T) {
	missing := &packageHost{}
	var out bytes.Buffer
	assert.NoError(t, ensureInstalled(missing, "firewall-cmd", "firewalld", "dnf install -y -q firewalld", &out))
	assert.Contains(t, out.String(), "Installing firewalld")
	assert.Contains(t, strings.Join(missing.cmds, "\n"), "dnf install -y -q firewalld")

	present := &packageHost{present: true}
	out.Reset()
	assert.NoError(t, ensureInstalled(present, "firewall-cmd", "firewalld", "dnf install -y -q firewalld", &out))
	assert.Empty(t, out.String())
	assert.NotContains(t, strings.Join(present.cmds, "\n"), "dnf install")
}

func TestInstallPackagesCommandWaitsForADnfLockWithinABound(t *testing.T) {
	bin := t.TempDir()
	calls := filepath.Join(bin, "calls")
	writeFake := func(name, body string) {
		//nolint:gosec // the fake commands have to be executable
		require.NoError(t, os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body), 0o755))
	}
	run := func(cmd string) (string, error) {
		c := exec.Command("sh", "-c", cmd+" >/dev/null")
		c.Env = []string{"PATH=" + bin + ":/usr/bin:/bin"}
		out, err := c.CombinedOutput()
		return string(out), err
	}
	count := func() int {
		got, _ := os.ReadFile(calls)
		return strings.Count(string(got), "x")
	}
	writeFake("sleep", "exit 0\n")
	cmd := HostOS{Family: FamilyRHEL}.InstallPackagesCommand(false, "firewalld")
	assert.Contains(t, cmd, "--setopt=exit_on_lock=True")

	writeFake("dnf", "echo x >> "+calls+"\n[ $(wc -l < "+calls+") -ge 3 ] && exit 0\nexit 200\n")
	out, err := run(cmd)
	require.NoError(t, err, out)
	assert.Equal(t, 3, count(), "dnf was retried until the lock was free")

	require.NoError(t, os.WriteFile(calls, nil, 0o600))
	writeFake("dnf", "echo x >> "+calls+"\nexit 200\n")
	out, err = run(cmd)
	require.Error(t, err)
	assert.Contains(t, out, "another package manager")

	require.NoError(t, os.WriteFile(calls, nil, 0o600))
	writeFake("dnf", "echo x >> "+calls+"\necho 'metadata already locked by 2297' >&2\nexit 200\n")
	out, err = run(cmd)
	require.Error(t, err)
	assert.Equal(t, 1, strings.Count(out, "already locked"), "only the last attempt's lock message is shown")

	require.NoError(t, os.WriteFile(calls, nil, 0o600))
	writeFake("dnf", "echo x >> "+calls+"\necho 'No match for argument: firewalld' >&2\nexit 1\n")
	out, err = run(cmd)
	require.Error(t, err)
	assert.Equal(t, 1, count(), "other dnf failures are not retried")
	assert.Contains(t, out, "No match for argument", "other dnf failures keep their message")

	refreshed := HostOS{Family: FamilyRHEL}.InstallPackagesCommand(true, "firewalld")
	assert.Contains(t, refreshed, "--refresh")
	assert.Contains(t, refreshed, "--setopt=exit_on_lock=True")
}

func TestConfigureFirewallSkipsInstallingUFWThatIsThere(t *testing.T) {
	h := &packageHost{present: true}
	var out bytes.Buffer
	require.NoError(t, ensureInstalled(h, "ufw", "ufw", "apt-get install -y -qq ufw", &out))
	assert.Empty(t, out.String())
	assert.NotContains(t, strings.Join(h.cmds, "\n"), "apt-get install")
}

func TestStoragePackagesAreInstalledOnlyWhenOneIsMissing(t *testing.T) {
	rocky := HostOS{ID: "rocky", Family: FamilyRHEL}

	missing := &packageHost{}
	var out bytes.Buffer
	require.NoError(t, installStoragePackages(missing, rocky, &out))
	assert.Contains(t, out.String(), "Installing iscsi-initiator-utils nfs-utils")
	assert.Contains(t, strings.Join(missing.cmds, "\n"), "dnf install")

	present := &packageHost{present: true}
	out.Reset()
	require.NoError(t, installStoragePackages(present, rocky, &out))
	assert.Empty(t, out.String())
	assert.NotContains(t, strings.Join(present.cmds, "\n"), "dnf install")
}

func TestPackagesInstalledCommandChecksEveryPackage(t *testing.T) {
	assert.Equal(t, "rpm -q --quiet iscsi-initiator-utils nfs-utils && echo present || true",
		HostOS{Family: FamilyRHEL}.PackagesInstalledCommand("iscsi-initiator-utils", "nfs-utils"))
	debian := HostOS{Family: FamilyDebian}.PackagesInstalledCommand("open-iscsi", "nfs-common")
	assert.Contains(t, debian, "dpkg-query")
	assert.Contains(t, debian, "open-iscsi nfs-common")
	assert.Empty(t, HostOS{}.PackagesInstalledCommand("x"))
}

func TestDebianPackagesInstalledCommandNeedsEveryPackageInstalled(t *testing.T) {
	bin := t.TempDir()
	run := func(states string) string {
		//nolint:gosec // The test command needs execute permission.
		require.NoError(t, os.WriteFile(filepath.Join(bin, "dpkg-query"), []byte("#!/bin/sh\ncase \"$4\" in\n"+states+"\n*) exit 1 ;;\nesac\n"), 0o755))
		c := exec.Command("sh", "-c", HostOS{Family: FamilyDebian}.PackagesInstalledCommand("open-iscsi", "nfs-common"))
		c.Env = []string{"PATH=" + bin + ":/usr/bin:/bin"}
		out, err := c.CombinedOutput()
		require.NoError(t, err, string(out))
		return strings.TrimSpace(string(out))
	}

	assert.Equal(t, "present", run("open-iscsi|nfs-common) printf installed ;;"))
	assert.Empty(t, run("open-iscsi) printf installed ;;\nnfs-common) printf config-files ;;"), "a removed package with leftover config is missing")
	assert.Empty(t, run("open-iscsi) printf installed ;;"), "an unknown package is missing")
}
