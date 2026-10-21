// Package opstunnel carries an operator workstation's WireGuard tunnel to the
// cluster's nodes inside kip's own process, on a userspace network stack. The
// workstation's network interfaces and routes remain unchanged.
package opstunnel

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

const (
	// mtu leaves room for WireGuard overhead on paths with a 1500-byte MTU.
	mtu = 1280
	// keepalive keeps NAT mappings on the workstation's side open.
	keepalive = 25
)

// Node describes a reachable peer. Endpoint is its WireGuard host:port,
// Address is its operator IPv4 address, and Server enables Kubernetes API access.
type Node struct {
	MachineID string
	Name      string
	Endpoint  string
	PublicKey string
	Address   netip.Addr
	SSHPorts  []int
	Server    bool
}

// apiPort is the Kubernetes API port the tunnel admits on the server.
const apiPort = 6443

// Manifest is everything the tunnel is configured from. PrivateKey is
// base64, as wg prints it.
type Manifest struct {
	PrivateKey string
	Address    netip.Addr
	Nodes      []Node
}

// Tunnel is a running WireGuard device on a userspace network stack.
type Tunnel struct {
	dev     *device.Device
	net     *netstack.Net
	address netip.Addr
	mu      sync.Mutex
	nodes   []Node
	applied string
}

// Up starts and configures a userspace WireGuard tunnel with 25-second peer
// keepalives. It returns configuration or startup errors without waiting for a
// successful handshake. A nil logf discards logs. The caller must close the tunnel.
func Up(m Manifest, logf func(string, ...any)) (*Tunnel, error) {
	cfg, err := uapiConfig(m)
	if err != nil {
		return nil, err
	}
	if !m.Address.Is4() {
		return nil, fmt.Errorf("the device address %s is not IPv4", m.Address)
	}
	tdev, tnet, err := netstack.CreateNetTUN([]netip.Addr{m.Address}, nil, mtu)
	if err != nil {
		return nil, fmt.Errorf("starting the tunnel's network stack: %w", err)
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	logger := &device.Logger{Verbosef: device.DiscardLogf, Errorf: logf}
	dev := device.NewDevice(tdev, conn.NewDefaultBind(), logger)
	if err := dev.IpcSet(cfg); err != nil {
		dev.Close()
		return nil, fmt.Errorf("configuring the tunnel: %w", err)
	}
	if err := dev.Up(); err != nil {
		dev.Close()
		return nil, fmt.Errorf("starting the tunnel: %w", err)
	}
	return &Tunnel{dev: dev, net: tnet, address: m.Address, nodes: append([]Node(nil), m.Nodes...), applied: cfg}, nil
}

// Reload updates the tunnel's private key, peers and node manifest. It reuses
// an unchanged WireGuard configuration to preserve established peer sessions.
// It returns configuration errors and rejects address changes, which require
// a new tunnel.
func (t *Tunnel) Reload(m Manifest) error {
	if m.Address != t.address {
		return fmt.Errorf("the device address changed from %s to %s", t.address, m.Address)
	}
	cfg, err := uapiConfig(m)
	if err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if cfg != t.applied {
		if err := t.dev.IpcSet(cfg); err != nil {
			return fmt.Errorf("configuring the tunnel: %w", err)
		}
		t.applied = cfg
	}
	t.nodes = append([]Node(nil), m.Nodes...)
	return nil
}

// uapiConfig renders the manifest in WireGuard's configuration protocol, which
// takes keys in hex. Each node is a peer reachable at its operator address.
func uapiConfig(m Manifest) (string, error) {
	key, err := hexKey(m.PrivateKey)
	if err != nil {
		return "", errors.New("the device's private key is not a WireGuard key")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "private_key=%s\nlisten_port=0\nreplace_peers=true\n", key)
	for _, n := range m.Nodes {
		pub, err := hexKey(n.PublicKey)
		if err != nil {
			return "", fmt.Errorf("node %s has no valid public key", n.Name)
		}
		ep, err := resolveEndpoint(n.Endpoint)
		if err != nil {
			return "", fmt.Errorf("node %s: %w", n.Name, err)
		}
		if !n.Address.Is4() {
			return "", fmt.Errorf("node %s has operator address %s", n.Name, n.Address)
		}
		fmt.Fprintf(&b, "public_key=%s\nendpoint=%s\nallowed_ip=%s/32\npersistent_keepalive_interval=%d\n", pub, ep, n.Address, keepalive)
	}
	return b.String(), nil
}

func hexKey(b64 string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(b) != 32 {
		return "", errors.New("not a WireGuard key")
	}
	return hex.EncodeToString(b), nil
}

// resolveEndpoint turns host:port into an address WireGuard accepts,
// preferring IPv4 when the name resolves to both.
func resolveEndpoint(endpoint string) (string, error) {
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil {
		return "", fmt.Errorf("endpoint %q is not host:port", endpoint)
	}
	if a, err := netip.ParseAddr(host); err == nil {
		return netip.AddrPortFrom(a.Unmap(), mustPort(port)).String(), nil
	}
	addrs, err := net.LookupHost(host)
	if err != nil || len(addrs) == 0 {
		return "", fmt.Errorf("resolving %s: %w", host, err)
	}
	pick := addrs[0]
	for _, a := range addrs {
		if ip, err := netip.ParseAddr(a); err == nil && ip.Is4() {
			pick = a
			break
		}
	}
	a, err := netip.ParseAddr(pick)
	if err != nil {
		return "", fmt.Errorf("resolving %s gave %q", host, pick)
	}
	return netip.AddrPortFrom(a.Unmap(), mustPort(port)).String(), nil
}

func mustPort(s string) uint16 {
	n, _ := strconv.ParseUint(s, 10, 16)
	return uint16(n)
}

// Node returns the manifest entry for machineID, or a zero Node and false
// when the manifest has no matching entry.
func (t *Tunnel) Node(machineID string) (Node, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, n := range t.nodes {
		if n.MachineID == machineID {
			return n, true
		}
	}
	return Node{}, false
}

// allowed reports whether the tunnel may open port on n: one of its SSH
// ports, or the API port on the server.
func allowed(n Node, port int) bool {
	if port == apiPort && n.Server {
		return true
	}
	for _, p := range n.SSHPorts {
		if p == port {
			return true
		}
	}
	return false
}

// Dial opens a TCP connection to port on the node with machineID, through
// the tunnel. It refuses nodes outside the manifest and ports other than the
// node's SSH ports and, on the server, the API port.
func (t *Tunnel) Dial(ctx context.Context, machineID string, port int) (net.Conn, error) {
	n, ok := t.Node(machineID)
	if !ok {
		return nil, fmt.Errorf("machine %s is not a node this device reaches. Run 'kip cluster access show' to refresh the node list", machineID)
	}
	if !allowed(n, port) {
		return nil, fmt.Errorf("the tunnel opens only SSH and the Kubernetes API, not port %d on %s", port, n.Name)
	}
	c, err := t.net.DialContextTCPAddrPort(ctx, netip.AddrPortFrom(n.Address, uint16(port))) //nolint:gosec // allowed checked the port against the node's permitted ports.
	if err != nil {
		return nil, fmt.Errorf("connecting to %s port %d through the tunnel: %w", n.Name, port, err)
	}
	return c, nil
}

// Close stops the tunnel.
func (t *Tunnel) Close() {
	t.dev.Close()
}

// splice copies both ways. When one side stops sending, the other side's
// write half is closed, or the whole connection when it has no half-close,
// and splice returns once both directions have ended.
func splice(a, b io.ReadWriteCloser) {
	done := make(chan struct{}, 2)
	cp := func(dst io.ReadWriteCloser, src io.Reader) {
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		} else {
			_ = dst.Close()
		}
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	<-done
	_ = a.Close()
	_ = b.Close()
}
