package installer

import (
	"io"
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
	assert.ErrorContains(t, configureFirewallWith(firewallOnly{unreadable}, false), "could not determine OS")
}

type firewallOnly struct{ osReleaseRunner }

func (firewallOnly) SurvivesSSHRateLimit() bool { return true }

func (firewallOnly) ConfirmFreshConnection() error { return nil }
