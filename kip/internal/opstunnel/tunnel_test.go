package opstunnel

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

type keyPair struct{ private, public []byte }

func newKeyPair(t *testing.T) keyPair {
	t.Helper()
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	require.NoError(t, err)
	return keyPair{private: k.Bytes(), public: k.PublicKey().Bytes()}
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

var (
	nodeAddr   = netip.MustParseAddr("10.99.0.1")
	deviceAddr = netip.MustParseAddr("10.99.0.129")
)

// startNode runs a WireGuard peer on loopback standing in for a node, with
// an echo server on port 22 of its operator address. It returns the peer's
// UDP port.
func startNode(t *testing.T, node, workstation keyPair) int {
	t.Helper()
	tdev, tnet, err := netstack.CreateNetTUN([]netip.Addr{nodeAddr}, nil, mtu)
	require.NoError(t, err)
	dev := device.NewDevice(tdev, conn.NewDefaultBind(), device.NewLogger(device.LogLevelSilent, ""))
	t.Cleanup(dev.Close)
	require.NoError(t, dev.IpcSet(fmt.Sprintf("private_key=%s\nlisten_port=0\npublic_key=%s\nallowed_ip=%s/32\n",
		hex.EncodeToString(node.private), hex.EncodeToString(workstation.public), deviceAddr)))
	require.NoError(t, dev.Up())
	l, err := tnet.ListenTCPAddrPort(netip.AddrPortFrom(nodeAddr, 22))
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); _ = c.Close() }()
		}
	}()
	cfg, err := dev.IpcGet()
	require.NoError(t, err)
	for _, line := range strings.Split(cfg, "\n") {
		if p, ok := strings.CutPrefix(line, "listen_port="); ok {
			var port int
			_, err := fmt.Sscan(p, &port)
			require.NoError(t, err)
			return port
		}
	}
	t.Fatal("the node peer reported no port")
	return 0
}

func TestTheTunnelCarriesTCPToANode(t *testing.T) {
	node, workstation := newKeyPair(t), newKeyPair(t)
	port := startNode(t, node, workstation)
	tun, err := Up(Manifest{
		PrivateKey: b64(workstation.private),
		Address:    deviceAddr,
		Nodes: []Node{{
			MachineID: "m-1", Name: "node-1", Endpoint: fmt.Sprintf("127.0.0.1:%d", port),
			PublicKey: b64(node.public), Address: nodeAddr, SSHPorts: []int{22},
		}},
	}, t.Logf)
	require.NoError(t, err)
	defer tun.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, err := tun.Dial(ctx, "m-1", 22)
	require.NoError(t, err)
	defer func() { _ = c.Close() }()
	_, err = c.Write([]byte("hello\n"))
	require.NoError(t, err)
	buf := make([]byte, 6)
	_, err = io.ReadFull(c, buf)
	require.NoError(t, err)
	assert.Equal(t, "hello\n", string(buf))
}

func TestDialRefusesWhatTheManifestDoesNotName(t *testing.T) {
	tun := &Tunnel{nodes: []Node{
		{MachineID: "m-server", Name: "server", Address: nodeAddr, SSHPorts: []int{22, 2222}, Server: true},
		{MachineID: "m-worker", Name: "worker", Address: netip.MustParseAddr("10.99.0.2"), SSHPorts: []int{22}},
	}}
	for name, tc := range map[string]struct {
		host string
		port int
		want string
	}{
		"unknown machine":     {"m-other", 22, "is not a node this device reaches"},
		"other port":          {"m-server", 80, "not port 80"},
		"API on a worker":     {"m-worker", 6443, "not port 6443"},
		"name, not machine":   {"server", 22, "is not a node this device reaches"},
		"second SSH port off": {"m-worker", 2222, "not port 2222"},
	} {
		_, err := tun.Dial(context.Background(), tc.host, tc.port)
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), tc.want, name)
	}
}

func TestAllowedPorts(t *testing.T) {
	server := Node{SSHPorts: []int{2222}, Server: true}
	assert.True(t, allowed(server, 2222))
	assert.True(t, allowed(server, apiPort))
	assert.False(t, allowed(server, 22))
	assert.False(t, allowed(Node{SSHPorts: []int{22}}, apiPort))
}

func TestUAPIConfigRefusesABadManifest(t *testing.T) {
	good := newKeyPair(t)
	base := func() Manifest {
		return Manifest{PrivateKey: b64(good.private), Address: deviceAddr, Nodes: []Node{{
			MachineID: "m", Name: "n", Endpoint: "198.51.100.7:443", PublicKey: b64(good.public), Address: nodeAddr,
		}}}
	}
	cfg, err := uapiConfig(base())
	require.NoError(t, err)
	assert.Contains(t, cfg, "endpoint=198.51.100.7:443\nallowed_ip=10.99.0.1/32\n")
	assert.Contains(t, cfg, "replace_peers=true\n")
	assert.NotContains(t, cfg, b64(good.private), "the configuration carries keys in hex")

	for name, mutate := range map[string]func(*Manifest){
		"private key":  func(m *Manifest) { m.PrivateKey = "x" },
		"node key":     func(m *Manifest) { m.Nodes[0].PublicKey = "x" },
		"endpoint":     func(m *Manifest) { m.Nodes[0].Endpoint = "198.51.100.7" },
		"IPv6 address": func(m *Manifest) { m.Nodes[0].Address = netip.MustParseAddr("2001:db8::1") },
	} {
		m := base()
		mutate(&m)
		_, err := uapiConfig(m)
		assert.Error(t, err, name)
	}
}

func TestResolveEndpointKeepsIPv6Literals(t *testing.T) {
	ep, err := resolveEndpoint("[2001:db8::7]:443")
	require.NoError(t, err)
	assert.Equal(t, "[2001:db8::7]:443", ep)
	ep, err = resolveEndpoint("[::ffff:198.51.100.7]:443")
	require.NoError(t, err)
	assert.Equal(t, "198.51.100.7:443", ep)
}

func TestSpliceClosesBothSides(t *testing.T) {
	a1, a2 := net.Pipe()
	b1, b2 := net.Pipe()
	done := make(chan struct{})
	go func() { splice(a2, b1); close(done) }()
	go func() { _, _ = io.Copy(io.Discard, b2) }()
	_ = a1.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("splice kept running after one side closed")
	}
	_, err := b2.Write([]byte("x"))
	assert.Error(t, err, "the other side is closed too")
}

func TestReloadReplacesTheNodes(t *testing.T) {
	node, workstation := newKeyPair(t), newKeyPair(t)
	m := Manifest{PrivateKey: b64(workstation.private), Address: deviceAddr, Nodes: []Node{{
		MachineID: "m-1", Name: "node-1", Endpoint: "127.0.0.1:9", PublicKey: b64(node.public), Address: nodeAddr, SSHPorts: []int{22},
	}}}
	tun, err := Up(m, t.Logf)
	require.NoError(t, err)
	defer tun.Close()

	m.Nodes[0].MachineID = "m-2"
	require.NoError(t, tun.Reload(m))
	_, ok := tun.Node("m-1")
	assert.False(t, ok, "a node left out of the manifest is gone")
	_, ok = tun.Node("m-2")
	assert.True(t, ok)

	m.Address = netip.MustParseAddr("10.99.0.130")
	assert.Error(t, tun.Reload(m), "the device address is fixed for the process")
	m.Address = deviceAddr
	m.Nodes[0].PublicKey = "x"
	assert.Error(t, tun.Reload(m))
	_, ok = tun.Node("m-2")
	assert.True(t, ok, "a refused manifest leaves the nodes as they were")
}
