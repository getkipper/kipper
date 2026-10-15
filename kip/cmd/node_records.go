package cmd

import (
	"fmt"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/infra"
	"github.com/getkipper/kipper/kip/internal/installer"
)

// checkNodeRecords validates and mirrors node records over SSH when the cluster
// has a host address.
func checkNodeRecords(cluster *config.Cluster, sshKey string) error {
	if cluster.Host == "" {
		return nil
	}
	explicitKey, fallbackKey := resolveSSHKey(sshKey, cluster)
	provider := &infra.BareMetalProvider{Host: cluster.Host, SSHKey: explicitKey, FallbackSSHKey: fallbackKey}
	if err := provider.Connect(); err != nil {
		return fmt.Errorf("connecting to %s: %w", cluster.Host, err)
	}
	defer func() { _ = provider.Close() }()
	return recordNodes(provider.Client(), cluster.Name, cluster.Host)
}

// recordNodes ensures every node is recorded, creating the server record if
// needed, then copies the records into the local cluster config.
func recordNodes(client installer.CommandRunner, clusterName, host string) error {
	records, err := installer.RequireRecordedNodes(client, host)
	if err != nil {
		return err
	}
	mirrorNodes(clusterName, records)
	return nil
}

// mirrorNodes treats local copy failures as warnings because the cluster
// keeps the authoritative records.
func mirrorNodes(clusterName string, records []installer.NodeRecord) {
	if err := config.MirrorNodes(clusterName, records); err != nil {
		fmt.Printf("  ⚠  could not update the local copy of the node records: %v\n", err)
	}
}
