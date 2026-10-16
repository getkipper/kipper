package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/getkipper/kipper/kip/internal/installer"
)

var installCmd = &cobra.Command{
	// Keep install errors visible without appending the full flag list.
	SilenceUsage: true,
	Use:          "install",
	Short:        "Install a Kipper cluster on a remote server",
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
	installCmd.Flags().String("domain", "", "name the cluster serves on: a free name under kipper.run (lab.kipper.run), or a domain you control (example.com). Default: derived from the server's address")
	installCmd.Flags().String("console-domain", "", "hostname to serve the web console on (default: console.<domain>)")
	installCmd.Flags().String("console-api-domain", "", "hostname to serve the console API on (default: console-api.<domain>)")
	installCmd.Flags().String("dex-domain", "", "hostname to serve Dex on (default: dex.<domain>)")
	installCmd.Flags().String("ssh-key", "", "path to SSH private key")
	installCmd.Flags().String("admin-email", "admin@kipper.local", "email for Let's Encrypt and admin account")
	installCmd.Flags().String("org", "", "organization short code (e.g. acme)")
	installCmd.Flags().String("org-display-name", "", "organization display name (e.g. Acme Inc)")
	installCmd.Flags().Bool("harden", true, "disable surplus host services exposed on public interfaces (e.g. rpcbind, Cockpit)")
	installCmd.Flags().Bool("admin-kubeconfig", false, "write the shared k3s admin certificate to this machine instead of a per-operator OIDC kubeconfig; CI escape hatch. The certificate is unattributed and revocable only by rotation")
	installCmd.Flags().Bool("no-login", false, "skip the inline sign-in; run kip auth login && kip auth verify later")
	installCmd.Flags().Bool("firewall", true, "configure the host firewall for k3s: firewalld on Rocky, AlmaLinux and CentOS Stream; UFW elsewhere (firewalld takeover requires --adopt-firewall or interactive consent)")
	installCmd.Flags().Bool("adopt-firewall", false, "allow takeover of running firewalld on Rocky, AlmaLinux or CentOS Stream without a prompt; replaces the public zone, removes services such as Cockpit, and allows SSH and TCP ports 80, 443 and 6443 (not with --local)")
	installCmd.Flags().String("request", "", requestFlagUsage)
	_ = installCmd.Flags().MarkHidden("request")
	installCmd.Flags().Bool("via-server", false, "run the install as a persistent server job and stream its log (default on Windows)")
	installCmd.Flags().String("server-kip", "", "with --via-server: upload a local Linux kip binary, useful for unreleased builds")
	installCmd.Flags().Bool("cancel", false, "with --via-server: stop the running install or discard its uncollected result")
	installCmd.Flags().Bool("local", false, "install on this machine; --host must reach it (for CI, cloud-init and provider user-data)")
	installCmd.Flags().Bool("host-maps-here", false, "with --local: confirm that --host routes to this machine, such as through NAT")
	installCmd.Flags().String("result", "", "with --local: save a result file to import on your management machine with 'kip cluster add --install-result'")
	installCmd.Flags().String("credentials-file", "", "save the generated admin email and password to a new local file (mode 0600); print them if saving fails")
	installCmd.Flags().Bool("no-ssh-rate-limit", false, "disable UFW SSH rate limiting on Ubuntu and Debian; useful for CI or operators sharing a NAT address")
	installCmd.Flags().StringSlice("dns-resolver", nil, "upstream DNS resolver CoreDNS forwards external queries to (repeatable; default 1.1.1.1, 8.8.8.8, 9.9.9.9). Set for clusters that must reach internal/corporate DNS.")
	installCmd.Flags().StringSlice("trusted-proxy", nil, "IP or CIDR whose X-Forwarded-* headers Traefik honors (repeatable). Set for clusters behind an external load balancer; the kipper.run gateway is trusted automatically.")

	installCmd.Flags().String("backup-storage-bucket", "", "S3-compatible bucket name for Velero backups (must already exist)")
	installCmd.Flags().String("backup-storage-region", "", "AWS region (or provider equivalent, e.g. 'auto' for Cloudflare R2)")
	installCmd.Flags().String("backup-storage-endpoint", "", "S3 endpoint URL (omit for native AWS S3, required for R2/MinIO/B2/Wasabi/Spaces)")
	installCmd.Flags().String("backup-storage-credentials", "", "path to AWS-style credentials file (defaults to ~/.aws/credentials when --backup-storage-bucket is set)")
	installCmd.Flags().String("backup-storage-profile", "default", "profile name within the credentials file (e.g. 'acme')")

	_ = installCmd.MarkFlagRequired("host")

	rootCmd.AddCommand(installCmd)
}

func runInstall(cmd *cobra.Command, args []string) error {
	if request, _ := cmd.Flags().GetString("request"); request != "" {
		return runInstallRequest(request)
	}
	credentialsFile, err := credentialsFilePath(cmd)
	if err != nil {
		return err
	}
	if usesDriver(cmd, runtime.GOOS) {
		return runDrivenInstall(cmd, credentialsFile)
	}
	if cancel, _ := cmd.Flags().GetBool("cancel"); cancel {
		return fmt.Errorf("--cancel stops an install run with --via-server")
	}
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
	adoptFirewall, _ := cmd.Flags().GetBool("adopt-firewall")
	noSSHRateLimit, _ := cmd.Flags().GetBool("no-ssh-rate-limit")
	adminKubeconfig, _ := cmd.Flags().GetBool("admin-kubeconfig")
	noLogin, _ := cmd.Flags().GetBool("no-login")
	dnsResolvers, _ := cmd.Flags().GetStringSlice("dns-resolver")
	trustedProxies, _ := cmd.Flags().GetStringSlice("trusted-proxy")
	local, _ := cmd.Flags().GetBool("local")
	hostMapsHere, _ := cmd.Flags().GetBool("host-maps-here")
	resultPath, _ := cmd.Flags().GetString("result")
	if !local && (hostMapsHere || resultPath != "") {
		return fmt.Errorf("--host-maps-here and --result only apply with --local")
	}
	if err := checkOutputPathsDiffer(credentialsFile, resultPath); err != nil {
		return err
	}
	if err := checkLocalAdoption(local, adoptFirewall); err != nil {
		return err
	}

	// Canonicalize the name before anything reads it, so LAB.KIPPER.RUN and
	// lab.kipper.run. take the same path as the host they name.
	domain = installer.NormaliseDomain(domain)

	// Decide the *.kipper.run name here so an unregistrable one fails now,
	// before this machine has connected to anything. Whether the name is free is
	// the gateway's to answer and is asked later, still ahead of the first change
	// to the server.
	_, wantsGatewayName, err := installer.KipperRunLabelFor(domain, host)
	if err != nil {
		return err
	}

	// When a custom domain is provided and the admin email was not
	// explicitly set, derive a valid email from the domain. The
	// default admin@kipper.local is rejected by Let's Encrypt.
	//
	// A *.kipper.run domain is left alone: those hosts are served behind the
	// gateway's own certificate and issue nothing through ACME, so both free-tier
	// paths keep the same default whether or not the label was chosen.
	if domain != "" && !wantsGatewayName && !cmd.Flags().Changed("admin-email") {
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
	if local {
		if err := guardLocalInstall(host, hostMapsHere, isTTY); err != nil {
			return err
		}
		// Defer browser sign-in to the machine used to manage the cluster.
		noLogin = true
		if !noSSHRateLimit {
			fmt.Printf("\n  The firewall will limit SSH to six new connections per thirty seconds per source.\n")
			fmt.Printf("  If you will manage this cluster from Windows, install with --no-ssh-rate-limit.\n")
		}
	}
	kubeconfigMode := installer.ResolveKubeconfigMode(adminKubeconfig, noLogin, isTTY)

	result, installErr := installer.Run(installer.Options{
		KipVersion:         installingKipVersion(),
		Host:               host,
		Local:              local,
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
		AdoptFirewall:      adoptFirewall,
		ConfirmAdoption:    adoptionQuestion(isTTY),
		NoSSHRateLimit:     noSSHRateLimit,
		BackupStorage:      backupStorage,
		DNSResolvers:       dnsResolvers,
		TrustedProxies:     trustedProxies,
		KubeconfigMode:     kubeconfigMode,
		CredentialsFile:    credentialsFile,
		LoginGate: func(ctx context.Context, domain, dexHost, server string, caData []byte) installer.GateResult {
			return installer.DefaultLoginGate(domain, dexHost)(ctx, domain, server, caData)
		},
	})

	// Write the result first so an undetected path collision triggers the
	// credential save's exclusive-creation check and the summary prints the password.
	var resultErr error
	if resultPath != "" && result != nil {
		resultErr = writeInstallResult(resultPath, host)
	}

	// Deliver available credentials even after a later install step fails.
	printInstallSummary(os.Stdout, result, installErr, credentialsFile)

	if resultPath != "" && result != nil {
		if resultErr != nil {
			return errors.Join(installErr, resultErr)
		}
		fmt.Printf("  ✔  Install result written to %s. On the machine you manage the cluster from: kip cluster add --install-result <file>\n\n", resultPath)
	}
	return installErr
}

// printInstallSummary shows cluster details and saves or prints credentials
// unless the interactive sign-in already delivered them.
func printInstallSummary(out io.Writer, result *installer.Result, installErr error, credentialsFile string) {
	if result == nil {
		return
	}
	if installErr != nil {
		if result.AdminPassword == "" {
			return
		}
		_, _ = fmt.Fprintf(out, "\n  Install stopped with errors (see above). Available cluster details:\n\n")
	} else {
		_, _ = fmt.Fprintf(out, "  Cluster ready.\n")
	}
	_, _ = fmt.Fprintf(out, "  Console:    %s\n", result.ConsoleURL)
	if result.KubeconfigPath != "" {
		_, _ = fmt.Fprintf(out, "  Kubeconfig: %s\n", result.KubeconfigPath)
	}
	if result.AdminPassword != "" && !result.CredentialsShown &&
		!installer.SaveAdminCredentials(out, credentialsFile, "admin@"+result.Domain, result.AdminPassword) {
		_, _ = fmt.Fprintf(out, "  Admin:      admin@%s\n", result.Domain)
		_, _ = fmt.Fprintf(out, "  Password:   %s\n", result.AdminPassword)
		_, _ = fmt.Fprintln(out)
		_, _ = fmt.Fprintf(out, "  Save these credentials now. They will not be shown again.\n")
		_, _ = fmt.Fprintf(out, "  If lost, run: kip auth reset-password\n")
	}
	_, _ = fmt.Fprintln(out)
}

// credentialsFilePath validates the local destination before installation.
// Server-driven installs keep this path on the client.
func credentialsFilePath(cmd *cobra.Command) (string, error) {
	path, _ := cmd.Flags().GetString("credentials-file")
	if path == "" {
		return "", nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("--credentials-file %s: %w", path, err)
	}
	return abs, installer.CheckCredentialsFile(abs)
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

func adoptionQuestion(isTTY bool) func(string) bool {
	if !isTTY {
		return nil
	}
	return confirm
}

// checkLocalAdoption requires an external SSH check before firewall takeover.
func checkLocalAdoption(local, adopt bool) error {
	if local && adopt {
		return fmt.Errorf("--adopt-firewall does not combine with --local: takeover requires an external SSH check; run kip install over SSH or with --via-server")
	}
	return nil
}

var pathSeparators = func() string {
	if os.PathSeparator == '/' {
		return "/"
	}
	return string(os.PathSeparator) + "/"
}()

// checkOutputPathsDiffer prevents the result and credentials from sharing a file.
func checkOutputPathsDiffer(credentialsFile, resultPath string) error {
	if credentialsFile == "" || resultPath == "" {
		return nil
	}
	a, b := resolvedPath(credentialsFile), resolvedPath(resultPath)
	if a == b || (runtime.GOOS != "linux" && strings.EqualFold(a, b)) {
		return fmt.Errorf("--credentials-file and --result name the same file; choose two different paths")
	}
	return nil
}

// resolvedPath resolves symlinks in the full path or its parent, falling back
// to lexical cleaning. Resolve symlinks before cleaning so ".." follows the
// filesystem hierarchy.
func resolvedPath(path string) string {
	if !filepath.IsAbs(path) {
		if wd, err := os.Getwd(); err == nil {
			path = wd + string(filepath.Separator) + path
		}
	}
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	i := strings.LastIndexAny(path, pathSeparators)
	dir, base := path[:i+1], path[i+1:]
	if base == "" || base == "." || base == ".." {
		return filepath.Clean(path)
	}
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		return filepath.Join(real, base)
	}
	return filepath.Clean(path)
}
