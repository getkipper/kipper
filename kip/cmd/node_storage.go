package cmd

import (
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/installer"
)

// kubeletPort is where the underlay probe connects on the server: every
// node can reach it, and it answers without a TLS handshake being needed.
const kubeletPort = 10250

var nodeStorageCmd = &cobra.Command{
	Use:   "storage <node> --copies[=false]",
	Short: "Check a node's disk and let it hold copies of volumes",
	Long: `With --copies, checks the node's disk with fio and, when it is fast enough,
lets the node hold copies of services and volumes created with two copies.
The check writes a small file in /var/lib/longhorn twice and takes about a
minute. A disk that cannot keep up with replication is refused: slow disks
can be accepted with --ignore-slow-disk, while a disk whose stalls approach
Longhorn's replica timeout cannot. The figures are recorded with the node.
Running the command again repeats the check.

With --copies=false, the node stops taking new copies. It is refused while
the node still holds copies.

Examples:
  kip node storage worker-1 --copies
  kip node storage worker-1 --copies --ignore-slow-disk
  kip node storage worker-1 --copies=false`,
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE:         runNodeStorage,
}

func init() {
	nodeStorageCmd.Flags().Bool("copies", false, "let the node hold copies (true) or stop it taking new ones (false)")
	nodeStorageCmd.Flags().Bool("ignore-slow-disk", false, "accept a disk the check finds slow. The override is recorded with the node")
	nodeStorageCmd.Flags().String("ssh-key", "", "path to SSH private key")
	nodeCmd.AddCommand(nodeStorageCmd)
}

func runNodeStorage(cmd *cobra.Command, args []string) error {
	name := args[0]
	if !cmd.Flags().Changed("copies") {
		return fmt.Errorf("pass --copies to let %s hold copies, or --copies=false to stop it taking new ones", name)
	}
	copies, _ := cmd.Flags().GetBool("copies")
	ignore, _ := cmd.Flags().GetBool("ignore-slow-disk")
	keyFlag, _ := cmd.Flags().GetString("ssh-key")

	cluster, err := loadCurrentClusterConfig()
	if err != nil {
		return err
	}
	if cluster.Host == "" {
		return fmt.Errorf("no host recorded for cluster %s. Checking a node needs SSH access to the server", cluster.Name)
	}
	leased, err := leaseCluster(cluster, keyFlag, "kip node storage")
	if err != nil {
		return err
	}
	defer leased.close()
	server := leased.server
	records, err := recordNodes(server, cluster)
	if err != nil {
		return err
	}
	if mode, err := installer.ReadStorageMode(server); err != nil {
		return err
	} else if mode == installer.StorageLocal {
		return fmt.Errorf("this cluster uses local storage, which keeps one copy on the node that runs each workload, so no node holds copies")
	}
	rec, found := recordNamed(records, name)
	if !found {
		return fmt.Errorf("no node named %s. 'kip node list' shows the cluster's nodes", name)
	}
	if rec.Role == installer.NodeRoleServer {
		return fmt.Errorf("%s is the server, which always holds copies", name)
	}
	if err := nodesSettled(records); err != nil {
		return err
	}

	save := func(s config.NodeStorage) error {
		rec.Storage = &s
		return saveNodeRecord(server, cluster, rec)
	}
	if !copies {
		if err := disableCopies(os.Stdout, name, func() (int, error) { return installer.ReplicasOnNode(server, name) },
			func(on bool) error { return installer.SetReplicaTag(server, name, on) }); err != nil {
			return err
		}
		if rec.Storage != nil {
			stopped := *rec.Storage
			stopped.Copies = false
			return save(stopped)
		}
		return nil
	}

	serverRec, _ := installer.ServerRecord(records)
	return onRecordedWorker(rec, leased.dialer(cluster, keyFlag), func(_ installer.NodeRecord, node installer.FirewallRunner) error {
		say(os.Stdout, "\n  Checking the disk of %s. This takes about a minute...\n", name)
		if err := installer.EnsureStoragePackages(node, os.Stdout); err != nil {
			return err
		}
		return newNodeCopiesCheck(os.Stdout, rec, server, node, serverRec, ignore, save).enable()
	})
}

func recordNamed(records []installer.NodeRecord, name string) (installer.NodeRecord, bool) {
	for _, r := range records {
		if r.Name == name {
			return r, true
		}
	}
	return installer.NodeRecord{}, false
}

// nodeCopiesCheck runs the disk check on one worker and tags it when it may
// hold copies. Its steps are functions so the decision can be tested apart
// from SSH.
type nodeCopiesCheck struct {
	node           string
	out            io.Writer
	runGate        func() ([]installer.FioSyncRun, error)
	measureRTT     func() *installer.RTTSample
	measureTunnel  func() *installer.RTTSample
	engineTimeout  time.Duration
	ignoreSlowDisk bool
	setTag         func(on bool) error
	isTagged       func() (bool, error)
	save           func(config.NodeStorage) error
	now            func() time.Time
	previous       *config.NodeStorage
}

func newNodeCopiesCheck(out io.Writer, rec installer.NodeRecord, server, node installer.CommandRunner, serverRec installer.NodeRecord, ignore bool, save func(config.NodeStorage) error) *nodeCopiesCheck {
	name := rec.Name
	return &nodeCopiesCheck{
		node:     name,
		previous: rec.Storage,
		out:      out,
		runGate:  func() ([]installer.FioSyncRun, error) { return installer.RunDiskGate(node) },
		measureRTT: func() *installer.RTTSample {
			addr := serverRec.PeerAddress
			if addr == "" {
				addr = serverRec.Host
			}
			if net.ParseIP(addr) == nil {
				return nil
			}
			sample, err := installer.MeasureUnderlayRTT(node, addr, kubeletPort)
			if err != nil {
				return nil
			}
			return &sample
		},
		measureTunnel: func() *installer.RTTSample {
			addr, err := installer.ServerTunnelAddress(server)
			if err != nil {
				return nil
			}
			sample, err := installer.MeasureTunnelRTT(node, addr)
			if err != nil {
				return nil
			}
			return &sample
		},
		engineTimeout:  installer.EngineReplicaTimeout(server),
		ignoreSlowDisk: ignore,
		setTag:         func(on bool) error { return installer.SetReplicaTag(server, name, on) },
		isTagged:       func() (bool, error) { return installer.ReplicaTagged(server, name) },
		save:           save,
		now:            time.Now,
	}
}

// enable runs the check, tags a passing node, then saves the measurements.
// A failed or refused check preserves any existing tag and returns an error.
// Tagging and save errors are returned, so a save failure may leave the node
// tagged without an updated record.
func (c *nodeCopiesCheck) enable() error {
	runs, err := c.runGate()
	if err != nil {
		return fmt.Errorf("the disk check on %s could not run, so it was not given copies: %w", c.node, err)
	}
	result := installer.EvaluateDiskGate(runs, c.engineTimeout, c.measureRTT(), c.ignoreSlowDisk)
	writeDiskGate(c.out, result)
	tunnel := writeTunnelRTT(c.out, c.measureTunnel())
	record := config.NodeStorage{
		Copies: result.Passed, P99Ms: result.P99Ms, MaxMs: result.MaxMs, CapHit: result.CapHit,
		UnderlayRTTP99Ms: result.UnderlayRTTP99Ms, TunnelRTTP99Ms: tunnel, Overridden: result.Overridden,
		CheckedAt: c.now().UTC().Format(time.RFC3339),
	}
	// A refused recheck leaves an earlier tag alone, because removing it is
	// refused while the node holds copies. The record follows the tag.
	stillTakes := ""
	if result.Passed {
		if err := c.setTag(true); err != nil {
			return err
		}
	} else if tagged, err := c.isTagged(); err != nil {
		stillTakes = fmt.Sprintf(" kip could not read whether %s still takes copies from an earlier check (%v).", c.node, err)
	} else if tagged {
		record.Copies = true
		record.Overridden = c.previous != nil && c.previous.Overridden
		stillTakes = fmt.Sprintf(" %s still takes copies from its earlier check. To stop new copies on it, run 'kip node storage %s --copies=false'.", c.node, c.node)
	}
	if err := c.save(record); err != nil {
		return err
	}
	switch {
	case result.Passed && result.Overridden:
		say(c.out, "  ✔  %s can hold copies. Its disk was accepted with --ignore-slow-disk, which is recorded with the node.\n\n", c.node)
		return nil
	case result.Passed:
		say(c.out, "  ✔  %s can hold copies\n\n", c.node)
		return nil
	case result.Hard:
		return fmt.Errorf("%s was not given copies: %s%s This cannot be overridden", c.node, strings.Join(result.Refusals, " "), stillTakes)
	}
	return fmt.Errorf("%s was not given copies: %s%s To accept the disk anyway, run 'kip node storage %s --copies --ignore-slow-disk'", c.node, strings.Join(result.Refusals, " "), stillTakes, c.node)
}

func writeDiskGate(out io.Writer, r installer.DiskGateResult) {
	say(out, "  Disk:     1 in 100 syncs took %.1f ms or longer, the slowest %.0f ms\n", r.P99Ms, r.MaxMs)
	if r.UnderlayRTTP99Ms != nil {
		say(out, "  Network:  1 in 100 connections to the server took %.1f ms or longer\n", *r.UnderlayRTTP99Ms)
	}
	for _, w := range r.Warnings {
		say(out, "  ⚠  %s\n", w)
	}
}

// writeTunnelRTT prints the round trip over the pod network, the path
// Longhorn's replica traffic takes, and returns its p99 for the node record.
// It only informs: a missing or lossy measurement warns and refuses nothing.
func writeTunnelRTT(out io.Writer, s *installer.RTTSample) *float64 {
	switch {
	case s == nil:
		say(out, "  ⚠  The round trip to the server over the pod network could not be measured.\n")
		return nil
	case s.Lost == s.Sent:
		say(out, "  ⚠  None of %d pings to the server over the pod network got a reply.\n", s.Sent)
		return nil
	}
	say(out, "  Tunnel:   the slowest of %d pings to the server over the pod network took %.1f ms\n", s.Sent-s.Lost, s.MaxMs)
	if s.Lost > 0 {
		say(out, "  ⚠  %d of %d pings to the server over the pod network got no reply.\n", s.Lost, s.Sent)
	}
	p99 := s.P99Ms
	return &p99
}

// disableCopies removes node's replica tag when it holds no copies. An
// unreadable replica count refuses, because removing the tag from a node
// that holds copies leaves them where Longhorn would not place them again.
func disableCopies(out io.Writer, node string, replicas func() (int, error), setTag func(on bool) error) error {
	n, err := replicas()
	if err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("%s holds %d %s of volumes. Moving copies off a node is not available yet, so it keeps taking copies", node, n, pluralCopies(int64(n)))
	}
	if err := setTag(false); err != nil {
		return err
	}
	say(out, "\n  ✔  %s no longer takes copies of volumes\n\n", node)
	return nil
}

// checkJoinedNodeForCopies runs the disk check on a node that has just
// joined. Check failures are printed with the retry command so the join
// can complete even when copy eligibility could not be established.
func checkJoinedNodeForCopies(out io.Writer, server, node installer.CommandRunner, serverRec, rec installer.NodeRecord, save func(config.NodeStorage) error) {
	say(out, "  Checking the disk of %s for copies of volumes. This takes about a minute...\n", rec.Name)
	if err := newNodeCopiesCheck(out, rec, server, node, serverRec, false, save).enable(); err != nil {
		say(out, "  ⚠  %v\n     %s runs workloads and holds no copies until 'kip node storage %s --copies' passes.\n\n", err, rec.Name, rec.Name)
	}
}

// nodeCopiesColumn reports recorded eligibility for copies. The server is
// always eligible. Workers with no check record show an unknown state.
func nodeCopiesColumn(r installer.NodeRecord) string {
	switch {
	case r.Role == installer.NodeRoleServer:
		return "yes"
	case r.Storage == nil:
		return "-"
	case r.Storage.Copies:
		return "yes"
	}
	return "no"
}

// reportCopiesForUpgrade recommends a disk check for untagged nodes holding
// healthy replicas and counts services and volumes whose copies are unset.
// It reads cluster state without changing it.
func reportCopiesForUpgrade(out io.Writer, client installer.CommandRunner) {
	inv, err := installer.ReadCopiesInventory(client)
	if err != nil {
		say(out, "  ⚠  could not list which nodes hold copies of volumes (%v)\n", err)
		return
	}
	for _, w := range inv.UntaggedHolders {
		say(out, "  ⚠  %s holds copies of volumes and has not passed the disk check. Run 'kip node storage %s --copies'\n", w, w)
	}
	switch {
	case inv.Unmanaged == 1:
		say(out, "  ·  1 service or volume has copies unset, so Kipper leaves its copies as they are. 'kip status' lists it\n")
	case inv.Unmanaged > 1:
		say(out, "  ·  %d services and volumes have copies unset, so Kipper leaves their copies as they are. 'kip status' lists them\n", inv.Unmanaged)
	}
}
