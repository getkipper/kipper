package main

import (
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
)

// allowLogSample logs one in this many allows. Denials are always logged;
// steady successful traffic only needs a periodic heartbeat.
const allowLogSample = 100

// maxLoggedPrefix bounds the key prefix written to forensics logs. Issued
// prefixes are 8 chars; anything longer is a malformed client key and is
// logged as "invalid" so an attacker cannot bloat the field.
const maxLoggedPrefix = 16

// maxLoggedForwardedFor bounds the X-Forwarded-For chain written to forensics
// logs so a client-padded header cannot bloat the field.
const maxLoggedForwardedFor = 256

// Server exposes the forwardAuth endpoint plus health and readiness.
type Server struct {
	authorizer *Authorizer
	freshness  *Freshness
	allowSeen  atomic.Uint64
}

// NewServer wires the HTTP layer over the authorizer.
func NewServer(a *Authorizer, f *Freshness) *Server {
	return &Server{authorizer: a, freshness: f}
}

// Routes registers the service's handlers on the mux.
func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /authorize", s.handleAuthorize)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /readyz", s.handleReady)
}

// denialBody is the stable error contract clients program against: code
// is machine-matched, message is for humans and may change.
type denialBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeDenial terminates a request at the gate. Denials never reach the
// app, so its CORS middleware cannot run; without these headers a browser
// client gets an opaque CORS failure instead of the body and Retry-After.
// The bodies carry no secrets, so the wildcard origin is safe.
func writeDenial(w http.ResponseWriter, status int, code, message string, retryAfter time.Duration) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Expose-Headers", "Retry-After")
	if retryAfter > 0 {
		seconds := (retryAfter + time.Second - 1) / time.Second
		w.Header().Set("Retry-After", strconv.FormatInt(int64(seconds), 10))
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(denialBody{Code: code, Message: message})
}

// gateUnavailableRetry is the advice on 503s: probes advance the
// freshness clock far faster than the 90s stale bound, so a short retry
// is realistic without inviting a stampede.
const gateUnavailableRetry = 10 * time.Second

// handleAuthorize is the Traefik forwardAuth target. The gated app's
// identity arrives as query parameters written by the app reconciler into
// the Middleware CR — trusted configuration, never client headers. The
// client's key arrives as X-API-Key.
func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	namespace := r.URL.Query().Get("namespace")
	app := r.URL.Query().Get("app")
	if namespace == "" || app == "" {
		// A gate reaching authz without its identity is a broken Middleware,
		// not a client action; log it loudly so the misconfiguration surfaces.
		slog.Error("authorize misconfigured", slog.String("reason", "missing_identity"), slog.String("client_ip", clientIP(r)))
		writeDenial(w, http.StatusInternalServerError, "misconfigured",
			"authz middleware misconfigured: namespace and app are required", 0)
		return
	}

	// CORS preflights carry no custom headers by spec, so a gated browser
	// API is impossible unless they pass the gate. Only requests shaped
	// like a preflight qualify: OPTIONS plus Access-Control-Request-Method
	// (forwarded by the middleware). A bare OPTIONS probe still needs a
	// key. The app answers its own preflight; no data crosses here and
	// nothing is metered.
	if r.Header.Get("X-Forwarded-Method") == http.MethodOptions &&
		r.Header.Get("Access-Control-Request-Method") != "" {
		metricDecisions.WithLabelValues("preflight").Inc()
		w.WriteHeader(http.StatusOK)
		return
	}

	res := s.authorizer.Authorize(r.Context(), namespace, app, r.Header.Get("X-API-Key"))
	metricDecisions.WithLabelValues(decisionLabel(res.Decision)).Inc()
	s.logDecision(r, namespace, app, res.Decision)

	switch res.Decision {
	case DecisionAllow:
		// Forward the consumer's non-secret identity to the backend. The
		// forwardAuth middleware copies these onto the upstream request and
		// strips any inbound client copies first, so the backend can trust
		// them. The prefix is the stable identifier; the name may be empty.
		w.Header().Set("X-Kipper-Key-Prefix", res.KeyPrefix)
		// The console API rejects a name with control bytes, but a key created
		// by a direct CR write bypasses that. Refuse to forward such a name
		// rather than let it break every request on the route: the prefix is
		// the identifier the backend can always rely on.
		if res.KeyName != "" && headerSafe(res.KeyName) {
			w.Header().Set("X-Kipper-Key-Name", res.KeyName)
		}
		w.WriteHeader(http.StatusOK)
	case DecisionDenyRate:
		writeDenial(w, http.StatusTooManyRequests, "rate_limited",
			"rate limit exceeded for this API key", res.RetryAfter)
	case DecisionDenyQuota:
		writeDenial(w, http.StatusTooManyRequests, "quota_exhausted",
			"usage quota exhausted for this API key", res.RetryAfter)
	case DecisionUnavailable:
		// A distinct, named failure so an operator can tell "authz
		// rejected the key" from "authz could not decide".
		writeDenial(w, http.StatusServiceUnavailable, "gate_unavailable",
			"kipper-authz cannot verify API keys right now (cache stale or syncing, or usage history unreadable); this route fails closed", gateUnavailableRetry)
	default:
		writeDenial(w, http.StatusUnauthorized, "invalid_key", "invalid API key", 0)
	}
}

// logDecision emits a structured forensics line. Denials are always logged
// at warn so key-spraying and quota exhaustion are visible; allows are
// sampled so steady traffic leaves a heartbeat without flooding. The key
// prefix is the non-secret handle; the secret never appears.
func (s *Server) logDecision(r *http.Request, namespace, app string, d Decision) {
	if d == DecisionAllow && s.allowSeen.Add(1)%allowLogSample != 0 {
		return
	}
	// The prefix comes from the client's key, so bound it: a well-formed key
	// yields the short handle, anything else logs "invalid" rather than
	// letting an attacker pad the field with junk.
	prefix, ok := keyPrefix(r.Header.Get("X-API-Key"))
	if !ok || len(prefix) > maxLoggedPrefix {
		prefix = "invalid"
	}
	attrs := []any{
		slog.String("namespace", namespace),
		slog.String("app", app),
		slog.String("key_prefix", prefix),
		slog.String("reason", decisionLabel(d)),
		slog.String("client_ip", clientIP(r)),
	}
	// Keep the full forwarded chain alongside the single best-guess IP. On a
	// path where an external load balancer appends to a client-supplied
	// X-Forwarded-For, the leftmost entry is attacker-influenced; the whole
	// chain lets an analyst spot the framing instead of trusting one value.
	if chain := forwardedChain(r); chain != "" {
		attrs = append(attrs, slog.String("forwarded_for", chain))
	}
	if d == DecisionAllow {
		slog.Info("authorize allow (sampled)", attrs...)
		return
	}
	slog.Warn("authorize deny", attrs...)
}

// clientIP returns the best single guess at the caller's address for
// forensics: the leftmost X-Forwarded-For entry, with RemoteAddr (the Traefik
// pod) as the fallback when the header is absent. The leftmost entry is only
// as trustworthy as the front proxy: it is authoritative when that proxy
// replaces the client-supplied header (the kipper.run gateway does), and
// client-influenced when a --trusted-proxy load balancer appends to it. The
// forwarded_for log field keeps the whole chain so the leftmost is never the
// only evidence.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// forwardedChain returns the X-Forwarded-For chain, bounded so a padded value
// cannot bloat the logs, for cross-checking the single client_ip. It joins
// every header line: a proxy may append the chain as a comma-separated value
// or as repeated header lines, and dropping the repeats would lose the exact
// evidence this field exists to keep.
func forwardedChain(r *http.Request) string {
	chain := strings.Join(r.Header.Values("X-Forwarded-For"), ", ")
	if len(chain) > maxLoggedForwardedFor {
		return chain[:maxLoggedForwardedFor]
	}
	return chain
}

// headerSafe reports whether v can be emitted verbatim as an HTTP header
// value: no control bytes that the transport would reject.
func headerSafe(v string) bool {
	return strings.IndexFunc(v, unicode.IsControl) < 0
}

// handleReady gates Traefik's routing to this replica on the freshness
// contract: initial informer sync completed and a recent successful probe.
func (s *Server) handleReady(w http.ResponseWriter, _ *http.Request) {
	if !s.freshness.Fresh() {
		// Causes are the initial sync still running, a wedged watch, or a
		// deleted freshness canary. The probe logs name the specific one; a
		// missing canary logs the re-apply-the-manifest recovery.
		http.Error(w, "authz cache not fresh (initial sync, a wedged watch, or a missing freshness canary). Check authz logs", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}
