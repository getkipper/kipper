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
	"github.com/getkipper/kipper/kip/internal/localrun"
	"github.com/getkipper/kipper/kip/internal/ssh"
)

// Options holds the parameters for a cluster installation.
type Options struct {
	Host                 string
	Domain               string
	KipVersion           string
	ConsoleDomain        string
	ConsoleAPIDomain     string
	DexDomain            string
	Local                bool
	SSHKeyPath           string
	FallbackSSHKeyPath   string
	AdminEmail           string
	AdminPasswordHash    string
	GatewayURL           string
	Org                  string
	OrgDisplayName       string
	Harden               bool
	Firewall             bool
	AdoptFirewall        bool
	NoSSHRateLimit       bool
	BackupStorage        *BackupStorageConfig
	Profile              string
	DNSResolvers         []string
	TrustedProxies       []string
	KubeconfigMode       KubeconfigMode
	LoginGate            func(ctx context.Context, domain, dexHost, server string, caData []byte) GateResult
	ConfirmConnection    func() error
	ConfirmAdoption      func(question string) bool
	AdoptFingerprint     string
	ChangeNetwork        bool
	ConfirmNetworkChange func(question string) bool
	CredentialsFile      string
}

// Result contains installation outputs, including those available after a failure.
type Result struct {
	Domain           string
	ConsoleURL       string
	KubeconfigPath   string
	GatewayToken     string
	AdminPassword    string
	AuthMode         string
	VerifiedEmail    string
	CredentialsShown bool
}

// hostRunner supports installation commands, uploads, and SSH connectivity checks.
type hostRunner interface {
	Uploader
	SurvivesSSHRateLimit() bool
	ConfirmFreshConnection() error
}

var (
	_ hostRunner = (*ssh.Client)(nil)
	_ hostRunner = (*localrun.Runner)(nil)
)

type installStep struct {
	name string
	fn   func() error
}

// adoptionConsent uses supplied consent for local runs, including consent
// collected by the SSH driver. Direct SSH installs may prompt here.
func adoptionConsent(client CommandRunner, opts Options, audit FirewallAudit, mesh FirewallMesh) (FirewallConsent, error) {
	if opts.AdoptFirewall {
		return FirewallConsent{Adopt: true, Fingerprint: opts.AdoptFingerprint}, nil
	}
	ask := opts.ConfirmAdoption
	if opts.Local {
		ask = nil
	}
	return AdoptionConsent(client, opts.Host, audit, opts.Firewall, false, ask, mesh)
}

// FirewallConsent authorizes takeover of an operator-managed firewalld.
// A nonempty fingerprint restricts consent to the previewed configuration.
type FirewallConsent struct {
	Adopt       bool
	Fingerprint string
}

// AdoptionConsent returns flag-based consent or prompts with a takeover preview.
// A nil ask disables prompting. Interactive consent includes the preview fingerprint.
func AdoptionConsent(client CommandRunner, host string, audit FirewallAudit, firewall, adopt bool, ask func(question string) bool, mesh FirewallMesh) (FirewallConsent, error) {
	if adopt {
		return FirewallConsent{Adopt: true}, nil
	}
	if !audit.Adoptable || !firewall || ask == nil {
		return FirewallConsent{}, nil
	}
	preview, err := PreviewFirewalldAdoption(client, mesh)
	if err != nil {
		return FirewallConsent{}, err
	}
	if !preview.Possible {
		fmt.Printf("  ℹ  kip cannot take over the running firewalld: %s\n", preview.Reason)
		return FirewallConsent{}, nil
	}
	if !ask(preview.Question(host)) {
		return FirewallConsent{}, nil
	}
	return FirewallConsent{Adopt: true, Fingerprint: preview.Fingerprint}, nil
}

// hardenAfterStoragePackages returns an optional hardening callback for services
// installed after the initial host audit. Running it before Longhorn deployment
// ensures hardening completes even if deployment fails.
func hardenAfterStoragePackages(harden bool, client CommandRunner) func() error {
	if !harden {
		return nil
	}
	return func() error { return HardenHost(client) }
}

// storageRestartSteps schedules needrestart configuration for Debian-family hosts.
func storageRestartSteps(host HostOS, client CommandRunner) []installStep {
	if !host.UsesNeedrestart() {
		fmt.Printf("  ℹ  Kipper configures needrestart only on Ubuntu and Debian. Update storage packages\n")
		fmt.Printf("     during planned downtime: their package scripts may restart iscsid.\n\n")
		return nil
	}
	return []installStep{{"Deferring storage-path restarts", func() error {
		return ConfigureStorageRestarts(client)
	}}}
}

// k3sAndFirewallSteps configures firewalld before installing k3s so the pod
// zone is available during startup. A nil firewall step leaves the firewall alone.
func k3sAndFirewallSteps(host HostOS, k3s installStep, firewall *installStep) []installStep {
	if firewall == nil {
		return []installStep{k3s}
	}
	if host.Family == FamilyRHEL {
		return []installStep{*firewall, k3s}
	}
	return []installStep{k3s, *firewall}
}

// confirmedRunner delegates SSH connectivity checks to the client driving
// the local installation.
type confirmedRunner struct {
	*localrun.Runner
	confirm func() error
}

func (r confirmedRunner) ConfirmFreshConnection() error { return r.confirm() }

// localClient uses opts.ConfirmConnection for SSH checks during server-driven installs.
func localClient(opts Options) hostRunner {
	if opts.ConfirmConnection != nil {
		return confirmedRunner{Runner: localrun.New(), confirm: opts.ConfirmConnection}
	}
	return localrun.New()
}

// requireOwnFirewalldIntact verifies the queried runtime settings against kip's
// expected configuration when the firewall step is skipped.
func requireOwnFirewalldIntact(client CommandRunner, mesh FirewallMesh) error {
	sshPorts, err := readSSHPorts(client)
	if err != nil {
		return err
	}
	want := wantFirewalldFor(sshPorts, mesh)
	state, err := client.Run(firewalldStateCommand(want))
	if err != nil {
		return fmt.Errorf("reading the running firewalld configuration: %w", err)
	}
	if problems := firewalldMismatches(state, want); len(problems) > 0 {
		return fmt.Errorf("the firewalld kip set up has changed since configuration (%s). Run the install with --firewall to restore kip's rules", strings.Join(problems, "; "))
	}
	return nil
}

// requireOperatorFirewalld checks cluster access when the install preserves
// an existing firewalld configuration.
func requireOperatorFirewalld(client CommandRunner, host HostOS, audit FirewallAudit, plan FirewallPlan) error {
	if host.Family == FamilyRHEL && audit.Existing == "firewalld and ufw" {
		return fmt.Errorf("firewalld and ufw are both running. Disable one so kip can verify cluster access, then retry")
	}
	leftRunning := audit.Existing == "firewalld" && (audit.Adoptable || audit.KipperManaged)
	if !leftRunning || plan.Configure {
		return nil
	}
	if audit.KipperManaged {
		return requireOwnFirewalldIntact(client, plan.Mesh)
	}
	missing, err := CheckOperatorFirewalld(client, plan.Mesh)
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		return fmt.Errorf("the existing firewalld configuration requires these changes for the cluster: %s. %s", strings.Join(missing, "; "), takeoverHint(client, plan.Mesh))
	}
	return nil
}

// takeoverHint suggests adoption when the preview passes configuration checks.
func takeoverHint(client CommandRunner, mesh FirewallMesh) string {
	preview, err := PreviewFirewalldAdoption(client, mesh)
	switch {
	case err != nil:
		return "Update the rules manually"
	case preview.Possible:
		return "Update the rules manually, or run again with --adopt-firewall to let kip replace them"
	default:
		return "Update the rules manually. Kipper cannot take over firewalld: " + preview.Reason
	}
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

	// The address a gateway registration will name, settled before anything is
	// touched. --host may be a hostname, which is what SSH wants, and the gateway
	// takes addresses only. Resolving here keeps both true and fails a name that
	// has no public address immediately, rather than after preflight with an error
	// naming neither the flag nor the value. A custom domain registers nothing, so
	// it keeps whatever was given.
	// The failure is held rather than raised, because an empty --domain is also
	// the natural re-run of an existing custom-domain cluster, which adopts its
	// identity and never asks the gateway for anything. Failing here would refuse
	// to reinstall a working cluster whose host has no public IPv4 — one behind a
	// load balancer, which --trusted-proxy exists for. It is raised at the claim,
	// which is the first point that genuinely needs an address and still ahead of
	// any change to the server.
	clusterAddress := opts.Host
	var addressErr error
	if claimsGatewayName(opts.Domain) {
		resolved, err := GatewayAddressFor(opts.Host)
		if err != nil {
			addressErr = err
		} else {
			clusterAddress = resolved
		}
	}

	provider := &infra.BareMetalProvider{
		Host:           opts.Host,
		SSHKey:         opts.SSHKeyPath,
		FallbackSSHKey: opts.FallbackSSHKeyPath,
	}

	var client hostRunner
	if opts.Local {
		fmt.Printf("\n  Installing on this machine (%s)\n\n", opts.Host)
		client = localClient(opts)
	} else {
		fmt.Printf("\n  Connecting to %s...\n", opts.Host)
		if err := provider.Connect(); err != nil {
			return nil, fmt.Errorf("connecting to %s: %w", opts.Host, err)
		}
		defer func() { _ = provider.Close() }()
		fmt.Printf("  ✔  Connected\n\n")
		client = provider.Client()
	}

	fmt.Printf("  Running preflight checks...\n")
	// Record machine identity for the local-uninstall guard, including hosts
	// reached through NAT. Address checks remain available if this read fails.
	hostMachineID, _ := ReadMachineID(client)

	sysInfo, err := GatherSystemInfo(client)
	if err != nil {
		return nil, fmt.Errorf("gathering system info: %w", err)
	}

	platformProfile := resolveProfile(opts.Profile, sysInfo)
	preflight, err := RunPreflightChecks(sysInfo, platformProfile)
	if err != nil {
		return nil, err
	}
	fmt.Printf("  ✔  OS: %s %s\n", sysInfo.OS, sysInfo.OSVersion)
	fmt.Printf("  ✔  RAM: %dMB available\n", sysInfo.RAMMB)
	fmt.Printf("  ✔  CPUs: %d\n", sysInfo.CPUs)
	fmt.Printf("  ✔  Disk: %dMB available\n", sysInfo.DiskMB)
	fmt.Printf("  ✔  Ports: 80, 443, 6443 not held by another service\n")

	if opts.Profile != "" {
		fmt.Printf("  ✔  Platform profile: %s (--profile)\n", platformProfile)
	} else {
		fmt.Printf("  ✔  Platform profile: %s\n", platformProfile)
		if note := profilePickNote(sysInfo.RAMMB, sysInfo.CPUs, platformProfile); note != "" {
			fmt.Printf("  ℹ  %s\n", note)
		}
	}
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

	// Check firewall ownership before requesting takeover consent.
	fwAudit, err := AuditFirewall(client)
	if err != nil {
		return nil, fmt.Errorf("auditing firewall: %w", err)
	}
	mesh := ServerFirewallMesh(client)
	consent, err := adoptionConsent(client, opts, fwAudit, mesh)
	if err != nil {
		return nil, err
	}
	fwPlan := PlanFirewall(fwAudit, opts.Firewall, consent.Adopt)
	fwPlan.AdoptFingerprint = consent.Fingerprint
	fwPlan.Mesh = mesh
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

	// An interrupted network change can leave k3s stopped, and reading the
	// identity needs the API, so the change is settled first.
	if err := recoverNetworkChange(client, opts); err != nil {
		return nil, err
	}

	// Read the stored identity to preserve any completed cluster reconfiguration.
	existingIdentity, k3sPreexisting, err := ReadExistingClusterIdentity(client)
	if err != nil {
		return nil, err
	}

	// Obtain network consent before domain registration and installation steps.
	changeNetwork, err := networkChangeConsent(client, k3sPreexisting, opts)
	if err != nil {
		return nil, err
	}
	if k3sPreexisting {
		records, err := refuseUnrecordedNodes(client, hostMachineID)
		if err != nil {
			return nil, err
		}
		// Recheck previewed peers against the validated records before applying rules.
		if final := FirewallMeshFor(records, hostMachineID); !final.equal(fwPlan.Mesh) {
			if fwPlan.Adopt {
				return nil, fmt.Errorf("the cluster's nodes changed while preparing the firewall; run the command again to review the changes")
			}
			fwPlan.Mesh = final
		}
	} else {
		fwPlan.Mesh = FirewallMesh{}
	}
	if err := requireOperatorFirewalld(client, sysInfo.Host, fwAudit, fwPlan); err != nil {
		return nil, err
	}

	// Register a kipper.run subdomain or use a custom domain
	var gatewayToken string
	// freshlyRegistered marks a label this run claimed, whose token the gateway
	// disclosed exactly once and which nothing else holds yet.
	freshlyRegistered := false
	credentialsAttempted := false
	passwordSent := false
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
		subdomain, wantsGatewayName, labelErr := KipperRunLabelFor(opts.Domain, clusterAddress)
		if labelErr != nil {
			return nil, labelErr
		}
		if wantsGatewayName {
			if addressErr != nil {
				return nil, addressErr
			}
			fmt.Printf("  Registering subdomain...\n")
			claimed, claimErr := claimGatewayName(
				domain.NewGatewayClient(opts.GatewayURL),
				localTokenStore{},
				subdomain, clusterAddress, opts.Host)
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
		if existing := cfg.GetClusterByHost(opts.Host, clusterAddress); existing != nil {
			clusterEntryName = existing.Name
			fmt.Print(reinstallNameHint(existing, domainName))
			// A reinstall registers nothing with the gateway, so inherit the
			// token mirror rather than clearing it.
			if gatewayToken == "" {
				gatewayToken = existing.GatewayToken
			}
			inheritClusterSettings(&opts, existing)
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

	// DNS preflight runs after identity adoption so it looks up the hosts
	// the install settled on, not the flags as typed. A re-run with a
	// matching --domain and no host flags inherits the cluster's existing
	// hosts; a re-run with no --domain at all still checks a custom-domain
	// cluster. Warnings only: the install still proceeds.
	for _, w := range CheckCustomDomainDNS(domainName, resolvedHosts.ConsoleHost, resolvedHosts.ConsoleAPIHost, resolvedHosts.DexHost, nil) {
		fmt.Printf("  ⚠  %s\n", w)
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
	var steps []installStep
	// Configure needrestart before package installation to protect active
	// Longhorn sessions from storage-service restarts.
	steps = append(steps, storageRestartSteps(sysInfo.Host, client)...)
	if opts.Harden && len(findings) > 0 {
		steps = append(steps, installStep{"Hardening host OS", func() error {
			return HardenHost(client)
		}})
	}
	k3sStep := installStep{"Installing k3s", func() error {
		return InstallK3s(client, opts.Host, opts.DNSResolvers, k3sPreexisting, changeNetwork)
	}}
	var firewallStep *installStep
	if configureFirewall {
		firewallStep = &installStep{"Configuring firewall", func() error {
			return ApplyFirewallPlan(client, fwPlan, !opts.NoSSHRateLimit)
		}}
	}
	steps = append(steps, k3sAndFirewallSteps(sysInfo.Host, k3sStep, firewallStep)...)
	// effectivePlatform is the cluster's actual sizing state after
	// InstallPlatformConfig runs (which preserves an existing CR's
	// overrides on reinstall). The observability install steps below
	// honor this, so a user who previously disabled Loki via
	// `kip platform disable loki` doesn't get it resurrected just
	// because re-running `kip install` re-detected a non-nano profile
	// from the host.
	var effectivePlatform PlatformState
	var nodeRecords []NodeRecord
	var clusterRegistered bool

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
			for _, note := range keptProfileNotes(platformProfile, state.Profile, sysInfo.CPUs) {
				fmt.Printf("       ⚠  %s\n", note)
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
			if err := InstallLonghornThen(client, hardenAfterStoragePackages(opts.Harden, client)); err != nil {
				return err
			}
			if !sysInfo.Host.UsesNeedrestart() {
				return nil
			}
			// Record the host configuration now that the node exists.
			machineID, err := ReadMachineID(client)
			if err != nil {
				fmt.Printf("       (could not record host configuration: %v)\n", err)
				return nil
			}
			nodeName, err := NodeNameForMachine(client, machineID)
			if err != nil {
				fmt.Printf("       (could not record host configuration: %v)\n", err)
				return nil
			}
			if err := StampStorageRestarts(client, nodeName, machineID); err != nil {
				fmt.Printf("       (could not record host configuration: %v)\n", err)
			}
			return nil
		}},
		{"Installing KEDA autoscaler", func() error {
			return InstallKEDA(client)
		}},
	}...)

	// Honor the saved PlatformConfig when installing observability components.
	// Always provision the TLS certificate so monitoring can be enabled later.
	steps = append(steps, []installStep{
		{"Installing log aggregation (Loki)", func() error {
			if !effectivePlatform.LokiEnabled() {
				fmt.Printf("       (disabled in PlatformConfig; skipping)\n")
				return nil
			}
			return InstallLokiWithResources(client, effectivePlatform.EffectiveResources())
		}},
		{"Issuing the node metrics certificate", func() error {
			return InstallNodeExporterTLS(client)
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
		{"Recording nodes", func() error {
			records, err := EnsureServerRecord(client, opts.Host)
			if err != nil {
				return err
			}
			nodeRecords = records
			// Refresh the local entry if it was saved before node records were available.
			if clusterRegistered {
				if err := config.MirrorNodes(clusterEntryName, records); err != nil {
					fmt.Printf("       (could not update the local copy of the node records: %v)\n", err)
				}
			}
			return nil
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
			sent, err := InstallDex(client, resolvedHosts.DexHost, resolvedHosts.ConsoleHost, domainName, opts.AdminPasswordHash)
			passwordSent = passwordSent || sent
			return err
		}},
		{"Staging operator access", func() error {
			return InstallOperatorRBAC(client, domainName)
		}},
		{"Enabling operator authentication", func() error {
			return InstallOperatorAuth(client, resolvedHosts.DexHost)
		}},
		{"Deploying console", func() error {
			return DeployConsole(client, resolvedHosts.DexHost, resolvedHosts.ConsoleHost, resolvedHosts.ConsoleAPIHost, domainName, gatewayKipperRunDomain, clusterAddress)
		}},
		{"Recording serving identity", func() error {
			return InstallClusterIdentity(client, domainName, opts.ConsoleDomain, opts.ConsoleAPIDomain, opts.DexDomain, gatewayKipperRunDomain, clusterAddress)
		}},
		{"Deploying API key service", func() error {
			return InstallAuthz(client)
		}},
		{"Isolating image builds", func() error {
			return InstallBuildIsolation(client)
		}},
		{"Pinning platform components to the server", func() error {
			return PinPlatformToServer(client)
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
	if err := ClearHostWipedMarker(opts.Host, clusterAddress); err != nil {
		// The last exit before the step loop, and the only one past the gateway
		// claim, so it owns the tidying the loop's own failure handler would
		// otherwise do: a name this run brought into existence goes back rather
		// than sitting on a registration whose cluster was never built.
		if freshlyRegistered && gatewayToken != "" {
			if relErr := domain.NewGatewayClient(opts.GatewayURL).Deregister(gatewayToken); relErr == nil {
				if clearErr := clearTokenIfHeld(gatewayToken); clearErr != nil {
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
	// The exec kubeconfig needs this local entry to resolve the cluster and Dex
	// host during login. Retry failed registration after the install steps and
	// require success before reporting the install complete.
	registerCluster := func(kubeconfigPath string) error {
		return config.Update(func(cfg *config.Config) error {
			nodes := nodeRecords
			if nodes == nil {
				if existing := cfg.GetCluster(clusterEntryName); existing != nil {
					nodes = existing.Nodes
				}
			}
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
				MachineID:        hostMachineID,
				DNSResolvers:     opts.DNSResolvers,
				TrustedProxies:   opts.TrustedProxies,
				Nodes:            nodes,
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
					if clearErr := clearTokenIfHeld(gatewayToken); clearErr != nil {
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
			partialResult.AdminPassword = passwordInEffect(adminPassword, passwordSent)
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
			// Deliver credentials before opening the sign-in prompt that needs them.
			shown := adminPassword
			if SaveAdminCredentials(os.Stdout, opts.CredentialsFile, "admin@"+domainName, adminPassword) {
				shown = ""
			}
			gate := signInWithCredentials(os.Stdout, domainName, shown, func() GateResult {
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
			if KipPinned() && gate.AuthMode == "oidc" {
				fmt.Printf("  %s\n\n", PinnedKipWarning)
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

// inheritClusterSettings preserves resolvers, trusted proxies, and external
// backup storage when the reinstall supplies no replacement.
func inheritClusterSettings(opts *Options, existing *config.Cluster) {
	if len(opts.DNSResolvers) == 0 {
		opts.DNSResolvers = existing.DNSResolvers
	}
	if len(opts.TrustedProxies) == 0 {
		opts.TrustedProxies = existing.TrustedProxies
	}
	if opts.BackupStorage == nil {
		opts.BackupStorage = BackupStorageFromRef(existing.BackupStorage)
	}
}

// passwordInEffect exposes the generated password after an attempted Dex apply,
// since an apply error can occur after the password has changed.
func passwordInEffect(password string, sent bool) string {
	if !sent {
		return ""
	}
	return password
}

// networkChangeConsent requires prior approval or an interactive confirmation
// when an existing cluster needs a network change.
func networkChangeConsent(client CommandRunner, k3sPreexisting bool, opts Options) (bool, error) {
	if !k3sPreexisting {
		return false, nil
	}
	change, err := PlanNetworkChange(client)
	if err != nil {
		return false, err
	}
	if !change.Needed() {
		return false, nil
	}
	fmt.Printf("\n%s\n", change.Describe())
	if opts.ChangeNetwork {
		return true, nil
	}
	if opts.ConfirmNetworkChange != nil && opts.ConfirmNetworkChange("Move the pod network to WireGuard now?") {
		return true, nil
	}
	return false, ErrNetworkChangeNotConfirmed
}

// recoverNetworkChange requires consent before recovering an interrupted change.
func recoverNetworkChange(client CommandRunner, opts Options) error {
	pending, err := NetworkChangePending(client)
	if err != nil || !pending {
		return err
	}
	fmt.Printf("\n%s\n", PendingNetworkChangeDescription())
	if !opts.ChangeNetwork && (opts.ConfirmNetworkChange == nil || !opts.ConfirmNetworkChange("Recover the unfinished pod network change now?")) {
		return ErrNetworkChangeNotConfirmed
	}
	return RecoverNetworkChange(client)
}

// refuseUnrecordedNodes checks node records and server identity before reinstalling.
// The connected server may be unrecorded because the install will record it.
func refuseUnrecordedNodes(client CommandRunner, serverMachineID string) ([]NodeRecord, error) {
	if serverMachineID == "" {
		return nil, fmt.Errorf("checking the cluster's nodes: the server's machine id could not be read")
	}
	records, err := ReadNodeRecords(client)
	if err != nil {
		return nil, err
	}
	if server, ok := ServerRecord(records); ok && server.MachineID != serverMachineID {
		return nil, fmt.Errorf("this host is machine %s, but the cluster records its server as machine %s (%s); check the address, then run kip install again", serverMachineID, server.MachineID, server.Name)
	}
	unrecorded, err := UnrecordedNodes(client, WithUnrecordedHost(records, serverMachineID))
	if err != nil {
		return nil, err
	}
	if len(unrecorded) > 0 {
		return nil, unrecordedNodesError(unrecorded)
	}
	return records, nil
}

// ServerFirewallMesh reads node records for the firewall preview, returning an
// empty mesh if the machine ID or records cannot be read. Reinstalls validate
// the records before applying firewall rules.
func ServerFirewallMesh(client CommandRunner) FirewallMesh {
	machineID, err := ReadMachineID(client)
	if err != nil {
		return FirewallMesh{}
	}
	records, err := ReadNodeRecords(client)
	if err != nil {
		return FirewallMesh{}
	}
	return FirewallMeshFor(records, machineID)
}
