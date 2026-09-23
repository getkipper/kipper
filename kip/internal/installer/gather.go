package installer

import (
	"fmt"
	"net"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/getkipper/kipper/kip/internal/ssh"
)

// GatherSystemInfo collects OS, RAM, disk, and port information from
// the remote server over SSH.
func GatherSystemInfo(client *ssh.Client) (SystemInfo, error) {
	var info SystemInfo

	os, version, err := gatherOS(client)
	if err != nil {
		return info, fmt.Errorf("detecting OS: %w", err)
	}
	info.OS = os
	info.OSVersion = version

	ram, err := gatherRAM(client)
	if err != nil {
		return info, fmt.Errorf("detecting RAM: %w", err)
	}
	info.RAMMB = ram

	disk, err := gatherDisk(client)
	if err != nil {
		return info, fmt.Errorf("detecting disk: %w", err)
	}
	info.DiskMB = disk

	listeners, err := gatherPortListeners(client)
	if err != nil {
		return info, fmt.Errorf("checking ports: %w", err)
	}
	info.PortListeners = listeners

	return info, nil
}

func gatherOS(client *ssh.Client) (string, string, error) {
	output, err := client.Run("cat /etc/os-release")
	if err != nil {
		return "", "", err
	}

	var id, versionID string
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "ID=") {
			id = strings.Trim(strings.TrimPrefix(line, "ID="), "\"")
		}
		if strings.HasPrefix(line, "VERSION_ID=") {
			versionID = strings.Trim(strings.TrimPrefix(line, "VERSION_ID="), "\"")
		}
	}

	if id == "" {
		return "", "", fmt.Errorf("could not determine OS from /etc/os-release")
	}

	return id, versionID, nil
}

func gatherRAM(client *ssh.Client) (int, error) {
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

func gatherDisk(client *ssh.Client) (int, error) {
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
func gatherPortListeners(client *ssh.Client) (map[int][]string, error) {
	filters := make([]string, 0, len(requiredPorts))
	for _, p := range requiredPorts {
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
