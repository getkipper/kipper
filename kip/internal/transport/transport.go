// Package transport decides, per cluster and node, whether kip reaches SSH and
// the Kubernetes API directly or through this workstation's device tunnel,
// and wires the tunnel into ssh and client-go when it does.
package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"k8s.io/client-go/rest"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/opstunnel"
)

const (
	Auto   = "auto"
	Tunnel = "tunnel"
	Direct = "direct"
)

// EstablishTimeout bounds the handshake and the first TCP connection.
const EstablishTimeout = 10 * time.Second

// APIPort is the Kubernetes API port the tunnel carries.
const APIPort = 6443

// Override is the --via flag of the running command, or "" for the stored
// default.
var Override string

// Warn receives the messages about falling back to the direct path.
var Warn io.Writer = os.Stderr

// ValidMode returns an error unless mode is auto, tunnel or direct.
func ValidMode(mode string) error {
	switch mode {
	case Auto, Tunnel, Direct:
		return nil
	}
	return fmt.Errorf("%q is not a transport. Use tunnel, direct or auto", mode)
}

// Mode returns the transport a command uses for cluster: --via, then the
// stored default, then auto. A cluster without an active device on this
// workstation is always direct.
func Mode(c *config.Cluster) string {
	if c == nil || c.Ops == nil || c.Ops.State != config.OpsActive {
		return Direct
	}
	if Override != "" {
		return Override
	}
	if c.Ops.Via != "" {
		return c.Ops.Via
	}
	return Auto
}

// dialCluster is opstunnel.DialCluster, replaced in tests.
var dialCluster = opstunnel.DialCluster

// established caches probe results for the command's lifetime, so each
// cluster, node and port combination is checked at most once.
var (
	mu          sync.Mutex
	established = map[string]error{}
)

// probe reports whether a stream to port on the node opens through the
// tunnel within EstablishTimeout.
func probe(c *config.Cluster, n config.OpsNode, port int) error {
	key := c.Name + "/" + n.MachineID + "/" + strconv.Itoa(port)
	mu.Lock()
	defer mu.Unlock()
	if err, ok := established[key]; ok {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), EstablishTimeout)
	defer cancel()
	conn, err := dialCluster(ctx, c.Name, n.MachineID, port)
	if err == nil {
		_ = conn.Close()
	}
	established[key] = err
	return err
}

// NodeForHost returns the manifest entry of the node kip reaches at host.
// Names compare case-insensitively and addresses by value.
func NodeForHost(c *config.Cluster, host string) (config.OpsNode, bool) {
	if c == nil || c.Ops == nil {
		return config.OpsNode{}, false
	}
	want, addrErr := netip.ParseAddr(strings.Trim(host, "[]"))
	for _, n := range c.Ops.Nodes {
		if addrErr == nil {
			if a, err := netip.ParseAddr(strings.Trim(n.Host, "[]")); err == nil && a.Unmap() == want.Unmap() {
				return n, true
			}
			continue
		}
		if strings.EqualFold(n.Host, host) {
			return n, true
		}
	}
	return config.OpsNode{}, false
}

// SSHOptions returns the ssh options that carry a connection to host through
// the tunnel, or nil when the command uses the direct path. In auto mode it
// falls back to the direct path, with a message, when the tunnel does not
// carry a connection to the node's SSH port within EstablishTimeout. Probe
// results are cached per cluster, node and port for the command's lifetime.
// An empty port probes the first recorded SSH port, or port 22 if none is
// recorded. Tunnel mode rejects hosts absent from the device's node list.
// Invalid port strings and ProxyCommand construction failures return errors.
func SSHOptions(c *config.Cluster, host, port string) ([]string, error) {
	return sshOptions(c, host, port, Mode(c))
}

// SSHOptionsVia uses SSHOptions' port selection, probing, caching and error
// behavior with an explicit mode. It bypasses the active-device check so
// pending devices can be verified during registration. mode must be Auto,
// Tunnel or Direct.
func SSHOptionsVia(c *config.Cluster, host, port, mode string) ([]string, error) {
	return sshOptions(c, host, port, mode)
}

func sshOptions(c *config.Cluster, host, port, mode string) ([]string, error) {
	if mode == Direct {
		return nil, nil
	}
	n, ok := NodeForHost(c, host)
	if !ok {
		if mode == Tunnel {
			return nil, fmt.Errorf("%s is not a node this workstation's device reaches. Run 'kip cluster access show' to refresh the node list", host)
		}
		return nil, nil
	}
	p := 22
	if len(n.SSHPorts) > 0 {
		p = n.SSHPorts[0]
	}
	if port != "" {
		v, err := strconv.Atoi(port)
		if err != nil {
			return nil, fmt.Errorf("%q is not an SSH port", port)
		}
		p = v
	}
	if mode == Auto {
		if err := probe(c, n, p); err != nil {
			_, _ = fmt.Fprintf(Warn, "The tunnel to %s did not come up within %s (%v), so kip connects directly. Run 'kip cluster access show' to check the device, or 'kip cluster access via direct' to skip the tunnel.\n", n.Name, EstablishTimeout, err)
			return nil, nil
		}
	}
	cmd, err := proxyCommand(c, n)
	if err != nil {
		return nil, err
	}
	// The tunnel admits only the node's recorded SSH ports, so ssh must ask
	// for the one probed here rather than its default.
	return []string{"ProxyCommand=" + cmd, "Port=" + strconv.Itoa(p)}, nil
}

// proxyCommand is the ssh ProxyCommand that runs this kip binary's proxy for
// the node. It names the device, so a connection through another device
// never shares a multiplexed master with this one.
func proxyCommand(c *config.Cluster, n config.OpsNode) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("finding kip's own binary: %w", err)
	}
	return ProxyCommandFor(exe, c.Name, n.MachineID, c.Ops.Device)
}

// ProxyCommandFor quotes a ProxyCommand for the platform's shell, doubles
// literal percent signs for SSH and appends SSH's %p port placeholder.
// On Windows, arguments containing quotes or line breaks return an error.
func ProxyCommandFor(exe, cluster, machineID, device string) (string, error) {
	quote := quotePOSIX
	if runtime.GOOS == "windows" {
		quote = quoteWindows
	}
	parts := []string{exe, "--cluster", cluster, "cluster", "access", "proxy", "--node", machineID, "--device", device}
	quoted := make([]string, len(parts))
	for i, p := range parts {
		q, err := quote(p)
		if err != nil {
			return "", err
		}
		quoted[i] = strings.ReplaceAll(q, "%", "%%")
	}
	return strings.Join(quoted, " ") + " %p", nil
}

func quotePOSIX(s string) (string, error) {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'", nil
}

func quoteWindows(s string) (string, error) {
	if strings.ContainsAny(s, "\"\r\n") {
		return "", fmt.Errorf("%q cannot be passed to ssh's ProxyCommand on Windows", s)
	}
	return `"` + s + `"`, nil
}

// ConfigureREST sends cfg's API connections through the tunnel when the
// command uses it. The server URL, CA and TLS name stay unchanged. The
// dialer accepts only the API host recorded for the server on port 6443,
// and bypasses proxies. A kubeconfig with a proxy, another host or another
// port keeps the direct path in auto mode and returns an error in tunnel mode.
// Auto mode also falls back with a warning if an API probe fails within
// EstablishTimeout. Probe results are cached for the command's lifetime.
func ConfigureREST(c *config.Cluster, cfg *rest.Config) error {
	return configureREST(c, cfg, Mode(c))
}

// ConfigureRESTVia applies ConfigureREST's routing and fallback rules with
// an explicit mode. It bypasses the active-device check so pending devices
// can be verified during registration. mode must be Auto, Tunnel or Direct.
func ConfigureRESTVia(c *config.Cluster, cfg *rest.Config, mode string) error {
	return configureREST(c, cfg, mode)
}

func configureREST(c *config.Cluster, cfg *rest.Config, mode string) error {
	if mode == Direct {
		return nil
	}
	server, ok := serverNode(c)
	reason := ""
	u, err := url.Parse(cfg.Host)
	switch {
	case !ok:
		reason = "this workstation's device does not know the server node"
	case err != nil || u.Hostname() == "":
		reason = fmt.Sprintf("the kubeconfig's server %q is not a URL", cfg.Host)
	case cfg.Proxy != nil:
		reason = "the kubeconfig names a proxy"
	case !sameHost(u.Hostname(), c.Ops.APIHost):
		reason = fmt.Sprintf("the kubeconfig's server %s is not the API host %s recorded when the device was added", u.Hostname(), c.Ops.APIHost)
	case u.Port() != strconv.Itoa(APIPort):
		reason = fmt.Sprintf("the kubeconfig's server uses port %q, not %d", u.Port(), APIPort)
	}
	if reason != "" {
		if mode == Tunnel {
			return fmt.Errorf("kip cannot send the Kubernetes API through the tunnel: %s", reason)
		}
		return nil
	}
	if mode == Auto {
		if err := probe(c, server, APIPort); err != nil {
			_, _ = fmt.Fprintf(Warn, "The tunnel to the Kubernetes API did not come up within %s (%v), so kip connects directly. Run 'kip cluster access show' to check the device.\n", EstablishTimeout, err)
			return nil
		}
	}
	cfg.Dial = APIDialer(c.Name, c.Ops.APIHost, server.MachineID)
	cfg.Proxy = func(*http.Request) (*url.URL, error) { return nil, nil }
	return nil
}

// APIDialer returns a TCP dialer for apiHost on port 6443 through the cluster
// tunnel. It ignores the network argument and rejects other hosts or ports.
func APIDialer(cluster, apiHost, serverMachineID string) func(ctx context.Context, network, address string) (net.Conn, error) {
	return func(ctx context.Context, _, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		if !sameHost(host, apiHost) || port != strconv.Itoa(APIPort) {
			return nil, fmt.Errorf("the tunnel carries only the Kubernetes API at %s, not %s", net.JoinHostPort(apiHost, strconv.Itoa(APIPort)), address)
		}
		return dialCluster(ctx, cluster, serverMachineID, APIPort)
	}
}

// ClusterAPIDialer returns APIDialer for cluster's recorded API host and
// server node, whatever the mode, for checking the API through the tunnel.
// It returns an error when the device does not know the server.
func ClusterAPIDialer(c *config.Cluster) (func(ctx context.Context, network, address string) (net.Conn, error), error) {
	server, ok := serverNode(c)
	if !ok {
		return nil, errors.New("this workstation's device does not know the server node")
	}
	return APIDialer(c.Name, c.Ops.APIHost, server.MachineID), nil
}

func serverNode(c *config.Cluster) (config.OpsNode, bool) {
	if c == nil || c.Ops == nil {
		return config.OpsNode{}, false
	}
	for _, n := range c.Ops.Nodes {
		if n.Server {
			return n, true
		}
	}
	return config.OpsNode{}, false
}

func sameHost(a, b string) bool {
	if b == "" {
		return false
	}
	x, errX := netip.ParseAddr(strings.Trim(a, "[]"))
	y, errY := netip.ParseAddr(strings.Trim(b, "[]"))
	if errX == nil && errY == nil {
		return x.Unmap() == y.Unmap()
	}
	return strings.EqualFold(strings.TrimSuffix(a, "."), strings.TrimSuffix(b, "."))
}
