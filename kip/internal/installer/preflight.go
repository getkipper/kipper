package installer

import (
	"fmt"
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

// supportedOS maps OS names to their minimum supported versions.
var supportedOS = map[string][]string{
	"ubuntu": {"20.04", "22.04", "24.04", "26.04"},
	"debian": {"11", "12"},
}

// SystemInfo holds the remote server's system information
// gathered before installation.
type SystemInfo struct {
	OS        string
	OSVersion string
	RAMMB     int
	DiskMB    int
	Ports     []int
}

// requiredPorts are the ports that must be available for a Kipper cluster.
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
	versions, ok := supportedOS[strings.ToLower(sys.OS)]
	if !ok {
		return fmt.Errorf("unsupported OS: %s (supported: ubuntu, debian)", sys.OS)
	}

	for _, v := range versions {
		if sys.OSVersion == v {
			return nil
		}
	}

	return fmt.Errorf("unsupported %s version: %s (supported: %s)",
		sys.OS, sys.OSVersion, strings.Join(versions, ", "))
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
	available := make(map[int]bool)
	for _, p := range sys.Ports {
		available[p] = true
	}

	var missing []string
	for _, p := range requiredPorts {
		if !available[p] {
			missing = append(missing, fmt.Sprintf("%d", p))
		}
	}

	if len(missing) > 0 {
		return fmt.Errorf("required ports unavailable: %s", strings.Join(missing, ", "))
	}

	return nil
}
