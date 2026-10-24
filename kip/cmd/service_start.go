package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/getkipper/kipper/controller/pkg/stranded"
	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/installer"
)

var serviceStartCmd = &cobra.Command{
	Use:   "start <name>",
	Short: "Start a service that kip node fence left stopped",
	Long: `Starts a service that 'kip node fence' stopped because no other node held a
current copy of its data.

The service starts at once when every one of its volumes now has a current
copy on a node that is not fenced. When its copies may be older than the
data on the fenced worker, kip shows each copy with its timestamps and asks
whether to start on them anyway. Writes made since those copies were current
are lost. Use --accept-stale-data to answer yes without a prompt. A service
whose only copy is on the fenced worker cannot start until the worker rejoins
with 'kip node add'. Once its volumes no longer exist, kip says how to restore
it from a backup and, on a terminal, offers to clear the fence's stop mark.`,
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE:         runServiceStart,
}

func init() {
	serviceStartCmd.Flags().String("project", "default", "project namespace")
	serviceStartCmd.Flags().String("environment", "", "target environment")
	serviceStartCmd.Flags().Bool("accept-stale-data", false, "start the service even when no copy of its data is known to be current")
	serviceStartCmd.Flags().String("ssh-key", "", "path to SSH private key")
	serviceCmd.AddCommand(serviceStartCmd)
}

// serviceStarter is what kip service start does through the server, under
// the node lifecycle lease.
type serviceStarter interface {
	FenceStop(ns, name string) (*installer.FenceStopMark, error)
	MoveMark(ns, name string) (string, error)
	Records() ([]installer.NodeRecord, error)
	Replicas() ([]stranded.Replica, error)
	LonghornVolumes() ([]string, error)
	Start(ns, name string) error
	ClearStop(ns, name string) error
	SaveRecord(installer.NodeRecord) error
}

// fenceStoppedStart starts one fence-stopped service.
type fenceStoppedStart struct {
	Out         io.Writer
	Server      serviceStarter
	Confirm     func(prompt string) (bool, error)
	Interactive bool
	AcceptStale bool
}

func runServiceStart(cmd *cobra.Command, args []string) error {
	name := args[0]
	cluster, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}
	if cluster.Host == "" {
		return fmt.Errorf("no host recorded for cluster %s. Starting a fence-stopped service needs SSH access to the server", cluster.Name)
	}
	namespace := resolveServiceNamespace(cmd)
	if !cmd.Flags().Changed("project") && !cmd.Flags().Changed("environment") {
		if namespace, err = findServiceNamespace(context.Background(), k8sClient.Dynamic(), name); err != nil {
			return err
		}
	}
	sshKey, _ := cmd.Flags().GetString("ssh-key")
	acceptStale, _ := cmd.Flags().GetBool("accept-stale-data")
	lc, err := leaseCluster(cluster, sshKey, "kip service start "+name)
	if err != nil {
		return err
	}
	defer lc.close()
	s := fenceStoppedStart{
		Out:         os.Stdout,
		Server:      startServer{fenceServer{master: lc.server, cluster: cluster}},
		Confirm:     confirmInteractive,
		Interactive: term.IsTerminal(int(os.Stdin.Fd())),
		AcceptStale: acceptStale,
	}
	return s.run(namespace, name)
}

func (s fenceStoppedStart) run(ns, name string) error {
	move, err := s.Server.MoveMark(ns, name)
	if err != nil {
		return err
	}
	if move != "" {
		return fmt.Errorf("service %s in %s is moving its volume to Longhorn. 'kip service storage longhorn %s' finishes the move, or undoes it with --rollback, and either one starts the service again", name, ns, name)
	}
	mark, err := s.Server.FenceStop(ns, name)
	if err != nil {
		return err
	}
	if mark == nil {
		return fmt.Errorf("service %s in %s was not stopped by 'kip node fence'. To restart a running service, use 'kip service restart %s'", name, ns, name)
	}
	records, err := s.Server.Records()
	if err != nil {
		return err
	}
	// The fenced worker's own copies count only once it has rejoined.
	exclude := mark.Node
	var fenced *installer.NodeRecord
	recorded := false
	for i := range records {
		if records[i].Name != mark.Node {
			continue
		}
		recorded = true
		if records[i].State == installer.NodeStateReady {
			exclude = ""
		}
		if records[i].Fence != nil {
			fenced = &records[i]
		}
	}
	svc := installer.FenceService{Namespace: ns, StatefulSet: name, Volumes: mark.Volumes, Stop: mark}
	gone, err := s.volumesGone(mark.Volumes)
	if err != nil {
		return err
	}
	if gone {
		return s.clearLost(svc, mark.Node, fenced)
	}
	replicas, err := s.Server.Replicas()
	if err != nil {
		return err
	}
	replicas = installer.AvailableReplicas(replicas, records, "")
	fresh := installer.JudgeFreshness(exclude, mark.Volumes, replicas)
	switch {
	case fresh.Current:
	case fresh.Reason == installer.FenceStopOnlyCopy && recorded:
		return fmt.Errorf("the only copy of the data of %s is on %s. Rejoin the worker with 'kip node add'. If it is lost, %s", svc.Label(), mark.Node, restoreAdvice(ns, name))
	case fresh.Reason == installer.FenceStopOnlyCopy:
		return fmt.Errorf("the only copy of the data of %s was on %s, which has been removed. To bring the service back, %s", svc.Label(), mark.Node, restoreAdvice(ns, name))
	case s.AcceptStale:
	case !s.Interactive:
		return fmt.Errorf("no copy of the data of %s is known to be current. Pass --accept-stale-data to start it on the older copies", svc.Label())
	default:
		ok, err := askStaleConsent(s.Out, s.Confirm, mark.Node, svc, fresh)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%s stays stopped", svc.Label())
		}
	}
	if err := s.Server.Start(ns, name); err != nil {
		return err
	}
	if err := s.dropEntry(fenced, ns, name); err != nil {
		return err
	}
	say(s.Out, "  ✔  Started %s\n", svc.Label())
	return nil
}

// volumesGone reports whether all listed volumes are absent from Longhorn,
// as after forced removal of the worker holding their only copy. An empty
// list returns false because it provides no evidence of lost volumes.
func (s fenceStoppedStart) volumesGone(volumes []string) (bool, error) {
	if len(volumes) == 0 {
		return false, nil
	}
	existing, err := s.Server.LonghornVolumes()
	if err != nil {
		return false, err
	}
	for _, v := range volumes {
		if slices.Contains(existing, v) {
			return false, nil
		}
	}
	return true, nil
}

// clearLost explains how to restore a service whose volumes are gone and, on
// a terminal, offers to drop the fence's stop mark. The service stays
// stopped either way.
func (s fenceStoppedStart) clearLost(svc installer.FenceService, node string, fenced *installer.NodeRecord) error {
	lost := fmt.Sprintf("the volumes of %s no longer exist, so its data was lost with %s. To bring the service back, %s", svc.Label(), node, restoreAdvice(svc.Namespace, svc.StatefulSet))
	if !s.Interactive {
		return errors.New(lost)
	}
	say(s.Out, "  ⚠  %s\n", strings.ToUpper(lost[:1])+lost[1:])
	ok, err := s.Confirm(fmt.Sprintf("  Clear the fence's stop mark from %s? It stays stopped. [y/N] ", svc.Label()))
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%s keeps its stop mark", svc.Label())
	}
	if err := s.Server.ClearStop(svc.Namespace, svc.StatefulSet); err != nil {
		return err
	}
	if err := s.dropEntry(fenced, svc.Namespace, svc.StatefulSet); err != nil {
		return err
	}
	say(s.Out, "  ✔  Cleared the stop mark of %s. It stays stopped until you delete it and restore it using the instructions above\n", svc.Label())
	return nil
}

// dropEntry removes the service from the fenced worker's stopped list.
func (s fenceStoppedStart) dropEntry(fenced *installer.NodeRecord, ns, name string) error {
	if fenced == nil {
		return nil
	}
	rec := *fenced
	var kept []config.FenceStop
	for _, st := range rec.Fence.Stopped {
		if st.Namespace != ns || st.StatefulSet != name {
			kept = append(kept, st)
		}
	}
	if len(kept) == len(rec.Fence.Stopped) {
		return nil
	}
	fence := *rec.Fence
	fence.Stopped = kept
	rec.Fence = &fence
	return s.Server.SaveRecord(rec)
}

// startServer runs kip service start's operations on the server.
type startServer struct{ fenceServer }

func (s startServer) FenceStop(ns, name string) (*installer.FenceStopMark, error) {
	return installer.ReadFenceStop(s.master, ns, name)
}

func (s startServer) MoveMark(ns, name string) (string, error) {
	sts, err := installer.KubectlMoveCluster{Server: s.master}.ReadStatefulSet(ns, name)
	if err != nil || sts == nil {
		return "", err
	}
	return sts.Mark, nil
}

func (s startServer) Replicas() ([]stranded.Replica, error) { return installer.ReadReplicas(s.master) }

func (s startServer) LonghornVolumes() ([]string, error) {
	return installer.ReadLonghornVolumes(s.master)
}

func (s startServer) ClearStop(ns, name string) error {
	return installer.ClearFenceStop(s.master, ns, name)
}
