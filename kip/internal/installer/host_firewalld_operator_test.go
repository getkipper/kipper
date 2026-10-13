package installer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/kip/internal/localrun"
)

// operatorFirewalld runs the operator check against a fake firewall-cmd. The
// default route leaves through eth0, bound to extZone; open lists what that
// zone allows, and the pod network's zone has the given target and ports.
func operatorFirewalld(t *testing.T, extZone, open, podZone, podTarget, podPorts string) []string {
	t.Helper()
	return operatorFirewalldWith(t, extZone, open, podZone, podTarget, podPorts, "", "")
}

var (
	httpsPorts  = "443/tcp"
	directEntry = ""
	policyRule  = ""
	forwardZone = "none"
	// extraSources adds lines to the active zone listing, quoted for printf.
	extraSources = ""
	panicAnswer  = "no"
)

// operatorFirewalldWith can add a dropping rich rule in richZone and an extra policy.
func operatorFirewalldWith(t *testing.T, extZone, open, podZone, podTarget, podPorts, richZone, extraPolicy string) []string {
	t.Helper()
	bin := t.TempDir()
	fakes := map[string]string{
		"sshd": "echo 'port 22'\n",
		"ip":   "echo 'default via 192.0.2.1 dev eth0 proto static'\n",
		"firewall-cmd": `case "$*" in
  *--permanent*) echo "the check must read the running configuration" >&2; exit 1 ;;
  *--get-zone-of-interface=eth0*) [ -n "` + extZone + `" ] && echo ` + extZone + ` && exit 0; exit 2 ;;
  *--get-default-zone*) echo public ;;
  *--get-zone-of-source=*) [ -n "` + podZone + `" ] && echo ` + podZone + ` && exit 0; echo no zone >&2; exit 2 ;;
  *--list-rich-rules*) case "$*" in *"--zone=` + richZone + ` "*) echo 'rule priority="-100" port port="6443" protocol="tcp" drop' ;; esac ;;
  *--get-policies*) echo "allow-host-ipv6 ` + extraPolicy + `" ;;
  *--list-forward-ports*) case "$*" in *"--zone=` + forwardZone + ` "*) echo "port=443:proto=tcp:toport=8443" ;; esac ;;
  *--get-all-passthroughs*) echo "` + directEntry + `" ;;
  *--direct*) ;;
  *--info-policy=allow-host-ipv6*) printf '%s\n' "allow-host-ipv6 (active)" "  target: CONTINUE" "  ingress-zones: ANY" "  egress-zones: HOST" "  services: " "  ports: " "  protocols: " "  masquerade: no" "  forward-ports: " "  source-ports: " "  rich rules: " "	rule family=\"ipv6\" icmp-type name=\"neighbour-solicitation\" accept" "` + policyRule + `" ;;
  *--info-service=*) svc=${*#*--info-service=}; case "$svc" in
      ssh) echo "  ports: 22/tcp" ;; http) echo "  ports: 80/tcp" ;;
      https) echo "  ports: ` + httpsPorts + `" ;; kube-apiserver) echo "  ports: 6443/tcp" ;; esac ;;
  *--query-panic*) echo ` + panicAnswer + ` ;;
  *--get-active-zones*) printf '%s\n' "` + extZone + `" "  interfaces: eth0" "` + podZone + `" "  sources: 10.42.0.0/16" ` + extraSources + ` ;;
  *--info-zone=*) [ -n "` + podTarget + `" ] && echo "  target: ` + podTarget + `" ;;
  *"--zone=` + podZone + ` --query-port="*) for p in ` + podPorts + `; do [ "$*" != "${*%=$p}" ] && exit 0; done; exit 1 ;;
  *"--zone=` + extZone + ` --query-"*) for p in ` + open + `; do [ "$*" != "${*%=$p}" ] && exit 0; done; exit 1 ;;
  *) exit 1 ;;
esac
`,
	}
	for name, body := range fakes {
		//nolint:gosec // the fake commands have to be executable
		require.NoError(t, os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body), 0o755))
	}
	runner := &localrun.Runner{Home: t.TempDir(), Dir: t.TempDir(), Path: bin + ":/usr/bin:/bin"}

	missing, err := CheckOperatorFirewalld(runner)

	require.NoError(t, err)
	return missing
}

func TestCheckOperatorFirewalldAcceptsTheK3sRecipe(t *testing.T) {
	missing := operatorFirewalld(t, "public", "ssh http https 6443/tcp", "trusted", "ACCEPT", "")
	assert.Empty(t, missing)
}

func TestCheckOperatorFirewalldAcceptsNarrowPodRules(t *testing.T) {
	missing := operatorFirewalld(t, "public", "22/tcp 80/tcp 443/tcp kube-apiserver", "pods", "default", "6443/tcp 10250/tcp 9100/tcp")
	assert.Empty(t, missing)
}

func TestCheckOperatorFirewalldListsEverythingMissing(t *testing.T) {
	missing := operatorFirewalld(t, "public", "ssh", "", "", "")

	all := strings.Join(missing, "\n")
	assert.Contains(t, all, "80/tcp open in zone public")
	assert.Contains(t, all, "443/tcp open in zone public")
	assert.Contains(t, all, "6443/tcp open in zone public")
	assert.Contains(t, all, "a zone with source 10.42.0.0/16")
	assert.NotContains(t, all, "22/tcp")
}

func TestCheckOperatorFirewalldNamesPortsClosedToPods(t *testing.T) {
	missing := operatorFirewalld(t, "public", "ssh http https 6443/tcp", "pods", "default", "6443/tcp")

	assert.Equal(t, []string{"10250/tcp open to the pod network", "9100/tcp open to the pod network"}, missing)
}

func TestCheckOperatorFirewalldSaysWhenItCannotReadThePodZone(t *testing.T) {
	missing := operatorFirewalld(t, "public", "ssh http https 6443/tcp", "pods", "", "")

	assert.Equal(t, []string{"a readable target for the pod network's zone"}, missing)
}

func TestCheckOperatorFirewalldReadsTheZoneOfTheExternalInterface(t *testing.T) {
	missing := operatorFirewalld(t, "external", "ssh", "trusted", "ACCEPT", "")

	all := strings.Join(missing, "\n")
	assert.Contains(t, all, "80/tcp open in zone external")
	assert.NotContains(t, all, "22/tcp")
}

// A rich rule can block traffic even when a port entry allows it.
func TestCheckOperatorFirewalldReportsOverridesItCannotEvaluate(t *testing.T) {
	missing := operatorFirewalldWith(t, "public", "ssh http https 6443/tcp", "pods", "default", "6443/tcp 10250/tcp 9100/tcp", "pods", "")
	assert.Equal(t, []string{"no rich rules in zone pods, which kip cannot evaluate"}, missing)

	missing = operatorFirewalldWith(t, "public", "ssh http https 6443/tcp", "trusted", "ACCEPT", "", "public", "block-api")
	assert.Equal(t, []string{"no rich rules in zone public, which kip cannot evaluate", "no policy block-api, which kip cannot evaluate"}, missing)
}

func TestCheckOperatorFirewalldReadsTheServiceDefinition(t *testing.T) {
	httpsPorts = "8443/tcp"
	defer func() { httpsPorts = "443/tcp" }()

	missing := operatorFirewalld(t, "public", "ssh http https 6443/tcp", "trusted", "ACCEPT", "")

	assert.Equal(t, []string{"443/tcp open in zone public"}, missing)
}

func TestCheckOperatorFirewalldReadsTheStockPolicy(t *testing.T) {
	policyRule = "	rule family=\\\"ipv4\\\" port port=\\\"6443\\\" protocol=\\\"tcp\\\" drop"
	defer func() { policyRule = "" }()

	missing := operatorFirewalld(t, "public", "ssh http https 6443/tcp", "trusted", "ACCEPT", "")

	require.Len(t, missing, 1)
	assert.Contains(t, missing[0], "the stock allow-host-ipv6 policy")
	assert.Contains(t, missing[0], "6443")
}

func TestCheckOperatorFirewalldReportsDirectPassthroughs(t *testing.T) {
	directEntry = "ipv4 -t filter -I INPUT 1 -p tcp --dport 6443 -j DROP"
	defer func() { directEntry = "" }()

	missing := operatorFirewalld(t, "public", "ssh http https 6443/tcp", "trusted", "ACCEPT", "")

	assert.Equal(t, []string{"no direct configuration, which kip cannot evaluate"}, missing)
}

// Port forwarding can redirect traffic away from the expected service.
func TestCheckOperatorFirewalldReportsForwardPorts(t *testing.T) {
	forwardZone = "public"
	defer func() { forwardZone = "none" }()

	missing := operatorFirewalld(t, "public", "ssh http https 6443/tcp", "trusted", "ACCEPT", "")

	assert.Equal(t, []string{"no forward ports in zone public, which kip cannot evaluate"}, missing)
}

// Overlapping source bindings can route pod traffic through another zone.
func TestCheckOperatorFirewalldReportsOtherSourceZones(t *testing.T) {
	extraSources = `"drop" "  sources: 10.42.0.0/24"`
	defer func() { extraSources = "" }()

	missing := operatorFirewalld(t, "public", "ssh http https 6443/tcp", "trusted", "ACCEPT", "")

	assert.Equal(t, []string{"no sources bound to zone drop, which kip cannot evaluate"}, missing)
}

// An unreadable panic-mode result must also fail the check.
func TestCheckOperatorFirewalldReportsPanicMode(t *testing.T) {
	for _, answer := range []string{"yes", "'Error: INVALID_COMMAND'"} {
		panicAnswer = answer
		missing := operatorFirewalld(t, "public", "ssh http https 6443/tcp", "trusted", "ACCEPT", "")
		assert.Equal(t, []string{"panic mode off"}, missing, answer)
	}
	panicAnswer = "no"
}
