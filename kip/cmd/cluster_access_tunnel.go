package cmd

import (
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/opstunnel"
	"github.com/getkipper/kipper/kip/internal/transport"
)

var clusterAccessConnectCmd = &cobra.Command{
	Use:   "connect",
	Short: "Run this workstation's tunnel to the cluster's nodes",
	Long: `Run the WireGuard tunnel that carries kip's SSH and API connections to the
nodes once this workstation is a registered device. kip starts it automatically
when a command needs it. Run it manually to watch its logs. It exits after
15 minutes with no open connections.

The tunnel uses its own network stack inside kip, leaving this workstation's
network interfaces and routes unchanged.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE:         runClusterAccessConnect,
}

var clusterAccessProxyCmd = &cobra.Command{
	Use:   "proxy <port>",
	Short: "Carry one connection to a node through the tunnel on stdin and stdout",
	Long: `Carry one TCP connection to a node's port through this workstation's tunnel,
on stdin and stdout. kip uses it as SSH's ProxyCommand. It starts the tunnel
if needed.`,
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE:         runClusterAccessProxy,
}

var clusterAccessViaCmd = &cobra.Command{
	Use:   "via tunnel|direct|auto",
	Short: "Set how this workstation reaches the cluster's nodes",
	Long: `Set how kip reaches SSH and the Kubernetes API on this cluster's nodes from
this workstation, once it is a registered device:

  auto    the tunnel, or the direct path when the tunnel is not up within
          10 seconds (the default)
  tunnel  always the tunnel
  direct  always a direct connection, for a network that blocks UDP

--via overrides this setting for one command.`,
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE:         runClusterAccessVia,
}

func init() {
	clusterAccessProxyCmd.Flags().String("node", "", "machine ID of the node to reach")
	clusterAccessProxyCmd.Flags().String("device", "", "the device the connection must use")
	_ = clusterAccessProxyCmd.MarkFlagRequired("node")
	clusterAccessCmd.AddCommand(clusterAccessConnectCmd, clusterAccessProxyCmd, clusterAccessViaCmd)
}

func runClusterAccessVia(cmd *cobra.Command, args []string) error {
	mode := args[0]
	if err := transport.ValidMode(mode); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	name := resolveClusterName(cfg)
	err = config.Update(func(cfg *config.Config) error {
		c := cfg.GetCluster(name)
		if c == nil {
			return fmt.Errorf("no cluster named %q in the local config", name)
		}
		if c.Ops == nil {
			return errNoDevice(name)
		}
		c.Ops.Via = mode
		if mode == transport.Auto {
			c.Ops.Via = ""
		}
		return nil
	})
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "kip now reaches cluster %s's nodes %s.\n", name, map[string]string{
		transport.Auto:   "through the tunnel, falling back to the direct path when it is not up within 10 seconds",
		transport.Tunnel: "through the tunnel only",
		transport.Direct: "directly, without the tunnel",
	}[mode])
	return nil
}

func errNoDevice(cluster string) error {
	return fmt.Errorf("this workstation has no device for cluster %s. Add one with 'kip cluster access device add <name>'", cluster)
}

// opsManifest builds the tunnel's manifest from the cluster's local device
// state and key file. Only pending and active devices can supply a manifest.
func opsManifest(cluster *config.Cluster) (opstunnel.Manifest, error) {
	ops := cluster.Ops
	if ops == nil || (ops.State != config.OpsPending && ops.State != config.OpsActive) {
		return opstunnel.Manifest{}, errNoDevice(cluster.Name)
	}
	keyPath, err := config.OpsKeyPath(cluster.Name)
	if err != nil {
		return opstunnel.Manifest{}, err
	}
	key, err := os.ReadFile(keyPath) //nolint:gosec // the path is kip's own key file for this cluster
	if err != nil {
		return opstunnel.Manifest{}, fmt.Errorf("reading the device key: %w. 'kip cluster access show' stops using a device whose key is lost", err)
	}
	addr, err := netip.ParseAddr(ops.Address)
	if err != nil {
		return opstunnel.Manifest{}, fmt.Errorf("the device address %q in the local config is not an address", ops.Address)
	}
	m := opstunnel.Manifest{PrivateKey: strings.TrimSpace(string(key)), Address: addr}
	for _, n := range ops.Nodes {
		a, err := netip.ParseAddr(n.Address)
		if err != nil {
			return opstunnel.Manifest{}, fmt.Errorf("node %s has operator address %q in the local config", n.Name, n.Address)
		}
		m.Nodes = append(m.Nodes, opstunnel.Node{
			MachineID: n.MachineID, Name: n.Name, Endpoint: n.Endpoint, PublicKey: n.PublicKey,
			Address: a, SSHPorts: n.SSHPorts, Server: n.Server,
		})
	}
	return m, nil
}

// tunnelSource reads the manifest from the local config, so a process picks
// up a node that was added or removed since it started.
func tunnelSource(cluster string) opstunnel.Source {
	stamp := func() string {
		var b strings.Builder
		paths := []string{}
		if p, err := config.Path(); err == nil {
			paths = append(paths, p)
		}
		if p, err := config.OpsKeyPath(cluster); err == nil {
			paths = append(paths, p)
		}
		for _, p := range paths {
			if info, err := os.Stat(p); err == nil {
				fmt.Fprintf(&b, "%d/%d;", info.ModTime().UnixNano(), info.Size())
			}
		}
		return b.String()
	}
	load := func() (opstunnel.Manifest, error) {
		cfg, err := config.Load()
		if err != nil {
			return opstunnel.Manifest{}, err
		}
		c := cfg.GetCluster(cluster)
		if c == nil {
			return opstunnel.Manifest{}, fmt.Errorf("no cluster named %q in the local config", cluster)
		}
		return opsManifest(c)
	}
	return opstunnel.Source{Load: load, Stamp: stamp}
}

func runClusterAccessConnect(cmd *cobra.Command, _ []string) error {
	cluster, err := loadCurrentClusterConfig()
	if err != nil {
		return err
	}
	if _, err := opsManifest(cluster); err != nil {
		return err
	}
	p, err := opstunnel.ClusterPaths(cluster.Name)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logf := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, format+"\n", args...)
	}
	err = opstunnel.Run(ctx, p, tunnelSource(cluster.Name), opstunnel.IdleExit, logf)
	if errors.Is(err, opstunnel.ErrRunning) {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "The tunnel for cluster %s is already running.\n", cluster.Name)
		return nil
	}
	return err
}

func runClusterAccessProxy(cmd *cobra.Command, args []string) error {
	port, err := strconv.Atoi(args[0])
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("%q is not a port", args[0])
	}
	machineID, _ := cmd.Flags().GetString("node")
	device, _ := cmd.Flags().GetString("device")
	cluster, err := loadCurrentClusterConfig()
	if err != nil {
		return err
	}
	if _, err := opsManifest(cluster); err != nil {
		return err
	}
	if device != "" && device != cluster.Ops.Device {
		return fmt.Errorf("this connection was set up for device %s, but this workstation is now device %s. Run the command again", device, cluster.Ops.Device)
	}
	conn, err := opstunnel.DialCluster(cmd.Context(), cluster.Name, machineID, port)
	if err != nil {
		return err
	}
	return pipeStdio(conn, os.Stdin, os.Stdout)
}

// pipeStdio copies between conn and stdin/stdout until conn closes. When
// stdin ends first, it closes conn's write half if supported so replies can
// continue to arrive.
func pipeStdio(conn io.ReadWriteCloser, in io.Reader, out io.Writer) error {
	defer func() { _ = conn.Close() }()
	go func() {
		_, _ = io.Copy(conn, in)
		if cw, ok := conn.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()
	_, err := io.Copy(out, conn)
	return err
}
