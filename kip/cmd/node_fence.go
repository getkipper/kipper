package cmd

import (
	"fmt"
	"io"
	"os"
	"os/user"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/installer"
	"github.com/getkipper/kipper/kip/internal/ssh"
)

var nodeFenceCmd = &cobra.Command{
	Use:   "fence <node>",
	Short: "Fence a dead worker so its services can start on other nodes",
	Long: `Make sure the old pods on a worker that stopped reporting can never write
to their volumes again, then let Kubernetes start its services on other nodes.

Run it once a worker has been NotReady for at least a minute. When the worker
still answers over SSH, kip installs a systemd drop-in that keeps its k3s
agent from starting, even after a reboot, then stops k3s and every container
on it and checks that no node processes or volume mounts remain. When it
does not answer, kip asks you to confirm that the host is powered off and
stays off until you rejoin it, and refuses while its kubelet still answers
or cannot be checked.

Before Kubernetes moves anything, kip checks each service with a volume on
the worker. A service starts elsewhere only if another node holds a current
copy of every one of its volumes. A service whose other copies are older
stays stopped until you accept the older data, interactively or with
--accept-stale-data. A service whose only copy is on the worker stays
stopped until the worker rejoins or the service is deleted and restored from
a backup taken before the fence. kip also checks services paused by a
migration whose data transfer runs on the worker. After checking the services,
kip marks the transfer failed and suspends any Job whose pod holds a
single-node volume there to prevent those pods from restarting. These checks
apply only to Kipper workloads. kip warns about other pods with single-node
volumes on the worker.

kip then taints the node out of service, which lets Kubernetes delete its
pods and detach their volumes. Pods Kubernetes leaves behind are deleted by
kip after a short wait, and only while the node is still NotReady and
tainted. Never delete a pod of a dead node with 'kubectl delete pod --force'
instead: that skips every one of these checks.

Running the command again resumes an interrupted fence. To bring the worker
back, run 'kip node add <host>', which requires a fresh boot of the host. To
give it up, run 'kip node remove <node> --force'.`,
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE:         runNodeFence,
}

func init() {
	nodeFenceCmd.Flags().Bool("powered-off", false, "confirm that the unreachable worker is powered off and stays off until 'kip node add'")
	nodeFenceCmd.Flags().Bool("yes", false, "stop k3s on a worker that answers over SSH without asking for confirmation")
	nodeFenceCmd.Flags().Bool("accept-stale-data", false, "start services whose only other copies are older than the data on the worker")
	nodeFenceCmd.Flags().String("ssh-key", "", "path to SSH private key")
	nodeFenceCmd.Flags().Duration("timeout", 10*time.Minute, "how long to wait for the worker's services to run on other nodes")
	nodeCmd.AddCommand(nodeFenceCmd)
}

// fenceCluster is what the fence does through the server. Every call runs
// under the node lifecycle lease.
type fenceCluster interface {
	Records() ([]installer.NodeRecord, error)
	SaveRecord(installer.NodeRecord) error
	Node(name string) (installer.FenceNode, error)
	KubeletAnswers(address string) (bool, error)
	View(node string) (installer.FenceView, error)
	Stop(ns, name string, mark installer.FenceStopMark) error
	Start(ns, name string) error
	Taint(node string) error
	FailTransfer(ns, name, reason string) error
	SuspendJob(ns, name string) error
	OwnerExists(ns, apiVersion, kind, name string) (bool, error)
	DeletionPolicy() (string, error)
	PodsWithVolumes(node string) ([]installer.FencedPod, error)
	Attachments(node string) ([]installer.Attachment, error)
	DeleteAttachment(name string) error
	DeletePod(ns, name, uid string) error
	PodPlacement(ns, name string) (node string, ready bool, err error)
}

// fenceHost is what the fence does on a worker that answers over SSH.
type fenceHost interface {
	MachineID() (string, error)
	BootID() (string, error)
	InstallInterlock() error
	StopAgent() error
}

// nodeFence fences one worker. Cluster, host and time are injected so tests
// can check the order of every step.
type nodeFence struct {
	Out         io.Writer
	Cluster     fenceCluster
	Dial        func(rec installer.NodeRecord) (fenceHost, func(), error)
	Confirm     func(prompt string) (bool, error)
	Interactive bool
	Operator    string
	Now         func() time.Time
	Sleep       func(time.Duration)
	Poll        time.Duration
	// MinNotReady applies when the record is ready but the Node is NotReady.
	MinNotReady  time.Duration
	NotReadyWait time.Duration
	PodGCGrace   time.Duration
	AttachWait   time.Duration
	Timeout      time.Duration
	PoweredOff   bool
	Yes          bool
	AcceptStale  bool
}

func runNodeFence(cmd *cobra.Command, args []string) error {
	cluster, _, err := loadCurrentCluster()
	if err != nil {
		return err
	}
	if cluster.Host == "" {
		return fmt.Errorf("no host recorded for cluster %s. Fencing a node needs SSH access to the server", cluster.Name)
	}
	sshKey, _ := cmd.Flags().GetString("ssh-key")
	poweredOff, _ := cmd.Flags().GetBool("powered-off")
	yes, _ := cmd.Flags().GetBool("yes")
	acceptStale, _ := cmd.Flags().GetBool("accept-stale-data")
	timeout, _ := cmd.Flags().GetDuration("timeout")
	if timeout < time.Second {
		return fmt.Errorf("--timeout must be at least 1s, got %s", timeout)
	}

	lc, err := leaseCluster(cluster, sshKey, "kip node fence "+args[0])
	if err != nil {
		return err
	}
	defer lc.close()
	master := lc.server
	serverID, err := installer.ReadMachineID(master)
	if err != nil {
		return fmt.Errorf("checking the server's identity: %w", err)
	}
	records, err := installer.ReadNodeRecords(master)
	if err != nil {
		return err
	}
	if err := serverIsTheCluster(serverID, cluster.MachineID, records); err != nil {
		return fmt.Errorf("%s: %w. Check the cluster's host. Nothing was changed", cluster.Host, err)
	}
	operator := ""
	if u, err := user.Current(); err == nil {
		operator = u.Username
	}
	f := &nodeFence{
		Out:     os.Stdout,
		Cluster: fenceServer{master: master, cluster: withNodes(cluster, records)},
		Dial: func(rec installer.NodeRecord) (fenceHost, func(), error) {
			cfg, err := nodeSSH(rec.Host, sshKey, cluster)
			if err != nil {
				return nil, nil, err
			}
			// A powered-off host must not stall the fence on a prompt.
			cfg.Options = append(cfg.Options, "BatchMode=yes")
			cfg.CommandTimeout = 5 * time.Minute
			c, err := ssh.Dial(cfg)
			if err != nil {
				return nil, nil, err
			}
			return fenceWorker{c: lc.lease.Host(c)}, func() { _ = c.Close() }, nil
		},
		Confirm:      confirmInteractive,
		Interactive:  term.IsTerminal(int(os.Stdin.Fd())),
		Operator:     operator,
		Now:          time.Now,
		Sleep:        time.Sleep,
		Poll:         5 * time.Second,
		MinNotReady:  time.Minute,
		NotReadyWait: 3 * time.Minute,
		PodGCGrace:   time.Minute,
		AttachWait:   2 * time.Minute,
		Timeout:      timeout,
		PoweredOff:   poweredOff,
		Yes:          yes,
		AcceptStale:  acceptStale,
	}
	return f.run(args[0])
}

// fenceMayStart refuses records the fence must not act on.
func fenceMayStart(rec installer.NodeRecord) error {
	switch {
	case rec.Role != installer.NodeRoleWorker:
		return fmt.Errorf("%s is the server, and only workers can be fenced", rec.Name)
	case rec.State == installer.NodeStateJoining:
		return fmt.Errorf("%s has not finished joining. Finish the join with 'kip node add %s', or remove it with 'kip node remove %s'", rec.Name, rec.Host, rec.Name)
	case installer.RemovalStarted(rec.State):
		return fmt.Errorf("%s is being removed. Finish the removal with 'kip node remove %s'", rec.Name, rec.Name)
	}
	return nil
}

// fenceBlocksReboot refuses to reboot a fenced worker, whose k3s must stay
// stopped until it rejoins.
func fenceBlocksReboot(rec installer.NodeRecord) error {
	if installer.Fenced(rec.State) {
		return fmt.Errorf("%s is fenced, and its k3s must stay stopped. Rejoin it with 'kip node add %s', which reboots it, or remove it with 'kip node remove %s --force'", rec.Name, rec.Host, rec.Name)
	}
	return nil
}

func (f *nodeFence) run(target string) error {
	records, err := f.Cluster.Records()
	if err != nil {
		return err
	}
	rec, err := findNodeRecord(records, target)
	if err != nil {
		return err
	}
	if err := fenceMayStart(rec); err != nil {
		return err
	}
	node, err := f.Cluster.Node(rec.Name)
	if err != nil {
		return err
	}
	if !node.Exists {
		return fmt.Errorf("the cluster has no node %s, so there is nothing to fence. Finish with 'kip node remove %s --force'", rec.Name, rec.Name)
	}
	switch rec.State {
	case installer.NodeStateReady:
		if node.Ready {
			return fmt.Errorf("%s is Ready. kip fences only a worker that stopped reporting. To restart a healthy one, use 'kip node reboot %s'", rec.Name, rec.Name)
		}
		if node.NotReadyFor < f.MinNotReady {
			return fmt.Errorf("%s has been NotReady for %s. Run the command again once it has been NotReady for at least %s, so that a short network outage is not mistaken for a dead node", rec.Name, node.NotReadyFor.Round(time.Second), f.MinNotReady)
		}
	case installer.NodeStateFenced:
		if !node.OutOfService {
			return fmt.Errorf("%s is recorded as fenced, but its out-of-service taint is gone, so a rejoin has already released it. Finish the rejoin with 'kip node add %s', or remove it with 'kip node remove %s --force'", rec.Name, rec.Host, rec.Name)
		}
	}

	host, done, err := f.choosePath(rec)
	if err != nil {
		return err
	}
	if done != nil {
		defer done()
	}
	if rec.State != installer.NodeStateFenced {
		if rec, err = f.markFencing(rec, host, node); err != nil {
			return err
		}
	}
	if host != nil {
		if err := f.stopHost(&rec, host); err != nil {
			return err
		}
	} else if err := f.requireNotReady(rec.Name); err != nil {
		return err
	}
	f.warnDeletionPolicy()

	view, err := f.stopStale(&rec)
	if err != nil {
		return err
	}
	if len(view.Unmanaged) > 0 {
		say(f.Out, "  ⚠  kip does not check data for these non-Kipper pods with single-node volumes on %s: %s. The taint lets Kubernetes start them using whichever copy Longhorn has available, which may be older\n", rec.Name, strings.Join(view.Unmanaged, ", "))
	}
	if rec.State != installer.NodeStateFenced {
		if err := f.Cluster.Taint(rec.Name); err != nil {
			return err
		}
		rec.State = installer.NodeStateFenced
		rec.Fence.FencedAt = f.stamp()
		if err := f.Cluster.SaveRecord(rec); err != nil {
			return err
		}
		say(f.Out, "  ✔  %s is fenced: tainted out of service, so Kubernetes can move its pods\n", rec.Name)
	}
	if err := f.nudge(rec.Name); err != nil {
		return err
	}
	if err := f.startConsented(&rec); err != nil {
		return err
	}
	return f.report(rec, view)
}

func (f *nodeFence) stamp() string { return f.Now().UTC().Format(time.RFC3339) }

// choosePath decides between stopping the host over SSH and the powered-off
// assertion. It returns a nil host for the powered-off path.
func (f *nodeFence) choosePath(rec installer.NodeRecord) (fenceHost, func(), error) {
	host, done, err := f.Dial(rec)
	if err == nil {
		id, err := host.MachineID()
		if err != nil {
			done()
			return nil, nil, fmt.Errorf("reading the machine ID of %s: %w. Nothing was changed", rec.Host, err)
		}
		if id != rec.MachineID {
			done()
			return nil, nil, fmt.Errorf("%s now has machine ID %s, not the recorded %s, so it was left alone. Check the address of %s", rec.Host, id, rec.MachineID, rec.Name)
		}
		if f.PoweredOff {
			done()
			return nil, nil, fmt.Errorf("%s answers over SSH, so it is not powered off. Run the command without --powered-off, and kip stops k3s on it", rec.Host)
		}
		if rec.State != installer.NodeStateFenced && !f.Yes {
			ok, err := f.ask(fmt.Sprintf("  Fence %s? kip stops k3s and every container on %s and keeps k3s from starting until 'kip node add %s'. [y/N] ", rec.Name, rec.Host, rec.Host), "--yes")
			if err != nil || !ok {
				done()
				return nil, nil, canceled(rec, err)
			}
		}
		return host, done, nil
	}

	address := peerAddressOf(rec)
	up, probeErr := f.Cluster.KubeletAnswers(address)
	if probeErr != nil {
		return nil, nil, fmt.Errorf("kip cannot reach %s over SSH (%v), and cannot probe its kubelet to check that the host is off: %v. Restore SSH access, or fix the address kip records for %s, and run the command again", rec.Host, err, probeErr, rec.Name)
	}
	if up {
		return nil, nil, fmt.Errorf("kip cannot reach %s over SSH (%v), but its kubelet still answers at %s:10250, so the host is running. Restore SSH access and run the command again. kip then stops k3s on it", rec.Host, err, address)
	}
	if rec.State == installer.NodeStateFenced || f.PoweredOff {
		return nil, nil, nil
	}
	say(f.Out, "\n  kip cannot reach %s over SSH (%v).\n", rec.Host, err)
	ok, err := f.ask(fmt.Sprintf("  Is %s powered off, and will it stay off until you run kip node add? [y/N] ", rec.Name), "--powered-off")
	if err != nil || !ok {
		return nil, nil, canceled(rec, err)
	}
	return nil, nil, nil
}

func canceled(rec installer.NodeRecord, err error) error {
	if err != nil {
		return err
	}
	return fmt.Errorf("fence canceled. %s is unchanged", rec.Name)
}

// ask confirms interactively, or tells the operator which flag answers yes
// when there is no terminal.
func (f *nodeFence) ask(prompt, flag string) (bool, error) {
	if !f.Interactive {
		return false, fmt.Errorf("stdin is not a terminal. Pass %s to confirm", flag)
	}
	return f.Confirm(prompt)
}

// markFencing records the fence before any host action. A resumed fence
// keeps its start time and boot ID.
func (f *nodeFence) markFencing(rec installer.NodeRecord, host fenceHost, node installer.FenceNode) (installer.NodeRecord, error) {
	method := installer.FenceMethodPoweredOff
	if host != nil {
		method = installer.FenceMethodKillall
	}
	if rec.State == installer.NodeStateFencing && rec.Fence != nil {
		rec.Fence.Method = method
		return rec, f.Cluster.SaveRecord(rec)
	}
	bootID := installer.BootIDUnknown
	if host != nil {
		if id, err := host.BootID(); err == nil {
			bootID = id
		}
	} else if installer.ValidBootID(node.BootID) {
		bootID = node.BootID
	}
	rec.State = installer.NodeStateFencing
	rec.Fence = &config.NodeFence{Method: method, Operator: f.Operator, StartedAt: f.stamp(), BootID: bootID}
	return rec, f.Cluster.SaveRecord(rec)
}

// stopHost installs the interlock before it stops anything, so k3s cannot
// start again, then waits for the node to report NotReady.
func (f *nodeFence) stopHost(rec *installer.NodeRecord, host fenceHost) error {
	say(f.Out, "  ...  Keeping k3s on %s from starting again\n", rec.Name)
	if err := host.InstallInterlock(); err != nil {
		return err
	}
	say(f.Out, "  ...  Stopping k3s and every container on %s\n", rec.Name)
	if err := host.StopAgent(); err != nil {
		return fmt.Errorf("%w. k3s cannot start again on %s. Run the command again once the cause is fixed", err, rec.Name)
	}
	rec.Fence.Method = installer.FenceMethodKillall
	rec.Fence.VerifiedAt = f.stamp()
	if err := f.Cluster.SaveRecord(*rec); err != nil {
		return err
	}
	say(f.Out, "  ✔  No node processes or volume mounts remain on %s\n", rec.Name)
	deadline := f.Now().Add(f.NotReadyWait)
	for {
		node, err := f.Cluster.Node(rec.Name)
		if err != nil {
			return err
		}
		if !node.Ready {
			return nil
		}
		if !f.Now().Before(deadline) {
			return fmt.Errorf("%s still reports Ready %s after k3s stopped on it. Check it with 'kip status', then run the command again", rec.Name, f.NotReadyWait)
		}
		f.Sleep(f.Poll)
	}
}

func (f *nodeFence) requireNotReady(name string) error {
	node, err := f.Cluster.Node(name)
	if err != nil {
		return err
	}
	if node.Ready {
		return fmt.Errorf("%s reports Ready again, so it is not powered off. Run the command again, and kip stops k3s on it over SSH", name)
	}
	return nil
}

func (f *nodeFence) warnDeletionPolicy() {
	policy, err := f.Cluster.DeletionPolicy()
	switch {
	case err != nil:
		say(f.Out, "  ⚠  %v\n", err)
	case policy != "do-nothing":
		say(f.Out, "  ⚠  Longhorn's node-down-pod-deletion-policy is %q, so Longhorn may delete pods of a dead node without a fence. Run 'kip upgrade' to apply kip's setting, do-nothing\n", policy)
	}
}

// stopEntry returns the record's entry for a stopped service, or a new one.
func stopEntry(rec installer.NodeRecord, ns, name string) config.FenceStop {
	for _, s := range rec.Fence.Stopped {
		if s.Namespace == ns && s.StatefulSet == name {
			return s
		}
	}
	return config.FenceStop{Namespace: ns, StatefulSet: name}
}

// view reads the services on node and filters replicas to nodes recorded as
// ready with no fence, so unavailable workers' copies do not count.
func (f *nodeFence) view(node string) (installer.FenceView, error) {
	view, err := f.Cluster.View(node)
	if err != nil {
		return view, err
	}
	records, err := f.Cluster.Records()
	if err != nil {
		return view, err
	}
	view.Replicas = installer.AvailableReplicas(view.Replicas, records, "")
	return view, nil
}

// stopHolders prevents pods outside StatefulSets from restarting with
// single-node volumes on node. It fails migration transfers and suspends Jobs
// before the taint and subsequent pod deletion. Any other existing owner
// blocks the fence.
func (f *nodeFence) stopHolders(node string, holders []installer.PodHolder) error {
	for _, h := range holders {
		switch h.Kind {
		case "":
			say(f.Out, "  ℹ  Nothing starts pod %s again. Kubernetes deletes it after the taint\n", h.Label())
		case installer.HolderDataTransfer:
			if err := f.Cluster.FailTransfer(h.Namespace, h.Name, node+" was fenced while the transfer ran"); err != nil {
				return err
			}
			say(f.Out, "  ✔  Marked data transfer %s/%s failed to prevent its mover pod %s from restarting\n", h.Namespace, h.Name, h.Label())
		case installer.HolderJob:
			if err := f.Cluster.SuspendJob(h.Namespace, h.Name); err != nil {
				return err
			}
			say(f.Out, "  ✔  Suspended Job %s/%s to prevent pod %s from restarting. Resume it once its data is safe\n", h.Namespace, h.Name, h.Label())
		default:
			exists, err := f.Cluster.OwnerExists(h.Namespace, h.APIVersion, h.Kind, h.Name)
			if err != nil {
				return err
			}
			if exists {
				return fmt.Errorf("%s %s owns pod %s, which mounts a single-node volume on %s, and kip cannot stop a %s. Delete %s %s in namespace %s so it does not start the pod again, then run the command again. kip deletes the pod after the taint", h.Kind, h.Name, h.Label(), node, h.Kind, h.Kind, h.Name, h.Namespace)
			}
			say(f.Out, "  ℹ  %s %s, which started pod %s, is gone. Kubernetes deletes the pod after the taint\n", h.Kind, h.Name, h.Label())
		}
	}
	return nil
}

// stopStale stops every service on the node without a current copy of all
// its volumes elsewhere and collects consent to start the stale ones. It
// runs before the taint, so no stale copy is ever started without consent.
// Services receive their stop annotations before pod holders are stopped,
// so migration cleanup honors the fence when it tries to restore a service.
func (f *nodeFence) stopStale(rec *installer.NodeRecord) (installer.FenceView, error) {
	view, err := f.view(rec.Name)
	if err != nil {
		return view, err
	}
	kept := rec.Fence.Stopped[:0:0]
	for _, svc := range view.Services {
		fresh := installer.JudgeFreshness(rec.Name, svc.Volumes, view.Replicas)
		if fresh.Current {
			continue
		}
		if svc.Stop == nil {
			if err := f.Cluster.Stop(svc.Namespace, svc.StatefulSet, installer.FenceStopMark{Node: rec.Name, Reason: fresh.Reason, Volumes: svc.Volumes, Replicas: svc.Replicas}); err != nil {
				return view, err
			}
			say(f.Out, "  ✔  Stopped %s: no other node holds a current copy of its data\n", svc.Label())
		}
		entry := stopEntry(*rec, svc.Namespace, svc.StatefulSet)
		entry.Reason = fresh.Reason
		if fresh.Reason != installer.FenceStopStale {
			entry.Consent = nil
		} else if !installer.SameStamps(entry.Consent, fresh.Stamps) {
			entry.Consent = nil
			ok, err := f.consent(rec.Name, svc, fresh)
			if err != nil {
				return view, err
			}
			if ok {
				entry.Consent = fresh.Stamps
			}
		}
		kept = append(kept, entry)
	}
	rec.Fence.Stopped = kept
	if err := f.Cluster.SaveRecord(*rec); err != nil {
		return view, err
	}
	return view, f.stopHolders(rec.Name, view.Holders)
}

// consent asks whether to start svc on copies older than the node's data.
func (f *nodeFence) consent(node string, svc installer.FenceService, fresh installer.Freshness) (bool, error) {
	if f.AcceptStale {
		return true, nil
	}
	if !f.Interactive {
		return false, nil
	}
	return askStaleConsent(f.Out, f.Confirm, node, svc, fresh)
}

// askStaleConsent shows every copy of svc's volumes with its timestamps and
// asks whether to start svc on the newest of them.
func askStaleConsent(out io.Writer, confirm func(string) (bool, error), node string, svc installer.FenceService, fresh installer.Freshness) (bool, error) {
	say(out, "\n  No copy of %s is known to be current with its data on %s:\n", svc.Label(), node)
	for _, s := range fresh.Stamps {
		say(out, "    %s  %s on %s  healthy at %s, last failed at %s%s\n", s.Volume, s.Name, s.Node, orDash(s.HealthyAt), orDash(s.LastFailedAt), failedNote(s.FailedAt))
	}
	return confirm(fmt.Sprintf("  Start %s on the newest of these copies? Writes made since then are lost. [y/N] ", svc.Label()))
}

func failedNote(failedAt string) string {
	if failedAt == "" {
		return ""
	}
	return ", failed since " + failedAt
}

// nudge deletes what Kubernetes leaves behind on the fenced node: first the
// volume attachments of each remaining pod, then the pod with no grace
// period. Before every deletion it checks again that the node is NotReady
// and tainted, and it never removes finalizers.
func (f *nodeFence) nudge(node string) error {
	pods, err := f.Cluster.PodsWithVolumes(node)
	if err != nil || len(pods) == 0 {
		return err
	}
	say(f.Out, "  ...  Giving Kubernetes %s to delete the pods on %s\n", f.PodGCGrace, node)
	f.Sleep(f.PodGCGrace)
	if pods, err = f.Cluster.PodsWithVolumes(node); err != nil {
		return err
	}
	guard := func() error {
		n, err := f.Cluster.Node(node)
		if err != nil {
			return err
		}
		if !n.Exists || n.Ready || !n.OutOfService {
			return fmt.Errorf("%s changed during the fence (it is Ready, has lost its out-of-service taint or is gone), so kip stopped before deleting anything else on it. Check it with 'kip status'", node)
		}
		return nil
	}
	for _, p := range pods {
		say(f.Out, "  ...  Kubernetes has not deleted pod %s/%s yet. kip detaches its volumes and deletes it\n", p.Namespace, p.Name)
		mine := func() ([]installer.Attachment, error) {
			all, err := f.Cluster.Attachments(node)
			var out []installer.Attachment
			for _, a := range all {
				for _, v := range p.PersistentVolumes {
					if a.PersistentVolume == v {
						out = append(out, a)
					}
				}
			}
			return out, err
		}
		attached, err := mine()
		if err != nil {
			return err
		}
		for _, a := range attached {
			if err := guard(); err != nil {
				return err
			}
			if err := f.Cluster.DeleteAttachment(a.Name); err != nil {
				return err
			}
		}
		deadline := f.Now().Add(f.AttachWait)
		for len(attached) > 0 {
			f.Sleep(f.Poll)
			if attached, err = mine(); err != nil {
				return err
			}
			if len(attached) > 0 && !f.Now().Before(deadline) {
				var names []string
				for _, a := range attached {
					names = append(names, a.Name)
				}
				return fmt.Errorf("the volume attachments %s of pod %s/%s still exist %s after kip deleted them. Run 'kip node fence %s' again, and do not delete the pod with kubectl --force", strings.Join(names, ", "), p.Namespace, p.Name, f.AttachWait, node)
			}
		}
		if err := guard(); err != nil {
			return err
		}
		if err := f.Cluster.DeletePod(p.Namespace, p.Name, p.UID); err != nil {
			return err
		}
	}
	return nil
}

// startConsented starts each service the fence stopped once all its data is
// current elsewhere, or its stale copies are unchanged since consent. The
// consent is in the record before the start, and the entry leaves the record
// after it.
func (f *nodeFence) startConsented(rec *installer.NodeRecord) error {
	view, err := f.view(rec.Name)
	if err != nil {
		return err
	}
	for _, svc := range view.Services {
		if svc.Stop == nil {
			continue
		}
		fresh := installer.JudgeFreshness(rec.Name, svc.Volumes, view.Replicas)
		var consent []config.ReplicaStamp
		for _, s := range rec.Fence.Stopped {
			if s.Namespace == svc.Namespace && s.StatefulSet == svc.StatefulSet {
				consent = s.Consent
			}
		}
		if !fresh.Current && (fresh.Reason != installer.FenceStopStale || !installer.SameStamps(consent, fresh.Stamps)) {
			continue
		}
		if err := f.Cluster.Start(svc.Namespace, svc.StatefulSet); err != nil {
			return err
		}
		kept := rec.Fence.Stopped[:0:0]
		for _, s := range rec.Fence.Stopped {
			if s.Namespace != svc.Namespace || s.StatefulSet != svc.StatefulSet {
				kept = append(kept, s)
			}
		}
		rec.Fence.Stopped = kept
		if err := f.Cluster.SaveRecord(*rec); err != nil {
			return err
		}
	}
	return nil
}

// report waits for the services that may start to run on another node and
// says what happened to each, from the replica state.
func (f *nodeFence) report(rec installer.NodeRecord, view installer.FenceView) error {
	say(f.Out, "\n")
	if len(view.Services) == 0 {
		say(f.Out, "  No Kipper service had a single-node volume on %s.\n\n", rec.Name)
		return nil
	}
	deadline := f.Now().Add(f.Timeout)
	waiting := 0
	for _, svc := range view.Services {
		if stop := stoppedEntry(rec, svc); stop != nil {
			f.reportStopped(rec, svc, *stop)
			continue
		}
		current := installer.JudgeFreshness(rec.Name, svc.Volumes, view.Replicas).Current
		pods := svc.Pods
		if len(pods) == 0 && current && svc.Stop == nil {
			say(f.Out, "  ✔  %s had no pod on %s, so there is none to move. Its data is current on another node\n", svc.Label(), rec.Name)
			continue
		}
		if len(pods) == 0 {
			// A paused service had no pods to track, so use the restored
			// replica count to identify the pods to wait for.
			replicas := max(svc.Replicas, 1)
			if svc.Stop != nil {
				replicas = svc.Stop.Replicas
			}
			for i := int32(0); i < replicas; i++ {
				pods = append(pods, fmt.Sprintf("%s-%d", svc.StatefulSet, i))
			}
		}
		where, err := f.waitRunning(svc.Namespace, pods, rec.Name, deadline)
		if err != nil {
			return err
		}
		if where == "" {
			waiting++
			say(f.Out, "  ⚠  %s is not running on another node yet. Check it with 'kip status'\n", svc.Label())
			continue
		}
		if current {
			say(f.Out, "  ✔  %s runs again on %s, on data current up to when %s stopped reporting\n", svc.Label(), where, rec.Name)
		} else {
			say(f.Out, "  ✔  %s runs again on %s, on older copies of its data, as accepted. Writes made on %s after those copies fell behind are lost\n", svc.Label(), where, rec.Name)
		}
	}
	say(f.Out, "\n  Bring %s back with 'kip node add %s' once its host has rebooted, or remove it with 'kip node remove %s --force'.\n\n", rec.Name, rec.Host, rec.Name)
	if waiting > 0 {
		return fmt.Errorf("%d %s did not run on another node within %s", waiting, plural(waiting, "service", "services"), f.Timeout)
	}
	return nil
}

// restoreAdvice says how to bring back a service from a backup once its only
// copy is lost. Velero skips objects that still exist, so the service and its
// claim are deleted first. File backups require a regular container in a running
// pod to mount the volume, so recovery needs a backup taken while the service ran,
// before the fence stopped it.
func restoreAdvice(ns, name string) string {
	return fmt.Sprintf("delete the service with 'kip service delete %s --delete-data' (using the --project and --environment that correspond to namespace %s), then restore it with 'kip backup restore <backup> --namespace %s' from a backup taken before the fence, while %s ran. A later backup holds none of its files and restores it stopped. To restore it beside the old one instead, use --namespace-mapping %s:<new namespace>", name, ns, ns, name, ns)
}

func stoppedEntry(rec installer.NodeRecord, svc installer.FenceService) *config.FenceStop {
	for _, s := range rec.Fence.Stopped {
		if s.Namespace == svc.Namespace && s.StatefulSet == svc.StatefulSet {
			return &s
		}
	}
	return nil
}

func (f *nodeFence) reportStopped(rec installer.NodeRecord, svc installer.FenceService, stop config.FenceStop) {
	if stop.Reason == installer.FenceStopOnlyCopy {
		say(f.Out, "  ⚠  %s stays stopped: the only copy of its data is on %s. Bring the worker back with 'kip node add %s'. If it is lost, %s\n", svc.Label(), rec.Name, rec.Host, restoreAdvice(svc.Namespace, svc.StatefulSet))
		return
	}
	say(f.Out, "  ⚠  %s stays stopped: its other copies may be older than its data on %s. Bring the worker back with 'kip node add %s', or start it on the older data with 'kip node fence %s --accept-stale-data'\n", svc.Label(), rec.Name, rec.Host, rec.Name)
}

// waitRunning waits until every pod is Ready on a node other than node. It
// returns the last pod's node, or an empty string at the deadline. The pods
// may run on different nodes.
func (f *nodeFence) waitRunning(ns string, pods []string, node string, deadline time.Time) (string, error) {
	for {
		where := ""
		for _, p := range pods {
			on, ready, err := f.Cluster.PodPlacement(ns, p)
			if err != nil {
				return "", err
			}
			if on == "" || on == node || !ready {
				where = ""
				break
			}
			where = on
		}
		if where != "" || !f.Now().Before(deadline) {
			return where, nil
		}
		f.Sleep(f.Poll)
	}
}

// fenceServer runs the fence's cluster operations on the server.
type fenceServer struct {
	master  installer.CommandRunner
	cluster *config.Cluster
}

func (s fenceServer) Records() ([]installer.NodeRecord, error) {
	return installer.ReadNodeRecords(s.master)
}

func (s fenceServer) SaveRecord(rec installer.NodeRecord) error {
	return saveNodeRecord(s.master, s.cluster, rec)
}

func (s fenceServer) Node(name string) (installer.FenceNode, error) {
	return installer.ReadFenceNode(s.master, name)
}

func (s fenceServer) KubeletAnswers(address string) (bool, error) {
	return installer.KubeletAnswers(s.master, address)
}

func (s fenceServer) View(node string) (installer.FenceView, error) {
	return installer.ReadFenceView(s.master, node)
}

func (s fenceServer) Stop(ns, name string, mark installer.FenceStopMark) error {
	return installer.StopForFence(s.master, ns, name, mark)
}

func (s fenceServer) Start(ns, name string) error {
	return installer.StartAfterFence(s.master, ns, name)
}

func (s fenceServer) FailTransfer(ns, name, reason string) error {
	return installer.FailDataTransfer(s.master, ns, name, reason)
}

func (s fenceServer) SuspendJob(ns, name string) error {
	return installer.SuspendJob(s.master, ns, name)
}

func (s fenceServer) OwnerExists(ns, apiVersion, kind, name string) (bool, error) {
	return installer.OwnerExists(s.master, ns, apiVersion, kind, name)
}

func (s fenceServer) Taint(node string) error { return installer.TaintOutOfService(s.master, node) }

func (s fenceServer) DeletionPolicy() (string, error) { return installer.NodeDownPolicy(s.master) }

func (s fenceServer) PodsWithVolumes(node string) ([]installer.FencedPod, error) {
	return installer.PodsWithVolumesOn(s.master, node)
}

func (s fenceServer) Attachments(node string) ([]installer.Attachment, error) {
	return installer.AttachmentsOn(s.master, node)
}

func (s fenceServer) DeleteAttachment(name string) error {
	return installer.DeleteAttachment(s.master, name)
}

func (s fenceServer) DeletePod(ns, name, uid string) error {
	return installer.ForceDeletePod(s.master, ns, name, uid)
}

func (s fenceServer) PodPlacement(ns, name string) (string, bool, error) {
	return installer.PodPlacement(s.master, ns, name)
}

// fenceWorker runs the fence's host steps on the worker.
type fenceWorker struct{ c installer.CommandRunner }

func (w fenceWorker) MachineID() (string, error) { return installer.ReadMachineID(w.c) }
func (w fenceWorker) BootID() (string, error)    { return installer.ReadBootID(w.c) }
func (w fenceWorker) InstallInterlock() error    { return installer.InstallFenceInterlock(w.c) }
func (w fenceWorker) StopAgent() error           { return installer.StopFencedAgent(w.c) }
