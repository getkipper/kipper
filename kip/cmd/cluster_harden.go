package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/getkipper/kipper/kip/internal/installer"
)

var clusterHardenCmd = &cobra.Command{
	Use:          "harden",
	SilenceUsage: true,
	Short:        "Apply host hardening and firewall to an existing cluster",
	Long: `Audit every recorded node for surplus services and an existing firewall,
then apply Kipper's hardening defaults. Safe to repeat.

Applies the service and firewall hardening of 'kip install' and 'kip node
add', so existing clusters can use current defaults without reinstalling.
It also turns off SSH password login on every node unless
--allow-ssh-passwords is set. 'kip node add' leaves SSH password login unchanged.
The server goes first, then each worker one at a time. A worker gets the
worker firewall: SSH open to all sources and cluster ports open to other
nodes, with no public access to ports 80, 443 or 6443. kip stops at the first
node it cannot reach or harden. Run the command again to finish, starting
from the server.

Examples:
  kip cluster harden
  kip cluster harden --firewall=false
  kip cluster harden --ssh-key ~/.ssh/kipper_ed25519`,
	RunE: runClusterHarden,
}

func init() {
	clusterHardenCmd.Flags().String("ssh-key", "", "path to SSH private key (defaults to ~/.ssh/id_ed25519)")
	clusterHardenCmd.Flags().Bool("harden", true, "disable surplus host services exposed on public interfaces (e.g. rpcbind, Cockpit)")
	clusterHardenCmd.Flags().Bool("firewall", true, "configure the host firewall for k3s: firewalld on Rocky, AlmaLinux and CentOS Stream and UFW elsewhere. Taking over a running firewall without a Kipper ownership marker requires --adopt-firewall or interactive consent")
	clusterHardenCmd.Flags().Bool("adopt-firewall", false, "take over a running firewall without prompting when it has no Kipper ownership marker: UFW on Ubuntu and Debian, firewalld on Rocky, AlmaLinux or CentOS Stream. Replaces UFW rules or the firewalld public zone, removing services such as Cockpit, and allows SSH on every node and public TCP ports 80, 443 and 6443 on the server")
	clusterHardenCmd.Flags().Bool("allow-ssh-passwords", false, "leave SSH password login unchanged. By default, kip turns it off after a successful key-only login")
	clusterHardenCmd.Flags().Bool("no-ssh-rate-limit", false, "disable UFW SSH rate limiting on Ubuntu and Debian. Useful for CI or operators sharing a NAT address")
	clusterCmd.AddCommand(clusterHardenCmd)
}

func runClusterHarden(cmd *cobra.Command, _ []string) error {
	cluster, err := loadCurrentClusterConfig()
	if err != nil {
		return err
	}

	flagKey, _ := cmd.Flags().GetString("ssh-key")
	noSSHRateLimit, _ := cmd.Flags().GetBool("no-ssh-rate-limit")
	allowSSHPasswords, _ := cmd.Flags().GetBool("allow-ssh-passwords")
	harden, _ := cmd.Flags().GetBool("harden")
	isTTY := term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
	opts := newHardenOptions(harden, noSSHRateLimit, allowSSHPasswords, adoptionQuestion(isTTY))
	opts.firewall, _ = cmd.Flags().GetBool("firewall")
	opts.adoptFirewall, _ = cmd.Flags().GetBool("adopt-firewall")

	fmt.Printf("\n  Hardening %s (%s)...\n", cluster.Name, cluster.Host)

	// The lease keeps kip node add and remove from changing peer rules while
	// this run rewrites them from the records.
	leased, err := leaseCluster(cluster, flagKey, "kip cluster harden")
	if err != nil {
		return err
	}
	defer leased.close()

	records, err := recordNodes(leased.server, cluster)
	if err != nil {
		return err
	}
	if err := nodesSettled(records); err != nil {
		return fmt.Errorf("%w. Hardening during a join or removal could grant access to that node's address", err)
	}

	step := hardenEach(records, func(client installer.FirewallRunner, host string, mesh installer.FirewallMesh) error {
		return hardenNode(client, host, mesh, opts)
	})
	if err := forEachNode(records, leased.server, leased.dialer(cluster, flagKey), step); err != nil {
		return err
	}
	fmt.Println()
	return nil
}

// hardenEach builds the per-node harden step. Each node gets the firewall for
// its own role, with peer rules for other records that have a peer address.
func hardenEach(records []installer.NodeRecord, harden func(client installer.FirewallRunner, host string, mesh installer.FirewallMesh) error) func(installer.NodeRecord, installer.FirewallRunner) error {
	return func(rec installer.NodeRecord, client installer.FirewallRunner) error {
		fmt.Printf("\n  Node %s (%s, %s)\n\n", rec.Name, rec.Host, rec.Role)
		return harden(client, rec.Host, installer.FirewallMeshFor(records, rec.MachineID))
	}
}

type hardenOptions struct {
	harden, firewall, adoptFirewall bool
	rateLimitSSH, keyOnlySSH        bool
	askToAdopt                      func(question string) bool
}

func newHardenOptions(harden, noSSHRateLimit, allowSSHPasswords bool, askToAdopt func(question string) bool) hardenOptions {
	return hardenOptions{
		harden:       harden,
		rateLimitSSH: !noSSHRateLimit,
		keyOnlySSH:   !allowSSHPasswords,
		askToAdopt:   askToAdopt,
	}
}

// hardenNode audits one host, then disables surplus services, configures its
// firewall for mesh and turns off SSH password login as opts allow. Every
// step is safe to repeat.
func hardenNode(client installer.FirewallRunner, host string, mesh installer.FirewallMesh, opts hardenOptions) error {
	fmt.Printf("  Auditing host security...\n")
	audit, err := installer.AuditHost(client)
	if err != nil {
		return fmt.Errorf("auditing host: %w", err)
	}
	findings := audit.Findings()
	if len(findings) == 0 {
		fmt.Printf("  ✔  No surplus services detected\n")
	} else {
		for _, f := range findings {
			fmt.Printf("  ⚠  %s\n", f)
		}
	}

	fwAudit, err := installer.AuditFirewall(client)
	if err != nil {
		return fmt.Errorf("auditing firewall: %w", err)
	}
	consent, err := installer.AdoptionConsent(client, host, fwAudit, opts.firewall, opts.adoptFirewall, opts.askToAdopt, mesh)
	if err != nil {
		return fmt.Errorf("checking the running firewall: %w", err)
	}
	fwPlan := installer.PlanFirewall(fwAudit, opts.firewall, consent.Adopt)
	fwPlan.AdoptFingerprint = consent.Fingerprint
	fwPlan.Mesh = mesh
	fmt.Print(fwPlan.Notice)
	fmt.Println()

	switch {
	case opts.harden && len(findings) > 0:
		fmt.Printf("  ...  Hardening host OS\n")
		if err := installer.HardenHost(client); err != nil {
			return fmt.Errorf("hardening host: %w", err)
		}
		fmt.Printf("  ✔  Hardening host OS\n")
	case !opts.harden && len(findings) > 0:
		fmt.Printf("  ⚠  Host hardening skipped (--harden=false)\n")
	}

	switch {
	case fwPlan.Configure:
		fmt.Printf("  ...  Configuring firewall\n")
		if err := installer.ApplyFirewallPlan(client, fwPlan, opts.rateLimitSSH); err != nil {
			return fmt.Errorf("configuring firewall: %w", err)
		}
		fmt.Printf("  ✔  Configuring firewall\n")
	case fwPlan.FlagNotice != "":
		fmt.Print(fwPlan.FlagNotice)
	}

	if opts.keyOnlySSH {
		fmt.Printf("  ...  Turning off SSH password login\n")
		changed, err := installer.EnsureKeyOnlySSH(client)
		if err != nil {
			return fmt.Errorf("turning off SSH password login: %w", err)
		}
		if changed {
			fmt.Printf("  ✔  SSH now accepts keys only\n")
		} else {
			fmt.Printf("  ✔  SSH password login is off in sshd's configuration, and sshd refuses a password for kip's login\n")
		}
	}
	return nil
}
