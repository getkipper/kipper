package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func askCheck(t *testing.T, upstream string, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	passed := false
	h := withUpstreamCheck(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		passed = true
		w.WriteHeader(http.StatusTeapot)
	}), upstream)
	req := httptest.NewRequest(upstreamCheckMethod, "/", nil)
	for k, v := range header {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if passed != (rec.Code == http.StatusTeapot) {
		t.Fatal("the recorder and the pass-through disagree")
	}
	return rec
}

func TestUpstreamCheckReportsAnAcceptingUpstream(t *testing.T) {
	app, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = app.Close() }()

	rec := askCheck(t, app.Addr().String(), nil)

	if got := rec.Header().Get(upstreamHeader); got != "accepting" {
		t.Errorf("%s = %q, want accepting", upstreamHeader, got)
	}
}

func TestUpstreamCheckReportsARefusingUpstream(t *testing.T) {
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := closed.Addr().String()
	_ = closed.Close()

	rec := askCheck(t, addr, nil)

	if got := rec.Header().Get(upstreamHeader); got != "refused" {
		t.Errorf("%s = %q, want refused", upstreamHeader, got)
	}
}

func TestUpstreamCheckPassesForwardedRequestsThrough(t *testing.T) {
	rec := askCheck(t, "127.0.0.1:1", http.Header{"X-Forwarded-For": {"203.0.113.7"}})

	if rec.Code != http.StatusTeapot || rec.Header().Get(upstreamHeader) != "" {
		t.Errorf("a forwarded request must reach the app untouched, got %d %q", rec.Code, rec.Header().Get(upstreamHeader))
	}
}

func TestUpstreamCheckPassesOtherMethodsThrough(t *testing.T) {
	h := withUpstreamCheck(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}), "127.0.0.1:1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusTeapot {
		t.Errorf("a GET must reach the app, got %d", rec.Code)
	}
}
