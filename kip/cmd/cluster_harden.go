package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/getkipper/kipper/kip/internal/infra"
	"github.com/getkipper/kipper/kip/internal/installer"
)

var clusterHardenCmd = &cobra.Command{
	Use:   "harden",
	Short: "Apply host hardening and firewall to an existing cluster",
	Long: `Audit the cluster's host for surplus services and an existing firewall,
then apply Kipper's hardening defaults. Idempotent, safe to re-run.

Mirrors the hardening that 'kip install' applies on fresh installs, so
existing clusters can be brought up to current defaults without reinstall.

Examples:
  kip cluster harden
  kip cluster harden --firewall=false
  kip cluster harden --ssh-key ~/.ssh/kipper_ed25519`,
	RunE: runClusterHarden,
}

func init() {
	clusterHardenCmd.Flags().String("ssh-key", "", "path to SSH private key (defaults to ~/.ssh/id_ed25519)")
	clusterHardenCmd.Flags().Bool("harden", true, "disable surplus host services exposed on public interfaces (e.g. rpcbind)")
	clusterHardenCmd.Flags().Bool("firewall", true, "install and configure UFW with a k3s-correct ruleset (skipped if another firewall is detected)")
	clusterHardenCmd.Flags().Bool("no-ssh-rate-limit", false, "open the SSH port outright instead of limiting it to six connections per thirty seconds per source; set this for CI, or when several operators share one NAT address")
	clusterCmd.AddCommand(clusterHardenCmd)
}

func runClusterHarden(cmd *cobra.Command, _ []string) error {
	cluster, err := loadCurrentClusterConfig()
	if err != nil {
		return err
	}

	flagKey, _ := cmd.Flags().GetString("ssh-key")
	noSSHRateLimit, _ := cmd.Flags().GetBool("no-ssh-rate-limit")
	explicit, fallback := resolveSSHKey(flagKey, cluster)
	harden, _ := cmd.Flags().GetBool("harden")
	firewall, _ := cmd.Flags().GetBool("firewall")

	fmt.Printf("\n  Hardening %s (%s)...\n\n", cluster.Name, cluster.Host)

	provider := &infra.BareMetalProvider{
		Host:           cluster.Host,
		SSHKey:         explicit,
		FallbackSSHKey: fallback,
	}
	if err := provider.Connect(); err != nil {
		return fmt.Errorf("connecting to %s: %w", cluster.Host, err)
	}
	defer func() { _ = provider.Close() }()
	client := provider.Client()

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
	fwPlan := installer.PlanFirewall(fwAudit, firewall)
	fmt.Print(fwPlan.Notice)
	fmt.Println()

	switch {
	case harden && len(findings) > 0:
		fmt.Printf("  ...  Hardening host OS\n")
		if err := installer.HardenHost(client); err != nil {
			return fmt.Errorf("hardening host: %w", err)
		}
		fmt.Printf("  ✔  Hardening host OS\n")
	case !harden && len(findings) > 0:
		fmt.Printf("  ⚠  Host hardening skipped (--harden=false)\n")
	}

	switch {
	case fwPlan.Configure:
		fmt.Printf("  ...  Configuring firewall\n")
		if err := installer.ApplyFirewallPlan(client, fwPlan, !noSSHRateLimit); err != nil {
			return fmt.Errorf("configuring firewall: %w", err)
		}
		fmt.Printf("  ✔  Configuring firewall\n")
	case fwPlan.FlagNotice != "":
		fmt.Print(fwPlan.FlagNotice)
	}

	fmt.Println()
	return nil
}
