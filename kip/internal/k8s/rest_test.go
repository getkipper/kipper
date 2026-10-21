package k8s

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/transport/spdy"
)

// tunnelConfig points at a TLS server whose address the dialer alone knows:
// the config names a host that does not resolve, so a connection that
// arrives went through the dialer.
func tunnelConfig(t *testing.T) (*rest.Config, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	var upgrades atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "" {
			upgrades.Add(1)
		}
		http.Error(w, "no upgrade here", http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)
	var dials atomic.Int32
	cfg := &rest.Config{
		Host:            "https://api.invalid:6443",
		TLSClientConfig: rest.TLSClientConfig{Insecure: true},
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			dials.Add(1)
			var d net.Dialer
			return d.DialContext(ctx, network, srv.Listener.Addr().String())
		},
		Proxy: func(*http.Request) (*url.URL, error) { return nil, nil },
	}
	return cfg, &dials, &upgrades
}

func TestExecGoesThroughTheConfiguredDialer(t *testing.T) {
	cfg, dials, upgrades := tunnelConfig(t)
	u, err := url.Parse(cfg.Host + "/api/v1/namespaces/default/pods/p/exec?command=true&stdout=true")
	require.NoError(t, err)
	exec, err := NewSPDYExecutor(cfg, "POST", u)
	require.NoError(t, err)
	err = exec.StreamWithContext(context.Background(), remotecommand.StreamOptions{Stdout: &discard{}})
	require.Error(t, err, "the fake server refuses the upgrade")
	assert.Positive(t, dials.Load(), "the upgrade was dialed through the tunnel dialer")
	assert.Positive(t, upgrades.Load(), "and reached the server as an upgrade request")
}

func TestPortForwardGoesThroughTheConfiguredDialer(t *testing.T) {
	cfg, dials, upgrades := tunnelConfig(t)
	rt, upgrader, err := SPDYRoundTripperFor(cfg)
	require.NoError(t, err)
	u, err := url.Parse(cfg.Host + "/api/v1/namespaces/default/pods/p/portforward")
	require.NoError(t, err)
	_, _, err = spdy.NewDialer(upgrader, &http.Client{Transport: rt}, "POST", u).Dial("portforward.k8s.io")
	require.Error(t, err)
	assert.Positive(t, dials.Load())
	assert.Positive(t, upgrades.Load())
}

func TestSPDYWithoutADialerIsClientGosOwn(t *testing.T) {
	rt, upgrader, err := SPDYRoundTripperFor(&rest.Config{Host: "https://198.51.100.7:6443"})
	require.NoError(t, err)
	assert.NotNil(t, rt)
	assert.NotNil(t, upgrader)
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
