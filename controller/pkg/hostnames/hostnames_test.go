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
