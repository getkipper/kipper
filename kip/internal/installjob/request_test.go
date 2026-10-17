package installjob

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/kip/internal/config"
)

func TestRequestRoundTripsThroughJSON(t *testing.T) {
	req := Request{
		Version: RequestVersion, RunID: "run-20261013-1", KipVersion: "0.29.0",
		Host: "203.0.113.10", Domain: "shop.example", AdminEmail: "ops@shop.example",
		OrgDisplayName: "Acme Inc", Harden: true, Firewall: true, AdoptFirewall: true, AdoptFingerprint: "0123456789abcdef", NoSSHRateLimit: true,
		DNSResolvers: []string{"192.0.2.53"}, TrustedProxies: []string{"198.51.100.0/24"}, Profile: "small",
		Backup: &Backup{Bucket: "b", Region: "eu-west-1", AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "secret"},
		Entry:  &config.Cluster{Name: "shop", Host: "203.0.113.10", GatewayToken: "tok"},
	}
	data, err := json.Marshal(req)
	require.NoError(t, err)

	got, err := ParseRequest(data)
	require.NoError(t, err)
	assert.Equal(t, req, got)
}

func TestRequestOptions(t *testing.T) {
	req := Request{Version: RequestVersion, RunID: "r1", Host: "203.0.113.10", Domain: "shop.example",
		Harden: true, Firewall: true, AdoptFirewall: true, AdoptFingerprint: "0123456789abcdef", NoSSHRateLimit: true, Profile: "nano",
		Backup: &Backup{Bucket: "b", Region: "auto", Endpoint: "https://s3.example.com", AccessKeyID: "k", SecretAccessKey: "s"}}

	opts := req.Options()

	assert.Equal(t, "nano", opts.Profile, "the profile chosen on the driver reaches the server's install")
	assert.True(t, opts.Local, "the server runs the install on itself")
	assert.Equal(t, "203.0.113.10", opts.Host)
	assert.True(t, opts.NoSSHRateLimit)
	assert.True(t, opts.AdoptFirewall)
	assert.Equal(t, "0123456789abcdef", opts.AdoptFingerprint, "the yes given on the driver stays tied to what it showed")
	require.NotNil(t, opts.BackupStorage)
	assert.Equal(t, "s", opts.BackupStorage.SecretAccessKey)
}

func TestParseRequestRefuses(t *testing.T) {
	for name, body := range map[string]string{
		"not json":                    "nope",
		"unknown version":             `{"version": 99, "run_id": "r1", "host": "203.0.113.10"}`,
		"no run id":                   `{"version": 1, "host": "203.0.113.10"}`,
		"a run id with a path":        `{"version": 1, "run_id": "../etc", "host": "203.0.113.10"}`,
		"no host":                     `{"version": 1, "run_id": "r1"}`,
		"unknown field":               `{"version": 1, "run_id": "r1", "host": "203.0.113.10", "ssh_command": "rm -rf /"}`,
		"a host that reads as a flag": `{"version": 1, "run_id": "r1", "host": "--local"}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseRequest([]byte(body))
			assert.Error(t, err)
		})
	}
}

// The request carries the backup credentials, so its String form, used in
// errors and logs, must not.
func TestRequestNeverPrintsItsSecrets(t *testing.T) {
	req := Request{Backup: &Backup{AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "very-secret"}}
	assert.False(t, strings.Contains(req.String(), "very-secret"))
}
