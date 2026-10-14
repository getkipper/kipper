package installer

import (
	"fmt"
	"strings"
)

// HostAudit records wildcard listeners on port 111 and the Cockpit socket.
type HostAudit struct {
	RpcbindExposed bool
	CockpitExposed bool
}

// Findings returns descriptions of the detected listeners.
func (a HostAudit) Findings() []string {
	var f []string
	if a.RpcbindExposed {
		f = append(f, "rpcbind port 111 is listening on all interfaces")
	}
	if a.CockpitExposed {
		f = append(f, "Cockpit socket is listening on all interfaces")
	}
	return f
}

// AuditHost checks for wildcard listeners on port 111 and the Cockpit socket.
// Internet reachability also depends on routing and firewall rules.
func AuditHost(client CommandRunner) (HostAudit, error) {
	var audit HostAudit
	var err error
	if audit.RpcbindExposed, err = listensPublicly(client, 111); err != nil {
		return audit, err
	}
	if audit.CockpitExposed, err = cockpitListensPublicly(client); err != nil {
		return audit, err
	}
	return audit, nil
}

// listensPublicly checks for a wildcard listener on port, accepting the
// 0.0.0.0, [::], and * forms from ss.
func listensPublicly(client CommandRunner, port int) (bool, error) {
	cmd := fmt.Sprintf(`ss -tulnH 2>/dev/null | awk '
	$5 ~ /:%d$/ {
		addr = $5
		sub(/:[0-9]+$/, "", addr)
		if (addr == "0.0.0.0" || addr == "[::]" || addr == "*") print
	}'`, port)
	out, err := client.Run(cmd)
	if err != nil {
		return false, fmt.Errorf("auditing port %d: %w", port, err)
	}
	return strings.TrimSpace(out) != "", nil
}

// HardenHost disables installed rpcbind units and stops Cockpit when its active
// socket listens on all interfaces. It is safe to repeat.
func HardenHost(client CommandRunner) error {
	if err := disableRpcbind(client); err != nil {
		return err
	}
	return disableCockpit(client)
}

// disableCockpit leaves the socket unmasked so operators can re-enable it
// with systemctl enable --now cockpit.socket.
func disableCockpit(client CommandRunner) error {
	exposed, err := cockpitListensPublicly(client)
	if err != nil || !exposed {
		return err
	}
	if _, err := client.Run(cockpitShutdownCommand); err != nil {
		return fmt.Errorf("disabling cockpit.socket: %w", err)
	}
	return nil
}

// cockpitShutdownCommand also stops the service, which can outlive its socket.
const cockpitShutdownCommand = "systemctl disable --now cockpit.socket >/dev/null && systemctl stop cockpit.service >/dev/null"

// cockpitListensPublicly checks the active socket unit for wildcard listeners.
// Using the unit identifies Cockpit even when its port is customized.
func cockpitListensPublicly(client CommandRunner) (bool, error) {
	out, err := client.Run("systemctl is-active cockpit.socket 2>/dev/null || true")
	if err != nil {
		return false, fmt.Errorf("checking cockpit.socket: %w", err)
	}
	if strings.TrimSpace(out) != "active" {
		return false, nil
	}
	listen, err := client.Run("systemctl show cockpit.socket -p Listen --value")
	if err != nil {
		return false, fmt.Errorf("reading where cockpit.socket listens: %w", err)
	}
	return listensOnEveryInterface(listen), nil
}

// listensOnEveryInterface checks systemd Listen output, such as "[::]:9090 (Stream)",
// for wildcard addresses, including bare port numbers.
func listensOnEveryInterface(listen string) bool {
	for _, line := range strings.Split(listen, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		addr := fields[0]
		if sshPortPattern.MatchString(addr) || strings.HasPrefix(addr, "[::]:") || strings.HasPrefix(addr, "0.0.0.0:") || strings.HasPrefix(addr, "*:") {
			return true
		}
	}
	return false
}

func disableRpcbind(client CommandRunner) error {
	out, err := client.Run("systemctl list-unit-files rpcbind.socket 2>/dev/null | awk '$1 == \"rpcbind.socket\" {print $1}'")
	if err != nil {
		return fmt.Errorf("checking rpcbind.socket: %w", err)
	}
	if strings.TrimSpace(out) == "" {
		return nil
	}

	// Attempt to stop both units, then mask the socket to block activation.
	// Masking still runs if disabling either unit fails.
	cmd := "systemctl disable --now rpcbind rpcbind.socket >/dev/null 2>&1 || true; " +
		"systemctl mask rpcbind.socket >/dev/null"
	if _, err := client.Run(cmd); err != nil {
		return fmt.Errorf("masking rpcbind.socket: %w", err)
	}
	return nil
}
