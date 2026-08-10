package installer

import (
	"fmt"
	"path"
	"strings"

	"github.com/getkipper/kipper/kip/internal/ssh"
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
func ApplyFirewallPlan(client *ssh.Client, plan FirewallPlan, rateLimitSSH bool) error {
	if !plan.Configure {
		return nil
	}
	return configureFirewallWith(client, rateLimitSSH)
}

// AuditFirewall detects whether a host firewall is already active, and whether
// it is one Kipper established.
func AuditFirewall(client *ssh.Client) (FirewallAudit, error) {
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
func firewallClaimed(client *ssh.Client) bool {
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

// configureFirewallWith applies the ruleset, rate-limiting the SSH port when
// the caller wants it and this client is actually multiplexing.
//
// Both have to hold. The limit fits one connection per install and not the
// hundreds a non-multiplexing client makes, so applying it to one of those
// would rate-limit the installer off a host it had half built.
//
// It is unexported so that ApplyFirewallPlan is the only way in from outside
// this package. A second exported entry point is a second place for the
// ownership decision to be skipped, and the compiler is a better guard against
// that than a test is.
func configureFirewallWith(client *ssh.Client, rateLimitSSH bool) error {
	return configureFirewall(client, rateLimitSSH && client.Multiplexed())
}

func configureFirewall(client *ssh.Client, rateLimitSSH bool) error {
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

	// The claim is attempted in this same command, sequenced behind the policy
	// edit by `&&`. That placement is what the ownership question needs. A claim
	// written earlier outlives a run that got no further and then vouches for
	// whatever firewall an admin raises next; a claim written at the end misses
	// a run that died holding a half-built firewall, which is the case this
	// whole thing exists to catch. Once this command completes, every later
	// failure leaves both behind, so a retry recognizes the wreckage as Kipper's.
	//
	// What the claim attests is that this command completed, not that the edit
	// changed a line: sed exits zero having matched nothing. That is the useful
	// fact anyway, since it says Kipper got as far as configuring ufw here.
	//
	// `&&` sequences, it does not make the pair atomic and nothing rolls back.
	// The edit can land while the claim fails, leaving the host unclaimed. That
	// errs the safe way: kip reads it as an admin's and keeps its hands off, and
	// a retry that finds ufw still inactive configures it from the start. The
	// error names both halves because either can be the one that failed.
	if _, err := client.Run(forwardCmd + " && " + claimCommand(firewallClaimPath)); err != nil {
		return fmt.Errorf("setting the ufw forward policy and claiming the firewall: %w", err)
	}

	for _, rule := range ufwRules(sshPort, rateLimitSSH) {
		if _, err := client.Run(rule + " >/dev/null"); err != nil {
			return fmt.Errorf("applying %q: %w", rule, err)
		}
	}

	// --force skips the "this may disrupt SSH" prompt. The SSH rule above
	// ran first, whether it opened the port or limited it, so the prompt's
	// concern does not apply.
	if _, err := client.Run("ufw --force enable >/dev/null"); err != nil {
		return fmt.Errorf("enabling ufw: %w", err)
	}

	return nil
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
	// limit drops a source that opens more than six connections in thirty
	// seconds. That is what an untargeted brute-force sweep looks like, and it
	// is also what an installer looks like when it opens a connection per
	// command — so it is applied only when this client is multiplexing and one
	// install is one connection. Rate-limiting the way in, and then needing
	// hundreds of connections through it, strands a half-built host.
	//
	// It is still a rule about a source address rather than an attacker. Six
	// connections in thirty seconds is generous for a person and reachable for
	// CI running several kip commands in a row, or for two operators behind one
	// NAT address. Hence --no-ssh-rate-limit.
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
func readSSHPort(client *ssh.Client) (string, error) {
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
