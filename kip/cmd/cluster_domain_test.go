package cmd

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/getkipper/kipper/controller/pkg/serving"
	"github.com/getkipper/kipper/kip/internal/clusteridentity"
)

func TestNeedsSSOAck(t *testing.T) {
	blocked := &clusteridentity.ClusterIdentity{Status: clusteridentity.Status{Conditions: []clusteridentity.Condition{
		{Type: clusteridentity.ConditionExternalCallbacks, Status: "False", Reason: clusteridentity.ReasonNeedsAck},
	}}}
	if !needsSSOAck(blocked) {
		t.Fatal("a False/NeedsAck ExternalCallbacks condition must block the cutover")
	}

	acked := &clusteridentity.ClusterIdentity{Status: clusteridentity.Status{Conditions: []clusteridentity.Condition{
		{Type: clusteridentity.ConditionExternalCallbacks, Status: "True", Reason: "Acknowledged"},
	}}}
	if needsSSOAck(acked) {
		t.Fatal("an acknowledged/absent-connectors cluster must not block")
	}

	if needsSSOAck(&clusteridentity.ClusterIdentity{}) {
		t.Fatal("no ExternalCallbacks condition means no block")
	}
}

func TestOverridesOf(t *testing.T) {
	if o := overridesOf(nil); o != (serving.Overrides{}) {
		t.Fatalf("nil hosts should give empty overrides, got %+v", o)
	}
	o := overridesOf(&clusteridentity.Hosts{Console: "console.example.com", ConsoleAPI: "api.example.com", Dex: "dex.example.com"})
	if o.Console != "console.example.com" || o.ConsoleAPI != "api.example.com" || o.Dex != "dex.example.com" {
		t.Fatalf("overrides not mapped: %+v", o)
	}
}

func TestTargetMatches(t *testing.T) {
	want := serving.ResolveHosts("acme.example.com", serving.Overrides{})

	good := &clusteridentity.ResolvedHosts{
		Console: want.Console, ConsoleAPI: want.ConsoleAPI, Dex: want.Dex, Issuer: want.Issuer(),
	}
	if !targetMatches(want, good) {
		t.Fatal("the requested target must match the transition destination")
	}
	if targetMatches(want, nil) {
		t.Fatal("a nil destination must not match")
	}
	// A concurrent edit that changed the destination must be rejected.
	other := serving.ResolveHosts("other.example.com", serving.Overrides{})
	bad := &clusteridentity.ResolvedHosts{
		Console: other.Console, ConsoleAPI: other.ConsoleAPI, Dex: other.Dex, Issuer: other.Issuer(),
	}
	if targetMatches(want, bad) {
		t.Fatal("a different destination must not match")
	}
}

func TestProbeURLAcceptsHealthyTLS(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// srv.Client() trusts the server's self-signed cert, standing in for a valid
	// public chain.
	if err := probeURL(srv.URL, srv.Client()); err != nil {
		t.Fatalf("healthy TLS endpoint should pass: %v", err)
	}
}

func TestProbeURLRejectsServerError(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	if err := probeURL(srv.URL, srv.Client()); err == nil {
		t.Fatal("a 502 must fail the probe")
	}
}

func TestProbeURLRejectsUntrustedChain(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// The default client does not trust the test server's self-signed cert, which
	// is exactly the "certificate not valid" case the gate must reject.
	if err := probeURL(srv.URL, http.DefaultClient); err == nil {
		t.Fatal("an untrusted TLS chain must fail the probe")
	}
}
