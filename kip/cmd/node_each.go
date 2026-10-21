package cmd

import (
	"fmt"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/installer"
	"github.com/getkipper/kipper/kip/internal/ssh"
)

// nodeDialer connects to a recorded node. The returned function closes the
// connection.
type nodeDialer func(rec installer.NodeRecord) (installer.FirewallRunner, func(), error)

// forEachNode runs step through the supplied server connection, then delegates
// to onEachWorker. It requires a server record and stops at the first failure,
// naming the failed node in the error.
func forEachNode(records []installer.NodeRecord, server installer.FirewallRunner, dial nodeDialer, step func(installer.NodeRecord, installer.FirewallRunner) error) error {
	rec, ok := installer.ServerRecord(records)
	if !ok {
		return fmt.Errorf("the node records have no server")
	}
	if err := step(rec, server); err != nil {
		return fmt.Errorf("server %s (%s): %w", rec.Name, rec.Host, err)
	}
	return onEachWorker(records, dial, step)
}

// onEachWorker runs step on every recorded worker in record order, one at a
// time. It checks each worker's machine ID before step and closes its connection
// afterwards. It stops at the first connection, identity or step failure and
// names that worker in the error. The step must be safe to repeat because
// each call starts from the first worker.
func onEachWorker(records []installer.NodeRecord, dial nodeDialer, step func(installer.NodeRecord, installer.FirewallRunner) error) error {
	for _, rec := range records {
		if rec.Role != installer.NodeRoleWorker {
			continue
		}
		if err := onRecordedWorker(rec, dial, step); err != nil {
			return err
		}
	}
	return nil
}

func onRecordedWorker(rec installer.NodeRecord, dial nodeDialer, step func(installer.NodeRecord, installer.FirewallRunner) error) error {
	client, done, err := dial(rec)
	if err != nil {
		return fmt.Errorf("connecting to node %s (%s): %w. kip stopped before changing it. Run the command again once the host is reachable", rec.Name, rec.Host, err)
	}
	defer done()
	id, err := installer.ReadMachineID(client)
	if err != nil {
		return fmt.Errorf("node %s (%s): %w", rec.Name, rec.Host, err)
	}
	if id != rec.MachineID {
		return fmt.Errorf("%s no longer answers as node %s (machine ID %q, recorded %s). Check the address, then run the command again", rec.Host, rec.Name, id, rec.MachineID)
	}
	if err := step(rec, client); err != nil {
		return fmt.Errorf("node %s (%s): %w. Earlier nodes completed successfully. To finish the remaining nodes, run the command again, which repeats the completed steps", rec.Name, rec.Host, err)
	}
	return nil
}

// nodesSettled refuses per-node maintenance while a node is joining or being
// removed, because its firewall rules and records are still changing.
func nodesSettled(records []installer.NodeRecord) error {
	if other, busy := installer.RemovalInProgress(records, ""); busy {
		return fmt.Errorf("%s is being removed. Finish the removal with 'kip node remove %s', or keep it with 'kip node add %s'", other.Name, other.Name, other.Host)
	}
	if other, busy := installer.JoinInProgress(records, ""); busy {
		return fmt.Errorf("%s has not finished joining. Finish the join with 'kip node add %s', or remove the node with 'kip node remove %s'", other.Name, other.Host, other.Name)
	}
	return nil
}

// leasedHost runs commands through the node lifecycle lease and checks new
// SSH logins on the host directly, as key-only SSH setup requires.
type leasedHost struct {
	installer.FirewallRunner
	direct *ssh.Client
}

func (h leasedHost) ConfirmFreshKeyOnlyConnection() error {
	return h.direct.ConfirmFreshKeyOnlyConnection()
}

func (h leasedHost) ConfirmPasswordRefused() error { return h.direct.ConfirmPasswordRefused() }

// leasedCluster is a connection to the cluster's server that holds the node
// lifecycle lease, so no kip node add or remove changes hosts meanwhile.
type leasedCluster struct {
	conn   *ssh.Client
	lease  *installer.NodeLease
	server leasedHost
}

// leaseCluster connects to the cluster's server and takes the node lifecycle
// lease. Competing runs see purpose in the refusal message. Call close when finished.
func leaseCluster(cluster *config.Cluster, sshKey, purpose string) (*leasedCluster, error) {
	conn, err := dialNodeSSH(cluster.Host, sshKey, cluster)
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", cluster.Host, err)
	}
	lease, err := installer.TakeNodeLease(conn, purpose)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return &leasedCluster{conn: conn, lease: lease, server: leasedHost{FirewallRunner: lease.Server(conn), direct: conn}}, nil
}

func (c *leasedCluster) close() {
	c.lease.Release()
	_ = c.conn.Close()
}

// dialer connects to workers with the cluster's SSH settings. Their commands
// run under the lease.
func (c *leasedCluster) dialer(cluster *config.Cluster, sshKey string) nodeDialer {
	return func(rec installer.NodeRecord) (installer.FirewallRunner, func(), error) {
		conn, err := dialNodeSSH(rec.Host, sshKey, cluster)
		if err != nil {
			return nil, nil, err
		}
		return leasedHost{FirewallRunner: c.lease.Host(conn), direct: conn}, func() { _ = conn.Close() }, nil
	}
}
