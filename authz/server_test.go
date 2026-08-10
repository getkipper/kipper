package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

// decodeDenial asserts the stable denial contract: JSON body with a code,
// CORS headers so browsers can read it, and Content-Type set.
func decodeDenial(t *testing.T, resp *http.Response) denialBody {
	t.Helper()
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want *", got)
	}
	var body denialBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding denial body: %v", err)
	}
	return body
}

func testServer(t *testing.T, fresh *Freshness) *httptest.Server {
	t.Helper()
	a := testAuthorizer(t, fresh, testApiKey(), testPlan())
	mux := http.NewServeMux()
	NewServer(a, fresh).Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, url, apiKey string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestHandleAuthorize_StatusCodes(t *testing.T) {
	srv := testServer(t, alwaysFresh())

	if resp := get(t, srv.URL+"/authorize?namespace=shop-prod&app=api", testKey); resp.StatusCode != http.StatusOK {
		t.Errorf("valid key: %d, want 200", resp.StatusCode)
	}
	if resp := get(t, srv.URL+"/authorize?namespace=shop-prod&app=api", "kip_bad_key"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("bad key: %d, want 401", resp.StatusCode)
	}
	if resp := get(t, srv.URL+"/authorize?namespace=shop-prod&app=api", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("missing key: %d, want 401", resp.StatusCode)
	}
	// Misconfigured middleware (no app identity) is a server-side error,
	// never a silent allow.
	if resp := get(t, srv.URL+"/authorize", testKey); resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("missing params: %d, want 500", resp.StatusCode)
	}
}

func TestHandleAuthorize_ForwardsConsumerIdentity(t *testing.T) {
	fresh := alwaysFresh()
	a := testAuthorizer(t, fresh, testApiKey(func(k *kipperv1.ApiKey) {
		k.Spec.DisplayName = "Acme partner"
	}), testPlan())
	mux := http.NewServeMux()
	NewServer(a, fresh).Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp := get(t, srv.URL+"/authorize?namespace=shop-prod&app=api", testKey)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid key: %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Kipper-Key-Prefix"); got != "ab12cd34" {
		t.Errorf("X-Kipper-Key-Prefix = %q, want the key prefix so the backend knows the consumer", got)
	}
	if got := resp.Header.Get("X-Kipper-Key-Name"); got != "Acme partner" {
		t.Errorf("X-Kipper-Key-Name = %q, want the display name", got)
	}
}

func TestHandleAuthorize_OmitsEmptyKeyName(t *testing.T) {
	// The default test key has no display name; the prefix is always sent
	// but an empty name header is not, so the strip middleware's cleared
	// value is what the backend sees.
	srv := testServer(t, alwaysFresh())

	resp := get(t, srv.URL+"/authorize?namespace=shop-prod&app=api", testKey)
	if got := resp.Header.Get("X-Kipper-Key-Prefix"); got != "ab12cd34" {
		t.Errorf("X-Kipper-Key-Prefix = %q, want the prefix", got)
	}
	if _, ok := resp.Header["X-Kipper-Key-Name"]; ok {
		t.Error("X-Kipper-Key-Name must be omitted when the key has no display name")
	}
}

func TestHandleAuthorize_DropsUnsafeKeyName(t *testing.T) {
	// A key created by a direct CR write can carry a control byte in its
	// display name that the console API would have rejected. authz must not
	// forward it as a header, or the transport rejects every request on the
	// route.
	fresh := alwaysFresh()
	a := testAuthorizer(t, fresh, testApiKey(func(k *kipperv1.ApiKey) {
		k.Spec.DisplayName = "acme\x07partner"
	}), testPlan())
	mux := http.NewServeMux()
	NewServer(a, fresh).Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp := get(t, srv.URL+"/authorize?namespace=shop-prod&app=api", testKey)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid key with an unsafe name still authorizes: %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Kipper-Key-Prefix"); got != "ab12cd34" {
		t.Errorf("X-Kipper-Key-Prefix = %q, want the prefix (always safe)", got)
	}
	if _, ok := resp.Header["X-Kipper-Key-Name"]; ok {
		t.Error("X-Kipper-Key-Name must be dropped when the display name carries control bytes")
	}
}

func TestForwardedChain_JoinsRepeatedHeaderLines(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "/authorize", nil)
	if err != nil {
		t.Fatal(err)
	}
	// A proxy may append the chain as separate header lines rather than one
	// comma-separated value; the forensics field must keep every hop.
	req.Header.Add("X-Forwarded-For", "203.0.113.7")
	req.Header.Add("X-Forwarded-For", "203.0.113.8, 10.0.0.1")
	if got := forwardedChain(req); got != "203.0.113.7, 203.0.113.8, 10.0.0.1" {
		t.Errorf("forwardedChain = %q, want every hop joined", got)
	}

	long := strings.Repeat("9", maxLoggedForwardedFor+50)
	req2, _ := http.NewRequest(http.MethodGet, "/authorize", nil)
	req2.Header.Set("X-Forwarded-For", long)
	if got := forwardedChain(req2); len(got) != maxLoggedForwardedFor {
		t.Errorf("forwardedChain bound = %d, want %d", len(got), maxLoggedForwardedFor)
	}
}

func TestHandleAuthorize_FailClosedIs503(t *testing.T) {
	stale := NewFreshness(nil, nil, func() bool { return true }, time.Hour, 90*time.Second, nil, "test")
	stale.lastFresh = time.Now().Add(-5 * time.Minute)
	srv := testServer(t, stale)

	resp := get(t, srv.URL+"/authorize?namespace=shop-prod&app=api", testKey)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("stale cache: %d, want 503", resp.StatusCode)
	}
	body := decodeDenial(t, resp)
	if body.Code != "gate_unavailable" {
		t.Errorf("503 code = %q, want gate_unavailable", body.Code)
	}
	if !strings.Contains(body.Message, "kipper-authz") {
		t.Errorf("503 message must name the authz service for diagnosability, got %q", body.Message)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("503 must advise Retry-After so clients back off instead of hammering a syncing gate")
	}
}

func TestHandleAuthorize_DenialContract(t *testing.T) {
	srv := testServer(t, alwaysFresh())

	resp := get(t, srv.URL+"/authorize?namespace=shop-prod&app=api", "kip_bad_key")
	if body := decodeDenial(t, resp); body.Code != "invalid_key" {
		t.Errorf("401 code = %q, want invalid_key", body.Code)
	}
	if resp.Header.Get("Retry-After") != "" {
		t.Error("invalid_key must not advise a retry; the key will not become valid by waiting")
	}

	resp = get(t, srv.URL+"/authorize", testKey)
	if body := decodeDenial(t, resp); body.Code != "misconfigured" {
		t.Errorf("500 code = %q, want misconfigured", body.Code)
	}
}

func TestHandleAuthorize_RateDenialAdvisesRetry(t *testing.T) {
	// A single-token plan makes the second immediate request a rate
	// denial, which must carry the contract for automated backoff.
	fresh := alwaysFresh()
	a := testAuthorizer(t, fresh, testApiKey(), testPlan(func(p *kipperv1.UsagePlan) {
		p.Spec.Rate = 1
		p.Spec.Burst = 1
	}))
	mux := http.NewServeMux()
	NewServer(a, fresh).Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	var resp *http.Response
	for i := 0; i < 5; i++ {
		resp = get(t, srv.URL+"/authorize?namespace=shop-prod&app=api", testKey)
		if resp.StatusCode == http.StatusTooManyRequests {
			break
		}
	}
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatal("could not exhaust a burst-1 token bucket within 5 requests")
	}
	if body := decodeDenial(t, resp); body.Code != "rate_limited" {
		t.Errorf("429 code = %q, want rate_limited", body.Code)
	}
	secs, err := strconv.Atoi(resp.Header.Get("Retry-After"))
	if err != nil || secs < 1 {
		t.Errorf("rate 429 Retry-After = %q, want a positive whole-second value", resp.Header.Get("Retry-After"))
	}
	if got := resp.Header.Get("Access-Control-Expose-Headers"); !strings.Contains(got, "Retry-After") {
		t.Errorf("Retry-After must be CORS-exposed for browser clients, got %q", got)
	}
}

func TestHandleAuthorize_PreflightPassesWithoutKey(t *testing.T) {
	srv := testServer(t, alwaysFresh())

	// Browsers never send X-API-Key on a CORS preflight, so a request
	// shaped like one (OPTIONS + Access-Control-Request-Method) must pass
	// the gate for the app to answer it.
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/authorize?namespace=shop-prod&app=api", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Forwarded-Method", http.MethodOptions)
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Errorf("preflight through the gate = %d, want 200", resp.StatusCode)
	}

	// A bare OPTIONS probe without the preflight header is not a
	// preflight and still needs a key.
	req, err = http.NewRequest(http.MethodGet, srv.URL+"/authorize?namespace=shop-prod&app=api", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Forwarded-Method", http.MethodOptions)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("keyless bare OPTIONS = %d, want 401", resp.StatusCode)
	}
}

func TestClientIP(t *testing.T) {
	cases := []struct {
		name, xff, remote, want string
	}{
		{"leftmost of a chain is the client", "203.0.113.7, 10.0.0.1", "10.1.2.3:5000", "203.0.113.7"},
		{"single forwarded value", "203.0.113.9", "10.1.2.3:5000", "203.0.113.9"},
		{"no header falls back to peer", "", "10.1.2.3:5000", "10.1.2.3"},
		{"unparseable peer passes through", "", "garbage", "garbage"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/authorize", nil)
			r.RemoteAddr = c.remote
			if c.xff != "" {
				r.Header.Set("X-Forwarded-For", c.xff)
			}
			if got := clientIP(r); got != c.want {
				t.Errorf("clientIP = %q, want %q", got, c.want)
			}
		})
	}
}

func TestLogDecision_DenialCarriesForensics(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	srv := testServer(t, alwaysFresh())
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/authorize?namespace=shop-prod&app=api", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-API-Key", "kip_ab12cd34_wrongsecret")
	req.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	var line map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &line); err != nil {
		t.Fatalf("denial must emit one JSON log line, got %q: %v", buf.String(), err)
	}
	if line["level"] != "WARN" {
		t.Errorf("denial level = %v, want WARN", line["level"])
	}
	if line["reason"] != "deny_key" {
		t.Errorf("reason = %v, want deny_key", line["reason"])
	}
	if line["key_prefix"] != "ab12cd34" {
		t.Errorf("key_prefix = %v, want the non-secret prefix", line["key_prefix"])
	}
	if line["client_ip"] != "203.0.113.7" {
		t.Errorf("client_ip = %v, want the leftmost forwarded address", line["client_ip"])
	}
	// The whole chain is kept so the spoofable leftmost is never the only
	// evidence in the log.
	if line["forwarded_for"] != "203.0.113.7, 10.0.0.1" {
		t.Errorf("forwarded_for = %v, want the full chain", line["forwarded_for"])
	}
}

func TestLogDecision_BoundsMalformedPrefix(t *testing.T) {
	cases := []struct {
		name, key string
	}{
		// Not kip_<prefix>_<secret> at all: keyPrefix rejects the shape.
		{"unshaped", strings.Repeat("A", 500)},
		// Well-shaped but the prefix is longer than any issued handle, so the
		// length bound rather than the shape check must catch it.
		{"overlong prefix", "kip_" + strings.Repeat("B", 40) + "_secret"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
			t.Cleanup(func() { slog.SetDefault(prev) })

			srv := testServer(t, alwaysFresh())
			req, err := http.NewRequest(http.MethodGet, srv.URL+"/authorize?namespace=shop-prod&app=api", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("X-API-Key", c.key)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()

			var line map[string]any
			if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &line); err != nil {
				t.Fatalf("want a JSON log line, got %q: %v", buf.String(), err)
			}
			if line["key_prefix"] != "invalid" {
				t.Errorf("key_prefix = %v, want invalid for a malformed key", line["key_prefix"])
			}
		})
	}
}

func TestHandleReady_TracksFreshness(t *testing.T) {
	fresh := alwaysFresh()
	srv := testServer(t, fresh)

	if resp := get(t, srv.URL+"/readyz", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("fresh replica: readyz = %d, want 200", resp.StatusCode)
	}

	fresh.mu.Lock()
	fresh.lastFresh = time.Now().Add(-2 * time.Hour)
	fresh.mu.Unlock()

	if resp := get(t, srv.URL+"/readyz", ""); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("stale replica: readyz = %d, want 503 so Traefik stops routing here", resp.StatusCode)
	}
}
