package installer

import (
	"fmt"
	"net"
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

	info.Ports = probeOpenPorts(client)

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

// probeOpenPorts checks whether the required ports are reachable on the
// remote host by attempting a TCP connection from the local machine.
// Ports that accept connections are returned.
func probeOpenPorts(client *ssh.Client) []int {
	// We check from the remote side whether the ports are not already
	// bound by another service. A port is "available" if nothing is
	// listening on it — which means our connection will be refused.
	// However, for the preflight check we actually want to verify that
	// these ports CAN be used (i.e. not blocked by a firewall).
	//
	// The simplest reliable check: try to bind each port briefly on the
	// remote host. If we can bind it, the port is available.
	var open []int
	for _, port := range requiredPorts {
		cmd := fmt.Sprintf(
			"timeout 1 bash -c 'echo | nc -l -p %d &>/dev/null &' 2>/dev/null; "+
				"sleep 0.2; "+
				"timeout 1 bash -c 'echo | nc -z 127.0.0.1 %d' 2>/dev/null && echo open || echo closed; "+
				"kill %%1 2>/dev/null",
			port, port)
		output, _ := client.Run(cmd)
		if strings.TrimSpace(output) == "open" {
			open = append(open, port)
		}
	}

	return open
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
