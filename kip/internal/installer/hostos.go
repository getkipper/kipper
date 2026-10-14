package installer

import (
	"fmt"
	"strings"
)

// Family groups distributions by their package management commands.
type Family string

const (
	FamilyDebian  Family = "debian"
	FamilyRHEL    Family = "rhel"
	FamilyUnknown Family = ""
)

// HostOS describes a host's distribution and package management family.
// Preflight checks determine which distributions and versions are supported.
type HostOS struct {
	ID        string
	VersionID string
	IDLike    []string
	Family    Family
}

// familyByID maps an os-release ID, or an ID_LIKE entry, to its family.
var familyByID = map[string]Family{
	"debian":    FamilyDebian,
	"ubuntu":    FamilyDebian,
	"rhel":      FamilyRHEL,
	"rocky":     FamilyRHEL,
	"almalinux": FamilyRHEL,
	"centos":    FamilyRHEL,
}

// DetectHostOS reads the host's /etc/os-release.
func DetectHostOS(client CommandRunner) (HostOS, error) {
	out, err := client.Run("cat /etc/os-release")
	if err != nil {
		return HostOS{}, fmt.Errorf("reading /etc/os-release: %w", err)
	}
	h := parseOSRelease(out)
	if h.ID == "" {
		return HostOS{}, fmt.Errorf("could not determine OS from /etc/os-release")
	}
	return h, nil
}

// parseOSRelease determines the family from ID, falling back to the first
// recognized ID_LIKE entry.
func parseOSRelease(content string) HostOS {
	var h HostOS
	for _, line := range strings.Split(content, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		value = strings.Trim(value, `"'`)
		switch key {
		case "ID":
			h.ID = strings.ToLower(value)
		case "VERSION_ID":
			h.VersionID = value
		case "ID_LIKE":
			h.IDLike = strings.Fields(strings.ToLower(value))
		}
	}
	h.Family = familyByID[h.ID]
	for _, like := range h.IDLike {
		if h.Family != FamilyUnknown {
			break
		}
		h.Family = familyByID[like]
	}
	return h
}

// MajorVersion returns VERSION_ID up to its first dot.
func (h HostOS) MajorVersion() string {
	major, _, _ := strings.Cut(h.VersionID, ".")
	return major
}

// InstallPackagesCommand returns a noninteractive install command, refreshing
// package lists first when refresh is true. It returns an empty string for
// unsupported families. Package names must be safe as unquoted shell arguments.
func (h HostOS) InstallPackagesCommand(refresh bool, pkgs ...string) string {
	switch h.Family {
	case FamilyDebian:
		install := "DEBIAN_FRONTEND=noninteractive apt-get install -y -qq " + strings.Join(pkgs, " ")
		if refresh {
			return "DEBIAN_FRONTEND=noninteractive apt-get update -qq && " + install
		}
		return install
	case FamilyRHEL:
		install := "dnf install -y -q --setopt=exit_on_lock=True "
		if refresh {
			install += "--refresh "
		}
		install += strings.Join(pkgs, " ")
		// Retry lock errors (exit 200) up to 60 attempts, waiting ten seconds between attempts.
		// Buffer stderr so only the final attempt's diagnostics are shown.
		return "(n=0; e=$(mktemp) || exit 1; trap 'rm -f \"$e\"' EXIT; while :; do " + install + " 2>\"$e\" && { cat \"$e\" >&2; exit 0; }; rc=$?; " +
			"[ \"$rc\" -eq 200 ] || { cat \"$e\" >&2; exit \"$rc\"; }; n=$((n+1)); if [ \"$n\" -ge 60 ]; then cat \"$e\" >&2; " +
			"echo 'dnf is still locked by another package manager after 60 attempts. Wait for that process to finish, then run kip install again.' >&2; exit 200; fi; sleep 10; done)"
	}
	return ""
}

// PackagesInstalledCommand returns a command that prints "present" when every
// package is installed. The command exits successfully even if packages are missing.
// It returns an empty string for unsupported families. Package names must be safe
// as unquoted shell arguments.
func (h HostOS) PackagesInstalledCommand(pkgs ...string) string {
	switch h.Family {
	case FamilyDebian:
		return "(for p in " + strings.Join(pkgs, " ") + "; do [ \"$(dpkg-query -W -f '${db:Status-Status}' \"$p\" 2>/dev/null)\" = installed ] || exit 1; done) && echo present || true"
	case FamilyRHEL:
		return "rpm -q --quiet " + strings.Join(pkgs, " ") + " && echo present || true"
	}
	return ""
}

func packageInstallNotice(pkgs []string) string {
	return fmt.Sprintf("  ·  Installing %s from the server's package mirrors. This may take a few minutes.\n", strings.Join(pkgs, " "))
}

// StoragePackages returns Longhorn's iSCSI and NFS client packages, or nil for
// unsupported families.
func (h HostOS) StoragePackages() []string {
	switch h.Family {
	case FamilyDebian:
		return []string{"open-iscsi", "nfs-common"}
	case FamilyRHEL:
		return []string{"iscsi-initiator-utils", "nfs-utils"}
	}
	return nil
}

// StorageServices returns the services Kipper explicitly enables for Longhorn.
func (h HostOS) StorageServices() []string {
	if h.Family == FamilyRHEL {
		return []string{"iscsid"}
	}
	return nil
}

// UsesNeedrestart reports whether Kipper configures needrestart for this OS family.
func (h HostOS) UsesNeedrestart() bool {
	return h.Family == FamilyDebian
}

// K3sSELinux reports whether Kipper enables SELinux support in k3s.
func (h HostOS) K3sSELinux() bool {
	return h.Family == FamilyRHEL
}

// KernelModules returns the modules to check before installing k3s and Longhorn.
func (h HostOS) KernelModules() []string {
	if h.Family == FamilyRHEL {
		return []string{"br_netfilter", "overlay", "vxlan", "iscsi_tcp"}
	}
	return nil
}

// KernelModulePackages returns extra packages to install when required modules
// are missing. The caller must select versions matching the running kernel.
func (h HostOS) KernelModulePackages() []string {
	if h.Family == FamilyRHEL && h.MajorVersion() == "10" {
		return []string{"kernel-modules-extra"}
	}
	return nil
}
