package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/installer"
	"github.com/getkipper/kipper/kip/internal/ssh"
)

var nodeRemoveCmd = &cobra.Command{
	Use:   "remove <node>",
	Short: "Remove a worker node from the cluster",
	Long: `Move a worker's workloads and volume copies to the other nodes, uninstall
k3s from it and remove it from the cluster.

kip asks Longhorn to copy the worker's volumes elsewhere and waits until no
volume has its only healthy copy there, then drains the worker. It checks that
the host still has the machine ID recorded for the worker before it uninstalls
k3s and wipes Kipper's data from it, deletes the node, removes its address from
the kip-managed firewalls of the remaining nodes and drops its record. Running
the command again resumes an interrupted removal. To keep the worker, stop
the removal and run 'kip node add' before kip records it as wiping, just
before it reports that it is uninstalling k3s. Only one kip node add or remove
runs on a cluster at a time. The server cannot be removed.

With --force, kip deregisters a worker whose host is gone for good without
draining or wiping it. It deletes the node, removes the worker's address from
kip-managed firewalls and drops its record. Resume an interrupted forced removal
with --force and include --accept-data-loss again if it was needed.`,
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE:         runNodeRemove,
}

func init() {
	nodeRemoveCmd.Flags().String("ssh-key", "", "path to SSH private key")
	nodeRemoveCmd.Flags().Bool("force", false, "remove a worker whose host is gone for good, without changing anything on the host")
	nodeRemoveCmd.Flags().Bool("accept-data-loss", false, "with --force, remove the worker even when a volume's only data is on it")
	nodeRemoveCmd.Flags().Bool("yes", false, "remove the worker without asking for confirmation")
	nodeRemoveCmd.Flags().Duration("timeout", 30*time.Minute, "maximum wait for Longhorn to copy volumes off the worker and, separately, for the drain")
	nodeCmd.AddCommand(nodeRemoveCmd)
}

// nodeRemoval removes one worker. Injected host and firewall operations allow
// tests to verify the order of destructive steps.
type nodeRemoval struct {
	Out            io.Writer
	Master         installer.CommandRunner
	Worker         installer.NodeRecord
	Timeout        time.Duration
	Poll           time.Duration
	Sleep          func(time.Duration)
	Force          bool
	AcceptDataLoss bool
	// WipeWorker must call claim after identity checks and before changing the host.
	WipeWorker        func(claim func() error) error
	HostHoldsReplicas func() (bool, error)
	HostRunsK3s       func() (bool, error)
	UpdatePeers       func(remaining []installer.NodeRecord) ([]string, error)
	SaveRecords       func(remaining []installer.NodeRecord)
}

func runNodeRemove(cmd *cobra.Command, args []string) error {
	cluster, _, err := loadCurrentCluster()
	if err != nil {
		return err
	}
	if cluster.Host == "" {
		return fmt.Errorf("no host recorded for cluster %s. Removing a node needs SSH access to the server", cluster.Name)
	}
	sshKey, _ := cmd.Flags().GetString("ssh-key")
	yes, _ := cmd.Flags().GetBool("yes")
	force, _ := cmd.Flags().GetBool("force")
	acceptDataLoss, _ := cmd.Flags().GetBool("accept-data-loss")
	if acceptDataLoss && !force {
		return fmt.Errorf("--accept-data-loss only applies with --force")
	}
	timeout, _ := cmd.Flags().GetDuration("timeout")
	if timeout < time.Second {
		return fmt.Errorf("--timeout must be at least 1s, got %s", timeout)
	}

	server, err := dialNodeSSH(cluster.Host, sshKey, cluster)
	if err != nil {
		return fmt.Errorf("connecting to the server: %w", err)
	}
	defer func() { _ = server.Close() }()

	serverID, err := installer.ReadMachineID(server)
	if err != nil {
		return fmt.Errorf("checking the server's identity: %w", err)
	}
	records, err := installer.ReadNodeRecords(server)
	if err != nil {
		return err
	}
	cluster = withNodes(cluster, records)
	if err := serverIsTheCluster(serverID, cluster.MachineID, records); err != nil {
		return fmt.Errorf("%s: %w. Check the cluster's host. Nothing was changed", cluster.Host, err)
	}
	worker, err := removableWorker(records, args[0])
	if err != nil {
		return err
	}
	if err := removalMayStart(records, worker.MachineID); err != nil {
		return err
	}
	if err := installer.RequireEvictionDrainPolicy(server); err != nil {
		return err
	}

	dialWorker := func() (installer.CommandRunner, func(), error) {
		c, err := dialNodeSSH(worker.Host, sshKey, cluster)
		if err != nil {
			return nil, nil, err
		}
		return c, func() { _ = c.Close() }, nil
	}
	switch {
	case force:
		// Bound the probe so password prompts and unreachable hosts cannot stall forced removal.
		probe := func() (installer.CommandRunner, func(), error) {
			cfg, err := nodeSSH(worker.Host, sshKey, cluster)
			if err != nil {
				return nil, nil, err
			}
			cfg.Options = append(cfg.Options, "BatchMode=yes")
			cfg.CommandTimeout = 30 * time.Second
			c, err := ssh.Dial(cfg)
			if err != nil {
				return nil, nil, err
			}
			return c, func() { _ = c.Close() }, nil
		}
		if err := refuseForceForLiveHost(worker, probe); err != nil {
			return err
		}
	case !installer.RemovalStarted(worker.State):
		// Verify the host before changing its record, because removal needs
		// SSH access to finish cleaning up the host.
		if err := checkWorkerIdentity(worker, dialWorker); err != nil {
			return err
		}
	}

	volumes, err := installer.VolumesOnNode(server, worker.Name)
	if err != nil {
		return err
	}
	if force {
		fmt.Printf("\n  Forcing the removal of worker %s (%s) deregisters it without changing\n", worker.Name, worker.Host)
		fmt.Printf("  anything on the host. Its node and record are deleted, and its address is\n")
		fmt.Printf("  removed from kip-managed firewalls. Use this only for a host that is powered off or gone for good.\n\n")
	} else {
		fmt.Printf("\n  Removing worker %s (%s) moves its workloads to the other nodes, asks\n", worker.Name, worker.Host)
		fmt.Printf("  Longhorn to copy its volumes elsewhere, uninstalls k3s from the host and\n")
		fmt.Printf("  wipes Kipper's data from it.\n\n")
	}
	reportNodeVolumes(os.Stdout, worker.Name, volumes)
	if !yes {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return fmt.Errorf("stdin is not a terminal. Pass --yes to remove %s without confirmation", worker.Name)
		}
		question := fmt.Sprintf("  Remove %s? [y/N] ", worker.Name)
		if force {
			question = fmt.Sprintf("  Is %s powered off or gone for good, and should kip forget it? [y/N] ", worker.Host)
		}
		ok, err := confirmInteractive(question)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("node removal canceled. %s is unchanged", worker.Name)
		}
	}

	lease, err := installer.TakeNodeLease(server, "kip node remove "+worker.Name)
	if err != nil {
		return err
	}
	defer lease.Release()
	master := lease.Server(server)
	// The records may have changed while kip waited for confirmation.
	if records, err = installer.ReadNodeRecords(master); err != nil {
		return err
	}
	cluster = withNodes(cluster, records)
	if current, found := findByMachineID(records, worker.MachineID); !found || current.State != worker.State || current.Host != worker.Host {
		return fmt.Errorf("%s changed while kip was waiting for confirmation. Run the command again", worker.Name)
	}
	if err := removalMayStart(records, worker.MachineID); err != nil {
		return err
	}

	dialHost := func(host string) (installer.FirewallRunner, installer.FirewallRunner, func(), error) {
		c, err := dialNodeSSH(host, sshKey, cluster)
		if err != nil {
			return nil, nil, nil, err
		}
		return lease.Host(c), c, func() { _ = c.Close() }, nil
	}
	onWorker := func(use func(installer.CommandRunner) error) error {
		client, _, done, err := dialHost(worker.Host)
		if err != nil {
			return fmt.Errorf("connecting to %s: %w. Run the command again once the host is reachable", worker.Host, err)
		}
		defer done()
		return use(client)
	}
	if !force {
		// Once this run has reached the worker, the worker refuses any older
		// run's command still on its way there.
		if err := onWorker(func(c installer.CommandRunner) error { _, err := c.Run("true"); return err }); err != nil {
			return err
		}
		if err := onWorker(func(c installer.CommandRunner) error { return checkRemovalTargetAccess(master, c, worker.Name) }); err != nil {
			return err
		}
	}
	r := nodeRemoval{
		Out: os.Stdout, Master: master, Worker: worker, Force: force, AcceptDataLoss: acceptDataLoss,
		Timeout: timeout, Poll: 10 * time.Second, Sleep: time.Sleep,
		WipeWorker: func(claim func() error) error {
			return onWorker(func(c installer.CommandRunner) error { return wipeRemovedWorker(c, worker, readLocalMachineID, claim) })
		},
		HostRunsK3s: func() (runs bool, err error) {
			err = onWorker(func(c installer.CommandRunner) error { runs, err = hostRunsK3s(c); return err })
			return runs, err
		},
		HostHoldsReplicas: func() (holds bool, err error) {
			err = onWorker(func(c installer.CommandRunner) error { holds, err = hostHoldsReplicas(c); return err })
			return holds, err
		},
		UpdatePeers: func(remaining []installer.NodeRecord) ([]string, error) {
			access := func(nodes []accessNode) (*nodeAccess, []string, error) {
				nc, err := newNodeAccessCluster(os.Stdout, master, cluster, sshKey)
				if err != nil {
					return nil, nil, err
				}
				return prepareNodeAccess(nc, nodes, nil, "node remove "+worker.Name, false)
			}
			return updateRemainingPeers(os.Stdout, accessNode{node: master, base: server}, remaining, worker, dialHost, access)
		},
		SaveRecords: func(remaining []installer.NodeRecord) { mirrorNodes(cluster, remaining) },
	}
	return r.run()
}

// run keeps the removal record until removal and peer firewall updates
// succeed, so an interrupted removal can resume. Forced removal skips host
// cleanup. A record already claimed for wiping resumes at the wipe.
func (r nodeRemoval) run() error {
	w := r.Worker
	if !installer.RemovalStarted(w.State) {
		w.State = installer.NodeStateRemoving
		if err := installer.SaveNodeRecord(r.Master, w); err != nil {
			return err
		}
	}
	if err := installer.EvictLonghornNode(r.Master, w.Name); err != nil {
		return err
	}
	if r.Force {
		if err := r.forgetDeadHost(); err != nil {
			return err
		}
		if err := r.claimTheWipe(""); err != nil {
			return err
		}
		return r.finish()
	}
	// Resume a claimed wipe without moving workloads again. Recheck data
	// safety because a copy elsewhere may have failed since the claim.
	prepare := r.prepareTheWipe
	if w.State == installer.NodeStateWiping {
		prepare = r.guardTheWipe
	}
	if err := prepare(); err != nil {
		return err
	}
	if err := r.WipeWorker(func() error { return r.claimTheWipe(fmt.Sprintf("  ...  Uninstalling k3s from %s\n", w.Host)) }); err != nil {
		return fmt.Errorf("uninstalling k3s from %s: %w", w.Host, err)
	}
	say(r.Out, "  ✔  k3s uninstalled from %s\n", w.Host)
	return r.finish()
}

// removalMayStart refuses a removal while another node is being removed or
// has not finished joining.
func removalMayStart(records []installer.NodeRecord, machineID string) error {
	if other, busy := installer.RemovalInProgress(records, machineID); busy {
		return fmt.Errorf("%s is still being removed. Finish the removal first with 'kip node remove %s', or keep it with 'kip node add %s'", other.Name, other.Name, other.Host)
	}
	if other, busy := installer.JoinInProgress(records, machineID); busy {
		return fmt.Errorf("%s has not finished joining, and its firewall could still admit the removed worker. First finish the join with 'kip node add %s', or remove the node with 'kip node remove %s'", other.Name, other.Host, other.Name)
	}
	return nil
}

// claimTheWipe records that the worker can no longer be kept, then prints
// announce. It is the last step before the host is wiped or forgotten.
func (r nodeRemoval) claimTheWipe(announce string) error {
	if err := r.stillRemoving(); err != nil {
		return err
	}
	w := r.Worker
	w.State = installer.NodeStateWiping
	if err := installer.SaveNodeRecord(r.Master, w); err != nil {
		return err
	}
	say(r.Out, "%s", announce)
	return nil
}

// prepareTheWipe waits for healthy volume copies elsewhere and drains a
// registered worker if k3s is still installed. It then checks for replicas
// whose data may exist only on this host before allowing the wipe.
func (r nodeRemoval) prepareTheWipe() error {
	w := r.Worker
	say(r.Out, "  ...  Waiting for Longhorn to copy volumes off %s (up to %s)\n", w.Name, r.Timeout)
	if err := r.waitForCopiesElsewhere(); err != nil {
		return err
	}
	registered, err := installer.NodeRegistered(r.Master, w.Name)
	if err != nil {
		return err
	}
	runsK3s := false
	if registered {
		if runsK3s, err = r.HostRunsK3s(); err != nil {
			return err
		}
	}
	switch {
	case registered && !runsK3s:
		say(r.Out, "  ℹ  k3s is already gone from %s, so there is nothing left to drain\n", w.Host)
	case registered:
		if err := r.stillRemoving(); err != nil {
			return err
		}
		if err := installer.CordonNode(r.Master, w.Name); err != nil {
			return err
		}
		say(r.Out, "  ...  Draining %s (up to %s)\n", w.Name, r.Timeout)
		if err := installer.DrainNode(r.Master, w.Name, r.Timeout); err != nil {
			return fmt.Errorf("%w. %s stays cordoned. Run the command again to retry", err, w.Name)
		}
		if err := r.lastCopies(); err != nil {
			return err
		}
		say(r.Out, "  ✔  %s is drained, and every volume with a healthy copy on it has one on another node\n", w.Name)
	default:
		say(r.Out, "  ℹ  %s is no longer registered. Finishing its removal\n", w.Name)
	}

	return r.guardTheWipe()
}

// forgetDeadHost checks that removal is still in progress and requires accepted
// data loss for volumes with a replica on the worker and no healthy copy elsewhere.
// It leaves the host untouched because forced removal assumes it is gone.
func (r nodeRemoval) forgetDeadHost() error {
	w := r.Worker
	if err := r.stillRemoving(); err != nil {
		return err
	}
	only, err := installer.VolumesOnlyOn(r.Master, w.Name)
	if err != nil {
		return err
	}
	if len(only) > 0 && !r.AcceptDataLoss {
		return fmt.Errorf("%s may hold the only data for %s because these volumes have no healthy copy on another node. Removing it risks losing that data. Pass --accept-data-loss to remove it anyway, or bring the host back and remove it without --force",
			w.Name, strings.Join(only, ", "))
	}
	if len(only) > 0 {
		say(r.Out, "  ⚠  Accepting the risk of losing data for %s when removing %s\n", strings.Join(only, ", "), w.Name)
	}
	say(r.Out, "  ℹ  Leaving %s untouched: its host is treated as gone\n", w.Host)
	return nil
}

// finish deletes the node objects, removes the worker's address from kip-managed
// firewalls on the remaining nodes and drops its record. It reports firewall
// changes that need manual action.
func (r nodeRemoval) finish() error {
	w := r.Worker
	if err := installer.DeleteNodeObjects(r.Master, w.Name); err != nil {
		return err
	}

	// Commands outside the node lease, such as an install re-run, can change
	// the records during the waits, so the peer rules come from a fresh read.
	current, err := installer.ReadNodeRecords(r.Master)
	if err != nil {
		return fmt.Errorf("%w. %s is still recorded as removing. Run the command again to finish", err, w.Name)
	}
	remaining := withoutNode(current, w.MachineID)
	manual, err := r.UpdatePeers(remaining)
	if err != nil {
		return fmt.Errorf("%w. %s is still recorded as removing. Run the command again to finish", err, w.Name)
	}
	if err := installer.DeleteNodeRecord(r.Master, w.MachineID); err != nil {
		return err
	}
	r.SaveRecords(remaining)
	say(r.Out, "\n  ✔  %s removed from the cluster\n", w.Name)
	for _, note := range manual {
		say(r.Out, "  ⚠  %s\n", note)
	}
	if r.Force {
		say(r.Out, "  ⚠  %s still holds k3s's credentials for this cluster. If it comes back, its agent or a kip command already in transit to the host can register the node again. Run /usr/local/bin/k3s-agent-uninstall.sh on it before it rejoins any network the cluster can reach, or reinstall it\n", w.Host)
	}
	say(r.Out, "\n")
	return nil
}

// guardTheWipe rechecks the removal record before wiping the host. It blocks
// the wipe if a volume has no healthy copy elsewhere and the host still has
// replica-directory entries that could contain the only remaining data.
func (r nodeRemoval) guardTheWipe() error {
	w := r.Worker
	if err := r.stillRemoving(); err != nil {
		return err
	}
	only, err := installer.VolumesOnlyOn(r.Master, w.Name)
	if err != nil {
		return err
	}
	if len(only) == 0 {
		return nil
	}
	holds, err := r.HostHoldsReplicas()
	if err != nil {
		return err
	}
	if holds {
		return fmt.Errorf("%s's disk may hold the only data of %s, which %s no healthy copy on another node, so kip leaves the host alone. Recover or delete %s first, then run the command again",
			w.Name, strings.Join(only, ", "), plural(len(only), "has", "have"), plural(len(only), "that volume", "those volumes"))
	}
	return nil
}

// stillRemoving stops removal if another command has cleared the worker's
// removal state or deleted its record. The node lease keeps other kip node
// runs out. This check also catches changes made before this run took the lease.
func (r nodeRemoval) stillRemoving() error {
	records, err := installer.ReadNodeRecords(r.Master)
	if err != nil {
		return err
	}
	current, found := findByMachineID(records, r.Worker.MachineID)
	if !found || !installer.RemovalStarted(current.State) {
		return fmt.Errorf("%s is no longer recorded as being removed, so another kip run changed it. This removal has stopped", r.Worker.Name)
	}
	return nil
}

func findByMachineID(records []installer.NodeRecord, machineID string) (installer.NodeRecord, bool) {
	for _, rec := range records {
		if rec.MachineID == machineID {
			return rec, true
		}
	}
	return installer.NodeRecord{}, false
}

// waitForCopiesElsewhere polls until no volume has its only healthy copy on
// the worker, or the timeout passes.
func (r nodeRemoval) waitForCopiesElsewhere() error {
	deadline := time.Now().Add(r.Timeout)
	for {
		err := r.lastCopies()
		if err == nil || time.Now().After(deadline) {
			return err
		}
		r.Sleep(r.Poll)
	}
}

// lastCopies returns an error naming volumes whose healthy copies are all on
// the worker, or nil when every volume with a healthy copy there has one elsewhere.
func (r nodeRemoval) lastCopies() error {
	volumes, err := installer.VolumesOnNode(r.Master, r.Worker.Name)
	if err != nil {
		return err
	}
	var last []string
	for _, v := range volumes {
		if v.LastCopy() {
			last = append(last, v.Label())
		}
	}
	if len(last) == 0 {
		return nil
	}
	return fmt.Errorf("%s still %s its only healthy copy on %s. Longhorn needs room for a copy on another node: free disk space or add a node, then run the command again",
		strings.Join(last, ", "), plural(len(last), "has", "have"), r.Worker.Name)
}

// hostHoldsReplicas reports whether Longhorn's replica directory has any entries.
// A missing directory returns false. Other read failures return an error.
func hostHoldsReplicas(client installer.CommandRunner) (bool, error) {
	// Treat a missing directory as empty so removal can resume after a wipe.
	// Other failures, such as an I/O error, must stop the wipe.
	out, err := client.Run(`if entries=$(LC_ALL=C ls -A /var/lib/longhorn/replicas 2>&1); then if [ -n "$entries" ]; then echo present; else echo empty; fi; else case "$entries" in *"No such file or directory"*) echo empty ;; *) printf '%s\n' "$entries" >&2; exit 1 ;; esac; fi`)
	if err != nil {
		return false, fmt.Errorf("looking for Longhorn data: %w", err)
	}
	switch strings.TrimSpace(out) {
	case "present":
		return true, nil
	case "empty":
		return false, nil
	}
	return false, fmt.Errorf("looking for Longhorn data: unexpected answer %q", strings.TrimSpace(out))
}

// hostRunsK3s reports whether /usr/local/bin/k3s exists on the host.
func hostRunsK3s(client installer.CommandRunner) (bool, error) {
	out, err := client.Run("if [ -e /usr/local/bin/k3s ]; then echo present; fi")
	if err != nil {
		return false, fmt.Errorf("checking for k3s: %w", err)
	}
	return strings.TrimSpace(out) == "present", nil
}

// keepRemovingNode allows kip node add to keep a removing worker only while
// its wipe is unclaimed and k3s is still installed. After a wipe, k3s rejects
// the host's new node password for the old Node, so removal must finish first.
func keepRemovingNode(worker installer.CommandRunner, rec installer.NodeRecord) error {
	if rec.State == installer.NodeStateWiping {
		return fmt.Errorf("the removal of %s has started wiping or forgetting its host, so the worker can no longer be kept. Finish it with 'kip node remove %s', then add the host again", rec.Name, rec.Name)
	}
	if rec.State != installer.NodeStateRemoving {
		return nil
	}
	runs, err := hostRunsK3s(worker)
	if err != nil {
		return err
	}
	if !runs {
		return fmt.Errorf("the removal of %s already wiped its host, so it cannot be kept. Finish it with 'kip node remove %s', then add the host again", rec.Name, rec.Name)
	}
	return nil
}

// refuseForceForLiveHost rejects a matching machine ID because forced removal
// could leave k3s running. A different ID or a connection or identity-read error
// allows forced removal to proceed.
func refuseForceForLiveHost(worker installer.NodeRecord, dial func() (installer.CommandRunner, func(), error)) error {
	client, done, err := dial()
	if err != nil {
		return nil
	}
	defer done()
	if id, err := installer.ReadMachineID(client); err == nil && id == worker.MachineID {
		return fmt.Errorf("%s still answers as %s, so remove it without --force, which drains and wipes it", worker.Host, worker.Name)
	}
	return nil
}

// checkWorkerIdentity checks the host's recorded machine ID over SSH before
// removal starts. An unreachable or replaced host returns an error before
// any removal changes are made.
func checkWorkerIdentity(worker installer.NodeRecord, dial func() (installer.CommandRunner, func(), error)) error {
	client, done, err := dial()
	if err != nil {
		return fmt.Errorf("connecting to %s: %w. kip requires a reachable host to start removing a worker, so nothing was changed", worker.Host, err)
	}
	defer done()
	id, err := installer.ReadMachineID(client)
	if err != nil {
		return fmt.Errorf("reading the machine ID of %s: %w. The identity check failed, so nothing was changed", worker.Host, err)
	}
	if id != worker.MachineID {
		return fmt.Errorf("%s now has machine ID %s, not the recorded %s, so nothing was changed. Check the address of %s", worker.Host, id, worker.MachineID, worker.Name)
	}
	return nil
}

// removalBlocksJoin refuses a join while another node is being removed: the
// join rewrites every node's peer rules, which would admit that node again.
// Joining the node being removed can keep it until its wipe is claimed.
func removalBlocksJoin(records []installer.NodeRecord, joining string) error {
	if other, busy := installer.RemovalInProgress(records, joining); busy {
		return fmt.Errorf("%s is being removed, and a join would give its address access again. Finish the removal with 'kip node remove %s', or keep it with 'kip node add %s', then add this node", other.Name, other.Name, other.Host)
	}
	return nil
}

// removalBlocksReboot refuses to reboot a node being removed, because the
// reboot uncordons it afterwards.
func removalBlocksReboot(rec installer.NodeRecord) error {
	if installer.RemovalStarted(rec.State) {
		return fmt.Errorf("%s is being removed. Finish the removal with 'kip node remove %s', or keep it with 'kip node add %s'", rec.Name, rec.Name, rec.Host)
	}
	return nil
}

// serverIsTheCluster checks the connected server's machine ID against the ID
// saved locally at installation, then requires a matching server record.
// The local ID is checked first because a different cluster at the same address
// matches its own records. An empty local ID, as in older configuration entries,
// leaves only the server record check.
func serverIsTheCluster(serverID, localID string, records []installer.NodeRecord) error {
	if localID != "" && serverID != localID {
		return fmt.Errorf("the host has machine ID %s, but this cluster's server was recorded as %s", serverID, localID)
	}
	rec, ok := installer.ServerRecord(records)
	if !ok {
		return fmt.Errorf("the cluster's node records hold no server record")
	}
	if rec.MachineID != serverID {
		return fmt.Errorf("the host has machine ID %s, but the cluster records its server %s as %s", serverID, rec.Name, rec.MachineID)
	}
	return nil
}

// removableWorker finds a worker by node name or host and rejects the server.
func removableWorker(records []installer.NodeRecord, target string) (installer.NodeRecord, error) {
	rec, err := findNodeRecord(records, target)
	if err != nil {
		return installer.NodeRecord{}, err
	}
	if rec.Role != installer.NodeRoleWorker {
		return installer.NodeRecord{}, fmt.Errorf("%s is the server, and only workers can be removed. Run 'kip cluster uninstall' to remove the whole cluster", rec.Name)
	}
	return rec, nil
}

func withoutNode(records []installer.NodeRecord, machineID string) []installer.NodeRecord {
	var out []installer.NodeRecord
	for _, rec := range records {
		if rec.MachineID != machineID {
			out = append(out, rec)
		}
	}
	return out
}

// wipeRemovedWorker checks the recorded machine ID, uninstalls k3s and wipes
// Kipper's data. It rejects the local machine when its ID can be read and
// verifies that the k3s binary is gone and k3s-agent is inactive afterwards.
// It calls claim after identity checks and before uninstalling, and stops
// without uninstalling if claim returns an error.
func wipeRemovedWorker(client installer.CommandRunner, node config.NodeRecord, localMachineID func() (string, error), claim func() error) error {
	id, err := installer.ReadMachineID(client)
	if err != nil {
		return err
	}
	if id != node.MachineID {
		return fmt.Errorf("the host now has machine ID %s, not the recorded %s, so it was left alone. Check the address of %s", id, node.MachineID, node.Name)
	}
	if local, err := localMachineID(); err == nil && local == id {
		return fmt.Errorf("it is the machine running kip. Run the command from another machine")
	}
	if err := claim(); err != nil {
		return err
	}
	if err := installer.UninstallHost(client); err != nil {
		return err
	}
	// The uninstall scripts are skipped when they are missing, so the Node is
	// only deleted once k3s is shown to be gone.
	out, err := client.Run("if [ -e /usr/local/bin/k3s ] || systemctl is-active --quiet k3s-agent; then echo present; fi")
	if err != nil {
		return fmt.Errorf("checking that k3s is gone: %w", err)
	}
	if strings.TrimSpace(out) == "present" {
		return fmt.Errorf("k3s is still installed after the uninstall. Remove it with /usr/local/bin/k3s-agent-uninstall.sh and run the command again")
	}
	return nil
}

// updateRemainingPeers rewrites the peer rules on the server and every
// remaining worker whose firewall kip manages, so the removed node's address
// loses its access there. Each worker's machine ID is checked before its
// firewall is changed. Where the cluster has an operator access list, each
// node's access table is rebuilt from the remaining node records first. It returns a
// note for each node whose firewall kip does not manage, naming the access
// the operator has to revoke, and for each node without an access table.
func updateRemainingPeers(out io.Writer, server accessNode, remaining []installer.NodeRecord, removed installer.NodeRecord, dial func(host string) (fenced, base installer.FirewallRunner, done func(), err error), access func([]accessNode) (*nodeAccess, []string, error)) ([]string, error) {
	serverRec, ok := installer.ServerRecord(remaining)
	if !ok {
		return nil, fmt.Errorf("the node records hold no server")
	}
	server.rec = serverRec
	nodes := []accessNode{server}
	var closers []func()
	defer func() {
		for _, done := range closers {
			done()
		}
	}()
	for _, rec := range remaining {
		if rec.Role != installer.NodeRoleWorker {
			continue
		}
		c, base, done, err := dial(rec.Host)
		if err != nil {
			return nil, fmt.Errorf("connecting to node %s (%s) to remove the old node's firewall rules: %w", rec.Name, rec.Host, err)
		}
		closers = append(closers, done)
		if id, err := installer.ReadMachineID(c); err != nil || id != rec.MachineID {
			return nil, fmt.Errorf("%s no longer answers as node %s (machine ID %q, recorded %s). Check the address", rec.Host, rec.Name, id, rec.MachineID)
		}
		nodes = append(nodes, accessNode{rec: rec, node: c, base: base})
	}
	acc, manual, err := access(nodes)
	if err != nil {
		return nil, err
	}
	for _, n := range nodes {
		if err := acc.load(n, remaining, removed.MachineID); err != nil {
			return nil, err
		}
		label := n.rec.Name
		if n.rec.Role == installer.NodeRoleServer {
			label = "the server"
		}
		note, err := revokePeer(out, n.node, label, installer.FirewallMeshFor(remaining, n.rec.MachineID), removed)
		if err != nil {
			return nil, err
		}
		manual = appendNote(manual, note)
	}
	return manual, nil
}

func appendNote(notes []string, note string) []string {
	if note == "" {
		return notes
	}
	return append(notes, note)
}

// revokePeer rewrites a kip-managed firewall for mesh, which drops the rules
// for any node no longer in it. For a firewall kip does not manage it changes
// nothing and returns the note that tells the operator what to revoke.
func revokePeer(out io.Writer, client installer.FirewallRunner, label string, mesh installer.FirewallMesh, removed installer.NodeRecord) (string, error) {
	audit, err := installer.AuditFirewall(client)
	if err != nil {
		return "", fmt.Errorf("auditing the firewall on %s: %w", label, err)
	}
	if !audit.KipperManaged {
		return fmt.Sprintf("kip does not manage the firewall on %s, so it did not change it. Remove any rules there that admit %s on 51820/udp, 10250/tcp or 9100/tcp", label, peerAddressOf(removed)), nil
	}
	return "", applyPeerFirewall(out, client, label, mesh, existingFirewall, false, false)
}

// peerAddressOf is the address other nodes' firewalls admit for node.
func peerAddressOf(node installer.NodeRecord) string {
	if node.PeerAddress != "" {
		return node.PeerAddress
	}
	return node.Host
}
