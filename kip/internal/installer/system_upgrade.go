package installer

import (
	"fmt"

	"github.com/getkipper/kipper/controller/pkg/platform"
	"github.com/getkipper/kipper/kip/internal/ssh"
)

// SystemComponent describes one cluster component that kip upgrade can
// reconcile in place. Re-running Apply against an existing cluster
// re-applies the chart manifest at the version this kip build pins —
// helm-controller (or kubectl apply for non-helm components) handles
// the actual upgrade.
type SystemComponent struct {
	// Name is the short identifier shown to operators in plan output.
	Name string
	// Apply re-runs the component's install function. Functions must
	// be idempotent: safe to call multiple times against the same
	// cluster without rotating secrets or causing data loss.
	Apply func(client *ssh.Client) error
}

// SystemComponents returns the list of cluster components that
// kip upgrade reconciles. Only components whose install functions
// are safely re-runnable are included.
//
// The following are intentionally excluded:
//   - cert-manager: install function needs the admin email, which is
//     not persisted in cluster config. Upgrade-safe variant pending.
//   - Dex: re-rendering its manifest rewrites the ConfigMap, including
//     staticPasswords, which would drop every user added after install.
//     (The older reason recorded here, that the install mints a new OAuth
//     client secret each call, is no longer true: ensureDexClientSecret
//     reuses the existing one. The exclusion stands on the user data.)
//   - Console: depends on Dex's client secret. Upgrade for the console
//     image happens via the existing kubectl-rollout path in kip upgrade.
//   - k3s: control-plane upgrade has its own blast radius. Separate flag.
//
// `domain` is threaded into the Traefik trusted-proxy resolution.
// `dnsResolvers` is the cluster's persisted resolver list, threaded into
// the cert-manager DNS patch so an upgrade keeps it consistent with the
// resolvers chosen at install (empty falls back to the public defaults).
// `state` carries the active profile plus any per-component overrides and
// the user's explicit enable/disable choices. On a nano cluster (or when
// the user has disabled them explicitly), Loki and kube-prometheus-stack
// are omitted entirely so the upgrade does not undo state the CR records.
// `trustedProxies` is the operator's persisted --trusted-proxy list; the
// kipper.run gateway addresses are re-resolved here on every upgrade so a
// gateway IP change cannot pin stale forwarded-header trust.
func SystemComponents(domain string, dnsResolvers, trustedProxies []string, state PlatformState) []SystemComponent {
	components := []SystemComponent{
		{Name: "traefik", Apply: func(c *ssh.Client) error {
			return InstallTraefik(c, ResolveTrustedProxies(domain, trustedProxies))
		}},
		{Name: "cert-manager-dns", Apply: func(c *ssh.Client) error { return patchCertManagerDNSConfig(c, dnsResolvers) }},
		{Name: "security-middleware", Apply: InstallSecurityHardening},
		{Name: "longhorn", Apply: InstallLonghorn},
		{Name: "keda", Apply: InstallKEDA},
		{Name: "authz", Apply: InstallAuthz},
	}
	if state.LokiEnabled() {
		res := state.EffectiveResources()
		components = append(components,
			SystemComponent{Name: "loki", Apply: func(c *ssh.Client) error { return InstallLokiWithResources(c, res) }},
		)
	}
	if state.PrometheusEnabled() {
		res := state.EffectiveResources()
		components = append(components,
			SystemComponent{Name: "kube-prometheus-stack", Apply: func(c *ssh.Client) error { return InstallPrometheusGrafanaWithResources(c, res) }},
		)
	}
	// Velero re-applies the BSL the cluster was installed with. The
	// state's BackupStorage is populated from the cluster's persisted
	// config so we never silently flip an external BSL back to in-
	// cluster mode on upgrade. Credentials are not in PlatformState
	// (they live only as a Secret on the cluster), so an external
	// upgrade preserves the existing cloud-credentials Secret —
	// installVeleroCredentials applies the secret only when fresh
	// credentials are available; the chart references it by name.
	bs := state.BackupStorage
	components = append(components,
		SystemComponent{Name: "velero", Apply: func(c *ssh.Client) error { return InstallBackup(c, bs) }},
		SystemComponent{Name: "zot", Apply: InstallZot},
		// Build isolation must be (re)applied on upgrade: a cluster installed
		// before it existed has no kipper-builds namespace/RBAC/policy, so the
		// new builder code would fail every build. Reapplying also refreshes the
		// egress policy's node-IP except-list for nodes added since install.
		SystemComponent{Name: "build-isolation", Apply: InstallBuildIsolation},
	)
	return components
}

// PlatformState captures the upgrade-relevant slice of PlatformConfig: the
// active profile, per-component memory overrides, and explicit enable/disable
// pointers. It is the contract between the upgrade command (which reads the
// CR) and SystemComponents (which decides what to reapply).
type PlatformState struct {
	Profile         string
	MemoryOverrides map[string]string
	// EnabledOverrides maps component name to an explicit enable/disable
	// flag. Absent entries mean "use profile default" (off for nano, on
	// otherwise).
	EnabledOverrides map[string]bool
	// BackupStorage is the BSL config the cluster was installed with,
	// loaded from ~/.kip/config.yaml at upgrade time. Nil means the
	// in-cluster MinIO default; non-nil makes the upgrade re-apply
	// Velero against an external bucket. Credentials are not carried
	// here — they stay only as a Kubernetes Secret on the cluster.
	BackupStorage *BackupStorageConfig
}

// EffectiveResources returns memory limits that respect both the profile and
// any overrides. Used by the upgrade Apply funcs so a manually-bumped
// Prometheus does not get downsized back to the profile default.
func (s PlatformState) EffectiveResources() platform.Resources {
	return platform.EffectiveResources(s.Profile, s.MemoryOverrides)
}

// PrometheusEnabled / LokiEnabled return whether the component should be
// included in the upgrade run. An explicit override (set via the API or
// `kip platform`) wins over the profile default.
func (s PlatformState) PrometheusEnabled() bool { return s.enabled("prometheus") }
func (s PlatformState) LokiEnabled() bool       { return s.enabled("loki") }

func (s PlatformState) enabled(name string) bool {
	if v, ok := s.EnabledOverrides[name]; ok {
		return v
	}
	return s.Profile != ProfileNano
}

// RunSystemUpgrade re-applies each system component in order. Stops at
// the first failure and returns the wrapped error so the caller can
// report which component broke.
func RunSystemUpgrade(client *ssh.Client, components []SystemComponent) error {
	for _, c := range components {
		fmt.Printf("  ...  %s\n", c.Name)
		if err := c.Apply(client); err != nil {
			return fmt.Errorf("%s: %w", c.Name, err)
		}
		fmt.Printf("  ✔  %s\n", c.Name)
	}
	return nil
}
