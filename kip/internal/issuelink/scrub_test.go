package issuelink

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestScrub_ReplacesKnownValuesWhereverTheyAppear(t *testing.T) {
	got := Scrub("ssh to server.example.net: password Hunter2-not-real rejected",
		[]string{"Hunter2-not-real"}, []string{"server.example.net"})

	assert.Equal(t, "ssh to <redacted>: password <redacted> rejected", got)
}

func TestScrub_IgnoresEmptyKnownValues(t *testing.T) {
	assert.Equal(t, "unchanged", Scrub("unchanged", []string{""}, []string{""}))
}

func TestScrub_ReplacesTheLongerKnownValueFirst(t *testing.T) {
	got := Scrub("console at console--lab.example.org", nil, []string{"lab.example.org", "console--lab.example.org"})

	assert.Equal(t, "console at <redacted>", got)
}

func TestScrub_ReplacesKnownValuesOnlyAsWholeTokens(t *testing.T) {
	got := Scrub("ed: connection refused by ed", nil, []string{"ed"})

	assert.Equal(t, "<redacted>: connection refused by <redacted>", got)
}

func TestScrub_DoesNotReprocessItsOwnPlaceholders(t *testing.T) {
	got := Scrub("value redacted and secret", nil, []string{"redacted", "secret"})

	assert.Equal(t, "value <redacted> and <redacted>", got)
}

func TestScrub_ReplacesEncodedFormsOfKnownValues(t *testing.T) {
	got := Scrub("GET /login?auth=blue%2Fbird failed", []string{"blue/bird"}, nil)

	assert.Equal(t, "GET /login?auth=<redacted> failed", got)
}

func TestScrub_ReplacesTheWholeEmailEvenWhenItsDomainIsKnown(t *testing.T) {
	assert.Equal(t, "contact <email>", Scrub("contact alice@lab.example.org", nil, []string{"lab.example.org"}))
}

func TestScrub_ReplacesTheUserOfAKnownHost(t *testing.T) {
	got := Scrub("ssh deploy@server01: permission denied", nil, []string{"server01"})

	assert.Equal(t, "ssh <user>@<redacted>: permission denied", got)
}

func TestScrub_ReplacesPublicAndPrivateAddresses(t *testing.T) {
	got := Scrub("dial tcp 203.0.113.10:22: i/o timeout; lan 192.168.1.5; v6 2001:db8::1 refused", nil, nil)

	assert.Equal(t, "dial tcp <ip>:22: i/o timeout; lan <ip>; v6 <ip> refused", got)
}

func TestScrub_ReplacesAnIPv6AddressFollowedByAColon(t *testing.T) {
	assert.Equal(t, "<ip>: connection refused", Scrub("2001:db8::1: connection refused", nil, nil))
}

func TestScrub_ReplacesMappedAndZonedIPv6AsAWhole(t *testing.T) {
	got := Scrub("from ::ffff:192.168.1.2 and ::ffff:203.0.113.10 via fe80::1%eth0", nil, nil)

	assert.Equal(t, "from <ip> and <ip> via fe80::1%eth0", got)
}

func TestScrub_KeepsClusterInternalLoopbackAndLinkLocalAddresses(t *testing.T) {
	in := "service 10.43.0.1:443, pod 10.42.0.7, local 127.0.0.1 and ::1, metadata 169.254.169.254"

	assert.Equal(t, in, Scrub(in, nil, nil))
}

func TestScrub_KeepsVersionsAndTimes(t *testing.T) {
	in := "k3s v1.31.4+k3s1 at 12:34:56 on 2026-10-10"

	assert.Equal(t, in, Scrub(in, nil, nil))
}

func TestScrub_ReplacesEmailAddresses(t *testing.T) {
	assert.Equal(t, "admin <email> exists", Scrub("admin ops@example.com exists", nil, nil))
}

func TestScrub_ReplacesKipperRunNames(t *testing.T) {
	got := Scrub("GET https://console--203-0-113-10.kipper.run/ and lab.kipper.run", nil, nil)

	assert.Equal(t, "GET https://<cluster>.kipper.run/ and <cluster>.kipper.run", got)
}

func TestScrub_ReplacesHostnamesEndingInACommonTopLevelDomain(t *testing.T) {
	got := Scrub("Could not resolve host: mirror.example.com; also example.net", nil, nil)

	assert.Equal(t, "Could not resolve host: <host>; also <host>", got)
}

func TestScrub_ReplacesAnyHostInAURL(t *testing.T) {
	got := Scrub("dial https://build.internal/v2 and https://build.example.net:8443/x", nil, nil)

	assert.Equal(t, "dial https://<host>/v2 and https://<host>:8443/x", got)
}

func TestScrub_ReplacesTenantHostsInAURL(t *testing.T) {
	got := Scrub("PUT https://acme-backups.s3.example.net/velero failed", nil, nil)

	assert.Equal(t, "PUT https://<host>/velero failed", got)
}

func TestScrub_KeepsKnownDependencyHostsAndFileNames(t *testing.T) {
	in := "pull ghcr.io/getkipper/console from https://registry-1.docker.io; get.k3s.io; read values.yaml, " +
		"kipper-hop.tls.crt and server.conf.bak; jsonpath {.spec.template.spec.containers} and status.kubernetesStatus.namespace"

	assert.Equal(t, in, Scrub(in, nil, nil))
}

func TestScrub_ReplacesTheUserInHomeDirectories(t *testing.T) {
	got := Scrub(`open /home/alice/.kip/config.yaml, /Users/bob/.ssh/id and C:\Users\carol\.kip failed`, nil, nil)

	assert.Equal(t, `open /home/<user>/.kip/config.yaml, /Users/<user>/.ssh/id and C:\Users\<user>\.kip failed`, got)
}

func TestScrub_ReplacesValuesOfCredentialKeys(t *testing.T) {
	got := Scrub(`password=abc123 token: "s3cr3t" K3S_TOKEN=K10abc api_key=xyz`, nil, nil)

	assert.Equal(t, `password=<secret> token: "<secret>" K3S_TOKEN=<secret> api_key=<secret>`, got)
}

func TestScrub_ReplacesQuotedAndJSONCredentials(t *testing.T) {
	got := Scrub(`{"password":"hunter2"} password="first second" secret='a b'`, nil, nil)

	assert.Equal(t, `{"password":"<secret>"} password="<secret>" secret='<secret>'`, got)
}

func TestScrub_ReplacesCredentialsInURLs(t *testing.T) {
	got := Scrub("clone https://oauth2:ghp-not-a-token@github.com/acme/app.git failed", nil, nil)

	assert.Equal(t, "clone https://<secret>@github.com/acme/app.git failed", got)
}

func TestScrub_ReplacesBearerTokensJWTsAndLongSecrets(t *testing.T) {
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4In0.c2lnbmF0dXJlLWZha2U"
	hex := "a3ffc8476a6bacb9b57a205acd92b589"
	b64 := "Zm9vYmFyQmF6UXV4MTIzNDU2Nzg5MGFiY2RlZmdoaWo="
	got := Scrub("Authorization: Bearer opaque-value "+jwt+" "+hex+" "+b64, nil, nil)

	assert.Equal(t, "Authorization: Bearer <secret> <secret> <secret> <secret>", got)
}

func TestScrub_KeepsOrdinaryWordsPathsAndURLs(t *testing.T) {
	in := "curl -sfL https://get.k3s.io failed in /usr/local/bin/k3s-install.sh: exit status 22"

	assert.Equal(t, in, Scrub(in, nil, nil))
}

func TestScrub_ReplacesAKnownSecretInsideALongerToken(t *testing.T) {
	assert.Equal(t, "auth=prefix<redacted>suffix", Scrub("auth=prefixblue/birdsuffix", []string{"blue/bird"}, nil))
}

func TestScrub_ReplacesAKnownSecretBeforeAnyPatternCanSplitIt(t *testing.T) {
	got := Scrub("auth=left/alice@example.com/right", []string{"left/alice@example.com/right"}, nil)

	assert.Equal(t, "auth=<redacted>", got)
}

func TestScrub_ReplacesLowerCaseEncodedFormsOfKnownSecrets(t *testing.T) {
	assert.Equal(t, "GET /login?auth=<redacted>", Scrub("GET /login?auth=blue%2fbird", []string{"blue/bird"}, nil))
}

func TestScrub_ReplacesCredentialsWithEscapedQuotes(t *testing.T) {
	got := Scrub(`{"password":"first\"second"} password=\"first second\"`, nil, nil)

	assert.Equal(t, `{"password":"<secret>"} password=\"<secret>\"`, got)
}

func TestScrub_ReplacesCredentialFlagArguments(t *testing.T) {
	got := Scrub(`kip install --password 'first second' --git-token abc123 --host lab`, nil, nil)

	assert.Equal(t, `kip install --password '<secret>' --git-token <secret> --host lab`, got)
}
