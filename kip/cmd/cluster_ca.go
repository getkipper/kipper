package cmd

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/installer"
	"github.com/getkipper/kipper/kip/internal/ssh"
)

// caDocsURL links operators to the manual replacement and recovery procedure.
const caDocsURL = "https://docs.getkipper.com/en/certificate-authority"

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
spans three Secrets and two files on the host, and those cannot be changed as one
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

// dialClusterHost uses SSH to access authentication files on the host.
func dialClusterHost(cmd *cobra.Command) (*ssh.Client, *config.Cluster, error) {
	cluster, err := loadCurrentClusterConfig()
	if err != nil {
		return nil, nil, err
	}
	if cluster.Host == "" {
		return nil, nil, fmt.Errorf("no host recorded for cluster %s. This command needs SSH access to the server", cluster.Name)
	}
	flagKey, _ := cmd.Flags().GetString("ssh-key")
	client, err := dialNodeSSH(cluster.Host, flagKey, cluster)
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

// caPhaseNote describes the stored authorities and anchor during a replacement.
func caPhaseNote(phase installer.CAPhase) string {
	switch phase {
	case installer.CAPhaseStaged:
		return "the new authority is stored but not trusted yet"
	case installer.CAPhaseNarrowed:
		return "the old authority is still stored but has been removed from the anchor file"
	default:
		return "the cluster trusts one authority more than it strictly needs to"
	}
}

func printCAStatus(clusterName string, s installer.CAStatus) {
	fmt.Printf("\n  Certificate authority: %s\n\n", clusterName)

	fmt.Printf("    Authority        %s (%s), expires %s\n",
		s.Authority.Subject, s.Authority.Fingerprint, expiryOf(s.Authority.Expires))
	if s.Incoming != nil {
		fmt.Printf("    Incoming         %s (%s), signing nothing yet\n", s.Incoming.Subject, s.Incoming.Fingerprint)
	}
	if s.Outgoing != nil {
		fmt.Printf("    Being replaced   %s (%s), still stored\n", s.Outgoing.Subject, s.Outgoing.Fingerprint)
	}
	if s.Leaf.Subject != "" {
		fmt.Printf("    Stored cert      %s, expires %s\n", s.Leaf.Subject, expiryOf(s.Leaf.Expires))
	}
	for _, h := range s.Hosts {
		fmt.Printf("    Login issuer     %s\n", h)
	}

	fmt.Println()
	fmt.Printf("    %s The anchor covers what signed this cluster's certificate\n", mark(s.AnchorCovers))
	switch {
	case len(s.Hosts) == 0:
		fmt.Printf("    –  No login issuer is configured, so the API server check was skipped\n")
	case s.AnchorLoadedUnknown:
		fmt.Printf("    ?  The API server could not be asked what it has loaded\n")
	case !s.AnchorUsed:
		fmt.Printf("    %s The API server has loaded its login config, which uses no anchor\n", mark(s.AnchorLoaded))
	default:
		fmt.Printf("    %s The API server has loaded that anchor\n", mark(s.AnchorLoaded))
	}
	// Report skipped live checks so operators can distinguish them from passes.
	if s.ServedByActive != nil {
		fmt.Printf("    %s The wire serves a certificate that verifies against the current authority\n", mark(*s.ServedByActive))
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
		// Keep the resume point and recovery link visible when a replacement needs repair.
		if s.Phase != installer.CAPhaseSteady {
			fmt.Printf("\n  A replacement is also part-way through (%s), resuming at %s.\n", s.Phase, s.Resume)
		}
		fmt.Printf("  If you need to undo a replacement, see \"If it goes wrong\" in:\n  %s\n\n", caDocsURL)
		return
	}

	// Show the resume point alongside any trust warning below.
	if s.Phase != installer.CAPhaseSteady {
		// Calling a phase safe requires confirmed trust and no failed live check.
		// A live check may be skipped when no gateway-fronted issuer is configured.
		trusted := s.TrustedByAPIServer && !s.AnchorLoadedUnknown
		wireFails := s.ServedByActive != nil && !*s.ServedByActive
		settled := trusted && !wireFails
		switch {
		case trusted && wireFails && s.Phase == installer.CAPhasePromoted:
			fmt.Printf("  A replacement is part-way through (%s). Before step 2.3 the gateway serves\n", s.Phase)
			fmt.Printf("  the outgoing authority's certificate. After the re-signed certificate is\n")
			fmt.Printf("  installed, it keeps serving the old certificate until Traefik restarts.\n")
			fmt.Printf("  The wire check cannot distinguish these cases from other verification failures.\n")
		case settled && !s.AnchorUsed:
			fmt.Printf("  A replacement is part-way through (%s). No login issuer uses the anchor,\n", s.Phase)
			fmt.Printf("  so this is a safe state to sit in. Finish it before changing this cluster's\n")
			fmt.Printf("  domain.\n")
		case settled:
			fmt.Printf("  A replacement is part-way through (%s). This is a safe state to sit in:\n", s.Phase)
			fmt.Printf("  %s, which\n", caPhaseNote(s.Phase))
			fmt.Printf("  affects nothing. Finish it before changing this cluster's domain.\n")
		default:
			fmt.Printf("  A replacement is part-way through (%s). Fix the issues below before\n", s.Phase)
			fmt.Printf("  finishing it.\n")
		}
		fmt.Printf("  Resume at %s:\n", s.Resume)
		fmt.Printf("  %s\n\n", caDocsURL)
	}

	switch {
	case s.Healthy():
		fmt.Printf("  Everything agrees. Nothing to do.\n")
	case !s.AnchorCovers && !s.AnchorUsed:
		fmt.Printf("  The anchor on disk does not cover the certificate stored for this cluster.\n")
		fmt.Printf("  The login config on disk has only custom-domain issuers, which are verified\n")
		fmt.Printf("  with public certificates and use no anchor.\n")
		switch {
		case s.AnchorLoaded:
			fmt.Printf("  The API server runs that config, so logins are unaffected.\n")
		case s.AnchorLoadedUnknown:
			fmt.Printf("  The API server did not answer, so which config it runs is unknown.\n")
		default:
			fmt.Printf("  The API server has not loaded that config, so which one it runs is unknown.\n")
		}
		fmt.Printf("  Put that authority back in the anchor before adding a kipper.run login.\n")
		fmt.Printf("  See \"If it goes wrong\" in:  %s\n", caDocsURL)
	case !s.AnchorCovers && s.AnchorLoaded:
		fmt.Printf("  The API server has loaded an anchor that does not cover the certificate\n")
		fmt.Printf("  stored for this cluster, so kipper.run logins fail whenever the gateway\n")
		fmt.Printf("  serves it. Certificate authentication over SSH still works, so you can reach\n")
		fmt.Printf("  the server and repair it.\n")
		fmt.Printf("  Put that authority back in the anchor, then run 'kip cluster auth sync'.\n")
		fmt.Printf("  See \"If it goes wrong\" in:  %s\n", caDocsURL)
	case !s.AnchorCovers && s.AnchorLoadedUnknown:
		fmt.Printf("  The anchor on disk does not cover the certificate stored for this cluster,\n")
		fmt.Printf("  and the API server did not answer, so which config it runs is unknown and\n")
		fmt.Printf("  whether logins work cannot be told from here. If it has loaded that anchor,\n")
		fmt.Printf("  kipper.run logins fail whenever the gateway serves the stored certificate.\n")
		fmt.Printf("  Certificate authentication over SSH still works.\n")
		fmt.Printf("  Check that the API server is running, put that authority back in the anchor,\n")
		fmt.Printf("  then run 'kip cluster auth sync'.\n")
		fmt.Printf("  See \"If it goes wrong\" in:  %s\n", caDocsURL)
	case s.AnchorLoadedUnknown:
		fmt.Printf("  The API server did not answer, so whether it has loaded the login config on\n")
		fmt.Printf("  disk is unknown. Logins may be fine. Check the API server is running, then\n")
		fmt.Printf("  ask again before changing anything.\n")
	case !s.AnchorCovers:
		fmt.Printf("  The anchor on disk does not cover the certificate stored for this cluster.\n")
		fmt.Printf("  The API server has not loaded the config on disk, so whether logins work\n")
		fmt.Printf("  cannot be told from here. Syncing the anchor as it stands would\n")
		fmt.Printf("  break kipper.run logins once the gateway serves the stored certificate.\n")
		fmt.Printf("  Put that authority back in the anchor first. See \"If it goes wrong\" in:\n")
		fmt.Printf("  %s\n", caDocsURL)
	case !s.AnchorLoaded && !s.AnchorUsed:
		fmt.Printf("  The API server has not loaded the login config on disk, so which config it\n")
		fmt.Printf("  runs cannot be told from here. The config on disk has only custom-domain\n")
		fmt.Printf("  logins, which are verified with public certificates and use no anchor.\n")
		fmt.Printf("  Apply it with:  %s\n", s.NextCommand())
	case !s.AnchorLoaded:
		fmt.Printf("  The API server has not loaded the anchor on disk, so which authority it\n")
		fmt.Printf("  trusts for kipper.run logins cannot be told from here. Apply it before\n")
		fmt.Printf("  anything else changes, or the cluster can end up serving a certificate the\n")
		fmt.Printf("  API server was never told to trust.\n")
		fmt.Printf("  Apply it with:  %s\n", s.NextCommand())
	case s.ServedByActive != nil && !*s.ServedByActive:
		fmt.Printf("  The certificate this cluster serves does not verify against the current\n")
		fmt.Printf("  authority: another authority signed it, it expired, or it names another host.\n")
		fmt.Printf("  Traefik serves its own copy of the certificate and picks up a new one when\n")
		fmt.Printf("  it restarts, so check that its rollout finished (kubectl -n traefik rollout\n")
		fmt.Printf("  status deployment/traefik), then check again before investigating.\n")
	}
	fmt.Println()
}

func mark(ok bool) string {
	if ok {
		return "✔"
	}
	return "✗"
}

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
