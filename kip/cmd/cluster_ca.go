package cmd

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/installer"
	"github.com/getkipper/kipper/kip/internal/ssh"
)

// caDocsURL is the replacement procedure. Replacing an authority is a manual
// operation with verification gates rather than a command, so anything that
// tells an operator to replace one has to tell them where the steps are.
const caDocsURL = "https://getkipper.com/en/certificate-authority"

func init() {
	clusterCmd.AddCommand(clusterCACmd)
	clusterCACmd.AddCommand(clusterCAStatusCmd)
	clusterCmd.AddCommand(clusterAuthCmd)
	clusterAuthCmd.AddCommand(clusterAuthSyncCmd)

	for _, c := range []*cobra.Command{clusterCAStatusCmd, clusterAuthSyncCmd} {
		c.Flags().String("ssh-key", "", "SSH key to reach the server with")
	}
}

var clusterCACmd = &cobra.Command{
	Use:   "ca",
	Short: "Inspect the cluster's certificate authority",
	Long: `The certificate authority signs what this cluster serves to the gateway, and
the API server trusts it to verify logins. It lasts 30 years, so replacing one
is something you do if it leaks, not on a schedule.

Start with 'kip cluster ca status'. It tells you whether anything needs doing.

Replacing an authority is a documented procedure rather than a command. It
spans two Secrets and two files on the host, and those cannot be changed as one
transaction, so each phase ends at a gate an operator verifies before carrying
on. The steps are at ` + caDocsURL + `.`,
}

var clusterCAStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the certificate authority and whether everything agrees",
	Long: `Reads the authority, the certificate the cluster serves, and what the API
server trusts, then checks they agree, including one live check against the
wire rather than trusting what is stored.

Safe to run at any time, including while a replacement is part-way through and
including when logins are failing. It reaches the server over SSH, so it does
not depend on the login path it reports on.`,
	RunE: runClusterCAStatus,
}

var clusterAuthCmd = &cobra.Command{
	Use:   "auth",
	Short: "Manage how the API server authenticates operators",
}

var clusterAuthSyncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Rebuild the API server's authentication config from the anchor on disk",
	Long: `Re-renders the API server's authentication config from the certificate
authority currently on the server, keeping exactly the issuers it already
trusts, and waits for the API server to load it.

This is the repair command. Run it when 'kip cluster ca status' reports that
the API server has not loaded the anchor on disk.

It cannot repair an anchor that names the wrong authority, because it adds
nothing to that file. It only re-renders what is already there.`,
	RunE: runClusterAuthSync,
}

// dialClusterHost opens the SSH connection every command in this file needs.
// All of them read or write host state the Kubernetes API cannot reach.
func dialClusterHost(cmd *cobra.Command) (*ssh.Client, *config.Cluster, error) {
	cluster, err := loadCurrentClusterConfig()
	if err != nil {
		return nil, nil, err
	}
	if cluster.Host == "" {
		return nil, nil, fmt.Errorf("no host recorded for cluster %s; this command needs SSH access to the server", cluster.Name)
	}
	flagKey, _ := cmd.Flags().GetString("ssh-key")
	explicit, fallback := resolveSSHKey(flagKey, cluster)
	client, err := ssh.Dial(ssh.Config{
		Host:            cluster.Host,
		User:            "root",
		KeyPath:         explicit,
		FallbackKeyPath: fallback,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("connecting to %s over SSH: %w", cluster.Host, err)
	}
	return client, cluster, nil
}

func runClusterCAStatus(cmd *cobra.Command, _ []string) error {
	client, cluster, err := dialClusterHost(cmd)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	status, err := installer.ReadCAStatus(client)
	if err != nil {
		return err
	}
	printCAStatus(cluster.Name, status)
	return nil
}

// printCAStatus lays the two sides of the trust relationship side by side and
// says whether they agree. An operator who reads nothing else should still be
// able to tell whether this cluster needs them.
func printCAStatus(clusterName string, s installer.CAStatus) {
	fmt.Printf("\n  Certificate authority: %s\n\n", clusterName)

	fmt.Printf("    Authority        %s (%s), expires %s\n",
		s.Authority.Subject, s.Authority.Fingerprint, expiryOf(s.Authority.Expires))
	if s.Incoming != nil {
		fmt.Printf("    Incoming         %s (%s), signing nothing yet\n", s.Incoming.Subject, s.Incoming.Fingerprint)
	}
	if s.Outgoing != nil {
		fmt.Printf("    Being replaced   %s (%s), still trusted\n", s.Outgoing.Subject, s.Outgoing.Fingerprint)
	}
	if s.Leaf.Subject != "" {
		fmt.Printf("    Served cert      %s, expires %s\n", s.Leaf.Subject, expiryOf(s.Leaf.Expires))
	}
	for _, h := range s.Hosts {
		fmt.Printf("    Login issuer     %s\n", h)
	}

	fmt.Println()
	fmt.Printf("    %s The anchor covers what signed this cluster's certificate\n", mark(s.AnchorCovers))
	if s.AnchorLoadedUnknown {
		fmt.Printf("    ?  The API server could not be asked what it has loaded\n")
	} else {
		fmt.Printf("    %s The API server has loaded that anchor\n", mark(s.AnchorLoaded))
	}
	// A check that cannot be made is said out loud. Printing nothing here reads
	// as two ticks out of two, and the wire check is the one the replacement
	// procedure leans on hardest before it narrows trust.
	if s.ServedByActive != nil {
		fmt.Printf("    %s The wire confirms the certificate in use\n", mark(*s.ServedByActive))
	} else {
		fmt.Printf("    –  The wire cannot be checked: no gateway-fronted issuer to ask\n")
	}

	if len(s.Problems) > 0 {
		fmt.Printf("\n  Needs attention:\n")
		for _, p := range s.Problems {
			fmt.Printf("    ✗  %s\n", p)
		}
	}

	fmt.Println()
	if len(s.Problems) > 0 {
		fmt.Printf("  Fix the above before replacing anything. Certificate authentication on the\n")
		fmt.Printf("  server is unaffected, so you can always reach the cluster over SSH.\n")
		// Returning here swallowed the phase, the resume point and the recovery
		// link in exactly the states that have all three — including the one the
		// procedure's own abandon instruction produces if only half of it runs.
		if s.Phase != installer.CAPhaseSteady {
			fmt.Printf("\n  A replacement is also part-way through (%s), resuming at %s.\n", s.Phase, s.Resume)
		}
		fmt.Printf("  If you need to undo a replacement, see \"If it goes wrong\" in:\n  %s\n\n", caDocsURL)
		return
	}

	// Where they are comes before what is wrong, and is printed whatever else
	// is true. Deriving it into a branch that a trust warning could pre-empt
	// left an operator mid-replacement with no idea which step came next.
	if s.Phase != installer.CAPhaseSteady {
		fmt.Printf("  A replacement is part-way through (%s). This is a safe state to sit in:\n", s.Phase)
		fmt.Printf("  the cluster trusts one authority more than it strictly needs to, which\n")
		fmt.Printf("  affects nothing. Finish it before changing this cluster's domain.\n")
		fmt.Printf("  Resume at %s:\n", s.Resume)
		fmt.Printf("  %s\n\n", caDocsURL)
	}

	switch {
	case s.Healthy():
		fmt.Printf("  Everything agrees. Nothing to do.\n")
	case s.AnchorLoadedUnknown:
		fmt.Printf("  The API server did not answer, so whether it has loaded this anchor is\n")
		fmt.Printf("  unknown. Logins may be fine. Check the API server is running, then ask\n")
		fmt.Printf("  again before changing anything.\n")
	case !s.AnchorCovers && s.AnchorLoaded:
		fmt.Printf("  The API server has loaded an anchor that does not cover what this cluster\n")
		fmt.Printf("  serves, so operator logins are failing. Certificate authentication over SSH\n")
		fmt.Printf("  still works, so you can reach the server and repair it.\n")
		fmt.Printf("  Put that authority back in the anchor, then run 'kip cluster auth sync'.\n")
		fmt.Printf("  See \"If it goes wrong\" in:  %s\n", caDocsURL)
	case !s.AnchorCovers:
		fmt.Printf("  The anchor on disk does not cover what this cluster serves. The API server\n")
		fmt.Printf("  has not loaded it, so logins are working for now, and syncing it as it\n")
		fmt.Printf("  stands would break them.\n")
		fmt.Printf("  Put that authority back in the anchor first. See \"If it goes wrong\" in:\n")
		fmt.Printf("  %s\n", caDocsURL)
	case !s.AnchorLoaded:
		fmt.Printf("  The API server has not loaded the anchor on disk. It is still running the\n")
		fmt.Printf("  config it had, so logins work for now: but this must be applied before\n")
		fmt.Printf("  anything else changes, or the cluster will serve a certificate the API\n")
		fmt.Printf("  server has never been told to trust.\n")
		fmt.Printf("  Apply it with:  %s\n", s.NextCommand())
	case s.ServedByActive != nil && !*s.ServedByActive:
		fmt.Printf("  This cluster is not serving a certificate the current authority signed.\n")
		fmt.Printf("  Traefik reloads its certificate store on its own schedule, so give it a\n")
		fmt.Printf("  minute and check again before investigating.\n")
	}
	fmt.Println()
}

func mark(ok bool) string {
	if ok {
		return "✔"
	}
	return "✗"
}

// expiryOf renders a date with the distance to it, because "2056" means little
// without "30 years from now" next to it.
func expiryOf(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	years := int(time.Until(t).Hours() / 24 / 365)
	switch {
	case years > 1:
		return fmt.Sprintf("%s (%d years)", t.Format("2 Jan 2006"), years)
	case years == 1:
		return fmt.Sprintf("%s (1 year)", t.Format("2 Jan 2006"))
	default:
		return fmt.Sprintf("%s (under a year)", t.Format("2 Jan 2006"))
	}
}

func runClusterAuthSync(cmd *cobra.Command, _ []string) error {
	client, cluster, err := dialClusterHost(cmd)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	fmt.Printf("\n  ...  Rebuilding the authentication config on %s\n", cluster.Name)
	if err := installer.SyncOperatorAuth(client); err != nil {
		return err
	}
	fmt.Printf("  ✔  The API server has loaded it\n\n")
	return nil
}
