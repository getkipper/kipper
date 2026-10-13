package installer

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
)

// k3sPodCIDR is the k3s default pod CIDR. Kipper does not customize k3s
// today, so this default is correct. If a --cluster-cidr flag is ever
// exposed in the installer, ufwRules must read the actual value from k3s
// instead of this constant. The service CIDR needs no firewall rule
// (service IPs are DNAT'd before they reach the host INPUT chain), so it is
// deliberately not a constant here.
const k3sPodCIDR = "10.42.0.0/16"

// firewallClaimPath records that Kipper's first configuration command ran to
// completion on this host, which is what makes the ruleset Kipper's. It is
// attempted in that same command and reached only if the rest of it succeeded,
// so a run that fails earlier leaves no claim, and one that fails later leaves
// a claim over the half-built firewall it left, which is what a retry needs.
const firewallClaimPath = "/etc/kipper/firewall-managed"

// FirewallAudit reports an existing host firewall configuration that
// Kipper should not modify. Cloud-side firewalls (Hetzner Cloud
// Firewall, AWS Security Groups, etc.) are external to the host and
// are not visible to this audit.
type FirewallAudit struct {
	// Existing names the active host firewall manager. Empty means
	// no managed firewall was detected and Kipper may install one.
	Existing string
	// KipperManaged reports that the active firewall is one Kipper set up
	// rather than an admin's own, which makes it Kipper's to reconfigure.
	KipperManaged bool
}

// BlocksConfiguration reports that the firewall on this host belongs to
// someone else, so Kipper must leave it alone.
func (a FirewallAudit) BlocksConfiguration() bool {
	return a.Existing != "" && !a.KipperManaged
}

// FirewallPlan is what a caller should do about the firewall on this host, and
// what to tell the operator about it.
type FirewallPlan struct {
	// Configure reports that kip should apply its ruleset to this host.
	Configure bool
	// Notice describes the firewall the audit found, ready to print.
	Notice string
	// FlagNotice explains an opt-out. Empty unless the operator turned off a
	// firewall step kip would otherwise have run.
	FlagNotice string
}

// PlanFirewall decides what to do about the firewall an audit found, and what
// the operator is told about it. Both `kip install` and `kip cluster harden`
// go through it, so neither the decision nor its explanation can drift.
func PlanFirewall(audit FirewallAudit, wantFirewall bool) FirewallPlan {
	mayConfigure := !audit.BlocksConfiguration()
	plan := FirewallPlan{Configure: wantFirewall && mayConfigure}

	// The notice states what was found. Announcing the work belongs to the
	// step that does it, which --firewall=false stops from running at all.
	switch {
	case audit.Existing == "":
		plan.Notice = "  ✔  No existing host firewall\n"
	case audit.KipperManaged:
		plan.Notice = "  ✔  Existing firewall was set up by kip\n"
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

// FirewallRunner runs the firewall's commands and says whether the SSH rate
// limit is safe for the connection the installer itself is using.
type FirewallRunner interface {
	CommandRunner
	SurvivesSSHRateLimit() bool
}

// ApplyFirewallPlan installs UFW and applies a k3s-correct ruleset when the
// plan calls for it, and does nothing when it does not: SSH (on the configured
// port), HTTP/HTTPS, the k3s API, and pod→host access limited to the specific
// ports k3s needs (the kubelet and node-exporter). Sets the FORWARD policy to
// ACCEPT in /etc/default/ufw so it persists across reloads.
//
// Idempotent: re-running adds no duplicate rules. Switching the SSH rule
// between allow and limit is also safe, because ufw matches a rule by its port
// and protocol and replaces the action in place rather than leaving both rules
// for the earlier one to win.
//
// This is the only way to configure a firewall from outside this package, so
// the plan's refusal is enforced by the code that changes the host rather than
// trusted to each caller. It audits again itself before starting, because the
// caller's plan predates host hardening and the k3s install. A cluster whose
// firewall predates the claim file carries no claim, so it reads as an admin's
// and keeps its rules until it is rebuilt (pre-release clusters are rebuilt,
// not migrated in place).
func ApplyFirewallPlan(client FirewallRunner, plan FirewallPlan, rateLimitSSH bool) error {
	if !plan.Configure {
		return nil
	}
	return configureFirewallWith(client, rateLimitSSH)
}

// AuditFirewall detects whether a host firewall is already active, and whether
// it is one Kipper established.
func AuditFirewall(client CommandRunner) (FirewallAudit, error) {
	var audit FirewallAudit

	// firewalld is asked about first because Kipper never manages it, and a
	// host running both managers has to come out blocked. Asking about ufw
	// first would let a claim on the ufw ruleset speak for a firewalld Kipper
	// cannot have set up, and configuring the host behind its back.
	//
	// Both probes swallow a missing or inactive service in the shell and exit
	// zero, so a non-zero status is the session itself failing. That has to be
	// an error rather than an answer: read as "no firewall" it would license
	// Kipper to configure a host it never managed to look at.
	out, err := client.Run("systemctl is-active firewalld 2>/dev/null || true")
	if err != nil {
		return audit, fmt.Errorf("checking for firewalld: %w", err)
	}
	if strings.TrimSpace(out) == "active" {
		audit.Existing = "firewalld"
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

// firewallClaimed answers whether Kipper left its claim on this host. Any
// failure to read it answers no, which keeps an unreadable host in the same
// hands-off state as an admin's own firewall.
//
// `test -s` rather than `test -f`: an empty file is not a claim. The write
// publishes by rename so an empty one should never appear, and requiring
// content costs nothing to be sure of it.
func firewallClaimed(client CommandRunner) bool {
	out, err := client.Run("test -s " + firewallClaimPath + " && echo claimed || true")
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) == "claimed"
}

// claimCommand records Kipper as the owner of this host's ufw ruleset. The
// file is addressed to whoever finds it, since it decides whether a later kip
// run reconfigures this firewall or keeps its hands off.
//
// The note is written to a staged file and moved into place, because `>`
// creates the destination before anything is written to it. Writing straight
// to the claim path would publish whatever prefix a failed write had already
// emitted, and a partial file is as readable a claim as a whole one.
//
// mktemp gives each run its own staged name. A fixed one is shared state
// between concurrent runs: the second to open it truncates the first's file,
// and the first then renames the second's half-written bytes over a claim that
// was valid. The template sits in the destination directory so the rename stays
// within one filesystem, and the trap clears the staged file on any exit that
// did not publish it.
func claimCommand(claimPath string) string {
	const note = "The ufw ruleset on this host is managed by Kipper (kip install, kip cluster harden).\n" +
		"Delete this file to take the firewall over, and kip will leave its rules alone.\n"

	// shellQuote (operator_auth.go) so the command does not depend on its
	// operands happening to be free of shell syntax. Kipper only ever claims one
	// fixed path, but a command assembled by string formatting should not rest
	// on that.
	dir := path.Dir(claimPath)
	return fmt.Sprintf(
		`mkdir -p %s && tmp=$(mktemp %s) && trap 'rm -f "$tmp"' EXIT && printf %s > "$tmp" && mv "$tmp" %s`,
		shellQuote(dir), shellQuote(path.Join(dir, path.Base(claimPath)+".XXXXXX")),
		shellQuote(note), shellQuote(claimPath))
}

// configureFirewallWith enables the requested SSH rate limit only when the
// runner can continue under it, either locally or over a shared SSH connection.
// ApplyFirewallPlan remains the public entry point to enforce ownership checks.
func configureFirewallWith(client FirewallRunner, rateLimitSSH bool) error {
	return configureFirewall(client, rateLimitSSH && client.SurvivesSSHRateLimit())
}

func configureFirewall(client CommandRunner, rateLimitSSH bool) error {
	// Installing the package changes no firewall: ufw ships inactive and
	// stays that way until it is enabled below.
	if _, err := client.Run("DEBIAN_FRONTEND=noninteractive apt-get install -y -qq ufw >/dev/null"); err != nil {
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
