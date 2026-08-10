package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/getkipper/kipper/controller/pkg/hostnames"
	"github.com/getkipper/kipper/gateway/internal/registry"
)

func postRegister(t *testing.T, handler http.HandlerFunc, body string) (int, registerResponse) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	var resp registerResponse
	_ = json.NewDecoder(w.Body).Decode(&resp)
	return w.Code, resp
}

// An anonymous request naming an existing registration must not echo the
// management token. The label and the address are both readable from public
// DNS, so anyone could otherwise ask for the token and then deregister or
// repoint the cluster. Such a request proves nothing: it refreshes no
// inactivity clock and moves no address.
func TestAnonymousRegisterDoesNotDiscloseToken(t *testing.T) {
	handler := handleRegister(registry.New(), "kipper.run", neverObserve)

	code, first := postRegister(t, handler, `{"subdomain":"myapp","ip":"198.51.100.1"}`)
	if code != http.StatusCreated || first.Token == "" {
		t.Fatalf("new registration should return 201 with a token, got %d token=%q", code, first.Token)
	}

	code, second := postRegister(t, handler, `{"subdomain":"myapp","ip":"198.51.100.1"}`)
	if code != http.StatusCreated {
		t.Fatalf("an anonymous request for an existing registration should return 201, got %d", code)
	}
	if second.Token != "" {
		t.Errorf("an anonymous request must not disclose the token, got %q", second.Token)
	}
}

func TestRegisterRejectsNonPublicIP(t *testing.T) {
	handler := handleRegister(registry.New(), "kipper.run", neverObserve)
	for _, ip := range []string{
		"127.0.0.1", "::1", // loopback
		"169.254.169.254", "fe80::1", // link-local incl. cloud metadata
		"10.0.0.1", "192.168.1.5", "172.16.0.1", // private
		"0.0.0.0",       // unspecified
		"224.0.0.1",     // multicast
		"not-an-ip", "", // malformed
	} {
		body := fmt.Sprintf(`{"subdomain":"app","ip":%q}`, ip)
		code, _ := postRegister(t, handler, body)
		if code != http.StatusBadRequest {
			t.Errorf("ip %q: expected 400, got %d", ip, code)
		}
	}
}

func TestRegisterRejectsReservedLabels(t *testing.T) {
	handler := handleRegister(registry.New(), "kipper.run", neverObserve)
	// Iterate the shared reserved set so every reserved label the CRD forbids is
	// also proven rejected at registration, and the two can't drift.
	for sub := range hostnames.ReservedLabels {
		body := fmt.Sprintf(`{"subdomain":%q,"ip":"198.51.100.1"}`, sub)
		code, _ := postRegister(t, handler, body)
		if code != http.StatusConflict {
			t.Errorf("reserved subdomain %q: expected 409, got %d", sub, code)
		}
	}
}

// Derived routes (console--<cluster>, dex--<cluster>, <app>--<cluster>) join
// prefix and cluster with a double dash, so no registered label may contain
// "--". Otherwise an attacker could register the exact derived hostname of a
// victim's console, Dex, or app URL — in either registration order.
func TestRegisterRejectsDoubleDashLabels(t *testing.T) {
	for _, sub := range []string{
		"console--203-0-113-12", "dex--203-0-113-12", "myapp--victim",
		"a--b--c", "xn--punycode",
	} {
		handler := handleRegister(registry.New(), "kipper.run", neverObserve)
		body := fmt.Sprintf(`{"subdomain":%q,"ip":"198.51.100.9"}`, sub)
		code, _ := postRegister(t, handler, body)
		if code != http.StatusBadRequest {
			t.Errorf("double-dash subdomain %q: expected 400, got %d", sub, code)
		}
	}
}

// Single-dash labels that merely look like the old service prefixes are plain
// cluster names under the double-dash scheme and must register normally.
func TestRegisterAllowsSingleDashServiceLookalikes(t *testing.T) {
	handler := handleRegister(registry.New(), "kipper.run", neverObserve)
	for i, sub := range []string{"console-tools", "dex-lab", "myapp-staging"} {
		body := fmt.Sprintf(`{"subdomain":%q,"ip":"198.51.100.%d"}`, sub, 20+i)
		code, resp := postRegister(t, handler, body)
		if code != http.StatusCreated || resp.Token == "" {
			t.Errorf("single-dash subdomain %q: expected 201 with token, got %d token=%q", sub, code, resp.Token)
		}
	}
}

// A legitimate hyphenated base subdomain must register even when a shorter suffix
// component is already taken by another IP. The previous suffix-matching check
// wrongly rejected these.
func TestRegisterAllowsHyphenatedBaseSubdomain(t *testing.T) {
	handler := handleRegister(registry.New(), "kipper.run", neverObserve)

	code, _ := postRegister(t, handler, `{"subdomain":"12","ip":"203.0.113.99"}`)
	if code != http.StatusCreated {
		t.Fatalf("registering short name failed: %d", code)
	}
	code, resp := postRegister(t, handler, `{"subdomain":"203-0-113-12","ip":"203.0.113.12"}`)
	if code != http.StatusCreated || resp.Token == "" {
		t.Errorf("legitimate base subdomain wrongly rejected: got %d token=%q", code, resp.Token)
	}
}

func TestIsPublicIP(t *testing.T) {
	for _, ip := range []string{"198.51.100.1", "203.0.113.5", "8.8.8.8", "2606:4700:4700::1111"} {
		if !isPublicIP(ip) {
			t.Errorf("%s should be treated as public", ip)
		}
	}
	for _, ip := range []string{
		"127.0.0.1", "::1", "10.0.0.1", "192.168.0.1", "172.16.5.5",
		"169.254.169.254", "fe80::1", "0.0.0.0", "224.0.0.1", "", "garbage",
		"100.64.0.1", "100.127.255.255", // CGNAT
		"198.18.0.1",      // benchmarking
		"::ffff:10.0.0.1", // IPv4-mapped private
		"0.1.2.3",         // this-network, can alias local
		"::127.0.0.1",     // IPv4-compatible IPv6 (deprecated)
		"64:ff9b::7f00:1", // NAT64 wrapping loopback
	} {
		if isPublicIP(ip) {
			t.Errorf("%s should not be treated as public", ip)
		}
	}
}
