package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/viper"

	"github.com/getkipper/kipper/controller/pkg/hostnames"
)

// Cluster holds the configuration for a single Kipper cluster.
//
// CurrentProject and CurrentEnvironment carry the persistent per-cluster
// project context, so that `kip <verb>` commands do not require the user
// to repeat --project/--environment on every invocation. They default to
// empty (meaning "no preference"); commands fall back to the standard flag
// default ("default" project, no environment) when both the flag and the
// persisted context are empty.
//
// GatewayToken mirrors the kipper.run gateway credential whose source of
// truth is the gateway-credentials Secret on the cluster. The local copy
// is the disaster-recovery fallback: it lets the operator deregister the
// public route when the cluster itself is gone. That is why the config
// file is written owner-only (see SaveTo).
//
// HostWiped records that `kip cluster uninstall` destroyed the host but the
// gateway refused to take its name back. The entry survives so the token
// does, and this flag is what lets the retry skip the host: a wiped server
// is often decommissioned before anyone re-runs the command, and connecting
// to it first would make the name unreleasable.
type Cluster struct {
	Name               string            `mapstructure:"name"                yaml:"name"`
	Provider           string            `mapstructure:"provider"            yaml:"provider"`
	Host               string            `mapstructure:"host"                yaml:"host"`
	Domain             string            `mapstructure:"domain"              yaml:"domain"`
	ConsoleDomain      string            `mapstructure:"console_domain"      yaml:"console_domain,omitempty"`
	ConsoleAPIDomain   string            `mapstructure:"console_api_domain"  yaml:"console_api_domain,omitempty"`
	DexDomain          string            `mapstructure:"dex_domain"          yaml:"dex_domain,omitempty"`
	Kubeconfig         string            `mapstructure:"kubeconfig"          yaml:"kubeconfig"`
	SSHKey             string            `mapstructure:"ssh_key"             yaml:"ssh_key,omitempty"`
	GatewayToken       string            `mapstructure:"gateway_token"       yaml:"gateway_token,omitempty"`
	HostWiped          bool              `mapstructure:"host_wiped"          yaml:"host_wiped,omitempty"`
	Org                string            `mapstructure:"org"                 yaml:"org,omitempty"`
	OrgDisplayName     string            `mapstructure:"org_display_name"    yaml:"org_display_name,omitempty"`
	CurrentProject     string            `mapstructure:"current_project"     yaml:"current_project,omitempty"`
	CurrentEnvironment string            `mapstructure:"current_environment" yaml:"current_environment,omitempty"`
	BackupStorage      *BackupStorageRef `mapstructure:"backup_storage"      yaml:"backup_storage,omitempty"`
	DNSResolvers       []string          `mapstructure:"dns_resolvers"       yaml:"dns_resolvers,omitempty"`
	TrustedProxies     []string          `mapstructure:"trusted_proxies"     yaml:"trusted_proxies,omitempty"`
}

// BackupStorageRef records the BSL mode and metadata for a cluster, so
// kip commands (especially `kip upgrade`) know whether to reconcile
// Velero with in-cluster MinIO or an external bucket. Credentials are
// NEVER stored here — they live only as a Kubernetes Secret in the
// `velero` namespace on the cluster. The fields are persisted so that
// upgrade can re-render the Velero HelmChart with the same BSL
// configuration the install used.
//
// Mode is the discriminator:
//   - "in-cluster" (or empty) — Velero points at the bundled MinIO.
//     Bucket/Region/Endpoint are ignored.
//   - "external" — Velero points at the named external bucket.
//     Bucket and Region are required; Endpoint is empty for native AWS
//     S3 and set for everything else.
type BackupStorageRef struct {
	Mode     string `mapstructure:"mode"     yaml:"mode"`
	Bucket   string `mapstructure:"bucket"   yaml:"bucket,omitempty"`
	Region   string `mapstructure:"region"   yaml:"region,omitempty"`
	Endpoint string `mapstructure:"endpoint" yaml:"endpoint,omitempty"`
}

// ConsoleHost returns the custom console domain if set, otherwise the
// conventional hostname for the cluster domain.
func (c *Cluster) ConsoleHost() string {
	return hostnames.HostFor("console", c.ConsoleDomain, c.Domain)
}

// ConsoleAPIHost returns the custom console-api domain if set, otherwise the
// conventional hostname for the cluster domain.
func (c *Cluster) ConsoleAPIHost() string {
	return hostnames.HostFor("console-api", c.ConsoleAPIDomain, c.Domain)
}

// DexHost returns the custom dex domain if set, otherwise the conventional
// hostname for the cluster domain.
func (c *Cluster) DexHost() string {
	return hostnames.HostFor("dex", c.DexDomain, c.Domain)
}

// ResolveNamespace builds the full Kubernetes namespace from org, project, and environment.
//
//	org=acme, project=blog, env=test  → acme-blog-test
//	org="",    project=blog, env=test  → blog-test
//	org="",    project=blog, env=""    → blog
//	org=acme, project=acme-deck, env=""    → acme-deck (no double prefix)
//
// When a project name already starts with the org prefix (e.g.
// `acme-deck` on a cluster with `org: acme`), the org is NOT prepended
// again. Without this guard, `kip export`, `kip backup`, `kip env`,
// `kip service`, `kip tunnel`, `kip webhook`, and `kip promote` would
// all look in `acme-acme-deck` and return empty results. Surfaced
// during the acme-tools migration on 2026-05-16.
func (c *Cluster) ResolveNamespace(project, environment string) string {
	ns := project
	if c.Org != "" && !strings.HasPrefix(project, c.Org+"-") {
		ns = c.Org + "-" + ns
	}
	if environment != "" && environment != "default" {
		ns = ns + "-" + environment
	}
	return ns
}

// AIFeatures controls which AI capabilities are enabled.
type AIFeatures struct {
	LogAnalysis          bool `mapstructure:"log_analysis"          yaml:"log_analysis"`
	AnomalyDetection     bool `mapstructure:"anomaly_detection"     yaml:"anomaly_detection"`
	DockerfileGeneration bool `mapstructure:"dockerfile_generation" yaml:"dockerfile_generation"`
}

// AIConfig holds the AI provider configuration. All AI features are
// optional — when Provider is empty or "none", AI is fully disabled
// and no external calls are made.
type AIConfig struct {
	Provider  string     `mapstructure:"provider"   yaml:"provider"`
	APIKey    string     `mapstructure:"api_key"    yaml:"api_key"`
	Model     string     `mapstructure:"model"      yaml:"model"`
	OllamaURL string     `mapstructure:"ollama_url" yaml:"ollama_url"`
	Features  AIFeatures `mapstructure:"features"   yaml:"features"`
}

// AIEnabled returns true if an AI provider other than "none" is configured.
func (a *AIConfig) AIEnabled() bool {
	return a.Provider != "" && a.Provider != "none"
}

// Config is the top-level structure stored in ~/.kip/config.yaml.
type Config struct {
	Clusters       []Cluster `mapstructure:"clusters"        yaml:"clusters"`
	CurrentCluster string    `mapstructure:"current_cluster" yaml:"current_cluster"`
	AI             AIConfig  `mapstructure:"ai"              yaml:"ai"`
}

// Dir returns the kip config directory (~/.kip).
func Dir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home directory: %w", err)
	}
	return filepath.Join(home, ".kip"), nil
}

// Path returns the full path to the config file (~/.kip/config.yaml).
func Path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.yaml"), nil
}

// Load reads the config file from disk. If the file does not exist,
// it returns an empty Config (not an error).
func Load() (*Config, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}

	v := viper.New()
	v.SetConfigName("config")
	v.SetConfigType("yaml")
	v.AddConfigPath(dir)

	if err := v.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); ok {
			return &Config{}, nil
		}
		return nil, fmt.Errorf("reading config: %w", err)
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	return &cfg, nil
}

// LoadFrom reads a config file from a specific path. If the file does
// not exist, it returns an empty Config (not an error).
func LoadFrom(path string) (*Config, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return &Config{}, nil
	}

	v := viper.New()
	v.SetConfigFile(path)

	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("reading config from %s: %w", path, err)
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("parsing config from %s: %w", path, err)
	}

	return &cfg, nil
}

// Save writes the config to disk, creating the directory if needed.
func Save(cfg *Config) error {
	dir, err := Dir()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}

	return SaveTo(cfg, filepath.Join(dir, "config.yaml"))
}

// SaveTo writes the config to a specific file path. The file carries
// credentials (the gateway token mirror, an AI API key), so it is owner
// read/write only: SetConfigPermissions covers creation, and the explicit
// Chmod tightens a file that already exists with a looser mode.
func SaveTo(cfg *Config, path string) error {
	v := viper.New()
	v.Set("clusters", cfg.Clusters)
	v.Set("current_cluster", cfg.CurrentCluster)
	v.Set("ai", cfg.AI)
	v.SetConfigPermissions(0o600)

	// Write beside the target and rename over it. Writing in place truncates
	// first, so anything reading the config during that window sees a partial
	// file — which parses as no clusters at all, and callers that read a missing
	// gateway credential as "there is none" then act on it. A rename is atomic,
	// so a reader gets either the whole old file or the whole new one.
	// The temp file shares the directory because rename cannot cross filesystems.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.yaml")
	if err != nil {
		return fmt.Errorf("creating temp file beside %s: %w", path, err)
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	defer func() { _ = os.Remove(tmpPath) }()

	if err := v.WriteConfigAs(tmpPath); err != nil {
		return fmt.Errorf("writing %s: %w", tmpPath, err)
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return fmt.Errorf("restricting permissions on %s: %w", tmpPath, err)
	}
	// Flush before the rename. This file holds gateway credentials that exist
	// nowhere else once a cluster is wiped, and a rename can reach the disk
	// ahead of the bytes it points at — which would surface after a power cut
	// as a config that is present, parses, and has lost a token.
	if err := syncFile(tmpPath); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replacing %s: %w", path, err)
	}

	return nil
}

// GetCluster returns the named cluster from the config, or nil if not found.
func (c *Config) GetCluster(name string) *Cluster {
	for i := range c.Clusters {
		if c.Clusters[i].Name == name {
			return &c.Clusters[i]
		}
	}
	return nil
}

// GetClusterByHost returns the first cluster whose Host matches, or nil. The
// host is the stable identity of a server across renames and domain moves,
// which is what a reinstall must key on: the entry's Name can be a short
// alias and its Domain changes with 'kip cluster domain'.
func (c *Config) GetClusterByHost(host string) *Cluster {
	for i := range c.Clusters {
		if c.Clusters[i].Host == host {
			return &c.Clusters[i]
		}
	}
	return nil
}

// GetClusterByDomain returns the first cluster whose Domain matches, or nil.
// The domain is a cluster's credential identity: the auth store is keyed on it
// and the exec kubeconfig is rendered from it, so it is what a credential
// lookup keys on. Unlike the entry's Name it is the same value on every
// machine that adds the cluster, which is what makes an exported kubeconfig
// resolve to the same session for its recipient.
func (c *Config) GetClusterByDomain(domain string) *Cluster {
	for i := range c.Clusters {
		if c.Clusters[i].Domain == domain {
			return &c.Clusters[i]
		}
	}
	return nil
}

// AddCluster appends a cluster to the config. If a cluster with the same
// name already exists, it is replaced.
func (c *Config) AddCluster(cluster Cluster) {
	for i := range c.Clusters {
		if c.Clusters[i].Name == cluster.Name {
			c.Clusters[i] = cluster
			return
		}
	}
	c.Clusters = append(c.Clusters, cluster)
}

// syncFile flushes a file's contents to disk.
//
// Opened read-write, not read-only. Sync is FlushFileBuffers on Windows, which
// needs the write right on the handle and fails with access denied without it —
// which would make every config write on that platform fail before the rename.
func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0) //nolint:gosec // path is the temp file this package just wrote
	if err != nil {
		return fmt.Errorf("opening %s to flush it: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("flushing %s: %w", path, err)
	}
	return nil
}
