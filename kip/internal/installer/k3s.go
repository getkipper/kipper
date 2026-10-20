package installer

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

// k3sVersion is the exact k3s release every install runs. Unpinned installs
// take whatever get.k3s.io serves that day, so two clusters built a week
// apart could run different Kubernetes versions and nothing version-dependent
// (structured authentication needs 1.30+) could be relied on.
// v1.36.2+k3s1 verified 2026-07-20: stable channel head at
// https://update.k3s.io/v1-release/channels, published (non-prerelease)
// GitHub release. INSTALL_K3S_VERSION per
// https://docs.k3s.io/reference/env-variables.
const k3sVersion = "v1.36.2+k3s1"

// k3sVersionRe matches a k3s release tag: vMAJOR.MINOR.PATCH+k3sREVISION.
var k3sVersionRe = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)\+k3s(\d+)$`)

// parseK3sVersion extracts the four numeric components of a k3s release tag.
func parseK3sVersion(tag string) ([4]int, error) {
	m := k3sVersionRe.FindStringSubmatch(tag)
	if m == nil {
		return [4]int{}, fmt.Errorf("invalid k3s version %q", tag)
	}
	var v [4]int
	for i := range v {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return [4]int{}, fmt.Errorf("invalid k3s version %q: %w", tag, err)
		}
		v[i] = n
	}
	return v, nil
}

// k3sVersionNewer reports whether release a is newer than release b,
// comparing the Kubernetes triple first and the k3s revision last.
func k3sVersionNewer(a, b string) (bool, error) {
	av, err := parseK3sVersion(a)
	if err != nil {
		return false, err
	}
	bv, err := parseK3sVersion(b)
	if err != nil {
		return false, err
	}
	for i := range av {
		if av[i] != bv[i] {
			return av[i] > bv[i], nil
		}
	}
	return false, nil
}

// k3sVersionProbeCmd reports the installed k3s version with marker lines
// distinguishing "not installed" from "installed but unable to report a
// version" — the SSH layer only returns combined output, and a broken binary
// must not read as absent, because absence authorises a fresh install. The
// command itself always exits zero.
const k3sVersionProbeCmd = "if command -v k3s >/dev/null 2>&1; then k3s --version || echo KIP_K3S_VERSION_FAILED; else echo KIP_K3S_ABSENT; fi"

// parseInstalledK3sVersion interprets k3sVersionProbeCmd output: "" when k3s
// is absent, otherwise the release tag from `k3s --version` output
// (`k3s version v1.36.2+k3s1 (commit)`). The whole output is scanned before
// deciding, and anything other than one unambiguous verdict is an error:
// the failure marker wins over a tag (k3s can print a version and still
// exit nonzero), and an absence marker alongside a tag is a contradiction,
// not proof of absence. Callers use the version to prevent downgrades and
// version skew, so an unreadable or ambiguous probe must not silently pass.
// Markers are matched as whole trimmed lines because the SSH layer returns
// combined output that can carry shell noise.
func parseInstalledK3sVersion(output string) (string, error) {
	absent, failed := false, false
	tag := ""
	for _, line := range strings.Split(output, "\n") {
		switch strings.TrimSpace(line) {
		case "KIP_K3S_ABSENT":
			absent = true
		case "KIP_K3S_VERSION_FAILED":
			failed = true
		}
		if tag == "" {
			for _, field := range strings.Fields(line) {
				if k3sVersionRe.MatchString(field) {
					tag = field
					break
				}
			}
		}
	}
	switch {
	case failed:
		return "", fmt.Errorf("k3s is installed but did not report a version")
	case absent && tag != "":
		return "", fmt.Errorf("conflicting k3s version probe output: absence marker alongside version %s", tag)
	case tag != "":
		return tag, nil
	case absent:
		return "", nil
	}
	return "", fmt.Errorf("could not parse installed k3s version from %q", strings.TrimSpace(output))
}

// decideK3sAgentJoin returns an error when installing the master's k3s
// release on a worker whose installed release is workerInstalled ("" when
// k3s is absent) would be a downgrade. Fresh, equal, and older workers all
// converge on the master's release: an agent carries no datastore, and the
// join target is the control plane's own version, so moving forward any
// distance stays within the Kubernetes skew policy. A newer worker is
// refused because k3s downgrades are unsupported.
func decideK3sAgentJoin(workerInstalled, masterVersion string) error {
	if workerInstalled == "" {
		return nil
	}
	newer, err := k3sVersionNewer(workerInstalled, masterVersion)
	if err != nil {
		return err
	}
	if newer {
		return fmt.Errorf("worker already runs k3s %s, newer than the master's %s: downgrading an agent is unsupported, upgrade the master first", workerInstalled, masterVersion)
	}
	return nil
}

// installedK3sVersion returns the k3s release tag installed on the host, or
// "" when k3s is not present.
func installedK3sVersion(client CommandRunner) (string, error) {
	output, err := client.Run(k3sVersionProbeCmd)
	if err != nil {
		return "", fmt.Errorf("checking installed k3s version: %w", err)
	}
	return parseInstalledK3sVersion(output)
}

// decideK3sInstall reports whether the k3s install script should run against
// a host whose installed release is installed ("" when k3s is absent), given
// the release kip pins. When it should not run, skipReason carries the
// operator-facing explanation. The script runs only for a fresh install, a
// re-install of the pinned release, or a supported upgrade: at most one
// Kubernetes minor forward, within the same major. Downgrades are refused
// because the k3s datastore schema only moves forward, and larger jumps are
// refused because the Kubernetes skew policy forbids skipping minors on
// upgrade.
func decideK3sInstall(installed, pin string) (run bool, skipReason string, err error) {
	if installed == "" {
		return true, "", nil
	}
	iv, err := parseK3sVersion(installed)
	if err != nil {
		return false, "", err
	}
	pv, err := parseK3sVersion(pin)
	if err != nil {
		return false, "", err
	}
	newer, err := k3sVersionNewer(installed, pin)
	if err != nil {
		return false, "", err
	}
	if newer {
		return false, fmt.Sprintf("server already runs k3s %s, newer than the %s this kip installs. Keeping that version and restarting k3s to load its config. Upgrade kip to manage this cluster's k3s", installed, pin), nil
	}
	if iv[0] != pv[0] || pv[1]-iv[1] > 1 {
		return false, fmt.Sprintf("server runs k3s %s, more than one Kubernetes minor behind the %s this kip installs. Keeping that version and restarting k3s to load its config. Upgrade k3s one minor at a time first, then re-run kip install", installed, pin), nil
	}
	return true, "", nil
}

// k3sResolvConfPath is the curated resolv.conf CoreDNS forwards external
// queries to. Pointed at by the resolv-conf setting in k3sConfig.
const k3sResolvConfPath = "/etc/rancher/k3s/resolv.conf"

// k3s config written to /etc/rancher/k3s/config.yaml before installation.
// resolv-conf makes both kubelet and CoreDNS use our curated resolver
// file instead of the host's /etc/resolv.conf. The apiserver flags point at
// files InstallK3s writes before the installer runs — the flags must never
// reference a missing file, or the API server refuses to start: the
// authentication config boots as a valid zero-authenticator stub (upstream
// explicitly supports adding authenticators later via dynamic reload), and
// the audit policy is complete from first boot, so every API action is
// attributed from the cluster's first second.
const k3sConfig = `tls-san:
  - "%s"
disable:
  - traefik
flannel-backend: ` + wireguardBackend + `
resolv-conf: ` + k3sResolvConfPath + `
` + k3sAPIServerArgs + `kubelet-arg:
  - "protect-kernel-defaults=true"
`

// renderK3sConfig renders the server config. addresses is either empty or
// YAML settings for the node's external address and the API's advertised address.
func renderK3sConfig(host, backend, addresses string) string {
	return strings.Replace(fmt.Sprintf(k3sConfig, host),
		"flannel-backend: "+wireguardBackend+"\n", "flannel-backend: "+backend+"\n", 1) + addresses
}

// addressSettings renders the given address settings as YAML, or "" when there
// are none.
func addressSettings(settings map[string]any) (string, error) {
	if len(settings) == 0 {
		return "", nil
	}
	out, err := yaml.Marshal(settings)
	return string(out), err
}

// k3sAddressKeys are the address settings an install writes and preserves.
// k3s advertises the API server on node-external-ip unless advertise-address
// is set, so the two are written together.
var k3sAddressKeys = []string{"advertise-address", "node-external-ip"}

// lookupHostIPs resolves a host name given as --host.
var lookupHostIPs = net.LookupIP

// natExternalIP returns host's IPv4 address when it is absent from the server's
// interfaces, as behind NAT, or "" when it is local. known is false for an IPv6
// host, a hostname resolving to zero or multiple distinct IPv4 addresses, or an
// unreadable interface list or one with no valid IPv4 entries. Advertising the
// NAT address lets DNS checks use the ingress address instead of a private one.
func natExternalIP(client CommandRunner, host string) (ip string, known bool) {
	addr, err := netip.ParseAddr(host)
	if err != nil {
		ips, err := lookupHostIPs(host)
		if err != nil {
			return "", false
		}
		for _, ip := range ips {
			v4 := ip.To4()
			if v4 == nil {
				continue
			}
			// SSH may have reached any of several addresses, so none of them is
			// known to be this server's.
			a := netip.AddrFrom4([4]byte(v4))
			if addr.IsValid() && a != addr {
				return "", false
			}
			addr = a
		}
	}
	if !addr.Is4() {
		return "", false
	}
	out, err := client.Run("ip -o -4 addr show")
	if err != nil {
		return "", false
	}
	seen := false
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[2] != "inet" {
			continue
		}
		p, err := netip.ParsePrefix(fields[3])
		if err != nil {
			continue
		}
		if p.Addr() == addr {
			return "", true
		}
		seen = true
	}
	if !seen {
		return "", false
	}
	return addr.String(), true
}

// routeSourceIP returns the source IPv4 address selected by the host's route to dst.
// The caller must supply a bare IPv4 literal because dst enters a shell command.
// ok is false if the route lookup fails or yields no valid IPv4 source address.
func routeSourceIP(client CommandRunner, dst string) (ip string, ok bool) {
	out, err := client.Run("ip -4 -o route get " + dst)
	if err != nil {
		return "", false
	}
	fields := strings.Fields(out)
	for i := 0; i+1 < len(fields); i++ {
		if fields[i] != "src" {
			continue
		}
		a, err := netip.ParseAddr(fields[i+1])
		if err != nil || !a.Is4() {
			return "", false
		}
		return a.String(), true
	}
	return "", false
}

// configuredAddresses renders the existing k3s address settings as YAML,
// preserving non-null values for k3sAddressKeys. It returns "" when none remain
// or the config is absent or null. Read and parse errors stop the caller from
// overwriting the existing config.
func configuredAddresses(client CommandRunner) (string, error) {
	out, err := client.Run("test ! -f " + k3sConfigPath + " || cat " + k3sConfigPath)
	if err != nil {
		return "", fmt.Errorf("reading %s to keep its address settings: %w", k3sConfigPath, err)
	}
	var config map[string]any
	if err := yaml.Unmarshal([]byte(out), &config); err != nil {
		return "", fmt.Errorf("reading %s to keep its address settings: %w", k3sConfigPath, err)
	}
	settings := map[string]any{}
	for _, k := range k3sAddressKeys {
		if value := config[k]; value != nil {
			settings[k] = value
		}
	}
	return addressSettings(settings)
}

// k3sAPIServerArgs is the API server block, held apart from the rest of the
// config because a cluster installed before it existed has it appended by
// EnsureAPIServerConfig. Install and that repair render the same bytes, so an
// upgraded cluster and a fresh one cannot end up configured differently.
const k3sAPIServerArgs = `kube-apiserver-arg:
  - "authentication-config=` + authnConfigPath + `"
  - "audit-policy-file=` + auditPolicyPath + `"
  - "audit-log-path=/var/lib/rancher/k3s/server/logs/audit.log"
  - "audit-log-maxage=30"
  - "audit-log-maxbackup=10"
  - "audit-log-maxsize=100"
`

// kubeletProtectSysctls are the kernel parameters the kubelet expects when
// protect-kernel-defaults is set. With that flag the kubelet refuses to start
// unless the host already matches these, rather than silently overwriting the
// operator's kernel tunables — so kip sets them before k3s installs. These are
// the upstream Kubernetes node defaults for OOM behaviour, panic-on-oops
// self-healing, and the kernel keyring limits containers rely on.
const kubeletProtectSysctls = `vm.panic_on_oom=0
vm.overcommit_memory=1
kernel.panic=10
kernel.panic_on_oops=1
kernel.keys.root_maxbytes=25000000
kernel.keys.root_maxkeys=1000000
`

// kubeletSysctlPath persists the kubelet sysctls so they survive a reboot and
// are re-applied by systemd-sysctl before k3s starts. The 90- prefix orders it
// ahead of the 99-kipper.conf inotify tuning.
const kubeletSysctlPath = "/etc/sysctl.d/90-kipper-kubelet.conf"

// DefaultDNSResolvers is what CoreDNS forwards external queries to when
// the operator does not pass --dns-resolver. Public resolvers are used
// instead of the host's /etc/resolv.conf because that file varies wildly
// by provider: unreachable IPv6 entries on an IPv4-only cluster, more
// than the three-nameserver limit, or a recursive resolver that
// rate-limits or caches NXDOMAIN long enough to break issuance. Any of
// those intermittently breaks cluster DNS for every workload, which is
// why cert-manager already pins its own resolvers. Returns a fresh slice
// so callers can mutate it. Exported because an empty dns_resolvers in
// the local config means this set, so status compares drift against it.
func DefaultDNSResolvers() []string {
	return []string{"1.1.1.1", "8.8.8.8", "9.9.9.9"}
}

// validateResolver checks a single resolver string is a bare IPv4 literal
// and returns its canonical form. IPv6 is rejected because the cluster pod
// network is IPv4-only, so an IPv6 upstream is unreachable.
func validateResolver(r string) (string, error) {
	addr, err := netip.ParseAddr(r)
	if err != nil {
		return "", fmt.Errorf("invalid DNS resolver %q: must be an IP address", r)
	}
	if !addr.Is4() {
		return "", fmt.Errorf("invalid DNS resolver %q: only IPv4 resolvers are supported (the cluster pod network is IPv4-only)", r)
	}
	return addr.String(), nil
}

// renderResolvConf builds a resolv.conf body from the resolver list, one
// `nameserver` line each.
func renderResolvConf(resolvers []string) string {
	lines := make([]string, len(resolvers))
	for i, r := range resolvers {
		lines[i] = "nameserver " + r
	}
	return strings.Join(lines, "\n") + "\n"
}

// maxDNSResolvers caps the resolver list at the resolv.conf / pod
// dnsConfig nameserver limit. More than this gets silently truncated by
// the kubelet, which is how an over-long host resolv.conf broke cluster
// DNS in the first place.
const maxDNSResolvers = 3

// resolveDNSResolvers validates and normalises operator-supplied
// resolvers, falling back to the default set when none remain. Each
// entry must be a bare IPv4 literal: hostnames, comments, and whitespace
// are rejected (which also closes off any newline that could break out
// of the install heredoc these values flow through), IPv6 is rejected
// because the cluster pod network is IPv4-only so an IPv6 upstream is
// unreachable, duplicates are dropped, and the list is capped at three.
// These are the exact conditions that made the host resolv.conf unsafe.
func resolveDNSResolvers(resolvers []string) ([]string, error) {
	seen := make(map[string]bool)
	cleaned := make([]string, 0, len(resolvers))
	for _, r := range resolvers {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		canon, err := validateResolver(r)
		if err != nil {
			return nil, err
		}
		if seen[canon] {
			continue
		}
		seen[canon] = true
		cleaned = append(cleaned, canon)
	}
	if len(cleaned) == 0 {
		return DefaultDNSResolvers(), nil
	}
	if len(cleaned) > maxDNSResolvers {
		return nil, fmt.Errorf("too many DNS resolvers (%d): at most %d are allowed", len(cleaned), maxDNSResolvers)
	}
	return cleaned, nil
}

// dnsProbeTimeoutSeconds bounds each resolver probe. With at most
// maxDNSResolvers entries the probe adds under ten seconds even when
// every resolver is unreachable.
const dnsProbeTimeoutSeconds = 3

// dnsProbeCommand builds the shell command that checks each resolver
// accepts TCP connections on port 53, run on the host in one SSH round
// trip. Bash's /dev/tcp is used because minimal Ubuntu/Debian hosts
// carry neither dig nor nslookup, and every real resolver serves DNS
// over TCP 53 (RFC 7766 makes TCP support mandatory). The command
// always exits zero; reachability is reported per resolver on stdout.
// Callers pass resolvers that resolveDNSResolvers has already reduced
// to bare IPv4 literals, so interpolating them is shell-safe.
func dnsProbeCommand(resolvers []string) string {
	probes := make([]string, len(resolvers))
	for i, r := range resolvers {
		probes[i] = fmt.Sprintf(
			"if timeout %d bash -c ':< /dev/tcp/%s/53' 2>/dev/null; then echo '%s ok'; else echo '%s unreachable'; fi",
			dnsProbeTimeoutSeconds, r, r, r)
	}
	return strings.Join(probes, "; ")
}

// parseDNSProbeOutput returns the resolvers a dnsProbeCommand run
// reported unreachable.
func parseDNSProbeOutput(output string) []string {
	var unreachable []string
	for _, line := range strings.Split(output, "\n") {
		if fields := strings.Fields(line); len(fields) == 2 && fields[1] == "unreachable" {
			unreachable = append(unreachable, fields[0])
		}
	}
	return unreachable
}

// probeDNSResolvers checks from the host that each resolver accepts TCP
// connections on port 53 and returns the ones that do not. A transport
// failure warns and returns nil: reachability advice is best-effort and
// must not block an install, but a silently skipped check would read as
// "all resolvers fine".
func probeDNSResolvers(client CommandRunner, resolvers []string) []string {
	unreachable, err := ProbeResolvers(client, resolvers)
	if err != nil {
		fmt.Printf("  ⚠  could not check DNS resolver reachability: %v\n", err)
		return nil
	}
	return unreachable
}

// ProbeResolvers checks from the host that each resolver accepts TCP
// connections on port 53 and returns the ones that do not. A transport
// failure is returned as an error, distinct from "all reachable", so
// callers can report an unperformed check instead of a false pass.
// Exported so status can run the install-time reachability probe when
// auditing the live resolver file. Callers must pass validated bare IPv4
// literals (CheckResolvConf output qualifies) — the values are
// interpolated into a shell command.
func ProbeResolvers(client CommandRunner, resolvers []string) ([]string, error) {
	output, err := client.Run(dnsProbeCommand(resolvers))
	if err != nil {
		return nil, err
	}
	return parseDNSProbeOutput(output), nil
}

// ResolversMatch reports whether the live nameserver list matches the
// configured resolver set, order included — resolv.conf order decides
// which resolver is asked first, so a reorder is drift too. Both sides
// are canonicalised before comparing; an entry that fails to parse makes
// the lists differ.
func ResolversMatch(live, configured []string) bool {
	if len(live) != len(configured) {
		return false
	}
	for i := range live {
		l, lerr := validateResolver(live[i])
		c, cerr := validateResolver(configured[i])
		if lerr != nil || cerr != nil || l != c {
			return false
		}
	}
	return true
}

// warnUnreachableResolvers prints a warning per unreachable resolver,
// and a summary warning when none of them answered — the cluster would
// come up with no working external DNS. Warnings only: a resolver can
// be transiently down at install time, and the operator may know their
// network better than the probe does.
func warnUnreachableResolvers(unreachable, all []string) {
	for _, r := range unreachable {
		fmt.Printf("  ⚠  DNS resolver %s did not accept a TCP connection on port 53 from this node\n", r)
	}
	if len(unreachable) > 0 && len(unreachable) == len(all) {
		fmt.Printf("  ⚠  no configured DNS resolver accepted a TCP connection on port 53. External DNS is likely broken in the cluster. Check that this node allows outbound port 53, or pass --dns-resolver with a resolver your network can reach\n")
	}
}

// parseResolvConf extracts the nameserver IPs from a resolv.conf body,
// ignoring comments and non-nameserver directives. Used to re-validate a
// resolver file mirrored from another node before trusting it.
func parseResolvConf(body string) []string {
	var ips []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if fields := strings.Fields(line); len(fields) == 2 && fields[0] == "nameserver" {
			ips = append(ips, fields[1])
		}
	}
	return ips
}

// ResolvConfPath is the host path of the curated resolv.conf CoreDNS
// forwards external queries to. Exported so status tooling can read the
// live file and check it for drift.
func ResolvConfPath() string {
	return k3sResolvConfPath
}

// CheckResolvConf audits a curated resolv.conf body and reports whether
// its nameservers are a safe set for a Kipper cluster. It returns every
// resolver found and a non-nil error when the file would break cluster
// DNS: no entries at all, a non-IPv4 or non-IP entry, or more than the
// nameserver limit. A valid custom set passes — an operator may
// deliberately point the cluster at their own IPv4 resolvers.
//
// Unlike resolveDNSResolvers, which normalises input for a file it is
// about to write, this audits the raw directives: every `nameserver`
// line counts toward the limit (no dedup can hide a fourth entry), and a
// value is validated even when a trailing comment follows it, so an
// unsafe hand-edit is caught rather than silently dropped.
func CheckResolvConf(body string) ([]string, error) {
	resolvers := rawResolvConfNameservers(body)
	if len(resolvers) == 0 {
		return nil, fmt.Errorf("no nameserver entries: the cluster has no external DNS")
	}
	if len(resolvers) > maxDNSResolvers {
		return resolvers, fmt.Errorf("too many DNS resolvers (%d): at most %d are allowed", len(resolvers), maxDNSResolvers)
	}
	for _, r := range resolvers {
		if _, err := validateResolver(r); err != nil {
			return resolvers, err
		}
	}
	return resolvers, nil
}

// rawResolvConfNameservers returns the value of every `nameserver`
// directive in a resolv.conf body, ignoring comments and other
// directives. It keeps the value even when trailing tokens follow (a
// hand-added comment), because a resolver's presence still matters to the
// audit, and every directive is returned separately so duplicates count.
func rawResolvConfNameservers(body string) []string {
	var resolvers []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if fields := strings.Fields(line); len(fields) >= 2 && fields[0] == "nameserver" {
			resolvers = append(resolvers, fields[1])
		}
	}
	return resolvers
}

// writeResolvConf creates the k3s config directory and writes the
// curated resolver file CoreDNS forwards to. The heredoc delimiter is
// quoted so the body is taken literally; callers pass content that
// resolveDNSResolvers has already reduced to bare `nameserver <ip>`
// lines.
func writeResolvConf(client CommandRunner, body string) error {
	if _, err := client.Run("mkdir -p /etc/rancher/k3s"); err != nil {
		return fmt.Errorf("creating k3s config directory: %w", err)
	}
	cmd := fmt.Sprintf("cat > %s << 'KIPEOF'\n%sKIPEOF", k3sResolvConfPath, body)
	if _, err := client.Run(cmd); err != nil {
		return fmt.Errorf("writing k3s resolv.conf: %w", err)
	}
	return nil
}

// writeKubeletSysctls persists and applies the kernel parameters
// protect-kernel-defaults requires. It writes them to a sysctl.d drop-in (so
// systemd-sysctl re-applies them on every boot before k3s starts) and applies
// them live, so the current kubelet start also sees them. A failure here is
// fatal to the install: starting k3s with protect-kernel-defaults against an
// unprepared kernel would leave the kubelet unable to come up.
func writeKubeletSysctls(client CommandRunner) error {
	writeCmd := fmt.Sprintf("cat > %s << 'KIPEOF'\n%sKIPEOF", kubeletSysctlPath, kubeletProtectSysctls)
	if _, err := client.Run(writeCmd); err != nil {
		return fmt.Errorf("writing kubelet sysctls: %w", err)
	}
	if _, err := client.Run("sysctl -p " + kubeletSysctlPath + " >/dev/null"); err != nil {
		return fmt.Errorf("applying kubelet sysctls: %w", err)
	}
	return nil
}

// writeAgentDNSConfig points a worker's kubelet at the curated resolver
// file through a config.yaml.d drop-in, which k3s merges on top of any
// existing config.yaml. A worker join must not clobber agent settings a
// pre-provisioned or retried host may already carry.
func writeAgentDNSConfig(client CommandRunner) error {
	if _, err := client.Run("mkdir -p /etc/rancher/k3s/config.yaml.d"); err != nil {
		return fmt.Errorf("creating k3s config.yaml.d directory: %w", err)
	}
	body := "resolv-conf: " + k3sResolvConfPath + "\n"
	cmd := fmt.Sprintf("cat > /etc/rancher/k3s/config.yaml.d/kipper-dns.yaml << 'KIPEOF'\n%sKIPEOF", body)
	if _, err := client.Run(cmd); err != nil {
		return fmt.Errorf("writing k3s agent dns config: %w", err)
	}
	return nil
}

// writeAgentKubeletConfig gives a worker's kubelet the same
// protect-kernel-defaults posture as the server, via a config.yaml.d drop-in.
// The matching sysctls must be applied (writeKubeletSysctls) before the agent
// starts, exactly as on the server, or the worker kubelet refuses to come up.
func writeAgentKubeletConfig(client CommandRunner) error {
	if _, err := client.Run("mkdir -p /etc/rancher/k3s/config.yaml.d"); err != nil {
		return fmt.Errorf("creating k3s config.yaml.d directory: %w", err)
	}
	body := "kubelet-arg:\n  - \"protect-kernel-defaults=true\"\n"
	cmd := fmt.Sprintf("cat > /etc/rancher/k3s/config.yaml.d/kipper-kubelet.yaml << 'KIPEOF'\n%sKIPEOF", body)
	if _, err := client.Run(cmd); err != nil {
		return fmt.Errorf("writing k3s agent kubelet config: %w", err)
	}
	return nil
}

// InstallK3s installs or updates k3s with Kipper's configuration. Traefik is
// installed separately. CoreDNS uses dnsResolvers or the default public resolvers
// when empty. Repeated calls preserve versions outside the supported upgrade path.
//
// A single IPv4 address supplied by host or resolved from it is advertised as
// the node's external address when absent from the server's interfaces. The API
// server is advertised on the source IPv4 address of the host's route to that
// external address.
// A local address clears both settings. If detection is inconclusive, the
// existing settings are preserved, and an unreadable or invalid config causes
// an error. On an existing cluster, k3s is restarted so that it runs with the
// config written here.
//
// The caller must sample k3sPreexisting before installation to ensure existing
// k3s services reload their configuration and existing CoreDNS deployments
// restart to pick up resolver changes.
//
// Existing clusters require changeNetwork consent to migrate to WireGuard,
// otherwise the function returns ErrNetworkChangeNotConfirmed.
// Migration clears stale flannel links before the new config is written.
// A storageMode of StorageLonghorn requires the iSCSI module on RHEL hosts.
func InstallK3s(client CommandRunner, host string, dnsResolvers []string, k3sPreexisting, changeNetwork bool, storageMode string) error {
	resolvers, err := resolveDNSResolvers(dnsResolvers)
	if err != nil {
		return err
	}

	warnUnreachableResolvers(probeDNSResolvers(client, resolvers), resolvers)

	var change NetworkChange
	if k3sPreexisting {
		if change, err = PlanNetworkChange(client); err != nil {
			return err
		}
	}
	// Allow VXLAN as a fallback when WireGuard is unavailable, provided no
	// network change needs recovery.
	vxlanServer := change.Running == "vxlan" && !change.Interrupted

	// The kubelet sysctls must be in place before k3s installs and before an
	// upgrade restarts the kubelet.
	if err := prepareNodeOS(client, vxlanServer, storageMode == StorageLonghorn); err != nil {
		return err
	}

	// Write k3s config and the curated resolv.conf before install so k3s
	// picks up our settings on first start.
	if err := writeResolvConf(client, renderResolvConf(resolvers)); err != nil {
		return err
	}

	backend := wireguardBackend
	if k3sPreexisting && change.Needed() {
		if !changeNetwork {
			return ErrNetworkChangeNotConfirmed
		}
		err := ChangeNetwork(client)
		if vxlanServer {
			var warning string
			if warning, err = keepNetworkWithoutWireGuard(err); warning != "" {
				fmt.Printf("  ⚠  %s\n", warning)
				backend = flannelLinks["vxlan"].setting
			}
		}
		if err != nil {
			return err
		}
	}

	var addresses string
	ip, known := natExternalIP(client, host)
	var private string
	if known && ip != "" {
		private, known = routeSourceIP(client, ip)
	}
	if !known {
		addresses, err = configuredAddresses(client)
	} else if ip != "" {
		addresses, err = addressSettings(map[string]any{"advertise-address": private, "node-external-ip": ip})
	}
	if err != nil {
		return err
	}
	configContent := renderK3sConfig(host, backend, addresses)
	writeCmd := fmt.Sprintf("cat > /etc/rancher/k3s/config.yaml << 'KIPEOF'\n%sKIPEOF", configContent)
	if _, err := client.Run(writeCmd); err != nil {
		return fmt.Errorf("writing k3s config: %w", err)
	}

	// The files the apiserver flags reference must exist before k3s starts.
	// A fresh install starts k3s afterwards, so the policy it just wrote is the
	// one the API server loads; whether it changed says nothing here.
	if _, err := writeAuthnStubAndAuditPolicy(client); err != nil {
		return err
	}

	// decideK3sInstall limits the script to fresh installs, re-installs and
	// upgrades of at most one minor within the same major. Newer clusters and
	// those too far behind keep their version while the rest of the install
	// proceeds. Re-running the script within these limits is safe.
	installed, err := installedK3sVersion(client)
	if err != nil {
		return err
	}
	runInstall, skipReason, err := decideK3sInstall(installed, k3sVersion)
	if err != nil {
		return err
	}
	if runInstall {
		// The script's change detection covers its binary, service and environment
		// files. Force a restart to load the config written by this run or an
		// interrupted earlier run, even if those files are unchanged.
		force := ""
		if k3sPreexisting {
			force = "INSTALL_K3S_FORCE_RESTART=true "
		}
		installCmd := fmt.Sprintf("curl -sfL https://get.k3s.io | %sINSTALL_K3S_VERSION='%s' INSTALL_K3S_EXEC='server' sh -", force, k3sVersion)
		if _, err := client.Run(installCmd); err != nil {
			return fmt.Errorf("running k3s installer: %w", err)
		}
	} else {
		fmt.Printf("  ⚠  %s\n", skipReason)
		if k3sPreexisting {
			if _, err := client.Run("systemctl restart k3s"); err != nil {
				return fmt.Errorf("restarting k3s to load its new config: %w", err)
			}
		}
	}

	// A freshly installed k3s has not registered its node the instant the
	// installer script returns, and `kubectl wait --all` errors on an empty
	// match rather than waiting for one.
	if err := WaitForNodeReady(client, 5*time.Minute); err != nil {
		return fmt.Errorf("waiting for k3s node to be ready: %w", err)
	}

	// Restore placement after k3s reapplies addon manifests. The later platform
	// placement step retries addons that are still absent here.
	if err := PinK3sAddonsToServer(client); err != nil {
		fmt.Printf("  ⚠  %v\n", err)
	}

	// CoreDNS only re-reads resolv.conf at startup. On a re-run against an
	// existing cluster it is already running with the old upstream, so roll
	// it to pick up the curated file. On a fresh install CoreDNS reads the
	// file when it first starts, so there is nothing to restart — the
	// pre-install sample above is what distinguishes the two.
	if k3sPreexisting {
		if _, err := client.Run("kubectl -n kube-system rollout restart deploy/coredns"); err != nil {
			fmt.Printf("  ⚠  could not restart CoreDNS (it will pick up the resolvers when it next starts): %v\n", err)
		}
	}

	return nil
}

// k3sPreexistingFromSample interprets the pre-install freshness sample
// (`test -d /var/lib/rancher/k3s && echo existing || echo fresh`). Only an
// exact "fresh" marker line, with no competing "existing" line, proves a
// first install; an errored or ambiguous sample counts as existing. The
// check is fail-closed because the fresh verdict skips both the CoreDNS
// restart and the ClusterIdentity read — a substring match could let shell
// noise like "refreshing configuration" masquerade as a fresh host and send
// the installer deriving a new identity for a live cluster. The SSH layer
// combines stdout and stderr, so markers are matched as whole trimmed lines.
func k3sPreexistingFromSample(output string, err error) bool {
	if err != nil {
		return true
	}
	fresh, existing := false, false
	for _, line := range strings.Split(output, "\n") {
		switch strings.TrimSpace(line) {
		case "fresh":
			fresh = true
		case "existing":
			existing = true
		}
	}
	return !fresh || existing
}

// WriteHostDNS rewrites /etc/rancher/k3s/resolv.conf on one node and returns
// the resolver addresses written, preserving their order. It trims whitespace,
// drops empty entries and duplicates, and uses the defaults if none remain.
// It returns an error for invalid IPv4 addresses, more than three distinct
// addresses or a failed write. Failed reachability probes only print warnings.
// Call RestartCoreDNS after writing all nodes so CoreDNS loads the files.
func WriteHostDNS(client CommandRunner, resolvers []string) ([]string, error) {
	cleaned, err := resolveDNSResolvers(resolvers)
	if err != nil {
		return nil, err
	}
	warnUnreachableResolvers(probeDNSResolvers(client, cleaned), cleaned)
	if err := writeResolvConf(client, renderResolvConf(cleaned)); err != nil {
		return nil, err
	}
	return cleaned, nil
}

// RestartCoreDNS requests a CoreDNS rollout restart through the server's
// kubectl so new pods load the resolver files. It returns command errors
// without waiting for the rollout to complete.
func RestartCoreDNS(server CommandRunner) error {
	if _, err := server.Run("kubectl -n kube-system rollout restart deploy/coredns"); err != nil {
		return fmt.Errorf("restarting CoreDNS to pick up the resolvers: %w", err)
	}
	return nil
}

// agentInstallCommand builds an install command that reads the join token from
// stdin to keep it out of process arguments.
func agentInstallCommand(serverVersion, serverURLHost string) string {
	return fmt.Sprintf(
		"read -r K3S_TOKEN && export K3S_TOKEN && curl -sfL https://get.k3s.io | INSTALL_K3S_VERSION='%s' K3S_URL=https://%s:6443 sh -",
		serverVersion, serverURLHost)
}

// WorkerJoin holds the server version and registry credentials for a worker join.
type WorkerJoin struct {
	serverVersion string
	zotIP         string
	pullPassword  string
}

// JoinMaterial contains the server's k3s release, registry ClusterIP, CA
// certificate and pull password, and upstream DNS resolvers for a worker join.
// PullPassword is secret.
type JoinMaterial struct {
	K3sVersion   string   `json:"k3s_version"`
	ZotIP        string   `json:"zot_ip"`
	RegistryCA   string   `json:"registry_ca"`
	PullPassword string   `json:"pull_password"`
	Resolvers    []string `json:"resolvers"`
}

// ReadJoinMaterial reads the server's k3s release, registry configuration and
// upstream DNS resolvers. Missing, unreadable or invalid resolvers fall back
// to DefaultDNSResolvers. It returns an error if k3s is absent or the release
// cannot be read, or if any required registry value is unreadable or empty.
func ReadJoinMaterial(server CommandRunner) (JoinMaterial, error) {
	resolv, rErr := server.Run("cat " + k3sResolvConfPath)
	resolvers, vErr := resolveDNSResolvers(parseResolvConf(resolv))
	if rErr != nil || vErr != nil {
		resolvers = DefaultDNSResolvers()
	}
	// installedK3sVersion validates the tag before shell interpolation.
	version, err := installedK3sVersion(server)
	if err != nil {
		return JoinMaterial{}, err
	}
	if version == "" {
		return JoinMaterial{}, fmt.Errorf("reading k3s version from master: k3s is not installed")
	}
	zotIP, err := server.Run(`kubectl get svc zot -n kipper-system -o jsonpath='{.spec.clusterIP}'`)
	if err != nil {
		return JoinMaterial{}, fmt.Errorf("reading zot ClusterIP from master: %w", err)
	}
	zotIP = strings.TrimSpace(zotIP)
	if zotIP == "" {
		return JoinMaterial{}, fmt.Errorf("zot service has no ClusterIP on the master")
	}
	pullPassword, err := readSecretValue(server, zotNamespace, zotPullSecret, "password")
	if err != nil {
		return JoinMaterial{}, err
	}
	if pullPassword == "" {
		return JoinMaterial{}, fmt.Errorf("registry pull credential missing on the master")
	}
	caPEM, err := readSecretValue(server, zotNamespace, zotTLSSecret, `ca\.crt`)
	if err != nil {
		return JoinMaterial{}, err
	}
	if caPEM == "" {
		return JoinMaterial{}, fmt.Errorf("registry CA missing on the master")
	}
	return JoinMaterial{K3sVersion: version, ZotIP: zotIP, RegistryCA: caPEM, PullPassword: pullPassword, Resolvers: resolvers}, nil
}

var pullPasswordPattern = regexp.MustCompile(`^[A-Za-z0-9]+$`)

// checked validates the values that reach worker commands and returns the
// material with its resolvers normalised.
func (m JoinMaterial) checked() (JoinMaterial, error) {
	if _, err := parseK3sVersion(m.K3sVersion); err != nil {
		return JoinMaterial{}, fmt.Errorf("server k3s release: %w", err)
	}
	if ip, err := netip.ParseAddr(m.ZotIP); err != nil || !ip.Is4() {
		return JoinMaterial{}, fmt.Errorf("registry ClusterIP %q is not an IPv4 address", m.ZotIP)
	}
	if m.RegistryCA == "" {
		return JoinMaterial{}, fmt.Errorf("the registry CA is required")
	}
	// The password reaches registries.yaml and curl's config unescaped.
	if !pullPasswordPattern.MatchString(m.PullPassword) {
		return JoinMaterial{}, fmt.Errorf("the registry pull credential must be nonempty and use the alphanumeric format kip generates")
	}
	resolvers, err := resolveDNSResolvers(m.Resolvers)
	if err != nil {
		return JoinMaterial{}, err
	}
	m.Resolvers = resolvers
	return m, nil
}

// PrepareWorker configures host dependencies, DNS, registry access and
// networkDropIn before agent startup, with the join material read from the
// server. It rejects workers running newer k3s versions than the server.
func PrepareWorker(masterClient, workerClient CommandRunner, networkDropIn string) (WorkerJoin, error) {
	m, err := ReadJoinMaterial(masterClient)
	if err != nil {
		return WorkerJoin{}, err
	}
	return PrepareWorkerFrom(workerClient, m, networkDropIn)
}

// PrepareWorkerFrom configures the worker as PrepareWorker does, using join
// material read earlier so only a worker connection is needed. Before running
// worker commands, it requires a valid k3s release, an IPv4 registry address,
// a nonempty CA and an alphanumeric pull password. It trims and deduplicates
// resolvers, requires at most three IPv4 addresses and uses DefaultDNSResolvers
// when none remain. It rejects workers running a newer k3s release than the server.
func PrepareWorkerFrom(workerClient CommandRunner, m JoinMaterial, networkDropIn string) (WorkerJoin, error) {
	m, err := m.checked()
	if err != nil {
		return WorkerJoin{}, err
	}
	if err := prepareWorkerHost(workerClient, m.Resolvers); err != nil {
		return WorkerJoin{}, err
	}
	// Reject newer workers to prevent an implicit downgrade on retry.
	workerVersion, err := installedK3sVersion(workerClient)
	if err != nil {
		return WorkerJoin{}, err
	}
	if err := decideK3sAgentJoin(workerVersion, m.K3sVersion); err != nil {
		return WorkerJoin{}, err
	}
	// The agent needs the server's registry mirror, credentials and CA at startup.
	if err := writeZotNodeFiles(workerClient, m.RegistryCA, m.ZotIP, m.PullPassword); err != nil {
		return WorkerJoin{}, err
	}
	if _, err := workerClient.RunStdin(writeDropInScript(k3sNetworkConfigPath), strings.NewReader(networkDropIn)); err != nil {
		return WorkerJoin{}, fmt.Errorf("writing %s on the worker: %v", k3sNetworkConfigPath, withoutOutput(err))
	}
	return WorkerJoin{serverVersion: m.K3sVersion, zotIP: m.ZotIP, pullPassword: m.PullPassword}, nil
}

// bootstrapTaint keeps ordinary pods off a node between its registration
// and the cordon that replaces it.
const bootstrapTaint = "kipper.run/bootstrap"

// bootstrapTaintPath holds the registration taint until the node is cordoned.
const bootstrapTaintPath = "/etc/rancher/k3s/config.yaml.d/kipper-bootstrap.yaml"

// Register joins a prepared host at the IPv4 address serverURLHost with a
// fresh one-hour bootstrap token. It returns the node name after confirming
// registration against id. The node is left cordoned, with the bootstrap
// taint removed.
func (j WorkerJoin) Register(masterClient, workerClient CommandRunner, serverURLHost string, id HostIdentity) (string, error) {
	if err := j.checkAgentStart(workerClient, serverURLHost); err != nil {
		return "", err
	}
	// Keep the token valid through registration before attempting cleanup.
	var nodeName string
	err := withJoinToken(masterClient, serverURLHost, func(token string) error {
		if err := j.startCheckedAgent(workerClient, serverURLHost, token); err != nil {
			return err
		}
		name, err := waitForHostNode(masterClient, id, 2*time.Minute, 3*time.Second)
		if err != nil {
			return fmt.Errorf("verifying worker node joined: %w", err)
		}
		nodeName = name
		// The cordon takes over from the taint as soon as the Node exists.
		if _, err := masterClient.Run("kubectl cordon " + name + " >/dev/null"); err != nil {
			return fmt.Errorf("cordoning %s: %w", name, err)
		}
		if _, err := masterClient.Run(removeTaintCommand(name, bootstrapTaint)); err != nil {
			return fmt.Errorf("removing the registration taint from %s: %w", name, err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if _, err := workerClient.Run("rm -f " + bootstrapTaintPath); err != nil {
		return "", fmt.Errorf("removing %s on the worker: %w", bootstrapTaintPath, err)
	}
	return nodeName, nil
}

// StartAgent writes the registration taint and runs the k3s agent installer
// against serverURLHost, an IPv4 address, with a valid k3s bootstrap token on
// stdin. It rejects other drop-ins that set node-taint or node-taint+ because
// they could replace the registration taint. Installer and taint-write errors
// have the token's secret redacted. The caller must verify registration.
func (j WorkerJoin) StartAgent(workerClient CommandRunner, serverURLHost, token string) error {
	if !bootstrapTokenPattern.MatchString(token) {
		return errors.New("the join token is not a k3s bootstrap token")
	}
	if err := j.checkAgentStart(workerClient, serverURLHost); err != nil {
		return err
	}
	return redactToken(j.startCheckedAgent(workerClient, serverURLHost, token), token)
}

func (j WorkerJoin) startCheckedAgent(workerClient CommandRunner, serverURLHost, token string) error {
	taint := fmt.Sprintf("cat > %s << 'KIPEOF'\nnode-taint+:\n  - %s=true:NoSchedule\nKIPEOF", bootstrapTaintPath, bootstrapTaint)
	if _, err := workerClient.Run("mkdir -p /etc/rancher/k3s/config.yaml.d && " + taint); err != nil {
		return fmt.Errorf("writing the registration taint on the worker: %w", err)
	}
	if _, err := workerClient.RunStdin(agentInstallCommand(j.serverVersion, serverURLHost), strings.NewReader(token+"\n")); err != nil {
		return fmt.Errorf("running k3s agent installer: %w", err)
	}
	return nil
}

// checkAgentStart requires an IPv4 server address and rejects other drop-ins
// that set node-taint or node-taint+, which could replace the registration taint.
func (j WorkerJoin) checkAgentStart(workerClient CommandRunner, serverURLHost string) error {
	if ip, err := netip.ParseAddr(serverURLHost); err != nil || !ip.Is4() {
		return fmt.Errorf("the server address %q is not an IPv4 address", serverURLHost)
	}
	out, err := workerClient.Run(configFilesCommand("/etc/rancher/k3s/config.yaml.d/*.yaml", bootstrapTaintPath))
	if err != nil {
		// Omit the Run error because its captured output may contain a token.
		return errors.New("reading the worker's k3s configuration failed")
	}
	files, err := parseConfigFiles(out)
	if err != nil {
		return fmt.Errorf("reading the worker's k3s configuration: %w", err)
	}
	for _, f := range files {
		for key := range f.doc {
			if strings.TrimSuffix(key, "+") == "node-taint" {
				return fmt.Errorf("%s sets node-taint, which can replace the taint that keeps workloads off the node until it is checked. Move those taints to /etc/rancher/k3s/config.yaml and run the command again", f.path)
			}
		}
	}
	return nil
}

// waitForHostNode polls for registration matching both machine and boot IDs.
// Duplicate matches fail immediately; lookup errors are retried until timeout.
// A reboot after the identity read changes the boot ID and causes a timeout.
func waitForHostNode(master CommandRunner, id HostIdentity, timeout, interval time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	var last error
	for {
		name, err := NodeForMachineBoot(master, id)
		if err == nil {
			return name, nil
		}
		if errors.Is(err, errSharedMachineID) {
			return "", err
		}
		last = err
		if time.Now().After(deadline) {
			return "", fmt.Errorf("could not confirm the worker registered within %s: %w", timeout, last)
		}
		time.Sleep(interval)
	}
}

// FetchKubeconfig retrieves the k3s kubeconfig from the remote server
// and replaces the loopback API server address with host.
func FetchKubeconfig(client CommandRunner, host string) (string, error) {
	output, err := client.Run("cat /etc/rancher/k3s/k3s.yaml")
	if err != nil {
		// Unwrap the SSH runner's error to omit command output, which may contain
		// the cluster's admin key.
		cause := errors.Unwrap(err)
		if cause == nil {
			cause = errors.New("the command failed")
		}
		return "", fmt.Errorf("reading kubeconfig: %v", cause)
	}

	rewritten := replaceServerAddress(output, host)
	return rewritten, nil
}

func replaceServerAddress(kubeconfig string, host string) string {
	return replaceAll(kubeconfig,
		"https://127.0.0.1:6443",
		fmt.Sprintf("https://%s:6443", host),
	)
}
