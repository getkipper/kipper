package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/installer"
)

var nodeKernelCmd = &cobra.Command{
	Use:          "kernel",
	Short:        "Show each node's kernels and whether the running one blocks container signals",
	SilenceUsage: true,
	Long: `Shows the running kernel, the kernel pinned with 'kip node kernel pin', and
the installed kernels of every recorded node, or of one node with --host.

It also counts AppArmor denials of signals sent inside containers since the
node booted. Some kernels make AppArmor deny these signals, causing programs
that signal their own threads to hang. This affects the Postgres image's
startup helper, preventing Postgres services from starting. Pin a kernel that
worked before and reboot the node to recover.

Examples:
  kip node kernel
  kip node kernel --host 203.0.113.11`,
	Args: cobra.NoArgs,
	RunE: runNodeKernel,
}

var nodeKernelPinCmd = &cobra.Command{
	Use:          "pin <version>",
	Short:        "Make a node boot a given installed kernel from its next reboot on",
	SilenceUsage: true,
	Long: `Makes the server, or the node given with --host, boot an installed kernel
from its next reboot on, and holds that kernel's package so automatic updates
keep it installed. Newer kernels can still be installed but do not boot until
'kip node kernel unpin'. The running kernel changes at reboot, so run
'kip node reboot' afterward.

The pin is a GRUB drop-in, /etc/default/grub.d/99-kipper-kernel.cfg, and
needs Ubuntu or Debian with GRUB. Pinning again replaces the earlier pin.
'kip status' stops reporting the newer kernel as a pending update while
the node runs its pinned kernel.

Examples:
  kip node kernel pin 7.0.0-38-generic
  kip node kernel pin 7.0.0-38-generic --host 203.0.113.11`,
	Args: cobra.ExactArgs(1),
	RunE: runNodeKernelPin,
}

var nodeKernelUnpinCmd = &cobra.Command{
	Use:          "unpin",
	Short:        "Remove a kernel pin so the node boots GRUB's default kernel again",
	SilenceUsage: true,
	Long: `Removes the pin set with 'kip node kernel pin' from the server, or from the
node given with --host, and releases the hold on the pinned kernel's package.
The node boots GRUB's default kernel, normally the newest one, from its next
reboot on. Run 'kip node reboot' afterward.

Examples:
  kip node kernel unpin
  kip node kernel unpin --host 203.0.113.11`,
	Args: cobra.NoArgs,
	RunE: runNodeKernelUnpin,
}

func init() {
	for _, c := range []*cobra.Command{nodeKernelCmd, nodeKernelPinCmd, nodeKernelUnpinCmd} {
		c.Flags().String("ssh-key", "", "path to SSH private key")
	}
	nodeKernelCmd.Flags().String("host", "", "IP address or hostname of a single node (default: every recorded node)")
	nodeKernelPinCmd.Flags().String("host", "", "IP address or hostname of the node (default: the server)")
	nodeKernelUnpinCmd.Flags().String("host", "", "IP address or hostname of the node (default: the server)")

	nodeKernelCmd.AddCommand(nodeKernelPinCmd, nodeKernelUnpinCmd)
	nodeCmd.AddCommand(nodeKernelCmd)
}

func runNodeKernel(cmd *cobra.Command, _ []string) error {
	nodeHost, _ := cmd.Flags().GetString("host")
	sshKey, _ := cmd.Flags().GetString("ssh-key")
	cluster, _, err := loadCurrentCluster()
	if err != nil {
		return err
	}
	leased, err := leaseCluster(cluster, sshKey, "kip node kernel")
	if err != nil {
		return err
	}
	defer leased.close()

	if nodeHost != "" {
		recorded, err := withRecordedNodes(leased.server, cluster)
		if err == nil {
			cluster = recorded
		}
		conn, err := dialNodeSSH(nodeHost, sshKey, cluster)
		if err != nil {
			return fmt.Errorf("connecting to %s: %w", nodeHost, err)
		}
		defer func() { _ = conn.Close() }()
		return showKernel(os.Stdout, nodeHost, nodeTarget(cluster, nodeHost), leased.lease.Host(conn))
	}

	records, err := recordNodes(leased.server, cluster)
	if err != nil {
		return err
	}
	dial := leased.dialer(cluster, sshKey)
	return listKernels(os.Stdout, cluster, records, leased.server, func(rec installer.NodeRecord) (installer.CommandRunner, func(), error) {
		return dial(rec)
	})
}

// listKernels shows the server's kernels, then each recorded worker's. A worker
// that cannot be reached, or answers as another machine, is reported and
// skipped, so the remaining nodes are still shown.
func listKernels(out io.Writer, cluster *config.Cluster, records []installer.NodeRecord, server installer.CommandRunner,
	dial func(installer.NodeRecord) (installer.CommandRunner, func(), error)) error {
	rec, ok := installer.ServerRecord(records)
	if !ok {
		return fmt.Errorf("the node records have no server")
	}
	if err := showKernel(out, fmt.Sprintf("%s (%s)", rec.Name, rec.Host), "", server); err != nil {
		return err
	}
	for _, rec := range records {
		if rec.Role != installer.NodeRoleWorker {
			continue
		}
		showWorkerKernel(out, cluster, rec, dial)
	}
	return nil
}

func showWorkerKernel(out io.Writer, cluster *config.Cluster, rec installer.NodeRecord, dial func(installer.NodeRecord) (installer.CommandRunner, func(), error)) {
	node := fmt.Sprintf("%s (%s)", rec.Name, rec.Host)
	client, done, err := dial(rec)
	if err != nil {
		say(out, "\n  Node %s\n  ⚠  not reached (%v)\n", node, err)
		return
	}
	defer done()
	id, err := installer.ReadMachineID(client)
	if err != nil {
		say(out, "\n  Node %s\n  ⚠  not checked (%v)\n", node, err)
		return
	}
	if id != rec.MachineID {
		say(out, "\n  Node %s\n  ⚠  not checked (%s no longer answers as %s)\n", node, rec.Host, rec.Name)
		return
	}
	_ = showKernel(out, node, nodeTarget(cluster, rec.Host), client)
}

// nodeTarget is the --host flag that points a command at host, empty for the server.
func nodeTarget(cluster *config.Cluster, host string) string {
	if host == "" || host == cluster.Host {
		return ""
	}
	return " --host " + host
}

// showKernel prints one node's kernels, with recovery commands that carry
// target, the node's --host flag. Read failures are printed and return nil so
// the listing continues with other nodes.
func showKernel(out io.Writer, node, target string, client installer.CommandRunner) error {
	say(out, "\n  Node %s\n", node)
	state, err := installer.ReadKernelState(client)
	if err != nil {
		say(out, "  ⚠  %v\n", err)
		return nil
	}
	say(out, "    Running:    %s\n", state.Running)
	pinned := state.Pinned
	if pinned == "" {
		pinned = "none"
	}
	say(out, "    Pinned:     %s\n", pinned)
	say(out, "    Installed:  %s\n", strings.Join(state.Installed, ", "))
	if state.BlocksContainerSignals() {
		say(out, "  ⚠  %s blocks signals inside containers (%d AppArmor %s since boot).\n",
			state.Running, state.SignalDenials, plural(state.SignalDenials, "denial", "denials"))
	} else {
		say(out, "  ✔  no blocked signals inside containers since boot\n")
	}
	reboot := "kip node reboot"
	if host := strings.TrimPrefix(target, " --host "); host != "" {
		reboot += " " + host
	}
	if hint := kernelHint(state, target, reboot, false); hint != "" {
		say(out, "     %s.\n", hint)
	}
	return nil
}

// kernelHint returns recovery guidance, or "" when no issue is detected.
// target is the node's --host argument with a leading space, or "" for the
// server. reboot is its reboot command. Set booted after a reboot so a pin
// mismatch prompts investigation before another reboot.
func kernelHint(state installer.KernelState, target, reboot string, booted bool) string {
	pinned := state.Pinned != "" && state.Pinned != state.Running
	switch {
	case state.Pinned != "" && !slices.Contains(state.Installed, state.Pinned):
		return fmt.Sprintf("Pinned kernel %s is no longer installed. Pin an installed kernel with 'kip node kernel pin <version>%s', or run 'kip node kernel unpin%s'", state.Pinned, target, target)
	case pinned && booted:
		return fmt.Sprintf("Kernel %s is pinned, but the node booted %s. Check that no other file in /etc/default/grub.d sets GRUB_DEFAULT, then run 'kip node kernel pin %s%s' again", state.Pinned, state.Running, state.Pinned, target)
	case pinned:
		return fmt.Sprintf("Kernel %s is pinned. Run '%s' to boot it", state.Pinned, reboot)
	case state.BlocksContainerSignals():
		return fmt.Sprintf("Pin a kernel that worked before with 'kip node kernel pin <version>%s', then run '%s'", target, reboot)
	}
	return ""
}

func runNodeKernelPin(cmd *cobra.Command, args []string) error {
	version := args[0]
	return onKernelNode(cmd, "kip node kernel pin", func(node string, client installer.CommandRunner) error {
		if err := pinKernelOn(node, version, hostFlagFor(cmd), client); err != nil {
			return err
		}
		say(os.Stdout, "\n  ✔  %s boots kernel %s from its next reboot on. Run '%s' to boot it now.\n\n",
			node, version, rebootCommandFor(cmd))
		return nil
	})
}

// pinKernelOn pins version on node. When the kernel is not installed, the
// error names the listing for the same node, which target selects.
func pinKernelOn(node, version, target string, client installer.CommandRunner) error {
	err := installer.PinKernel(client, version)
	if errors.Is(err, installer.ErrKernelNotInstalled) {
		return fmt.Errorf("%s: %w. 'kip node kernel%s' lists the installed kernels", node, err, target)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", node, err)
	}
	return nil
}

func runNodeKernelUnpin(cmd *cobra.Command, _ []string) error {
	return onKernelNode(cmd, "kip node kernel unpin", func(node string, client installer.CommandRunner) error {
		version, err := installer.UnpinKernel(client)
		if err != nil {
			return fmt.Errorf("%s: %w", node, err)
		}
		if version == "" {
			say(os.Stdout, "\n  ✔  %s has no kernel pin\n\n", node)
			return nil
		}
		say(os.Stdout, "\n  ✔  %s no longer pins kernel %s and boots GRUB's default kernel from its next reboot on. Run '%s' to boot it now.\n\n",
			node, version, rebootCommandFor(cmd))
		return nil
	})
}

// onKernelNode runs fn on the server, or on the node given with --host, while
// holding the cluster's node lease.
func onKernelNode(cmd *cobra.Command, purpose string, fn func(node string, client installer.CommandRunner) error) error {
	nodeHost, _ := cmd.Flags().GetString("host")
	sshKey, _ := cmd.Flags().GetString("ssh-key")
	cluster, _, err := loadCurrentCluster()
	if err != nil {
		return err
	}
	leased, err := leaseCluster(cluster, sshKey, purpose)
	if err != nil {
		return err
	}
	defer leased.close()
	if nodeHost == "" || nodeHost == cluster.Host {
		return fn(cluster.Host, leased.server)
	}
	recorded, err := withRecordedNodes(leased.server, cluster)
	if err == nil {
		cluster = recorded
	}
	conn, err := dialNodeSSH(nodeHost, sshKey, cluster)
	if err != nil {
		return fmt.Errorf("connecting to %s: %w", nodeHost, err)
	}
	defer func() { _ = conn.Close() }()
	return fn(nodeHost, leased.lease.Host(conn))
}

// rebootCommandFor names the reboot command for the node the command targeted.
func rebootCommandFor(cmd *cobra.Command) string {
	if nodeHost, _ := cmd.Flags().GetString("host"); nodeHost != "" {
		return "kip node reboot " + nodeHost
	}
	return "kip node reboot"
}

// hostFlagFor returns the --host flag the command was given, as " --host X",
// or "" when --host was omitted.
func hostFlagFor(cmd *cobra.Command) string {
	if nodeHost, _ := cmd.Flags().GetString("host"); nodeHost != "" {
		return " --host " + nodeHost
	}
	return ""
}
