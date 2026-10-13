package installer

import (
	"fmt"
	"slices"
	"strings"
)

const (
	// minRAMMB is the hard floor: k3s plus Kipper's lightest system stack
	// (Traefik, cert-manager, Dex, Zot, console, console-api, gateway) fits
	// in roughly 1 GB. 2 GB leaves headroom for a few small apps. Hosts
	// below this get rejected because k3s itself becomes unstable.
	minRAMMB         = 2048
	recommendedRAMMB = 8192
	minDiskMB        = 30720 // 30GB — a 40GB disk has ~35GB free after OS install
)

var supportedOS = map[string][]string{
	"ubuntu":    {"20.04", "22.04", "24.04", "26.04"},
	"debian":    {"11", "12", "13"},
	"rocky":     {"9", "10"},
	"almalinux": {"9", "10"},
	"centos":    {"9", "10"},
}

// majorVersionOS allows point releases within each supported major version.
// VERSION_ID may include a minor version, such as Rocky Linux 9.6.
var majorVersionOS = map[string]bool{"rocky": true, "almalinux": true, "centos": true}

// pastStandardSupportOS retains older releases for compatibility with
// existing clusters, with a warning about their support status.
var pastStandardSupportOS = map[string][]string{
	"ubuntu": {"20.04"},
	"debian": {"11"},
}

var osDisplayName = map[string]string{
	"ubuntu":    "Ubuntu",
	"debian":    "Debian",
	"rocky":     "Rocky Linux",
	"almalinux": "AlmaLinux",
	"centos":    "CentOS Stream",
}

// SystemInfo holds the remote server's system information
// gathered before installation.
type SystemInfo struct {
	Host          HostOS
	OS            string
	OSVersion     string
	RAMMB         int
	DiskMB        int
	PortListeners map[int][]string
}

// requiredPorts are the ports that must be free of other services for a Kipper cluster.
var requiredPorts = []int{80, 443, 6443}

// PreflightResult contains the outcome of preflight checks.
type PreflightResult struct {
	Warnings []string
}

// RunPreflightChecks validates that the target server meets the minimum
// requirements for a Kipper installation.
func RunPreflightChecks(sys SystemInfo) (*PreflightResult, error) {
	result := &PreflightResult{}

	if err := checkOS(sys); err != nil {
		return nil, err
	}
	if warning := pastStandardSupportWarning(sys); warning != "" {
		result.Warnings = append(result.Warnings, warning)
	}
	if err := checkRAM(sys); err != nil {
		return nil, err
	}
	if err := checkDisk(sys); err != nil {
		return nil, err
	}
	if err := checkPorts(sys); err != nil {
		return nil, err
	}

	switch pickProfile(sys.RAMMB) {
	case ProfileNano:
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("RAM is below 4GB (%dMB available): the 'nano' profile will be selected: monitoring (Prometheus, Loki, Grafana) is disabled by default and headroom for apps is very tight. Best for demos and dev; production workloads need 8GB or more.", sys.RAMMB))
	case ProfileSmall:
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("RAM is below recommended 8GB (%dMB available): the 'small' profile will be selected: monitoring runs with tight limits. Disable it with 'kip platform disable prometheus' and 'kip platform disable loki' after install if you need more room for apps.", sys.RAMMB))
	}

	return result, nil
}

func checkOS(sys SystemInfo) error {
	id := strings.ToLower(sys.OS)
	versions, ok := supportedOS[id]
	if !ok {
		return fmt.Errorf("unsupported OS: %s (supported: ubuntu, debian, rocky, almalinux, centos)", sys.OS)
	}

	version := sys.OSVersion
	if majorVersionOS[id] {
		version, _, _ = strings.Cut(version, ".")
	}
	for _, v := range versions {
		if version == v {
			return nil
		}
	}

	return fmt.Errorf("unsupported %s version: %s (supported: %s)",
		sys.OS, sys.OSVersion, strings.Join(versions, ", "))
}

func pastStandardSupportWarning(sys SystemInfo) string {
	id := strings.ToLower(sys.OS)
	for _, v := range pastStandardSupportOS[id] {
		if sys.OSVersion == v {
			return fmt.Sprintf("%s %s is past the end of its standard support. Installation can continue. Kipper no longer tests this release; upgrade to a current release.", osDisplayName[id], v)
		}
	}
	return ""
}

func checkRAM(sys SystemInfo) error {
	if sys.RAMMB < minRAMMB {
		return fmt.Errorf("insufficient RAM: %dMB available, %dMB required", sys.RAMMB, minRAMMB)
	}
	return nil
}

func checkDisk(sys SystemInfo) error {
	if sys.DiskMB < minDiskMB {
		return fmt.Errorf("insufficient disk: %dMB available, %dMB required", sys.DiskMB, minDiskMB)
	}
	return nil
}

func checkPorts(sys SystemInfo) error {
	var held []string
	for _, p := range requiredPorts {
		var foreign []string
		for _, name := range sys.PortListeners[p] {
			if isK3s(name) {
				continue
			}
			if name == "" {
				name = "an unidentified process"
			}
			if !slices.Contains(foreign, name) {
				foreign = append(foreign, name)
			}
		}
		if len(foreign) > 0 {
			held = append(held, fmt.Sprintf("%d (%s)", p, strings.Join(foreign, ", ")))
		}
	}

	if len(held) > 0 {
		return fmt.Errorf("required ports already in use: %s. Stop the service holding them, or install on a clean server",
			strings.Join(held, ", "))
	}

	return nil
}

// isK3s reports whether a listener belongs to k3s itself, which holds 6443 on a
// host where kip install is being re-run.
func isK3s(process string) bool {
	return process == "k3s-server" || process == "k3s"
}
