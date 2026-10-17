package installer

import (
	"fmt"
	"net"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// GatherSystemInfo collects host details through client, including total RAM
// and available root disk space in MiB, the CPU count, and listeners on required
// server ports. On error, it returns the information collected so far.
func GatherSystemInfo(client CommandRunner) (SystemInfo, error) {
	return gatherSystemInfo(client, RoleServer)
}

func gatherSystemInfo(client CommandRunner, role HostRole) (SystemInfo, error) {
	var info SystemInfo

	host, err := DetectHostOS(client)
	if err != nil {
		return info, fmt.Errorf("detecting OS: %w", err)
	}
	info.Host = host
	info.OS = host.ID
	info.OSVersion = host.VersionID

	ram, err := gatherRAM(client)
	if err != nil {
		return info, fmt.Errorf("detecting RAM: %w", err)
	}
	info.RAMMB = ram

	cpus, err := gatherCPUs(client)
	if err != nil {
		return info, fmt.Errorf("detecting CPUs: %w", err)
	}
	info.CPUs = cpus

	disk, err := gatherDisk(client)
	if err != nil {
		return info, fmt.Errorf("detecting disk: %w", err)
	}
	info.DiskMB, info.DiskSizeMB = disk.free, disk.size
	info.ImageDiskMB, info.SeparateImageDisk, info.ImagesPresentMB = disk.imageFree, disk.separate, disk.imagesPresentMB

	listeners, err := gatherPortListeners(client, role.requiredPorts())
	if err != nil {
		return info, fmt.Errorf("checking ports: %w", err)
	}
	info.PortListeners = listeners

	return info, nil
}

func gatherRAM(client CommandRunner) (int, error) {
	// MemTotal is in kB
	output, err := client.Run("grep MemTotal /proc/meminfo | awk '{print $2}'")
	if err != nil {
		return 0, err
	}

	kb, err := strconv.Atoi(strings.TrimSpace(output))
	if err != nil {
		return 0, fmt.Errorf("parsing meminfo: %w", err)
	}

	return kb / 1024, nil
}

func gatherCPUs(client CommandRunner) (int, error) {
	output, err := client.Run("nproc")
	if err != nil {
		return 0, err
	}
	cpus, err := strconv.Atoi(strings.TrimSpace(output))
	if err != nil {
		return 0, fmt.Errorf("parsing nproc output: %w", err)
	}
	return cpus, nil
}

// diskReport records filesystem capacity and containerd usage in MiB.
type diskReport struct {
	free, size      int
	imageFree       int
	separate        bool
	imagesPresentMB int
}

// k3sImageStore is where containerd keeps the images k3s pulls.
const k3sImageStore = "/var/lib/rancher/k3s/agent/containerd"

// gatherDisk measures the filesystems holding Longhorn and k3s data, using
// existing parent directories before installation. Containerd usage reduces
// the estimated additional space needed on subsequent runs. The usage scan has
// a 60-second timeout. Missing usage data leaves the full estimate in place.
func gatherDisk(client CommandRunner) (diskReport, error) {
	output, err := client.Run(`l=/var/lib/longhorn; [ -d "$l" ] || l=/var/lib; ` +
		`r=/var/lib/rancher/k3s; [ -d "$r" ] || r=/var/lib/rancher; [ -d "$r" ] || r=/var/lib; ` +
		`df -BM --output=avail,size,target "$l" "$r" | tail -n +2; ` +
		`[ -d ` + k3sImageStore + ` ] && echo "images-present[$(timeout 60 du -sm ` + k3sImageStore + ` 2>/dev/null | cut -f1)]"; true`)
	if err != nil {
		return diskReport{}, err
	}
	return parseDiskReport(output)
}

var (
	diskReportLine     = regexp.MustCompile(`^(\d+)M\s+(\d+)M\s+(\S+)$`)
	imagesPresentField = regexp.MustCompile(`^images-present\[(\d+)\]$`)
)

// parseDiskReport reads two df lines (Longhorn's filesystem, then k3s's) and
// the optional image store size, ignoring any other line.
func parseDiskReport(output string) (diskReport, error) {
	var report diskReport
	var targets []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if m := imagesPresentField.FindStringSubmatch(line); len(m) == 2 {
			report.imagesPresentMB, _ = strconv.Atoi(m[1])
			continue
		}
		m := diskReportLine.FindStringSubmatch(line)
		if len(m) < 4 {
			continue
		}
		free, _ := strconv.Atoi(m[1])
		size, _ := strconv.Atoi(m[2])
		switch len(targets) {
		case 0:
			report.free, report.size = free, size
		case 1:
			report.imageFree = free
		}
		targets = append(targets, m[3])
	}
	if len(targets) != 2 {
		return diskReport{}, fmt.Errorf("parsing disk space from %q", strings.TrimSpace(output))
	}
	report.separate = targets[0] != targets[1]
	return report, nil
}

// gatherPortListeners lists the processes listening on each required port,
// keyed by port. A listener ss cannot attribute to a process maps to "".
func gatherPortListeners(client CommandRunner, ports []int) (map[int][]string, error) {
	filters := make([]string, 0, len(ports))
	for _, p := range ports {
		filters = append(filters, fmt.Sprintf("sport = :%d", p))
	}
	output, err := client.Run(fmt.Sprintf("ss -tlnpH '( %s )'", strings.Join(filters, " or ")))
	if err != nil {
		return nil, err
	}
	return parsePortListeners(output), nil
}

var ssProcessName = regexp.MustCompile(`\("([^"]*)"`)

// parsePortListeners reads `ss -tlnpH` output into the process names listening
// on each port. Lines other than LISTEN rows are ignored.
func parsePortListeners(output string) map[int][]string {
	listeners := map[int][]string{}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[0] != "LISTEN" {
			continue
		}
		local := fields[3]
		port, err := strconv.Atoi(local[strings.LastIndex(local, ":")+1:])
		if err != nil {
			continue
		}
		names := []string{""}
		if matches := ssProcessName.FindAllStringSubmatch(line, -1); len(matches) > 0 {
			names = names[:0]
			for _, m := range matches {
				names = append(names, m[1])
			}
		}
		for _, name := range names {
			if !slices.Contains(listeners[port], name) {
				listeners[port] = append(listeners[port], name)
			}
		}
	}
	return listeners
}

// ProbePortFromLocal checks if a port is reachable on a remote host
// by dialling from the local machine. Used as a fallback check.
func ProbePortFromLocal(host string, port int) bool {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
