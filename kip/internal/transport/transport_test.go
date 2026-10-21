package transport

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"

	"github.com/getkipper/kipper/kip/internal/config"
)

func deviceCluster() *config.Cluster {
	return &config.Cluster{Name: "prod", Host: "198.51.100.7", Ops: &config.Ops{
		Device: "laptop", State: config.OpsActive, Address: "10.99.0.129", APIHost: "198.51.100.7",
		Nodes: []config.OpsNode{
			{MachineID: "m-server", Name: "server", Host: "198.51.100.7", Address: "10.99.0.1", SSHPorts: []int{2222}, Server: true},
			{MachineID: "m-worker", Name: "worker", Host: "Worker.Example.com", Address: "10.99.0.2", SSHPorts: []int{22}},
		},
	}}
}

// fakeTunnel stands in for the tunnel process: it records each dial and
// answers with fail, or with one end of a pipe.
type fakeTunnel struct {
	dials []string
	fail  error
}

func (f *fakeTunnel) install(t *testing.T) {
	t.Helper()
	old := dialCluster
	dialCluster = func(_ context.Context, cluster, machineID string, port int) (net.Conn, error) {
		f.dials = append(f.dials, cluster+"/"+machineID+":"+strconv.Itoa(port))
		if f.fail != nil {
			return nil, f.fail
		}
		a, b := net.Pipe()
		go func() { _ = b.Close() }()
		return a, nil
	}
	mu.Lock()
	established = map[string]error{}
	mu.Unlock()
	var warn bytes.Buffer
	oldWarn, oldOverride := Warn, Override
	Warn, Override = &warn, ""
	t.Cleanup(func() { dialCluster, Warn, Override = old, oldWarn, oldOverride })
}

func TestModeIsDirectWithoutAnActiveDevice(t *testing.T) {
	(&fakeTunnel{}).install(t)
	Override = Tunnel
	assert.Equal(t, Direct, Mode(&config.Cluster{Name: "plain"}), "no device, no tunnel, whatever --via says")
	stale := deviceCluster()
	stale.Ops.State = config.OpsStale
	assert.Equal(t, Direct, Mode(stale))
	pending := deviceCluster()
	pending.Ops.State = config.OpsPending
	assert.Equal(t, Direct, Mode(pending), "a pending key is only for its own verification")
}

func TestModePrecedence(t *testing.T) {
	(&fakeTunnel{}).install(t)
	c := deviceCluster()
	assert.Equal(t, Auto, Mode(c))
	c.Ops.Via = Direct
	assert.Equal(t, Direct, Mode(c), "the stored default")
	Override = Tunnel
	assert.Equal(t, Tunnel, Mode(c), "--via wins for one command")
}

func TestNoDeviceTouchesNothing(t *testing.T) {
	f := &fakeTunnel{}
	f.install(t)
	opts, err := SSHOptions(&config.Cluster{Name: "plain", Host: "198.51.100.7"}, "198.51.100.7", "")
	require.NoError(t, err)
	assert.Nil(t, opts)
	cfg := &rest.Config{Host: "https://198.51.100.7:6443"}
	require.NoError(t, ConfigureREST(&config.Cluster{Name: "plain"}, cfg))
	assert.Nil(t, cfg.Dial)
	assert.Nil(t, cfg.Proxy)
	assert.Empty(t, f.dials, "no tunnel was started or dialed")
}

func TestSSHOptionsUseTheTunnelInAutoModeOnceItCarries(t *testing.T) {
	f := &fakeTunnel{}
	f.install(t)
	opts, err := SSHOptions(deviceCluster(), "worker.example.com", "")
	require.NoError(t, err)
	require.Len(t, opts, 2)
	assert.True(t, strings.HasPrefix(opts[0], "ProxyCommand="))
	assert.Equal(t, "Port=22", opts[1])
	assert.Contains(t, opts[0], "--node")
	assert.Contains(t, opts[0], "m-worker")
	assert.Contains(t, opts[0], "laptop")
	assert.True(t, strings.HasSuffix(opts[0], " %p"))
	assert.Equal(t, []string{"prod/m-worker:22"}, f.dials)

	_, err = SSHOptions(deviceCluster(), "worker.example.com", "")
	require.NoError(t, err)
	assert.Len(t, f.dials, 1, "the check runs once per node and command")

	opts, err = SSHOptions(deviceCluster(), "198.51.100.7", "")
	require.NoError(t, err)
	assert.Equal(t, "prod/m-server:2222", f.dials[1], "the probe uses the node's recorded SSH port")
	assert.Contains(t, opts, "Port=2222", "and ssh connects to the same port")
}

func TestSSHOptionsFallBackWhenTheTunnelDoesNotCarry(t *testing.T) {
	f := &fakeTunnel{fail: errors.New("handshake did not complete")}
	f.install(t)
	var warn bytes.Buffer
	Warn = &warn
	opts, err := SSHOptions(deviceCluster(), "worker.example.com", "")
	require.NoError(t, err)
	assert.Nil(t, opts, "auto falls back to the direct path")
	assert.Contains(t, warn.String(), "connects directly")
	assert.Contains(t, warn.String(), "handshake did not complete")
}

func TestSSHOptionsForcedTunnel(t *testing.T) {
	f := &fakeTunnel{fail: errors.New("unreachable")}
	f.install(t)
	Override = Tunnel
	opts, err := SSHOptions(deviceCluster(), "worker.example.com", "")
	require.NoError(t, err)
	require.Len(t, opts, 2, "a forced tunnel never checks and never falls back")
	assert.Empty(t, f.dials)

	_, err = SSHOptions(deviceCluster(), "203.0.113.9", "")
	require.Error(t, err, "a host the device does not reach is refused, not dialed directly")
	assert.Contains(t, err.Error(), "is not a node")

	Override = Auto
	opts, err = SSHOptions(deviceCluster(), "203.0.113.9", "")
	require.NoError(t, err)
	assert.Nil(t, opts, "auto reaches an unknown host directly")
}

func TestSSHOptionsViaUsesAPendingDevice(t *testing.T) {
	(&fakeTunnel{}).install(t)
	c := deviceCluster()
	c.Ops.State = config.OpsPending
	opts, err := SSHOptionsVia(c, "worker.example.com", "", Tunnel)
	require.NoError(t, err)
	assert.Len(t, opts, 2)
}

func TestProxyCommandQuoting(t *testing.T) {
	if runtime.GOOS == "windows" {
		cmd, err := ProxyCommandFor(`C:\Program Files\kip\kip.exe`, "prod", "m-1", "laptop")
		require.NoError(t, err)
		assert.Equal(t, `"C:\Program Files\kip\kip.exe" "--cluster" "prod" "cluster" "access" "proxy" "--node" "m-1" "--device" "laptop" %p`, cmd)
		_, err = ProxyCommandFor(`C:\a"b\kip.exe`, "prod", "m-1", "laptop")
		assert.Error(t, err)
		return
	}
	cmd, err := ProxyCommandFor("/opt/it's 100%/kip", "prod", "m-1", "laptop")
	require.NoError(t, err)
	assert.Equal(t, `'/opt/it'\''s 100%%/kip' '--cluster' 'prod' 'cluster' 'access' 'proxy' '--node' 'm-1' '--device' 'laptop' %p`, cmd)
}

func TestConfigureRESTSendsOnlyTheAPIThroughTheTunnel(t *testing.T) {
	f := &fakeTunnel{}
	f.install(t)
	Override = Tunnel
	cfg := &rest.Config{Host: "https://198.51.100.7:6443"}
	require.NoError(t, ConfigureREST(deviceCluster(), cfg))
	require.NotNil(t, cfg.Dial)
	require.NotNil(t, cfg.Proxy)
	proxy, err := cfg.Proxy(&http.Request{URL: &url.URL{Scheme: "https", Host: "198.51.100.7:6443"}})
	require.NoError(t, err)
	assert.Nil(t, proxy, "no proxy applies, whatever HTTPS_PROXY says")

	conn, err := cfg.Dial(context.Background(), "tcp", "198.51.100.7:6443")
	require.NoError(t, err)
	_ = conn.Close()
	assert.Equal(t, []string{"prod/m-server:6443"}, f.dials)

	for _, addr := range []string{"203.0.113.9:6443", "198.51.100.7:22", "198.51.100.7:443"} {
		_, err := cfg.Dial(context.Background(), "tcp", addr)
		assert.Error(t, err, addr)
	}
	assert.Len(t, f.dials, 1, "refused destinations never reach the tunnel")
}

func TestConfigureRESTKeepsAnUnexpectedKubeconfigDirect(t *testing.T) {
	(&fakeTunnel{}).install(t)
	proxied := &rest.Config{Host: "https://198.51.100.7:6443", Proxy: http.ProxyFromEnvironment}
	other := &rest.Config{Host: "https://api.example.com:6443"}
	for name, cfg := range map[string]*rest.Config{"proxy": proxied, "other host": other} {
		Override = Auto
		dial := cfg.Dial
		require.NoError(t, ConfigureREST(deviceCluster(), cfg), name)
		assert.Nil(t, dial, name)
		assert.Nil(t, cfg.Dial, "auto keeps dialing as the kubeconfig says: "+name)

		Override = Tunnel
		assert.Error(t, ConfigureREST(deviceCluster(), cfg), "a forced tunnel refuses rather than proving nothing: "+name)
	}
}

func TestConfigureRESTFallsBackInAutoMode(t *testing.T) {
	f := &fakeTunnel{fail: errors.New("no handshake")}
	f.install(t)
	cfg := &rest.Config{Host: "https://198.51.100.7:6443"}
	require.NoError(t, ConfigureREST(deviceCluster(), cfg))
	assert.Nil(t, cfg.Dial)
	assert.Equal(t, []string{"prod/m-server:6443"}, f.dials)
}

func TestValidMode(t *testing.T) {
	for _, m := range []string{Auto, Tunnel, Direct} {
		assert.NoError(t, ValidMode(m))
	}
	assert.Error(t, ValidMode("vpn"))
}
