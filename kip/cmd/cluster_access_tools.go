package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/signal"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/spf13/cobra"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/transport"
)

// forwardPort is the local port forward listens on and kubeconfig points at
// by default, so the two agree without options.
const forwardPort = 16443

var clusterAccessSSHConfigCmd = &cobra.Command{
	Use:   "ssh-config",
	Short: "Print OpenSSH Host blocks that reach each node through the tunnel",
	Long: `Print one OpenSSH Host block per node that reaches it through this
workstation's device tunnel, for ssh, scp and other tools that read
~/.ssh/config. The blocks call kip through PATH, so they keep working after
kip is upgraded. Print them again after a node is added or removed.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE:         runClusterAccessSSHConfig,
}

var clusterAccessForwardCmd = &cobra.Command{
	Use:   "forward",
	Short: "Forward a local port to the Kubernetes API through the tunnel",
	Long: `Listen on 127.0.0.1 and carry each connection to the server's Kubernetes API
through this workstation's device tunnel, until Ctrl+C. Use it with the context
'kip cluster access kubeconfig' writes. Any local user of this machine can
reach the API through the port while it runs. The API still asks for
credentials.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE:         runClusterAccessForward,
}

var clusterAccessKubeconfigCmd = &cobra.Command{
	Use:   "kubeconfig",
	Short: "Add a kubeconfig context that reaches the API through 'forward'",
	Long: `Add a cluster and context to the cluster's kubeconfig whose server is
https://127.0.0.1:<port>, the port 'kip cluster access forward' listens on.
The entry keeps the CA, the credentials and the TLS server name, so kubectl
and other tools check the API's certificate as before.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE:         runClusterAccessKubeconfig,
}

func init() {
	clusterAccessForwardCmd.Flags().Int("port", forwardPort, "local port to listen on")
	clusterAccessKubeconfigCmd.Flags().Int("port", forwardPort, "local port 'forward' listens on")
	clusterAccessCmd.AddCommand(clusterAccessSSHConfigCmd, clusterAccessForwardCmd, clusterAccessKubeconfigCmd)
}

// activeDeviceCluster loads the current cluster and refuses one this
// workstation has no active device for.
func activeDeviceCluster() (*config.Cluster, error) {
	cluster, err := loadCurrentClusterConfig()
	if err != nil {
		return nil, err
	}
	if cluster.Ops == nil || cluster.Ops.State != config.OpsActive {
		return nil, errNoDevice(cluster.Name)
	}
	return cluster, nil
}

func runClusterAccessSSHConfig(cmd *cobra.Command, _ []string) error {
	cluster, err := activeDeviceCluster()
	if err != nil {
		return err
	}
	out, err := sshConfigBlocks(cluster, "kip")
	if err != nil {
		return err
	}
	_, err = fmt.Fprint(cmd.OutOrStdout(), out)
	return err
}

// sshConfigBlocks returns a Host block per node of cluster's device
// manifest, named <cluster>-<node>, whose ProxyCommand runs exe.
func sshConfigBlocks(cluster *config.Cluster, exe string) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "# Nodes of cluster %s through device %s, from 'kip cluster access ssh-config'\n", cluster.Name, cluster.Ops.Device)
	for _, n := range cluster.Ops.Nodes {
		proxy, err := transport.ProxyCommandFor(exe, cluster.Name, n.MachineID, cluster.Ops.Device)
		if err != nil {
			return "", err
		}
		port := 22
		if len(n.SSHPorts) > 0 {
			port = n.SSHPorts[0]
		}
		fmt.Fprintf(&b, "Host %s-%s\n  HostName %s\n  User %s\n  Port %d\n", cluster.Name, n.Name, strings.Trim(n.Host, "[]"), nodeSSHUser(cluster, n.Host), port)
		if cluster.SSHKey != "" {
			fmt.Fprintf(&b, "  IdentityFile %s\n", cluster.SSHKey)
		}
		fmt.Fprintf(&b, "  ProxyCommand %s\n", proxy)
	}
	return b.String(), nil
}

func runClusterAccessForward(cmd *cobra.Command, _ []string) error {
	cluster, err := activeDeviceCluster()
	if err != nil {
		return err
	}
	port, _ := cmd.Flags().GetInt("port")
	dial, err := transport.ClusterAPIDialer(cluster)
	if err != nil {
		return err
	}
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return fmt.Errorf("listening on 127.0.0.1:%d: %w. Pick another port with --port, for 'kubeconfig' too", port, err)
	}
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	say(cmd.OutOrStdout(), "Forwarding 127.0.0.1:%d to the Kubernetes API of %s through device %s. Press Ctrl+C to stop.\n", port, cluster.Name, cluster.Ops.Device)
	target := net.JoinHostPort(cluster.Ops.APIHost, strconv.Itoa(transport.APIPort))
	return serveForward(ctx, l, func(ctx context.Context) (net.Conn, error) {
		return dial(ctx, "tcp", target)
	})
}

// serveForward accepts on l until ctx ends and copies each connection both
// ways to one from dial. A failed dial closes only that connection. Context
// cancellation closes active connections, and the function waits for all
// connection handlers before returning.
func serveForward(ctx context.Context, l net.Listener, dial func(context.Context) (net.Conn, error)) error {
	go func() {
		<-ctx.Done()
		_ = l.Close()
	}()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { _ = c.Close() }()
			up, err := dial(ctx)
			if err != nil {
				return
			}
			defer func() { _ = up.Close() }()
			done := make(chan struct{}, 2)
			go func() { _, _ = io.Copy(up, c); done <- struct{}{} }()
			go func() { _, _ = io.Copy(c, up); done <- struct{}{} }()
			select {
			case <-done:
			case <-ctx.Done():
			}
		}()
	}
}

func runClusterAccessKubeconfig(cmd *cobra.Command, _ []string) error {
	cluster, err := activeDeviceCluster()
	if err != nil {
		return err
	}
	port, _ := cmd.Flags().GetInt("port")
	kc, err := clientcmd.LoadFromFile(cluster.Kubeconfig)
	if err != nil {
		return fmt.Errorf("loading kubeconfig %s: %w", cluster.Kubeconfig, err)
	}
	name, err := addTunnelContext(kc, port)
	if err != nil {
		return err
	}
	if err := clientcmd.WriteToFile(*kc, cluster.Kubeconfig); err != nil {
		return fmt.Errorf("writing kubeconfig %s: %w", cluster.Kubeconfig, err)
	}
	say(cmd.OutOrStdout(), "Added context %s to %s. Run 'kip cluster access forward', then 'kubectl --context %s ...'\n", name, cluster.Kubeconfig, name)
	return nil
}

// addTunnelContext adds to kc a copy of its current context named
// <context>-tunnel, whose cluster entry points at https://127.0.0.1:port with
// the same CA and the original host as TLS server name, unless one was set.
// The copied cluster entry bypasses the original proxy.
// It replaces existing entries only when they are what it would write, apart
// from the local port, and otherwise returns an error and leaves kc
// unchanged. It returns the new context's name.
func addTunnelContext(kc *clientcmdapi.Config, port int) (string, error) {
	ctx, ok := kc.Contexts[kc.CurrentContext]
	if !ok {
		return "", fmt.Errorf("the kubeconfig has no current context")
	}
	orig, ok := kc.Clusters[ctx.Cluster]
	if !ok {
		return "", fmt.Errorf("the kubeconfig has no cluster %q", ctx.Cluster)
	}
	u, err := url.Parse(orig.Server)
	if err != nil || u.Hostname() == "" {
		return "", fmt.Errorf("the kubeconfig's server %q is not a URL", orig.Server)
	}
	entry := orig.DeepCopy()
	entry.Server = "https://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	entry.ProxyURL = ""
	if entry.TLSServerName == "" {
		entry.TLSServerName = u.Hostname()
	}
	name := kc.CurrentContext + "-tunnel"
	tctx := ctx.DeepCopy()
	tctx.Cluster = ctx.Cluster + "-tunnel"
	if old, ok := kc.Clusters[tctx.Cluster]; ok && !sameTunnelCluster(old, entry) {
		return "", fmt.Errorf("the kubeconfig already has a different cluster called %[1]s, so kip left the kubeconfig unchanged. If kip wrote it before, remove it with 'kubectl config delete-cluster %[1]s' and run this again", tctx.Cluster)
	}
	if old, ok := kc.Contexts[name]; ok && !reflect.DeepEqual(old, tctx) {
		return "", fmt.Errorf("the kubeconfig already has a different context called %[1]s, so kip left the kubeconfig unchanged. If kip wrote it before, remove it with 'kubectl config delete-context %[1]s' and run this again", name)
	}
	kc.Clusters[tctx.Cluster] = entry
	kc.Contexts[name] = tctx
	return name, nil
}

// sameTunnelCluster reports whether old is entry with another local port.
func sameTunnelCluster(old, entry *clientcmdapi.Cluster) bool {
	if !strings.HasPrefix(old.Server, "https://127.0.0.1:") {
		return false
	}
	c := old.DeepCopy()
	c.Server = entry.Server
	return reflect.DeepEqual(c, entry)
}
