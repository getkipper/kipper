package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	chimw "github.com/go-chi/chi/v5/middleware"

	"github.com/go-chi/httprate"

	"github.com/getkipper/kipper/controller/pkg/spki"

	"github.com/getkipper/kipper/gateway/internal/registry"
)

func TestFindCluster(t *testing.T) {
	reg := registry.New()
	_, _, _ = reg.Register("203-0-113-12", "203.0.113.12", "")
	_, _, _ = reg.Register("myapp", "10.0.0.1", "")

	p := &Proxy{Registry: reg, BaseDomain: "kipper.run"}

	tests := []struct {
		host    string
		wantIP  string
		wantNil bool
	}{
		// Exact match
		{"203-0-113-12.kipper.run", "203.0.113.12", false},
		{"myapp.kipper.run", "10.0.0.1", false},

		// Derived route (prefix--cluster)
		{"console--203-0-113-12.kipper.run", "203.0.113.12", false},
		{"hello--203-0-113-12.kipper.run", "203.0.113.12", false},
		{"my-app--203-0-113-12.kipper.run", "203.0.113.12", false},
		{"console--myapp.kipper.run", "10.0.0.1", false},
		// The prefix itself may contain "--"; the cluster is the segment
		// after the last one.
		{"a--b--203-0-113-12.kipper.run", "203.0.113.12", false},
		// A registered label can never shadow another cluster's derived
		// route: "myapp" is registered, but myapp--<cluster> still routes
		// to the cluster because labels containing "--" are unregistrable.
		{"myapp--203-0-113-12.kipper.run", "203.0.113.12", false},

		// With port
		{"203-0-113-12.kipper.run:443", "203.0.113.12", false},
		{"console--203-0-113-12.kipper.run:443", "203.0.113.12", false},

		// Single-dash hosts are exact-match only under the double-dash
		// scheme; the old flattened form no longer routes.
		{"console-203-0-113-12.kipper.run", "", true},
		{"hello-203-0-113-12.kipper.run", "", true},

		// No match
		{"kipper.run", "", true},
		{"unknown.kipper.run", "", true},
		{"--myapp.kipper.run", "", true},
		{"example.com", "", true},
		{"", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			entry := p.findCluster(tt.host)
			if tt.wantNil {
				if entry != nil {
					t.Errorf("expected nil, got %+v", entry)
				}
				return
			}
			if entry == nil {
				t.Fatal("expected entry, got nil")
			}
			if entry.IP != tt.wantIP {
				t.Errorf("expected IP %s, got %s", tt.wantIP, entry.IP)
			}
		})
	}
}

func TestFindClusterIsCaseInsensitiveViaNormalise(t *testing.T) {
	reg := registry.New()
	_, _, _ = reg.Register("203-0-113-12", "203.0.113.12", "")
	p := &Proxy{Registry: reg, BaseDomain: "kipper.run"}

	// The middleware normalises the host before lookup, so a mixed-case request
	// (host names are case-insensitive) must still resolve to the cluster.
	for _, host := range []string{
		"MYAPP--203-0-113-12.kipper.run",
		"myapp--203-0-113-12.KIPPER.RUN:443",
		"CONSOLE--203-0-113-12.Kipper.Run",
	} {
		if entry := p.findCluster(normaliseHost(host)); entry == nil || entry.IP != "203.0.113.12" {
			t.Errorf("host %q did not route after normalisation", host)
		}
	}
}

func TestProxyForCachesPerHost(t *testing.T) {
	p := &Proxy{Registry: registry.New(), BaseDomain: "kipper.run"}
	entry := &registry.Entry{Subdomain: "203-0-113-12", IP: "203.0.113.12"}

	first := p.proxyFor("myapp--203-0-113-12.kipper.run", entry)
	if first == nil {
		t.Fatal("expected a proxy, got nil")
	}
	// Same host reuses the cached proxy.
	if again := p.proxyFor("myapp--203-0-113-12.kipper.run", entry); again != first {
		t.Error("expected the same host to reuse the cached proxy")
	}
	// A different host (different SNI) gets its own proxy.
	if other := p.proxyFor("console--203-0-113-12.kipper.run", entry); other == first {
		t.Error("expected a different host to get its own proxy")
	}
	if got := p.cache().len(); got != 2 {
		t.Errorf("expected 2 cached proxies, got %d", got)
	}
}

func TestProxyForCollapsesHostVariants(t *testing.T) {
	p := &Proxy{Registry: registry.New(), BaseDomain: "kipper.run"}
	entry := &registry.Entry{Subdomain: "203-0-113-12", IP: "203.0.113.12"}

	// Case and port variants of the same host must not each mint a cache entry,
	// or an attacker could multiply the working set for one valid cluster.
	base := p.proxyFor(normaliseHost("myapp--203-0-113-12.kipper.run"), entry)
	withPort := p.proxyFor(normaliseHost("myapp--203-0-113-12.kipper.run:443"), entry)
	upper := p.proxyFor(normaliseHost("MYAPP--203-0-113-12.kipper.run"), entry)
	if base != withPort || base != upper {
		t.Error("case and port variants should collapse to one cached proxy")
	}
	if got := p.cache().len(); got != 1 {
		t.Errorf("expected 1 cached proxy for the host variants, got %d", got)
	}
}

func TestProxyForCapBounded(t *testing.T) {
	p := &Proxy{Registry: registry.New(), BaseDomain: "kipper.run"}
	entry := &registry.Entry{Subdomain: "203-0-113-12", IP: "203.0.113.12"}

	// Flood well past the cap; every request still gets a working proxy and the
	// LRU holds steady at the cap instead of growing without bound.
	for i := 0; i < maxCachedProxies+50; i++ {
		host := fmt.Sprintf("flood-%d--203-0-113-12.kipper.run", i)
		if p.proxyFor(host, entry) == nil {
			t.Fatalf("expected a proxy for %s, got nil", host)
		}
	}
	if got := p.cache().len(); got != maxCachedProxies {
		t.Errorf("expected cache to stay at the cap %d, got %d", maxCachedProxies, got)
	}
}

func TestBuildProxyRejectsBadTarget(t *testing.T) {
	p := &Proxy{Registry: registry.New(), BaseDomain: "kipper.run"}
	// A control character in the IP makes url.Parse fail.
	if got := p.buildProxy("host.kipper.run", &registry.Entry{Subdomain: "x", IP: "\x7f"}, false); got != nil {
		t.Errorf("expected nil proxy for an unparseable target, got %+v", got)
	}
}

// captureRT records the outbound request the proxy would send upstream.
type captureRT struct{ got *http.Request }

func (c *captureRT) RoundTrip(req *http.Request) (*http.Response, error) {
	c.got = req
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: make(http.Header)}, nil
}

// The cluster trusts this gateway's X-Forwarded-For, so it must carry the client
// Caddy measured and nothing a client can choose. The request goes through the
// same trusted-header middleware the router installs, since that is what decides
// who the client is.
func TestBuildProxyForwardsMeasuredClientNotSpoof(t *testing.T) {
	p := &Proxy{Registry: registry.New(), BaseDomain: "kipper.run"}

	cases := []struct {
		name       string
		realIP     string // what Caddy overwrote X-Real-IP with
		wantClient string
	}{
		{"measured client reaches the cluster", "203.0.113.7", "203.0.113.7"},
		// Caddy always sets the header; without it the client is unknown, and an
		// unknown client must not be reported as this proxy's own address.
		{"no measurable client sends no header", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rp := p.buildProxy("shop.kipper.run", &registry.Entry{Subdomain: "203-0-113-12", IP: "203.0.113.12"}, false)
			if rp == nil {
				t.Fatal("expected a proxy for a valid target")
			}
			capture := &captureRT{}
			rp.Transport = capture

			req := httptest.NewRequest(http.MethodGet, "https://shop.kipper.run/", nil)
			req.Header.Set("X-Forwarded-For", "198.51.100.9") // spoofed by the client
			req.Header.Set("True-Client-IP", "198.51.100.9")  // and again, another way
			if tc.realIP != "" {
				req.Header.Set(clientIPHeader, tc.realIP)
			}
			req.RemoteAddr = "10.0.0.2:40000" // Caddy, the only TCP peer there ever is

			chimw.ClientIPFromHeader(clientIPHeader)(rp).ServeHTTP(httptest.NewRecorder(), req)

			if got := capture.got.Header.Get("X-Forwarded-For"); got != tc.wantClient {
				t.Errorf("upstream X-Forwarded-For = %q, want %q", got, tc.wantClient)
			}
		})
	}
}

func TestBuildProxyStripsInboundForwardedHeader(t *testing.T) {
	p := &Proxy{Registry: registry.New(), BaseDomain: "kipper.run"}
	rp := p.buildProxy("shop.kipper.run", &registry.Entry{Subdomain: "203-0-113-12", IP: "203.0.113.12"}, false)
	if rp == nil {
		t.Fatal("expected a proxy for a valid target")
	}
	capture := &captureRT{}
	rp.Transport = capture

	req := httptest.NewRequest(http.MethodGet, "https://shop.kipper.run/", nil)
	req.Header.Set("Forwarded", `for=198.51.100.9;host=evil.example.com;proto=https`) // spoofed by the client
	req.RemoteAddr = "203.0.113.7"

	rp.ServeHTTP(httptest.NewRecorder(), req)

	if got := capture.got.Header.Get("Forwarded"); got != "" {
		t.Errorf("upstream Forwarded = %q, want it stripped", got)
	}
}

func TestProxyForCapConcurrent(t *testing.T) {
	p := &Proxy{Registry: registry.New(), BaseDomain: "kipper.run"}
	entry := &registry.Entry{Subdomain: "203-0-113-12", IP: "203.0.113.12"}

	var wg sync.WaitGroup
	for i := 0; i < maxCachedProxies+500; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			host := fmt.Sprintf("flood-%d--203-0-113-12.kipper.run", n)
			if p.proxyFor(host, entry) == nil {
				t.Errorf("expected a proxy for %s, got nil", host)
			}
		}(i)
	}
	wg.Wait()

	if got := p.cache().len(); got > maxCachedProxies {
		t.Errorf("cache exceeded the cap under concurrency: got %d, want <= %d", got, maxCachedProxies)
	}
}

// --- pin enforcement through real TLS handshakes ---

// pinBackend is a TLS server standing in for a cluster's Traefik, plus the
// SPKI fingerprint of the certificate it serves.
func pinBackend(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "cluster says hi")
	}))
	t.Cleanup(ts.Close)
	return ts, spki.Fingerprint(ts.Certificate())
}

// proxyVia builds a proxy for the entry and points it at the test backend.
func proxyVia(t *testing.T, p *Proxy, entry *registry.Entry, ts *httptest.Server, pinned bool) *httputil.ReverseProxy {
	t.Helper()
	return viaBackend(t, p.buildProxy("myapp.kipper.run", entry, pinned), ts)
}

// viaBackend rewires a proxy's dialer to the test backend, keeping the real TLS
// stack — and VerifyConnection — in the path.
func viaBackend(t *testing.T, rp *httputil.ReverseProxy, ts *httptest.Server) *httputil.ReverseProxy {
	t.Helper()
	if rp == nil {
		t.Fatal("expected a proxy")
	}
	addr := ts.Listener.Addr().String()
	rp.Transport.(*http.Transport).DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	return rp
}

func proxyGet(rp *httputil.ReverseProxy) (int, string) {
	req := httptest.NewRequest(http.MethodGet, "https://myapp.kipper.run/", nil)
	req.RemoteAddr = "203.0.113.7"
	w := httptest.NewRecorder()
	rp.ServeHTTP(w, req)
	body, _ := io.ReadAll(w.Result().Body)
	return w.Code, string(body)
}

func TestVerifyPinGraceAcceptsUnpinned(t *testing.T) {
	ts, _ := pinBackend(t)
	reg := registry.New()
	entry, _, _ := reg.Register("myapp", "203.0.113.1", "")
	p := &Proxy{Registry: reg, BaseDomain: "kipper.run"}

	code, body := proxyGet(proxyVia(t, p, entry, ts, false))
	if code != http.StatusOK || body != "cluster says hi" {
		t.Errorf("unpinned grace should proxy: got %d %q", code, body)
	}
}

func TestVerifyPinEnforcesMismatch(t *testing.T) {
	ts, _ := pinBackend(t)
	// An established pin (first activation well past the settle window).
	now := time.Now().UTC().Format(time.RFC3339)
	longAgo := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	reg := registryFromSnapshot(t, `{"entries":[{"subdomain":"myapp","ip":"203.0.113.1","token":"tok","created_at":"`+now+
		`","last_seen":"`+now+`","cert_fingerprint":"`+strings.Repeat("a", 64)+
		`","pin_updated_at":"`+longAgo+`","first_pinned_at":"`+longAgo+`"}]}`)
	p := &Proxy{Registry: reg, BaseDomain: "kipper.run"}

	code, _ := proxyGet(proxyVia(t, p, reg.Lookup("myapp"), ts, true))
	if code != http.StatusBadGateway {
		t.Errorf("a non-matching certificate must fail the handshake: got %d", code)
	}
}

func TestVerifyPinAcceptsCurrent(t *testing.T) {
	ts, fp := pinBackend(t)
	reg := registry.New()
	entry, _, _ := reg.Register("myapp", "203.0.113.1", "")
	if !reg.ActivatePin("myapp", entry.Token, fp) {
		t.Fatal("activate")
	}
	p := &Proxy{Registry: reg, BaseDomain: "kipper.run"}

	code, _ := proxyGet(proxyVia(t, p, entry, ts, true))
	if code != http.StatusOK {
		t.Errorf("the pinned certificate must proxy: got %d", code)
	}
}

func TestVerifyPinAcceptsPrevDuringRotation(t *testing.T) {
	ts, fp := pinBackend(t)
	reg := registry.New()
	entry, _, _ := reg.Register("myapp", "203.0.113.1", "")
	// The backend cert was current, then a rotation activated a new pin.
	if !reg.ActivatePin("myapp", entry.Token, fp) || !reg.ActivatePin("myapp", entry.Token, strings.Repeat("b", 64)) {
		t.Fatal("activate")
	}
	p := &Proxy{Registry: reg, BaseDomain: "kipper.run"}

	code, _ := proxyGet(proxyVia(t, p, entry, ts, true))
	if code != http.StatusOK {
		t.Errorf("the previous pin must stay accepted through rotation: got %d", code)
	}
}

func TestVerifyPinPromotesPendingOnObservation(t *testing.T) {
	ts, fp := pinBackend(t)
	reg := registry.New()
	entry, _, _ := reg.Register("myapp", "203.0.113.1", "")
	if !reg.StorePendingPin("myapp", entry.Token, fp) {
		t.Fatal("store pending")
	}
	changed := make(chan struct{}, 1)
	p := &Proxy{Registry: reg, BaseDomain: "kipper.run", OnPinChange: func() { changed <- struct{}{} }}

	code, _ := proxyGet(proxyVia(t, p, entry, ts, false))
	if code != http.StatusOK {
		t.Fatalf("pending observation should proxy: got %d", code)
	}
	select {
	case <-changed:
	default:
		t.Error("expected OnPinChange after a handshake promotion")
	}
	if s := reg.PinState("myapp"); s.Current != fp || s.Pending != "" {
		t.Errorf("expected the pending pin promoted to current, got %+v", s)
	}
}

func TestGraceTransportDoesNotPool(t *testing.T) {
	reg := registry.New()
	entry, _, _ := reg.Register("myapp", "203.0.113.1", "")
	p := &Proxy{Registry: reg, BaseDomain: "kipper.run"}

	grace := p.buildProxy("myapp.kipper.run", entry, false)
	if tr := grace.Transport.(*http.Transport); !tr.DisableKeepAlives {
		t.Error("the unpinned-grace transport must not pool connections")
	}
	pinned := p.buildProxy("myapp.kipper.run", entry, true)
	if tr := pinned.Transport.(*http.Transport); tr.DisableKeepAlives {
		t.Error("the pinned transport should pool connections")
	}
}

func TestProxyForSwitchesTransportOnPinActivation(t *testing.T) {
	reg := registry.New()
	entry, _, _ := reg.Register("myapp", "203.0.113.1", "")
	p := &Proxy{Registry: reg, BaseDomain: "kipper.run"}

	grace := p.proxyFor("myapp.kipper.run", entry)
	if !reg.ActivatePin("myapp", entry.Token, strings.Repeat("a", 64)) {
		t.Fatal("activate")
	}
	pinned := p.proxyFor("myapp.kipper.run", entry)
	if grace == pinned {
		t.Error("pin activation must switch requests onto a fresh pooled transport")
	}
}

// registryFromSnapshot loads crafted persisted state, so tests can shape
// timestamps the public API stamps itself.
func registryFromSnapshot(t *testing.T, snapshot string) *registry.Registry {
	t.Helper()
	path := filepath.Join(t.TempDir(), "registry.json")
	if err := os.WriteFile(path, []byte(snapshot), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := registry.New()
	if err := reg.LoadFrom(path); err != nil {
		t.Fatalf("load: %v", err)
	}
	return reg
}

// A lagging Traefik replica can serve the pre-hop-cert fallback for a short
// period after the FIRST pin activates. Inside the settle window that
// mismatch must proxy (the cluster was fully fail-open moments before);
// after it, enforcement is unconditional.
func TestVerifyPinFirstActivationSettleWindow(t *testing.T) {
	ts, _ := pinBackend(t) // backend cert does NOT match the pin below
	now := time.Now().UTC().Format(time.RFC3339)
	old := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)

	snapshot := func(firstPinnedAt string) string {
		return `{"entries":[{"subdomain":"myapp","ip":"203.0.113.1","token":"tok","created_at":"` + now +
			`","last_seen":"` + now + `","cert_fingerprint":"` + strings.Repeat("a", 64) +
			`","pin_updated_at":"` + firstPinnedAt + `","first_pinned_at":"` + firstPinnedAt + `"}]}`
	}

	inSettle := registryFromSnapshot(t, snapshot(now))
	p := &Proxy{Registry: inSettle, BaseDomain: "kipper.run"}
	code, _ := proxyGet(proxyVia(t, p, inSettle.Lookup("myapp"), ts, true))
	if code != http.StatusOK {
		t.Errorf("inside the first-pin settle window a mismatch must still proxy, got %d", code)
	}
	if s := inSettle.PinState("myapp"); s.Current != strings.Repeat("a", 64) {
		t.Errorf("the settle window must never re-pin the observed leaf, got %+v", s)
	}

	afterSettle := registryFromSnapshot(t, snapshot(old))
	p2 := &Proxy{Registry: afterSettle, BaseDomain: "kipper.run"}
	code, _ = proxyGet(proxyVia(t, p2, afterSettle.Lookup("myapp"), ts, true))
	if code != http.StatusBadGateway {
		t.Errorf("after the settle window a mismatch must fail the handshake, got %d", code)
	}
}

// Rotation must never soften enforcement: with a previous pin present, an
// unknown leaf fails closed even seconds after the promotion.
func TestVerifyPinRotationMismatchFailsClosed(t *testing.T) {
	ts, _ := pinBackend(t) // backend cert matches neither current nor prev
	now := time.Now().UTC().Format(time.RFC3339)
	longAgo := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)

	snapshot := `{"entries":[{"subdomain":"myapp","ip":"203.0.113.1","token":"tok","created_at":"` + now +
		`","last_seen":"` + now + `","cert_fingerprint":"` + strings.Repeat("a", 64) +
		`","prev_fingerprint":"` + strings.Repeat("b", 64) + `","prev_since":"` + now +
		`","pin_updated_at":"` + now + `","first_pinned_at":"` + longAgo + `"}]}`

	reg := registryFromSnapshot(t, snapshot)
	p := &Proxy{Registry: reg, BaseDomain: "kipper.run"}
	code, _ := proxyGet(proxyVia(t, p, reg.Lookup("myapp"), ts, true))
	if code != http.StatusBadGateway {
		t.Errorf("a rotation-time mismatch must fail closed, got %d", code)
	}
}

// --- proof-before-route gate (B16) ---

func provenEntry(t *testing.T, reg *registry.Registry, subdomain, ip string) {
	t.Helper()
	entry, _, _ := reg.Register(subdomain, ip, "")
	nonce, ok, _ := reg.IssueChallenge(subdomain, entry.Token)
	if !ok {
		t.Fatal("issue challenge")
	}
	if !reg.RecordProof(subdomain, entry.Token, nonce, "spki", "kipper-hop-proof-v1") {
		t.Fatal("record proof")
	}
}

// gateTestIP points at loopback so the "routes" cases fail their dial fast
// (connection refused) instead of timing out for 10s against a routable-but-
// dead public IP — the tests only care about the gate decision, not the proxy.
const gateTestIP = "127.0.0.1"

func routeStatus(p *Proxy, host string) int {
	req := httptest.NewRequest(http.MethodGet, "https://"+host+"/", nil)
	req.Host = host
	req.RemoteAddr = "203.0.113.7"
	w := httptest.NewRecorder()
	// Terminal handler stands in for the gateway API fall-through.
	p.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})).ServeHTTP(w, req)
	return w.Code
}

func TestProofGateBlocksUnprovenWhenEnforcing(t *testing.T) {
	reg := registry.New()
	_, _, _ = reg.Register("myapp", gateTestIP, "") // never proven
	p := &Proxy{Registry: reg, BaseDomain: "kipper.run", EnforceProof: true}

	// Unproven entry: not a proxy attempt, not a fall-through — an explicit 404.
	if code := routeStatus(p, "myapp.kipper.run"); code != http.StatusNotFound {
		t.Errorf("an unproven cluster must get 404 when enforcing, got %d", code)
	}
	// The derived route resolves to the same cluster and is gated too.
	if code := routeStatus(p, "console--myapp.kipper.run"); code != http.StatusNotFound {
		t.Errorf("a derived route of an unproven cluster must also be 404, got %d", code)
	}
}

func TestProofGateAllowsUnprovenWhenNotEnforcing(t *testing.T) {
	reg := registry.New()
	_, _, _ = reg.Register("myapp", gateTestIP, "") // never proven
	p := &Proxy{Registry: reg, BaseDomain: "kipper.run", EnforceProof: false}

	// Transition mode: an unproven entry still routes (reaches the proxy, which
	// builds a reverse proxy — not the 404 gate and not the teapot fall-through).
	if code := routeStatus(p, "myapp.kipper.run"); code == http.StatusNotFound || code == http.StatusTeapot {
		t.Errorf("with enforcement off an unproven entry must still route, got %d", code)
	}
}

func TestProofGateAllowsProvenCluster(t *testing.T) {
	reg := registry.New()
	provenEntry(t, reg, "myapp", gateTestIP)
	p := &Proxy{Registry: reg, BaseDomain: "kipper.run", EnforceProof: true}

	if code := routeStatus(p, "myapp.kipper.run"); code == http.StatusNotFound || code == http.StatusTeapot {
		t.Errorf("a proven cluster must route when enforcing, got %d", code)
	}
}

// The gate binds the lease to the pinned key: a cluster whose pin has moved onto
// a key no proof covers is refused, even though its lease is unexpired.
func TestProofGateBlocksPinnedKeyWithoutProof(t *testing.T) {
	reg := registry.New()
	entry, _, _ := reg.Register("myapp", gateTestIP, "")
	if !reg.ActivatePin("myapp", entry.Token, strings.Repeat("a", 64)) {
		t.Fatal("activate")
	}
	nonce, ok, _ := reg.IssueChallenge("myapp", entry.Token)
	if !ok {
		t.Fatal("issue challenge")
	}
	if !reg.RecordProof("myapp", entry.Token, nonce, strings.Repeat("a", 64), "kipper-hop-proof-v1") {
		t.Fatal("record proof")
	}
	p := &Proxy{Registry: reg, BaseDomain: "kipper.run", EnforceProof: true}
	if code := routeStatus(p, "myapp.kipper.run"); code == http.StatusNotFound {
		t.Fatal("a cluster pinned to its proven key must route")
	}

	// A token holder parks their own key; the gateway observes it on the path.
	if !reg.StorePendingPin("myapp", entry.Token, strings.Repeat("b", 64)) {
		t.Fatal("store pending")
	}
	if !reg.PromoteOnObserve("myapp", strings.Repeat("b", 64)) {
		t.Fatal("promote")
	}
	if code := routeStatus(p, "myapp.kipper.run"); code != http.StatusNotFound {
		t.Errorf("a pin with no proof of its own must not route, got %d", code)
	}
	if code := routeStatus(p, "console--myapp.kipper.run"); code != http.StatusNotFound {
		t.Errorf("derived routes must be refused too, got %d", code)
	}
}

// provenPinnedEntry registers a cluster, pins spki and proves possession of it,
// which is the steady state of a healthy cluster under enforcement.
func provenPinnedEntry(t *testing.T, reg *registry.Registry, subdomain, ip, spki string) *registry.Entry {
	t.Helper()
	entry, _, _ := reg.Register(subdomain, ip, "")
	if !reg.ActivatePin(subdomain, entry.Token, spki) {
		t.Fatal("activate")
	}
	nonce, ok, _ := reg.IssueChallenge(subdomain, entry.Token)
	if !ok {
		t.Fatal("issue challenge")
	}
	if !reg.RecordProof(subdomain, entry.Token, nonce, spki, "kipper-hop-proof-v1") {
		t.Fatal("record proof")
	}
	return entry
}

// The healthy steady state: the cluster serves the key it proved, so enforcement
// changes nothing about a normal exchange.
func TestVerifyPinServesTheProvenKeyWhenEnforcing(t *testing.T) {
	ts, fp := pinBackend(t)
	reg := registry.New()
	entry := provenPinnedEntry(t, reg, "myapp", "203.0.113.1", fp)
	p := &Proxy{Registry: reg, BaseDomain: "kipper.run", EnforceProof: true}

	if code, body := proxyGet(proxyVia(t, p, entry, ts, true)); code != http.StatusOK || body != "cluster says hi" {
		t.Errorf("the proven key must proxy under enforcement: got %d %q", code, body)
	}
}

// TLS verification runs once per connection, so a pooled keep-alive connection
// must not keep carrying requests to a key the registry has moved off. Requests
// after the change ride a transport that has to handshake again, which refuses
// the old key.
func TestProxyDoesNotReusePooledConnectionAfterTheKeyChanges(t *testing.T) {
	ts, fp := pinBackend(t)
	reg := registry.New()
	entry := provenPinnedEntry(t, reg, "myapp", "203.0.113.1", fp)
	p := &Proxy{Registry: reg, BaseDomain: "kipper.run", EnforceProof: true}

	// A first exchange over a pooled (keep-alive) connection to the proven key.
	first := viaBackend(t, p.proxyFor("myapp.kipper.run", entry), ts)
	if code, _ := proxyGet(first); code != http.StatusOK {
		t.Fatalf("the proven key must serve: got %d", code)
	}

	// The cluster rotates: another key is pinned and proves itself. The backend
	// still serves the old key, standing in for a connection that is still open
	// to it.
	rotated := strings.Repeat("c", 64)
	if !reg.ActivatePin("myapp", entry.Token, rotated) {
		t.Fatal("rotate")
	}
	nonce, _, _ := reg.IssueChallenge("myapp", entry.Token)
	if !reg.RecordProof("myapp", entry.Token, nonce, rotated, "kipper-hop-proof-v1") {
		t.Fatal("record proof")
	}

	second := p.proxyFor("myapp.kipper.run", entry)
	if second == first {
		t.Fatal("a change of authorised key must not reuse the transport that pooled connections to the old one")
	}
	if code, _ := proxyGet(viaBackend(t, second, ts)); code != http.StatusBadGateway {
		t.Errorf("the superseded key must not receive further requests, got %d", code)
	}
}

// Ending rotation grace must actually end it. A connection pooled to the previous
// key while it was accepted must not keep serving after a re-assertion clears the
// slot — the pin set is the hop's only identity control while enforcement is off,
// so grace has to last no longer than the rotation it bridged.
func TestProxyStopsServingPreviousKeyOnceGraceEnds(t *testing.T) {
	ts, fp := pinBackend(t)
	now := time.Now().UTC().Format(time.RFC3339)
	longAgo := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	current := strings.Repeat("d", 64)
	// Mid-rotation, well past the first-pin settle window: the backend's key is
	// the displaced one, accepted as previous while propagation completes.
	reg := registryFromSnapshot(t, `{"entries":[{"subdomain":"myapp","ip":"203.0.113.1","token":"tok","created_at":"`+longAgo+
		`","last_seen":"`+now+`","cert_fingerprint":"`+current+
		`","prev_fingerprint":"`+fp+`","prev_since":"`+now+
		`","pin_updated_at":"`+now+`","first_pinned_at":"`+longAgo+`"}]}`)
	entry := reg.Lookup("myapp")
	p := &Proxy{Registry: reg, BaseDomain: "kipper.run"}

	during := viaBackend(t, p.proxyFor("myapp.kipper.run", entry), ts)
	if code, _ := proxyGet(during); code != http.StatusOK {
		t.Fatalf("the previous key must serve while the rotation propagates: got %d", code)
	}

	// The cluster re-asserts the enforced pin, which ends grace.
	if got := reg.AssertPin("myapp", "tok", current); got != registry.AssertActive {
		t.Fatalf("expected the re-assertion to be active, got %v", got)
	}
	after := p.proxyFor("myapp.kipper.run", entry)
	if after == during {
		t.Fatal("ending grace must not leave requests on the transport that pooled connections to the previous key")
	}
	if code, _ := proxyGet(viaBackend(t, after, ts)); code != http.StatusBadGateway {
		t.Errorf("once grace ends the previous key must fail closed, got %d", code)
	}
}

// The first-pin settle tolerance must not outlive its window through a pooled
// connection: once the window closes, requests move onto a transport that has to
// handshake again, and the tolerated leaf is refused. This holds during the pin
// rollout, where enforcement is still off and the pin set is the only guard.
func TestProxyStopsServingSettleWindowLeafAfterItCloses(t *testing.T) {
	ts, _ := pinBackend(t) // the backend's leaf matches no pin
	now := time.Now().UTC().Format(time.RFC3339)
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	pin := strings.Repeat("a", 64)
	snapshot := func(firstPinnedAt string) string {
		return `{"entries":[{"subdomain":"myapp","ip":"203.0.113.1","token":"tok","created_at":"` + now +
			`","last_seen":"` + now + `","cert_fingerprint":"` + pin +
			`","pin_updated_at":"` + firstPinnedAt + `","first_pinned_at":"` + firstPinnedAt + `"}]}`
	}

	path := filepath.Join(t.TempDir(), "registry.json")
	write := func(snap string) {
		if err := os.WriteFile(path, []byte(snap), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	reg := registry.New()
	write(snapshot(now)) // inside the settle window
	if err := reg.LoadFrom(path); err != nil {
		t.Fatal(err)
	}
	p := &Proxy{Registry: reg, BaseDomain: "kipper.run"}

	inSettle := viaBackend(t, p.proxyFor("myapp.kipper.run", reg.Lookup("myapp")), ts)
	if code, _ := proxyGet(inSettle); code != http.StatusOK {
		t.Fatalf("inside the settle window the lagging leaf must proxy: got %d", code)
	}

	// The window has closed (the same entry, first pinned an hour ago).
	write(snapshot(past))
	if err := reg.LoadFrom(path); err != nil {
		t.Fatal(err)
	}
	after := p.proxyFor("myapp.kipper.run", reg.Lookup("myapp"))
	if after == inSettle {
		t.Fatal("once the settle window closes, requests must not reuse the transport that tolerated the leaf")
	}
	if code, _ := proxyGet(viaBackend(t, after, ts)); code != http.StatusBadGateway {
		t.Errorf("after the settle window the unpinned leaf must fail closed, got %d", code)
	}
}

// A pin change that lands between the routing gate and the handshake must not
// slip an unproven key through: the handshake authorises the key it observes,
// not the pin the gate saw.
func TestVerifyPinRejectsKeyPinnedAfterTheGate(t *testing.T) {
	ts, fp := pinBackend(t)
	reg := registry.New()
	// Proven for another key; the backend's key is pinned only now, as the
	// token holder's assertion would do mid-request.
	entry := provenPinnedEntry(t, reg, "myapp", "203.0.113.1", strings.Repeat("a", 64))
	p := &Proxy{Registry: reg, BaseDomain: "kipper.run", EnforceProof: true}
	rp := proxyVia(t, p, entry, ts, true)
	if !reg.ActivatePin("myapp", entry.Token, fp) {
		t.Fatal("activate")
	}

	if code, _ := proxyGet(rp); code != http.StatusBadGateway {
		t.Errorf("a key pinned without its own proof must not receive the exchange, got %d", code)
	}
}

// An unproven previous key — seeded during the transition or by a persisted
// snapshot — must not serve once enforcement is on, even though the pin set
// still accepts it for the rotation window.
func TestVerifyPinRejectsUnprovenPreviousKeyWhenEnforcing(t *testing.T) {
	ts, fp := pinBackend(t)
	reg := registry.New()
	entry, _, _ := reg.Register("myapp", "203.0.113.1", "")
	// Transition mode: the backend's key was pinned, then displaced before
	// anything was ever proven, so it sits in Prev with no proof of its own.
	if !reg.ActivatePin("myapp", entry.Token, fp) {
		t.Fatal("activate")
	}
	current := strings.Repeat("b", 64)
	if !reg.ActivatePin("myapp", entry.Token, current) {
		t.Fatal("rotate")
	}
	nonce, _, _ := reg.IssueChallenge("myapp", entry.Token)
	if !reg.RecordProof("myapp", entry.Token, nonce, current, "kipper-hop-proof-v1") {
		t.Fatal("record proof")
	}
	if s := reg.PinState("myapp"); s.Prev != fp {
		t.Fatalf("expected the displaced key in Prev, got %+v", s)
	}

	enforcing := &Proxy{Registry: reg, BaseDomain: "kipper.run", EnforceProof: true}
	if code, _ := proxyGet(proxyVia(t, enforcing, entry, ts, true)); code != http.StatusBadGateway {
		t.Errorf("an unproven previous key must not serve under enforcement, got %d", code)
	}
	// Transition mode keeps B5's rotation grace so the pin rollout cannot break
	// a cluster whose replicas still serve the pre-rotation key.
	transition := &Proxy{Registry: reg, BaseDomain: "kipper.run", EnforceProof: false}
	if code, _ := proxyGet(proxyVia(t, transition, entry, ts, true)); code != http.StatusOK {
		t.Errorf("with enforcement off the previous key must still proxy, got %d", code)
	}
}

// The first-pin settle window tolerates an unknown leaf so a lagging Traefik
// replica does not 502 during the pin rollout. Under enforcement that tolerance
// must not become an interception window (Codex Finding 8).
func TestVerifyPinSettleWindowRejectsUnknownLeafWhenEnforcing(t *testing.T) {
	ts, _ := pinBackend(t) // the backend's key matches neither the pin nor the proof
	now := time.Now().UTC().Format(time.RFC3339)
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	pin := strings.Repeat("a", 64)
	// Pinned and proven for the same key, inside the settle window.
	reg := registryFromSnapshot(t, `{"entries":[{"subdomain":"myapp","ip":"203.0.113.1","token":"tok","created_at":"`+now+
		`","last_seen":"`+now+`","cert_fingerprint":"`+pin+
		`","pin_updated_at":"`+now+`","first_pinned_at":"`+now+
		`","proven_at":"`+now+`","proof_expiry":"`+future+`","proof_key_spki":"`+pin+`"}]}`)
	entry := reg.Lookup("myapp")

	enforcing := &Proxy{Registry: reg, BaseDomain: "kipper.run", EnforceProof: true}
	if code, _ := proxyGet(proxyVia(t, enforcing, entry, ts, true)); code != http.StatusBadGateway {
		t.Errorf("the settle window must not admit an unproven leaf under enforcement, got %d", code)
	}
	transition := &Proxy{Registry: reg, BaseDomain: "kipper.run", EnforceProof: false}
	if code, _ := proxyGet(proxyVia(t, transition, entry, ts, true)); code != http.StatusOK {
		t.Errorf("with enforcement off the settle window still tolerates the lagging leaf, got %d", code)
	}
}

// The handshake that promotes a pending key is not a loophole: under enforcement
// it fails rather than serving one exchange to a key with no proof.
func TestVerifyPinPromotionWithoutProofFailsWhenEnforcing(t *testing.T) {
	ts, fp := pinBackend(t)
	reg := registry.New()
	entry, _, _ := reg.Register("myapp", "203.0.113.1", "")
	// Proven for a different key, so the promotion below leaves the pin
	// unauthorised.
	nonce, ok, _ := reg.IssueChallenge("myapp", entry.Token)
	if !ok {
		t.Fatal("issue challenge")
	}
	if !reg.RecordProof("myapp", entry.Token, nonce, strings.Repeat("a", 64), "kipper-hop-proof-v1") {
		t.Fatal("record proof")
	}
	if !reg.StorePendingPin("myapp", entry.Token, fp) {
		t.Fatal("store pending")
	}
	p := &Proxy{Registry: reg, BaseDomain: "kipper.run", EnforceProof: true}

	code, _ := proxyGet(proxyVia(t, p, entry, ts, false))
	if code != http.StatusBadGateway {
		t.Errorf("promoting an unproven key must fail the handshake when enforcing, got %d", code)
	}
	// The promotion itself still happens, so the cluster's next beat asserts and
	// proves the key it now serves.
	if s := reg.PinState("myapp"); s.Current != fp {
		t.Errorf("expected the observed key promoted to current, got %+v", s)
	}
}

// With enforcement off (transition mode) the same promotion proxies, so the pin
// rollout cannot break routing before the fleet has proofs.
func TestVerifyPinPromotionServesWhenNotEnforcing(t *testing.T) {
	ts, fp := pinBackend(t)
	reg := registry.New()
	entry, _, _ := reg.Register("myapp", "203.0.113.1", "")
	if !reg.StorePendingPin("myapp", entry.Token, fp) {
		t.Fatal("store pending")
	}
	p := &Proxy{Registry: reg, BaseDomain: "kipper.run", EnforceProof: false}

	if code, _ := proxyGet(proxyVia(t, p, entry, ts, false)); code != http.StatusOK {
		t.Errorf("with enforcement off a promoting handshake must proxy, got %d", code)
	}
}

// Each cached transport serves exactly one cluster, so the default of two idle
// connections per host means a burst re-handshakes TLS and re-runs the pin
// check for nearly every request. This is the whole throughput ceiling of the
// data plane, so it is worth an explicit assertion rather than trusting the
// clone to carry a sensible value.
func TestProxyTransportPoolsConnectionsPerCluster(t *testing.T) {
	p := &Proxy{Registry: registry.New(), BaseDomain: "kipper.run"}
	entry := &registry.Entry{Subdomain: "203-0-113-12", IP: "203.0.113.12"}

	proxy := p.proxyFor("myapp--203-0-113-12.kipper.run", entry)
	if proxy == nil {
		t.Fatal("expected a proxy, got nil")
	}
	transport, ok := proxy.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected an *http.Transport, got %T", proxy.Transport)
	}

	def := http.DefaultTransport.(*http.Transport)
	if transport.MaxIdleConnsPerHost <= def.MaxIdleConnsPerHost {
		t.Errorf("per-host idle pool is %d, no better than the default %d",
			transport.MaxIdleConnsPerHost, def.MaxIdleConnsPerHost)
	}
	if transport.MaxIdleConnsPerHost != transport.MaxIdleConns {
		t.Errorf("the per-host pool (%d) should match the total idle budget (%d): one transport serves one cluster",
			transport.MaxIdleConnsPerHost, transport.MaxIdleConns)
	}
}

// The proxy middleware short-circuits the chain for every registered host, so
// the API limiter registered after it never runs on proxied traffic. This test
// drives the real router rather than the middleware alone: a unit test of
// Middleware cannot see someone reordering the chain, and that ordering is the
// whole reason the data plane was unmetered.
func TestDataPlaneIsMeteredThroughTheRealStack(t *testing.T) {
	reg := registry.New()
	// 127.0.0.1 refuses instantly, so a proxied request fails fast instead of
	// waiting out a dial timeout to an unroutable address.
	if _, _, err := reg.Register("203-0-113-12", "127.0.0.1", ""); err != nil {
		t.Fatalf("register: %v", err)
	}

	const budget = 3
	proxy := &Proxy{
		Registry:     reg,
		BaseDomain:   "kipper.run",
		ClientBudget: httprate.NewRateLimiter(budget, time.Minute),
	}
	router := newRouter(reg, proxy, "kipper.run", "")

	call := func(host string) int {
		req := httptest.NewRequest(http.MethodGet, "http://"+host+"/", nil)
		req.Host = host
		req.Header.Set(clientIPHeader, "203.0.113.99")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w.Code
	}

	// The exact registered host is proxied traffic and must be metered.
	for i := 0; i < budget; i++ {
		if code := call("203-0-113-12.kipper.run"); code == http.StatusTooManyRequests {
			t.Fatalf("request %d was limited before the budget was spent", i+1)
		}
	}
	if code := call("203-0-113-12.kipper.run"); code != http.StatusTooManyRequests {
		t.Errorf("the exact registered host is unmetered: got %d after %d requests, want 429", code, budget)
	}

	// And so is every derived route of that cluster: they resolve through the
	// same wildcard and reach the same machine.
	if code := call("console--203-0-113-12.kipper.run"); code != http.StatusTooManyRequests {
		t.Errorf("the derived route is unmetered: got %d, want 429", code)
	}

	// A different client keeps its own budget, so one noisy address cannot deny
	// the platform to everyone else.
	req := httptest.NewRequest(http.MethodGet, "http://203-0-113-12.kipper.run/", nil)
	req.Host = "203-0-113-12.kipper.run"
	req.Header.Set(clientIPHeader, "203.0.113.100")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code == http.StatusTooManyRequests {
		t.Error("a second client address shares the first one's budget")
	}
}

// A per-client budget cannot bound a distributed source, so the destination
// needs its own ceiling: this hop can cost the cluster a fresh TLS handshake per
// request, which makes it an amplifier worth capping.
func TestClusterInFlightCapIsSharedAcrossDerivedRoutes(t *testing.T) {
	p := &Proxy{Registry: registry.New(), BaseDomain: "kipper.run", ClusterInFlight: 2}

	first, ok := p.enterCluster("203-0-113-12")
	if !ok {
		t.Fatal("the first request must be admitted")
	}
	second, ok := p.enterCluster("203-0-113-12")
	if !ok {
		t.Fatal("the second request must be admitted")
	}
	if _, ok := p.enterCluster("203-0-113-12"); ok {
		t.Error("the third concurrent request must be refused at a cap of 2")
	}
	// A different cluster has its own budget.
	if _, ok := p.enterCluster("198-51-100-7"); !ok {
		t.Error("one cluster's saturation must not refuse another's traffic")
	}

	first()
	if release, ok := p.enterCluster("203-0-113-12"); !ok {
		t.Error("a released slot must be reusable")
	} else {
		release()
	}
	second()
}

// Zero means the operator meters at the edge instead; it must not accidentally
// mean "refuse everything".
func TestClusterInFlightCapDisabledAtZero(t *testing.T) {
	p := &Proxy{Registry: registry.New(), BaseDomain: "kipper.run", ClusterInFlight: 0}
	for i := 0; i < 100; i++ {
		release, ok := p.enterCluster("203-0-113-12")
		if !ok {
			t.Fatalf("request %d refused with the cap disabled", i+1)
		}
		release()
	}
}

// A process that has served a long tail of registrations must not carry state
// for every label it ever saw. The counter is dropped only when it reads zero,
// because removing one a request still holds would let the next arrival install
// a second counter and route past the ceiling.
func TestForgetClusterDropsPerRegistrationState(t *testing.T) {
	p := &Proxy{Registry: registry.New(), BaseDomain: "kipper.run", ClusterInFlight: 2}
	p.proofSkipLogged.Store("203-0-113-12", time.Now())
	p.graceLogged.Store("203-0-113-12", time.Now())

	release, ok := p.enterCluster("203-0-113-12")
	if !ok {
		t.Fatal("the first request must be admitted")
	}

	// A registration with a live request keeps its counter.
	p.forgetCluster("203-0-113-12")
	if _, held := p.proofSkipLogged.Load("203-0-113-12"); held {
		t.Error("the proof-skip log throttle was not dropped")
	}
	if _, held := p.graceLogged.Load("203-0-113-12"); held {
		t.Error("the grace log throttle was not dropped")
	}
	p.inFlightMu.Lock()
	_, stillCounted := p.inFlight["203-0-113-12"]
	p.inFlightMu.Unlock()
	if !stillCounted {
		t.Error("a counter with a live request was removed, so the ceiling can be doubled")
	}

	// Once the request finishes, the counter goes too.
	release()
	p.forgetCluster("203-0-113-12")
	p.inFlightMu.Lock()
	_, leaked := p.inFlight["203-0-113-12"]
	p.inFlightMu.Unlock()
	if leaked {
		t.Error("the in-flight counter outlived the registration")
	}
}

// If the reverse proxy in front stops setting the client header, falling back to
// one shared key would put every cluster in a single bucket and let one caller
// throttle the whole platform. The fallback is per destination instead.
func TestUnmeasuredClientsAreMeteredPerDestination(t *testing.T) {
	reg := registry.New()
	for _, sub := range []string{"203-0-113-12", "198-51-100-7"} {
		if _, _, err := reg.Register(sub, "127.0.0.1", ""); err != nil {
			t.Fatalf("register %s: %v", sub, err)
		}
	}

	const budget = 2
	proxy := &Proxy{
		Registry:     reg,
		BaseDomain:   "kipper.run",
		ClientBudget: httprate.NewRateLimiter(budget, time.Minute),
	}
	router := newRouter(reg, proxy, "kipper.run", "")

	// No client header at all: the client cannot be measured.
	call := func(host string) int {
		req := httptest.NewRequest(http.MethodGet, "http://"+host+"/", nil)
		req.Host = host
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w.Code
	}

	for i := 0; i < budget; i++ {
		if code := call("203-0-113-12.kipper.run"); code == http.StatusTooManyRequests {
			t.Fatalf("request %d limited before the budget was spent", i+1)
		}
	}
	if code := call("203-0-113-12.kipper.run"); code != http.StatusTooManyRequests {
		t.Errorf("unmeasured traffic to a cluster is unmetered: got %d, want 429", code)
	}
	// The other cluster has its own budget, so one saturated destination does
	// not throttle the platform.
	if code := call("198-51-100-7.kipper.run"); code == http.StatusTooManyRequests {
		t.Error("a second cluster shares the first one's unmeasured budget")
	}
}
