package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"syscall"
	"time"
)

const (
	upstreamCheckMethod = "KIPPER-UPSTREAM-CHECK"
	upstreamHeader      = "X-Kipper-Upstream"
	upstreamDialTimeout = 2 * time.Second
)

// withUpstreamCheck lets the controller check listeners reachable only via localhost.
// Requests with a nonempty X-Forwarded-For header bypass the check so forwarded
// traffic keeps its usual handling.
func withUpstreamCheck(next http.Handler, upstream string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != upstreamCheckMethod || r.Header.Get("X-Forwarded-For") != "" {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set(upstreamHeader, upstreamAnswer(r.Context(), upstream))
		w.WriteHeader(http.StatusNoContent)
	})
}

func upstreamAnswer(ctx context.Context, upstream string) string {
	d := net.Dialer{Timeout: upstreamDialTimeout}
	conn, err := d.DialContext(ctx, "tcp", upstream)
	switch {
	case err == nil:
		_ = conn.Close()
		return "accepting"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "refused"
	default:
		return "unknown"
	}
}
