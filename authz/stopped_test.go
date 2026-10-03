package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func getStopped(t *testing.T, url, host, accept string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url+"/stopped", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	req.Header.Set("Authorization", "Basic c2VjcmV0OnNlY3JldA==")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(body)
}

func TestStoppedPageForABrowser(t *testing.T) {
	srv := testServer(t, alwaysFresh())

	resp, body := getStopped(t, srv.URL, "shop.example.com", "text/html,application/xhtml+xml,*/*;q=0.8")

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", got)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if !strings.Contains(body, "This app is stopped") || !strings.Contains(body, "shop.example.com") {
		t.Errorf("the page does not say the app at this host is stopped: %s", body)
	}
	if strings.Contains(body, "c2VjcmV0") {
		t.Error("the page repeats the visitor's credentials")
	}
}

func TestStoppedPageForAnAPIClient(t *testing.T) {
	srv := testServer(t, alwaysFresh())

	resp, body := getStopped(t, srv.URL, "api.example.com", "application/json")

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	var got denialBody
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("body is not JSON: %s", body)
	}
	if got.Code != "app_stopped" || got.Message != "This app is stopped." {
		t.Errorf("body = %+v", got)
	}
}

func TestStoppedPageEscapesTheHost(t *testing.T) {
	srv := testServer(t, alwaysFresh())

	_, body := getStopped(t, srv.URL, "<script>x</script>.example.com", "")

	if strings.Contains(body, "<script>x") {
		t.Errorf("the host reached the page unescaped: %s", body)
	}
}
