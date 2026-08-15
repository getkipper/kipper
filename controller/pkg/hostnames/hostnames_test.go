package hostnames

import (
	"strings"
	"testing"
)

func TestSubdomainFor(t *testing.T) {
	cases := []struct {
		prefix, domain, want string
	}{
		// Free *.kipper.run: single label, double-dash separator.
		{"console", "acme.kipper.run", "console--acme.kipper.run"},
		{"console-api", "203-0-113-20.kipper.run", "console-api--203-0-113-20.kipper.run"},
		{"dex", "acme.kipper.run", "dex--acme.kipper.run"},
		// Custom domain: dot subdomain.
		{"console", "example.com", "console.example.com"},
		{"console-api", "storefront.com", "console-api.storefront.com"},
		{"dex", "example.com", "dex.example.com"},
	}
	for _, c := range cases {
		if got := SubdomainFor(c.prefix, c.domain); got != c.want {
			t.Errorf("SubdomainFor(%q, %q) = %q, want %q", c.prefix, c.domain, got, c.want)
		}
	}
}

func TestSubdomainForUsesTheSharedSeparator(t *testing.T) {
	// A derived kipper.run host must always carry the reserved separator so it
	// can never equal a registrable cluster label.
	got := SubdomainFor("console", "acme.kipper.run")
	if want := "console" + DerivedRouteSeparator + "acme.kipper.run"; got != want {
		t.Fatalf("derived host = %q, want %q (separator drift)", got, want)
	}
	if DerivedRouteSeparator != "--" {
		t.Fatalf("DerivedRouteSeparator = %q, want %q", DerivedRouteSeparator, "--")
	}
}

func TestHostFor(t *testing.T) {
	if got := HostFor("console", "console.custom.example", "acme.kipper.run"); got != "console.custom.example" {
		t.Errorf("override must win, got %q", got)
	}
	if got := HostFor("console", "", "acme.kipper.run"); got != "console--acme.kipper.run" {
		t.Errorf("empty override falls back to convention, got %q", got)
	}
}

func TestUIDomainFor(t *testing.T) {
	cases := map[string]string{
		"example.com":             "example.com",
		"storefront.com":          "storefront.com",
		"kipper.run":              "",
		"acme.kipper.run":         "",
		"203-0-113-20.kipper.run": "",
	}
	for in, want := range cases {
		if got := UIDomainFor(in); got != want {
			t.Errorf("UIDomainFor(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidateClusterLabel(t *testing.T) {
	valid := []string{"acme", "203-0-113-20", "a", "my-cluster", strings.Repeat("a", 63)}
	for _, l := range valid {
		if err := ValidateClusterLabel(l); err != nil {
			t.Errorf("ValidateClusterLabel(%q) = %v, want nil", l, err)
		}
	}

	invalid := map[string]string{
		"":                      "empty",
		"UPPER":                 "uppercase",
		"-lead":                 "leading hyphen",
		"trail-":                "trailing hyphen",
		"a--b":                  "contains separator",
		"console--acme":         "contains separator",
		"has.dot":               "not a single label",
		strings.Repeat("a", 64): "too long",
		"console":               "reserved",
		"dex":                   "reserved",
		"console-api":           "reserved",
		"admin":                 "reserved",
	}
	for l, why := range invalid {
		if err := ValidateClusterLabel(l); err == nil {
			t.Errorf("ValidateClusterLabel(%q) = nil, want error (%s)", l, why)
		}
	}
}

// The reserved list is what stops an operator registering a label that reads as
// the platform's own. Its job is the same for every entry, so a spot check of
// the categories is enough; the exhaustive comparison that matters is the
// lockstep test against the CRD's copy of this list.
func TestReservedLabelsCoverPlatformNames(t *testing.T) {
	for _, l := range []string{
		"console", "dex", "api", "admin",
		"login", "logout", "auth", "sso", "oauth", "oidc",
		"account", "accounts", "signup",
		"status", "gateway", "docs", "support", "help", "security", "billing", "blog",
		"mail", "smtp", "mx", "ns1", "ns2", "webmail",
	} {
		if !ReservedLabels[l] {
			t.Errorf("ReservedLabels[%q] = false, want true", l)
		}
		if err := ValidateClusterLabel(l); err == nil {
			t.Errorf("ValidateClusterLabel(%q) = nil, want it refused as reserved", l)
		}
	}

	// Reserving costs an operator a name, so names that read as somebody's own
	// cluster have to stay registrable.
	for _, l := range []string{"acme", "prod", "staging", "lab", "test", "demo", "dev"} {
		if ReservedLabels[l] {
			t.Errorf("ReservedLabels[%q] = true, want it left registrable", l)
		}
	}
}

func TestLabelForIP(t *testing.T) {
	cases := map[string]string{
		"203.0.113.10": "203-0-113-10",
		"198.51.100.1": "198-51-100-1",
	}
	for ip, want := range cases {
		if got := LabelForIP(ip); got != want {
			t.Errorf("LabelForIP(%q) = %q, want %q", ip, got, want)
		}
		// The default label every free-tier install has always used must remain
		// registrable, or the derivation and the rule have drifted apart.
		if err := ValidateClusterLabel(want); err != nil {
			t.Errorf("ValidateClusterLabel(LabelForIP(%q)) = %v, want nil", ip, err)
		}
	}
}

func TestIPShapedLabel(t *testing.T) {
	shaped := []string{"203-0-113-10", "1-2-3-4", "255-255-255-255"}
	for _, l := range shaped {
		if !IPShapedLabel(l) {
			t.Errorf("IPShapedLabel(%q) = false, want true", l)
		}
	}

	// Only a label that could be somebody's address is claimed by the guard.
	// Anything else is an ordinary name and must stay freely registrable.
	plain := []string{"acme", "lab", "203-0-113", "203-0-113-10-1", "999-0-0-1", "a-1-2-3", "203-0-113-x"}
	for _, l := range plain {
		if IPShapedLabel(l) {
			t.Errorf("IPShapedLabel(%q) = true, want false", l)
		}
	}
}

// The guard the gateway applies is only sound if every address it accepts
// renders to a label the guard then recognises. Drift either way opens a hole:
// a shape LabelForIP produces but IPShapedLabel misses is a squattable default
// name.
func TestLabelForIPIsAlwaysIPShaped(t *testing.T) {
	for _, ip := range []string{"203.0.113.10", "1.2.3.4", "255.255.255.255", "198.51.100.1"} {
		if !IPShapedLabel(LabelForIP(ip)) {
			t.Errorf("IPShapedLabel(LabelForIP(%q)) = false, want true", ip)
		}
	}
}
