package installer

import (
	"fmt"
	"strings"
)

// Family groups distributions by their package management commands.
type Family string

const (
	FamilyDebian  Family = "debian"
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
	"debian": FamilyDebian,
	"ubuntu": FamilyDebian,
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

// InstallPackagesCommand returns a noninteractive install command, refreshing
// package lists first when refresh is true. It returns an empty string for
// unsupported families. Package names must be safe as unquoted shell arguments.
func (h HostOS) InstallPackagesCommand(refresh bool, pkgs ...string) string {
	if h.Family != FamilyDebian {
		return ""
	}
	install := "DEBIAN_FRONTEND=noninteractive apt-get install -y -qq " + strings.Join(pkgs, " ")
	if refresh {
		return "DEBIAN_FRONTEND=noninteractive apt-get update -qq && " + install
	}
	return install
}

// StoragePackages returns Longhorn's iSCSI and NFS client packages, or nil for
// unsupported families.
func (h HostOS) StoragePackages() []string {
	if h.Family != FamilyDebian {
		return nil
	}
	return []string{"open-iscsi", "nfs-common"}
}
