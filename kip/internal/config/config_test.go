package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadFromReturnsEmptyConfigWhenFileDoesNotExist(t *testing.T) {
	cfg, err := LoadFrom("/nonexistent/path/config.yaml")
	require.NoError(t, err)
	assert.Empty(t, cfg.Clusters)
	assert.Empty(t, cfg.CurrentCluster)
}

func TestSaveAndLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	original := &Config{
		CurrentCluster: "staging",
		Clusters: []Cluster{
			{
				Name:       "staging",
				Provider:   "baremetal",
				Host:       "198.51.100.1",
				Domain:     "staging.kipper.run",
				Kubeconfig: "~/.kip/clusters/staging.yaml",
			},
			{
				Name:       "production",
				Provider:   "baremetal",
				Host:       "198.51.100.2",
				Domain:     "prod.example.com",
				Kubeconfig: "~/.kip/clusters/production.yaml",
			},
		},
	}

	err := SaveTo(original, path)
	require.NoError(t, err)

	loaded, err := LoadFrom(path)
	require.NoError(t, err)

	assert.Equal(t, original.CurrentCluster, loaded.CurrentCluster)
	assert.Len(t, loaded.Clusters, 2)
	assert.Equal(t, "staging", loaded.Clusters[0].Name)
	assert.Equal(t, "198.51.100.1", loaded.Clusters[0].Host)
	assert.Equal(t, "production", loaded.Clusters[1].Name)
}

func TestSaveCreatesDirectoryIfMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "deep", "config.yaml")

	// SaveTo does not create parent dirs — that is Save's job.
	// But we can verify the file is written if the parent exists.
	err := os.MkdirAll(filepath.Dir(path), 0o700)
	require.NoError(t, err)

	cfg := &Config{CurrentCluster: "test"}
	err = SaveTo(cfg, path)
	require.NoError(t, err)

	_, err = os.Stat(path)
	assert.NoError(t, err)
}

func TestGetClusterReturnsMatchingCluster(t *testing.T) {
	cfg := &Config{
		Clusters: []Cluster{
			{Name: "alpha", Host: "10.0.0.1"},
			{Name: "beta", Host: "10.0.0.2"},
		},
	}

	cluster := cfg.GetCluster("beta")
	require.NotNil(t, cluster)
	assert.Equal(t, "10.0.0.2", cluster.Host)
}

func TestGetClusterReturnsNilWhenNotFound(t *testing.T) {
	cfg := &Config{
		Clusters: []Cluster{
			{Name: "alpha"},
		},
	}

	assert.Nil(t, cfg.GetCluster("nonexistent"))
}

func TestAddClusterAppendsNewCluster(t *testing.T) {
	cfg := &Config{}
	cfg.AddCluster(Cluster{Name: "alpha", Host: "10.0.0.1"})
	cfg.AddCluster(Cluster{Name: "beta", Host: "10.0.0.2"})

	assert.Len(t, cfg.Clusters, 2)
	assert.Equal(t, "alpha", cfg.Clusters[0].Name)
	assert.Equal(t, "beta", cfg.Clusters[1].Name)
}

func TestAddClusterReplacesExistingByName(t *testing.T) {
	cfg := &Config{
		Clusters: []Cluster{
			{Name: "alpha", Host: "10.0.0.1"},
		},
	}

	cfg.AddCluster(Cluster{Name: "alpha", Host: "10.0.0.99"})

	assert.Len(t, cfg.Clusters, 1)
	assert.Equal(t, "10.0.0.99", cfg.Clusters[0].Host)
}

func TestClusterHostsDefaultToConvention(t *testing.T) {
	tests := []struct {
		name           string
		domain         string
		wantConsole    string
		wantConsoleAPI string
		wantDex        string
	}{
		{
			name:           "kipper.run uses hyphen separator",
			domain:         "acme-prod.kipper.run",
			wantConsole:    "console--acme-prod.kipper.run",
			wantConsoleAPI: "console-api--acme-prod.kipper.run",
			wantDex:        "dex--acme-prod.kipper.run",
		},
		{
			name:           "custom domain uses dot separator",
			domain:         "example.com",
			wantConsole:    "console.example.com",
			wantConsoleAPI: "console-api.example.com",
			wantDex:        "dex.example.com",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Cluster{Domain: tt.domain}
			assert.Equal(t, tt.wantConsole, c.ConsoleHost())
			assert.Equal(t, tt.wantConsoleAPI, c.ConsoleAPIHost())
			assert.Equal(t, tt.wantDex, c.DexHost())
		})
	}
}

func TestClusterHostOverridesAreUsedVerbatim(t *testing.T) {
	// An override field wins over the default derived from Domain, even
	// when the override does not look like a subdomain of Domain. This is
	// the whole point: the admin's chosen hostname is the truth.
	c := Cluster{
		Domain:           "acme-prod.kipper.run",
		ConsoleDomain:    "console.example.com",
		ConsoleAPIDomain: "api.example.com",
		DexDomain:        "auth.example.com",
	}
	assert.Equal(t, "console.example.com", c.ConsoleHost())
	assert.Equal(t, "api.example.com", c.ConsoleAPIHost())
	assert.Equal(t, "auth.example.com", c.DexHost())
}

func TestClusterHostsMixOverridesAndDefaults(t *testing.T) {
	// Only one component moved; the others stay on convention.
	c := Cluster{
		Domain:        "acme-prod.kipper.run",
		ConsoleDomain: "console.example.com",
	}
	assert.Equal(t, "console.example.com", c.ConsoleHost())
	assert.Equal(t, "console-api--acme-prod.kipper.run", c.ConsoleAPIHost())
	assert.Equal(t, "dex--acme-prod.kipper.run", c.DexHost())
}

func TestAIConfigRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	original := &Config{
		AI: AIConfig{ //nolint:gosec // test data, not real credentials
			Provider:  "claude",
			APIKey:    "sk-ant-test-key",
			Model:     "claude-sonnet-4-5",
			OllamaURL: "",
			Features: AIFeatures{
				LogAnalysis:          true,
				AnomalyDetection:     true,
				DockerfileGeneration: false,
			},
		},
	}

	err := SaveTo(original, path)
	require.NoError(t, err)

	loaded, err := LoadFrom(path)
	require.NoError(t, err)

	assert.Equal(t, "claude", loaded.AI.Provider)
	assert.Equal(t, "sk-ant-test-key", loaded.AI.APIKey)
	assert.Equal(t, "claude-sonnet-4-5", loaded.AI.Model)
	assert.True(t, loaded.AI.Features.LogAnalysis)
	assert.True(t, loaded.AI.Features.AnomalyDetection)
	assert.False(t, loaded.AI.Features.DockerfileGeneration)
}

func TestAIEnabledReturnsFalseByDefault(t *testing.T) {
	cfg := &Config{}
	assert.False(t, cfg.AI.AIEnabled())
}

func TestAIEnabledReturnsFalseForNoneProvider(t *testing.T) {
	cfg := &Config{AI: AIConfig{Provider: "none"}}
	assert.False(t, cfg.AI.AIEnabled())
}

func TestAIEnabledReturnsTrueWhenConfigured(t *testing.T) {
	cfg := &Config{AI: AIConfig{Provider: "claude"}}
	assert.True(t, cfg.AI.AIEnabled())

	cfg.AI.Provider = "ollama"
	assert.True(t, cfg.AI.AIEnabled())
}

func TestResolveNamespaceProjectOnly(t *testing.T) {
	c := &Cluster{}
	assert.Equal(t, "myproject", c.ResolveNamespace("myproject", ""))
}

func TestResolveNamespaceWithEnvironment(t *testing.T) {
	c := &Cluster{}
	assert.Equal(t, "myproject-test", c.ResolveNamespace("myproject", "test"))
}

func TestResolveNamespaceDefaultEnvironmentIgnored(t *testing.T) {
	c := &Cluster{}
	assert.Equal(t, "myproject", c.ResolveNamespace("myproject", "default"))
}

func TestResolveNamespaceWithOrg(t *testing.T) {
	c := &Cluster{Org: "acme"}
	assert.Equal(t, "acme-myproject", c.ResolveNamespace("myproject", ""))
}

func TestResolveNamespaceWithOrgAndEnvironment(t *testing.T) {
	c := &Cluster{Org: "acme"}
	assert.Equal(t, "acme-myproject-prod", c.ResolveNamespace("myproject", "prod"))
}

func TestResolveNamespaceProjectAlreadyStartsWithOrg(t *testing.T) {
	// Regression for the acme-tools migration: a project named
	// `acme-deck` on a cluster with `org: acme` should land in
	// `acme-deck`, not `acme-acme-deck`. The pre-fix behavior
	// returned `acme-acme-deck` and every Kubernetes lookup against
	// the resulting namespace came back empty.
	c := &Cluster{Org: "acme"}
	assert.Equal(t, "acme-deck", c.ResolveNamespace("acme-deck", ""))
	assert.Equal(t, "acme-deck-test", c.ResolveNamespace("acme-deck", "test"))
	assert.Equal(t, "acme-tools-prod", c.ResolveNamespace("acme-tools", "prod"))
}

func TestResolveNamespaceOrgPrefixMatchMustBeExact(t *testing.T) {
	// Guard against a different bug: a project name that merely
	// CONTAINS the org as a substring (but does not start with `org+"-"`)
	// must still get the prefix. `myorg` + project `myorg2-deck` is
	// different from `myorg` + project `myorg-deck` — only the latter is
	// already prefixed.
	c := &Cluster{Org: "myorg"}
	assert.Equal(t, "myorg-myorg2-deck", c.ResolveNamespace("myorg2-deck", ""))
	assert.Equal(t, "myorg-myorg", c.ResolveNamespace("myorg", ""))
}

func TestLoadFromMalformedFileReturnsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	err := os.WriteFile(path, []byte("{{{{not yaml"), 0o600)
	require.NoError(t, err)

	_, err = LoadFrom(path)
	assert.Error(t, err)
}

func TestSaveToRestrictsFilePermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	require.NoError(t, SaveTo(&Config{CurrentCluster: "staging"}, path))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestSaveToTightensExistingLoosePermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	//nolint:gosec // the loose mode is the fixture: SaveTo must tighten it
	require.NoError(t, os.WriteFile(path, []byte("clusters: []\n"), 0o644))

	require.NoError(t, SaveTo(&Config{CurrentCluster: "staging"}, path))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestGetClusterByHost(t *testing.T) {
	cfg := &Config{Clusters: []Cluster{
		{Name: "production", Host: "198.51.100.1", Domain: "kipper.example.com"},
		{Name: "dev", Host: "203.0.113.10", Domain: "203-0-113-10.kipper.run"},
	}}

	got := cfg.GetClusterByHost("198.51.100.1")
	require.NotNil(t, got)
	// The name is the stable alias; the domain has moved off the default.
	assert.Equal(t, "production", got.Name)

	assert.Nil(t, cfg.GetClusterByHost("192.0.2.99"))
}

// One server has two spellings once --host accepts a hostname: the name and the
// address it resolves to. A lookup that matches only the literal string treats
// them as different servers, so a reinstall spelled the other way misses the
// existing entry and loses its gateway token, DNS resolvers and trusted proxies.
func TestGetClusterByHostMatchesEitherSpelling(t *testing.T) {
	cfg := &Config{Clusters: []Cluster{
		{Name: "lab.kipper.run", Host: "157.180.46.126", GatewayToken: "tok"},
		{Name: "other", Host: "198.51.100.4"},
	}}

	if got := cfg.GetClusterByHost("157.180.46.126", "box.kipper.sh"); got == nil || got.Name != "lab.kipper.run" {
		t.Errorf("the recorded spelling must match, got %v", got)
	}
	if got := cfg.GetClusterByHost("box.kipper.sh", "157.180.46.126"); got == nil || got.Name != "lab.kipper.run" {
		t.Errorf("the resolved address must match an entry recorded under it, got %v", got)
	}
	if got := cfg.GetClusterByHost("nothing.example.com", "203.0.113.9"); got != nil {
		t.Errorf("an unrelated server must not match, got %v", got)
	}
	// An empty alias must not match an entry that happens to have an empty host.
	cfg.Clusters = append(cfg.Clusters, Cluster{Name: "hostless"})
	if got := cfg.GetClusterByHost("box.kipper.sh", ""); got != nil {
		t.Errorf("an empty alias must match nothing, got %v", got)
	}
}
