package installer

import (
	"errors"
	"fmt"
	"os"
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
	Mesh             FirewallMesh
	Configure        bool
	Notice           string
	FlagNotice       string
	Adopt            bool
	AdoptFingerprint string
}

// PlanFirewall shares firewall configuration decisions and notices between
// kip install and kip cluster harden.
func PlanFirewall(audit FirewallAudit, wantFirewall, adopt bool) FirewallPlan {
	adopting := wantFirewall && adopt && audit.Adoptable
	mayConfigure := !audit.BlocksConfiguration() || adopting
	plan := FirewallPlan{Configure: wantFirewall && mayConfigure, Adopt: adopting}

	switch {
	case audit.Existing == "" && audit.KipperManaged:
		plan.Notice = "  ⚠  The firewall kip set up is switched off. Enable firewall configuration to turn it back on\n"
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
	// SurvivesSSHRateLimit reports whether commands can continue under UFW
	// SSH rate limiting, using a shared connection or local execution.
	SurvivesSSHRateLimit() bool
	// ConfirmFreshConnection verifies access over a new SSH connection.
	// It returns localrun.ErrNoConnection when commands run locally.
	ConfirmFreshConnection() error
}

// ApplyFirewallPlan configures firewalld on RHEL-family hosts or UFW elsewhere
// when the plan permits it. Each backend rechecks ownership before changing rules.
func ApplyFirewallPlan(client FirewallRunner, plan FirewallPlan, rateLimitSSH bool) error {
	if !plan.Configure {
		return nil
	}
	return configureFirewallWith(client, rateLimitSSH, FirewallConsent{Adopt: plan.Adopt, Fingerprint: plan.AdoptFingerprint}, plan.Mesh)
}

// AuditFirewall detects active host firewall managers and Kipper ownership.
// An inactive UFW installation retains ownership if its claim file is present.
// Runner errors and UFW status timeouts are returned.
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
		out, err := client.Run(ufwStateCommand)
		if err != nil {
			return audit, fmt.Errorf("checking for ufw: %w", err)
		}
		if strings.TrimSpace(out) == "active" {
			audit.Existing, audit.KipperManaged, audit.Adoptable = "firewalld and ufw", false, false
		}
		return audit, nil
	}

	// Preserve status timeouts so an unresponsive firewall cannot pass as absent.
	out, err = client.Run(ufwStateCommand)
	if err != nil {
		return audit, fmt.Errorf("checking for ufw: %w", err)
	}
	if strings.TrimSpace(out) == "active" {
		audit.Existing = "ufw"
		audit.KipperManaged = firewallClaimed(client)
		return audit, nil
	}
	// Retain ownership so later configuration can reactivate kip-managed UFW.
	audit.KipperManaged = firewallClaimed(client)
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
// requires a runner that can continue under the limit. firewalld uses an allow rule.
func configureFirewallWith(client FirewallRunner, rateLimitSSH bool, consent FirewallConsent, mesh FirewallMesh) error {
	if err := mesh.validate(); err != nil {
		return fmt.Errorf("configuring the firewall: %w", err)
	}
	host, err := DetectHostOS(client)
	if err != nil {
		return fmt.Errorf("configuring the firewall: %w", err)
	}
	if host.Family == FamilyRHEL {
		if rateLimitSSH {
			fmt.Printf("  ℹ  Kipper's firewalld configuration allows SSH without rate limiting\n")
		}
		return configureFirewalld(client, host, consent, mesh)
	}
	return configureFirewall(client, host, rateLimitSSH && client.SurvivesSSHRateLimit(), mesh)
}

func configureFirewall(client FirewallRunner, host HostOS, rateLimitSSH bool, mesh FirewallMesh) error {
	// Refresh package lists so UFW can be installed on a fresh host.
	install := host.InstallPackagesCommand(true, "ufw")
	if install == "" {
		return fmt.Errorf("installing ufw: kip cannot install packages on %s", host.ID)
	}
	if err := ensureInstalled(client, "ufw", "ufw", install, os.Stdout); err != nil {
		return err
	}

	sshPorts, err := readSSHPorts(client)
	if err != nil {
		return err
	}

	// Recheck ownership before changing rules: host hardening and the k3s
	// install may have left the caller's audit several minutes out of date.
	audit, err := AuditFirewall(client)
	if err != nil {
		return fmt.Errorf("re-auditing firewall: %w", err)
	}
	if audit.BlocksConfiguration() {
		return fmt.Errorf("a %s firewall became active on this host, so kip left it alone", audit.Existing)
	}

	rollback, err := armUFWRollback(client)
	if err != nil {
		return err
	}
	return rollback.finish(client, loadUFW(rollback.runner(client), sshPorts, rateLimitSSH, mesh))
}

// loadUFW saves and loads kip's rules. The caller must arm rollback and
// provide a runner that guards commands with the rollback lock.
func loadUFW(client CommandRunner, sshPorts []string, rateLimitSSH bool, mesh FirewallMesh) error {
	// Allow forwarding for kube-proxy NAT and Flannel pod traffic.
	forwardCmd := `sed -i 's|^DEFAULT_FORWARD_POLICY=.*|DEFAULT_FORWARD_POLICY="ACCEPT"|' /etc/default/ufw`

	// Record ownership after the forward-policy command succeeds so retries
	// recognize partially configured firewalls. The command and claim are
	// sequenced, not atomic: claim failure can leave an unclaimed policy edit.
	if _, err := client.Run(forwardCmd + " && " + claimCommand(firewallClaimPath)); err != nil {
		return fmt.Errorf("setting the ufw forward policy and claiming the firewall: %w", err)
	}

	// A saved rule can survive a failed reload. Reload and check the live SSH
	// rules below even if retrying a rule command succeeds.
	var ruleErr error
	for _, rule := range append(ufwRules(sshPorts, rateLimitSSH, mesh.Role), ufwPeerRules(mesh)...) {
		if err := runUFW(client, rule); err != nil {
			ruleErr = fmt.Errorf("applying %q: %w", rule, err)
			break
		}
	}
	if ruleErr == nil {
		ruleErr = removeStaleRules(client, mesh, sshPorts)
	}

	if err := loadSavedRules(client, sshPorts); err != nil {
		if rolledBackDuring(err) {
			return err
		}
		// Disable the firewall to preserve host access when loading or verifying
		// any SSH rule fails.
		if stopErr := stopUFW(client); stopErr != nil {
			return errors.Join(ruleErr, fmt.Errorf("ufw failed to load its rules (%w), and disabling ufw also failed (%v)", err, stopErr))
		}
		return errors.Join(ruleErr, fmt.Errorf("ufw failed to load its rules: %w", err))
	}
	if ruleErr != nil {
		return ruleErr
	}
	return requirePeerRulesLoaded(client, mesh)
}

// removeStaleRules deletes obsolete peer rules and, on workers, saved server
// port rules, except those for configured SSH ports. It stops at the first
// command error.
func removeStaleRules(client CommandRunner, mesh FirewallMesh, sshPorts []string) error {
	added, err := runUFWOutput(client, "ufw show added")
	if err != nil {
		return fmt.Errorf("listing ufw's rules: %w", err)
	}
	for _, rule := range append(staleUFWPeerRules(added, mesh), staleUFWRoleRules(added, mesh, sshPorts)...) {
		if err := runUFW(client, rule); err != nil {
			return fmt.Errorf("applying %q: %w", rule, err)
		}
	}
	return nil
}

// requirePeerRulesLoaded verifies live peer access after loading saved rules.
func requirePeerRulesLoaded(client CommandRunner, mesh FirewallMesh) error {
	if len(mesh.Peers) == 0 {
		return nil
	}
	chain, err := client.Run("iptables -w 5 -S ufw-user-input")
	if err != nil {
		return fmt.Errorf("reading ufw's live rules: %w", err)
	}
	if missing := ufwPeerRulesMissing(chain, mesh); len(missing) > 0 {
		return fmt.Errorf("ufw did not load the rules for the cluster's other nodes (%s); run the command again", strings.Join(missing, ", "))
	}
	return nil
}

// Retry transient UFW failures while k3s updates netfilter during startup.
const ufwAttempts = 5

var ufwRetryDelay = 3 * time.Second

// runUFW retries failures with increasing delays, stopping on rollback or
// loss of ownership. Commands must be safe to repeat.
func runUFW(client CommandRunner, command string) error {
	_, err := runUFWOutput(client, command+" >/dev/null")
	return err
}

// runUFWOutput retries failures with increasing delays, stopping on rollback or
// loss of ownership. It returns the final attempt's output and error. Commands
// must be safe to repeat.
func runUFWOutput(client CommandRunner, command string) (string, error) {
	var out string
	var err error
	for attempt := 1; attempt <= ufwAttempts; attempt++ {
		if out, err = client.Run(ufwTimeout + command); err == nil || rolledBackDuring(err) {
			return out, err
		}
		if attempt < ufwAttempts {
			time.Sleep(ufwRetryDelay * time.Duration(attempt))
		}
	}
	return out, err
}

// loadSavedRules stops UFW before each start to recover partially loaded rules.
// After each successful start, it checks for every SSH port in the live input chain.
func loadSavedRules(client CommandRunner, sshPorts []string) error {
	var err error
	for attempt := 1; attempt <= ufwAttempts; attempt++ {
		if err = stopUFW(client); rolledBackDuring(err) {
			return err
		}
		if err == nil {
			_, err = client.Run(enableUFWCommand)
			if err == nil {
				if missing := unloadedSSHPorts(client, sshPorts); len(missing) == 1 {
					err = fmt.Errorf("the rule for SSH port %s is not loaded", missing[0])
				} else if len(missing) > 1 {
					err = fmt.Errorf("the rules for SSH ports %s are not loaded", strings.Join(missing, ", "))
				}
			}
			if err == nil || rolledBackDuring(err) {
				return err
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
	// Listing the whole filter table avoids the misleading "incompatible"
	// error for missing chains in iptables-nft 1.8.9 (Debian 12).
	// Only a successful listing can confirm that the chain is absent.
	out, err := client.Run(`out=$(iptables -w 5 -S 2>&1); rc=$?; ` +
		`if [ $rc -ne 0 ]; then echo "unknown: $out"; exit 1; ` +
		`elif printf '%s\n' "$out" | grep -qx -- '-N ufw-user-input'; then echo present; else echo absent; fi`)
	switch strings.TrimSpace(out) {
	case "absent":
		return nil
	case "present":
		return fmt.Errorf("ufw's chains are still loaded after switching it off")
	}
	return fmt.Errorf("could not tell whether ufw's chains are gone: %v", err)
}

// unloadedSSHPorts returns ports without a matching --dport in UFW's live input
// chain, in input order. A read failure returns all supplied ports. The check
// matches destination ports only, so it cannot confirm that a rule allows SSH.
func unloadedSSHPorts(client CommandRunner, sshPorts []string) []string {
	out, err := client.Run("iptables -w 5 -S ufw-user-input 2>/dev/null")
	if err != nil {
		return sshPorts
	}
	var missing []string
	for _, port := range sshPorts {
		if !strings.Contains(out+"\n", "--dport "+port+" ") && !strings.Contains(out+"\n", "--dport "+port+"\n") {
			missing = append(missing, port)
		}
	}
	return missing
}

// k3s host ports pods legitimately reach on the node's INPUT chain.
const (
	// kubeletPort is scraped by metrics-server and Prometheus (both pods).
	kubeletPort = "10250"
	// nodeExporterPort receives Prometheus scrapes on the node IP.
	nodeExporterPort = "9100"
)

// ufwRules returns default policies, rules for the supplied SSH ports, public
// server ports and pod access to monitoring ports. Apply these rules before
// enabling UFW to preserve SSH access.
//
// Pod-specific rules allow kubelet and node-exporter scrapes without granting
// access to every host port. Pods can also use ports opened to all sources.
func ufwRules(sshPorts []string, rateLimitSSH bool, role HostRole) []string {
	rules := []string{
		"ufw default deny incoming",
		"ufw default allow outgoing",
	}
	// CI and operators behind a shared NAT can exceed the per-source limit.
	// --no-ssh-rate-limit selects an unrestricted SSH rule.
	sshAction := "limit"
	if !rateLimitSSH {
		sshAction = "allow"
	}
	for _, port := range sshPorts {
		rules = append(rules, fmt.Sprintf("ufw %s %s/tcp", sshAction, port))
	}
	// Ingress and API access are public only on the server.
	if role != RoleWorker {
		rules = append(rules, "ufw allow 80/tcp", "ufw allow 443/tcp", "ufw allow 6443/tcp")
	}
	return append(rules,
		fmt.Sprintf("ufw allow from %s to any port %s proto tcp", k3sPodCIDR, kubeletPort),
		fmt.Sprintf("ufw allow from %s to any port %s proto tcp", k3sPodCIDR, nodeExporterPort),
	)
}

// readSSHPort returns the first port reported by sshd -T, or "22" if its
// output is empty. Runner errors are returned.
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

// SSHPort returns the first configured port reported by sshd -T, or "22" if
// its output is empty. Runner errors are returned.
func SSHPort(client CommandRunner) (string, error) {
	return readSSHPort(client)
}

// SSHRateLimited reports whether UFW lists a LIMIT rule for the port returned
// by SSHPort. It reads saved rules when UFW is inactive and returns command
// errors. A missing LIMIT rule returns false.
func SSHRateLimited(client CommandRunner) (bool, error) {
	port, err := readSSHPort(client)
	if err != nil {
		return false, err
	}
	out, err := client.Run(ufwStatusCommand)
	if err != nil {
		return false, fmt.Errorf("reading the UFW rules: %w", err)
	}
	if strings.Contains(out, "Status: inactive") {
		// Inactive UFW omits rules from its status output.
		added, err := client.Run(ufwTimeout + "ufw show added")
		if err != nil {
			return false, fmt.Errorf("reading the saved UFW rules: %w", err)
		}
		for _, line := range strings.Split(added, "\n") {
			if f := strings.Fields(line); len(f) == 3 && f[0] == "ufw" && f[1] == "limit" && (f[2] == port+"/tcp" || f[2] == port) {
				return true, nil
			}
		}
		return false, nil
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && (f[0] == port+"/tcp" || f[0] == port) && f[1] == "LIMIT" {
			return true, nil
		}
	}
	return false, nil
}
