package installer

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCertManagerDNSPatch(t *testing.T) {
	// Operator-chosen resolvers flow into the pod's dnsConfig verbatim, so
	// cert-manager's self-check uses the same DNS policy as the cluster.
	patch, err := certManagerDNSPatch([]string{"10.0.0.53", "10.0.0.54"})
	assert.NoError(t, err)
	assert.Contains(t, patch, `"nameservers":["10.0.0.53","10.0.0.54"]`)
	assert.Contains(t, patch, `"dnsPolicy":"None"`)
	assert.Contains(t, patch, `{"name":"ndots","value":"5"}`)

	// An empty list falls back to the public defaults rather than writing
	// an empty nameserver set that would leave the pod with no resolver.
	patch, err = certManagerDNSPatch(nil)
	assert.NoError(t, err)
	assert.Contains(t, patch, `"nameservers":["1.1.1.1","8.8.8.8","9.9.9.9"]`)

	// A hand-corrupted persisted config reaching the upgrade path must not
	// render a broken dnsConfig — the same validation as the k3s side.
	for _, bad := range [][]string{
		{"2606:4700:4700::1111"},                     // IPv6 on an IPv4-only pod network
		{"dns.example.com"},                          // hostname
		{"1.1.1.1", "8.8.8.8", "9.9.9.9", "8.8.4.4"}, // over the nameserver limit
	} {
		_, err := certManagerDNSPatch(bad)
		assert.Error(t, err, "expected %v to be rejected", bad)
	}
}
