package installer

import (
	"regexp"
	"strconv"
	"strings"
)

// longhornRHEL97Hold explains why kip keeps RHEL 9.7-level hosts out of
// Longhorn storage. It lifts when kip adopts a Longhorn release whose notes
// list the fix for issue 13606.
const longhornRHEL97Hold = "Longhorn issue 13606: on Rocky Linux 9.7 Longhorn's v1 volumes stop attaching, and no Longhorn release carries a fix yet. " +
	"kip holds every host at that level or later (Rocky Linux or AlmaLinux 9.7 or a later 9.x, and any EL9 kernel from 5.14.0-611), because what triggers it is unknown"

var (
	el9KernelPattern = regexp.MustCompile(`^5\.14\.0-(\d+)\..*el9`)
	osImagePattern   = regexp.MustCompile(`^(Rocky Linux|AlmaLinux|Red Hat Enterprise Linux) (\d+)\.(\d+)`)
)

// RHEL97LevelHost reports whether a host with this os-release ID and
// VERSION_ID and running this kernel is at the RHEL 9.7 package level or
// later: Rocky Linux, AlmaLinux or RHEL 9.7 or a later 9.x, or any EL9 kernel from
// the 5.14.0-611 series on, which also covers CentOS Stream 9.
func RHEL97LevelHost(id, versionID, kernel string) bool {
	switch strings.ToLower(id) {
	case "rocky", "almalinux", "rhel":
		major, minor, ok := strings.Cut(versionID, ".")
		if ok && major == "9" {
			if n, err := strconv.Atoi(minor); err == nil && n >= 7 {
				return true
			}
		}
	}
	return el9KernelAtLeast611(kernel)
}

// RHEL97LevelNode applies RHEL97LevelHost to a Kubernetes Node's OS image
// name and kernel version.
func RHEL97LevelNode(n NodeFacts) bool {
	if m := osImagePattern.FindStringSubmatch(n.OSImage); len(m) == 4 && m[2] == "9" {
		if minor, err := strconv.Atoi(m[3]); err == nil && minor >= 7 {
			return true
		}
	}
	return el9KernelAtLeast611(n.Kernel)
}

func el9KernelAtLeast611(kernel string) bool {
	m := el9KernelPattern.FindStringSubmatch(kernel)
	if m == nil {
		return false
	}
	build, err := strconv.Atoi(m[1])
	return err == nil && build >= 611
}
