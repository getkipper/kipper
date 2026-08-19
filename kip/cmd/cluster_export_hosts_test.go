package cmd

import (
	"strings"
	"testing"

	"github.com/getkipper/kipper/kip/internal/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/kip/internal/clusteridentity"
)

func identityWithHosts(domain string, hosts *clusteridentity.Hosts) *clusteridentity.ClusterIdentity {
	return &clusteridentity.ClusterIdentity{
		Spec: clusteridentity.Spec{Domain: domain, Hosts: hosts},
	}
}

// The defect this change exists for: an export that omits the hosts leaves the
// importing machine to derive them by prefixing the cluster domain, which on a
// cluster whose domain is already a subdomain produces
// dex.console.example.com for dex.example.com — a host with no certificate,
// met as a browser warning while the colleague is asked for their password.
func TestExportCarriesTheServingHosts(t *testing.T) {
	identity := identityWithHosts("console.example.com", &clusteridentity.Hosts{
		Console:    "console.example.com",
		ConsoleAPI: "console-api.example.com",
		Dex:        "dex.example.com",
	})

	bundle, err := renderExportHosts(&config.Cluster{Name: "shop"}, identity)

	require.NoError(t, err)
	assert.Equal(t, "console.example.com", bundle.Console)
	assert.Equal(t, "console-api.example.com", bundle.ConsoleAPI)
	assert.Equal(t, "dex.example.com", bundle.Dex)
}

// All three are always written, even when every one of them matches the
// convention. Their presence is the only signal that tells a new-format bundle
// from a legacy one, and omitting the conventional case destroys it.
func TestExportCarriesTheHostsEvenWhenTheyAreConventional(t *testing.T) {
	identity := identityWithHosts("example.com", nil)

	bundle, err := renderExportHosts(&config.Cluster{Name: "shop"}, identity)

	require.NoError(t, err)
	assert.Equal(t, "console.example.com", bundle.Console)
	assert.Equal(t, "console-api.example.com", bundle.ConsoleAPI)
	assert.Equal(t, "dex.example.com", bundle.Dex)
}

// Mid-cutover the spec names where the cluster is going while the certificate
// still answers for where it is. The export carries what answers now and says
// the move is happening: refusing outright would mean nobody can be onboarded
// while a transition is open or stuck, which is when it is most needed.
func TestExportCarriesTheServingIdentityWhileTheClusterIsMoving(t *testing.T) {
	identity := identityWithHosts("new.example.com", &clusteridentity.Hosts{Dex: "dex.new.example.com"})
	identity.Status.Transition = &clusteridentity.Transition{Phase: clusteridentity.PhaseDualServe}
	identity.Status.Steady = &clusteridentity.SteadyIdentity{
		Domain: "old.example.com",
		Hosts:  &clusteridentity.Hosts{Dex: "dex.old.example.com"},
	}

	bundle, err := renderExportHosts(&config.Cluster{Name: "shop"}, identity)

	require.NoError(t, err, "a move in flight must not stop an operator onboarding a colleague")
	assert.Equal(t, "dex.old.example.com", bundle.Dex,
		"the hosts that answer now are the ones a colleague can reach")
}

// An identity that names no domain cannot produce hosts, and deriving them
// from the local config is the drift this change exists to stop carrying.
func TestExportRefusesAnIdentityWithNoDomain(t *testing.T) {
	_, err := renderExportHosts(&config.Cluster{Name: "shop"}, identityWithHosts("", nil))

	require.Error(t, err)
}

// The importer's half. A host that matches the convention for the domain it
// arrived with is stored as no override at all, so the entry keeps tracking
// the convention if the cluster's domain later changes.
func TestImportStoresNoOverrideForAConventionalHost(t *testing.T) {
	entry := importedHostOverrides("example.com", exportHosts{
		Console:    "console.example.com",
		ConsoleAPI: "console-api.example.com",
		Dex:        "dex.example.com",
	})

	assert.Empty(t, entry.Console, "a conventional host stored as an override stops tracking a later domain change")
	assert.Empty(t, entry.ConsoleAPI)
	assert.Empty(t, entry.Dex)
}

func TestImportStoresAnOverrideForACustomHost(t *testing.T) {
	entry := importedHostOverrides("console.example.com", exportHosts{
		Console:    "console.example.com",
		ConsoleAPI: "console-api.example.com",
		Dex:        "dex.example.com",
	})

	assert.Equal(t, "console.example.com", entry.Console)
	assert.Equal(t, "console-api.example.com", entry.ConsoleAPI)
	assert.Equal(t, "dex.example.com", entry.Dex)
}

// The producer always writes three, so one or two means the file was truncated
// or hand-edited. Treating that as "new format" would replace the good
// overrides on the entry with empty strings and recreate the original defect.
func TestImportRejectsAPartialHostTriple(t *testing.T) {
	for _, partial := range []exportHosts{
		{Console: "console.example.com", keys: 1},
		{Console: "console.example.com", Dex: "dex.example.com", keys: 2},
		{ConsoleAPI: "console-api.example.com", Dex: "dex.example.com", keys: 2},
	} {
		err := partial.validate()
		require.Error(t, err, "a partial triple must not be treated as a complete one")
		assert.Contains(t, err.Error(), "incomplete")
	}
}

func TestImportAcceptsNoHostsAtAllAsALegacyBundle(t *testing.T) {
	var none exportHosts

	require.NoError(t, none.validate())
	assert.False(t, none.present(), "a bundle with no host keys predates them and must leave existing overrides alone")
}

func TestImportAcceptsACompleteTriple(t *testing.T) {
	complete := exportHosts{
		Console:    "console.example.com",
		ConsoleAPI: "console-api.example.com",
		Dex:        "dex.example.com",
		keys:       3,
	}

	require.NoError(t, complete.validate())
	assert.True(t, complete.present())
}

// The bundle is parsed line by line with the indentation stripped, so a nested
// block whose child key is "domain" would be read as the top-level domain and
// overwrite the cluster's. Flat keys are what make that impossible.
func TestExportedHostKeysAreFlatAndUnambiguous(t *testing.T) {
	rendered := renderExportHostLines(exportHosts{
		Console:    "console.example.com",
		ConsoleAPI: "console-api.example.com",
		Dex:        "dex.example.com",
	})

	for _, line := range strings.Split(strings.TrimSpace(rendered), "\n") {
		assert.False(t, strings.HasPrefix(line, " "), "an indented key is read by an older kip as a top-level one: %q", line)
	}
	assert.Contains(t, rendered, "console_domain: console.example.com")
	assert.Contains(t, rendered, "console_api_domain: console-api.example.com")
	assert.Contains(t, rendered, "dex_domain: dex.example.com")
	assert.NotContains(t, rendered, "\ndomain:", "no key named domain, which an older kip would apply to the cluster domain")
}

// The export's promise is that the file holds no credential, and the AuthInfo
// checks alone cannot keep it: the server and proxy addresses are carried
// across for reachability, and a URL may carry a username and password. A
// bundle with no authentication data at all can still hand a colleague the
// operator's proxy password.
func TestExportRefusesAProxyURLCarryingACredential(t *testing.T) {
	err := rejectURLCredentials([]byte(`apiVersion: v1
kind: Config
clusters:
  - name: shop
    cluster:
      server: https://203.0.113.10:6443
      certificate-authority-data: Y2EtcGVt
      proxy-url: https://operator:hunter2@proxy.example.com:3128
contexts: []
users: []
`))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "proxy address")
}

func TestExportRefusesAServerURLCarryingACredential(t *testing.T) {
	err := rejectURLCredentials([]byte(`apiVersion: v1
kind: Config
clusters:
  - name: shop
    cluster:
      server: https://operator:hunter2@203.0.113.10:6443
      certificate-authority-data: Y2EtcGVt
contexts: []
users: []
`))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "server address")
}

// A proxy with no credential is exactly what has to keep working: without it
// the address is unreachable for the colleague importing it.
func TestExportKeepsAnUnauthenticatedProxy(t *testing.T) {
	require.NoError(t, rejectURLCredentials([]byte(`apiVersion: v1
kind: Config
clusters:
  - name: shop
    cluster:
      server: https://203.0.113.10:6443
      certificate-authority-data: Y2EtcGVt
      proxy-url: socks5://proxy.example.com:1080
contexts: []
users: []
`)))
}

// A hand-edited bundle is explicitly in scope: these values become the hosts
// kip builds every endpoint from, so one that cannot be a hostname has to be
// caught at the import rather than surfacing later as a malformed URL.
func TestImportRejectsAHostThatIsNotAHostname(t *testing.T) {
	for _, bad := range []exportHosts{
		{Console: "https://console.example.com", ConsoleAPI: "console-api.example.com", Dex: "dex.example.com", keys: 3},
		{Console: "console.example.com", ConsoleAPI: "console-api.example.com/path", Dex: "dex.example.com", keys: 3},
		{Console: "console.example.com", ConsoleAPI: "console-api.example.com", Dex: "operator@dex.example.com", keys: 3},
	} {
		require.Error(t, bad.validate(), "%+v was accepted", bad)
	}
}

// The case only key-counting catches. A bundle whose three host lines lost
// their values is damaged, not old: judging presence by value reads it as a
// bundle written before hosts were carried, so validate passes, the import
// stores no overrides, and a fresh machine derives the plausible wrong hosts
// this whole contract exists to stop.
func TestImportTreatsEmptyHostValuesAsDamageRatherThanAge(t *testing.T) {
	damaged := exportHosts{keys: 3}

	assert.True(t, damaged.present(), "three keys arrived, so this bundle is not one that predates them")
	require.Error(t, damaged.validate())
}

func TestImportTreatsNoHostKeysAtAllAsAge(t *testing.T) {
	var legacy exportHosts

	assert.False(t, legacy.present())
	require.NoError(t, legacy.validate())
}
