package installer

import (
	"fmt"
	"strings"
)

// HostAudit records wildcard listeners on the rpcbind and Cockpit ports.
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
		f = append(f, "Cockpit port 9090 is listening on all interfaces")
	}
	return f
}

// AuditHost checks for wildcard listeners on the rpcbind and Cockpit ports.
// Internet reachability also depends on routing and firewall rules.
func AuditHost(client CommandRunner) (HostAudit, error) {
	var audit HostAudit
	var err error
	if audit.RpcbindExposed, err = listensPublicly(client, 111); err != nil {
		return audit, err
	}
	if audit.CockpitExposed, err = listensPublicly(client, 9090); err != nil {
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

// HardenHost disables rpcbind and Cockpit socket activation where installed.
// It is safe to repeat.
func HardenHost(client CommandRunner) error {
	if err := disableRpcbind(client); err != nil {
		return err
	}
	return disableCockpit(client)
}

// disableCockpit leaves the socket unmasked so operators can re-enable it
// with systemctl enable --now cockpit.socket.
func disableCockpit(client CommandRunner) error {
	out, err := client.Run("systemctl list-unit-files cockpit.socket 2>/dev/null | awk '$1 == \"cockpit.socket\" {print $1}'")
	if err != nil {
		return fmt.Errorf("checking cockpit.socket: %w", err)
	}
	if strings.TrimSpace(out) == "" {
		return nil
	}
	if _, err := client.Run("systemctl disable --now cockpit.socket >/dev/null"); err != nil {
		return fmt.Errorf("disabling cockpit.socket: %w", err)
	}
	return nil
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
