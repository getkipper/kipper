package manifest

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The CLI's --redirect-from and kipper.yaml ask the same question through the
// same function, so a host one accepts the other cannot refuse. A second
// spelling of this rule is exactly the drift that produced the wave 0 defect in
// the env-templating work.
func TestValidateRedirectFromHosts(t *testing.T) {
	for _, tc := range []struct {
		name  string
		hosts []string
		ok    bool
	}{
		{"ordinary hostnames", []string{"www.example.com", "old-brand.example"}, true},
		{"none at all", nil, true},
		{"a single label is not a hostname", []string{"localhost"}, false},
		{"uppercase is not a DNS name", []string{"WWW.example.com"}, false},
		{"kipper.run cannot serve redirects", []string{"shop.kipper.run"}, false},
		{"nor the apex", []string{"kipper.run"}, false},
		{"ten is the cap", []string{"a.example", "b.example", "c.example", "d.example", "e.example",
			"f.example", "g.example", "h.example", "i.example", "j.example"}, true},
		{"eleven is over it", []string{"a.example", "b.example", "c.example", "d.example", "e.example",
			"f.example", "g.example", "h.example", "i.example", "j.example", "k.example"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateRedirectFromHosts(tc.hosts)
			if tc.ok {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestValidate_HealthCheck(t *testing.T) {
	base := func(h *HealthSpec) *Manifest {
		return &Manifest{Project: "acme", Apps: map[string]AppSpec{"api": {Image: "nginx:1.27", Port: 8080, Health: h}}}
	}
	if err := Validate(base(&HealthSpec{Type: "http", Path: "/ready", StartupTimeoutSeconds: 600})); err != nil {
		t.Fatalf("a valid check is refused: %v", err)
	}
	if err := Validate(base(nil)); err != nil {
		t.Fatalf("no check is automatic, not an error: %v", err)
	}
	for _, tc := range []struct {
		health *HealthSpec
		want   string
	}{
		{&HealthSpec{Type: "http"}, "needs a path"},
		{&HealthSpec{Type: "tcp", Path: "/ready"}, "only an HTTP check supports a path"},
		{&HealthSpec{Type: "none", Port: 8081}, "accepts no port"},
		{&HealthSpec{Type: "tcp", Port: 18080}, "instance proxy"},
		{&HealthSpec{Type: "auto"}, "leave the health block out"},
		{&HealthSpec{Type: "grpc"}, "http, tcp or none"},
	} {
		err := Validate(base(tc.health))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Validate(%+v) = %v, want an error mentioning %q", *tc.health, err, tc.want)
		}
	}
}
