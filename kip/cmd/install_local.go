package cmd

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/installer"
)

// checkLocalHost rejects loopback addresses because --host becomes the cluster's
// TLS name and API address. bound reports a match with a local interface;
// addresses behind NAT require separate confirmation.
func checkLocalHost(host string, lookup func(string) ([]net.IP, error), local func() ([]net.IP, error)) (bound bool, err error) {
	ips := []net.IP{net.ParseIP(host)}
	if ips[0] == nil {
		ips, err = lookup(host)
		if err != nil || len(ips) == 0 {
			return false, fmt.Errorf("--host %s does not resolve on this machine; give the server's public address or a DNS name that resolves to it", host)
		}
	}
	for _, ip := range ips {
		if ip.IsLoopback() {
			return false, fmt.Errorf("--host %s is a loopback address; give the server's public address, which the cluster's certificate and API address are built on", host)
		}
	}
	addrs, err := local()
	if err != nil {
		return false, fmt.Errorf("listing this machine's addresses: %w", err)
	}
	for _, ip := range ips {
		for _, a := range addrs {
			if ip.Equal(a) {
				return true, nil
			}
		}
	}
	return false, nil
}

// checkLocalPrivileges requires root and HOME=/root to match the installer's
// command and configuration paths.
func checkLocalPrivileges(uid int, home string) error {
	if uid != 0 {
		return fmt.Errorf("--local installs this machine and has to run as root")
	}
	if home != "/root" {
		return fmt.Errorf("--local needs HOME=/root, where kip keeps the cluster's config; run it as root with HOME=/root")
	}
	return nil
}

func localAddresses() ([]net.IP, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			ips = append(ips, n.IP)
		}
	}
	return ips, nil
}

// confirmUnboundHost asks the operator to confirm routing for an address that
// is absent from local interfaces, such as a public address behind NAT.
func confirmUnboundHost(in io.Reader, out io.Writer, host string) bool {
	say(out, "\n  %s is not assigned to a local interface. A public address behind NAT can still reach this machine.\n", host)
	say(out, "  Does %s reach this machine? [y/N] ", host)
	answer, _ := bufio.NewReader(in).ReadString('\n')
	return isYes(answer)
}

func guardLocalInstall(host string, hostMapsHere, isTTY bool) error {
	if err := checkLocalPrivileges(os.Geteuid(), os.Getenv("HOME")); err != nil {
		return err
	}
	bound, err := checkLocalHost(host, net.LookupIP, localAddresses)
	if err != nil {
		return err
	}
	if bound || hostMapsHere {
		return nil
	}
	if isTTY && confirmUnboundHost(os.Stdin, os.Stdout, host) {
		return nil
	}
	return fmt.Errorf("%s is not assigned to a local interface; pass --host-maps-here if this address routes to this machine, for example through NAT", host)
}

// Importing the settings requires --install-result as well as this file header.
const installResultHeader = installResultMarker + ": cluster settings for your management machine\n# Import with: kip cluster add --install-result <file>\n"

// installResultMarker identifies result files independently of the explanatory
// text and line endings.
const installResultMarker = "# Kipper install result"

func isInstallResult(data []byte) bool {
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		return strings.HasPrefix(trimmed, installResultMarker)
	}
	return false
}

// writeInstallResult adds maintenance settings to a cluster export bundle.
// Mode 0600 protects the gateway token.
func writeInstallResult(path, host string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("reading the installed cluster's entry: %w", err)
	}
	cluster := cfg.GetClusterByHost(host)
	if cluster == nil {
		return fmt.Errorf("the install saved no cluster entry for %s", host)
	}
	kubeconfig, err := os.ReadFile(cluster.Kubeconfig)
	if err != nil {
		return fmt.Errorf("reading the installed cluster's kubeconfig: %w", err)
	}
	hosts := exportHosts{Console: cluster.ConsoleHost(), ConsoleAPI: cluster.ConsoleAPIHost(), Dex: cluster.DexHost()}
	var b strings.Builder
	if err := writeClusterBundle(&b, installResultHeader, cluster, cluster.Domain, hosts, kubeconfig, installResultExtras(cluster)); err != nil {
		return err
	}
	return installer.WriteFileAtomic(path, []byte(b.String()), 0o600)
}

func installResultExtras(c *config.Cluster) []string {
	var lines []string
	add := func(key, value string) {
		if value != "" {
			lines = append(lines, key+": "+value)
		}
	}
	add("org_display_name", c.OrgDisplayName)
	add("gateway_token", c.GatewayToken)
	if bs := c.BackupStorage; bs != nil {
		add("backup_storage_mode", bs.Mode)
		add("backup_storage_bucket", bs.Bucket)
		add("backup_storage_region", bs.Region)
		add("backup_storage_endpoint", bs.Endpoint)
	}
	for _, r := range c.DNSResolvers {
		add("dns_resolver", r)
	}
	for _, p := range c.TrustedProxies {
		add("trusted_proxy", p)
	}
	return lines
}

// resultSettings are the settings an install result carries beyond an export.
type resultSettings struct {
	orgDisplayName string
	gatewayToken   string
	backup         config.BackupStorageRef
	dnsResolvers   []string
	trustedProxies []string
}

func (r *resultSettings) validate() error {
	switch r.backup.Mode {
	case "", "external", "in-cluster":
	default:
		return fmt.Errorf("install result: backup storage mode %q is not external or in-cluster", r.backup.Mode)
	}
	if e := r.backup.Endpoint; e != "" {
		if u, err := url.Parse(e); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return fmt.Errorf("install result: backup storage endpoint %q is not an http or https URL", e)
		}
	}
	for _, ip := range r.dnsResolvers {
		if net.ParseIP(ip) == nil {
			return fmt.Errorf("install result: DNS resolver %q is not an IP address", ip)
		}
	}
	for _, p := range r.trustedProxies {
		if net.ParseIP(p) == nil {
			if _, _, err := net.ParseCIDR(p); err != nil {
				return fmt.Errorf("install result: trusted proxy %q is not an IP address or CIDR", p)
			}
		}
	}
	if t := r.gatewayToken; t != "" && strings.ContainsFunc(t, func(c rune) bool { return c <= ' ' || c > '~' }) {
		return fmt.Errorf("install result: the gateway token must contain only printable ASCII characters without spaces")
	}
	return nil
}

func (r *resultSettings) set(key, value string) {
	switch key {
	case "org_display_name":
		r.orgDisplayName = value
	case "gateway_token":
		r.gatewayToken = value
	case "backup_storage_mode":
		r.backup.Mode = value
	case "backup_storage_bucket":
		r.backup.Bucket = value
	case "backup_storage_region":
		r.backup.Region = value
	case "backup_storage_endpoint":
		r.backup.Endpoint = value
	case "dns_resolver":
		r.dnsResolvers = append(r.dnsResolvers, value)
	case "trusted_proxy":
		r.trustedProxies = append(r.trustedProxies, value)
	}
}

// applyTo replaces maintenance settings with those recorded by the install.
func (r *resultSettings) applyTo(entry *config.Cluster) {
	entry.OrgDisplayName = r.orgDisplayName
	entry.GatewayToken = r.gatewayToken
	entry.BackupStorage = nil
	if r.backup.Mode != "" {
		backup := r.backup
		entry.BackupStorage = &backup
	}
	entry.DNSResolvers = r.dnsResolvers
	entry.TrustedProxies = r.trustedProxies
}

// refuseUninstallingThisMachine checks both machine identity and local addresses
// to prevent removing the cluster while kip is running on its host.
func refuseUninstallingThisMachine(cluster *config.Cluster, localMachineID func() (string, error), lookup func(string) ([]net.IP, error), local func() ([]net.IP, error)) error {
	thisMachine := false
	if cluster.MachineID != "" {
		id, err := localMachineID()
		thisMachine = err == nil && id == cluster.MachineID
	}
	if thisMachine || hostIsThisMachine(cluster.Host, lookup, local) {
		return fmt.Errorf("%s is this machine; run kip cluster uninstall from another machine that has the cluster", cluster.Host)
	}
	return nil
}

func readLocalMachineID() (string, error) {
	data, err := os.ReadFile("/etc/machine-id")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// hostIsThisMachine matches resolved addresses against local interfaces.
// Lookup failures return false; the later SSH connection reports reachability.
func hostIsThisMachine(host string, lookup func(string) ([]net.IP, error), local func() ([]net.IP, error)) bool {
	ips := []net.IP{net.ParseIP(host)}
	if ips[0] == nil {
		resolved, err := lookup(host)
		if err != nil {
			return false
		}
		ips = resolved
	}
	addrs, err := local()
	if err != nil {
		return false
	}
	for _, ip := range ips {
		for _, a := range addrs {
			if ip.Equal(a) {
				return true
			}
		}
	}
	return false
}
