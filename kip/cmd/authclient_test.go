package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/getkipper/kipper/kip/internal/config"
)

func TestAuthRejectedMessage(t *testing.T) {
	t.Run("states both versions when they differ", func(t *testing.T) {
		msg := authRejectedMessage("dev", "v0.4.2")
		assert.Contains(t, msg, "dev")
		assert.Contains(t, msg, "v0.4.2")
		assert.Contains(t, msg, "update kip", "a version difference must point at updating kip, not just re-login")
	})

	t.Run("stays generic when the cluster version is unknown", func(t *testing.T) {
		msg := authRejectedMessage("dev", "")
		assert.NotContains(t, msg, "update kip")
		assert.Contains(t, msg, "not expired", "the fallback still explains the token is not simply expired")
	})

	t.Run("does not point at updating kip when the versions match", func(t *testing.T) {
		msg := authRejectedMessage("v0.4.2", "v0.4.2")
		assert.NotContains(t, msg, "update kip")
	})

	// The whole point of the fix: a rejected-but-valid token must never be
	// reduced to a bare "session expired" that only `kip auth login` would fix.
	t.Run("never reduces the problem to a bare session expiry", func(t *testing.T) {
		for _, m := range []string{authRejectedMessage("dev", "v1"), authRejectedMessage("dev", "")} {
			assert.NotEqual(t, "session expired — run: kip auth login", m)
			assert.Contains(t, m, "not expired")
		}
	})
}

// withHealthServer points healthClient at a test server and returns a cluster
// aimed at it, restoring the client when the test ends.
func withHealthServer(t *testing.T, handler http.HandlerFunc) *config.Cluster {
	t.Helper()
	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)

	original := healthClient
	healthClient = srv.Client()
	t.Cleanup(func() { healthClient = original })

	return &config.Cluster{ConsoleAPIDomain: strings.TrimPrefix(srv.URL, "https://")}
}

func TestConsoleAPIVersion(t *testing.T) {
	t.Run("reads the version from /health", func(t *testing.T) {
		cluster := withHealthServer(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "/health", r.URL.Path)
			_, _ = w.Write([]byte(`{"status":"ok","version":"v0.4.2"}`))
		})
		assert.Equal(t, "v0.4.2", consoleAPIVersion(context.Background(), cluster))
	})

	t.Run("returns empty when the endpoint predates the version field", func(t *testing.T) {
		cluster := withHealthServer(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		})
		assert.Equal(t, "", consoleAPIVersion(context.Background(), cluster))
	})

	t.Run("returns empty on a non-200 response", func(t *testing.T) {
		cluster := withHealthServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		})
		assert.Equal(t, "", consoleAPIVersion(context.Background(), cluster))
	})

	t.Run("returns empty on malformed json", func(t *testing.T) {
		cluster := withHealthServer(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{not json`))
		})
		assert.Equal(t, "", consoleAPIVersion(context.Background(), cluster))
	})

	t.Run("returns empty when the cluster is unreachable", func(t *testing.T) {
		// A cluster that resolves nowhere: the fetch must fail fast to "" rather
		// than hang or panic.
		cluster := &config.Cluster{ConsoleAPIDomain: "127.0.0.1:1"}
		assert.Equal(t, "", consoleAPIVersion(context.Background(), cluster))
	})
}
