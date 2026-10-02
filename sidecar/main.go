package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	listenPort := os.Getenv("LISTEN_PORT")
	if listenPort == "" {
		log.Fatal("LISTEN_PORT is required")
	}

	upstreamPort := os.Getenv("UPSTREAM_PORT")
	if upstreamPort == "" {
		log.Fatal("UPSTREAM_PORT is required")
	}

	podName := os.Getenv("POD_NAME")
	if podName == "" {
		log.Fatal("POD_NAME is required")
	}

	instanceID := shortHash(podName)

	target, err := url.Parse(fmt.Sprintf("http://localhost:%s", upstreamPort))
	if err != nil {
		log.Fatalf("invalid upstream URL: %v", err)
	}

	proxy := newProxy(target, instanceID)

	addr := ":" + listenPort
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("listen on %s: %v", addr, err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	log.Printf("sidecar listening on %s, forwarding to %s, instance-id=%s", addr, target, instanceID)
	err = serve(ctx, ln, proxy)
	stop()
	if err != nil {
		log.Fatalf("server failed: %v", err)
	}
}

// serve answers on ln until ctx is cancelled, then stops accepting and waits
// for in-flight requests to finish. The kubelet's grace period bounds the wait.
func serve(ctx context.Context, ln net.Listener, handler http.Handler) error {
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	errs := make(chan error, 1)
	go func() { errs <- srv.Serve(ln) }()
	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}
	if err := srv.Shutdown(context.WithoutCancel(ctx)); err != nil {
		return err
	}
	if err := <-errs; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func shortHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", h[:4])
}

// newProxy shares proxy construction between main and tests.
func newProxy(target *url.URL, instanceID string) *httputil.ReverseProxy {
	proxy := httputil.NewSingleHostReverseProxy(target) //nolint:gosec // G704: main constructs the target from localhost and UPSTREAM_PORT
	proxy.ModifyResponse = func(resp *http.Response) error {
		resp.Header.Set("X-Instance-ID", instanceID)
		return nil
	}
	proxy.ErrorHandler = clientAwareErrorHandler
	return proxy
}

// clientAwareErrorHandler ignores client cancellations and reports upstream
// failures as 502, keeping disconnects out of upstream error logs.
func clientAwareErrorHandler(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.Canceled) || errors.Is(r.Context().Err(), context.Canceled) {
		return
	}
	log.Printf("proxy error: %v", err)
	w.WriteHeader(http.StatusBadGateway)
}
