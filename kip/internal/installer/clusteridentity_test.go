package installer

import (
	"strings"
	"testing"
)

func TestClusterIdentityManifestKipperRun(t *testing.T) {
	// A *.kipper.run install: no host overrides (derived by convention), with a
	// gateway registration.
	m := ClusterIdentityManifest("203-0-113-20.kipper.run", "", "", "", "203-0-113-20.kipper.run", "203.0.113.20")
	for _, want := range []string{
		"kind: ClusterIdentity",
		"name: cluster",
		"domain: 203-0-113-20.kipper.run",
		"kipperRunDomain: 203-0-113-20.kipper.run",
		// The gateway heartbeat needs the public host, and the reconciler can only
		// keep rendering CLUSTER_HOST if the CR carries it.
		"clusterHost: 203.0.113.20",
		"register: true",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("manifest missing %q in:\n%s", want, m)
		}
	}
	if strings.Contains(m, "hosts:") {
		t.Errorf("no overrides means no hosts block:\n%s", m)
	}
}

func TestClusterIdentityManifestCustomDomainNoGateway(t *testing.T) {
	// A custom-domain install: no gateway registration.
	m := ClusterIdentityManifest("example.com", "", "", "", "", "")
	if strings.Contains(m, "gateway:") {
		t.Errorf("custom domain must not register a gateway subdomain:\n%s", m)
	}
	if !strings.Contains(m, "domain: example.com") {
		t.Errorf("manifest missing domain:\n%s", m)
	}
}

func TestClusterIdentityManifestPartialOverrides(t *testing.T) {
	// Only a console override: the hosts block carries just that key.
	m := ClusterIdentityManifest("example.com", "admin.example.com", "", "", "", "")
	if !strings.Contains(m, "hosts:") || !strings.Contains(m, "console: admin.example.com") {
		t.Errorf("expected a console override in the hosts block:\n%s", m)
	}
	if strings.Contains(m, "consoleAPI:") || strings.Contains(m, "dex:") {
		t.Errorf("unset overrides must be omitted:\n%s", m)
	}
}

func TestValidateHostRejectsInjection(t *testing.T) {
	// Values that could break the YAML manifest or the kubectl heredoc must be
	// rejected before they reach either.
	bad := []string{
		"foo\nextra: injected",
		"foo bar",
		"KIPEOF",
		"a: b",
		"foo\"; rm -rf /",
	}
	for _, v := range bad {
		if err := validateHost("domain", v, true); err == nil {
			t.Errorf("validateHost accepted an unsafe value %q", v)
		}
	}
	// Valid hostnames and empty optional overrides pass.
	if err := validateHost("domain", "console.example.com", true); err != nil {
		t.Errorf("a valid hostname was rejected: %v", err)
	}
	if err := validateHost("console", "", false); err != nil {
		t.Errorf("an empty optional override should be allowed: %v", err)
	}
	if err := validateHost("domain", "", true); err == nil {
		t.Error("a required empty domain must be rejected")
	}
}

func TestValidateClusterHost(t *testing.T) {
	// clusterHost is what the gateway registers: a routable public address. A
	// name, or an address the gateway refuses, would install cleanly and then
	// fail every heartbeat.
	for _, bad := range []string{
		"cluster.example.com", "203.0.113.7:443", "not an ip", "203.0.113",
		"10.0.0.5", "127.0.0.1", "169.254.169.254", "100.64.0.1",
	} {
		if err := validateClusterHost(bad); err == nil {
			t.Errorf("validateClusterHost accepted %q", bad)
		}
	}
	for _, ok := range []string{"203.0.113.7", "2001:db8::1", ""} {
		if err := validateClusterHost(ok); err != nil {
			t.Errorf("validateClusterHost rejected %q: %v", ok, err)
		}
	}
}

// The install shape that matters here is the whole argument list: a custom-domain
// cluster reached through an SSH hostname registers nothing, so its host must not
// be held to the gateway's address rules and fail the install at the last step.
func TestValidateIdentityInputsByInstallShape(t *testing.T) {
	cases := []struct {
		name                          string
		domain, kipperRunDomain, host string
		wantErr                       bool
	}{
		{"custom domain reached by hostname", "example.com", "", "v2202503260491323449.example.net", false},
		{"custom domain reached by address", "example.com", "", "203.0.113.7", false},
		{"kipper.run registration by address", "acme.kipper.run", "acme.kipper.run", "203.0.113.7", false},
		{"kipper.run registration by hostname", "acme.kipper.run", "acme.kipper.run", "host.example.com", true},
		{"kipper.run registration on a private address", "acme.kipper.run", "acme.kipper.run", "10.0.0.5", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateIdentityInputs(tc.domain, "", "", "", tc.kipperRunDomain, tc.host)
			if tc.wantErr && err == nil {
				t.Error("expected an error")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}
