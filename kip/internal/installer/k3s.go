package installer

import (
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"

	"github.com/getkipper/kipper/kip/internal/ssh"
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
func installedK3sVersion(client *ssh.Client) (string, error) {
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
		return false, fmt.Sprintf("server already runs k3s %s, newer than the %s this kip installs. Leaving k3s unchanged. Upgrade kip to manage this cluster's k3s", installed, pin), nil
	}
	if iv[0] != pv[0] || pv[1]-iv[1] > 1 {
		return false, fmt.Sprintf("server runs k3s %s, more than one Kubernetes minor behind the %s this kip installs. Leaving k3s unchanged. Upgrade k3s one minor at a time first, then re-run kip install", installed, pin), nil
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
resolv-conf: ` + k3sResolvConfPath + `
kube-apiserver-arg:
  - "authentication-config=` + authnConfigPath + `"
  - "audit-policy-file=` + auditPolicyPath + `"
  - "audit-log-path=/var/lib/rancher/k3s/server/logs/audit.log"
  - "audit-log-maxage=30"
  - "audit-log-maxbackup=10"
  - "audit-log-maxsize=100"
kubelet-arg:
  - "protect-kernel-defaults=true"
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
func probeDNSResolvers(client *ssh.Client, resolvers []string) []string {
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
func ProbeResolvers(client *ssh.Client, resolvers []string) ([]string, error) {
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
		fmt.Printf("  ⚠  DNS resolver %s did not accept a TCP connection on port 53 from the server\n", r)
	}
	if len(unreachable) > 0 && len(unreachable) == len(all) {
		fmt.Printf("  ⚠  no configured DNS resolver accepted a TCP connection on port 53 — external DNS is likely broken in the cluster. Check that the server allows outbound port 53, or pass --dns-resolver with a resolver your network can reach\n")
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
		return nil, fmt.Errorf("no nameserver entries — the cluster has no external DNS")
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
func writeResolvConf(client *ssh.Client, body string) error {
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
func writeKubeletSysctls(client *ssh.Client) error {
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
func writeAgentDNSConfig(client *ssh.Client) error {
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
func writeAgentKubeletConfig(client *ssh.Client) error {
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

// InstallK3s installs k3s on the remote server with Kipper's
// opinionated configuration. Traefik is disabled because we install
// it separately with custom settings. CoreDNS is pointed at
// dnsResolvers (or the default public resolvers when empty) rather than
// the host's resolv.conf. Safe to re-run — the k3s installer is
// idempotent and will upgrade or no-op as needed.
//
// k3sPreexisting decides the post-install CoreDNS restart and must have
// been sampled BEFORE this function runs (the installer does it while
// reading the existing cluster's identity): a fresh k3s server deploys
// CoreDNS as soon as the apiserver is up, so an after-the-fact existence
// check cannot tell a re-run from a fresh install.
func InstallK3s(client *ssh.Client, host string, dnsResolvers []string, k3sPreexisting bool) error {
	resolvers, err := resolveDNSResolvers(dnsResolvers)
	if err != nil {
		return err
	}

	warnUnreachableResolvers(probeDNSResolvers(client, resolvers), resolvers)

	// protect-kernel-defaults makes the kubelet refuse to start unless these
	// sysctls are already set, so they must be in place before k3s installs
	// (and before an upgrade restarts the kubelet). Applied live and persisted.
	if err := writeKubeletSysctls(client); err != nil {
		return err
	}

	// Write k3s config and the curated resolv.conf before install so k3s
	// picks up our settings on first start.
	if err := writeResolvConf(client, renderResolvConf(resolvers)); err != nil {
		return err
	}

	configContent := fmt.Sprintf(k3sConfig, host)
	writeCmd := fmt.Sprintf("cat > /etc/rancher/k3s/config.yaml << 'KIPEOF'\n%sKIPEOF", configContent)
	if _, err := client.Run(writeCmd); err != nil {
		return fmt.Errorf("writing k3s config: %w", err)
	}

	// The files the apiserver flags reference must exist before k3s starts.
	if err := writeAuthnStubAndAuditPolicy(client); err != nil {
		return err
	}

	// The install script only runs when converging on the pin is supported —
	// fresh install, re-install, or an at-most-one-minor upgrade. Anything
	// else (an older kip re-run against a newer cluster, or a cluster too
	// far behind) leaves k3s alone and lets the rest of the install proceed;
	// decideK3sInstall documents the policy. Within that envelope the script
	// is idempotent: re-running it will upgrade or no-op safely.
	installed, err := installedK3sVersion(client)
	if err != nil {
		return err
	}
	runInstall, skipReason, err := decideK3sInstall(installed, k3sVersion)
	if err != nil {
		return err
	}
	if runInstall {
		installCmd := fmt.Sprintf("curl -sfL https://get.k3s.io | INSTALL_K3S_VERSION='%s' INSTALL_K3S_EXEC='server' sh -", k3sVersion)
		if _, err := client.Run(installCmd); err != nil {
			return fmt.Errorf("running k3s installer: %w", err)
		}
	} else {
		fmt.Printf("  ⚠  %s\n", skipReason)
	}

	// Wait for k3s to be ready
	if _, err := client.Run("kubectl wait --for=condition=Ready node --all --timeout=120s"); err != nil {
		return fmt.Errorf("waiting for k3s node to be ready: %w", err)
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

// RepairHostDNS rewrites the curated resolv.conf from the given resolver set
// (the defaults when empty) and restarts CoreDNS to pick it up. It is the
// scoped remediation for resolver drift on a live cluster; nothing else about
// the installation is touched. Returns the resolver set it wrote.
func RepairHostDNS(client *ssh.Client, resolvers []string) ([]string, error) {
	cleaned, err := resolveDNSResolvers(resolvers)
	if err != nil {
		return nil, err
	}
	warnUnreachableResolvers(probeDNSResolvers(client, cleaned), cleaned)
	if err := writeResolvConf(client, renderResolvConf(cleaned)); err != nil {
		return nil, err
	}
	if _, err := client.Run("kubectl -n kube-system rollout restart deploy/coredns"); err != nil {
		return nil, fmt.Errorf("restarting CoreDNS to pick up the resolvers: %w", err)
	}
	return cleaned, nil
}

// JoinWorkerNode joins a new worker node to an existing k3s cluster.
// The token is read from the master node.
func JoinWorkerNode(masterClient *ssh.Client, workerClient *ssh.Client, masterHost string) error {
	// Get the join token from the master
	token, err := masterClient.Run("cat /var/lib/rancher/k3s/server/node-token")
	if err != nil {
		return fmt.Errorf("reading node token from master: %w", err)
	}

	// Mirror the server's curated resolvers onto the worker and point the
	// agent at them, so CoreDNS keeps forwarding to the same upstreams if
	// it reschedules onto this node. Re-validate the master's file rather
	// than copying it verbatim: a partial or hand-edited file must not
	// propagate bad DNS to a node CoreDNS may land on. Any read or
	// validation failure falls back to the defaults.
	masterResolv, rErr := masterClient.Run("cat " + k3sResolvConfPath)
	resolvers, vErr := resolveDNSResolvers(parseResolvConf(masterResolv))
	if rErr != nil || vErr != nil {
		resolvers = DefaultDNSResolvers()
	}
	// The worker probes too: CoreDNS can reschedule here, and a worker
	// often sits behind different egress rules than the master.
	warnUnreachableResolvers(probeDNSResolvers(workerClient, resolvers), resolvers)
	if err := writeResolvConf(workerClient, renderResolvConf(resolvers)); err != nil {
		return err
	}
	if err := writeAgentDNSConfig(workerClient); err != nil {
		return err
	}
	// Match the server's kubelet hardening: sysctls (applied live) before the
	// agent starts, then the protect-kernel-defaults drop-in.
	if err := writeKubeletSysctls(workerClient); err != nil {
		return err
	}
	if err := writeAgentKubeletConfig(workerClient); err != nil {
		return err
	}

	// The agent is pinned to the master's exact k3s version, not kip's own
	// pin: a node newer than the control plane violates the Kubernetes skew
	// policy, and kip's pin moves ahead of clusters installed earlier. The
	// tag is regex-validated by the parse, so interpolating it is shell-safe.
	serverVersion, err := installedK3sVersion(masterClient)
	if err != nil {
		return err
	}
	if serverVersion == "" {
		return fmt.Errorf("reading k3s version from master: k3s is not installed")
	}

	// The worker gets the same probe: a re-run of node add against a host
	// already running a newer k3s than the master must not downgrade it;
	// decideK3sAgentJoin documents the policy.
	workerVersion, err := installedK3sVersion(workerClient)
	if err != nil {
		return err
	}
	if err := decideK3sAgentJoin(workerVersion, serverVersion); err != nil {
		return err
	}

	// The worker's containerd pulls app images from the cluster registry,
	// and k3s reads registry mirrors, credentials, and the CA only from
	// node-local files, so they must be on the worker before the agent
	// starts. All three come from the master, which holds the authoritative
	// copies.
	zotIP, err := masterClient.Run(`kubectl get svc zot -n kipper-system -o jsonpath='{.spec.clusterIP}'`)
	if err != nil {
		return fmt.Errorf("reading zot ClusterIP from master: %w", err)
	}
	zotIP = strings.TrimSpace(zotIP)
	if zotIP == "" {
		return fmt.Errorf("zot service has no ClusterIP on the master")
	}
	pullPassword, err := readSecretValue(masterClient, zotNamespace, zotPullSecret, "password")
	if err != nil {
		return err
	}
	if pullPassword == "" {
		return fmt.Errorf("registry pull credential missing on the master")
	}
	caPEM, err := readSecretValue(masterClient, zotNamespace, zotTLSSecret, `ca\.crt`)
	if err != nil {
		return err
	}
	if caPEM == "" {
		return fmt.Errorf("registry CA missing on the master")
	}
	if err := writeZotNodeFiles(workerClient, caPEM, zotIP, pullPassword); err != nil {
		return err
	}

	// Install k3s agent on the worker — idempotent within the join policy
	installCmd := fmt.Sprintf(
		"curl -sfL https://get.k3s.io | INSTALL_K3S_VERSION='%s' K3S_URL=https://%s:6443 K3S_TOKEN=%s sh -",
		serverVersion, masterHost, strings.TrimSpace(token),
	)
	if _, err := workerClient.Run(installCmd); err != nil {
		return fmt.Errorf("running k3s agent installer: %w", err)
	}

	// Verify the worker appears as a node on the master
	verifyCmd := "kubectl get nodes --no-headers | wc -l"
	if _, err := masterClient.Run(verifyCmd); err != nil {
		return fmt.Errorf("verifying worker node joined: %w", err)
	}

	return nil
}

// FetchKubeconfig retrieves the k3s kubeconfig from the remote server
// and rewrites the server address to point to the public host.
func FetchKubeconfig(client *ssh.Client, host string) (string, error) {
	output, err := client.Run("cat /etc/rancher/k3s/k3s.yaml")
	if err != nil {
		return "", fmt.Errorf("reading kubeconfig: %w", err)
	}

	// k3s writes https://127.0.0.1:6443 — replace with the public IP
	rewritten := replaceServerAddress(output, host)
	return rewritten, nil
}

func replaceServerAddress(kubeconfig string, host string) string {
	return replaceAll(kubeconfig,
		"https://127.0.0.1:6443",
		fmt.Sprintf("https://%s:6443", host),
	)
}
