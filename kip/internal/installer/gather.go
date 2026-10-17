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
	info.DiskMB = disk

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

func gatherDisk(client CommandRunner) (int, error) {
	// Available space on root partition in 1M blocks
	output, err := client.Run("df -BM --output=avail / | tail -1")
	if err != nil {
		return 0, err
	}

	cleaned := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(output), "M"))
	mb, err := strconv.Atoi(cleaned)
	if err != nil {
		return 0, fmt.Errorf("parsing disk space: %w", err)
	}

	return mb, nil
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
