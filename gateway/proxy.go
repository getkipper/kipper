package main

import (
	"container/list"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/httprate"

	"github.com/getkipper/kipper/controller/pkg/spki"
	"github.com/getkipper/kipper/gateway/internal/registry"
)

// maxCachedProxies bounds the reverse-proxy cache. Any label paired with a
// registered cluster suffix proxies through, so a flood of distinct
// subdomains for one valid cluster could otherwise grow the cache without
// limit. The cache is LRU: past the cap the least-recently-used proxy is
// evicted and its idle connections closed, so the working set stays hot while
// an attacker's throwaway hosts age out instead of pinning memory or sockets.
const maxCachedProxies = 4096

// idlePerTransport bounds idle upstream sockets per cached authorization state.
// Keep idlePerTransport × maxCachedProxies within the container's descriptor
// budget, allowing two more descriptors per in-flight exchange.
// TestIdlePoolFitsTheDescriptorBudget checks this bound.
const idlePerTransport = 16

// graceLogInterval spaces the unpinned-grace log lines per subdomain, so an
// unpinned cluster stays visible in the logs without one line per handshake.
const graceLogInterval = 5 * time.Minute

// Proxy handles incoming requests to *.kipper.run by looking up the
// subdomain in the registry and reverse-proxying to the target cluster.
type Proxy struct {
	Registry   *registry.Registry
	BaseDomain string

	// OnPinChange runs after a handshake-time pin promotion, so main can
	// flush the registry without the data plane blocking on disk IO.
	OnPinChange func()

	// EnforceProof gates proof-before-route (B16): when set, a cluster that
	// has not proven control of its IP (via the signed-nonce proof) does not
	// proxy — an unproven registration to an arbitrary IP never serves. Off
	// during the transition so the fleet acquires proofs before enforcement.
	EnforceProof bool

	// proofSkipLogged throttles the per-subdomain unproven-skip log line.
	proofSkipLogged sync.Map

	// proxies caches one reverse proxy per (cluster IP, request host, pin
	// mode) so connections to a pinned cluster are pooled and reused across
	// requests instead of paying a fresh TCP+TLS handshake every time. Keyed
	// by host because the TLS ServerName (SNI) Traefik routes on is the
	// request host, and by pin mode because the unpinned-grace transport
	// must never pool (see buildProxy).
	proxies     *proxyCache
	proxiesOnce sync.Once

	// graceLogged throttles the per-subdomain unpinned-grace log line.
	graceLogged sync.Map

	// ClientBudget bounds how much proxied traffic one client address may
	// generate. The API limiter cannot do this job: it sits behind this
	// middleware and never runs for a registered host, and its 30-per-minute
	// budget would break a single page load, which is dozens of requests. Nil
	// disables it, for a deployment that meters at the edge instead.
	ClientBudget *httprate.RateLimiter

	// ClusterInFlight caps concurrent proxied requests to any one cluster. A
	// per-client budget cannot bound a distributed source, and this hop is an
	// amplifier: the transport cache is keyed by host (4096 of them) and an
	// unpinned target runs without keep-alives, so a request can cost the
	// cluster a fresh TLS handshake. Zero disables the cap.
	ClusterInFlight int64

	// inFlight counts live proxied requests per subdomain, for ClusterInFlight.
	// The map is guarded rather than a sync.Map because installing a counter and
	// removing a spent one have to be one decision: a counter deleted while a
	// request still held a slot would let the next arrival install a second
	// counter and route past the ceiling.
	inFlightMu sync.Mutex
	inFlight   map[string]*atomic.Int64
}

// cache returns the lazily-initialised proxy cache, so a zero-value Proxy
// (used in tests) needs no explicit constructor.
func (p *Proxy) cache() *proxyCache {
	p.proxiesOnce.Do(func() { p.proxies = newProxyCache(maxCachedProxies) })
	return p.proxies
}

// proxyCache is a bounded LRU of reverse proxies. Eviction closes the evicted
// proxy's idle connections so an unbounded stream of distinct hosts can't
// strand sockets or goroutines.
type proxyCache struct {
	mu    sync.Mutex
	order *list.List // front = most recently used; values are *proxyEntry
	items map[string]*list.Element
	cap   int
}

type proxyEntry struct {
	key   string
	proxy *httputil.ReverseProxy
}

func newProxyCache(cap int) *proxyCache {
	return &proxyCache{order: list.New(), items: make(map[string]*list.Element), cap: cap}
}

func (c *proxyCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}

func (c *proxyCache) get(key string) (*httputil.ReverseProxy, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.order.MoveToFront(el)
		return el.Value.(*proxyEntry).proxy, true
	}
	return nil, false
}

// add stores proxy under key and returns the proxy the caller should use. When
// another goroutine already cached this key, the existing proxy wins and the
// caller's freshly built one is discarded (its idle connections closed).
func (c *proxyCache) add(key string, proxy *httputil.ReverseProxy) *httputil.ReverseProxy {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.order.MoveToFront(el)
		closeIdle(proxy)
		return el.Value.(*proxyEntry).proxy
	}
	el := c.order.PushFront(&proxyEntry{key: key, proxy: proxy})
	c.items[key] = el
	if c.order.Len() > c.cap {
		if oldest := c.order.Back(); oldest != nil {
			c.order.Remove(oldest)
			ent := oldest.Value.(*proxyEntry)
			delete(c.items, ent.key)
			closeIdle(ent.proxy)
		}
	}
	return proxy
}

// closeIdle releases the pooled connections behind a proxy that is being
// evicted or discarded.
func closeIdle(proxy *httputil.ReverseProxy) {
	if t, ok := proxy.Transport.(*http.Transport); ok {
		t.CloseIdleConnections()
	}
}

// Middleware intercepts requests to registered subdomains and proxies them.
// Requests to the base domain or unregistered subdomains pass through
// to the next handler (the API routes).
//
// Subdomain scheme (all single-level, wildcard-cert compatible):
//   - cluster:  203-0-113-12.kipper.run
//   - console:  console--203-0-113-12.kipper.run
//   - app:      myapp--203-0-113-12.kipper.run
//
// Derived routes join the service prefix and the cluster with a double
// dash. Registered cluster labels may never contain "--" (handleRegister
// rejects them), so a registration can never shadow another cluster's
// derived route — in either registration order.
func (p *Proxy) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Normalise once, up front: host names are case-insensitive and may
		// carry a port, so both the registry lookup and the proxy must see the
		// same lowercased, port-free host or a mixed-case request fails to route.
		host := normaliseHost(r.Host)
		entry := p.findCluster(host)
		if entry == nil {
			next.ServeHTTP(w, r)
			return
		}

		// Proof-before-route gate (B16), applied once on the resolved cluster
		// entry so it covers the exact host and every derived route
		// (console--<cluster>, <app>--<cluster>). An unproven cluster gets an
		// explicit 404 — never a proxy attempt, and never a fall-through to the
		// gateway API, which would confirm the reserved name or expose base
		// behaviour.
		// Meter first, because nothing downstream will: this middleware
		// short-circuits the chain for every registered host, so the API limiter
		// registered after it never sees a proxied request. It runs ahead of the
		// proof gate so the rejection path is metered too — cheap to serve is
		// not free to serve.
		if p.ClientBudget != nil {
			key, _ := rateLimitKey(r)
			if key == "" {
				// No measurable client, which in the shipped topology means the
				// reverse proxy in front stopped setting the header. Falling back
				// to one shared key would put every cluster in a single bucket and
				// let one caller throttle the whole platform; keying by
				// destination contains that to the cluster actually being hit.
				key = "cluster:" + entry.Subdomain
			}
			if p.ClientBudget.RespondOnLimit(w, r, key) {
				return
			}
		}

		if p.EnforceProof && !p.Registry.Routable(entry.Subdomain) {
			p.logProofSkip(entry)
			http.NotFound(w, r)
			return
		}

		if release, ok := p.enterCluster(entry.Subdomain); ok {
			defer release()
		} else {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "cluster is at its concurrent request limit", http.StatusServiceUnavailable)
			return
		}

		p.proxyTo(w, r, entry, host)
	})
}

// enterCluster takes a slot in the destination's concurrency budget and returns
// the release. The second result is false when the cluster is already at its
// cap, which the caller answers with a 503 rather than adding to the pile.
//
// The counter is per subdomain rather than per host, so every derived route of
// a cluster shares one budget: they all land on the same machine, which is what
// the cap protects.
func (p *Proxy) enterCluster(subdomain string) (release func(), ok bool) {
	if p.ClusterInFlight <= 0 {
		return func() {}, true
	}
	p.inFlightMu.Lock()
	defer p.inFlightMu.Unlock()

	counter := p.inFlight[subdomain]
	if counter == nil {
		counter = new(atomic.Int64)
		if p.inFlight == nil {
			p.inFlight = make(map[string]*atomic.Int64)
		}
		p.inFlight[subdomain] = counter
	}
	if counter.Load() >= p.ClusterInFlight {
		return nil, false
	}
	counter.Add(1)
	return func() { counter.Add(-1) }, true
}

// forgetCluster removes log-throttle state and an idle in-flight counter.
// A live counter must remain shared until its holders release it; replacing it
// would let new arrivals bypass the per-cluster limit.
func (p *Proxy) forgetCluster(subdomain string) {
	p.proofSkipLogged.Delete(subdomain)
	p.graceLogged.Delete(subdomain)

	p.inFlightMu.Lock()
	defer p.inFlightMu.Unlock()
	if counter, ok := p.inFlight[subdomain]; ok && counter.Load() == 0 {
		delete(p.inFlight, subdomain)
	}
}

// logProofSkip surfaces an unproven-route rejection at most once per
// graceLogInterval per subdomain, naming the actual cause. The three lead
// somewhere completely different — a cluster that never proved control, a
// heartbeat that stopped renewing the lease, and a pin that moved onto a key no
// proof covers — so the log has to tell them apart.
func (p *Proxy) logProofSkip(entry *registry.Entry) {
	now := time.Now()
	if last, ok := p.proofSkipLogged.Load(entry.Subdomain); ok && now.Sub(last.(time.Time)) < graceLogInterval {
		return
	}
	p.proofSkipLogged.Store(entry.Subdomain, now)
	switch {
	case entry.ProvenAt.IsZero():
		log.Printf("refusing to route %s: registration has not proven control of its IP (proof-before-route)", entry.Subdomain)
	case !now.Before(entry.ProofExpiry):
		log.Printf("refusing to route %s: its proof lease lapsed at %s; the cluster heartbeat has not renewed it",
			entry.Subdomain, entry.ProofExpiry.UTC().Format(time.RFC3339))
	default:
		log.Printf("refusing to route %s: the pinned hop key %s is not the key possession was proven for (%s); awaiting a fresh proof",
			entry.Subdomain, entry.CertFingerprint, entry.ProofKeySPKI)
	}
}

func (p *Proxy) proxyTo(w http.ResponseWriter, r *http.Request, entry *registry.Entry, host string) {
	proxy := p.proxyFor(host, entry)
	if proxy == nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	proxy.ServeHTTP(w, r)
}

// normaliseHost lowercases the host and drops any port, so requests that differ
// only by case or port share one cache entry (and one connection pool) and the
// value is a valid TLS ServerName. Host names are case-insensitive, so this is
// safe for both routing and SNI.
func normaliseHost(host string) string {
	host = strings.ToLower(host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

// proxyFor caches a reverse proxy by normalized host, IP, and authorization state.
// The key includes accepted pins, first-pin tolerance, and the proven key because
// TLS authenticates a connection once. Policy changes select a transport that
// must handshake under the new state; idle connections on old transports expire.
// A recurring state may reuse connections authenticated under that same state.
func (p *Proxy) proxyFor(host string, entry *registry.Entry) *httputil.ReverseProxy {
	pins := p.Registry.PinState(entry.Subdomain)
	pinned := pins.Pinned()
	settle := ""
	if pins.InFirstPinSettle() {
		settle = "settle"
	}
	key := strings.Join([]string{
		entry.IP, host, pins.Current, pins.Pending, pins.Prev, settle,
		p.Registry.ProvenKey(entry.Subdomain),
	}, "|")
	if cached, ok := p.cache().get(key); ok {
		return cached
	}

	proxy := p.buildProxy(host, entry, pinned)
	if proxy == nil {
		return nil
	}
	return p.cache().add(key, proxy)
}

func (p *Proxy) buildProxy(host string, entry *registry.Entry, pinned bool) *httputil.ReverseProxy {
	// Bracket IPv6 literals so the host parses correctly (https://[::1]).
	targetHost := entry.IP
	if strings.Contains(targetHost, ":") {
		targetHost = "[" + targetHost + "]"
	}
	target, err := url.Parse("https://" + targetHost)
	if err != nil {
		log.Printf("invalid target URL for %s: %v", entry.Subdomain, err) //nolint:gosec // subdomain from registry
		return nil
	}

	// Clone the default transport for its timeouts and connection limits.
	// SNI uses the request host while dialing the cluster IP. VerifyConnection
	// checks live SPKI pins in place of WebPKI validation for the self-signed hop
	// certificate; proxyFor's cache key controls reuse of verified connections.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Size the idle pool within the process-wide descriptor budget above.
	transport.MaxIdleConnsPerHost = idlePerTransport
	transport.MaxIdleConns = idlePerTransport
	transport.TLSClientConfig = &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // chain+hostname checks replaced by the SPKI pin in VerifyConnection
		MinVersion:         tls.VersionTLS12,
		ServerName:         host,
		VerifyConnection:   p.verifyPin(entry.Subdomain),
	}
	transport.DialContext = (&net.Dialer{
		Timeout: 10 * time.Second,
	}).DialContext
	if !pinned {
		// A connection accepted under unpinned grace must not survive into
		// the pinned steady state via a pool, where it could carry requests
		// indefinitely without ever re-handshaking against the pin. No
		// keep-alives: each grace connection serves one exchange and closes.
		transport.DisableKeepAlives = true
	}

	return &httputil.ReverseProxy{
		Transport: transport,
		// Rewrite replaces inbound forwarded headers with the client address measured
		// by trusted-header middleware. Avoid SetXForwarded, which preserves inbound XFF.
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = target.Scheme
			pr.Out.URL.Host = target.Host
			pr.Out.Host = host
			// Rewrite drops the inbound X-Forwarded-* trio but not RFC 7239
			// Forwarded, so strip that too: nothing downstream reads it today,
			// but leaving a client-supplied value in place would be a spoofable
			// channel the moment something does.
			pr.Out.Header.Del("Forwarded")
			// Forward the trusted client address when available.
			if clientIP := chimw.GetClientIP(pr.In.Context()); clientIP != "" {
				pr.Out.Header.Set("X-Forwarded-For", clientIP)
			}
		},
	}
}

// verifyPin checks live registry state on every TLS handshake. It first checks
// the accepted pin set, then, when proof enforcement is enabled, verifies that
// the observed leaf is the proven key. All checks use in-memory registry state.
// proxyFor's cache key handles policy changes between connections.
func (p *Proxy) verifyPin(subdomain string) func(tls.ConnectionState) error {
	return func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return fmt.Errorf("cluster for %s presented no certificate", subdomain)
		}
		observed := spki.Fingerprint(cs.PeerCertificates[0])

		if err := p.checkPin(subdomain, observed); err != nil {
			return err
		}
		if p.EnforceProof && !p.Registry.ProofAuthorizes(subdomain, observed) {
			return fmt.Errorf("cluster for %s presented SPKI %s, which holds no current proof of possession", subdomain, observed)
		}
		return nil
	}
}

// checkPin enforces the subdomain's accepted-fingerprint set (B5).
//
// Policy: with no enforced pin the handshake is accepted and logged (unpinned
// grace — availability wins until the cluster's first token-authenticated
// assertion lands). Once pins exist, the leaf SPKI must match the current,
// pending, or previous fingerprint; anything else fails the handshake and the
// client sees the proxy's normal 502. A pending match is the observation proof
// that promotes it to enforced.
func (p *Proxy) checkPin(subdomain, observed string) error {
	pins := p.Registry.PinState(subdomain)

	if registry.FingerprintsEqual(observed, pins.Pending) {
		if p.Registry.PromoteOnObserve(subdomain, observed) {
			log.Printf("pin promoted for %s: observed the pending SPKI %s on a live handshake", subdomain, observed)
			if p.OnPinChange != nil {
				p.OnPinChange()
			}
		}
		return nil
	}
	if pins.Current == "" {
		p.logGrace(subdomain, observed)
		return nil
	}
	if registry.FingerprintsEqual(observed, pins.Current) || registry.FingerprintsEqual(observed, pins.Prev) {
		return nil
	}
	// Allow fallback certificates during the first-pin settle window while
	// Traefik replicas converge. The window stays anchored to first activation;
	// rotation uses Prev instead. The proof gate still rejects an unproven leaf.
	if pins.InFirstPinSettle() {
		log.Printf("first-pin settle for %s: accepted SPKI %s (does not match the pin yet); enforcement begins when the settle window ends", subdomain, observed)
		return nil
	}
	return fmt.Errorf("cluster for %s presented SPKI %s, which matches no pinned fingerprint", subdomain, observed)
}

// logGrace surfaces an unpinned-grace handshake at most once per
// graceLogInterval per subdomain.
func (p *Proxy) logGrace(subdomain, observed string) {
	now := time.Now()
	if last, ok := p.graceLogged.Load(subdomain); ok && now.Sub(last.(time.Time)) < graceLogInterval {
		return
	}
	p.graceLogged.Store(subdomain, now)
	log.Printf("proxying %s unpinned (grace): observed SPKI %s; awaiting a token-authenticated assertion", subdomain, observed)
}

// findCluster extracts the subdomain label from the host and checks
// if it matches a registered cluster. "203-0-113-12" matches directly.
// "console--203-0-113-12" matches the cluster after its "--" separator.
// The service prefix itself may contain "--" (an app named a--b), so the
// cluster is always the segment after the last "--"; cluster labels
// cannot contain "--".
func (p *Proxy) findCluster(host string) *registry.Entry {
	// Strip port
	if idx := strings.LastIndex(host, ":"); idx != -1 {
		host = host[:idx]
	}

	suffix := "." + p.BaseDomain
	if !strings.HasSuffix(host, suffix) {
		return nil
	}

	label := strings.TrimSuffix(host, suffix)
	if label == "" {
		return nil
	}

	// Exact match: "203-0-113-12.kipper.run"
	if entry := p.Registry.Lookup(label); entry != nil {
		return entry
	}

	// Derived route: "console--203-0-113-12.kipper.run"
	if idx := strings.LastIndex(label, derivedRouteSeparator); idx > 0 {
		return p.Registry.Lookup(label[idx+len(derivedRouteSeparator):])
	}

	return nil
}
