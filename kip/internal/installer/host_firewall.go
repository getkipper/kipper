package installer

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
)

// k3sPodCIDR matches the default used by the k3s installer. If the installer
// makes the pod CIDR configurable, both firewall backends must use that value.
const k3sPodCIDR = "10.42.0.0/16"

// firewallClaimPath records ownership of the UFW ruleset after the first
// configuration change, allowing retries to repair a partially applied ruleset.
const firewallClaimPath = "/etc/kipper/firewall-managed"

// FirewallAudit reports active host firewall managers and Kipper ownership.
// Cloud firewalls are outside the scope of this audit.
type FirewallAudit struct {
	Existing      string
	KipperManaged bool
	Adoptable     bool
}

// BlocksConfiguration reports whether ownership prevents configuration
// without explicit adoption.
func (a FirewallAudit) BlocksConfiguration() bool {
	return a.Existing != "" && !a.KipperManaged
}

// FirewallPlan is what a caller should do about the firewall on this host, and
// what to tell the operator about it.
type FirewallPlan struct {
	Configure  bool
	Notice     string
	FlagNotice string
	Adopt      bool
}

// PlanFirewall shares firewall configuration decisions and notices between
// kip install and kip cluster harden.
func PlanFirewall(audit FirewallAudit, wantFirewall, adopt bool) FirewallPlan {
	adopting := wantFirewall && adopt && audit.Adoptable
	mayConfigure := !audit.BlocksConfiguration() || adopting
	plan := FirewallPlan{Configure: wantFirewall && mayConfigure, Adopt: adopting}

	switch {
	case audit.Existing == "":
		plan.Notice = "  ✔  No existing host firewall\n"
	case audit.KipperManaged:
		plan.Notice = "  ✔  Existing firewall was set up by kip\n"
	case adopting:
		plan.Notice = "  ⚠  Existing firewalld detected. kip will replace its public zone and add a pod zone and forwarding policies (--adopt-firewall).\n"
	default:
		plan.Notice = fmt.Sprintf(
			"  ⚠  Existing firewall detected (%s). Skipping kip's firewall step to avoid trampling your rules.\n"+
				"     Your firewall is now your responsibility. Review it regularly and make sure\n"+
				"     22, 80, 443, and 6443 are reachable.\n", audit.Existing)
	}

	if !wantFirewall && mayConfigure {
		tail := "     Your host has no firewall. Configure one yourself, or re-run with --firewall.\n"
		if audit.KipperManaged {
			tail = "     The firewall kip set up earlier keeps the rules it already has.\n"
		}
		plan.FlagNotice = "  ⚠  Firewall configuration skipped (--firewall=false).\n" + tail
	}

	return plan
}

// FirewallRunner runs firewall commands and checks SSH connectivity.
type FirewallRunner interface {
	CommandRunner
	SurvivesSSHRateLimit() bool
	// ConfirmFreshConnection returns localrun.ErrNoConnection when no external
	// SSH connectivity check is available.
	ConfirmFreshConnection() error
}

// ApplyFirewallPlan configures firewalld on RHEL-family hosts or UFW elsewhere
// when the plan permits it. Each backend rechecks ownership before changing rules.
func ApplyFirewallPlan(client FirewallRunner, plan FirewallPlan, rateLimitSSH bool) error {
	if !plan.Configure {
		return nil
	}
	return configureFirewallWith(client, rateLimitSSH, plan.Adopt)
}

// AuditFirewall detects whether a host firewall is already active, and whether
// it is one Kipper established.
func AuditFirewall(client CommandRunner) (FirewallAudit, error) {
	var audit FirewallAudit

	// Probe commands tolerate missing or inactive services. A runner error
	// must stop configuration rather than be treated as an absent firewall.
	out, err := client.Run("systemctl is-active firewalld 2>/dev/null || true")
	if err != nil {
		return audit, fmt.Errorf("checking for firewalld: %w", err)
	}
	if strings.TrimSpace(out) == "active" {
		audit.Existing = "firewalld"
		// Honor firewalld ownership only on the family this backend supports.
		host, err := DetectHostOS(client)
		if err != nil {
			return audit, fmt.Errorf("checking for firewalld: %w", err)
		}
		audit.KipperManaged = host.Family == FamilyRHEL && firewalldClaimed(client)
		audit.Adoptable = host.Family == FamilyRHEL && !audit.KipperManaged
		// Two active managers block configuration even when one is claimed.
		out, err := client.Run("ufw status 2>/dev/null | awk 'NR==1 {print $2}'")
		if err != nil {
			return audit, fmt.Errorf("checking for ufw: %w", err)
		}
		if strings.TrimSpace(out) == "active" {
			audit.Existing, audit.KipperManaged, audit.Adoptable = "firewalld and ufw", false, false
		}
		return audit, nil
	}

	// ufw status prints "Status: active" or "Status: inactive". A missing ufw
	// binary leaves awk to set the exit status, and awk succeeds on no input.
	out, err = client.Run("ufw status 2>/dev/null | awk 'NR==1 {print $2}'")
	if err != nil {
		return audit, fmt.Errorf("checking for ufw: %w", err)
	}
	if strings.TrimSpace(out) == "active" {
		audit.Existing = "ufw"
		audit.KipperManaged = firewallClaimed(client)
		return audit, nil
	}

	return audit, nil
}

// firewallClaimed requires the runner to confirm a nonempty ownership file.
// A failed check leaves an active firewall under operator control.
func firewallClaimed(client CommandRunner) bool {
	out, err := client.Run("test -s " + firewallClaimPath + " && echo claimed || true")
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) == "claimed"
}

// claimCommand builds an ownership claim for the UFW ruleset.
func claimCommand(claimPath string) string {
	return claimCommandFor(claimPath, "ufw ruleset")
}

// claimCommandFor builds an ownership claim naming the managed configuration.
// A unique temporary file in the same directory allows atomic publication by
// rename after a successful write. The trap removes any unpublished file.
func claimCommandFor(claimPath, what string) string {
	note := "The " + what + " on this host is managed by Kipper (kip install, kip cluster harden).\n" +
		"Delete this file to take the firewall over, and kip will leave its rules alone.\n"

	dir := path.Dir(claimPath)
	return fmt.Sprintf(
		`mkdir -p %s && tmp=$(mktemp %s) && trap 'rm -f "$tmp"' EXIT && printf %s > "$tmp" && mv "$tmp" %s`,
		shellQuote(dir), shellQuote(path.Join(dir, path.Base(claimPath)+".XXXXXX")),
		shellQuote(note), shellQuote(claimPath))
}

// configureFirewallWith selects the host's firewall backend. UFW rate limiting
// requires a runner that can continue under the limit; firewalld uses an allow rule.
func configureFirewallWith(client FirewallRunner, rateLimitSSH, adopt bool) error {
	host, err := DetectHostOS(client)
	if err != nil {
		return fmt.Errorf("configuring the firewall: %w", err)
	}
	if host.Family == FamilyRHEL {
		if rateLimitSSH {
			fmt.Printf("  ℹ  Kipper's firewalld configuration allows SSH without rate limiting\n")
		}
		return configureFirewalld(client, host, adopt)
	}
	return configureFirewall(client, host, rateLimitSSH && client.SurvivesSSHRateLimit())
}

func configureFirewall(client CommandRunner, host HostOS, rateLimitSSH bool) error {
	// Refresh package lists so UFW can be installed on a fresh host.
	install := host.InstallPackagesCommand(true, "ufw")
	if install == "" {
		return fmt.Errorf("installing ufw: kip cannot install packages on %s", host.ID)
	}
	if _, err := client.Run(install + " >/dev/null"); err != nil {
		return fmt.Errorf("installing ufw: %w", err)
	}

	sshPort, err := readSSHPort(client)
	if err != nil {
		return err
	}

	// The caller audited before host hardening and the k3s install, so its
	// answer can be minutes old. Ask again with nothing changed yet, so a
	// firewall that came up in the meantime is still somebody else's.
	audit, err := AuditFirewall(client)
	if err != nil {
		return fmt.Errorf("re-auditing firewall: %w", err)
	}
	if audit.BlocksConfiguration() {
		return fmt.Errorf("a %s firewall became active on this host, so kip left it alone", audit.Existing)
	}

	// FORWARD policy must be ACCEPT for kube-proxy NAT and Flannel
	// vxlan to forward pod traffic. UFW defaults this to DROP, which
	// silently breaks pod-to-pod and pod-to-service connectivity.
	forwardCmd := `sed -i 's|^DEFAULT_FORWARD_POLICY=.*|DEFAULT_FORWARD_POLICY="ACCEPT"|' /etc/default/ufw`

	// Record ownership after the forward-policy command succeeds so retries
	// recognize partially configured firewalls. The command and claim are
	// sequenced, not atomic: claim failure can leave an unclaimed policy edit.
	if _, err := client.Run(forwardCmd + " && " + claimCommand(firewallClaimPath)); err != nil {
		return fmt.Errorf("setting the ufw forward policy and claiming the firewall: %w", err)
	}

	// A saved rule can survive a failed reload. Reload and check the live SSH
	// rule below even if retrying a rule command succeeds.
	var ruleErr error
	for _, rule := range ufwRules(sshPort, rateLimitSSH) {
		if err := runUFW(client, rule); err != nil {
			ruleErr = fmt.Errorf("applying %q: %w", rule, err)
			break
		}
	}

	if err := loadSavedRules(client, sshPort); err != nil {
		// A firewall that denies by default without the rule letting SSH in
		// locks the host out, so it is switched off and the install fails.
		if stopErr := stopUFW(client); stopErr != nil {
			return errors.Join(ruleErr, fmt.Errorf("ufw would not load its rules (%w) and could not be switched off either (%v), so the host may refuse SSH; switch ufw off from the provider's console, then re-run kip install", err, stopErr))
		}
		return errors.Join(ruleErr, fmt.Errorf("ufw would not load its rules, so it was left not active; re-run kip install: %w", err))
	}
	return ruleErr
}

// Retry transient UFW failures while k3s updates netfilter during startup.
const ufwAttempts = 5

var ufwRetryDelay = 3 * time.Second

// runUFW runs a ufw command, retrying a failure with a growing pause. Every
// command it runs is idempotent on disk.
func runUFW(client CommandRunner, command string) error {
	var err error
	for attempt := 1; attempt <= ufwAttempts; attempt++ {
		if _, err = client.Run(command + " >/dev/null"); err == nil {
			return nil
		}
		if attempt < ufwAttempts {
			time.Sleep(ufwRetryDelay * time.Duration(attempt))
		}
	}
	return err
}

// loadSavedRules stops UFW before each start to recover partially loaded rules.
// Each attempt checks that the live user-input chain contains the SSH port rule.
func loadSavedRules(client CommandRunner, sshPort string) error {
	var err error
	for attempt := 1; attempt <= ufwAttempts; attempt++ {
		if err = stopUFW(client); err == nil {
			_, err = client.Run("ufw --force enable >/dev/null")
			if err == nil && !sshRuleLoaded(client, sshPort) {
				err = fmt.Errorf("the rule for SSH port %s is not loaded", sshPort)
			}
			if err == nil {
				return nil
			}
		}
		if attempt < ufwAttempts {
			time.Sleep(ufwRetryDelay * time.Duration(attempt))
		}
	}
	return err
}

// stopUFW retries disabling UFW, then checks that its user-input chain is gone.
func stopUFW(client CommandRunner) error {
	if err := runUFW(client, "ufw --force disable"); err != nil {
		return err
	}
	// Only iptables' own answer for a missing chain proves it gone; a query
	// that failed for any other reason proves nothing.
	out, err := client.Run(`out=$(iptables -S ufw-user-input 2>&1); rc=$?; ` +
		`if [ $rc -eq 0 ]; then echo present; elif printf '%s' "$out" | grep -q 'No chain/target/match'; then echo absent; ` +
		`else echo "unknown: $out"; exit 1; fi`)
	switch strings.TrimSpace(out) {
	case "absent":
		return nil
	case "present":
		return fmt.Errorf("ufw's chains are still loaded after switching it off")
	}
	return fmt.Errorf("could not tell whether ufw's chains are gone: %v", err)
}

// sshRuleLoaded checks for a rule matching the SSH port in UFW's live input chain.
func sshRuleLoaded(client CommandRunner, sshPort string) bool {
	out, err := client.Run("iptables -S ufw-user-input 2>/dev/null")
	if err != nil {
		return false
	}
	return strings.Contains(out+"\n", "--dport "+sshPort+" ") || strings.Contains(out+"\n", "--dport "+sshPort+"\n")
}

// k3s host ports pods legitimately reach on the node's INPUT chain.
const (
	// kubeletPort is scraped by metrics-server and Prometheus (both pods).
	kubeletPort = "10250"
	// nodeExporterPort is scraped by Prometheus; node-exporter is a
	// host-network DaemonSet, so the scrape lands on the node IP.
	nodeExporterPort = "9100"
)

// ufwRules returns the rules applied to a fresh Kipper host. SSH must come
// before `ufw --force enable` to avoid locking out the installer.
//
// Pod→host traffic is allowed only on the specific ports k3s needs, rather
// than admitting the whole pod CIDR to every host port — a compromised pod
// must not reach SSH, the k3s supervisor, or any other host service. The
// allowed pod→host ports are the kubelet (metrics-server, Prometheus) and
// node-exporter (Prometheus). Pod→apiserver arrives on 6443 (the
// kubernetes.default service VIP is DNAT'd to the node's apiserver before it
// reaches INPUT), already covered by the 6443 rule. Host-initiated
// connections to pods (Longhorn iSCSI, admission webhooks) return via UFW's
// default ESTABLISHED,RELATED accept, so they need no pod→host rule. The
// service CIDR needs no rule at all: a service IP is virtual and DNAT'd away
// before routing, so no packet ever arrives on INPUT with a service-CIDR
// source or an un-rewritten service-CIDR destination.
func ufwRules(sshPort string, rateLimitSSH bool) []string {
	// CI and operators behind a shared NAT can exceed the per-source limit;
	// --no-ssh-rate-limit selects an unrestricted SSH rule.
	sshRule := fmt.Sprintf("ufw limit %s/tcp", sshPort)
	if !rateLimitSSH {
		sshRule = fmt.Sprintf("ufw allow %s/tcp", sshPort)
	}
	return []string{
		"ufw default deny incoming",
		"ufw default allow outgoing",
		sshRule,
		"ufw allow 80/tcp",
		"ufw allow 443/tcp",
		"ufw allow 6443/tcp",
		fmt.Sprintf("ufw allow from %s to any port %s proto tcp", k3sPodCIDR, kubeletPort),
		fmt.Sprintf("ufw allow from %s to any port %s proto tcp", k3sPodCIDR, nodeExporterPort),
	}
}

// readSSHPort returns the effective sshd port. `sshd -T` prints the
// running configuration including defaults, so this works even when
// /etc/ssh/sshd_config is empty.
func readSSHPort(client CommandRunner) (string, error) {
	out, err := client.Run(`sshd -T 2>/dev/null | awk '$1 == "port" {print $2; exit}'`)
	if err != nil {
		return "", fmt.Errorf("reading sshd port: %w", err)
	}
	port := strings.TrimSpace(out)
	if port == "" {
		port = "22"
	}
	return port, nil
}
