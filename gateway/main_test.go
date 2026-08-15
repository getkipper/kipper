package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	chimw "github.com/go-chi/chi/v5/middleware"

	"github.com/getkipper/kipper/gateway/internal/registry"
)

func TestHealthEndpointIsLivenessOnly(t *testing.T) {
	handler := handleHealth()

	r := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var resp map[string]any
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if resp["status"] != "ok" {
		t.Errorf("expected ok status, got %v", resp["status"])
	}
	// Caddy proxies this path to anyone. Whether enforcement is on, and which
	// registrations are unpinned or unproven, tells a caller exactly when the
	// gateway is fail-open and what to aim at.
	for _, leaked := range []string{"proof_before_route", "unpinned", "unproven", "registrations",
		"unpinned_oldest_seconds", "unproven_oldest_seconds"} {
		if _, present := resp[leaked]; present {
			t.Errorf("the anonymous liveness answer discloses %q: %v", leaked, resp)
		}
	}
}

// The posture the cutover audit and monitoring read stays available, just not
// anonymously.
func TestStatusEndpointNeedsItsToken(t *testing.T) {
	reg := registry.New()
	if _, _, err := reg.Register("myapp", "203.0.113.1", ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	handler := handleStatus(reg, "s3cret")

	get := func(auth string) (int, map[string]any) {
		r := httptest.NewRequest(http.MethodGet, "/status", nil)
		if auth != "" {
			r.Header.Set("Authorization", auth)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		var resp map[string]any
		_ = json.NewDecoder(w.Body).Decode(&resp)
		return w.Code, resp
	}

	if code, _ := get(""); code != http.StatusUnauthorized {
		t.Errorf("no credential: got %d, want 401", code)
	}
	if code, _ := get("Bearer wrong"); code != http.StatusUnauthorized {
		t.Errorf("wrong token: got %d, want 401", code)
	}

	code, resp := get("Bearer s3cret")
	if code != http.StatusOK {
		t.Fatalf("with the token: got %d, want 200", code)
	}
	for _, field := range []string{"registrations", "unpinned", "unpinned_oldest_seconds",
		"unproven", "unproven_oldest_seconds", "proof_before_route"} {
		if _, ok := resp[field]; !ok {
			t.Errorf("status response missing %q, which monitoring reads: %v", field, resp)
		}
	}
}

// An unconfigured gateway should not advertise that it has an endpoint worth
// guessing a credential for.
func TestStatusEndpointIsAbsentWithoutAToken(t *testing.T) {
	handler := handleStatus(registry.New(), "")

	r := httptest.NewRequest(http.MethodGet, "/status", nil)
	r.Header.Set("Authorization", "Bearer anything")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Errorf("got %d, want 404 when no status token is configured", w.Code)
	}
}

func TestStatusReportsUnpinnedAge(t *testing.T) {
	reg := registry.New()
	_, _, _ = reg.Register("bare", "203.0.113.2", "")

	r := httptest.NewRequest(http.MethodGet, "/status", nil)
	r.Header.Set("Authorization", "Bearer s3cret")
	w := httptest.NewRecorder()
	handleStatus(reg, "s3cret").ServeHTTP(w, r)

	var resp map[string]any
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if resp["unpinned"] != float64(1) {
		t.Errorf("expected 1 unpinned registration, got %v", resp["unpinned"])
	}
	if age, ok := resp["unpinned_oldest_seconds"].(float64); !ok || age < 0 {
		t.Errorf("expected a non-negative oldest age, got %v", resp["unpinned_oldest_seconds"])
	}
}

func TestRegisterSuccess(t *testing.T) {
	reg := registry.New()
	handler := handleRegister(reg, "kipper.run", neverObserve)

	body := `{"subdomain":"myapp","ip":"198.51.100.1"}`
	r := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusCreated {
		t.Errorf("expected 201, got %d", w.Code)
	}

	var resp registerResponse
	_ = json.NewDecoder(w.Body).Decode(&resp)

	if resp.Subdomain != "myapp" {
		t.Errorf("expected myapp, got %s", resp.Subdomain)
	}
	if resp.Domain != "myapp.kipper.run" {
		t.Errorf("expected myapp.kipper.run, got %s", resp.Domain)
	}
	if resp.Token == "" {
		t.Error("expected non-empty token")
	}
}

func TestRegisterInvalidSubdomain(t *testing.T) {
	reg := registry.New()
	handler := handleRegister(reg, "kipper.run", neverObserve)

	tests := []string{
		`{"subdomain":"UPPER","ip":"203.0.113.1"}`,
		`{"subdomain":"has space","ip":"203.0.113.1"}`,
		`{"subdomain":"-starts-with-dash","ip":"203.0.113.1"}`,
		`{"subdomain":"","ip":"203.0.113.1"}`,
	}

	for _, body := range tests {
		r := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(body))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)

		if w.Code != http.StatusBadRequest {
			t.Errorf("body %s: expected 400, got %d", body, w.Code)
		}
	}
}

func TestRegisterMissingIP(t *testing.T) {
	reg := registry.New()
	handler := handleRegister(reg, "kipper.run", neverObserve)

	body := `{"subdomain":"myapp","ip":""}`
	r := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestRegisterDuplicate(t *testing.T) {
	reg := registry.New()
	handler := handleRegister(reg, "kipper.run", neverObserve)

	body := `{"subdomain":"myapp","ip":"203.0.113.1"}`
	r := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	// Register again with different IP
	body2 := `{"subdomain":"myapp","ip":"203.0.113.2"}`
	r2 := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(body2))
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, r2)

	if w2.Code != http.StatusConflict {
		t.Errorf("expected 409, got %d", w2.Code)
	}
}

func TestDeregisterSuccess(t *testing.T) {
	reg := registry.New()
	entry, _, _ := reg.Register("myapp", "203.0.113.1", "")

	handler := handleDeregister(reg, &Proxy{Registry: reg, BaseDomain: "kipper.run"})
	body := `{"token":"` + entry.Token + `"}`
	r := httptest.NewRequest(http.MethodDelete, "/register", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusNoContent {
		t.Errorf("expected 204, got %d", w.Code)
	}

	if reg.Lookup("myapp") != nil {
		t.Error("expected subdomain to be removed")
	}
}

func TestDeregisterInvalidToken(t *testing.T) {
	reg := registry.New()
	handler := handleDeregister(reg, &Proxy{Registry: reg, BaseDomain: "kipper.run"})

	body := `{"token":"bogus"}`
	r := httptest.NewRequest(http.MethodDelete, "/register", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestPingSuccess(t *testing.T) {
	reg := registry.New()
	entry, _, _ := reg.Register("myapp", "203.0.113.1", "")

	handler := handlePing(reg)
	body := `{"token":"` + entry.Token + `"}`
	r := httptest.NewRequest(http.MethodPost, "/ping", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

// neverObserve fails every verification dial; tests exercising the pin path
// inject their own observation.
func neverObserve(ip, sni string) (string, error) {
	return "", fmt.Errorf("no network in tests")
}

// fixedObserve reports a fixed fingerprint for every verification dial.
func fixedObserve(fp string) observeFunc {
	return func(ip, sni string) (string, error) { return fp, nil }
}

// pinTestSetup registers a subdomain and points dataPath at a scratch file so
// pin persistence succeeds.
func pinTestSetup(t *testing.T) (*registry.Registry, *registry.Entry) {
	t.Helper()
	dataPath = filepath.Join(t.TempDir(), "registry.json")
	reg := registry.New()
	entry, _, err := reg.Register("myapp", "203.0.113.1", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	return reg, entry
}

func assertPin(t *testing.T, handler http.HandlerFunc, fp, token string) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"subdomain":"myapp","ip":"203.0.113.1","certFingerprint":%q,"token":%q}`, fp, token)
	r := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

var (
	fpA = strings.Repeat("a", 64)
	fpB = strings.Repeat("b", 64)
)

func TestRegisterCreationIgnoresFingerprint(t *testing.T) {
	dataPath = filepath.Join(t.TempDir(), "registry.json")
	reg := registry.New()
	handler := handleRegister(reg, "kipper.run", fixedObserve(fpA))

	body := fmt.Sprintf(`{"subdomain":"myapp","ip":"203.0.113.1","certFingerprint":%q,"token":"whatever"}`, fpA)
	r := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", w.Code)
	}
	var resp registerResponse
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if resp.Token == "" {
		t.Error("creation should still return the token")
	}
	if resp.Pin != pinNone {
		t.Errorf("creation must not process the fingerprint, got pin %q", resp.Pin)
	}
	if s := reg.PinState("myapp"); s.Current != "" || s.Pending != "" {
		t.Errorf("creation must store no pin state, got %+v", s)
	}
}

func TestPinAssertActivatesWhenObserved(t *testing.T) {
	reg, entry := pinTestSetup(t)
	handler := handleRegister(reg, "kipper.run", fixedObserve(fpA))

	w := assertPin(t, handler, fpA, entry.Token)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var resp registerResponse
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if resp.Pin != pinActive {
		t.Errorf("expected pin active, got %q", resp.Pin)
	}
	if s := reg.PinState("myapp"); s.Current != fpA {
		t.Errorf("expected current pin %s, got %+v", fpA, s)
	}
}

func TestPinAssertPendingOnMismatch(t *testing.T) {
	reg, entry := pinTestSetup(t)
	handler := handleRegister(reg, "kipper.run", fixedObserve(fpB))

	w := assertPin(t, handler, fpA, entry.Token)
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}
	var resp registerResponse
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if resp.Pin != pinPending || resp.AssertedFingerprint != fpA || resp.ObservedFingerprint != fpB {
		t.Errorf("unexpected pending response: %+v", resp)
	}
	s := reg.PinState("myapp")
	if s.Current != "" {
		t.Errorf("an unobserved assertion must not become the enforced pin, got current %q", s.Current)
	}
	if s.Pending != fpA {
		t.Errorf("expected pending %s, got %q", fpA, s.Pending)
	}
}

func TestPinAssertPendingOnDialFailure(t *testing.T) {
	reg, entry := pinTestSetup(t)
	handler := handleRegister(reg, "kipper.run", neverObserve)

	w := assertPin(t, handler, fpA, entry.Token)
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", w.Code)
	}
	if s := reg.PinState("myapp"); s.Pending != fpA || s.Current != "" {
		t.Errorf("expected pending-only state, got %+v", s)
	}
}

func TestPinAssertRejectsBadToken(t *testing.T) {
	reg, _ := pinTestSetup(t)
	handler := handleRegister(reg, "kipper.run", fixedObserve(fpA))

	w := assertPin(t, handler, fpA, "wrong-token")
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
	if s := reg.PinState("myapp"); s.Current != "" || s.Pending != "" {
		t.Errorf("a bad token must not change pin state, got %+v", s)
	}
}

func TestPinAssertRejectsMalformedFingerprint(t *testing.T) {
	reg, entry := pinTestSetup(t)
	handler := handleRegister(reg, "kipper.run", fixedObserve(fpA))

	for _, fp := range []string{"short", strings.Repeat("A", 64), strings.Repeat("z", 64)} {
		w := assertPin(t, handler, fp, entry.Token)
		if w.Code != http.StatusBadRequest {
			t.Errorf("fingerprint %q: expected 400, got %d", fp, w.Code)
		}
	}
}

func TestPinRotationKeepsPreviousAccepted(t *testing.T) {
	reg, entry := pinTestSetup(t)

	w := assertPin(t, handleRegister(reg, "kipper.run", fixedObserve(fpA)), fpA, entry.Token)
	if w.Code != http.StatusCreated {
		t.Fatalf("first activation: expected 201, got %d", w.Code)
	}
	// The cluster rotates its key: it now serves and asserts fpB.
	w = assertPin(t, handleRegister(reg, "kipper.run", fixedObserve(fpB)), fpB, entry.Token)
	if w.Code != http.StatusCreated {
		t.Fatalf("rotation: expected 201, got %d", w.Code)
	}
	s := reg.PinState("myapp")
	if s.Current != fpB || s.Prev != fpA {
		t.Errorf("expected current %s with prev %s, got %+v", fpB, fpA, s)
	}
	// The next unchanged assertion ends the rotation grace.
	w = assertPin(t, handleRegister(reg, "kipper.run", fixedObserve(fpB)), fpB, entry.Token)
	if w.Code != http.StatusCreated {
		t.Fatalf("steady state: expected 201, got %d", w.Code)
	}
	if s := reg.PinState("myapp"); s.Prev != "" || s.Pending != "" {
		t.Errorf("unchanged assertion must clear rotation grace, got %+v", s)
	}
}

func TestPlainRenewalReportsPinState(t *testing.T) {
	reg, entry := pinTestSetup(t)
	handler := handleRegister(reg, "kipper.run", fixedObserve(fpA))

	renew := func() registerResponse {
		body := `{"subdomain":"myapp","ip":"203.0.113.1"}`
		r := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(body))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		var resp registerResponse
		_ = json.NewDecoder(w.Body).Decode(&resp)
		return resp
	}

	if resp := renew(); resp.Pin != pinNone {
		t.Errorf("unpinned renewal: expected pin none, got %q", resp.Pin)
	}
	assertPin(t, handler, fpA, entry.Token)
	if resp := renew(); resp.Pin != pinActive {
		t.Errorf("pinned renewal: expected pin active, got %q", resp.Pin)
	}
}

func TestRegisterBodyCap(t *testing.T) {
	reg := registry.New()
	handler := handleRegister(reg, "kipper.run", neverObserve)

	body := `{"subdomain":"` + strings.Repeat("a", int(maxRequestBody)) + `","ip":"203.0.113.1"}`
	r := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for an oversized body, got %d", w.Code)
	}
}

// The rate-limit bucket has to survive an IPv6 client walking around inside the
// prefix it was delegated: keying on the full address would hand out a fresh
// quota per request, which is how a limit stops being a limit.
func TestRateLimitKeyBucketsIPv6ByPrefix(t *testing.T) {
	key := func(ip string) string {
		req := httptest.NewRequest(http.MethodPost, "/register", nil)
		req.Header.Set(clientIPHeader, ip)
		var got string
		chimw.ClientIPFromHeader(clientIPHeader)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			k, err := rateLimitKey(r)
			if err != nil {
				t.Fatalf("rate limit key for %s: %v", ip, err)
			}
			got = k
		})).ServeHTTP(httptest.NewRecorder(), req)
		return got
	}

	sameNet := []string{"2001:db8:1:2::1", "2001:db8:1:2::dead:beef", "2001:db8:1:2:ffff:ffff:ffff:ffff"}
	first := key(sameNet[0])
	if first == "" {
		t.Fatal("a measured client must produce a key")
	}
	for _, ip := range sameNet[1:] {
		if got := key(ip); got != first {
			t.Errorf("%s keyed as %q, want the same bucket as %s (%q)", ip, got, sameNet[0], first)
		}
	}
	if other := key("2001:db8:1:3::1"); other == first {
		t.Errorf("a different /64 must get its own bucket, both keyed as %q", other)
	}

	// IPv4 keeps its own address as the bucket — no prefix aggregation.
	if got := key("203.0.113.7"); got != "203.0.113.7" {
		t.Errorf("IPv4 must key on the address itself, got %q", got)
	}
	if key("203.0.113.7") == key("203.0.113.8") {
		t.Error("two IPv4 clients must not share a bucket")
	}

	// No measurable client: one shared bucket, rate limited rather than exempt.
	req := httptest.NewRequest(http.MethodPost, "/register", nil)
	var unmeasured string
	chimw.ClientIPFromHeader(clientIPHeader)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		unmeasured, _ = rateLimitKey(r)
	})).ServeHTTP(httptest.NewRecorder(), req)
	if unmeasured != "" {
		t.Errorf("an unmeasurable client must fall into the shared bucket, got %q", unmeasured)
	}
}

// A restart must not cut requests that are already being served: this process
// is the only path to every cluster behind it, so a dropped connection is
// visible to every user of every one of them. The registry flush matters just
// as much — ping-driven LastSeen updates that the periodic flush has not
// written yet would otherwise be lost, ageing live registrations towards expiry.
func TestServeDrainsInFlightRequestsThenFlushes(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/slow", func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("finished"))
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	ctx, cancel := context.WithCancel(context.Background())
	flushed := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- serve(ctx, srv, ln, nil, func() error { close(flushed); return nil }) }()

	// Start a request and hold it open, then ask the server to stop.
	result := make(chan string, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/slow") //nolint:noctx // test client
		if err != nil {
			result <- "error: " + err.Error()
			return
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		result <- string(body)
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("request never reached the handler")
	}
	cancel()

	// The drain must still be waiting on the in-flight request.
	select {
	case <-flushed:
		t.Fatal("flushed before the in-flight request finished")
	case <-time.After(100 * time.Millisecond):
	}
	// And the listener must already be closed, so nothing new gets in.
	if _, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second); err == nil {
		t.Error("the listener still accepts connections during the drain")
	}

	close(release)
	select {
	case got := <-result:
		if got != "finished" {
			t.Errorf("in-flight request did not complete cleanly, got %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight request never completed")
	}
	select {
	case <-flushed:
	case <-time.After(5 * time.Second):
		t.Fatal("the registry was never flushed on shutdown")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serve returned %v, want nil after a clean drain", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve never returned")
	}
}

// A serving failure is the one thing serve raises, so the process exits instead
// of sitting there serving nothing.
func TestServeReturnsServingFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	// A closed listener fails Serve immediately with something other than
	// ErrServerClosed.
	_ = ln.Close()

	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second}
	errCh := make(chan error, 1)
	go func() { errCh <- serve(context.Background(), srv, ln, nil, nil) }()

	select {
	case err := <-errCh:
		if err == nil {
			t.Error("a listener that cannot serve must be reported")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve never reported the failure")
	}
}

// The drain has to finish inside the container's stop grace period, or the
// runtime kills the process mid-drain and the shutdown flush never runs.
func TestShutdownGraceFitsTheContainerStopPeriod(t *testing.T) {
	compose, err := os.ReadFile("docker-compose.yml")
	if err != nil {
		t.Fatalf("reading docker-compose.yml: %v", err)
	}
	m := regexp.MustCompile(`stop_grace_period:\s*(\d+)s`).FindSubmatch(compose)
	if m == nil {
		t.Fatal("the gateway service declares no stop_grace_period, so the runtime default (10s) can cut the drain")
	}
	grace, err := strconv.Atoi(string(m[1]))
	if err != nil {
		t.Fatalf("parsing stop_grace_period: %v", err)
	}
	if time.Duration(grace)*time.Second <= shutdownGrace {
		t.Errorf("stop_grace_period is %ds but the drain takes up to %s", grace, shutdownGrace)
	}
}

// The keep-alive pool is a process-wide budget, not a per-transport one: there
// is a transport per cache key, so the ceiling is idlePerTransport ×
// maxCachedProxies and it has to leave room for the two descriptors every
// in-flight exchange holds. Raising either side alone must fail here rather than
// produce EMFILE on the shared data plane.
func TestIdlePoolFitsTheDescriptorBudget(t *testing.T) {
	compose, err := os.ReadFile("docker-compose.yml")
	if err != nil {
		t.Fatalf("reading docker-compose.yml: %v", err)
	}
	m := regexp.MustCompile(`nofile:\s*\n\s*soft:\s*(\d+)`).FindSubmatch(compose)
	if m == nil {
		t.Fatal("the gateway service declares no nofile limit, so it runs on the runtime default of 1024")
	}
	nofile, err := strconv.Atoi(string(m[1]))
	if err != nil {
		t.Fatalf("parsing nofile: %v", err)
	}

	worstCaseIdle := idlePerTransport * maxCachedProxies
	// A quarter of the budget for idle sockets leaves three quarters for the
	// client and upstream descriptors of live exchanges, plus the runtime's own.
	if worstCaseIdle > nofile/4 {
		t.Errorf("idle pools can reach %d sockets (%d per cluster × %d cached), more than a quarter of the %d descriptor budget",
			worstCaseIdle, idlePerTransport, maxCachedProxies, nofile)
	}
	if idlePerTransport <= 2 {
		t.Errorf("idlePerTransport is %d, no better than Go's default", idlePerTransport)
	}
}

// The point of the pool is that a second burst of traffic reuses the
// connections the first burst opened, instead of paying a TLS handshake and a
// pin verification again. Assert the reuse, not just the configured number.
//
// The bursts are barriered rather than merely concurrent: every request blocks
// in the handler until all of them have arrived, which forces exactly one
// connection per request in the first burst and makes the second burst's count
// a fact about pooling rather than about how the scheduler happened to
// interleave. Counting connections from unsynchronised concurrency is flaky —
// requests that finish early get reused within their own burst.
func TestIdlePoolActuallyReusesConnections(t *testing.T) {
	type barrier struct {
		arrived sync.WaitGroup
		release chan struct{}
	}
	var current atomic.Pointer[barrier]

	var opened atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		b := current.Load()
		b.arrived.Done()
		<-b.release
		w.WriteHeader(http.StatusOK)
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			opened.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()

	// Same limits the proxy transports carry.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = idlePerTransport
	transport.MaxIdleConns = idlePerTransport
	client := &http.Client{Transport: transport}

	burst := func() {
		b := &barrier{release: make(chan struct{})}
		b.arrived.Add(idlePerTransport)
		current.Store(b)

		var done sync.WaitGroup
		for i := 0; i < idlePerTransport; i++ {
			done.Add(1)
			go func() {
				defer done.Done()
				resp, err := client.Get(srv.URL) //nolint:noctx // test client
				if err != nil {
					t.Errorf("request failed: %v", err)
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}()
		}
		// Every request is now in the handler, so each holds its own connection.
		b.arrived.Wait()
		close(b.release)
		done.Wait()
	}

	burst()
	afterFirst := opened.Load()
	if afterFirst != int64(idlePerTransport) {
		t.Fatalf("the first burst opened %d connections for %d simultaneous requests, want %d",
			afterFirst, idlePerTransport, idlePerTransport)
	}

	burst()
	if extra := opened.Load() - afterFirst; extra != 0 {
		t.Errorf("the second burst opened %d new connections; a pool of %d should have served it entirely from idle",
			extra, idlePerTransport)
	}
}

// Trimming the scheme without checking it accepts the bare secret as the whole
// header, which is a different credential format than the one advertised.
func TestStatusRejectsAnythingButABearerCredential(t *testing.T) {
	const token = "s3cret"
	cases := []struct {
		name   string
		header string
		want   int
	}{
		{"the documented form", "Bearer " + token, http.StatusOK},
		{"scheme is case-insensitive per RFC 7235", "bearer " + token, http.StatusOK},
		{"padding around the credential", "Bearer   " + token + "   ", http.StatusOK},
		{"the bare secret with no scheme", token, http.StatusUnauthorized},
		{"no header at all", "", http.StatusUnauthorized},
		{"the scheme with no credential", "Bearer ", http.StatusUnauthorized},
		{"the scheme with only padding", "Bearer    ", http.StatusUnauthorized},
		{"another scheme", "Basic " + token, http.StatusUnauthorized},
		{"a wrong credential", "Bearer nope", http.StatusUnauthorized},
		{"the credential as a prefix", "Bearer " + token + "x", http.StatusUnauthorized},
		{"no separating space", "Bearer" + token, http.StatusUnauthorized},
	}

	handler := handleStatus(registry.New(), token)
	for _, tc := range cases {
		r := httptest.NewRequest(http.MethodGet, "/status", nil)
		if tc.header != "" {
			r.Header.Set("Authorization", tc.header)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, w.Code, tc.want)
		}
	}
}

// "Does not exist" has to hold for every method, not just GET: a route left
// registered answers an unsupported method with 405 while an unknown path
// answers 404, which identifies it however the handler behaves.
func TestDisabledStatusRouteIsIndistinguishableFromAnUnknownPath(t *testing.T) {
	reg := registry.New()
	proxy := &Proxy{Registry: reg, BaseDomain: "kipper.run"}
	router := newRouter(reg, proxy, "kipper.run", "")

	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodOptions, http.MethodPut} {
		probe := func(path string) int {
			r := httptest.NewRequest(method, "http://kipper.run"+path, nil)
			r.Host = "kipper.run"
			w := httptest.NewRecorder()
			router.ServeHTTP(w, r)
			return w.Code
		}
		if status, unknown := probe("/status"), probe("/no-such-path"); status != unknown {
			t.Errorf("%s /status answers %d while an unknown path answers %d, which reveals the route",
				method, status, unknown)
		}
	}
}

// And with a token configured it is reachable, so the guard above cannot be
// satisfied by disabling it everywhere.
func TestConfiguredStatusRouteIsServed(t *testing.T) {
	reg := registry.New()
	proxy := &Proxy{Registry: reg, BaseDomain: "kipper.run"}
	router := newRouter(reg, proxy, "kipper.run", "s3cret")

	r := httptest.NewRequest(http.MethodGet, "http://kipper.run/status", nil)
	r.Host = "kipper.run"
	r.Header.Set("Authorization", "Bearer s3cret")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("a configured /status must serve, got %d", w.Code)
	}
}

// Enforcement must be the value an operator gets without knowing the setting
// exists, and only an explicit denial may turn it off.
func TestProofBeforeRouteDefaultsOn(t *testing.T) {
	const name = "KIPPER_TEST_DEFAULT_TRUE"
	cases := map[string]bool{
		"":        true,
		"   ":     true,
		"true":    true,
		"1":       true,
		"garbage": true,
		"false":   false,
		"FALSE":   false,
		" off ":   false,
		"0":       false,
		"no":      false,
	}
	for value, want := range cases {
		t.Setenv(name, value)
		if got := boolEnvDefaultTrue(name); got != want {
			t.Errorf("%q: got %v, want %v", value, got, want)
		}
	}
	if err := os.Unsetenv(name); err != nil {
		t.Fatalf("unsetenv: %v", err)
	}
	if !boolEnvDefaultTrue(name) {
		t.Error("an unset variable must leave enforcement on")
	}
}

// TestMain gives the package a writable dataPath. Handlers persist the registry
// after a change, and a release is no longer acknowledged unless that write
// succeeded, so an unwritable default path turns those tests into 500s. It also
// clears the "failed to persist registry" noise the suite used to print. Tests
// that assert on the file's contents still point dataPath at their own scratch
// copy.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "kipper-gateway-test")
	if err != nil {
		panic(err)
	}
	dataPath = filepath.Join(dir, "registry.json")
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
