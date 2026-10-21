package cmd

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/deployer"
	"github.com/getkipper/kipper/kip/internal/installer"
	"github.com/getkipper/kipper/kip/internal/k8s"
	"github.com/getkipper/kipper/kip/internal/ssh"
)

var nodeRebootCmd = &cobra.Command{
	Use:   "reboot [<node>]",
	Short: "Reboot a node and wait until the cluster is healthy again",
	Long: `Reboots a node to apply a new kernel or pending service restarts listed
by 'kip status'. Without an argument, or with the server's name or address,
it reboots the server.

Apps on a single-server cluster are unavailable during the reboot.
kip takes a backup and reboots the server. It waits for all nodes and the
components checked by 'kip status' to be ready, then waits for previously
ready apps to become ready again and runs the host checks.

For a worker, pass its node name or address. kip takes a backup, then
cordons and drains the worker. Controllers can recreate evicted pods on
other nodes; DaemonSet and mirror pods stay. Draining deletes evicted pods'
emptyDir data. Longhorn copies volumes with their last healthy replica on
the worker to another node before the drain finishes.
If draining fails, kip reports the error, attempts to lift any cordon it
set, and cancels the reboot. Use --drain-timeout to limit the drain wait.
After the reboot, kip lifts any cordon it set and waits for the cluster
and previously ready workloads to be ready again.

On clusters with workers, kip requires attached volumes to be healthy
before rebooting. It checks before confirmation and again after the backup.
Longhorn rebuilds degraded volumes automatically when healthy replicas
and node capacity are available. After a reboot or node outage, allow time
for rebuilding, then run the command again.
If a volume requests more replicas than the cluster has nodes, kip allows
the reboot with a warning when every node holds a healthy copy.
Use --ignore-degraded-volumes to proceed with unhealthy volumes or when
volume health cannot be read. If kip cannot read the node list before a
server reboot, use --skip-backup to proceed without a backup. kip retries
the node list before rebooting and checks volume health if workers are found.
If the node list is still unavailable, it warns and proceeds.`,
	Args:         cobra.MaximumNArgs(1),
	SilenceUsage: true,
	RunE:         runNodeReboot,
}

func init() {
	nodeRebootCmd.Flags().Bool("yes", false, "reboot without asking for confirmation")
	nodeRebootCmd.Flags().Bool("skip-backup", false, "skip the backup and allow a server reboot if the node list is unavailable")
	nodeRebootCmd.Flags().String("ssh-key", "", "path to SSH private key for the host (overrides cluster.ssh_key in config and KIP_SSH_KEY env)")
	nodeRebootCmd.Flags().Duration("timeout", 20*time.Minute, "how long to wait for node, component, and app readiness after reboot")
	nodeRebootCmd.Flags().Duration("drain-timeout", 30*time.Minute, "maximum wait for a worker to drain, including volume copies to other nodes")
	nodeRebootCmd.Flags().Bool("ignore-degraded-volumes", false, "reboot even when an attached volume is unhealthy or its health cannot be read")
	nodeCmd.AddCommand(nodeRebootCmd)
}

// nodeReboot injects host operations and time to test sequencing and failures.
type nodeReboot struct {
	Out              io.Writer
	SSH              ssh.Config
	Impact           string
	Confirm          func(prompt string) (bool, error)
	Prepare          func() (undo func() error, err error)
	Settle           func(within time.Duration) error
	Backup           func() (string, error)
	NoBackups        bool
	BootID           func(ssh.Config) (string, error)
	Reboot           func(ssh.Config) (time.Time, error)
	Healthy          func(within time.Duration, bootID string) (bool, string)
	Report           func(io.Writer, ssh.Config)
	ServingApps      func() ([]appRef, error)
	NotServing       func(apps []appRef, since time.Time, within time.Duration) ([]string, error)
	Sleep            func(time.Duration)
	Now              func() time.Time
	Interval         time.Duration
	Probe            time.Duration
	NotRebootedAfter time.Duration
	Timeout          time.Duration
}

func (r *nodeReboot) run(host string, yes, skipBackup bool) error {
	before, err := r.BootID(r.SSH)
	if err != nil {
		return fmt.Errorf("reaching %s over SSH before the reboot: %w", host, err)
	}

	if !yes {
		ok, err := r.Confirm(fmt.Sprintf("\n  Reboot %s? %s [y/N] ", host, r.Impact))
		if err != nil {
			return err
		}
		if !ok {
			say(r.Out, "  Not rebooted.\n")
			return nil
		}
	}

	if r.NoBackups && !skipBackup {
		say(r.Out, "\n  ℹ  This cluster has no backups, so the reboot proceeds without one\n")
	}
	if !skipBackup && !r.NoBackups {
		say(r.Out, "\n  ...  Backing up before the reboot\n")
		name, err := r.Backup()
		if err != nil {
			return fmt.Errorf("backup before the reboot: %w; reboot canceled. A timed-out backup may still be running. Check 'kip backup list' before using --skip-backup", err)
		}
		say(r.Out, "  ✔  Backup %s completed\n", name)
	}

	var apps []appRef
	if r.ServingApps != nil {
		say(r.Out, "\n  If console-api uses :latest, it may start with a newer image after reboot.\n")
		say(r.Out, "  Run 'kip upgrade --check' to check for required data updates.\n")
		apps, err = r.ServingApps()
		if err != nil {
			say(r.Out, "  ⚠  could not list apps (%v); skipping app readiness checks after reboot\n", err)
		}
	}

	undo := func() error { return nil }
	if r.Prepare != nil {
		u, err := r.Prepare()
		if err != nil {
			return err
		}
		undo = onlyOnce(u)
		defer func() {
			if err := undo(); err != nil {
				say(r.Out, "  ⚠  %v\n", err)
			}
		}()
	}

	say(r.Out, "\n  ...  Rebooting %s\n", host)
	scheduled, err := r.Reboot(r.SSH)
	if err != nil {
		return fmt.Errorf("rebooting %s: %w", host, err)
	}
	start := r.Now()
	deadline := start.Add(r.Timeout)
	remaining := func() time.Duration { return deadline.Sub(r.Now()) }

	say(r.Out, "  ...  Waiting for %s to start again\n", host)
	var after string
	for {
		r.Sleep(min(r.Interval, max(remaining(), 0)))
		if remaining() <= 0 {
			return fmt.Errorf("%s did not come back within %s. Some servers boot slowly; check it with 'kip status', and through your provider's console if it stays unreachable", host, r.Timeout)
		}
		id, err := r.BootID(r.SSH)
		if err == nil && id != before {
			after = id
			break
		}
		if err == nil && r.Now().Sub(start) >= r.NotRebootedAfter {
			return fmt.Errorf("%s did not reboot: its boot ID is unchanged %s after the reboot was scheduled. Check 'systemctl list-jobs' and the system journal on the host for the cause", host, r.NotRebootedAfter)
		}
	}
	say(r.Out, "  ✔  %s is back after %s\n", host, r.Now().Sub(start).Round(time.Second))
	// Lift our cordon before waiting: evicted components may need this
	// node's capacity to run again.
	if err := undo(); err != nil {
		return err
	}

	say(r.Out, "  ...  Waiting for every node and component to be ready\n")
	// Require consecutive healthy checks to reduce transient readiness results.
	healthy := 0
	for healthy < 2 {
		if remaining() <= 0 {
			say(r.Out, "\n")
			r.Report(r.Out, r.SSH)
			return fmt.Errorf("not ready within %s of the reboot. Check it with 'kip status'", r.Timeout)
		}
		ok, notReady := r.Healthy(min(r.Probe, remaining()), after)
		if ok {
			healthy++
		} else {
			healthy = 0
		}
		if !ok && remaining() <= r.Interval {
			say(r.Out, "\n")
			r.Report(r.Out, r.SSH)
			return fmt.Errorf("not ready within %s of the reboot: %s. Check it with 'kip status'", r.Timeout, notReady)
		}
		if healthy < 2 {
			r.Sleep(r.Interval)
		}
	}
	say(r.Out, "  ✔  Every node and component is ready\n")

	if r.Settle != nil {
		if err := r.Settle(max(remaining(), 0)); err != nil {
			return err
		}
	}

	if len(apps) > 0 {
		say(r.Out, "  ...  Waiting for %d %s to become ready again\n", len(apps), plural(len(apps), "app", "apps"))
		for {
			waiting, err := r.NotServing(apps, scheduled, min(r.Probe, max(remaining(), 0)))
			if err == nil && len(waiting) == 0 {
				break
			}
			if remaining() <= r.Interval {
				say(r.Out, "\n")
				r.Report(r.Out, r.SSH)
				if err != nil {
					return fmt.Errorf("could not confirm app readiness within %s after reboot: %w. Check progress with 'kip app list'", r.Timeout, err)
				}
				return fmt.Errorf("apps still waiting for readiness %s after reboot: %s. Check progress with 'kip app list'", r.Timeout, strings.Join(waiting, ", "))
			}
			r.Sleep(r.Interval)
		}
		say(r.Out, "  ✔  All previously ready apps are ready again\n")
	}
	say(r.Out, "\n")
	r.Report(r.Out, r.SSH)
	return nil
}

func runNodeReboot(cmd *cobra.Command, args []string) error {
	cluster, client, err := loadCurrentCluster()
	if err != nil {
		return err
	}
	if cluster.Host == "" {
		return fmt.Errorf("cluster %s has no host recorded, so kip cannot reach its server", cluster.Name)
	}
	yes, _ := cmd.Flags().GetBool("yes")
	skipBackup, _ := cmd.Flags().GetBool("skip-backup")
	keyFlag, _ := cmd.Flags().GetString("ssh-key")
	timeout, _ := cmd.Flags().GetDuration("timeout")
	ignoreVolumes, _ := cmd.Flags().GetBool("ignore-degraded-volumes")

	sshCfg, err := rebootSSHConfig(cluster, keyFlag)
	if err != nil {
		return err
	}
	r := &nodeReboot{
		Out:     os.Stdout,
		SSH:     sshCfg,
		Impact:  "Its apps will be unavailable during the reboot.",
		Confirm: confirmReboot,
		Backup: func() (string, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()
			return backupBeforeReboot(ctx, cluster, client, 10*time.Second)
		},
		NoBackups: !backupsConfiguredOn(context.Background(), client.Dynamic()),
		BootID: func(cfg ssh.Config) (string, error) {
			return onHost(cfg, func(c *ssh.Client) (string, error) { return installer.ReadBootID(c) })
		},
		Reboot: func(cfg ssh.Config) (time.Time, error) {
			out, err := onHost(cfg, func(c *ssh.Client) (string, error) {
				return c.Run(rebootCommand(time.Now()))
			})
			if err != nil {
				return time.Time{}, err
			}
			return scheduledAt(out)
		},
		Healthy: func(within time.Duration, bootID string) (bool, string) {
			return clusterReady(client, within, bootID)
		},
		Report: func(out io.Writer, cfg ssh.Config) { reportHostAfterReboot(out, cfg, cluster, client) },
		ServingApps: func() ([]appRef, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			return servingApps(ctx, client.Dynamic())
		},
		NotServing: func(apps []appRef, since time.Time, within time.Duration) ([]string, error) {
			ctx, cancel := context.WithTimeout(context.Background(), within)
			defer cancel()
			pods, err := client.Clientset().CoreV1().Pods("").List(ctx, metav1.ListOptions{LabelSelector: "app"})
			if err != nil {
				return nil, err
			}
			// Ready worker pods count even if they became ready before the server reboot.
			workers, err := client.Clientset().CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: "!" + installer.ServerNodeLabel})
			if err != nil {
				return nil, err
			}
			unaffected := map[string]bool{}
			for _, n := range workers.Items {
				unaffected[n.Name] = true
			}
			return appsNotServingSince(apps, pods.Items, since, unaffected), nil
		},
		Sleep:            time.Sleep,
		Now:              time.Now,
		Interval:         10 * time.Second,
		Probe:            30 * time.Second,
		NotRebootedAfter: 3 * time.Minute,
		Timeout:          timeout,
	}
	drainTimeout, _ := cmd.Flags().GetDuration("drain-timeout")
	// Allow the worker drain to reach its own timeout before SSH times out.
	masterSSH := sshCfg
	masterSSH.CommandTimeout = max(drainTimeout, 0) + 2*time.Minute
	master, err := ssh.Dial(masterSSH)
	if err != nil {
		return fmt.Errorf("connecting to the server %s: %w", cluster.Host, err)
	}
	defer func() { _ = master.Close() }()
	rebootServer := func() error {
		if err := planServerReboot(os.Stdout, master, r, cluster.Host, yes, skipBackup, ignoreVolumes, recordedWorkers(cluster.Nodes)); err != nil {
			return err
		}
		return r.run(cluster.Host, yes, skipBackup)
	}
	// Resolve the server locally so API outages do not prevent a reboot.
	if rebootsTheServer(args, cluster.Host) {
		return rebootServer()
	}

	records, err := recordNodes(master, cluster.Name, cluster.Host)
	if err != nil {
		return fmt.Errorf("reading the node records: %w. To reboot the server without looking up node records, run 'kip node reboot' with no argument", err)
	}
	refreshDeviceNodes(os.Stdout, cluster, master, records)
	rec, err := findNodeRecord(records, args[0])
	if err != nil {
		return err
	}
	if err := removalBlocksReboot(rec); err != nil {
		return err
	}
	if rec.Role == installer.NodeRoleServer {
		return rebootServer()
	}

	if drainTimeout < time.Second {
		return fmt.Errorf("--drain-timeout must be at least 1s, got %s", drainTimeout)
	}
	if r.SSH, err = retargetSSH(r.SSH, cluster, rec.Host); err != nil {
		return err
	}
	id, err := onHost(r.SSH, func(c *ssh.Client) (string, error) { return installer.ReadMachineID(c) })
	if err != nil {
		return fmt.Errorf("reaching %s over SSH before the reboot: %w", rec.Host, err)
	}
	if id != rec.MachineID {
		return fmt.Errorf("%s no longer answers as node %s (machine id %q, recorded %s); check the address before rebooting it", rec.Host, rec.Name, id, rec.MachineID)
	}
	w := &workerReboot{Out: os.Stdout, Master: master, Node: rec.Name, DrainTimeout: drainTimeout, IgnoreVolumes: ignoreVolumes}
	if err := w.preview(); err != nil {
		return err
	}
	r.Impact = "Its pods are drained first; evicted pods lose their emptyDir data."
	r.ServingApps = nil
	r.Prepare = w.prepare
	r.Settle = w.settle
	r.Report = func(out io.Writer, cfg ssh.Config) { reportWorkerAfterReboot(out, cfg, rec.Name, master) }
	return r.run(rec.Name, yes, skipBackup)
}

func findNodeRecord(records []installer.NodeRecord, target string) (installer.NodeRecord, error) {
	var known []string
	for _, r := range records {
		if r.Name == target || r.Host == target {
			return r, nil
		}
		known = append(known, r.Name)
	}
	return installer.NodeRecord{}, fmt.Errorf("no node %q in this cluster; recorded nodes: %s", target, strings.Join(known, ", "))
}

type workerReboot struct {
	Out           io.Writer
	Master        installer.CommandRunner
	Node          string
	DrainTimeout  time.Duration
	IgnoreVolumes bool
	ready         []string
}

// preview checks drain prerequisites and shows affected volumes before confirmation.
func (w *workerReboot) preview() error {
	if err := installer.RequireEvictionDrainPolicy(w.Master); err != nil {
		return err
	}
	volumes, err := installer.VolumesOnNode(w.Master, w.Node)
	if err != nil {
		return err
	}
	if err := w.lastCopiesCanMove(volumes); err != nil {
		return err
	}
	reportNodeVolumes(w.Out, w.Node, volumes)
	return volumeGate(w.Out, w.Master, w.IgnoreVolumes)
}

// lastCopiesCanMove rejects a drain when the preflight check finds no destination
// for a volume's last healthy copy. This avoids waiting for the drain timeout
// after other pods may have been evicted. Read failures produce a warning and
// allow the drain to proceed, since it still waits for Longhorn to copy replicas.
func (w *workerReboot) lastCopiesCanMove(volumes []installer.NodeVolume) error {
	var last []installer.NodeVolume
	for _, v := range volumes {
		if v.LastCopy() {
			last = append(last, v)
		}
	}
	blocked, err := installer.LastCopiesWithoutRoom(w.Master, w.Node, last)
	if err != nil {
		say(w.Out, "  ⚠  could not check where Longhorn can copy the volumes on %s (%v). The drain still waits for those copies.\n", w.Node, err)
		return nil
	}
	if len(blocked) == 0 {
		return nil
	}
	return fmt.Errorf("%s holds the only healthy copy of %s, and no other node can take a copy:\n    %s\n  Without a destination for the copy, the drain would time out and cancel the reboot of %s. Free disk space or make a disk schedulable on a node the volume may use, then run this command again",
		w.Node, plural(len(blocked), "a volume", "volumes"), strings.Join(blocked, "\n    "), w.Node)
}

// prepare returns cleanup for the cordon it adds, preserving an existing cordon.
// If draining or replica verification fails, it attempts cleanup before returning.
func (w *workerReboot) prepare() (func() error, error) {
	ready, err := installer.ReadyWorkloads(w.Master)
	if err != nil {
		return nil, fmt.Errorf("listing the ready workloads before the drain: %w", err)
	}
	w.ready = ready
	cordoned, err := installer.NodeCordoned(w.Master, w.Node)
	if err != nil {
		return nil, err
	}
	// Recheck just before cordoning: health may change during confirmation and backup.
	if err := volumeGate(w.Out, w.Master, w.IgnoreVolumes); err != nil {
		return nil, err
	}
	if volumes, err := installer.VolumesOnNode(w.Master, w.Node); err != nil {
		say(w.Out, "  ⚠  could not check where Longhorn can copy the volumes on %s (%v). The drain still waits for those copies.\n", w.Node, err)
	} else if err := w.lastCopiesCanMove(volumes); err != nil {
		return nil, err
	}

	undo := func() error { return nil }
	if cordoned {
		say(w.Out, "  ℹ  %s is already cordoned and stays cordoned after the reboot\n", w.Node)
	} else {
		if err := installer.CordonNode(w.Master, w.Node); err != nil {
			return nil, err
		}
		undo = func() error {
			if err := installer.UncordonNode(w.Master, w.Node); err != nil {
				return fmt.Errorf("%w; %s is still cordoned", err, w.Node)
			}
			say(w.Out, "  ✔  %s is uncordoned\n", w.Node)
			return nil
		}
	}
	fail := func(cause error) (func() error, error) {
		if err := undo(); err != nil {
			return nil, fmt.Errorf("%w; %s was not rebooted, and uncordoning it failed: %v", cause, w.Node, err)
		}
		return nil, fmt.Errorf("%w; %s was not rebooted", cause, w.Node)
	}

	say(w.Out, "  ...  Draining %s (up to %s)\n", w.Node, w.DrainTimeout)
	if err := installer.DrainNode(w.Master, w.Node, w.DrainTimeout); err != nil {
		return fail(err)
	}
	left, err := installer.VolumesOnNode(w.Master, w.Node)
	if err != nil {
		return fail(err)
	}
	var last []string
	for _, v := range left {
		if v.LastCopy() {
			last = append(last, v.Label())
		}
	}
	if len(last) > 0 {
		return fail(fmt.Errorf("the drain finished, but %s still %s its only healthy copy on %s", strings.Join(last, ", "), plural(len(last), "has", "have"), w.Node))
	}
	say(w.Out, "  ✔  %s is drained; every volume checked has a healthy copy on another node\n", w.Node)
	return undo, nil
}

func (w *workerReboot) settle(within time.Duration) error {
	say(w.Out, "  ...  Waiting for %d %s that %s ready before the drain\n", len(w.ready), plural(len(w.ready), "workload", "workloads"), plural(len(w.ready), "was", "were"))
	if err := installer.WaitForWorkloadsReady(w.Master, w.ready, within); err != nil {
		return fmt.Errorf("%w. Check it with 'kip status'", err)
	}
	say(w.Out, "  ✔  Every workload that was ready before is ready again\n")
	reportUnhealthyVolumes(w.Out, w.Master)
	return nil
}

type workerCount int

const (
	singleNode workerCount = iota
	withWorkers
	unreadableNodes
)

// countWorkers uses live nodes because local records may be stale.
func countWorkers(master installer.CommandRunner) (workerCount, error) {
	nodes, err := installer.NodeCount(master)
	switch {
	case err != nil:
		return unreadableNodes, err
	case nodes > 1:
		return withWorkers, nil
	}
	return singleNode, nil
}

// planServerReboot checks volumes on clusters with workers before confirmation
// and again before rebooting, since health can change during the backup.
// If the node list is unavailable, --skip-backup permits proceeding; the final
// check retries the list and checks volumes if workers are found.
func planServerReboot(out io.Writer, master installer.CommandRunner, r *nodeReboot, host string, yes, skipBackup, ignore bool, recorded int) error {
	noUndo := func() error { return nil }
	workers, err := countWorkers(master)
	switch workers {
	case singleNode:
		return nil
	case withWorkers:
		gate := func() error { return volumeGate(out, master, ignore) }
		if err := gate(); err != nil {
			return err
		}
		r.Prepare = func() (func() error, error) { return noUndo, gate() }
		return nil
	}
	if !skipBackup {
		return fmt.Errorf("kip cannot read the cluster's node list from %s (%v). The usual cause is that the Kubernetes API is not answering, and rebooting the server is the usual repair. To reboot without a backup, run again with --skip-backup. kip will retry the node list before rebooting and check volume health if workers are found. If the list is still unavailable, it will warn and proceed", host, err)
	}
	if recorded > 0 {
		say(out, "  ⚠  kip's local node records list %d worker(s); these records may be out of date. Volume replicas on this server will be unavailable during the reboot.\n", recorded)
	}
	r.Impact = "Apps on this server will be unavailable during the reboot. kip cannot read the cluster's node list, so no backup is taken with --skip-backup. kip will retry the node list and check volume health if workers are found; otherwise it will proceed. Volume replicas on this server will be unavailable during the reboot."
	if yes {
		say(out, "  ⚠  kip cannot read the cluster's node list from %s (%v); rebooting without a backup, node count or volume health check if the node list remains unavailable\n", host, err)
	}
	r.Prepare = func() (func() error, error) {
		again, err := countWorkers(master)
		switch again {
		case withWorkers:
			return noUndo, volumeGate(out, master, ignore)
		case unreadableNodes:
			say(out, "  ⚠  the node list still cannot be read (%v); proceeding with --skip-backup\n", err)
		}
		return noUndo, nil
	}
	return nil
}

func rebootsTheServer(args []string, host string) bool {
	return len(args) == 0 || args[0] == host
}

func recordedWorkers(records []config.NodeRecord) int {
	n := 0
	for _, rec := range records {
		if rec.Role == installer.NodeRoleWorker {
			n++
		}
	}
	return n
}

// volumeGate checks attached volumes before rebooting. To allow small clusters
// to reboot, it accepts excess requested replicas if every node has a healthy copy.
func volumeGate(out io.Writer, master installer.CommandRunner, ignore bool) error {
	names, err := installer.NodeNames(master)
	var volumes []installer.VolumeHealth
	if err == nil {
		volumes, err = installer.LonghornVolumes(master)
	}
	nodes := len(names)
	if err != nil {
		if ignore {
			say(out, "  ⚠  could not read volume health (%v); volume health not checked (--ignore-degraded-volumes)\n", err)
			return nil
		}
		return fmt.Errorf("checking volume health before the reboot: %w. Use --ignore-degraded-volumes to reboot without this check", err)
	}
	var unhealthy []string
	var copies map[string]map[string]bool
	for _, v := range volumes {
		if !v.Unhealthy() {
			continue
		}
		if v.Replicas > nodes {
			if copies == nil {
				if copies, err = installer.HealthyReplicaNodes(master); err != nil {
					if ignore {
						say(out, "  ⚠  could not read volume replicas (%v); volume health not checked (--ignore-degraded-volumes)\n", err)
						return nil
					}
					return fmt.Errorf("checking volume replicas before the reboot: %w. Use --ignore-degraded-volumes to reboot without this check", err)
				}
			}
			if onEveryNode(copies[v.Name], names) {
				say(out, "  ⚠  %s wants %d replicas but the cluster has %d nodes, so it stays degraded until a node is added for each missing replica or the requested replica count is reduced. Every node holds a healthy copy; continuing\n", v.Label(), v.Replicas, nodes)
				continue
			}
		}
		unhealthy = append(unhealthy, volumeHealthLabel(v))
	}
	if len(unhealthy) == 0 {
		return nil
	}
	if ignore {
		say(out, "  ⚠  proceeding with unhealthy volumes: %s (--ignore-degraded-volumes)\n", strings.Join(unhealthy, ", "))
		return nil
	}
	return fmt.Errorf("%s %s not healthy; rebooting now can further reduce availability. Longhorn rebuilds degraded volumes on its own when healthy replicas and node capacity are available. Wait and run this command again. If recovery stalls, check node readiness and disk space. Use --ignore-degraded-volumes to accept the risk and reboot anyway", strings.Join(unhealthy, ", "), plural(len(unhealthy), "is", "are"))
}

func onEveryNode(holders map[string]bool, nodes []string) bool {
	for _, n := range nodes {
		if !holders[n] {
			return false
		}
	}
	return true
}

func reportUnhealthyVolumes(out io.Writer, master installer.CommandRunner) {
	volumes, err := installer.LonghornVolumes(master)
	if err != nil {
		say(out, "  ⚠  could not check volume health: %v\n", err)
		return
	}
	var unhealthy []string
	for _, v := range volumes {
		if v.Unhealthy() {
			unhealthy = append(unhealthy, volumeHealthLabel(v))
		}
	}
	if len(unhealthy) > 0 {
		say(out, "  ℹ  Attached volumes not healthy: %s. Longhorn may still be rebuilding after the reboot. A volume requesting more replicas than the cluster has nodes does not block the next reboot while every node holds a healthy copy. Otherwise, wait for recovery before rebooting again. If recovery stalls, check node readiness and disk space\n", strings.Join(unhealthy, ", "))
	}
}

func volumeHealthLabel(v installer.VolumeHealth) string {
	robustness := v.Robustness
	if robustness == "" {
		robustness = "unknown"
	}
	return fmt.Sprintf("%s (%s, %d %s)", v.Label(), robustness, v.Replicas, plural(v.Replicas, "replica", "replicas"))
}

func reportNodeVolumes(out io.Writer, node string, volumes []installer.NodeVolume) {
	if len(volumes) == 0 {
		say(out, "\n  No healthy volume replicas found on %s.\n", node)
		return
	}
	say(out, "\n  Volumes with healthy replicas on %s:\n", node)
	for _, v := range volumes {
		if v.LastCopy() {
			say(out, "    %s: all healthy copies are here; Longhorn copies the volume to another node during the drain\n", v.Label())
		} else {
			say(out, "    %s: %d healthy %s on other nodes\n", v.Label(), v.Elsewhere, plural(v.Elsewhere, "copy", "copies"))
		}
	}
}

func reportWorkerAfterReboot(out io.Writer, cfg ssh.Config, node string, server installer.CommandRunner) {
	c, err := ssh.Dial(cfg)
	if err != nil {
		say(out, "  Pending restarts:\n    ⚠  not checked (could not reach the host over SSH: %v)\n\n", err)
		return
	}
	defer func() { _ = c.Close() }()
	say(out, "  Pending restarts:\n")
	reportPendingRestarts(out, hostTarget{runner: c, reboot: "kip node reboot " + node})
	say(out, "\n")
	reportAccessGuard(out, server, c)
}

// rebootSSHConfig limits command duration because a booting server can accept
// SSH connections before it can complete commands.
func rebootSSHConfig(cluster *config.Cluster, keyFlag string) (ssh.Config, error) {
	cfg, err := hostSSHConfig(cluster, keyFlag)
	cfg.CommandTimeout = 30 * time.Second
	return cfg, err
}

// onlyOnce attempts cleanup once, including when cleanup fails.
func onlyOnce(fn func() error) func() error {
	done := false
	return func() error {
		if done {
			return nil
		}
		done = true
		return fn()
	}
}

func onHost(cfg ssh.Config, fn func(*ssh.Client) (string, error)) (string, error) {
	c, err := ssh.Dial(cfg)
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()
	return fn(c)
}

func confirmReboot(prompt string) (bool, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return false, fmt.Errorf("stdin is not a terminal. Pass --yes to reboot without confirmation")
	}
	fmt.Print(prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return false, fmt.Errorf("reading confirmation: %w", err)
	}
	return parseYesNo(line), nil
}

// backupBeforeReboot uses the defaults from 'kip backup create' and waits for completion.
func backupBeforeReboot(ctx context.Context, cluster *config.Cluster, client *k8s.Client, interval time.Duration) (string, error) {
	name := "before-reboot-" + time.Now().UTC().Format("20060102-150405")
	backups := client.Dynamic().Resource(backupGVR).Namespace("velero")
	if _, err := backups.Create(ctx, buildBackupCR(name, "", "", "168h", false, cluster), metav1.CreateOptions{}); err != nil {
		return "", fmt.Errorf("creating backup %s: %w", name, err)
	}
	phase := func(ctx context.Context) (string, error) {
		current, err := backups.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		p, _, _ := unstructured.NestedString(current.Object, "status", "phase")
		return p, nil
	}
	if err := waitForBackupPhase(ctx, name, phase, interval); err != nil {
		return "", err
	}
	return name, nil
}

// waitForBackupPhase retries read errors until ctx ends or Velero reports a
// terminal phase. On timeout, it includes the most recent read error, if any.
func waitForBackupPhase(ctx context.Context, name string, phase func(context.Context) (string, error), interval time.Duration) error {
	var lastErr error
	for {
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return fmt.Errorf("backup %s did not complete in time (last read failed: %v); see 'kip backup list'", name, lastErr)
			}
			return fmt.Errorf("backup %s did not complete in time; see 'kip backup list'", name)
		case <-time.After(interval):
		}
		p, err := phase(ctx)
		if err != nil {
			lastErr = err
			continue
		}
		switch p {
		case "Completed":
			return nil
		case "PartiallyFailed", "Failed", "FailedValidation":
			return fmt.Errorf("backup %s ended %s; see 'kip backup list'", name, p)
		}
	}
}

// clusterReady uses the node and component health checks from 'kip status'.
func clusterReady(client *k8s.Client, within time.Duration, bootID string) (bool, string) {
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	nodes, err := client.ListNodes(ctx)
	if err != nil {
		return false, "the API server is not answering yet"
	}
	health, err := client.ClusterHealth(ctx)
	if err != nil {
		return false, "component health could not be read yet"
	}
	return readyAfterBoot(nodes, health, bootID)
}

// readyAfterBoot requires the new boot ID before accepting cluster readiness.
func readyAfterBoot(nodes []k8s.NodeInfo, health []k8s.ComponentStatus, bootID string) (bool, string) {
	rebooted := false
	for _, n := range nodes {
		rebooted = rebooted || n.BootID == bootID
	}
	if !rebooted {
		return false, "the kubelet has not reported the new boot yet"
	}
	var notReady []string
	for _, n := range nodes {
		if n.Status != "Ready" {
			notReady = append(notReady, "node "+n.Name+" "+n.Status)
		}
	}
	for _, c := range health {
		if !c.Healthy {
			notReady = append(notReady, c.Name+" "+c.Message)
		}
	}
	return len(notReady) == 0, strings.Join(notReady, ", ")
}

func reportHostAfterReboot(out io.Writer, sshConfig ssh.Config, cluster *config.Cluster, client *k8s.Client) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	nodes, err := client.ListNodes(ctx)
	if err != nil {
		// SSH host checks can proceed even if the API request fails.
		say(out, "  ⚠  could not list nodes: %v\n\n", err)
	}
	records, recordsErr := nodeRecordsFromAPI(ctx, client.Clientset(), cluster)
	reportRecordFallback(out, recordsErr)
	checkHostWith(out, sshConfig, cluster, nodes, records)
}

// rebootCommand delays reboot so SSH can return the scheduling result.
// The timestamp avoids reusing units from attempts made in earlier seconds.
func rebootCommand(now time.Time) string {
	return fmt.Sprintf("echo %s$(date +%%s) && systemd-run --on-active=3 --unit=kip-node-reboot-%d systemctl reboot", serverTimeMarker, now.Unix())
}

const serverTimeMarker = "kip-server-time="

// scheduledAt extracts the marked server timestamp from combined SSH output.
// Using server time keeps pod readiness comparisons independent of the local clock.
func scheduledAt(out string) (time.Time, error) {
	for _, line := range strings.Split(out, "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), serverTimeMarker); ok {
			secs, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return time.Time{}, fmt.Errorf("reading the server time from %q: %w", line, err)
			}
			return time.Unix(secs, 0), nil
		}
	}
	return time.Time{}, fmt.Errorf("the server time is missing from the reboot output %q", strings.TrimSpace(out))
}

type appRef struct{ Namespace, Name string }

func (a appRef) String() string { return a.Namespace + "/" + a.Name }

// servingApps selects apps to wait for using their pre-reboot readiness.
func servingApps(ctx context.Context, dyn dynamic.Interface) ([]appRef, error) {
	list, err := dyn.Resource(deployer.AppGVR).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var apps []appRef
	for _, app := range list.Items {
		if ready, _, _ := unstructured.NestedInt64(app.Object, "status", "readyReplicas"); ready > 0 {
			apps = append(apps, appRef{Namespace: app.GetNamespace(), Name: app.GetName()})
		}
	}
	return apps, nil
}

// appsNotServingSince returns apps lacking a ready, non-terminating pod.
// To exclude stale status, readiness must have changed after since, the time the
// reboot was scheduled on the server's clock. Current readiness suffices on unaffected nodes.
func appsNotServingSince(apps []appRef, pods []corev1.Pod, since time.Time, unaffected map[string]bool) []string {
	serving := map[appRef]bool{}
	for _, pod := range pods {
		if pod.DeletionTimestamp != nil {
			continue
		}
		for _, c := range pod.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue && (unaffected[pod.Spec.NodeName] || c.LastTransitionTime.After(since)) {
				serving[appRef{Namespace: pod.Namespace, Name: pod.Labels["app"]}] = true
			}
		}
	}
	var waiting []string
	for _, app := range apps {
		if !serving[app] {
			waiting = append(waiting, app.String())
		}
	}
	return waiting
}
