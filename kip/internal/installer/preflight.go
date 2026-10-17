package installer

import (
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/getkipper/kipper/controller/pkg/platform"
)

const (
	// minRAMMB is the minimum detected RAM in MiB accepted by preflight.
	minRAMMB         = 2048
	tightRAMMB       = 3500
	recommendedRAMMB = 8192
)

// Preflight uses Longhorn's default reservation and free-space thresholds.
const (
	longhornReservedPercent         = 30
	longhornMinimalAvailablePercent = 25
)

// installFootprintMB estimates image and system data usage in MiB.
func installFootprintMB(monitoring bool) int {
	if monitoring {
		return 9216
	}
	return 7168
}

// systemVolumesMB budgets fresh system volumes in MiB using manifest defaults.
// Existing volumes may be larger.
func systemVolumesMB(state PlatformState, inClusterBackup bool) int {
	sizes := []string{zotDefaultStorage}
	if inClusterBackup {
		sizes = append(sizes, minioDefaultStorage)
	}
	if state.LokiEnabled() {
		sizes = append(sizes, platform.LokiDefaultStorage)
	}
	total := 0
	for _, size := range sizes {
		q := resource.MustParse(size)
		total += int(q.Value() / (1024 * 1024))
	}
	return total
}

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

// SystemInfo holds measurements gathered before installation. Sizes are in MiB.
// DiskMB is free space and DiskSizeMB is total capacity on the Longhorn filesystem.
// ImageDiskMB is free space on the k3s filesystem. SeparateImageDisk indicates
// distinct filesystems, and ImagesPresentMB measures existing containerd usage.
type SystemInfo struct {
	Host              HostOS
	OS                string
	OSVersion         string
	RAMMB             int
	DiskMB            int
	DiskSizeMB        int
	ImageDiskMB       int
	SeparateImageDisk bool
	ImagesPresentMB   int
	CPUs              int
	PortListeners     map[int][]string
}

type PreflightResult struct {
	Warnings []string
}

// RunPreflightChecks checks server requirements for a known platform profile
// and its component overrides. Local storage uses fixed free-space thresholds.
// For Longhorn, setting inClusterBackup includes MinIO in the disk budget.
// Setting existingCluster reports disk shortages as warnings so
// existing installations can be updated. Other failed checks return a nil result
// and an error. Successful checks may include OS support and resource warnings.
// A non-positive CPU count skips the CPU headroom check.
func RunPreflightChecks(sys SystemInfo, state PlatformState, inClusterBackup, existingCluster bool) (*PreflightResult, error) {
	return runPreflightChecks(sys, state, inClusterBackup, existingCluster, RoleServer)
}

func runPreflightChecks(sys SystemInfo, state PlatformState, inClusterBackup, existingCluster bool, role HostRole) (*PreflightResult, error) {
	profile := state.Profile
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
	if role == RoleServer && state.Storage == StorageLocal {
		warning, err := checkLocalDisk(sys)
		switch {
		case err != nil && !existingCluster:
			return nil, err
		case err != nil:
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("%s. Installation will continue on this existing cluster. Low disk space may affect workloads.", sentence(err.Error())))
		case warning != "":
			result.Warnings = append(result.Warnings, warning)
		}
	} else if err := checkDisk(sys, state, inClusterBackup, role); err != nil {
		if !existingCluster {
			return nil, err
		}
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("%s. Installation will continue on this existing cluster. Low disk space may affect workloads and prevent Longhorn from placing new volumes.", sentence(err.Error())))
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
	risk := fmt.Sprintf(" The '%s' profile enables monitoring (Prometheus, Loki, Grafana) by default, which can push the host into swapping, and the install itself can fail at Zot. Use --profile nano to disable monitoring on a new cluster.", profile)
	switch {
	case ramMB < tightRAMMB:
		w := fmt.Sprintf("RAM is below 4GB (%dMB available): headroom for apps is very tight. Use this host for demos or development. For production, start with at least 8GB.", ramMB)
		if monitoring {
			w += risk
		} else {
			w += " The 'nano' profile keeps monitoring (Prometheus, Loki, Grafana) off."
		}
		return []string{w}
	case ramMB < recommendedRAMMB && !monitoring:
		return []string{fmt.Sprintf("RAM is below recommended 8GB (%dMB available): the 'nano' profile keeps monitoring (Prometheus, Loki, Grafana) off to leave more memory for apps.", ramMB)}
	case ramMB < nanoRAMMB:
		return []string{fmt.Sprintf("RAM is below recommended 8GB (%dMB available).", ramMB) + risk}
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
			return fmt.Sprintf("%s %s is past the end of its standard support. Installation can continue. Kipper no longer tests this release. Upgrade to a current release.", osDisplayName[id], v)
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

// Local storage thresholds are in MiB. Volumes share the k3s filesystem with
// images and system data.
const (
	localDiskFloorMB       = 10240
	localDiskRecommendedMB = 20480
)

// checkLocalDisk checks free space for local storage, which keeps volumes on
// the filesystem holding k3s's data. It credits existing images up to the
// estimated installation footprint, then returns an error below the floor
// or a warning below the recommended space.
func checkLocalDisk(sys SystemInfo) (string, error) {
	free := sys.DiskMB
	if sys.SeparateImageDisk {
		free = sys.ImageDiskMB
	}
	// Credit images from earlier runs to avoid budgeting for them twice,
	// capped at the estimated image and system data footprint.
	available := free + min(sys.ImagesPresentMB, installFootprintMB(false))
	if available < localDiskFloorMB {
		return "", fmt.Errorf("insufficient disk: %d MiB free on the filesystem holding k3s's data (/var/lib/rancher/k3s). Local storage needs at least %d MiB for images, system data and volumes. Existing images count towards this minimum up to the estimated installation footprint. Free up space or use a larger disk",
			free, localDiskFloorMB)
	}
	if available < localDiskRecommendedMB {
		return fmt.Sprintf("%d MiB free on the filesystem holding k3s's data. At least %d MiB is recommended for apps and their data. Local volumes can grow beyond their requested size, so monitor disk usage with 'kip status'",
			free, localDiskRecommendedMB), nil
	}
	return "", nil
}

// checkDisk budgets system volumes on servers and free space on all hosts.
// Existing containerd usage reduces the estimated installation footprint.
func checkDisk(sys SystemInfo, state PlatformState, inClusterBackup bool, role HostRole) error {
	monitoring := false
	if role == RoleServer {
		monitoring = state.LokiEnabled() || state.PrometheusEnabled()
		volumes := systemVolumesMB(state, inClusterBackup)
		schedulable := sys.DiskSizeMB * (100 - longhornReservedPercent) / 100
		if schedulable < volumes {
			needed := (volumes*100 + (100 - longhornReservedPercent) - 1) / (100 - longhornReservedPercent)
			return fmt.Errorf("insufficient disk: the filesystem holding /var/lib/longhorn is %d MiB. Longhorn places Kipper's system volumes (%d MiB on the '%s' profile) in %d%% of it, so it must be at least %d MiB. Use a server with a larger disk",
				sys.DiskSizeMB, volumes, state.Profile, 100-longhornReservedPercent, needed)
		}
	}
	footprint := max(installFootprintMB(monitoring)-sys.ImagesPresentMB, 0)
	if sys.SeparateImageDisk {
		if sys.ImageDiskMB <= footprint {
			return fmt.Errorf("insufficient disk: %d MiB free on the filesystem holding k3s's data (/var/lib/rancher/k3s). Images and system data still need about %d MiB. Free up space or use a larger disk",
				sys.ImageDiskMB, footprint)
		}
		footprint = 0
	}
	minimalFree := sys.DiskSizeMB * longhornMinimalAvailablePercent / 100
	if sys.DiskMB-footprint <= minimalFree {
		return fmt.Errorf("insufficient disk: %d MiB free on the filesystem holding /var/lib/longhorn. After about %d MiB of images and system data, Longhorn needs more than %d%% of the filesystem (%d MiB) kept free. Free up space or use a larger disk",
			sys.DiskMB, footprint, longhornMinimalAvailablePercent, minimalFree)
	}
	return nil
}

// sentence capitalizes an initial ASCII letter for display as a warning.
func sentence(msg string) string {
	if msg == "" {
		return msg
	}
	return strings.ToUpper(msg[:1]) + msg[1:]
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
