package netguard

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestIsPublicIP(t *testing.T) {
	public := []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"}
	for _, ip := range public {
		if !IsPublicIP(ip) {
			t.Errorf("expected %s public", ip)
		}
	}
	nonPublic := []string{
		"127.0.0.1", "10.0.0.1", "10.43.0.1", "172.16.0.1", "192.168.1.1",
		"169.254.169.254",  // cloud metadata
		"100.64.0.1",       // CGNAT
		"192.0.0.1",        // IETF protocol
		"192.0.2.5",        // TEST-NET-1
		"198.18.0.1",       // benchmarking
		"198.51.100.9",     // TEST-NET-2
		"203.0.113.7",      // TEST-NET-3 (documentation — not routable)
		"240.0.0.1",        // reserved
		"255.255.255.255",  // broadcast
		"64:ff9b:1::a00:1", // local-use NAT64 smuggling a private v4
		"2001:db8::1",      // IPv6 documentation
		"0.0.0.0", "::1", "not-an-ip", "",
	}
	for _, ip := range nonPublic {
		if IsPublicIP(ip) {
			t.Errorf("expected %s non-public", ip)
		}
	}
}

func TestClientRefusesRedirects(t *testing.T) {
	// A credential-bearing request must not be bounced to another host by a
	// 3xx. The client's redirect policy returns the redirect response instead
	// of following it. (Cannot exercise this end-to-end because the dial guard
	// refuses the loopback address httptest binds — which is itself covered by
	// TestClientRefusesNonPublicTarget.)
	c := Client(2 * time.Second)
	if c.CheckRedirect == nil {
		t.Fatal("the client must set a redirect policy")
	}
	if err := c.CheckRedirect(nil, nil); err != http.ErrUseLastResponse {
		t.Errorf("redirect policy must refuse to follow, got %v", err)
	}
}

func TestClientRefusesNonPublicTarget(t *testing.T) {
	// httptest binds loopback; the guard must refuse to connect to it.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	if _, err := Client(2 * time.Second).Get(ts.URL); err == nil {
		t.Error("expected a connection to a loopback address to be refused")
	}
}
