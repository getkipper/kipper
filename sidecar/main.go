package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
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
	log.Printf("sidecar listening on %s, forwarding to %s, instance-id=%s", addr, target, instanceID)
	if err := http.ListenAndServe(addr, proxy); err != nil { //nolint:gosec // localhost-only sidecar
		log.Fatalf("server failed: %v", err)
	}
}

func shortHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", h[:4])
}

// newProxy builds the reverse proxy this sidecar serves. Assembled here rather
// than inline in main so a test drives the same construction the pod runs,
// instead of a second one that agrees with itself.
func newProxy(target *url.URL, instanceID string) *httputil.ReverseProxy {
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ModifyResponse = func(resp *http.Response) error {
		resp.Header.Set("X-Instance-ID", instanceID)
		return nil
	}
	proxy.ErrorHandler = clientAwareErrorHandler
	return proxy
}

// clientAwareErrorHandler answers a failed proxy attempt, telling a client that
// went away apart from an upstream that fell over.
//
// The default handler cannot: it logs every failure and answers 502, so a
// browser navigating mid-request, a health probe closing its socket, or a shell
// client that shuts the connection when its input ends all read as the app
// behind this proxy having failed. That is a gateway status nobody can act on
// and a log line that sends whoever is debugging to the wrong side — and any
// alerting counting 5xx counts disconnects as outages.
//
// Nothing is written back when the client is gone, because there is nobody left
// to read it.
func clientAwareErrorHandler(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.Canceled) || errors.Is(r.Context().Err(), context.Canceled) {
		return
	}
	log.Printf("proxy error: %v", err)
	w.WriteHeader(http.StatusBadGateway)
}
