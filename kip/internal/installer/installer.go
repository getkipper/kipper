package installer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/bcrypt"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/domain"
	"github.com/getkipper/kipper/kip/internal/infra"
)

// Options holds the parameters for a cluster installation.
type Options struct {
	Host   string
	Domain string
	// KipVersion is the version of the CLI performing the install, recorded on
	// the CRDs it writes. A later, older kip reads it to refuse replacing a
	// newer schema with its own. Empty leaves the CRDs unstamped, which reads
	// as an install that predates stamping.
	KipVersion string
	// ConsoleDomain, ConsoleAPIDomain, DexDomain are optional admin
	// overrides for each component's public hostname. Empty values fall
	// back to the SubdomainFor convention applied to Domain.
	ConsoleDomain    string
	ConsoleAPIDomain string
	DexDomain        string
	// SSHKeyPath is the user's explicit key choice. When non-empty, ssh
	// is invoked with -i + IdentitiesOnly=yes to force this key.
	SSHKeyPath string
	// FallbackSSHKeyPath is a soft hint (typically ~/.ssh/id_ed25519);
	// ssh tries it if it exists and otherwise falls through to ssh-agent
	// and the rest of OpenSSH's normal key resolution.
	FallbackSSHKeyPath string
	AdminEmail         string
	AdminPasswordHash  string
	GatewayURL         string
	Org                string
	OrgDisplayName     string
	// Harden enables host-level security hardening of surplus services
	// (e.g. masking rpcbind so it does not respond to public scans).
	// Default is true via the --harden flag; set false only when the
	// operator manages host security themselves.
	Harden bool
	// Firewall enables installation and configuration of UFW with a
	// k3s-correct ruleset. Default is true via the --firewall flag.
	// If a pre-existing host firewall is detected (UFW or firewalld),
	// this step is skipped regardless of the flag value.
	Firewall bool

	// NoSSHRateLimit opens the SSH port outright instead of rate-limiting it.
	//
	// The limit allows six connections per thirty seconds from one address,
	// which is generous for a person and reachable for CI running several kip
	// commands in a row, or for two operators sharing a NAT address. It is a
	// rule about a source address, not about an attacker, so anyone whose
	// legitimate traffic looks like a burst needs this.
	NoSSHRateLimit bool
	// BackupStorage configures Velero's BSL. When nil the installer
	// deploys an in-cluster MinIO (default). When non-nil the installer
	// skips MinIO and points Velero at the user-supplied S3-compatible
	// bucket so backups survive a cluster wipe.
	BackupStorage *BackupStorageConfig
	// DNSResolvers are the upstream nameservers CoreDNS forwards external
	// queries to. Empty uses the default public resolvers; set it (via
	// --dns-resolver) for clusters that must reach internal or corporate
	// DNS.
	DNSResolvers []string
	// TrustedProxies are extra IPs/CIDRs (via --trusted-proxy) whose
	// X-Forwarded-* headers Traefik honours, for clusters behind an
	// external load balancer. The kipper.run gateway is trusted
	// automatically by resolving the cluster's own domain; it is never
	// persisted here.
	TrustedProxies []string
	// KubeconfigMode decides what credential lands on the operator machine:
	// the credential-free exec kubeconfig (interactive or deferred) or the
	// shared admin certificate (explicit opt-out). Zero value is
	// KubeconfigExecInteractive.
	KubeconfigMode KubeconfigMode
	// LoginGate runs the inline OIDC login and proof for an interactive
	// install. nil skips the gate (the exec kubeconfig is written but not
	// verified inline) — the cmd layer wires the real gate.
	LoginGate func(ctx context.Context, domain, dexHost, server string, caData []byte) GateResult
}

// Result contains the outputs of a successful installation.
type Result struct {
	Domain         string
	ConsoleURL     string
	KubeconfigPath string
	GatewayToken   string
	AdminPassword  string
	// AuthMode records how the operator authenticates: "oidc" (verified
	// exec kubeconfig), "deferred" (exec kubeconfig; login pending,
	// unreachable, or proof failed — finish with kip auth verify), or
	// "admin-optout" (--admin-kubeconfig). The exec kubeconfig is the only
	// thing install writes by default; the shared certificate lands on disk
	// only via --admin-kubeconfig.
	AuthMode      string
	VerifiedEmail string
	// CredentialsShown reports that the admin credentials were already printed,
	// which the interactive path does before its sign-in prompt. The caller's
	// closing summary reads it so one install does not disclose the same
	// password twice under two different "save this now" headings.
	CredentialsShown bool
}

// Run executes the full cluster installation sequence.
func Run(opts Options) (*Result, error) {
	// Without a version the CRDs this install writes go out unstamped, and an
	// unstamped CRD is read as predating the guard — so a later, older kip
	// replaces those schemas unchallenged. That is a silent loss discovered
	// only on the downgrade, so refuse here instead, before any side effect.
	// This is what protects the wiring: a test can prove InstallCRDs stamps
	// what it is given, but only this proves something gives it a version.
	if strings.TrimSpace(opts.KipVersion) == "" {
		return nil, fmt.Errorf("install was invoked without a kip version, so the CRDs it writes could not be stamped")
	}

	// Canonicalise every hostname the operator typed before anything compares
	// them. The gateway suffix test, the label rule and the CRD's lowercase
	// pattern all work on the string, so LAB.KIPPER.RUN reaching them unchanged
	// is read as a custom domain, registers nothing, and installs a cluster
	// serving a name the gateway will not route.
	opts.Domain = NormaliseDomain(opts.Domain)
	opts.ConsoleDomain = NormaliseDomain(opts.ConsoleDomain)
	opts.ConsoleAPIDomain = NormaliseDomain(opts.ConsoleAPIDomain)
	opts.DexDomain = NormaliseDomain(opts.DexDomain)

	// Reject an explicit --dns-resolver before any side effect (SSH,
	// firewall, subdomain registration). The no-flag path is validated
	// later, once domainName is known and any persisted list is inherited.
	if len(opts.DNSResolvers) > 0 {
		resolved, err := resolveDNSResolvers(opts.DNSResolvers)
		if err != nil {
			return nil, err
		}
		opts.DNSResolvers = resolved
	}
	// Same fail-fast for --trusted-proxy: a typo'd CIDR must not surface
	// halfway through an install at the Traefik step.
	if len(opts.TrustedProxies) > 0 {
		if _, err := renderTrustedIPs(opts.TrustedProxies); err != nil {
			return nil, err
		}
	}

	provider := &infra.BareMetalProvider{
		Host:           opts.Host,
		SSHKey:         opts.SSHKeyPath,
		FallbackSSHKey: opts.FallbackSSHKeyPath,
	}

	fmt.Printf("\n  Connecting to %s...\n", opts.Host)
	if err := provider.Connect(); err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", opts.Host, err)
	}
	defer func() { _ = provider.Close() }()
	fmt.Printf("  ✔  Connected\n\n")

	client := provider.Client()

	// Gather system info and run preflight checks
	fmt.Printf("  Running preflight checks...\n")
	sysInfo, err := GatherSystemInfo(client)
	if err != nil {
		return nil, fmt.Errorf("gathering system info: %w", err)
	}

	preflight, err := RunPreflightChecks(sysInfo)
	if err != nil {
		return nil, err
	}
	fmt.Printf("  ✔  OS: %s %s\n", sysInfo.OS, sysInfo.OSVersion)
	fmt.Printf("  ✔  RAM: %dMB available\n", sysInfo.RAMMB)
	fmt.Printf("  ✔  Disk: %dMB available\n", sysInfo.DiskMB)
	fmt.Printf("  ✔  Ports: 80, 443, 6443 open\n")

	platformProfile := pickProfile(sysInfo.RAMMB)
	fmt.Printf("  ✔  Platform profile: %s\n", platformProfile)
	if platformProfile == ProfileNano {
		fmt.Printf("  ℹ  Monitoring (Prometheus, Loki, Grafana) will be skipped to keep this nano install lean.\n")
	}
	for _, w := range preflight.Warnings {
		fmt.Printf("  ⚠  %s\n", w)
	}
	fmt.Println()

	// Audit host security and decide whether to insert a hardening step.
	fmt.Printf("  Auditing host security...\n")
	audit, err := AuditHost(client)
	if err != nil {
		return nil, fmt.Errorf("auditing host: %w", err)
	}
	findings := audit.Findings()
	if len(findings) == 0 {
		fmt.Printf("  ✔  No surplus services detected\n")
	} else {
		for _, f := range findings {
			fmt.Printf("  ⚠  %s\n", f)
		}
	}

	// Audit existing firewall — we never trample an admin's existing rules.
	fwAudit, err := AuditFirewall(client)
	if err != nil {
		return nil, fmt.Errorf("auditing firewall: %w", err)
	}
	fwPlan := PlanFirewall(fwAudit, opts.Firewall)
	configureFirewall := fwPlan.Configure
	fmt.Print(fwPlan.Notice)

	if !opts.Harden && len(findings) > 0 {
		fmt.Println()
		fmt.Printf("  ⚠  Host hardening skipped (--harden=false).\n")
		fmt.Printf("     Findings not addressed:\n")
		for _, f := range findings {
			fmt.Printf("       - %s\n", f)
		}
		fmt.Printf("     Re-run with --harden, or fix manually.\n")
	}
	if fwPlan.FlagNotice != "" {
		fmt.Println()
		fmt.Print(fwPlan.FlagNotice)
	}
	fmt.Println()

	// An existing cluster's serving identity comes from its ClusterIdentity
	// CR, never re-derived from flags or the IP: re-deriving would replace a
	// reconfigured identity outside the phase machine (no dual-serve, no
	// approval, no rollback snapshot), and the reconciler's issuer guard
	// would then hold the cluster on the wrong identity instead of
	// converging back.
	existingIdentity, k3sPreexisting, err := ReadExistingClusterIdentity(client)
	if err != nil {
		return nil, err
	}

	// Register a kipper.run subdomain or use a custom domain
	var gatewayToken string
	// freshlyRegistered marks a label this run claimed, whose token the gateway
	// disclosed exactly once and which nothing else holds yet.
	freshlyRegistered := false
	credentialsAttempted := false
	domainName := opts.Domain
	if existingIdentity != nil {
		adopted, err := AdoptIdentity(existingIdentity, opts.Domain, opts.ConsoleDomain, opts.ConsoleAPIDomain, opts.DexDomain)
		if err != nil {
			return nil, err
		}
		domainName = adopted.Domain
		opts.ConsoleDomain = adopted.ConsoleHost
		opts.ConsoleAPIDomain = adopted.ConsoleAPIHost
		opts.DexDomain = adopted.DexHost
		fmt.Printf("  ✔  Existing cluster: keeping its serving identity %s\n\n", domainName)
	} else {
		// Either no --domain, taking the label derived from the server's address,
		// or a *.kipper.run --domain naming the label the operator chose. Both
		// claim a name from the gateway, which owns that DNS. A custom domain
		// claims nothing, because its DNS is the operator's.
		subdomain, wantsGatewayName, labelErr := KipperRunLabelFor(opts.Domain, opts.Host)
		if labelErr != nil {
			return nil, labelErr
		}
		if wantsGatewayName {
			fmt.Printf("  Registering subdomain...\n")
			claimed, claimErr := claimGatewayName(
				domain.NewGatewayClient(opts.GatewayURL),
				localTokenStore{},
				subdomain, opts.Host)
			if claimErr != nil {
				return nil, claimErr
			}
			domainName = claimed.Domain
			gatewayToken = claimed.Token
			// Only a name this run brought into existence may be handed back if
			// the install fails. A renewal means the registration predates this
			// attempt, and a half-built cluster from an earlier one may still be
			// serving on it — releasing that frees a name in use.
			freshlyRegistered = claimed.Created
			fmt.Printf("  ✔  Subdomain assigned: %s\n\n", domainName)
		}
	}
	// A re-run inherits per-cluster state from the local entry for this HOST.
	// The host is the server's stable identity: the entry's Name may be a
	// short alias ('kip cluster rename') and its Domain moves with 'kip
	// cluster domain', so keying on either would miss the entry and silently
	// reset the settings below to defaults. The same entry (and its name) is
	// reused when the result is persisted.
	clusterEntryName := domainName
	if cfg, err := config.Load(); err == nil {
		if existing := cfg.GetClusterByHost(opts.Host); existing != nil {
			clusterEntryName = existing.Name
			// A reinstall registers nothing with the gateway, so inherit the
			// token mirror rather than clearing it.
			if gatewayToken == "" {
				gatewayToken = existing.GatewayToken
			}
			// With no explicit flag, inherit the resolver choice persisted at
			// install time so a re-run does not silently revert a custom
			// resolver to the public defaults. An explicit flag was already
			// validated above and wins.
			if len(opts.DNSResolvers) == 0 {
				opts.DNSResolvers = existing.DNSResolvers
			}
			// Same for the trusted-proxy extras: a re-run without the flag
			// keeps the operator's load-balancer trust instead of dropping it.
			if len(opts.TrustedProxies) == 0 {
				opts.TrustedProxies = existing.TrustedProxies
			}
		}
	}
	dnsResolvers, err := resolveDNSResolvers(opts.DNSResolvers)
	if err != nil {
		return nil, err
	}
	opts.DNSResolvers = dnsResolvers

	// Generate an admin password if none was provided
	adminPassword := ""
	if opts.AdminPasswordHash == "" {
		pw := make([]byte, 16)
		rand.Read(pw)
		adminPassword = hex.EncodeToString(pw)
		hash, err := bcrypt.GenerateFromPassword([]byte(adminPassword), bcrypt.DefaultCost)
		if err != nil {
			return nil, fmt.Errorf("hashing admin password: %w", err)
		}
		opts.AdminPasswordHash = string(hash)
	}

	// Resolve per-component hostnames. Admin overrides win; otherwise
	// the SubdomainFor convention is applied to domainName.
	resolvedHosts := struct {
		ConsoleHost    string
		ConsoleAPIHost string
		DexHost        string
	}{
		ConsoleHost:    pickHost(opts.ConsoleDomain, "console", domainName),
		ConsoleAPIHost: pickHost(opts.ConsoleAPIDomain, "console-api", domainName),
		DexHost:        pickHost(opts.DexDomain, "dex", domainName),
	}

	// The registered *.kipper.run domain the gateway heartbeat keeps alive.
	// A *.kipper.run install registers a subdomain, whether the label was derived
	// from the address (opts.Domain empty) or chosen by the operator; a
	// custom-domain install has no gateway registration; an adopted identity
	// keeps whatever registration its CR already records — after a custom-
	// domain move that differs from the serving domain.
	//
	// The condition tracks the claim above rather than restating it as
	// "opts.Domain == "". Reading only the empty case is what left a chosen label
	// with no gateway block: console-api got no KIPPER_RUN_DOMAIN, never
	// heartbeated, and the cluster served a name the gateway would not route.
	gatewayKipperRunDomain := gatewayRegistrationFor(existingIdentity, domainName)

	// Install cluster components
	type installStep struct {
		name string
		fn   func() error
	}
	var steps []installStep
	if opts.Harden && len(findings) > 0 {
		steps = append(steps, installStep{"Hardening host OS", func() error {
			return HardenHost(client)
		}})
	}
	steps = append(steps, []installStep{
		{"Installing k3s", func() error {
			return InstallK3s(client, opts.Host, opts.DNSResolvers, k3sPreexisting)
		}},
	}...)
	if configureFirewall {
		steps = append(steps, installStep{"Configuring firewall", func() error {
			return ApplyFirewallPlan(client, fwPlan, !opts.NoSSHRateLimit)
		}})
	}
	// effectivePlatform is the cluster's actual sizing state after
	// InstallPlatformConfig runs (which preserves an existing CR's
	// overrides on reinstall). The observability install steps below
	// honor this, so a user who previously disabled Loki via
	// `kip platform disable loki` doesn't get it resurrected just
	// because re-running `kip install` re-detected a non-nano profile
	// from the host.
	var effectivePlatform PlatformState

	steps = append(steps, []installStep{
		{"Registering Kipper CRDs", func() error {
			return InstallCRDs(client, opts.KipVersion)
		}},
		{"Recording platform sizing profile", func() error {
			if err := InstallPlatformConfig(client, platformProfile); err != nil {
				return err
			}
			state, err := ReadPlatformStateViaSSH(client)
			if err != nil {
				return err
			}
			effectivePlatform = state
			return nil
		}},
		{"Installing Traefik ingress", func() error {
			return InstallTraefik(client, ResolveTrustedProxies(domainName, opts.TrustedProxies))
		}},
		{"Applying security hardening", func() error {
			return InstallSecurityHardening(client)
		}},
		{"Configuring cert-manager", func() error {
			return InstallCertManager(client, opts.AdminEmail, opts.DNSResolvers)
		}},
		{"Setting up storage", func() error {
			return InstallLonghorn(client)
		}},
		{"Installing KEDA autoscaler", func() error {
			return InstallKEDA(client)
		}},
	}...)

	// Always include the observability steps; each one consults the
	// effective platform state at run time and skips when the
	// corresponding component is disabled on the cluster. This makes
	// reinstall behavior predictable: the CR is the single source of
	// truth even when host-side profile detection would suggest a
	// different default.
	steps = append(steps, []installStep{
		{"Installing log aggregation (Loki)", func() error {
			if !effectivePlatform.LokiEnabled() {
				fmt.Printf("       (disabled in PlatformConfig; skipping)\n")
				return nil
			}
			return InstallLokiWithResources(client, effectivePlatform.EffectiveResources())
		}},
		{"Installing metrics and dashboards (Prometheus + Grafana)", func() error {
			if !effectivePlatform.PrometheusEnabled() {
				fmt.Printf("       (disabled in PlatformConfig; skipping)\n")
				return nil
			}
			return InstallPrometheusGrafanaWithResources(client, effectivePlatform.EffectiveResources())
		}},
	}...)

	steps = append(steps, []installStep{
		{"Setting up backup and restore (Velero)", func() error {
			return InstallBackup(client, opts.BackupStorage)
		}},
		{"Creating kipper-system namespace", func() error {
			_, err := client.Run("kubectl create namespace kipper-system --dry-run=client -o yaml | kubectl apply -f -")
			return err
		}},
		{"Storing gateway credentials", func() error {
			// Marked before the attempt, not after it. A dropped connection can
			// lose the acknowledgement of a kubectl apply that committed, so a
			// returned error does not prove the cluster is without the token —
			// and releasing the name on that assumption frees a name the cluster
			// is about to serve on. Once this has been tried, the name stays.
			credentialsAttempted = true
			// An empty token here is a re-install of a cluster whose token was
			// never mirrored locally; a fresh registration without one is
			// refused above. Nothing can be stored, and the cluster cannot
			// prove possession or release its name — so say it rather than
			// print a tick over a step that did nothing.
			if gatewayToken == "" {
				if strings.HasSuffix(domainName, ".kipper.run") {
					fmt.Printf("\n  !   No gateway token is available for %s, so this cluster cannot prove\n", domainName)
					fmt.Printf("      possession of its name. It will not be routable through the gateway,\n")
					fmt.Printf("      and its name cannot be released later.\n")
				}
				return nil
			}
			return StoreGatewayCredentials(client, gatewayToken)
		}},
		{"Minting the cluster certificate authority", func() error {
			return EnsureHopMaterial(client)
		}},
		{"Installing container registry (Zot)", func() error {
			return InstallZot(client)
		}},
		{"Configuring identity provider", func() error {
			return InstallDex(client, resolvedHosts.DexHost, resolvedHosts.ConsoleHost, domainName, opts.AdminPasswordHash)
		}},
		{"Staging operator access", func() error {
			return InstallOperatorRBAC(client, domainName)
		}},
		{"Enabling operator authentication", func() error {
			return InstallOperatorAuth(client, resolvedHosts.DexHost)
		}},
		{"Deploying console", func() error {
			return DeployConsole(client, resolvedHosts.DexHost, resolvedHosts.ConsoleHost, resolvedHosts.ConsoleAPIHost, domainName, gatewayKipperRunDomain, opts.Host)
		}},
		{"Recording serving identity", func() error {
			return InstallClusterIdentity(client, domainName, opts.ConsoleDomain, opts.ConsoleAPIDomain, opts.DexDomain, gatewayKipperRunDomain, opts.Host)
		}},
		{"Deploying API key service", func() error {
			return InstallAuthz(client)
		}},
		{"Isolating image builds", func() error {
			return InstallBuildIsolation(client)
		}},
	}...)

	consoleURL := fmt.Sprintf("https://%s", resolvedHosts.ConsoleHost)
	partialResult := &Result{
		Domain:        domainName,
		ConsoleURL:    consoleURL,
		AdminPassword: adminPassword,
	}

	// The line between reading this host and changing it. Everything above only
	// inspects the server — preflight, the security and firewall audits, the
	// identity read, resolver validation — and any of them can reject it having
	// changed nothing, which is why a wipe an earlier uninstall recorded has to
	// survive them. From the first step below, this host may carry a cluster
	// again, and a marker saying otherwise would send a later uninstall past the
	// wipe. Clearing early costs at most a prompt: an uninstall that cannot
	// reach the host still offers to hand the name back without it.
	if err := ClearHostWipedMarker(opts.Host); err != nil {
		// The last exit before the step loop, and the only one past the gateway
		// claim, so it owns the tidying the loop's own failure handler would
		// otherwise do: a name this run brought into existence goes back rather
		// than sitting on a registration whose cluster was never built.
		if freshlyRegistered && gatewayToken != "" {
			if relErr := domain.NewGatewayClient(opts.GatewayURL).Deregister(gatewayToken); relErr == nil {
				if clearErr := clearTokenIfHeld(opts.Host, gatewayToken); clearErr != nil {
					fmt.Printf("  !   Released %s, but its dead credential is still recorded locally (%v).\n", domainName, clearErr)
					fmt.Printf("      A later command may read that as the name already being released.\n")
				} else {
					fmt.Printf("  ·  Released %s, so re-running this install can claim it again.\n", domainName)
				}
			} else {
				fmt.Printf("  ·  %s stays registered (%v). Re-running this install renews it.\n", domainName, relErr)
			}
		}
		return nil, fmt.Errorf("clearing the wiped marker for %s: %w", opts.Host, err)
	}

	fmt.Printf("  Installing cluster...\n")

	var kubeconfigPath string
	// The admin certificate is held only inside writeClusterKubeconfig, which
	// converts it to a credential-free kubeconfig (or writes it verbatim only
	// in the explicit admin-cert opt-out). It never persists in this scope,
	// so no failure path can land it on disk. execServer/execCA carry the
	// server and CA the login gate proves against.
	var execServer string
	var execCA []byte
	pastCheckpoint := false
	// clusterRegistered tracks the second half of the install checkpoint. The
	// default exec kubeconfig is not self-contained: `kip auth kubectl-token`
	// resolves the cluster (and its Dex host) from this config entry, so a
	// written exec kubeconfig without a matching registered entry is an
	// unusable credential. Registration failures must therefore never be
	// swallowed — the fallback below retries and fails the install loudly
	// rather than reporting success with a broken credential.
	clusterRegistered := false
	registerCluster := func(kubeconfigPath string) error {
		return config.Update(func(cfg *config.Config) error {
			cfg.AddCluster(config.Cluster{
				Name:             clusterEntryName,
				Provider:         provider.Name(),
				Host:             opts.Host,
				Domain:           domainName,
				ConsoleDomain:    opts.ConsoleDomain,
				ConsoleAPIDomain: opts.ConsoleAPIDomain,
				DexDomain:        opts.DexDomain,
				Kubeconfig:       kubeconfigPath,
				SSHKey:           opts.SSHKeyPath,
				GatewayToken:     gatewayToken,
				Org:              opts.Org,
				OrgDisplayName:   opts.OrgDisplayName,
				BackupStorage:    backupStorageRef(opts.BackupStorage),
				DNSResolvers:     opts.DNSResolvers,
				TrustedProxies:   opts.TrustedProxies,
			})
			cfg.CurrentCluster = clusterEntryName
			return nil
		})
	}
	for _, step := range steps {
		fmt.Printf("  ...  %s\n", step.name)
		if err := step.fn(); err != nil {
			// A failed install keeps the credential-free kubeconfig — a
			// component error must never land the shared admin certificate on
			// disk (it would recreate the attribution/revocation hole exactly
			// when the install is unhealthy and likely to be copied around).
			// The operator inspects the half-built cluster through the
			// server-side break-glass path they already hold.
			if pastCheckpoint && opts.KubeconfigMode != KubeconfigAdminCert {
				fmt.Printf("  ⚠  Install failed at %q. Your machine holds no cluster credential. Inspect the server with: ssh <host>, then sudo k3s kubectl. Re-run kip install when ready, or re-run with --admin-kubeconfig if you need a local credential now.\n", step.name)
			}
			// A name this run created, abandoned before the cluster ever held
			// its token. The token is durable by now — claimGatewayName does not
			// return until it is — so this is not stranding-prevention; that is
			// the claim's job and it owns the case where the record cannot be
			// written. This is tidiness: a name nothing is using goes back, and
			// the retry starts from an ordinary first install rather than
			// renewing a registration whose cluster never existed.
			//
			// Only a name this run created. A renewal predates this attempt, and
			// a half-built cluster from the earlier one may still be serving.
			if freshlyRegistered && !credentialsAttempted && gatewayToken != "" {
				if relErr := domain.NewGatewayClient(opts.GatewayURL).Deregister(gatewayToken); relErr == nil {
					// The token names a registration that no longer exists.
					// Leaving it on disk would let a later command present a
					// dead credential and read the gateway's "not registered"
					// as "already released", hiding a real registration made
					// since.
					if clearErr := clearTokenIfHeld(opts.Host, gatewayToken); clearErr != nil {
						fmt.Printf("  !   Released %s, but its dead credential is still recorded locally (%v).\n", domainName, clearErr)
						fmt.Printf("      A later command may read that as the name already being released.\n")
					} else {
						fmt.Printf("  ·  Released %s, so re-running this install can claim it again.\n", domainName)
					}
				} else {
					fmt.Printf("  !   Could not release %s: %v\n", domainName, relErr)
					fmt.Printf("      Re-running will renew it rather than claim it fresh; the local record still holds its token.\n")
				}
			}
			partialResult.KubeconfigPath = kubeconfigPath
			partialResult.GatewayToken = gatewayToken
			return partialResult, fmt.Errorf("%s: %w", step.name, err)
		}
		fmt.Printf("  ✔  %s\n", step.name)

		// As soon as k3s is up, save the kubeconfig and register the
		// cluster in config so credentials survive a later step failing.
		if step.name == "Installing k3s" && kubeconfigPath == "" {
			kubeconfig, fetchErr := FetchKubeconfig(client, opts.Host)
			if fetchErr == nil {
				pastCheckpoint = true
				if path, srv, ca, saveErr := writeClusterKubeconfig(domainName, kubeconfig, opts.KubeconfigMode); saveErr == nil {
					kubeconfigPath, execServer, execCA = path, srv, ca
					// Best effort here; the fallback below re-attempts and fails
					// the install if registration still can't complete, so a
					// written-but-unregistered exec kubeconfig never survives.
					if err := registerCluster(path); err == nil {
						clusterRegistered = true
					}
				}
			}
		}
	}
	fmt.Println()

	// Complete the checkpoint if either half did not land: fetch and write the
	// kubeconfig if missing, and register the cluster entry if it is not yet
	// saved. Registration errors are fatal — the exec kubeconfig is useless
	// without its config entry, so a nominal install "success" that leaves the
	// operator without a working credential is worse than a loud failure they
	// can fix and re-run (the install is idempotent). Mode-aware through the
	// same helper, so this path never lands the admin certificate in exec mode.
	if kubeconfigPath == "" {
		kubeconfig, err := FetchKubeconfig(client, opts.Host)
		if err != nil {
			return nil, err
		}
		var saveErr error
		kubeconfigPath, execServer, execCA, saveErr = writeClusterKubeconfig(domainName, kubeconfig, opts.KubeconfigMode)
		if saveErr != nil {
			return nil, saveErr
		}
		clusterRegistered = false
	}
	if !clusterRegistered {
		if err := registerCluster(kubeconfigPath); err != nil {
			return nil, err
		}
	}

	result := &Result{
		Domain:         domainName,
		ConsoleURL:     consoleURL,
		KubeconfigPath: kubeconfigPath,
		GatewayToken:   gatewayToken,
		AdminPassword:  adminPassword,
	}

	// The exec kubeconfig is already on disk. Decide the auth mode and, for
	// an interactive install with a gate wired, run the inline login + proof.
	switch opts.KubeconfigMode {
	case KubeconfigAdminCert:
		result.AuthMode = "admin-optout"
	case KubeconfigExecDeferred:
		result.AuthMode = "deferred"
		fmt.Printf("  This machine holds no cluster credential. First operator: kip auth login && kip auth verify\n\n")
	case KubeconfigExecInteractive:
		result.AuthMode = "deferred"
		if opts.LoginGate != nil && execServer != "" {
			// Disclosed here rather than by the caller's closing summary: this
			// is the only path that prompts for the password, and the summary
			// does not run until the prompt has been answered or timed out.
			gate := signInWithCredentials(os.Stdout, domainName, adminPassword, func() GateResult {
				return opts.LoginGate(context.Background(), domainName, resolvedHosts.DexHost, execServer, execCA)
			})
			result.CredentialsShown = adminPassword != ""
			result.AuthMode = gate.AuthMode
			result.VerifiedEmail = gate.VerifiedEmail
			// The gate never writes the admin certificate; whatever it decides,
			// the on-disk file stays the credential-free exec kubeconfig.
			if gate.Message != "" {
				fmt.Printf("  %s\n\n", gate.Message)
			}
			if _, pinned := execCommandForHostPinned(); pinned && gate.AuthMode == "oidc" {
				fmt.Printf("  ⚠  kip is not on PATH; the kubeconfig pins this binary's location. Install kip to PATH and run: kip auth kubeconfig\n\n")
			}
		}
	}

	return result, nil
}

// writeClusterKubeconfig converts the admin kubeconfig to what the mode asks
// for and writes it, returning the on-disk content plus the exec server and
// CA (empty in admin-cert mode). Both the k3s checkpoint and the
// ensure-fallback path go through this one function so they can never drift:
// the admin certificate lands on disk only in KubeconfigAdminCert mode.
func writeClusterKubeconfig(domain, adminKubeconfig string, mode KubeconfigMode) (path, server string, caData []byte, err error) {
	content := adminKubeconfig
	if mode != KubeconfigAdminCert {
		ec, srv, ca, rerr := RenderExecFromAdmin(domain, adminKubeconfig, execCommandForHost())
		if rerr != nil {
			return "", "", nil, rerr
		}
		content, server, caData = ec, srv, ca
	}
	path, err = saveKubeconfig(domain, content)
	if err != nil {
		return "", "", nil, err
	}
	return path, server, caData, nil
}

// pickHost returns the admin override when non-empty, otherwise the
// SubdomainFor convention applied to the bare cluster domain.
func pickHost(override, prefix, bareDomain string) string {
	if override != "" {
		return override
	}
	return SubdomainFor(prefix, bareDomain)
}

func saveKubeconfig(name string, content string) (string, error) {
	configDir, err := config.Dir()
	if err != nil {
		return "", err
	}

	clustersDir := filepath.Join(configDir, "clusters")
	if err := os.MkdirAll(clustersDir, 0o700); err != nil {
		return "", fmt.Errorf("creating clusters directory: %w", err)
	}

	path := filepath.Join(clustersDir, name+".yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return "", fmt.Errorf("writing kubeconfig: %w", err)
	}

	return path, nil
}
