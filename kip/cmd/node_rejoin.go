package cmd

import (
	"fmt"
	"io"
	"os"
	"time"

	"golang.org/x/term"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/installer"
)

// rejoinHost is what a rejoin does on the fenced worker.
type rejoinHost interface {
	fenceHost
	VerifyInterlock() error
	Quiescent() error
	Reboot() error
	RemoveInterlock() error
	StartAgent() error
	Runner() installer.CommandRunner
}

// rejoinCluster is what a rejoin does through the server.
type rejoinCluster interface {
	Records() ([]installer.NodeRecord, error)
	SaveRecord(installer.NodeRecord) error
	Node(name string) (installer.FenceNode, error)
	Cordon(name string) error
	RemoveTaint(name string) error
	View(node string) (installer.FenceView, error)
	Start(ns, name string) error
}

// nodeRejoin brings a fenced worker back. It never runs the normal join,
// whose agent install would restart k3s under the interlock.
type nodeRejoin struct {
	Out       io.Writer
	Cluster   rejoinCluster
	Host      rejoinHost
	Reconnect func() (rejoinHost, error)
	// Checks runs the join's storage and node checks and uncordons the node.
	Checks      func(node string, worker installer.CommandRunner) error
	Confirm     func(prompt string) (bool, error)
	Interactive bool
	AcceptStale bool
	Now         func() time.Time
	Sleep       func(time.Duration)
	Poll        time.Duration
	BootWait    time.Duration
	ReadyWait   time.Duration
}

// fenceBlocksJoin refuses a join of a worker whose fence is unfinished, or
// whose forced removal started from a fence.
func fenceBlocksJoin(rec installer.NodeRecord) error {
	switch {
	case rec.State == installer.NodeStateFencing:
		return fmt.Errorf("the fence of %s has not finished. Finish it with 'kip node fence %s', then add the host again", rec.Name, rec.Name)
	case installer.RemovalStarted(rec.State) && rec.Fence != nil:
		return fmt.Errorf("%s was fenced and is being removed, so it cannot be kept. Finish the removal with 'kip node remove %s --force', then add the host again", rec.Name, rec.Name)
	}
	return nil
}

// replacedFencedHost rejects a different machine at a fenced worker's address.
// Removing the old worker first clears the Node and node password that would
// otherwise prevent its replacement from joining k3s.
func replacedFencedHost(records []installer.NodeRecord, host, machineID string) error {
	for _, r := range records {
		if r.Fence != nil && r.Host == host && r.MachineID != machineID {
			return fmt.Errorf("%s is recorded as the fenced worker %s, but now answers with another machine ID. If the host was replaced, remove %s with 'kip node remove %s --force' first, then add the host", host, r.Name, r.Name, r.Name)
		}
	}
	return nil
}

// rejoinMoves reports whether ip differs from published, the worker's external
// address. In public mode, an empty published address falls back to the recorded
// peer address. An unknown or unchanged address returns false with no error.
// A move is allowed in private mode because peers use the private address.
// In public mode, peers use the old address, so a move returns an error.
func rejoinMoves(rec installer.NodeRecord, ip, published string) (bool, error) {
	old := published
	if old == "" && rec.Mode != installer.NetworkModePrivate {
		old = rec.PeerAddress
	}
	if old == "" || old == ip {
		return false, nil
	}
	if rec.Mode != installer.NetworkModePrivate {
		return false, fmt.Errorf("%s answers at %s, but the other nodes reach it at %s in public mode. Restore that address on the host, for example by reattaching its static IP, and run 'kip node add %s'. Otherwise remove the worker with 'kip node remove %s --force' and add the host again", rec.Name, ip, old, old, rec.Name)
	}
	return true, nil
}

// recordedPeers returns the server's and existing worker's recorded addresses
// for private network fallback. Both are empty for a new host or when the
// server's mode is not private.
func recordedPeers(server installer.NodeRecord, existing *installer.NodeRecord) (string, string) {
	if existing == nil || server.Mode != installer.NetworkModePrivate {
		return "", ""
	}
	return server.PeerAddress, existing.PeerAddress
}

// clearsLeftoverInterlock reports whether kip node add may remove a fence
// interlock it finds on the host. Only a record without a fence allows it:
// the interlock is then left from a membership a forced removal ended, also
// when an earlier attempt of this add already saved its joining record.
func clearsLeftoverInterlock(existing *installer.NodeRecord) bool {
	return existing == nil || existing.Fence == nil
}

func (r *nodeRejoin) run(rec installer.NodeRecord) error {
	node, err := r.Cluster.Node(rec.Name)
	if err != nil {
		return err
	}
	if !node.Exists {
		return fmt.Errorf("the cluster has no node %s any more. Remove it with 'kip node remove %s --force', then add the host again", rec.Name, rec.Name)
	}
	if node.OutOfService {
		if err := r.stopAndReboot(&rec); err != nil {
			return err
		}
		if err := r.release(rec); err != nil {
			return err
		}
	} else {
		// An earlier rejoin released the node, which may already accept
		// pods, so it is cordoned again before the checks.
		say(r.Out, "  ℹ  %s is no longer tainted out of service, so kip finishes its rejoin\n", rec.Name)
		if err := r.Cluster.Cordon(rec.Name); err != nil {
			return err
		}
	}
	say(r.Out, "  ...  Checking %s with a test pod\n", rec.Name)
	if err := r.Checks(rec.Name, r.Host.Runner()); err != nil {
		return err
	}
	if err := r.startServices(rec); err != nil {
		return err
	}
	rec.State = installer.NodeStateReady
	rec.Fence = nil
	if err := r.Cluster.SaveRecord(rec); err != nil {
		return err
	}
	say(r.Out, "\n  ✔  %s rejoined the cluster\n\n", rec.Name)
	return nil
}

// rebooted reports whether bootID differs from the boot the fence saw, or from
// the boot an earlier rejoin rebooted.
func rebooted(f *config.NodeFence, bootID string) bool {
	return (f.BootID != installer.BootIDUnknown && bootID != f.BootID) ||
		(f.RebootCheckpoint != "" && bootID != f.RebootCheckpoint)
}

// stopAndReboot stops k3s under the interlock and reboots unless the host has
// booted since the fence. It verifies the interlock and host quiescence on both
// paths. Saving a checkpoint before reboot lets an interrupted rejoin recognize
// the new boot.
func (r *nodeRejoin) stopAndReboot(rec *installer.NodeRecord) error {
	say(r.Out, "  ...  Making sure k3s stays stopped on %s\n", rec.Name)
	if err := r.Host.InstallInterlock(); err != nil {
		return err
	}
	if err := r.Host.StopAgent(); err != nil {
		return err
	}
	current, err := r.Host.BootID()
	if err != nil {
		return err
	}
	if rebooted(rec.Fence, current) {
		say(r.Out, "  ✔  %s has booted since the fence\n", rec.Name)
		return r.checkStopped()
	}
	rec.Fence.RebootCheckpoint = current
	if err := r.Cluster.SaveRecord(*rec); err != nil {
		return err
	}
	say(r.Out, "  ...  Rebooting %s, so it rejoins with a fresh boot\n", rec.Name)
	if err := r.Host.Reboot(); err != nil {
		return fmt.Errorf("rebooting %s: %w", rec.Host, err)
	}
	deadline := r.Now().Add(r.BootWait)
	for {
		r.Sleep(r.Poll)
		if h, err := r.Reconnect(); err == nil {
			if id, err := h.BootID(); err == nil && id != current {
				r.Host = h
				break
			}
		}
		if !r.Now().Before(deadline) {
			return fmt.Errorf("%s did not come back with a new boot within %s. Check it through your provider's console, then run 'kip node add %s' again", rec.Host, r.BootWait, rec.Host)
		}
	}
	if err := r.checkStopped(); err != nil {
		return err
	}
	say(r.Out, "  ✔  %s is back, and k3s stayed stopped\n", rec.Name)
	return nil
}

// checkStopped verifies the interlock and requires the agent to be stopped,
// with no container shims, Longhorn processes or Longhorn or CSI mounts remaining.
func (r *nodeRejoin) checkStopped() error {
	if err := r.Host.VerifyInterlock(); err != nil {
		return err
	}
	return r.Host.Quiescent()
}

// release lifts the fence in order: cordon, remove the interlock, start the
// agent, wait for the node to report Ready from this boot, remove the taint.
func (r *nodeRejoin) release(rec installer.NodeRecord) error {
	if err := r.Cluster.Cordon(rec.Name); err != nil {
		return err
	}
	if err := r.Host.RemoveInterlock(); err != nil {
		return err
	}
	if err := r.Host.StartAgent(); err != nil {
		return err
	}
	bootID, err := r.Host.BootID()
	if err != nil {
		return err
	}
	say(r.Out, "  ...  Waiting for %s to report Ready\n", rec.Name)
	deadline := r.Now().Add(r.ReadyWait)
	for {
		node, err := r.Cluster.Node(rec.Name)
		if err != nil {
			return err
		}
		if node.Ready && node.BootID == bootID {
			break
		}
		if !r.Now().Before(deadline) {
			return fmt.Errorf("%s did not report Ready within %s of starting k3s. It stays cordoned and tainted out of service. Check it with 'kip status', then run 'kip node add %s' again", rec.Name, r.ReadyWait, rec.Host)
		}
		r.Sleep(r.Poll)
	}
	return r.Cluster.RemoveTaint(rec.Name)
}

// startServices starts each service the fence stopped when every one of its
// volumes has a current copy on this worker or a node that is not fenced, or
// after consent to older copies. The others stay stopped for
// 'kip service start'.
func (r *nodeRejoin) startServices(rec installer.NodeRecord) error {
	view, err := r.Cluster.View(rec.Name)
	if err != nil {
		return err
	}
	records, err := r.Cluster.Records()
	if err != nil {
		return err
	}
	replicas := installer.AvailableReplicas(view.Replicas, records, rec.Name)
	for _, svc := range view.Services {
		if svc.Stop == nil {
			continue
		}
		fresh := installer.JudgeFreshness("", svc.Volumes, replicas)
		ok := fresh.Current
		if !ok && fresh.Reason == installer.FenceStopStale {
			if ok = r.AcceptStale; !ok && r.Interactive {
				if ok, err = askStaleConsent(r.Out, r.Confirm, rec.Name, svc, fresh); err != nil {
					return err
				}
			}
		}
		if !ok {
			say(r.Out, "  ⚠  %s stays stopped: no copy of its data is known to be current. Start it with 'kip service start %s' once Longhorn has rebuilt a copy, or accept the older data with --accept-stale-data\n", svc.Label(), svc.StatefulSet)
			continue
		}
		if err := r.Cluster.Start(svc.Namespace, svc.StatefulSet); err != nil {
			return err
		}
		say(r.Out, "  ✔  Started %s again\n", svc.Label())
	}
	return nil
}

// rejoinServer runs the rejoin's cluster operations on the server.
type rejoinServer struct{ fenceServer }

func (s rejoinServer) Cordon(name string) error { return installer.CordonNode(s.master, name) }

func (s rejoinServer) RemoveTaint(name string) error {
	return installer.RemoveOutOfServiceTaint(s.master, name)
}

// rejoinWorker runs the rejoin's host steps on the worker.
type rejoinWorker struct{ fenceWorker }

func (w rejoinWorker) VerifyInterlock() error          { return installer.VerifyFenceInterlock(w.c) }
func (w rejoinWorker) Quiescent() error                { return installer.VerifyQuiescent(w.c) }
func (w rejoinWorker) RemoveInterlock() error          { return installer.RemoveFenceInterlock(w.c) }
func (w rejoinWorker) StartAgent() error               { return installer.StartAgent(w.c) }
func (w rejoinWorker) Runner() installer.CommandRunner { return w.c }

func (w rejoinWorker) Reboot() error {
	out, err := w.c.Run(rebootCommand(time.Now()))
	if err != nil {
		return err
	}
	_, err = scheduledAt(out)
	return err
}

// rejoinFenced runs the rejoin of a fenced worker from kip node add.
func rejoinFenced(server, worker installer.CommandRunner, reconnect func() (installer.CommandRunner, error), cluster *config.Cluster, rec installer.NodeRecord, serverNode string, acceptStale bool) error {
	r := &nodeRejoin{
		Out:     os.Stdout,
		Cluster: rejoinServer{fenceServer{master: server, cluster: cluster}},
		Host:    rejoinWorker{fenceWorker{c: worker}},
		Reconnect: func() (rejoinHost, error) {
			c, err := reconnect()
			if err != nil {
				return nil, err
			}
			return rejoinWorker{fenceWorker{c: c}}, nil
		},
		Checks: func(node string, w installer.CommandRunner) error {
			if err := installer.WaitForWorkerStorage(server, node); err != nil {
				return err
			}
			join, err := installer.JoinForChecks(server)
			if err != nil {
				return err
			}
			err = installer.OpenWorkerForChecks(server, node)
			if err == nil {
				err = installer.ValidateWorker(server, w, join, node, serverNode, rec.PeerAddress)
			}
			if err != nil {
				if cordonErr := installer.CordonNode(server, node); cordonErr != nil {
					return fmt.Errorf("%w. Cordoning %s after the checks also failed: %v", err, node, cordonErr)
				}
				return err
			}
			fmt.Printf("  ✔  DNS, server pod connectivity, Longhorn storage, registry and kubelet checks passed\n")
			return installer.UncordonWorker(server, node)
		},
		Confirm:     confirmInteractive,
		Interactive: term.IsTerminal(int(os.Stdin.Fd())),
		AcceptStale: acceptStale,
		Now:         time.Now,
		Sleep:       time.Sleep,
		Poll:        10 * time.Second,
		BootWait:    10 * time.Minute,
		ReadyWait:   5 * time.Minute,
	}
	return r.run(rec)
}
