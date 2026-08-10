package installer

import (
	"fmt"
	"strings"

	"github.com/getkipper/kipper/kip/internal/ssh"
)

// HostAudit reports host-level security findings detected before
// installation. AuditHost populates this struct; HardenHost acts on it.
type HostAudit struct {
	// RpcbindExposed is true when rpcbind is listening on a public
	// interface (0.0.0.0:111 or [::]:111). It can be abused for DDoS
	// reflection and leaks RPC service information.
	RpcbindExposed bool
}

// Findings returns a human-readable list of security issues detected on
// the host. An empty slice means the host has no surplus services
// exposed on public interfaces.
func (a HostAudit) Findings() []string {
	var f []string
	if a.RpcbindExposed {
		f = append(f, "rpcbind exposed on 0.0.0.0:111")
	}
	return f
}

// AuditHost inspects the remote host for surplus services exposed on
// public interfaces. It does not modify state — pair it with HardenHost
// to fix anything found.
func AuditHost(client *ssh.Client) (HostAudit, error) {
	var audit HostAudit

	// A socket on 0.0.0.0:111, [::]:111, or *:111 is reachable from
	// the public internet. Loopback-only bindings (127.0.0.1, [::1])
	// are not. ss-format may quote the IPv6 wildcard as [::] or just *.
	cmd := `ss -tulnH 2>/dev/null | awk '
	$5 ~ /:111$/ {
		addr = $5
		sub(/:[0-9]+$/, "", addr)
		if (addr == "0.0.0.0" || addr == "[::]" || addr == "*") print
	}'`
	out, err := client.Run(cmd)
	if err != nil {
		return audit, fmt.Errorf("auditing port 111: %w", err)
	}
	audit.RpcbindExposed = strings.TrimSpace(out) != ""

	return audit, nil
}

// HardenHost applies host-level security defaults for the surplus
// services AuditHost can detect. Idempotent: safe to run on hosts that
// are already hardened or that never had the service installed.
func HardenHost(client *ssh.Client) error {
	if err := disableRpcbind(client); err != nil {
		return err
	}
	return nil
}

func disableRpcbind(client *ssh.Client) error {
	// Skip silently if rpcbind isn't installed. systemctl
	// list-unit-files prints the unit name only when it exists.
	out, err := client.Run("systemctl list-unit-files rpcbind.socket 2>/dev/null | awk '$1 == \"rpcbind.socket\" {print $1}'")
	if err != nil {
		return fmt.Errorf("checking rpcbind.socket: %w", err)
	}
	if strings.TrimSpace(out) == "" {
		return nil
	}

	// Disable both the service and its socket activator, then mask
	// the socket so a future package upgrade or systemd reload cannot
	// re-enable it. `|| true` on disable because systemctl returns
	// non-zero when the unit was already disabled, which is the desired
	// end state.
	cmd := "systemctl disable --now rpcbind rpcbind.socket >/dev/null 2>&1 || true; " +
		"systemctl mask rpcbind.socket >/dev/null"
	if _, err := client.Run(cmd); err != nil {
		return fmt.Errorf("masking rpcbind.socket: %w", err)
	}
	return nil
}
