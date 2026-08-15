package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/httprate"

	"github.com/getkipper/kipper/controller/pkg/hopproof"
	"github.com/getkipper/kipper/controller/pkg/hostnames"
	"github.com/getkipper/kipper/controller/pkg/pubip"
	"github.com/getkipper/kipper/controller/pkg/spki"
	"github.com/getkipper/kipper/gateway/internal/registry"
)

// The registrable-label shape (LabelPattern), the reserved-name set
// (ReservedLabels), and the separator all come from the shared hostnames
// package, so the gateway's registration guard and the ClusterIdentity CRD
// enforce one definition instead of drifting copies.

// Derived per-cluster routes (console--<cluster>, dex--<cluster>,
// <app>--<cluster>) join the service prefix and the cluster label with a
// double dash, and the proxy resolves them by looking up the segment after
// the last "--". Keeping "--" out of registered labels means no
// registration can ever equal a derived hostname, so nobody can shadow
// another cluster's console, dex, or app URL — in either registration
// order. The separator is sourced from the shared hostnames package so the
// gateway and the host renderer can never disagree on it.
const derivedRouteSeparator = hostnames.DerivedRouteSeparator

type registerRequest struct {
	Subdomain string `json:"subdomain"`
	IP        string `json:"ip"`
	// CertFingerprint is the SPKI SHA-256 the cluster asserts for the
	// gateway→cluster hop, authenticated by Token. Both are optional: a plain
	// {subdomain, ip} naming an existing registration carries neither and is
	// answered without proving anything, refreshing nothing and moving nothing.
	// The first registration happens before the cluster exists (kip install), so
	// a fingerprint on a creating request is ignored — the cluster asserts it in
	// a follow-up call authenticated by the token this one returns.
	CertFingerprint string `json:"certFingerprint,omitempty"`
	Token           string `json:"token,omitempty"`
}

// Pin states reported to the cluster. Their presence in a response also tells
// the cluster this gateway supports pinning at all; an older gateway omits
// the field and the cluster keeps heartbeating without pin semantics.
const (
	pinNone    = "none"
	pinActive  = "active"
	pinPending = "pending"
)

type registerResponse struct {
	Subdomain string `json:"subdomain"`
	Domain    string `json:"domain"`
	// Only set on a new registration. Nothing else echoes the token: an
	// anonymous request naming an existing registration has proven nothing, and
	// the label and address it names are both readable from public DNS.
	Token string `json:"token,omitempty"`
	// Pin reports the entry's pin state after this request. On a pending
	// assertion the asserted and observed fingerprints ride along so the
	// mismatch is debuggable from cluster-side logs alone.
	Pin                 string `json:"pin,omitempty"`
	AssertedFingerprint string `json:"assertedFingerprint,omitempty"`
	ObservedFingerprint string `json:"observedFingerprint,omitempty"`
	// Challenge is a fresh proof-of-possession nonce (B16) for the token
	// holder to sign with the hop-cert private key and submit to /proof. Set
	// only when a valid token accompanied the request. Its absence tells an
	// older cluster this gateway wants no proof; the cluster keeps heartbeating.
	Challenge string `json:"challenge,omitempty"`
}

// proofRequest carries a signed challenge: the token holder proves it possesses
// the private key the gateway observes at the registered IP:443.
type proofRequest struct {
	Subdomain string `json:"subdomain"`
	Token     string `json:"token"`
	Nonce     string `json:"nonce"`
	Signature string `json:"signature"`
}

type proofResponse struct {
	Proven      bool   `json:"proven"`
	ProofExpiry string `json:"proofExpiry,omitempty"`
	Error       string `json:"error,omitempty"`
}

type deregisterRequest struct {
	Token string `json:"token"`
}

type pingRequest struct {
	Token string `json:"token"`
}

// clientIPHeader is the header Caddy overwrites with the true client address on
// every request. The gateway trusts this one and nothing else.
const clientIPHeader = "X-Real-IP"

// rateLimitPrefix is the IPv6 prefix a rate-limit bucket covers. A single client
// is routinely delegated a /64 and can pick any address inside it at will, so
// keying on the full address would hand an IPv6 caller a fresh quota per request.
const rateLimitPrefix = 64

// unmeasuredLogged throttles the no-client-measured warning to one line per
// unmeasuredLogInterval.
var unmeasuredLogged atomic.Int64

// unmeasuredLogInterval spaces that warning out.
const unmeasuredLogInterval = 5 * time.Minute

// rateLimitKey buckets a request by the client Caddy measured: an IPv4 address
// on its own, an IPv6 address by its /64. Requests with no measurable client
// share the empty bucket, which rate limits them rather than exempting them.
func rateLimitKey(r *http.Request) (string, error) {
	addr := chimw.GetClientIPAddr(r.Context())
	if !addr.IsValid() {
		// In the intended deployment this cannot happen: Caddy overwrites the
		// header on every request. If it does, every API caller is sharing one
		// 30-per-minute bucket, which looks like the platform throttling itself
		// for no reason — so name the cause rather than leave it to be guessed.
		now := time.Now().Unix()
		if last := unmeasuredLogged.Load(); now-last > int64(unmeasuredLogInterval.Seconds()) &&
			unmeasuredLogged.CompareAndSwap(last, now) {
			log.Printf("no client address on an API request: %s is missing or unparseable, so these requests share one rate-limit bucket. Check the reverse proxy in front of the gateway", clientIPHeader)
		}
		return "", nil
	}
	if addr.Is4() || addr.Is4In6() {
		return addr.String(), nil
	}
	prefix, err := addr.Prefix(rateLimitPrefix)
	if err != nil {
		// Unreachable for a valid address, but a key is still owed: fall back to
		// the full address rather than dropping the caller into the shared bucket.
		return addr.String(), nil //nolint:nilerr // a rate-limit key must always be produced
	}
	return prefix.String(), nil
}

var dataPath string

func main() {
	reg := registry.New()
	baseDomain := os.Getenv("BASE_DOMAIN")
	if baseDomain == "" {
		baseDomain = "kipper.run"
	}

	dataPath = os.Getenv("DATA_PATH")
	if dataPath == "" {
		dataPath = "/var/lib/kipper-gateway/registry.json"
	}

	if err := reg.LoadFrom(dataPath); err != nil {
		log.Fatalf("failed to load registry from %s: %v", dataPath, err)
	}
	pruned := false
	if dropped := reg.Prune(isPublicIP); dropped > 0 {
		log.Printf("dropped %d persisted registration(s) with non-public IPs", dropped)
		pruned = true
	}
	// Re-apply the whole current admission rule to the loaded state, so a policy
	// tightened since the snapshot was written takes effect on the names already
	// in it. Registration-time enforcement alone would protect only unused names:
	// a label reserved by a later build, or one spelling somebody else's address,
	// would keep serving for as long as its holder renewed it. Re-applied every
	// startup and safe to run when nothing matches.
	if dropped := reg.PruneEntries(registrableEntry); dropped > 0 {
		log.Printf("dropped %d persisted registration(s) the current label policy refuses", dropped)
		pruned = true
	}
	// Named, not acted on. Each of these is either a cluster that moved servers
	// and kept its name, which must keep serving, or a label taken from another
	// server before the registration guard existed, which an operator can release
	// by hand. Only an operator can tell which.
	if flagged := reg.FlagEntries(addressMismatch); len(flagged) > 0 {
		log.Printf("%d registration(s) carry a label spelling an address they do not point at: %s. "+
			"Each is a cluster that changed servers, or a name claimed from another server before this was refused. "+
			"They keep serving; release one by hand if it is the latter",
			len(flagged), strings.Join(flagged, ", "))
	}
	if pruned {
		if err := reg.SaveTo(dataPath); err != nil {
			log.Printf("failed to persist pruned registry: %v", err)
		}
	}
	log.Printf("loaded %d registration(s) from %s", reg.Count(), dataPath)

	// Proof-before-route (B16): a registration routes only once its holder has
	// proven possession of the key served at the address it registered. On
	// unless explicitly disabled, because an operator who has never heard of the
	// setting should get the protection: with it off, anyone who can register a
	// label can point it at any address and have the gateway serve traffic there.
	//
	// Turning it off is the transition mode, for a fleet that has not acquired
	// proofs yet: proofs are still recorded and routing stays fail-open (B5).
	// The way back on is to audit the detail endpoint for zero unproven entries
	// and unset this.
	enforceProof := boolEnvDefaultTrue("KIPPER_PROOF_BEFORE_ROUTE")
	reg.EnforceProof = enforceProof
	if enforceProof {
		log.Printf("proof-before-route ENABLED: only clusters that have proven control will route")
	} else {
		log.Printf("proof-before-route DISABLED by KIPPER_PROOF_BEFORE_ROUTE (transition mode): recording proofs, routing unchanged. An unproven registration routes to whatever address it named")
	}

	// Data-plane budgets. The API limiter below cannot cover proxied traffic:
	// the proxy middleware short-circuits the chain for every registered host,
	// so nothing registered after it ever runs. Defaults are sized for browsing
	// rather than for API calls; set either to 0 to meter at the edge instead.
	clientRPM := intEnv("KIPPER_DATA_PLANE_RPM", defaultDataPlaneRPM)
	clusterInFlight := intEnv("KIPPER_CLUSTER_INFLIGHT", defaultClusterInFlight)
	var clientBudget *httprate.RateLimiter
	if clientRPM > 0 {
		clientBudget = httprate.NewRateLimiter(clientRPM, time.Minute)
		log.Printf("data plane: %d requests/minute per client address, %d concurrent per cluster",
			clientRPM, clusterInFlight)
	} else {
		log.Printf("data plane: per-client rate limiting disabled, %d concurrent per cluster", clusterInFlight)
	}

	proxy := &Proxy{
		Registry:        reg,
		BaseDomain:      baseDomain,
		EnforceProof:    enforceProof,
		ClientBudget:    clientBudget,
		ClusterInFlight: int64(clusterInFlight),
		// A handshake-time pin promotion happens on the data plane; flush it
		// to disk off the request path so a restart keeps the promotion.
		OnPinChange: func() {
			go func() {
				if _, err := reg.FlushIfDirty(dataPath); err != nil {
					log.Printf("failed to persist promoted pin: %v", err)
				}
			}()
		},
	}

	// The posture endpoint is gated on an operator-set token; with none set it
	// does not exist. Monitoring needs it (see gateway/OPERATING.md), so a
	// gateway with no token configured says so once at startup rather than
	// leaving a silent gap.
	statusToken := strings.TrimSpace(os.Getenv("KIPPER_STATUS_TOKEN"))
	if statusToken == "" {
		log.Printf("KIPPER_STATUS_TOKEN is not set, so /status is disabled: nothing can poll the unpinned and unproven counters")
	}

	r := newRouter(reg, proxy, baseDomain, statusToken)

	// Start periodic cleanup of expired subdomains and a periodic flush that
	// persists ping-driven LastSeen updates.
	go cleanupLoop(reg, proxy)
	go flushLoop(reg)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("gateway listening on :%s (domain: *.%s)", port, baseDomain) //nolint:gosec // trusted config values
	// ReadHeaderTimeout bounds how long a client may take to send request
	// headers, closing the Slowloris vector on the one binary that takes
	// raw public traffic. No WriteTimeout: the proxy serves long-lived
	// streaming responses that a write deadline would cut off.
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
	}
	// Serve until a signal arrives, then drain. This process is the only path to
	// every cluster behind it, so a restart that drops connections mid-flight is
	// visible to every user of every one of them.
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		log.Fatalf("listening on %s: %v", srv.Addr, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err = serve(ctx, srv, ln, stop, func() error {
		_, ferr := reg.FlushIfDirty(dataPath)
		return ferr
	})
	// Not deferred: log.Fatalf below exits the process without running defers.
	stop()
	if err != nil {
		log.Fatalf("server failed: %v", err)
	}
	log.Printf("gateway stopped")
}

// shutdownGrace bounds the drain. It must stay below the container's stop grace
// period (see docker-compose.yml) or the runtime SIGKILLs the process mid-drain
// and the shutdown flush never runs.
const shutdownGrace = 20 * time.Second

// serve runs srv on ln until ctx is cancelled, drains in-flight requests, then
// flushes. It returns only a serving failure. A drain that overruns is reported
// and closed out rather than raised, because by then the process is leaving
// either way; flush runs on the shutdown path, which is the path where unwritten
// state exists to lose.
//
// The flush matters as much as the drain: the registry holds ping-driven
// LastSeen updates the periodic flush has not written yet, and losing them ages
// registrations towards expiry that were in fact alive.
//
// disarmSignals restores default signal handling once the first signal has been
// taken, so a second Ctrl-C or SIGTERM during the drain kills the process
// instead of being swallowed.
//
// Known gap: hijacked connections — what the proxied WebSocket log and terminal
// streams become — are outside all of this. Shutdown neither waits for them nor
// closes them, and neither does Close, so they end when the process does. They
// get no grace period, which is what they had before any of this existed;
// covering them needs per-connection tracking through ConnState.
func serve(ctx context.Context, srv *http.Server, ln net.Listener, disarmSignals func(), flush func() error) error {
	serveErr := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
		close(serveErr)
	}()

	select {
	case err := <-serveErr:
		if err != nil {
			return err
		}
		// Serve returned ErrServerClosed without anyone asking, which means the
		// listener is gone; there is nothing to drain.
		return nil
	case <-ctx.Done():
		if disarmSignals != nil {
			disarmSignals()
		}
		log.Printf("shutting down: draining in-flight requests for up to %s", shutdownGrace)
	}

	drainCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := srv.Shutdown(drainCtx); err != nil {
		log.Printf("drain did not finish (%v); closing remaining connections", err)
		if err := srv.Close(); err != nil {
			log.Printf("closing listeners: %v", err)
		}
	}
	// Wait for the serving goroutine to actually leave before flushing, so
	// nothing is still writing to the registry, and keep what it reported rather
	// than discarding it. A real failure and the cancellation can become ready
	// together and the select above picks between ready arms at random, so the
	// cancellation arm can win with a genuine failure already buffered; throwing
	// it away here would report a clean exit for a server that broke. There is
	// no deterministic test for that ordering through this seam: forcing the
	// failure first makes the select take the other arm, and cancelling first
	// makes Serve return ErrServerClosed, which is filtered out by design.
	failure := <-serveErr

	if flush != nil {
		if err := flush(); err != nil {
			log.Printf("failed to persist registry on shutdown: %v", err)
		}
	}
	return failure
}

// defaultDataPlaneRPM is the per-client-address budget for proxied traffic. A
// page load is dozens of requests, so this is generous by design: it exists to
// stop one address flooding the hop, not to shape ordinary browsing. The API
// limiter's 30/minute would be absurd here.
const defaultDataPlaneRPM = 600

// defaultClusterInFlight caps concurrent proxied requests to one cluster. This
// is the control a per-client budget cannot provide, because a distributed
// source defeats per-address counting while a single destination still absorbs
// every request — and on this hop a request can cost a fresh TLS handshake.
const defaultClusterInFlight = 128

// intEnv reads a non-negative integer from the environment, falling back to def
// when unset, unparseable, or negative.
func intEnv(name string, def int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < 0 {
		log.Printf("%s=%q is not a non-negative integer; using %d", name, raw, def)
		return def
	}
	return v
}

// newRouter wires the middleware chain and the API routes. It exists as its own
// function so a test can exercise the real stack: the proxy middleware
// short-circuits the chain for every registered host, so which protections run
// on proxied traffic is decided by this ordering and by nothing else, and a unit
// test of any single middleware cannot see a regression in it.
func newRouter(reg *registry.Registry, proxy *Proxy, baseDomain, statusToken string) http.Handler {
	r := chi.NewRouter()
	r.Use(chimw.Recoverer)
	// The gateway runs behind Caddy (see docker-compose.yml and Caddyfile) and
	// has no published port, so every request arrives through the proxy and the
	// TCP peer is always Caddy. Caddy overwrites X-Real-IP with the true client
	// on every request, so that one header — named explicitly, never a list of
	// headers a client could also set — is the only trustworthy source of the
	// client IP here. Anything else would let a client pick its own rate-limit
	// bucket and its own entry in a cluster's logs.
	r.Use(chimw.ClientIPFromHeader(clientIPHeader))

	// Proxy middleware — intercepts requests to registered subdomains
	// and reverse-proxies them. All other requests fall through to the API.
	r.Use(proxy.Middleware)

	// API routes, rate limited per client. The key comes from the measured
	// client rather than RemoteAddr, which is Caddy for every request and would
	// throttle the whole platform as one bucket. A request whose client could
	// not be measured shares the empty key: it is rate limited, not exempt.
	r.Use(httprate.Limit(30, 1*time.Minute, httprate.WithKeyFuncs(rateLimitKey)))

	r.Get("/health", handleHealth())
	// Registered only when configured. Leaving the route in place would answer
	// an unsupported method with 405 while an unknown path answers 404, which
	// distinguishes it however the handler behaves.
	if statusToken != "" {
		r.Get("/status", handleStatus(reg, statusToken))
	}

	r.Post("/register", handleRegister(reg, baseDomain, observeClusterSPKI))
	r.Delete("/register", handleDeregister(reg, proxy))
	r.Post("/ping", handlePing(reg))
	r.Post("/proof", handleProof(reg, baseDomain, observeClusterKey))

	return r
}

// maxRequestBody caps the API request bodies. The largest legitimate payload
// is a subdomain, an IP, a token, and a fingerprint — well under 4KB.
const maxRequestBody = 4 << 10

// handleHealth is the anonymous liveness answer and nothing more. It used to
// carry the posture too — whether proof-before-route was enforcing, and how many
// registrations were unpinned or unproven — on a path Caddy proxies without
// filtering, which told anyone asking exactly when the gateway was fail-open and
// which registrations to aim at. The counters moved to handleStatus.
func handleHealth() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		respondJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	}
}

// bearerToken returns the credential from an Authorization header, and whether
// the header actually carried the Bearer scheme. Trimming the prefix without
// checking it would accept the bare secret as a header value, which is a
// different credential format than the one advertised.
func bearerToken(header string) (string, bool) {
	const scheme = "Bearer "
	if len(header) < len(scheme) || !strings.EqualFold(header[:len(scheme)], scheme) {
		return "", false
	}
	credential := strings.TrimSpace(header[len(scheme):])
	return credential, credential != ""
}

// handleStatus reports the posture the cutover audit and monitoring need: how
// many registrations proxy without an enforced pin, how many are not routable,
// how long the oldest of each has been that way, and whether enforcement is on.
// A poller can tell a normal seconds-long convergence from a cluster stuck in
// grace for days.
//
// Gated on a token the operator sets. With no token configured the endpoint
// answers 404 rather than 401: an unconfigured gateway should not advertise that
// it has one.
func handleStatus(reg *registry.Registry, token string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if token == "" {
			http.NotFound(w, r)
			return
		}
		presented, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok || subtle.ConstantTimeCompare([]byte(presented), []byte(token)) != 1 {
			respondError(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		unpinned, oldest := reg.UnpinnedSummary()
		unproven, unprovenOldest := reg.UnprovenSummary()
		respondJSON(w, http.StatusOK, map[string]any{
			"status":                  "ok",
			"registrations":           reg.Count(),
			"unpinned":                unpinned,
			"unpinned_oldest_seconds": int64(oldest.Seconds()),
			// The zero-unproven gate for the proof-before-route cutover reads
			// these: how many active registrations are not routable (never
			// proven or lease-expired), and the oldest one's age.
			"unproven":                unproven,
			"unproven_oldest_seconds": int64(unprovenOldest.Seconds()),
			"proof_before_route":      reg.EnforceProof,
		})
	}
}

// observeFunc dials a cluster and returns the SPKI SHA-256 fingerprint of the
// leaf certificate it serves. Injected into handleRegister so tests can stub
// the network.
type observeFunc func(ip, sni string) (string, error)

func handleRegister(reg *registry.Registry, baseDomain string, observe observeFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
		var req registerRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondError(w, http.StatusBadRequest, "invalid request body")
			return
		}

		if !hostnames.LabelPattern.MatchString(req.Subdomain) {
			respondError(w, http.StatusBadRequest,
				"subdomain must be lowercase alphanumeric with optional hyphens, 1-63 characters")
			return
		}

		if strings.Contains(req.Subdomain, derivedRouteSeparator) {
			respondError(w, http.StatusBadRequest,
				"subdomain must not contain '--' (reserved for per-cluster service routes)")
			return
		}

		if hostnames.ReservedLabels[req.Subdomain] {
			respondError(w, http.StatusConflict, "subdomain is reserved")
			return
		}

		if !isPublicIP(req.IP) {
			respondError(w, http.StatusBadRequest, "ip must be a public address")
			return
		}

		// A label shaped like an address belongs to that address. It is the name
		// an install derives for a server by default, so letting anyone hold it
		// means holding the default name of a machine they do not run: the
		// operator who installs there later finds it taken, and until then every
		// link under it points wherever the holder chose. Checked after the
		// public-IP guard, so the address compared against is one the gateway
		// would route to.
		if hostnames.IPShapedLabel(req.Subdomain) && req.Subdomain != hostnames.LabelForIP(req.IP) {
			respondError(w, http.StatusConflict,
				"a subdomain that spells an IP address may only be registered by that address")
			return
		}

		entry, outcome, err := reg.Register(req.Subdomain, req.IP, req.Token)
		if err != nil {
			if errors.Is(err, registry.ErrSubdomainTaken) {
				respondError(w, http.StatusConflict, err.Error())
				return
			}
			// The registry failed at its own job — running out of entropy while
			// minting the token, in practice. Answering 409 would tell the caller
			// the name is held by someone else, and an operator acts on that by
			// renaming or abandoning a name that is free.
			log.Printf("registering %s failed: %v", req.Subdomain, err) //nolint:gosec // subdomain validated above
			respondError(w, http.StatusInternalServerError, "could not complete the registration")
			return
		}
		if outcome == registry.Moved {
			log.Printf("registration %s moved to %s: pin and proof cleared, awaiting re-assertion", //nolint:gosec // values validated above
				entry.Subdomain, entry.IP)
		}

		resp := registerResponse{
			Subdomain: entry.Subdomain,
			Domain:    entry.Subdomain + "." + baseDomain,
		}

		// Disclose the management token only when creating a new registration.
		// Every other outcome either already holds it or has proven nothing, and
		// echoing it would hand control to anyone who can read a public DNS name.
		// A fingerprint on a creating request is deliberately not processed:
		// this request was authenticated by nothing, so the cluster must
		// assert the pin in a second call carrying the token returned here.
		if outcome == registry.Created {
			resp.Token = entry.Token
			resp.Pin = pinNone
			if err := reg.SaveTo(dataPath); err != nil {
				log.Printf("failed to persist registry: %v", err)
			}
			respondJSON(w, http.StatusCreated, resp)
			return
		}

		// Issue a fresh proof-of-possession challenge to the token holder
		// (B16). IssueChallenge is token-gated, so a plain unauthenticated
		// renewal gets none. The cluster signs it and calls /proof.
		// A caller reads the absence of a challenge as its token having been
		// rejected, so an internal failure to mint one must not be served as a
		// success — on a move the registration has already been changed by this
		// point, and the caller would be told the name belongs to someone else.
		nonce, accepted, challengeErr := reg.IssueChallenge(entry.Subdomain, req.Token)
		if challengeErr != nil {
			log.Printf("could not issue a proof challenge for %s: %v", entry.Subdomain, challengeErr)
			respondError(w, http.StatusInternalServerError, "could not issue a proof challenge; retry")
			return
		}
		if accepted {
			resp.Challenge = nonce
		}

		if req.CertFingerprint == "" {
			if reg.PinState(entry.Subdomain).Pinned() {
				resp.Pin = pinActive
			} else {
				resp.Pin = pinNone
			}
			if err := reg.SaveTo(dataPath); err != nil {
				log.Printf("failed to persist registry: %v", err)
			}
			respondJSON(w, http.StatusCreated, resp)
			return
		}

		handlePinAssert(w, reg, entry, req, baseDomain, observe, &resp)
	}
}

// handlePinAssert processes a token-authenticated fingerprint assertion on a
// renewal. Activation requires observation: the gateway dials the cluster
// once and compares what it serves against the assertion. Observed equals
// asserted → the pin activates (or rotates) immediately. Anything else —
// mismatch or failed dial — parks the fingerprint as pending, accepted
// alongside the enforced pin but never displacing it, and answers 202 so the
// cluster's retry loop converges once Traefik serves the asserted key. Pin
// transitions are persisted before they are acknowledged; a save failure is a
// 500 with the dirty flag retained, so the periodic flush retries and a
// restart cannot silently drop back to weaker pin state.
func handlePinAssert(w http.ResponseWriter, reg *registry.Registry, entry *registry.Entry,
	req registerRequest, baseDomain string, observe observeFunc, resp *registerResponse) {
	if !registry.ValidFingerprint(req.CertFingerprint) {
		respondError(w, http.StatusBadRequest, "certFingerprint must be a lowercase-hex SHA-256")
		return
	}

	switch reg.AssertPin(entry.Subdomain, req.Token, req.CertFingerprint) {
	case registry.AssertInvalidToken:
		respondError(w, http.StatusForbidden, "invalid token")
		return
	case registry.AssertActive:
		resp.Pin = pinActive
		respondPinResult(w, reg, http.StatusCreated, resp)
		return
	case registry.AssertNeedsDial:
	}

	observed, err := observe(entry.IP, entry.Subdomain+"."+baseDomain)
	if err != nil {
		log.Printf("pin verification dial for %s failed: %v", entry.Subdomain, err)
	}

	if err == nil && registry.FingerprintsEqual(observed, req.CertFingerprint) {
		if !reg.ActivatePin(entry.Subdomain, req.Token, req.CertFingerprint) {
			respondError(w, http.StatusForbidden, "invalid token")
			return
		}
		log.Printf("pin activated for %s: SPKI %s observed and enforced", entry.Subdomain, req.CertFingerprint)
		resp.Pin = pinActive
		respondPinResult(w, reg, http.StatusCreated, resp)
		return
	}

	if !reg.StorePendingPin(entry.Subdomain, req.Token, req.CertFingerprint) {
		respondError(w, http.StatusForbidden, "invalid token")
		return
	}
	// A persistent asserted≠observed mismatch is the on-path-attacker (or
	// misconfigured cluster) signature, so log both sides loudly.
	log.Printf("pin assertion for %s pending: asserted SPKI %s, observed %q", entry.Subdomain, req.CertFingerprint, observed)
	resp.Pin = pinPending
	resp.AssertedFingerprint = req.CertFingerprint
	resp.ObservedFingerprint = observed
	respondPinResult(w, reg, http.StatusAccepted, resp)
}

// respondPinResult persists pin state before acknowledging it. The dirty flag
// set by the transition survives a failed save, so the flush loop retries.
func respondPinResult(w http.ResponseWriter, reg *registry.Registry, status int, resp *registerResponse) {
	if err := reg.SaveTo(dataPath); err != nil {
		log.Printf("failed to persist pin state: %v", err)
		respondError(w, http.StatusInternalServerError, "failed to persist pin state")
		return
	}
	respondJSON(w, status, resp)
}

// observeClusterSPKI dials the cluster's HTTPS endpoint with the subdomain's
// SNI and returns the SPKI SHA-256 of the leaf certificate it serves. The
// dial is confirmation and availability machinery, not the source of trust:
// the fingerprint it confirms was token-authenticated, so an on-path attacker
// answering this dial can delay a pin's activation but never poison it.
func observeClusterSPKI(ip, sni string) (string, error) {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	conn, err := tls.DialWithDialer(dialer, "tcp", net.JoinHostPort(ip, "443"), &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // the fingerprint compare replaces chain verification on this hop
		MinVersion:         tls.VersionTLS12,
		ServerName:         sni,
	})
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()
	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return "", fmt.Errorf("no peer certificate")
	}
	return spki.Fingerprint(certs[0]), nil
}

// observeKeyFunc dials a cluster and returns the ECDSA public key of the leaf
// it serves plus that leaf's SPKI fingerprint. Injected into handleProof so
// tests can stub the network.
type observeKeyFunc func(ip, sni string) (*ecdsa.PublicKey, string, error)

// observeClusterKey dials the cluster's HTTPS endpoint and returns the ECDSA
// public key of the served leaf, which the proof verifies the signature
// against. Possession of the matching private key — not knowledge of the
// public certificate — is what proves control of the destination.
func observeClusterKey(ip, sni string) (*ecdsa.PublicKey, string, error) {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	conn, err := tls.DialWithDialer(dialer, "tcp", net.JoinHostPort(ip, "443"), &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // proof verifies possession of this key, not the chain
		MinVersion:         tls.VersionTLS12,
		ServerName:         sni,
	})
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = conn.Close() }()
	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return nil, "", fmt.Errorf("no peer certificate")
	}
	pub, ok := certs[0].PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return nil, "", fmt.Errorf("served certificate is not ECDSA")
	}
	return pub, spki.Fingerprint(certs[0]), nil
}

// handleProof completes a proof of possession: the token holder signed a fresh
// gateway nonce with the hop-cert private key. The gateway re-checks the token
// and the outstanding nonce, dials the registered IP for the served public
// key, and verifies the signature against it — so only the holder of the
// destination's private key can prove control, and echoing its public
// certificate does not. A failed dial answers 202 (retry, the cluster may
// still be converging); a bad signature or token answers 403; success records
// a fresh proof lease.
func handleProof(reg *registry.Registry, baseDomain string, observe observeKeyFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
		var req proofRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondJSON(w, http.StatusBadRequest, proofResponse{Error: "invalid request body"})
			return
		}

		entry := reg.Lookup(req.Subdomain)
		if entry == nil {
			respondJSON(w, http.StatusNotFound, proofResponse{Error: "unknown subdomain"})
			return
		}
		// Reject an uncommittable request (bad token or stale/expired nonce) in
		// constant time before doing any network work. RecordProof re-checks
		// under the write lock to close the dial-window TOCTOU.
		if !reg.ChallengeMatches(req.Subdomain, req.Token, req.Nonce) {
			respondJSON(w, http.StatusConflict, proofResponse{Error: "invalid token or no matching outstanding challenge"})
			return
		}

		pub, keySPKI, err := observe(entry.IP, req.Subdomain+"."+baseDomain)
		if err != nil {
			log.Printf("proof dial for %s failed: %v", req.Subdomain, err)
			respondJSON(w, http.StatusAccepted, proofResponse{Error: "could not reach the cluster to verify; retry"})
			return
		}

		if !hopproof.Verify(pub, req.Nonce, entry.Subdomain, entry.IP, baseDomain, req.Token, req.Signature) {
			log.Printf("proof for %s rejected: signature does not verify against the served key", req.Subdomain)
			respondJSON(w, http.StatusForbidden, proofResponse{Error: "signature does not verify"})
			return
		}

		if !reg.RecordProof(req.Subdomain, req.Token, req.Nonce, keySPKI, hopproof.Protocol) {
			respondJSON(w, http.StatusForbidden, proofResponse{Error: "invalid token or challenge"})
			return
		}
		if err := reg.SaveTo(dataPath); err != nil {
			log.Printf("failed to persist proof: %v", err)
			respondJSON(w, http.StatusInternalServerError, proofResponse{Error: "failed to persist proof"})
			return
		}
		log.Printf("proof recorded for %s (key %s)", req.Subdomain, keySPKI)
		// Read the lease back defensively: a concurrent deregistration between
		// recording the proof and reporting it would otherwise dereference nil.
		var expiry string
		if proven := reg.Lookup(req.Subdomain); proven != nil {
			expiry = proven.ProofExpiry.UTC().Format(time.RFC3339)
		}
		respondJSON(w, http.StatusOK, proofResponse{Proven: true, ProofExpiry: expiry})
	}
}

// boolEnv reports whether an environment variable is set to a truthy value.
// boolEnvDefaultTrue reads a boolean that is on unless it is explicitly turned
// off. Every boolean the gateway reads is of this shape: the safe value is the
// enabled one, so an operator who has never heard of the setting gets the
// protection rather than the exposure.
func boolEnvDefaultTrue(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "0", "false", "no", "off":
		return false
	}
	return true
}

// noDerivedSeparator reports whether a subdomain is free of the derived-route
// separator. A label containing it would shadow a per-cluster service route.
// registrableEntry reports whether a persisted registration still satisfies the
// label rule: shape, no derived-route separator, not reserved. Startup is where
// a rule tightened after a snapshot was written gets applied, so a name reserved
// by a later build stops serving on the next restart instead of being protected
// only against new registrations.
//
// The address guard handleRegister applies is left out here on purpose. In
// persisted state a label spelling an address it no longer points at has two
// causes that look identical: a cluster that moved to a new server and kept the
// name its links were published under, and a squatter who took another server's
// default name before the guard existed. Nothing recorded distinguishes them,
// and the costs are not symmetric — dropping the entry takes a live cluster off
// the air on a restart, while keeping it costs one operator their default name,
// which choosing another name resolves. New registrations cannot create either
// case, so this is confined to entries written before the guard and shrinks to
// nothing. addressMismatch names them in the log rather than acting on them.
func registrableEntry(subdomain, ip string) bool {
	return hostnames.ValidateClusterLabel(subdomain) == nil
}

// addressMismatch reports a label that spells an address other than the one it
// points at. Reported, never acted on: see registrableEntry.
func addressMismatch(subdomain, ip string) bool {
	return hostnames.IPShapedLabel(subdomain) && subdomain != hostnames.LabelForIP(ip)
}

// isPublicIP reports whether s is an address the gateway may register and proxy
// to. The policy lives in controller/pkg/pubip so the CLI applies exactly the
// same rule before it records an address a cluster will try to register with.
func isPublicIP(s string) bool { return pubip.IsPublic(s) }

func handleDeregister(reg *registry.Registry, proxy *Proxy) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
		var req deregisterRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondError(w, http.StatusBadRequest, "invalid request body")
			return
		}

		subdomain, err := reg.Deregister(req.Token)
		if err != nil {
			respondError(w, http.StatusNotFound, err.Error())
			return
		}
		proxy.forgetCluster(subdomain)

		if err := reg.SaveTo(dataPath); err != nil {
			log.Printf("failed to persist registry: %v", err)
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

func handlePing(reg *registry.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
		var req pingRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondError(w, http.StatusBadRequest, "invalid request body")
			return
		}

		if err := reg.Ping(req.Token); err != nil {
			respondError(w, http.StatusNotFound, err.Error())
			return
		}

		respondJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

func cleanupLoop(reg *registry.Registry, proxy *Proxy) {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for range ticker.C {
		removed := reg.Cleanup()
		for _, subdomain := range removed {
			proxy.forgetCluster(subdomain)
		}
		if len(removed) > 0 {
			log.Printf("cleaned up %d expired subdomain(s)", len(removed))
			if err := reg.SaveTo(dataPath); err != nil {
				log.Printf("failed to persist registry after cleanup: %v", err)
			}
		}
		// A cluster stuck in unpinned grace proxies unverified; surface it
		// periodically so drift is visible without per-handshake logs.
		if count, oldest := reg.UnpinnedSummary(); count > 0 {
			log.Printf("%d active registration(s) have no certificate pin (oldest %s); their hops proxy unverified until the cluster asserts a fingerprint",
				count, oldest.Round(time.Minute))
		}
	}
}

// flushInterval bounds how stale the persisted LastSeen timestamps can be after
// a restart. It sits well under the 30-day inactivity TTL, so an actively-pinged
// subdomain always survives a restart.
const flushInterval = 5 * time.Minute

func flushLoop(reg *registry.Registry) {
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()

	for range ticker.C {
		if _, err := reg.FlushIfDirty(dataPath); err != nil {
			log.Printf("failed to persist registry: %v", err)
		}
	}
}

func respondJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if data != nil {
		_ = json.NewEncoder(w).Encode(data)
	}
}

func respondError(w http.ResponseWriter, status int, msg string) {
	respondJSON(w, status, map[string]string{"error": msg})
}
