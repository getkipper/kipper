package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/getkipper/kipper/kip/internal/installer"
)

var installCmd = &cobra.Command{
	Use:   "install",
	Short: "Install a Kipper cluster on a remote server",
	Long: `Connects to a Linux server via SSH, runs preflight checks, and installs
a production-ready Kubernetes cluster with automatic SSL, storage,
authentication, and a web console.

Backup storage defaults to an in-cluster MinIO. To survive a cluster
wipe, point Velero at off-cluster S3-compatible object storage with
the --backup-storage-* flags below. The bucket must already exist; kip
does not create buckets.`,
	RunE: runInstall,
}

func init() {
	installCmd.Flags().String("host", "", "IP address or hostname of the target server")
	installCmd.Flags().String("domain", "", "custom domain for the cluster")
	installCmd.Flags().String("console-domain", "", "hostname to serve the web console on (default: console.<domain>)")
	installCmd.Flags().String("console-api-domain", "", "hostname to serve the console API on (default: console-api.<domain>)")
	installCmd.Flags().String("dex-domain", "", "hostname to serve Dex on (default: dex.<domain>)")
	installCmd.Flags().String("ssh-key", "", "path to SSH private key")
	installCmd.Flags().String("admin-email", "admin@kipper.local", "email for Let's Encrypt and admin account")
	installCmd.Flags().String("org", "", "organisation short code (e.g. acme)")
	installCmd.Flags().String("org-display-name", "", "organisation display name (e.g. Acme Inc)")
	installCmd.Flags().Bool("harden", true, "disable surplus host services exposed on public interfaces (e.g. rpcbind)")
	installCmd.Flags().Bool("admin-kubeconfig", false, "write the shared k3s admin certificate to this machine instead of a per-operator OIDC kubeconfig; CI escape hatch. The certificate is unattributed and revocable only by rotation")
	installCmd.Flags().Bool("no-login", false, "skip the inline sign-in; run kip auth login && kip auth verify later")
	installCmd.Flags().Bool("firewall", true, "install and configure UFW with a k3s-correct ruleset (skipped if another firewall is detected)")
	installCmd.Flags().Bool("no-ssh-rate-limit", false, "open the SSH port outright instead of limiting it to six connections per thirty seconds per source; set this for CI, or when several operators share one NAT address")
	installCmd.Flags().StringSlice("dns-resolver", nil, "upstream DNS resolver CoreDNS forwards external queries to (repeatable; default 1.1.1.1, 8.8.8.8, 9.9.9.9). Set for clusters that must reach internal/corporate DNS.")
	installCmd.Flags().StringSlice("trusted-proxy", nil, "IP or CIDR whose X-Forwarded-* headers Traefik honours (repeatable). Set for clusters behind an external load balancer; the kipper.run gateway is trusted automatically.")

	installCmd.Flags().String("backup-storage-bucket", "", "S3-compatible bucket name for Velero backups (must already exist)")
	installCmd.Flags().String("backup-storage-region", "", "AWS region (or provider equivalent, e.g. 'auto' for Cloudflare R2)")
	installCmd.Flags().String("backup-storage-endpoint", "", "S3 endpoint URL (omit for native AWS S3, required for R2/MinIO/B2/Wasabi/Spaces)")
	installCmd.Flags().String("backup-storage-credentials", "", "path to AWS-style credentials file (defaults to ~/.aws/credentials when --backup-storage-bucket is set)")
	installCmd.Flags().String("backup-storage-profile", "default", "profile name within the credentials file (e.g. 'acme')")

	_ = installCmd.MarkFlagRequired("host")

	rootCmd.AddCommand(installCmd)
}

func runInstall(cmd *cobra.Command, args []string) error {
	host, _ := cmd.Flags().GetString("host")
	domain, _ := cmd.Flags().GetString("domain")
	consoleDomain, _ := cmd.Flags().GetString("console-domain")
	consoleAPIDomain, _ := cmd.Flags().GetString("console-api-domain")
	dexDomain, _ := cmd.Flags().GetString("dex-domain")
	sshKey, _ := cmd.Flags().GetString("ssh-key")
	adminEmail, _ := cmd.Flags().GetString("admin-email")
	org, _ := cmd.Flags().GetString("org")
	orgDisplayName, _ := cmd.Flags().GetString("org-display-name")
	harden, _ := cmd.Flags().GetBool("harden")
	firewall, _ := cmd.Flags().GetBool("firewall")
	noSSHRateLimit, _ := cmd.Flags().GetBool("no-ssh-rate-limit")
	adminKubeconfig, _ := cmd.Flags().GetBool("admin-kubeconfig")
	noLogin, _ := cmd.Flags().GetBool("no-login")
	dnsResolvers, _ := cmd.Flags().GetStringSlice("dns-resolver")
	trustedProxies, _ := cmd.Flags().GetStringSlice("trusted-proxy")

	// When a custom domain is provided and the admin email was not
	// explicitly set, derive a valid email from the domain. The
	// default admin@kipper.local is rejected by Let's Encrypt.
	if domain != "" && !cmd.Flags().Changed("admin-email") {
		adminEmail = "admin@" + domain
	}

	backupStorage, err := resolveBackupStorage(cmd)
	if err != nil {
		return err
	}

	// Pre-install there is no cluster entry to read SSH config from, so
	// resolveSSHKey only consults the flag and KIP_SSH_KEY env var; the
	// fallback to ~/.ssh/id_ed25519 is a soft hint that ssh tries only
	// if the file actually exists. Persist the explicit key onto the
	// new cluster entry so subsequent kip commands inherit it.
	explicitKey, fallbackKey := resolveSSHKey(sshKey, nil)

	// Terminal on both stdin and stdout decides interactive vs deferred
	// (precedent: cmd/secret.go). The domain the cluster ends up on is only
	// known inside Run for the *.kipper.run case, so the gate closure is
	// built there from the resolved domain, dex host, server, and CA.
	isTTY := term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
	kubeconfigMode := installer.ResolveKubeconfigMode(adminKubeconfig, noLogin, isTTY)

	result, installErr := installer.Run(installer.Options{
		KipVersion:         installingKipVersion(),
		Host:               host,
		Domain:             domain,
		ConsoleDomain:      consoleDomain,
		ConsoleAPIDomain:   consoleAPIDomain,
		DexDomain:          dexDomain,
		SSHKeyPath:         explicitKey,
		FallbackSSHKeyPath: fallbackKey,
		AdminEmail:         adminEmail,
		Org:                org,
		OrgDisplayName:     orgDisplayName,
		Harden:             harden,
		Firewall:           firewall,
		NoSSHRateLimit:     noSSHRateLimit,
		BackupStorage:      backupStorage,
		DNSResolvers:       dnsResolvers,
		TrustedProxies:     trustedProxies,
		KubeconfigMode:     kubeconfigMode,
		LoginGate: func(ctx context.Context, domain, dexHost, server string, caData []byte) installer.GateResult {
			return installer.DefaultLoginGate(domain, dexHost)(ctx, domain, server, caData)
		},
	})

	// Always print credentials if available, even if a later install step failed
	printInstallSummary(os.Stdout, result, installErr)

	return installErr
}

// printInstallSummary closes an install with where the cluster lives and, when
// they have not already been disclosed, the credentials to reach it.
//
// The credentials are skipped when the install showed them itself, which the
// interactive path does before its sign-in prompt. Printing them again here
// would put the same password under a second "save these now" heading in one
// run, and leave an operator wondering which of the two to keep.
func printInstallSummary(out io.Writer, result *installer.Result, installErr error) {
	if result == nil {
		return
	}
	if installErr != nil {
		_, _ = fmt.Fprintf(out, "\n  Install completed with errors (see above). Credentials saved:\n\n")
	} else {
		_, _ = fmt.Fprintf(out, "  Cluster ready.\n")
	}
	_, _ = fmt.Fprintf(out, "  Console:    %s\n", result.ConsoleURL)
	_, _ = fmt.Fprintf(out, "  Kubeconfig: %s\n", result.KubeconfigPath)
	if result.AdminPassword != "" && !result.CredentialsShown {
		_, _ = fmt.Fprintf(out, "  Admin:      admin@%s\n", result.Domain)
		_, _ = fmt.Fprintf(out, "  Password:   %s\n", result.AdminPassword)
		_, _ = fmt.Fprintln(out)
		_, _ = fmt.Fprintf(out, "  Save these credentials now. They will not be shown again.\n")
		_, _ = fmt.Fprintf(out, "  If lost, run: kip auth reset-password\n")
	}
	_, _ = fmt.Fprintln(out)
}

// resolveBackupStorage builds an installer.BackupStorageConfig from the
// --backup-storage-* flags. The flags are all-or-nothing: setting any of
// bucket/region without the others is rejected. Credentials default to
// ~/.aws/credentials when bucket is set and --backup-storage-credentials
// is not specified. Returns nil when no backup-storage flag is set, in
// which case the installer falls back to in-cluster MinIO.
func resolveBackupStorage(cmd *cobra.Command) (*installer.BackupStorageConfig, error) {
	bucket, _ := cmd.Flags().GetString("backup-storage-bucket")
	region, _ := cmd.Flags().GetString("backup-storage-region")
	endpoint, _ := cmd.Flags().GetString("backup-storage-endpoint")
	credsPath, _ := cmd.Flags().GetString("backup-storage-credentials")
	profile, _ := cmd.Flags().GetString("backup-storage-profile")

	anySet := bucket != "" || region != "" || endpoint != "" || credsPath != "" ||
		cmd.Flags().Changed("backup-storage-profile")
	if !anySet {
		return nil, nil
	}

	if bucket == "" {
		return nil, fmt.Errorf("--backup-storage-bucket is required when configuring external backup storage")
	}
	if region == "" {
		return nil, fmt.Errorf("--backup-storage-region is required when configuring external backup storage")
	}

	if credsPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolving ~/.aws/credentials path: %w", err)
		}
		credsPath = filepath.Join(home, ".aws", "credentials")
	} else if strings.HasPrefix(credsPath, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("expanding ~ in credentials path: %w", err)
		}
		credsPath = filepath.Join(home, credsPath[2:])
	}

	creds, err := installer.LoadAWSCredentials(credsPath, profile)
	if err != nil {
		return nil, fmt.Errorf("loading credentials for external backup storage: %w", err)
	}

	return &installer.BackupStorageConfig{
		Bucket:          bucket,
		Region:          region,
		Endpoint:        endpoint,
		AccessKeyID:     creds.AccessKeyID,
		SecretAccessKey: creds.SecretAccessKey,
	}, nil
}

// installingKipVersion is the version recorded on the CRDs an install writes, so
// a later, older kip refuses to replace their schemas. It is a function rather
// than an inline field read so a test can prove the install path supplies it:
// asserting that InstallCRDs stamps what it is given says nothing about whether
// anything gives it a version at all.
func installingKipVersion() string {
	return rootCmd.Version
}
