package main

import (
	"strings"
	"testing"

	"github.com/getkipper/kipper/controller/pkg/hostnames"
)

// The gateway's route-shadowing defence (rejecting "--" in a registered label,
// splitting derived routes on the last "--") only holds if the gateway and the
// host renderer agree on the separator. They must be the same symbol, not two
// literals that could drift — that drift is what broke reconfiguration.
func TestGatewaySeparatorIsTheSharedOne(t *testing.T) {
	if derivedRouteSeparator != hostnames.DerivedRouteSeparator {
		t.Fatalf("gateway separator %q != shared %q", derivedRouteSeparator, hostnames.DerivedRouteSeparator)
	}

	// A derived host the renderer produces must carry exactly the separator the
	// gateway splits on and forbids in registrations.
	host := hostnames.SubdomainFor("console", "acme.kipper.run")
	if !strings.Contains(host, derivedRouteSeparator) {
		t.Fatalf("rendered host %q does not contain the gateway separator %q", host, derivedRouteSeparator)
	}
	if idx := strings.LastIndex(host, derivedRouteSeparator); idx <= 0 || host[:idx] != "console" {
		t.Fatalf("gateway last-%q split of %q does not recover the service prefix", derivedRouteSeparator, host)
	}
}
