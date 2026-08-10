package authncfg

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValidateDexHost(t *testing.T) {
	for _, ok := range []string{"dex.example.com", "dex--203-0-113-10.kipper.run", "a.b.c.d"} {
		assert.NoError(t, ValidateDexHost(ok), ok)
	}
	// Every rejected value carries a shell metacharacter or is otherwise not
	// a plain hostname — the injection gate for command interpolation.
	for _, bad := range []string{
		"", "dex.example.com; rm -rf /", "dex .example.com", "dex$(whoami).com",
		"dex`id`.com", "-dex.example.com", "dex.example.com/", "HTTP://x", "127.0.0.1:6443",
		strings.Repeat("a", 254),
	} {
		assert.Error(t, ValidateDexHost(bad), "%q must be rejected", bad)
	}
}

func TestValidateAdminEmail(t *testing.T) {
	assert.NoError(t, ValidateAdminEmail("admin@cluster.example.com"))
	for _, bad := range []string{"", "admin", "admin@", "a@b", "admin@x;rm", "admin @x.com", "admin@x`.com"} {
		assert.Error(t, ValidateAdminEmail(bad), bad)
	}
}

func TestRenderSingleAndMultiIssuer(t *testing.T) {
	single := Render("", "dex.example.com")
	assert.Equal(t, 1, strings.Count(single, "url: https://"))
	assert.Contains(t, single, "url: https://dex.example.com/dex")
	assert.Contains(t, single, `prefix: "oidc:"`)
	assert.Contains(t, single, "kipper-cli")

	dual := Render("", "dex.old.example.com", "dex.new.example.com")
	assert.Equal(t, 2, strings.Count(dual, "url: https://"))
	assert.Contains(t, dual, "dex.old.example.com/dex")
	assert.Contains(t, dual, "dex.new.example.com/dex")
}

func TestHashIsStableAndDistinct(t *testing.T) {
	h := Hash(Render("", "dex.example.com"))
	assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, h)
	assert.Equal(t, h, Hash(Render("", "dex.example.com")))
	assert.NotEqual(t, h, Hash(Render("", "dex.other.example.com")))
}

func TestHostsFor(t *testing.T) {
	assert.Equal(t, []string{"a.com", "b.com"}, HostsFor("a.com", "b.com"))
	assert.Equal(t, []string{"a.com"}, HostsFor("a.com", "a.com"), "override-only move degenerates")
	assert.Equal(t, []string{"a.com"}, HostsFor("a.com", ""))
	assert.Equal(t, []string{"b.com"}, HostsFor("", "b.com"))
}

func TestActiveHashMatches(t *testing.T) {
	want := Hash(Render("", "dex.example.com"))
	metrics := `# HELP apiserver_authentication_config_controller_last_config_info ...
apiserver_authentication_config_controller_last_config_info{apiserver_id_hash="sha256:deadbeef",hash="` + want + `"} 1
apiserver_authentication_config_controller_automatic_reloads_total{status="success"} 3
`
	assert.True(t, ActiveHashMatches(metrics, want))
	assert.False(t, ActiveHashMatches(metrics, Hash(Render("", "dex.other.example.com"))))
	// A bare substring of the wanted hash elsewhere must not match: exact
	// label extraction, not strings.Contains.
	sneaky := `some_other_metric{note="` + want + `"} 1` + "\n"
	assert.False(t, ActiveHashMatches(sneaky, want), "the hash must be read from the last_config_info hash label only")
}

// A gateway-fronted host is served the cluster's own hop certificate, which no
// public authority signed. Without an anchor in its issuer the API server
// verifies it against the system trust store and fails, which is what made a
// fresh install on a kipper.run domain deadlock — and would have left operator
// login broken even if the installer had been taught to skip past it.
func TestKipperRunIssuerCarriesTheClusterAnchor(t *testing.T) {
	const ca = "-----BEGIN CERTIFICATE-----\nMIIB\nAAAA\n-----END CERTIFICATE-----"

	gateway := Render(ca, "dex--159-195-217-19.kipper.run")
	assert.Contains(t, gateway, "certificateAuthority: |",
		"a gateway-fronted issuer must carry an anchor")
	assert.Contains(t, gateway, "        -----BEGIN CERTIFICATE-----",
		"the PEM must be indented into the block scalar")
	assert.Contains(t, gateway, "        MIIB", "every PEM line must be indented")

	// A custom domain carries a real WebPKI certificate. Anchoring it to the
	// cluster CA would break the moment cert-manager renewed it.
	custom := Render(ca, "dex.example.com")
	assert.NotContains(t, custom, "certificateAuthority",
		"a custom-domain issuer must keep verifying against the system trust store")
}

// A cutover legitimately trusts both kinds at once. The anchor has to be per
// issuer for that to work: one global anchor would replace the system pool and
// stop the custom-domain issuer verifying at all.
func TestCutoverAnchorsOnlyTheGatewayIssuer(t *testing.T) {
	const ca = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----"
	both := Render(ca, "dex.example.com", "dex--acme.kipper.run")

	assert.Equal(t, 1, strings.Count(both, "certificateAuthority: |"),
		"exactly one of the two issuers may carry an anchor")

	gatewayAt := strings.Index(both, "dex--acme.kipper.run")
	anchorAt := strings.Index(both, "certificateAuthority: |")
	customAt := strings.Index(both, "dex.example.com")
	assert.Greater(t, anchorAt, customAt, "the anchor must not land on the custom-domain issuer")
	assert.Less(t, anchorAt, gatewayAt+len(both)-gatewayAt, "the anchor belongs to the gateway issuer")
	assert.Greater(t, anchorAt, gatewayAt-len("      url: https://"), "the anchor must follow its own issuer's url")
}

// The installer writes this file and console-api hashes it to gate a cutover.
// They must render the same bytes from the same inputs or the gate can never
// pass.
func TestRenderIsByteStableForTheSameAnchor(t *testing.T) {
	const ca = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----"
	first := Render(ca, "dex--acme.kipper.run")
	second := Render(ca, "dex--acme.kipper.run")
	assert.Equal(t, first, second)
	assert.Equal(t, Hash(first), Hash(second))

	// A different anchor is a different config: the apiserver must reload.
	other := Render("-----BEGIN CERTIFICATE-----\nZZZZ\n-----END CERTIFICATE-----", "dex--acme.kipper.run")
	assert.NotEqual(t, Hash(first), Hash(other))

	// No anchor available renders the same as a custom domain would.
	assert.NotContains(t, Render("", "dex--acme.kipper.run"), "certificateAuthority")
}
