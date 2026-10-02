package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"testing"
	"time"
)

func TestShortHash(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"myapp-7f8b9c-x4k2z", "f1582f7c"},
		{"myapp-7f8b9c-a1b2c", "71987b16"},
		{"", "e3b0c442"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := shortHash(tt.input)
			if got != tt.want {
				t.Errorf("shortHash(%q) = %q, want %q", tt.input, got, tt.want)
			}
			if len(got) != 8 {
				t.Errorf("shortHash(%q) length = %d, want 8", tt.input, len(got))
			}
		})
	}
}

func TestShortHashDeterministic(t *testing.T) {
	a := shortHash("test-pod-abc123")
	b := shortHash("test-pod-abc123")
	if a != b {
		t.Errorf("shortHash is not deterministic: %q != %q", a, b)
	}
}

func TestShortHashUnique(t *testing.T) {
	a := shortHash("pod-a")
	b := shortHash("pod-b")
	if a == b {
		t.Errorf("shortHash collision for different inputs: %q", a)
	}
}

func TestProxyAddsInstanceHeader(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Custom", "preserved")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	target, _ := url.Parse(backend.URL)
	proxy := httputil.NewSingleHostReverseProxy(target)

	instanceID := shortHash("test-pod-xyz")
	proxy.ModifyResponse = func(resp *http.Response) error {
		resp.Header.Set("X-Instance-ID", instanceID)
		return nil
	}

	frontend := httptest.NewServer(proxy)
	defer frontend.Close()

	resp, err := http.Get(frontend.URL)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	got := resp.Header.Get("X-Instance-ID")
	if got != instanceID {
		t.Errorf("X-Instance-ID = %q, want %q", got, instanceID)
	}

	if resp.Header.Get("X-Custom") != "preserved" {
		t.Error("backend headers were not preserved")
	}
}

func TestAVanishedClientIsNotAGatewayError(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil).WithContext(cancelled)
	clientAwareErrorHandler(rec, req, context.Canceled)

	if rec.Code != http.StatusOK {
		t.Errorf("wrote status %d for a client that had already gone; want nothing written", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("wrote a body to a client that had already gone: %q", rec.Body.String())
	}
}

// An upstream that genuinely failed still answers 502, because that is a
// gateway error and somebody should see it.
func TestAFailedUpstreamIsStillAGatewayError(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	clientAwareErrorHandler(rec, req, errors.New("dial tcp [::1]:3000: connect: connection refused"))

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 for a real upstream failure", rec.Code)
	}
}

// Through the proxy main actually builds, not a second one assembled by the
// test: an upstream that is not there answers 502, and a client that has
// already gone gets nothing.
func TestTheProxyMainBuildsHandlesBothFailures(t *testing.T) {
	// Nothing listening on this port, so every attempt fails to dial.
	dead, err := url.Parse("http://127.0.0.1:1")
	if err != nil {
		t.Fatalf("parsing the upstream: %v", err)
	}
	proxy := newProxy(dead, "testid")

	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("a dead upstream gave %d, want 502", rec.Code)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	rec = httptest.NewRecorder()
	proxy.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil).WithContext(cancelled))
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Errorf("a vanished client got status %d body %q; want nothing written",
			rec.Code, rec.Body.String())
	}
}

func TestServeDrainsInFlightRequestsOnShutdown(t *testing.T) {
	arrived := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(arrived)
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte("done"))
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- serve(ctx, ln, newProxy(target, "abcd1234")) }()

	type result struct {
		code int
		body string
		err  error
	}
	got := make(chan result, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/slow")
		if err != nil {
			got <- result{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		got <- result{code: resp.StatusCode, body: string(b)}
	}()

	<-arrived
	cancel()

	r := <-got
	if r.err != nil || r.code != http.StatusOK || r.body != "done" {
		t.Fatalf("in-flight request = %d %q %v, want 200 done", r.code, r.body, r.err)
	}
	if err := <-served; err != nil {
		t.Fatalf("serve returned %v after a clean shutdown", err)
	}
	if conn, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second); err == nil {
		_ = conn.Close()
		t.Error("the listener still accepts connections after shutdown")
	}
}
