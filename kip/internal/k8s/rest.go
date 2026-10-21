package k8s

import (
	"fmt"
	"net/http"
	"net/url"
	"time"

	"k8s.io/apimachinery/pkg/util/httpstream/spdy"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
	spdytransport "k8s.io/client-go/transport/spdy"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/transport"
)

// RESTConfig loads the cluster's kubeconfig and sends its connections
// through this workstation's device tunnel when the command uses it.
func RESTConfig(cluster *config.Cluster) (*rest.Config, error) {
	cfg, err := clientcmd.BuildConfigFromFlags("", cluster.Kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig %s: %w", cluster.Kubeconfig, err)
	}
	if err := transport.ConfigureREST(cluster, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// SPDYRoundTripperFor honors cfg.Dial for upgraded connections, which
// client-go's spdy.RoundTripperFor ignores. With a custom dialer it bypasses
// proxies and sends pings every five seconds. Otherwise it delegates to
// client-go. TLS, transport and HTTP wrapper configuration errors are returned.
func SPDYRoundTripperFor(cfg *rest.Config) (http.RoundTripper, spdytransport.Upgrader, error) {
	if cfg.Dial == nil {
		return spdytransport.RoundTripperFor(cfg)
	}
	tlsConfig, err := rest.TLSConfigFor(cfg)
	if err != nil {
		return nil, nil, err
	}
	// Bypass proxies so every connection uses the tunnel dialer.
	upgrade, err := spdy.NewRoundTripperWithConfig(spdy.RoundTripperConfig{
		UpgradeTransport: &http.Transport{DialContext: cfg.Dial, TLSClientConfig: tlsConfig},
		PingPeriod:       5 * time.Second,
	})
	if err != nil {
		return nil, nil, err
	}
	wrapper, err := rest.HTTPWrappersForConfig(cfg, upgrade)
	if err != nil {
		return nil, nil, err
	}
	return wrapper, upgrade, nil
}

// NewSPDYExecutor creates a remote command executor using SPDYRoundTripperFor
// so upgraded connections honor cfg.Dial. Transport and executor setup errors
// are returned to the caller.
func NewSPDYExecutor(cfg *rest.Config, method string, u *url.URL) (remotecommand.Executor, error) {
	wrapper, upgrader, err := SPDYRoundTripperFor(cfg)
	if err != nil {
		return nil, err
	}
	return remotecommand.NewSPDYExecutorForTransports(wrapper, upgrader, method, u)
}
