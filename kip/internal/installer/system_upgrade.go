package installer

import (
	"fmt"

	"github.com/getkipper/kipper/controller/pkg/platform"
)

// SystemComponent reconciles a component at the version pinned by this kip build.
// Apply must be safe to repeat while preserving secrets and data.
type SystemComponent struct {
	Name  string
	Apply func(client CommandRunner) error
}

// SystemComponents returns components reconciled by kip upgrade, honoring
// profile overrides and explicit enable/disable choices. It reuses saved DNS
// settings and refreshes gateway addresses for trusted-proxy configuration.
// BackupNone omits Velero. Otherwise, BackupStorage selects its destination,
// with nil selecting in-cluster MinIO. The Velero action requires an Uploader.
// Dex's full manifest would overwrite users. A full cert-manager install needs
// the admin email, so only its DNS settings are reconciled here.
// Console and k3s upgrades have separate paths.
func SystemComponents(domain string, dnsResolvers, trustedProxies []string, state PlatformState) []SystemComponent {
	components := []SystemComponent{
		{Name: "traefik", Apply: func(c CommandRunner) error {
			if err := InstallTraefik(c, ResolveTrustedProxies(domain, trustedProxies)); err != nil {
				return err
			}
			return RestartTraefikOntoCopy(c)
		}},
		{Name: "cert-manager-dns", Apply: func(c CommandRunner) error { return patchCertManagerDNSConfig(c, dnsResolvers) }},
		{Name: "service-ca", Apply: InstallServiceCA},
		{Name: "security-middleware", Apply: InstallSecurityHardening},
	}
	// k3s manages the local-path provisioner used for local storage.
	if platform.StorageMode(state.Storage) == StorageLonghorn {
		components = append(components, SystemComponent{Name: "longhorn", Apply: InstallLonghorn})
	}
	components = append(components,
		SystemComponent{Name: "keda", Apply: func(c CommandRunner) error { return InstallKEDA(c, state.Profile) }},
		SystemComponent{Name: "authz", Apply: InstallAuthz},
		SystemComponent{Name: "node-exporter-tls", Apply: InstallNodeExporterTLS},
	)
	if state.LokiEnabled() {
		res := state.EffectiveResources()
		components = append(components,
			SystemComponent{Name: "loki", Apply: func(c CommandRunner) error { return InstallLokiWithResources(c, res, state.Storage) }},
		)
	}
	if state.PrometheusEnabled() {
		res := state.EffectiveResources()
		components = append(components,
			SystemComponent{Name: "kube-prometheus-stack", Apply: func(c CommandRunner) error { return InstallPrometheusGrafanaWithResources(c, res) }},
		)
	}
	// Reuse the saved backup destination and its credential Secret so upgrades
	// preserve external backups. Credentials are replaced only when supplied.
	bs := state.BackupStorage
	storage := platform.StorageMode(state.Storage)
	if platform.BackupsConfigured(state.Backup) {
		components = append(components, SystemComponent{Name: "velero", Apply: func(c CommandRunner) error {
			u, ok := c.(Uploader)
			if !ok {
				return fmt.Errorf("this runner cannot upload the Velero credentials")
			}
			return InstallBackup(u, bs, storage, nil)
		}})
	}
	components = append(components,
		SystemComponent{Name: "zot", Apply: func(c CommandRunner) error { return InstallZot(c, storage) }},
		// Build isolation must be (re)applied on upgrade: a cluster installed
		// before it existed has no kipper-builds namespace/RBAC/policy, so the
		// new builder code would fail every build. Reapplying also refreshes the
		// egress policy's node-IP except-list for nodes added since install.
		SystemComponent{Name: "build-isolation", Apply: InstallBuildIsolation},
		// Apply placement after the components above create their workloads.
		SystemComponent{Name: "placement", Apply: PinPlatformToServer},
	)
	return components
}

// PlatformState holds the settings used during upgrades and reinstalls.
// Empty storage represents legacy Longhorn. Missing enablement overrides use
// the profile default. Empty backup mode enables Velero for legacy clusters.
// When backups are enabled, nil backup storage selects in-cluster MinIO.
type PlatformState struct {
	Profile          string
	Storage          string
	Backup           string
	MemoryOverrides  map[string]string
	EnabledOverrides map[string]bool
	BackupStorage    *BackupStorageConfig
}

// EffectiveResources returns memory limits that respect both the profile and
// any overrides. Used by the upgrade Apply funcs so a manually-bumped
// Prometheus does not get downsized back to the profile default.
func (s PlatformState) EffectiveResources() platform.Resources {
	return platform.EffectiveResources(s.Profile, s.MemoryOverrides)
}

// PrometheusEnabled reports whether upgrades should include Prometheus.
// Explicit overrides take precedence over the profile default.
func (s PlatformState) PrometheusEnabled() bool { return s.enabled("prometheus") }

// LokiEnabled reports whether upgrades should include Loki.
// Explicit overrides take precedence over the profile default.
func (s PlatformState) LokiEnabled() bool { return s.enabled("loki") }

func (s PlatformState) enabled(name string) bool {
	if v, ok := s.EnabledOverrides[name]; ok {
		return v
	}
	return s.Profile != ProfileNano
}

// RunSystemUpgrade re-applies each system component in order. Stops at
// the first failure and returns the wrapped error so the caller can
// report which component broke.
func RunSystemUpgrade(client CommandRunner, components []SystemComponent) error {
	for _, c := range components {
		fmt.Printf("  ...  %s\n", c.Name)
		if err := c.Apply(client); err != nil {
			return fmt.Errorf("%s: %w", c.Name, err)
		}
		fmt.Printf("  ✔  %s\n", c.Name)
	}
	return nil
}
