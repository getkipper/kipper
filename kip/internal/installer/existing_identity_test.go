package installer

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const existingIdentityJSON = `{
  "apiVersion": "kipper.run/v1alpha1",
  "kind": "ClusterIdentity",
  "spec": {
    "domain": "kipper.example.com",
    "hosts": {"dex": "login.example.com"},
    "gateway": {"kipperRunDomain": "203-0-113-10.kipper.run", "register": true}
  },
  "status": {"activeHosts": {"dex": "login.example.com"}}
}`

func TestParseExistingIdentity(t *testing.T) {
	got, err := ParseExistingIdentity(existingIdentityJSON)
	require.NoError(t, err)
	assert.Equal(t, "kipper.example.com", got.Domain)
	assert.Equal(t, "login.example.com", got.DexHost)
	assert.Empty(t, got.ConsoleHost, "an absent override stays empty (derive by convention)")
	assert.Equal(t, "203-0-113-10.kipper.run", got.KipperRunDomain)
}

func TestParseExistingIdentitySkipsLeadingWarnings(t *testing.T) {
	got, err := ParseExistingIdentity("W0713 10:00:00 some kubectl warning\n" + existingIdentityJSON)
	require.NoError(t, err)
	assert.Equal(t, "kipper.example.com", got.Domain)
}

func TestParseExistingIdentityRejectsGarbage(t *testing.T) {
	_, err := ParseExistingIdentity("error: something unrelated")
	assert.Error(t, err)
}

func TestParseExistingIdentityRejectsEmptyDomain(t *testing.T) {
	_, err := ParseExistingIdentity(`{"spec": {"hosts": {"dex": "login.example.com"}}}`)
	assert.Error(t, err, "an identity without a domain is a broken cluster, never silently adoptable")
}

func TestIdentityAbsentFromKubectl(t *testing.T) {
	tests := []struct {
		name     string
		combined string
		want     bool
	}{
		{"CR not created yet", `Error from server (NotFound): clusteridentities.kipper.run "cluster" not found`, true},
		{"CRD not installed", `error: the server doesn't have a resource type "clusteridentity"`, true},
		{"apiserver down is not absence", "The connection to the server 127.0.0.1:6443 was refused - did you specify the right host or port?", false},
		{"generic transport error is not absence", "ssh: connection reset", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, identityAbsentFromKubectl(tt.combined))
		})
	}
}

func TestAdoptIdentityKeepsExistingByDefault(t *testing.T) {
	existing := &ExistingIdentity{Domain: "kipper.example.com", DexHost: "login.example.com"}

	got, err := AdoptIdentity(existing, "", "", "", "")
	require.NoError(t, err)
	assert.Equal(t, existing, got)
}

func TestAdoptIdentityAcceptsMatchingFlags(t *testing.T) {
	existing := &ExistingIdentity{Domain: "kipper.example.com", DexHost: "login.example.com"}

	got, err := AdoptIdentity(existing, "kipper.example.com", "", "", "login.example.com")
	require.NoError(t, err)
	assert.Equal(t, "kipper.example.com", got.Domain)
}

func TestAdoptIdentityRejectsConflictingDomain(t *testing.T) {
	existing := &ExistingIdentity{Domain: "kipper.example.com"}

	_, err := AdoptIdentity(existing, "other.example.com", "", "", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kip cluster domain", "the error must point at the safe path for a domain change")
}

func TestAdoptIdentityRejectsConflictingHostOverride(t *testing.T) {
	existing := &ExistingIdentity{Domain: "kipper.example.com", DexHost: "login.example.com"}

	_, err := AdoptIdentity(existing, "", "", "", "dex.kipper.example.com")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kip cluster domain")
}

func TestParseExistingIdentityRejectsInFlightTransition(t *testing.T) {
	inFlight := `{
  "spec": {"domain": "kipper.example.com"},
  "status": {"transition": {"phase": "DualServe"}}
}`
	_, err := ParseExistingIdentity(inFlight)
	require.Error(t, err, "reinstalling mid-transition would cut over outside the phase machine")
	assert.Contains(t, err.Error(), "--sync")
	assert.Contains(t, err.Error(), "DualServe")
}
