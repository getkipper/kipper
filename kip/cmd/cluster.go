package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/k8s"
)

// loadCurrentCluster reads the kip config and returns a k8s client
// for the currently selected cluster. Resolution order:
//  1. --cluster flag (highest priority)
//  2. KIP_CLUSTER env var
//  3. current_cluster in config (lowest priority)
func loadCurrentCluster() (*config.Cluster, *k8s.Client, error) {
	cluster, err := loadCurrentClusterConfig()
	if err != nil {
		return nil, nil, err
	}

	client, err := k8s.NewFromCluster(cluster)
	if err != nil {
		return nil, nil, err
	}

	return cluster, client, nil
}

// loadCurrentClusterConfig returns the cluster config without building a
// k8s client. Used by host-level commands (e.g. `cluster harden`) that
// reach the server over SSH and do not need kubeconfig.
func loadCurrentClusterConfig() (*config.Cluster, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}

	clusterName := resolveClusterName(cfg)
	if clusterName == "" {
		return nil, fmt.Errorf("no cluster configured. Run 'kip install' first")
	}

	cluster := findCluster(cfg, clusterName)
	if cluster == nil {
		fmt.Fprintf(os.Stderr, "\n  Cluster %q not found. Available clusters:\n\n", clusterName)
		for _, c := range cfg.Clusters {
			fmt.Fprintf(os.Stderr, "    %s\n", c.Name)
		}
		fmt.Fprintln(os.Stderr)
		return nil, fmt.Errorf("cluster %q not found", clusterName)
	}

	return cluster, nil
}

// resolveClusterName determines which cluster to use from flag, env, or config.
func resolveClusterName(cfg *config.Config) string {
	// 1. --cluster flag
	if flag := rootCmd.PersistentFlags().Lookup("cluster"); flag != nil && flag.Changed {
		return flag.Value.String()
	}

	// 2. KIP_CLUSTER env var
	if env := os.Getenv("KIP_CLUSTER"); env != "" {
		return env
	}

	// 3. config default
	return cfg.CurrentCluster
}

// resolveSSHKey returns the explicit SSH key to pass to ssh -i (or
// empty when no caller-controlled key is set) plus the fallback hint
// for the default key file. Lookup precedence for the explicit key:
//
//  1. --ssh-key flag (caller passes flagValue from cmd.Flags)
//  2. KIP_SSH_KEY env var
//  3. cluster.SSHKey from ~/.kip/config.yaml
//
// When all three are empty, the explicit key is "" and ssh is allowed
// to consult ssh-agent / ~/.ssh/config / default identity files. The
// fallback hint is ~/.ssh/id_ed25519 (Kipper's historical default);
// it is passed only as a soft -i without IdentitiesOnly so it cannot
// lock out users whose key lives elsewhere.
func resolveSSHKey(flagValue string, cluster *config.Cluster) (explicit, fallback string) {
	switch {
	case flagValue != "":
		explicit = flagValue
	case os.Getenv("KIP_SSH_KEY") != "":
		explicit = os.Getenv("KIP_SSH_KEY")
	case cluster != nil && cluster.SSHKey != "":
		explicit = cluster.SSHKey
	}
	if home, err := os.UserHomeDir(); err == nil {
		fallback = filepath.Join(home, ".ssh", "id_ed25519")
	}
	return explicit, fallback
}

// findCluster looks up a cluster by exact name or partial match.
func findCluster(cfg *config.Config, name string) *config.Cluster {
	// Exact match first
	if c := cfg.GetCluster(name); c != nil {
		return c
	}

	// Partial match — allows short names like "example" to match
	// "example.kipper.run"
	var match *config.Cluster
	matches := 0
	for i := range cfg.Clusters {
		if strings.Contains(cfg.Clusters[i].Name, name) {
			match = &cfg.Clusters[i]
			matches++
		}
	}
	if matches == 1 {
		return match
	}

	return nil
}
