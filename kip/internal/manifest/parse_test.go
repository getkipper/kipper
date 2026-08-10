package manifest

import (
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
