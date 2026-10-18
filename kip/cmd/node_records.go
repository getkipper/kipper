package cmd

import (
	"fmt"
	"io"
	"os"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/infra"
	"github.com/getkipper/kipper/kip/internal/installer"
)

// checkNodesAndStorage validates and mirrors node records over SSH when the
// cluster has a host address, then pins single-copy volumes to the server.
func checkNodesAndStorage(cluster *config.Cluster, sshKey string) error {
	if cluster.Host == "" {
		return nil
	}
	explicitKey, fallbackKey := resolveSSHKey(sshKey, cluster)
	provider := &infra.BareMetalProvider{Host: cluster.Host, User: sshUser(cluster), SSHKey: explicitKey, FallbackSSHKey: fallbackKey}
	if err := provider.Connect(); err != nil {
		return fmt.Errorf("connecting to %s: %w", cluster.Host, err)
	}
	defer func() { _ = provider.Close() }()
	if _, err := recordNodes(provider.Client(), cluster.Name, cluster.Host); err != nil {
		return err
	}
	pinForUpgrade(os.Stdout, provider.Client())
	return nil
}

// pinForUpgrade attempts server-only scheduling and reports failures as warnings
// so storage problems allow the rest of the upgrade to continue. A local-storage
// cluster has no Longhorn volumes to pin.
func pinForUpgrade(out io.Writer, client installer.CommandRunner) {
	mode, err := installer.ReadStorageMode(client)
	if err != nil {
		say(out, "  ⚠  could not keep single-copy volumes on the server (%v). The upgrade continues. Run 'kip upgrade' again to retry\n", err)
		return
	}
	if mode == installer.StorageLocal {
		return
	}
	placement, err := installer.PinSingleCopiesToServer(client)
	if err != nil {
		say(out, "  ⚠  could not keep single-copy volumes on the server (%v). The upgrade continues. Run 'kip upgrade' again to retry\n", err)
		return
	}
	reportSingleCopyPlacement(out, placement)
}

// recordNodes ensures every node is recorded, creating the server record if
// needed, then copies the records into the local cluster config.
func recordNodes(client installer.CommandRunner, clusterName, host string) ([]installer.NodeRecord, error) {
	records, err := installer.RequireRecordedNodes(client, host)
	if err != nil {
		return nil, err
	}
	mirrorNodes(clusterName, records)
	return records, nil
}

// mirrorNodes treats local copy failures as warnings because the cluster
// keeps the authoritative records.
func mirrorNodes(clusterName string, records []installer.NodeRecord) {
	if err := config.MirrorNodes(clusterName, records); err != nil {
		fmt.Printf("  ⚠  could not update the local copy of the node records: %v\n", err)
	}
}
