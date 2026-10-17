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
	CPUs          int
	PortListeners map[int][]string
}

// PreflightResult contains the outcome of preflight checks.
type PreflightResult struct {
	Warnings []string
}

// RunPreflightChecks validates that the target server meets the minimum
// requirements for a Kipper installation with the given sizing profile.
// The caller must supply a known profile. Failed checks return a nil result
// and an error. Resource headroom concerns are returned as warnings.
// A non-positive CPU count skips the CPU headroom check.
func RunPreflightChecks(sys SystemInfo, profile string) (*PreflightResult, error) {
	return runPreflightChecks(sys, profile, RoleServer)
}

func runPreflightChecks(sys SystemInfo, profile string, role HostRole) (*PreflightResult, error) {
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
	if err := checkPorts(sys, role); err != nil {
		return nil, err
	}

	// Platform resource profiles are selected during server setup.
	if role == RoleWorker {
		return result, nil
	}
	result.Warnings = append(result.Warnings, ramWarnings(sys.RAMMB, profile)...)
	if w := cpuWarning(profile, sys.CPUs); w != "" {
		result.Warnings = append(result.Warnings, w)
	}

	return result, nil
}

// ramWarnings uses measured RAM because CPU limits or an explicit choice can
// select a different profile from the memory-based default.
func ramWarnings(ramMB int, profile string) []string {
	monitoring := profile != ProfileNano
	switch {
	case ramMB < nanoRAMMB:
		w := fmt.Sprintf("RAM is below 4GB (%dMB available): headroom for apps is very tight. Use this host for demos or development. For production, start with at least 8GB.", ramMB)
		if monitoring {
			w += fmt.Sprintf(" The '%s' profile enables monitoring (Prometheus, Loki, Grafana) by default, which can push the host into swapping, and the install itself can fail at Zot. Use --profile nano to disable monitoring on a new cluster.", profile)
		} else {
			w += " The 'nano' profile keeps monitoring (Prometheus, Loki, Grafana) off."
		}
		return []string{w}
	case ramMB < recommendedRAMMB && !monitoring:
		return []string{fmt.Sprintf("RAM is below recommended 8GB (%dMB available): the 'nano' profile keeps monitoring (Prometheus, Loki, Grafana) off to leave more memory for apps.", ramMB)}
	case ramMB < recommendedRAMMB:
		return []string{fmt.Sprintf("RAM is below recommended 8GB (%dMB available): the '%s' profile enables monitoring by default, leaving less memory for apps. Use --profile nano on a new cluster, or disable monitoring after install with 'kip platform disable prometheus' and 'kip platform disable loki'.", ramMB, profile)}
	}
	return nil
}

// cpuWarning says when a profile's platform pods leave less than a default
// database service's request on the host. It returns an empty string when the
// estimate fits or the CPU count is non-positive.
func cpuWarning(profile string, cpus int) string {
	if profileFitsCPU(profile, cpus) {
		return ""
	}
	return fmt.Sprintf("%d %s: the '%s' profile's platform pods request about %dm of the %dm available, leaving %dm for apps. A default database service requests %dm, which exceeds this estimate of available CPU. Use a host with more CPUs, or lower --cpu for services if capacity allows.",
		cpus, plural(cpus, "CPU", "CPUs"), profile, platformCPURequestMilli[profile], cpus*1000,
		max(cpuHeadroomMilli(profile, cpus), 0), defaultServiceCPURequestMilli)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
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

func checkPorts(sys SystemInfo, role HostRole) error {
	var held []string
	for _, p := range role.requiredPorts() {
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

// isK3s recognizes process names allowed to hold ports when setup is retried.
func isK3s(process string) bool {
	return process == "k3s-server" || process == "k3s" || process == "k3s-agent"
}
