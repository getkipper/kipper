package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/getkipper/kipper/kip/internal/installer"
	"github.com/getkipper/kipper/kip/internal/service"
)

var serviceStorageCmd = &cobra.Command{
	Use:   "storage",
	Short: "Move a service's data to another kind of storage",
}

var serviceStorageLonghornCmd = &cobra.Command{
	Use:   "longhorn <name>",
	Short: "Move a service's data volume from local storage onto Longhorn",
	Long: `Moves a service's data volume from local storage onto a single-copy Longhorn
volume, on a cluster that 'kip cluster storage longhorn' switched to Longhorn.

The service is stopped for the whole copy, and the prompt estimates the
downtime. kip keeps other pods off the volume, copies the data into a new
Longhorn volume, and once Longhorn has detached the volume, a separate pod
compares every file of the copy, read from a fresh mount, with the original.
The service then starts again on the new volume. The old local directory is
kept and listed by 'kip status' until 'kip cluster storage prune' deletes it.

Before stopping the service, kip checks that the server has room for the move's
pods and the service after the service's pods stop, and that the namespace's
ResourceQuota admits the move's pods. The node check reads requests at that
moment and reserves nothing. A copy that runs out of room grows the new volume
once and copies again.

A completed backup of the service's project is required when the cluster has
backups. On a cluster without backups, pass --no-backup to move anyway.

An interrupted move continues where it stopped when you run the command again,
after checking that the service is still stopped and other pods are still kept
off its volume. A finished copy is kept unless another pod mounted the new
volume or its protection was lifted in the meantime, in which case kip copies
the data again. A move that resumes after verifying its copy verifies it again
before it commits. While a move runs, its namespace cannot be deleted. Before
the move commits, --rollback undoes it and starts the service on its local
volume again. If the move record is missing, --rollback rebuilds it from the
remaining volumes.`,
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE:         runServiceStorageLonghorn,
}

func init() {
	serviceStorageLonghornCmd.Flags().String("project", "default", "project namespace")
	serviceStorageLonghornCmd.Flags().String("environment", "", "target environment")
	serviceStorageLonghornCmd.Flags().Bool("no-backup", false, "move the service on a cluster without backups")
	serviceStorageLonghornCmd.Flags().Bool("rollback", false, "undo an unfinished move and start the service on its local volume")
	serviceStorageLonghornCmd.Flags().BoolP("yes", "y", false, "skip the confirmation prompt")
	serviceStorageLonghornCmd.Flags().String("ssh-key", "", "path to SSH private key")
	serviceStorageCmd.AddCommand(serviceStorageLonghornCmd)
	serviceCmd.AddCommand(serviceStorageCmd)
}

func runServiceStorageLonghorn(cmd *cobra.Command, args []string) error {
	name := args[0]
	cluster, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}
	if cluster.Host == "" {
		return fmt.Errorf("no host recorded for cluster %s. Moving a service's storage needs SSH access to the server", cluster.Name)
	}
	ctx := context.Background()
	namespace := resolveServiceNamespace(cmd)
	if !cmd.Flags().Changed("project") && !cmd.Flags().Changed("environment") {
		if namespace, err = findServiceNamespace(ctx, k8sClient.Dynamic(), name); err != nil {
			return err
		}
	}
	sshKey, _ := cmd.Flags().GetString("ssh-key")
	noBackup, _ := cmd.Flags().GetBool("no-backup")
	rollback, _ := cmd.Flags().GetBool("rollback")
	yes, _ := cmd.Flags().GetBool("yes")

	lc, err := leaseCluster(cluster, sshKey, "kip service storage longhorn "+name)
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
	mgr := &service.Manager{Client: k8sClient.Clientset(), Dynamic: k8sClient.Dynamic()}
	move := &installer.StorageMove{
		Cluster:   installer.KubectlMoveCluster{Server: master},
		Namespace: namespace,
		Service:   name,
		Operator:  operator,
		NoBackup:  noBackup,
		Out:       os.Stdout,
		Now:       time.Now,
		Sleep:     time.Sleep,
		Consent:   moveConsent(os.Stdout, confirmInteractive, term.IsTerminal(int(os.Stdin.Fd())), yes),
		CheckRoom: func(data, dump int64) error {
			return refuseMoveWithoutRoom(func(volumes []service.NewVolume) (string, error) {
				return mgr.CheckStorageRoom(ctx, volumes, "the service's storage")
			}, namespace, name, data, dump)
		},
	}
	if rollback {
		return move.Rollback()
	}
	err = move.Run()
	if errors.Is(err, installer.ErrMoveCanceled) {
		say(os.Stdout, "  Nothing was changed.\n")
		return nil
	}
	return err
}

// refuseMoveWithoutRoom checks Longhorn room for the move's new volumes. The
// move deletes the old claim, so a check that could not run refuses too.
func refuseMoveWithoutRoom(check func([]service.NewVolume) (string, error), namespace, name string, data, dump int64) error {
	volumes := []service.NewVolume{{Name: name + " data", Bytes: data, Namespace: namespace, Copies: 1}}
	if dump > 0 {
		volumes = append(volumes, service.NewVolume{Name: name + " dump", Bytes: dump, Namespace: namespace, Copies: 1})
	}
	warning, err := check(volumes)
	if err != nil {
		return err
	}
	if warning != "" {
		return fmt.Errorf("%s. The move needs a confirmed answer, so it stops here", warning)
	}
	return nil
}

// moveConsent asks before a move stops the service. --yes skips the
// question. Without a terminal, --yes is required.
func moveConsent(out io.Writer, confirm func(string) (bool, error), interactive, yes bool) func(installer.MovePlan) (bool, error) {
	return func(p installer.MovePlan) (bool, error) {
		say(out, "  Moving %s (%s) onto Longhorn:\n", p.Service, p.Namespace)
		say(out, "    %s of data in %d files and directories is copied into a new %s volume, then read back from a fresh mount and compared file by file.\n", service.FormatSize(p.UsedBytes), p.Entries, p.TargetSize)
		if p.DumpClaim != "" {
			say(out, "    Its dump volume %s is recreated on Longhorn after the move.\n", p.DumpClaim)
		}
		if p.NoBackup {
			say(out, "    ⚠  The cluster has no backups.\n")
		} else {
			say(out, "    The latest backup of %s finished %s ago.\n", p.Namespace, p.BackupAge.Round(time.Minute))
		}
		say(out, "    The service stays stopped until the copy is verified. Its local directory is kept.\n")
		say(out, "    %s will be stopped for about %d minutes. The estimate is conservative, and a slow disk takes longer.\n", p.Service, int(p.Downtime.Minutes()))
		if yes {
			return true, nil
		}
		if !interactive {
			return false, fmt.Errorf("the move stops %s. Pass --yes to move it without a prompt", p.Service)
		}
		return confirm(fmt.Sprintf("  Stop %s and move it? [y/N] ", p.Service))
	}
}
